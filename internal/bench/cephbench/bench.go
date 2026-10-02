package cephbench

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ceph/go-ceph/rados"

	"shardpost/internal/bench"
	"shardpost/internal/cephstore"
	"shardpost/internal/model"
)

// DrainTimeout bounds the wait for the views of a store to finish
// before it is closed (cephstore.Store.Drain).
const DrainTimeout = time.Minute

// Close closes a store once its views have finished; a store whose
// views do not finish within DrainTimeout is left open (the process
// ends anyway).
func Close(s *cephstore.Store) {
	if s.Drain(DrainTimeout) {
		s.Close()
	}
}

// DeliveryStore is the RADOS store as the delivery service sees it, with
// the server's refusal of accounts under an import lock.
type DeliveryStore struct{ *cephstore.Store }

// User opens an account view.
func (s DeliveryStore) User(name string) (model.User, error) {
	u, err := s.Store.User(name)
	if err != nil {
		return nil, err
	}

	if err := model.RefuseDuringImport(u); err != nil {
		u.Close()
		return nil, err
	}

	return u, nil
}

// Populate checks the guard and the pools (marking them on first use)
// and fills the accounts (bench.Populate).
func Populate(ctx context.Context, g bench.Guard, s *cephstore.Store, cfg cephstore.Config, d bench.Deliverer, o bench.PopulateOptions) (*bench.PopulateReport, error) {
	if err := checkConfig(g, cfg); err != nil {
		return nil, err
	}

	if o.Prefix != g.Prefix {
		return nil, fmt.Errorf("%w: prefix %q is not the guarded %q", bench.ErrGuard, o.Prefix, g.Prefix)
	}

	if err := checkPools(s, cfg.IndexPool, cfg.DataPool, o.Prefix, true); err != nil {
		return nil, err
	}

	return bench.Populate(ctx, o, DeliveryStore{s}, d)
}

// MeasureOptions describe a measurement.
type MeasureOptions struct {
	Guard bench.Guard
	// Config is the configuration of Store; the traced rounds open
	// stores of their own with it.
	Config cephstore.Config
	Store  *cephstore.Store
	// NewDeliverer builds the delivery service on a store.
	NewDeliverer func(*cephstore.Store) bench.Deliverer
	Gen          bench.Generator
	// Every samples the inventory: every Every-th account by number.
	Every    int
	Parallel int
	// ProbeAccounts accounts get ProbeMessages messages in each of the
	// two delivery rounds (0: no delivery probe); the same accounts are
	// opened in the open rounds.
	ProbeAccounts, ProbeMessages int
	// Contention, when above 0, adds two delivery rounds in which
	// Contention deliveries at a time go into one account (each of them
	// ProbeMessages messages long), to measure compare-and-swap
	// contention on the objects of one account.
	Contention int
	// Search, when above 0, runs that many search queries
	// (bench.SearchQueries) in each probe account, before the deliveries.
	Search int
	// Expunge, when above 0, expunges that many of the oldest INBOX
	// messages of each probe account in each of two rounds, at the end.
	Expunge int
	// EC are the parameters of the data pool's raw space estimate.
	EC bench.ECEstimate
	// Extrapolate scales the inventory to this many accounts (0: no).
	Extrapolate int64
	// TraceDir receives the objecter logs (they are removed afterwards
	// unless KeepTraces).
	TraceDir   string
	KeepTraces bool
	Log        *slog.Logger
}

