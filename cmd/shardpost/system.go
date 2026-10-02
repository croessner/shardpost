package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"shardpost/internal/cephstore"
	"shardpost/internal/config"
	"shardpost/internal/logging"
)

// "shardpost [--config <file>] system status|init|migrate|verify|abort|
// revert|cleanup [flags]" places the store's own namespaces in a system
// pool and takes that back (temp/12 §5; shardpost-system(1),
// docs/operations/README.md "System pool"). The commands talk to the
// pools directly, never register in the admin store, and print their
// report as JSON on standard output; the log goes to standard error.

// Exit codes of the system tools.
const (
	systemOK       = 0
	systemFailed   = 1 // an error; the command can be run again
	systemRefused  = 2 // usage, or a precondition that does not hold
	systemMismatch = 3 // a check found the pools different from what they must be
)

const commandSystem = "system"

// Subcommands of system.
const (
	systemStatus  = "status"
	systemInit    = "init"
	systemMigrate = "migrate"
	systemVerify  = "verify"
	systemAbort   = "abort"
	systemRevert  = "revert"
	systemCleanup = "cleanup"
)

// systemFlags are the options of the system subcommands.
type systemFlags struct {
	pool, to, run, confirm                                      string
	deep, wait, check, force, sourceOnly, delCopies, delSurplus bool
	parallel                                                    int
	minAge                                                      time.Duration
}

// register adds the flags of subcommand sub.
func (f *systemFlags) register(fs *flag.FlagSet, sub string) {
	f.registerOwn(fs, sub)

	if sub == systemStatus {
		return
	}

	fs.StringVar(&f.confirm, "confirm", "", "confirm a destructive step by naming its run")
	fs.BoolVar(&f.wait, "wait", false, "take over the lease of another tool once it was idle for three minutes (only when its job is known to be gone)")

	if sub != systemInit {
		fs.IntVar(&f.parallel, "parallel", cephstore.DefaultSystemParallel, "objects copied or hashed at a time")
	}
}

// Flag help shared by several subcommands.
const (
	helpRun     = "the run to work on (required; shardpost system status shows it)"
	helpSurplus = "delete objects at the old place that must not be there (with --confirm)"
)

// registerOwn adds the flags only sub has.
func (f *systemFlags) registerOwn(fs *flag.FlagSet, sub string) {
	switch sub {
	case systemStatus:
		fs.BoolVar(&f.deep, "deep", false, "count the objects of the store's namespaces in both pools and of the quarantines (lists the pools)")
	case systemInit:
		fs.StringVar(&f.pool, "pool", "", "the system pool to initialize (required)")
	case systemMigrate:
		fs.StringVar(&f.to, "to", "", "the system pool to migrate into (required)")
		fs.BoolVar(&f.check, "check", false, "only check the preconditions; running processes are reported, not refused")
		fs.BoolVar(&f.force, "force-registrations", false, "ignore registrations of processes that run (only for processes known to be dead)")
	case systemVerify:
		fs.BoolVar(&f.sourceOnly, "source-only", false, "check the old place after the migration instead of source against copy")
		fs.BoolVar(&f.delSurplus, "delete-surplus", false, helpSurplus)
	case systemAbort:
		fs.StringVar(&f.run, "run", "", helpRun)
		fs.BoolVar(&f.delCopies, "delete-copies", false, "also delete the copies in the system pool (with --confirm)")
		fs.BoolVar(&f.delSurplus, "delete-surplus", false, helpSurplus)
	case systemRevert:
		fs.StringVar(&f.run, "run", "", helpRun)
		fs.BoolVar(&f.delSurplus, "delete-surplus", false, helpSurplus)
	case systemCleanup:
		fs.StringVar(&f.run, "run", "", helpRun)
		fs.DurationVar(&f.minAge, "min-age", cephstore.DefaultCleanupAge, "how long ago the run must have ended")
		fs.BoolVar(&f.delSurplus, "delete-surplus", false, helpSurplus)
	}
}

// validate refuses missing or contradicting flags.
func (f *systemFlags) validate(sub string) error {
	if err := f.required(sub); err != nil {
		return err
	}

	if f.delSurplus && sub == systemVerify && !f.sourceOnly {
		return errors.New("--delete-surplus belongs to verify --source-only")
	}

	if sub != systemStatus && sub != systemInit && f.parallel < 1 {
		return fmt.Errorf("--parallel must be 1 to %d", cephstore.MaxSystemParallel)
	}

	return f.ranges()
}

