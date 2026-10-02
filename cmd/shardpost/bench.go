package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"shardpost/internal/bench"
	"shardpost/internal/bench/cephbench"
	"shardpost/internal/cephstore"
	"shardpost/internal/charset"
	"shardpost/internal/config"
	"shardpost/internal/delivery"
	"shardpost/internal/fts"
	"shardpost/internal/logging"
	"shardpost/internal/membudget"
	"shardpost/internal/sieve"
)

// "shardpost [--config <file>] bench populate|measure [flags]" fills
// separate bench pools with accounts and measures what they cost (scaling
// plan, stage R4; docs/operations/bench.md, shardpost-bench(1)).

// Exit codes of the bench.
const (
	benchOK      = 0
	benchFailed  = 1 // the run failed or some accounts failed
	benchRefused = 2 // usage error or a refusal of the guard
)

const commandBench = "bench"

// splitBench finds "bench" after the global options; it returns those
// options and the arguments after "bench".
func splitBench(args []string) ([]string, []string, bool) {
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case flagConfig:
			i++
		case commandBench:
			return args[:i], args[i+1:], true
		default:
			return nil, nil, false
		}
	}

	return nil, nil, false
}

// benchFlags are the options both subcommands share.
type benchFlags struct {
	indexPool, dataPool, systemPool, prefix, jsonOut, sizes string
	parallel                                                int
	seed                                                    uint64
	// ftsLayout is the layout of new full-text indexes (0: the
	// configuration's); 3 sets the cluster mark in the bench pools.
	ftsLayout int
}

func (f *benchFlags) register(fs *flag.FlagSet) {
	fs.StringVar(&f.indexPool, "index-pool", "", "bench index pool (replicated; required, must not be ceph.index_pool or ceph.data_pool)")
	fs.StringVar(&f.dataPool, "data-pool", "", "bench data pool (erasure coded like production; required, must not be a production pool)")
	fs.StringVar(&f.systemPool, "system-pool", "", "bench system pool (replicated; optional, must not be a production pool; ceph.system_pool is never taken over)")
	fs.StringVar(&f.prefix, "prefix", bench.RequiredPrefix, "account name prefix (must start with "+bench.RequiredPrefix+")")
	fs.IntVar(&f.parallel, "parallel", 4, "accounts worked on at the same time")
	fs.StringVar(&f.jsonOut, "json", "", "write the report as JSON to this file (- for standard output)")
	fs.StringVar(&f.sizes, "sizes", bench.DefaultSizes, "message size distribution <weight>:<min>-<max>,...")
	fs.Uint64Var(&f.seed, "seed", 1, "seed of the message generator")
	fs.IntVar(&f.ftsLayout, "fts-layout", 0, "layout of new full-text indexes: 2 or 3 (3 sets the layout 3 mark in the bench pools; 0: the configuration's)")
}

// runBench runs a bench subcommand and returns the exit code.
func runBench(pre, args []string) int {
	log := logging.New(os.Stderr, "info").With("service", "shardpost-bench")

	settings, _, err := loadConfig(pre)
	if err != nil {
		log.Error("bench: configuration", "err", err)
		return benchRefused
	}

	log = logging.New(os.Stderr, settings.Logging.Level).With("service", "shardpost-bench")

	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: shardpost [--config <file>] bench populate|measure [flags]")
		return benchRefused
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	switch args[0] {
	case "populate":
		return benchPopulate(ctx, log, settings, args[1:])
	case "measure":
		return benchMeasure(ctx, log, settings, args[1:])
	default:
		fmt.Fprintf(os.Stderr, "unknown bench command %q (populate, measure)\n", args[0])
		return benchRefused
	}
}