// Measure takes the inventory and counts the operations of deliveries
// and account opens.
func Measure(ctx context.Context, o MeasureOptions) (*bench.MeasureReport, error) {
	if err := checkConfig(o.Guard, o.Config); err != nil {
		return nil, err
	}

	if o.Log == nil {
		o.Log = slog.New(slog.DiscardHandler)
	}

	cfg := o.Config
	if err := checkPools(o.Store, cfg.IndexPool, cfg.DataPool, o.Guard.Prefix, false); err != nil {
		return nil, err
	}

	r := &bench.MeasureReport{Prefix: o.Guard.Prefix, IndexPool: cfg.IndexPool, DataPool: cfg.DataPool,
		FTSShards: orInt(cfg.FTSShards, cephstore.DefaultFTSShards), ShardSize: cfg.ShardSize, ChunkSize: cfg.ChunkSize}

	inv, err := inventoryOf(o)
	if err != nil {
		return nil, err
	}

	r.Inventory, r.StoreWide, r.DataRaw = bench.Summarize(inv.Accounts), inv.StoreWide, inv.DataRaw
	r.Extrapolate(o.Extrapolate)

	if inv.Skipped > 0 {
		r.Notes = append(r.Notes, fmt.Sprintf("%d namespaces outside the sample were skipped", inv.Skipped))
	}

	if err := probe(ctx, o, r, probeAccounts(inv.Accounts, o.ProbeAccounts)); err != nil {
		return r, err
	}

	err = o.Store.WithConn(func(conn *rados.Conn) error {
		var err error

		r.Pools, err = poolStats(conn, cfg.IndexPool, cfg.DataPool)

		return err
	})

	return r, err
}

func orInt(v, def int) int {
	if v <= 0 {
		return def
	}

	return v
}

func inventoryOf(o MeasureOptions) (*InventoryResult, error) {
	var inv *InventoryResult

	start := time.Now()

	err := o.Store.WithConn(func(conn *rados.Conn) error {
		var err error

		inv, err = takeInventory(conn, o.Config.IndexPool, o.Config.DataPool, o.Guard.Prefix, o.Every, o.Parallel, o.EC)

		return err
	})
	if err == nil {
		o.Log.Info("bench: inventory taken", "accounts", len(inv.Accounts), "seconds", time.Since(start).Seconds())
	}

	return inv, err
}

// probeAccounts picks n of the inventoried accounts, spread evenly.
func probeAccounts(accts []bench.AccountUsage, n int) []string {
	if n <= 0 || len(accts) == 0 {
		return nil
	}

	n = min(n, len(accts))
	out := make([]string, 0, n)

	for i := range n {
		out = append(out, accts[i*len(accts)/n].Account)
	}

	return out
}

// probe runs the search, delivery, open and expunge rounds, in that
// order: the search sees the accounts as they were left (a run without
// deliveries and expunges finds what the previous one found), the
// expunge removes messages last.
func probe(ctx context.Context, o MeasureOptions, r *bench.MeasureReport, accounts []string) error {
	if len(accounts) == 0 {
		return nil
	}

	m := &measurer{o: o, accounts: accounts}

	if err := os.MkdirAll(o.TraceDir, 0o700); err != nil {
		return fmt.Errorf("trace directory: %w", err)
	}

	err := m.probeSearch(ctx, r)
	if err == nil {
		err = m.probeDeliveries(ctx, r)
	}

	if err == nil {
		err = m.probeOpens(ctx, r)
	}

	if err == nil {
		err = m.probeExpunge(ctx, r)
	}

	if m.unparsed > 0 {
		r.Notes = append(r.Notes, fmt.Sprintf("%d objecter log lines had an unknown format and were not counted; the operation counts are too low", m.unparsed))
	}

	return err
}

func (m *measurer) probeSearch(ctx context.Context, r *bench.MeasureReport) error {
	if m.o.Search <= 0 {
		return nil
	}

	s, err := m.search(ctx, m.o.Search)
	if err != nil {
		return err
	}

	r.Search = s
	if s.Mismatch {
		r.Notes = append(r.Notes, "the two search rounds found different messages; the accounts changed during the probe")
	}

	return nil
}

func (m *measurer) probeDeliveries(ctx context.Context, r *bench.MeasureReport) error {
	o := m.o
	if o.ProbeMessages <= 0 {
		return nil
	}

	d, err := m.deliveries(ctx, 1)
	if err != nil {
		return err
	}

	r.Delivery = d
	r.Notes = append(r.Notes, fmt.Sprintf("the delivery probe added %d messages to each of %d accounts (after the inventory)",
		2*o.ProbeMessages, len(m.accounts)))

	if o.Contention <= 0 {
		return nil
	}

	if d, err = m.deliveries(ctx, o.Contention); err != nil {
		return err
	}

	r.Contention = d
	r.Notes = append(r.Notes, fmt.Sprintf("the contention probe added %d messages to each of %d accounts, %d at a time",
		2*o.Contention*o.ProbeMessages, len(m.accounts), o.Contention))

	return nil
}