// ranges refuses flags that contradict each other or are out of range.
func (f *systemFlags) ranges() error {
	switch {
	case (f.delSurplus || f.delCopies) && f.confirm == "":
		return errors.New("--delete-surplus and --delete-copies need --confirm <run>")
	case f.check && f.wait:
		return errors.New("migrate --check takes no lease; --wait does not apply")
	case f.parallel > cephstore.MaxSystemParallel:
		return fmt.Errorf("--parallel must be 1 to %d", cephstore.MaxSystemParallel)
	case f.minAge < 0:
		return errors.New("--min-age must not be negative")
	}

	return nil
}

// required refuses a subcommand without its required flags.
func (f *systemFlags) required(sub string) error {
	switch {
	case sub == systemInit && f.pool == "":
		return errors.New("init needs --pool")
	case sub == systemMigrate && f.to == "":
		return errors.New("migrate needs --to")
	case (sub == systemAbort || sub == systemRevert || sub == systemCleanup) && f.run == "":
		return fmt.Errorf("%s needs --run", sub)
	case (sub == systemRevert || sub == systemCleanup) && f.confirm == "":
		return fmt.Errorf("%s needs --confirm <run>", sub)
	}

	return nil
}

// target is the system pool the subcommand names ("" none).
func (f *systemFlags) target() string {
	if f.pool != "" {
		return f.pool
	}

	return f.to
}

// splitSystem finds "system" after the global options; it returns those
// options and the arguments after "system".
func splitSystem(args []string) ([]string, []string, bool) {
	return splitCommandAt(args, commandSystem)
}

// systemUsage is the usage line.
const systemUsage = "usage: shardpost [--config <file>] system status|init|migrate|verify|abort|revert|cleanup [flags]"

// runSystem runs a system subcommand and returns the exit code.
func runSystem(pre, args []string) int {
	log := logging.New(os.Stderr, "info").With("service", "shardpost-system")

	settings, _, err := loadConfig(pre)
	if err != nil {
		log.Error("system: configuration", "err", err)
		return systemRefused
	}

	log = logging.New(os.Stderr, settings.Logging.Level).With("service", "shardpost-system")

	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, systemUsage)
		return systemRefused
	}

	sub := args[0]

	var f systemFlags

	if code := parseSystemFlags(sub, args[1:], &f, os.Stderr); code >= 0 {
		return code
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	store, code := openSystemStore(ctx, log, settings, sub, &f)
	if store == nil {
		return code
	}

	defer func() {
		if !store.Drain(30 * time.Second) {
			log.Warn("system: views still open after 30 s")
		}

		store.Close()
	}()

	rep, err := runSystemCommand(ctx, store, log, sub, &f)

	return reportSystem(os.Stdout, log, rep, err)
}

// parseSystemFlags parses the flags of sub, with messages to w; it
// returns -1 to go on, else the exit code.
func parseSystemFlags(sub string, args []string, f *systemFlags, w io.Writer) int {
	switch sub {
	case systemStatus, systemInit, systemMigrate, systemVerify, systemAbort, systemRevert, systemCleanup:
	default:
		_, _ = fmt.Fprintf(w, "unknown system command %q\n%s\n", sub, systemUsage)
		return systemRefused
	}

	fs := flag.NewFlagSet("shardpost system "+sub, flag.ContinueOnError)
	fs.SetOutput(w)
	f.register(fs, sub)

	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return systemOK
		}

		return systemRefused
	}

	if fs.NArg() > 0 {
		_, _ = fmt.Fprintf(w, "unexpected argument %q\n", fs.Arg(0))
		return systemRefused
	}

	if err := f.validate(sub); err != nil {
		_, _ = fmt.Fprintln(w, err)
		return systemRefused
	}

	return -1
}

// systemConfig is the store configuration of a system tool: opened as a
// tool, with the target as the system pool of an init (none else: the
// tools find it in the fence), and objecter throttles that let the
// parallel copies start.
func systemConfig(settings *config.Config, sub string, f *systemFlags) (cephstore.Config, error) {
	cfg := cephConfig(settings)
	cfg.PlacementTool = true

	if target := f.target(); target != "" && settings.Ceph.SystemPool != "" && settings.Ceph.SystemPool != target {
		return cfg, fmt.Errorf("ceph.system_pool is %q, the command names %q; run the tools with the system pool empty or the same",
			settings.Ceph.SystemPool, target)
	}

	switch sub {
	case systemInit:
		cfg.SystemPool = f.pool
	case systemStatus:
	default:
		cfg.SystemPool = ""
	}

	cfg.ClientOptions = toolClientOptions(cfg.ClientOptions, max(f.parallel, 1))

	return cfg, nil
}