// benchSetup checks the guard and opens the store on the bench pools, as
// a tool: the placement is checked once the pools are set up
// (cephbench.Populate, cephbench.Measure).
func benchSetup(ctx context.Context, log *slog.Logger, settings *config.Config, f *benchFlags) (bench.Guard, cephstore.Config, *cephstore.Store, int) {
	g := bench.Guard{ProdIndex: settings.Ceph.IndexPool, ProdData: settings.Ceph.DataPool, ProdSystem: settings.Ceph.SystemPool,
		Reserved:   []string{config.Default("ceph.index_pool"), config.Default("ceph.data_pool")},
		BenchIndex: f.indexPool, BenchData: f.dataPool, BenchSystem: f.systemPool, Prefix: f.prefix}
	if err := g.Check(); err != nil {
		log.Error("bench: refused", "err", err)
		return g, cephstore.Config{}, nil, benchRefused
	}

	// The full-text index prepares text the way the server does.
	if err := charset.SetFallback(settings.FTS.FallbackCharset); err != nil {
		log.Error("bench: configuration", "err", err)
		return g, cephstore.Config{}, nil, benchRefused
	}

	if err := fts.SetHTMLMode(settings.FTS.HTML); err != nil {
		log.Error("bench: configuration", "err", err)
		return g, cephstore.Config{}, nil, benchRefused
	}

	membudget.SetLimit(settings.Server.MemoryBudgetBytes)

	// The production system pool is never taken over: the bench's is
	// --system-pool or none.
	cfg := cephConfig(settings)
	cfg.IndexPool, cfg.DataPool, cfg.SystemPool, cfg.PlacementTool = f.indexPool, f.dataPool, f.systemPool, true

	cfg.FTS.Layout = f.effectiveLayout(cfg.FTS.Layout)

	rotate, err := rookCredentials(ctx, log, settings, &cfg)
	if err != nil {
		log.Error("bench: credentials", "err", err)
		return g, cfg, nil, benchFailed
	}

	store, err := cephstore.Open(cfg, log)
	if err != nil {
		log.Error("bench: connect", "err", err)
		return g, cfg, nil, benchFailed
	}

	if rotate != nil {
		rotate(store)
	}

	return g, cfg, store, benchOK
}

// benchDeliverer is the delivery pipeline of LMTP on a store, without
// what reaches beyond the account: no relay (redirects keep the
// message, vacation and notify are skipped), no external programs, no
// global, before or after scripts.
func benchDeliverer(log *slog.Logger, settings *config.Config) func(*cephstore.Store) bench.Deliverer {
	sieveCfg := sieve.ParseSettings(settings.Sieve.Settings)

	return func(s *cephstore.Store) bench.Deliverer {
		return &delivery.Service{Store: storeAdapter{s}, Log: log.With("component", "delivery"),
			SieveConfig: sieveCfg, PublicPrefix: settings.Server.PublicPrefix, SharedPrefix: settings.Server.SharedPrefix,
			ReturnPath: settings.Server.LmtpReturnPath, DeliveredTo: settings.Server.LmtpDeliveredTo,
			Hostname: settings.Server.LmtpHostname}
	}
}

func parseBenchFlags(fs *flag.FlagSet, args []string) int {
	fs.SetOutput(os.Stderr)

	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return benchOK
		}

		return benchRefused
	}

	if fs.NArg() > 0 {
		fmt.Fprintf(os.Stderr, "unexpected argument %q\n", fs.Arg(0))
		return benchRefused
	}

	return -1
}

func benchPopulate(ctx context.Context, log *slog.Logger, settings *config.Config, args []string) int {
	var (
		f          benchFlags
		o          bench.PopulateOptions
		mailboxes  string
		parseCode  int
		sizeParsed bench.SizeDist
		err        error
	)

	fs := flag.NewFlagSet("bench populate", flag.ContinueOnError)
	f.register(fs)
	fs.IntVar(&o.Accounts, "accounts", 1000, "number of accounts")
	fs.IntVar(&o.First, "first", 0, "number of the first account")
	fs.IntVar(&o.Messages, "messages", 50, "messages every account ends up with")
	fs.StringVar(&mailboxes, "mailboxes", bench.DefaultMailboxes, "mailboxes of every account (INBOX first)")
	fs.Float64Var(&o.InboxShare, "inbox-share", 0.7, "share of the messages delivered to INBOX through Sieve")
	fs.DurationVar(&o.Progress, "progress", 30*time.Second, "interval of the progress log (0: none)")

	if parseCode = parseBenchFlags(fs, args); parseCode >= 0 {
		return parseCode
	}

	if sizeParsed, err = bench.ParseSizes(f.sizes); err != nil {
		log.Error("bench: --sizes", "err", err)
		return benchRefused
	}

	o.Prefix, o.Parallel, o.Log = f.prefix, f.parallel, log
	o.Mailboxes = bench.ParseMailboxes(mailboxes)
	o.Gen = bench.Generator{Seed: f.seed, Sizes: sizeParsed}

	if err := o.Validate(); err != nil {
		log.Error("bench: refused", "err", err)
		return benchRefused
	}

	g, cfg, store, code := benchSetup(ctx, log, settings, &f)
	if store == nil {
		return code
	}
	defer cephbench.Close(store)

	rep, err := cephbench.Populate(ctx, g, store, cfg, benchDeliverer(log, settings)(store), o)
	if rep != nil {
		code = writeBenchReport(f.jsonOut, rep, func(w io.Writer) error { return bench.WritePopulateTable(w, rep) })
	}

	return benchExit(log, err, code)
}