func (m *measurer) probeOpens(ctx context.Context, r *bench.MeasureReport) error {
	var err error

	if r.UserOpen, err = m.open(ctx, "Store.User() and Close()", openOnly); err != nil {
		return err
	}

	r.UserOpen.HeapBytes = m.heapPerView()
	r.SessionOpen, err = m.open(ctx, "Store.User(), ListMailboxes(), Info(INBOX), Close()", openSession)

	return err
}

func (m *measurer) probeExpunge(ctx context.Context, r *bench.MeasureReport) error {
	if m.o.Expunge <= 0 {
		return nil
	}

	e, err := m.expunge(ctx, m.o.Expunge)
	if err != nil {
		return err
	}

	r.Expunge = e
	r.Notes = append(r.Notes, fmt.Sprintf("the expunge probe removed the %d oldest INBOX messages of each of %d accounts",
		2*m.o.Expunge, len(m.accounts)))

	return nil
}

// measurer runs the rounds of a measurement.
type measurer struct {
	o        MeasureOptions
	accounts []string
	traces   int
	// unparsed counts the objecter log lines of an unknown format.
	unparsed int
}

// round runs fn once per account (and unit) on the untraced store,
// recording the latency of each call, and once more on a store of its
// own with the objecter log on, counting the operations. With p above 1,
// p workers run the units of one account at the same time (each of them
// units calls). It returns the calls of one round and its wall time.
func (m *measurer) round(ctx context.Context, units, p int, fn func(s *cephstore.Store, account string, unit int) error) (*bench.Latency, bench.OpCounts, int64, time.Duration, error) {
	lat := &bench.Latency{}

	var n atomic.Int64

	start := time.Now()

	err := m.each(ctx, units, p, func(account string, unit int) error {
		start := time.Now()

		if err := fn(m.o.Store, account, unit); err != nil {
			return err
		}

		lat.Add(time.Since(start))
		n.Add(1)

		return nil
	})
	if err != nil {
		return nil, nil, 0, 0, err
	}

	took := time.Since(start)
	ops, err := m.traced(ctx, units, p, fn)

	return lat, ops, n.Load(), took, err
}

// each runs fn for every account, units times, the accounts one after
// the other; p workers run the units of an account at the same time
// (each of them units calls, unit counting them per worker).
func (m *measurer) each(ctx context.Context, units, p int, fn func(account string, unit int) error) error {
	for _, a := range m.accounts {
		if err := eachUnit(ctx, units, max(p, 1), func(u int) error { return fn(a, u) }); err != nil {
			return fmt.Errorf("account %s: %w", a, err)
		}
	}

	return nil
}

// eachUnit runs fn units times in each of p workers and returns the
// first error; a worker stops at its first error or when ctx ends.
func eachUnit(ctx context.Context, units, p int, fn func(unit int) error) error {
	if p == 1 {
		for u := range units {
			if err := ctx.Err(); err != nil {
				return err
			}

			if err := fn(u); err != nil {
				return err
			}
		}

		return nil
	}

	var (
		wg   sync.WaitGroup
		once sync.Once
		werr error
	)

	for range p {
		wg.Add(1)

		go func() {
			defer wg.Done()

			if err := eachUnit(ctx, units, 1, fn); err != nil {
				once.Do(func() { werr = err })
			}
		}()
	}

	wg.Wait()

	return werr
}

// traced runs fn for every account on a store with the objecter log and
// counts the operations from the log.
func (m *measurer) traced(ctx context.Context, units, p int, fn func(s *cephstore.Store, account string, unit int) error) (bench.OpCounts, error) {
	m.traces++
	path := filepath.Join(m.o.TraceDir, fmt.Sprintf("objecter-%d-%d.log", os.Getpid(), m.traces))

	cfg := m.o.Config
	cfg.ClientOptions = bench.ObjecterOptions(path)

	s, err := cephstore.Open(cfg, m.o.Log)
	if err != nil {
		return nil, err
	}

	err = m.each(ctx, units, p, func(a string, u int) error { return fn(s, a, u) })

	var ids map[int64]string

	if err == nil {
		err = s.WithConn(func(conn *rados.Conn) error {
			var err error

			ids, err = poolIDs(conn, cfg.IndexPool, cfg.DataPool)

			return err
		})
	}

	// What the views leave to the background after Close (intent
	// removal, log trimming) belongs to what was measured, and the log
	// is complete once the connection is shut down. A connection whose
	// views do not finish stays open: shutting it down would crash.
	if !s.Drain(DrainTimeout) {
		return nil, errors.New("the views of the traced round did not finish")
	}

	s.Close()

	if err != nil {
		return nil, err
	}

	if !m.o.KeepTraces {
		defer func() { _ = os.Remove(path) }()
	}

	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("objecter log: %w", err)
	}
	defer func() { _ = f.Close() }()

	ops, unparsed, err := bench.ParseObjecterLog(f, ids, nil)
	m.unparsed += unparsed

	return ops, err
}