// Objecter defaults of librados, which the tool raises for its parallel
// copies.
const (
	objecterOps   = 1024
	objecterBytes = 100 << 20
	toolChunk     = 4 << 20
	toolOpsPerJob = 4
)

// toolClientOptions adds objecter throttles for parallel workers where
// the configuration sets none; configured values are checked by the
// tool.
func toolClientOptions(opts map[string]string, parallel int) map[string]string {
	out := map[string]string{}
	for k, v := range opts {
		out[k] = v
	}

	if _, ok := out["objecter_inflight_ops"]; !ok {
		out["objecter_inflight_ops"] = strconv.Itoa(max(objecterOps, toolOpsPerJob*parallel))
	}

	if _, ok := out["objecter_inflight_op_bytes"]; !ok {
		out["objecter_inflight_op_bytes"] = strconv.FormatInt(max(objecterBytes, int64(parallel)*toolChunk), 10)
	}

	return out
}

// openSystemStore opens the store for a system tool (nil and the exit
// code on failure).
func openSystemStore(ctx context.Context, log *slog.Logger, settings *config.Config, sub string, f *systemFlags) (*cephstore.Store, int) {
	cfg, err := systemConfig(settings, sub, f)
	if err != nil {
		log.Error("system: refused", "err", err)
		return nil, systemRefused
	}

	rotate, err := rookCredentials(ctx, log, settings, &cfg)
	if err != nil {
		log.Error("system: credentials", "err", err)
		return nil, systemFailed
	}

	store, err := cephstore.Open(cfg, log)
	if err != nil {
		log.Error("system: connect", "err", err)
		return nil, systemFailed
	}

	if rotate != nil {
		rotate(store)
	}

	return store, systemOK
}

// initReport is the report of "system init".
type initReport struct {
	Command string `json:"command"`
	*cephstore.InitResult
}

// runSystemCommand runs the subcommand on the store.
func runSystemCommand(ctx context.Context, store *cephstore.Store, log *slog.Logger, sub string, f *systemFlags) (any, error) {
	o := cephstore.SystemOptions{Context: ctx, Log: log, By: "shardpost system " + sub, Wait: f.wait, Parallel: f.parallel,
		Run: f.run, Confirm: f.confirm, Check: f.check, ForceRegistrations: f.force, DeleteCopies: f.delCopies,
		DeleteSurplus: f.delSurplus, SourceOnly: f.sourceOnly, MinAge: f.minAge, Deep: f.deep}

	switch sub {
	case systemInit:
		res, err := store.InitSystem(cephstore.InitOptions{Confirm: f.confirm, Wait: f.wait, Log: log, By: o.By})
		return initReport{Command: systemInit, InitResult: res}, err
	case systemStatus:
		return store.SystemStatus(o)
	case systemMigrate:
		return store.MigrateSystem(f.to, o)
	case systemVerify:
		return store.VerifySystem(o)
	case systemAbort:
		return store.AbortSystem(o)
	case systemRevert:
		return store.RevertSystem(o)
	default:
		return store.CleanupSystem(o)
	}
}

// reportSystem prints the report and returns the exit code of err.
func reportSystem(w io.Writer, log *slog.Logger, rep any, err error) int {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")

	if eerr := enc.Encode(rep); eerr != nil {
		log.Error("system: report", "err", eerr)
	}

	code := systemExit(err)

	if err != nil {
		log.Error("system: "+exitText(code), "err", err, "exit", code)
	}

	return code
}

// systemExit is the exit code of the result of a system tool.
func systemExit(err error) int {
	switch {
	case err == nil:
		return systemOK
	case errors.Is(err, cephstore.ErrSystemMismatch):
		return systemMismatch
	case errors.Is(err, cephstore.ErrSystemRefused), errors.Is(err, cephstore.ErrInitRefused),
		errors.Is(err, cephstore.ErrFenceHeld), errors.Is(err, cephstore.ErrSystemPlacement):
		return systemRefused
	default:
		return systemFailed
	}
}

// exitText names an exit code in the log.
func exitText(code int) string {
	switch code {
	case systemRefused:
		return "refused"
	case systemMismatch:
		return "verification failed"
	default:
		return "failed"
	}
}