func benchMeasure(ctx context.Context, log *slog.Logger, settings *config.Config, args []string) int {
	var (
		f  benchFlags
		o  cephbench.MeasureOptions
		ec bench.ECEstimate
	)

	fs := flag.NewFlagSet("bench measure", flag.ContinueOnError)
	f.register(fs)
	fs.IntVar(&o.Every, "sample-every", 1, "take the inventory of every n-th account (by number)")
	fs.IntVar(&o.ProbeAccounts, "probe-accounts", 20, "accounts of the delivery and open rounds (0: inventory only)")
	fs.IntVar(&o.ProbeMessages, "probe-messages", 10, "messages per probe account and delivery round (0: no delivery rounds)")
	fs.IntVar(&o.Contention, "contention", 0, "also deliver this many messages at a time into each probe account (0: no contention rounds)")
	fs.IntVar(&o.Search, "search", 0, "search queries per probe account (0: no search rounds)")
	fs.IntVar(&o.Expunge, "expunge", 0, "INBOX messages per probe account and expunge round (0: no expunge rounds)")
	fs.BoolVar(&o.Convert, "convert", false, "convert the probe accounts' full-text index to layout 3 (needs layout 3: --fts-layout 3 or fts.layout 3, the default) and reshard it once more, counting the second round")
	fs.Int64Var(&o.Extrapolate, "extrapolate", 1000000, "scale the per-account means to this many accounts (0: no)")
	fs.IntVar(&ec.DataChunks, "ec-k", 2, "data chunks of the data pool's erasure code profile")
	fs.IntVar(&ec.CodingChunks, "ec-m", 1, "coding chunks of the data pool's erasure code profile")
	fs.Int64Var(&ec.StripeUnit, "stripe-unit", 4096, "stripe unit of the data pool's erasure code profile")
	fs.Int64Var(&ec.MinAlloc, "min-alloc", 4096, "BlueStore allocation unit of the data pool's OSDs")
	fs.StringVar(&o.TraceDir, "trace-dir", filepath.Join(os.TempDir(), "shardpost-bench"), "directory of the objecter logs")
	fs.BoolVar(&o.KeepTraces, "keep-traces", false, "keep the objecter logs")

	if code := parseBenchFlags(fs, args); code >= 0 {
		return code
	}

	sizes, err := bench.ParseSizes(f.sizes)
	if err != nil || !measureValid(&o, &f, settings.FTS.Layout) {
		log.Error("bench: invalid options", "err", err)
		return benchRefused
	}

	g, cfg, store, code := benchSetup(ctx, log, settings, &f)
	if store == nil {
		return code
	}
	defer cephbench.Close(store)

	o.Guard, o.Config, o.Store, o.EC, o.Log, o.Parallel = g, cfg, store, ec, log, f.parallel
	o.Gen = bench.Generator{Seed: f.seed, Sizes: sizes}
	o.NewDeliverer = benchDeliverer(log, settings)

	rep, err := cephbench.Measure(ctx, o)
	if rep != nil {
		code = writeBenchReport(f.jsonOut, rep, func(w io.Writer) error { return bench.WriteMeasureTable(w, rep) })
	}

	return benchExit(log, err, code)
}

// effectiveLayout is the fts.layout the bench runs with: --fts-layout
// when given, otherwise the configuration's (default 3).
func (f *benchFlags) effectiveLayout(configured int) int {
	if f.ftsLayout != 0 {
		return f.ftsLayout
	}

	return configured
}

// measureValid checks the counts of measure (--convert needs layout 3:
// --fts-layout 3, or fts.layout 3 of the configuration without the
// flag).
func measureValid(o *cephbench.MeasureOptions, f *benchFlags, configured int) bool {
	counts := []int{o.ProbeAccounts, o.ProbeMessages, o.Contention, o.Search, o.Expunge}
	for _, n := range counts {
		if n < 0 {
			return false
		}
	}

	return o.Every > 0 && f.parallel > 0 && (!o.Convert || f.effectiveLayout(configured) == 3)
}

// writeBenchReport writes the table (to standard error when the JSON
// goes to standard output) and the JSON.
func writeBenchReport(jsonOut string, rep any, table func(io.Writer) error) int {
	tableOut := io.Writer(os.Stdout)
	if jsonOut == "-" {
		tableOut = os.Stderr
	}

	if err := table(tableOut); err != nil {
		return benchFailed
	}

	if jsonOut != "" {
		if err := bench.WriteJSON(os.Stdout, jsonOut, rep); err != nil {
			fmt.Fprintf(os.Stderr, "write %s: %v\n", jsonOut, err)
			return benchFailed
		}
	}

	return benchOK
}

func benchExit(log *slog.Logger, err error, code int) int {
	switch {
	case errors.Is(err, bench.ErrGuard) || errors.Is(err, cephbench.ErrNotBenchPool):
		log.Error("bench: refused", "err", err)
		return benchRefused
	case err != nil:
		log.Error("bench: failed", "err", err)
		return benchFailed
	}

	return code
}