// deliveries runs the two delivery rounds, p deliveries at a time into
// each account (1: one after the other).
// The messages continue each account's numbering, as a further populate
// would, and go to INBOX through Sieve.
func (m *measurer) deliveries(ctx context.Context, p int) (*bench.ProbeReport, error) {
	next := map[string]int{}

	for _, a := range m.accounts {
		n, err := messageCount(m.o.Store, a)
		if err != nil {
			return nil, fmt.Errorf("delivery probe: account %s: %w", a, err)
		}

		next[a] = n
	}

	var mu sync.Mutex

	deliver := func(s *cephstore.Store, account string, _ int) error {
		mu.Lock()
		n := next[account]
		next[account] = n + 1
		mu.Unlock()

		raw, _ := m.o.Gen.Message(account, n)
		_, err := m.o.NewDeliverer(s).Deliver(ctx, bench.Request(account, raw, ""))

		return err
	}

	cas0 := bench.CASRetries()

	lat, ops, n, took, err := m.round(ctx, m.o.ProbeMessages, p, deliver)
	if err != nil {
		return nil, fmt.Errorf("delivery probe: %w", err)
	}

	// The counter holds the retries of both rounds (the traced round
	// delivers as many messages).
	r := &bench.ProbeReport{Accounts: len(m.accounts), Parallel: p, Deliveries: n, Latency: lat.Summary(),
		CASRetries: bench.CASDelta(cas0, bench.CASRetries(), 2*n)}
	r.Ops, r.Total = bench.Rates(ops, n)

	if took > 0 {
		r.PerSecond = float64(n) / took.Seconds()
	}

	return r, nil
}

func messageCount(s *cephstore.Store, account string) (int, error) {
	u, err := s.User(account)
	if err != nil {
		return 0, err
	}
	defer u.Close()

	q, err := u.QuotaUsage()

	return int(q.Msgs), err
}

// The open rounds.
func openOnly(s *cephstore.Store, account string, _ int) error {
	u, err := s.User(account)
	if err != nil {
		return err
	}

	u.Close()

	return nil
}

func openSession(s *cephstore.Store, account string, _ int) error {
	u, err := s.User(account)
	if err != nil {
		return err
	}
	defer u.Close()

	if _, err := u.ListMailboxes(); err != nil {
		return err
	}

	_, err = u.Info("INBOX")

	return err
}

func (m *measurer) open(ctx context.Context, what string, fn func(*cephstore.Store, string, int) error) (*bench.OpenReport, error) {
	lat, ops, n, _, err := m.round(ctx, 1, 1, fn)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", what, err)
	}

	o := &bench.OpenReport{What: what, Count: int(n), Latency: lat.Summary()}
	o.Ops, o.Total = bench.Rates(ops, n)

	return o, nil
}

// heapPerView is the heap an open view holds, from opening all probe
// accounts at once.
func (m *measurer) heapPerView() int64 {
	var before, after runtime.MemStats

	runtime.GC()
	runtime.ReadMemStats(&before)

	views := make([]*cephstore.User, 0, len(m.accounts))

	for _, a := range m.accounts {
		if u, err := m.o.Store.User(a); err == nil {
			views = append(views, u)
		}
	}

	runtime.GC()
	runtime.ReadMemStats(&after)

	for _, u := range views {
		u.Close()
	}

	if len(views) == 0 || after.HeapAlloc < before.HeapAlloc {
		return 0
	}

	return int64(after.HeapAlloc-before.HeapAlloc) / int64(len(views))
}
