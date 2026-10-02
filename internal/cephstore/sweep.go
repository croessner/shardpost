package cephstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"log/slog"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/ceph/go-ceph/rados"

	"shardpost/internal/model"
)

// The sweep ("shardpost scrub --sweep", scaling plan B7) finds what the
// intents cannot: debris that servers before 0.42.0 left without an
// intent, the objects of accounts that were purged before the purge
// named its objects, full-text shards above a lowered fts_shards. It is
// a tool for a one-off cleanup, not a routine: it lists both pools in all
// namespaces, which costs a walk over every placement group of each.
//
// Pass 1 lists a pool (all namespaces) and spills the object names into
// spill objects in "~accounts" ("sweep/<run>/<pool>/<nnn>", one omap key
// "<namespace>\x1f<object>" per object, the spill object chosen by the
// namespace), so that pass 2 sees the objects of one namespace together
// without holding them in memory. The listing is resumable: the
// iterator's position (the PG hash position, go-ceph Iter.Token) is
// checkpointed, and a resumed sweep seeks to it; when the pool's pg_num
// changed meanwhile the position means nothing any more and the pool's
// listing starts over (spilling a name twice changes nothing).
//
// Pass 2 reads each spill object in key order and, per namespace,
// compares the names with what the account's data model names (the
// enumeration of the purge, purge.go). What it does not name is a
// remnant: with --repair a remnant older than scrub.grace_seconds is
// deleted (a blob through collectBlob, under the version of its chunk
// 0). A namespace without registry is a remnant as a whole, unless the
// account directory still holds the account as active: a lost registry
// is reported, never cleaned up. Accounts under an import lock are
// skipped. The private \Seen marks of a live public folder that have no
// "ps:" key (written before 0.42.0) are adopted: the key is written.
//
// The state ("sweep/state") and a cls_lock ("sweep/lock") live in
// "~accounts" as well; one sweep runs at a time. Lock, state and spill
// stay in the index pool when the rest of "~accounts" lives in the
// system pool (ceph.system_pool): the spill holds a name per object of
// both pools and does not belong on a small pool. The account
// directory and the purged index are read where they are. The store's
// own namespaces, and the quarantine of a system pool migration
// ("~<namespace>.moved/<run>"), are reserved and never swept.

// DefaultSweepSpillShards is the number of spill objects per pool.
const DefaultSweepSpillShards = 256

// sweepClockMargin is the default of SweepOptions.ClockMargin (a variable
// for the tests).
var sweepClockMargin = time.Minute

const (
	sweepLockOID  = "sweep/lock"
	sweepStateOID = "sweep/state"
	sweepStateKey = "state"
	sweepSep      = "\x1f"
	sweepLockTTL  = 5 * time.Minute
	// sweepBatch is how many object names pass 1 spills between two
	// checkpoints; pass 2 checkpoints after every page.
	sweepBatch = 10000
	sweepPage  = 1000
	// sweepMaxErrors and sweepMaxSamples bound the report.
	sweepMaxErrors  = 50
	sweepMaxSamples = 20
)

// Remnant classes of the sweep report.
const (
	remnantBlob      = "blob"
	remnantMailbox   = "mailbox"
	remnantMarks     = "marks"
	remnantOther     = "other"
	remnantNamespace = "namespace" // an object of a namespace without registry
	remnantFTS       = "fts"       // a full-text shard above the configured count (kept)
)

// ftsShardObject reports whether oid is a shard of the full-text index
// whose number follows the configured shard count (fts/, ftsh/, ftsd/).
func ftsShardObject(oid string) bool {
	for _, p := range []string{ftsPrefix, headerPrefix, legacyDictPrefix} {
		if n, ok := strings.CutPrefix(oid, p); ok {
			_, err := strconv.Atoi(n)
			return err == nil
		}
	}

	return false
}

// SweepOptions tunes a sweep.
type SweepOptions struct {
	// Repair deletes the remnants (otherwise the sweep only reports).
	Repair bool
	// Grace leaves remnants younger than this alone.
	Grace time.Duration
	// SpillShards is the number of spill objects per pool of a new run
	// (0: DefaultSweepSpillShards).
	SpillShards int
	// Restart drops a sweep in progress and starts a new one.
	Restart bool
	// ClockMargin is how far before the moment a namespace is read the
	// sweep sets the line after which nothing is a remnant (0:
	// sweepClockMargin): object mtimes have whole seconds and come from
	// the writer's clock.
	ClockMargin time.Duration
	// Batch overrides sweepBatch (tests).
	Batch int
	Log   *slog.Logger
	// afterLoad runs after a namespace's data model was read (tests).
	afterLoad func(namespace string)
}

// SweepReport is what a sweep found and did.
type SweepReport struct {
	Run      string    `json:"run"`
	Started  time.Time `json:"started"`
	Finished time.Time `json:"finished,omitzero"`
	Repair   bool      `json:"repair"`
	// Listed counts the objects pass 1 listed, by pool ("index",
	// "data"); Restarts the pool listings that started over because
	// pg_num changed.
	Listed   map[string]int64 `json:"listed"`
	Restarts int              `json:"restarts,omitempty"`
	// Skipped counts the objects of namespaces that are no account
	// (the default namespace, the store's own).
	Skipped int64 `json:"skipped"`
	// Namespaces counts the namespaces pass 2 looked at; ImportLocked
	// the ones it left alone, RegistryMissing those without registry
	// that the directory holds as active, Unregistered those without
	// registry and directory entry; the objects of both are only
	// reported (class namespace).
	Namespaces      int64 `json:"namespaces"`
	ImportLocked    int64 `json:"import_locked,omitempty"`
	RegistryMissing int64 `json:"registry_missing,omitempty"`
	Unregistered    int64 `json:"unregistered,omitempty"`
	// Recreated counts the namespaces whose account was created again
	// while the sweep looked at them (nothing of them is deleted).
	Recreated int64 `json:"recreated,omitempty"`
	// Remnants counts the objects nothing names, by class; Removed
	// those deleted, Young those left for being younger than the grace.
	Remnants map[string]int64 `json:"remnants"`
	Removed  int64            `json:"removed"`
	Young    int64            `json:"young"`
	// Adopted counts the ps: keys written for marks of live public
	// folders; PurgedForgotten the records of purged accounts a
	// repairing run removed at its end.
	Adopted         int64 `json:"adopted,omitempty"`
	PurgedForgotten int64 `json:"purged_forgotten,omitempty"`
	Samples []string `json:"samples,omitempty"`
	Errors  []string `json:"errors,omitempty"`
}

func (r *SweepReport) sample(s string) {
	if len(r.Samples) < sweepMaxSamples {
		r.Samples = append(r.Samples, s)
	}
}

func (r *SweepReport) fail(err error) {
	if len(r.Errors) < sweepMaxErrors {
		r.Errors = append(r.Errors, err.Error())
	}
}

// sweepState is the checkpoint of a sweep.
type sweepState struct {
	Run    string `json:"run"`
	Shards int    `json:"shards"`
	// Pass is 1 (listing) or 2 (evaluation); Pool indexes sweepPools.
	Pass int `json:"pass"`
	Pool int `json:"pool"`
	// Token and PGNum are the listing position of pass 1 (valid with
	// Positioned) and the pg_num it belongs to.
	Positioned bool   `json:"positioned,omitempty"`
	Token      uint32 `json:"token"`
	PGNum      int    `json:"pgnum"`
	// Shard and After are the spill object and its last key pass 2 did.
	Shard  int         `json:"shard"`
	After  string      `json:"after"`
	// LastNS is the pool and namespace pass 2 counted last.
	LastNS string      `json:"last_ns,omitempty"`
	// Unclean are the purged namespaces the run could not clean
	// completely (young objects, failures); their records stay.
	Unclean map[string]bool `json:"unclean,omitempty"`
	Report SweepReport `json:"report"`
}

// The pools a sweep walks, by the name the report uses.
const (
	sweepIndexPool = "index"
	sweepDataPool  = "data"
)

var sweepPools = []string{sweepIndexPool, sweepDataPool}

// sweeper runs one sweep.
type sweeper struct {
	s     *Store
	o     SweepOptions
	log   *slog.Logger
	lost  func() bool
	ctx   context.Context
	acct  *rados.IOContext // "~accounts" of the index pool
	st    *sweepState
	pools map[string]string // report name -> pool name
}

// errSweepStopped ends a sweep whose lease was lost or whose context
// ended; the checkpoint lets the next one go on.
var errSweepStopped = errors.New("sweep stopped; run it again to continue")

// Sweep runs (or continues) the sweep; see the comment at the top.
func (s *Store) Sweep(ctx context.Context, o SweepOptions) (*SweepReport, error) {
	s.mu.RLock()
	index := s.cfg.IndexPool
	s.mu.RUnlock()

	lease, err := s.holdLockIn(index, model.AccountsNamespace, sweepLockOID, "sweep", "scrub sweep", sweepLockTTL, true)
	if err != nil {
		return nil, fmt.Errorf("sweep lock: %w", err)
	}
	defer lease.Release()

	sw, release, err := s.newSweeper(ctx, o, lease.Lost)
	if err != nil {
		return nil, err
	}
	defer release()

	if err := sw.begin(); err != nil {
		return nil, err
	}

	for sw.st.Pass == 1 {
		if err := sw.listPool(); err != nil {
			return &sw.st.Report, err
		}
	}

	for sw.st.Pass == 2 {
		if err := sw.evaluatePool(); err != nil {
			return &sw.st.Report, err
		}
	}

	// Every purged namespace the run looked at is clean now; records of
	// purges newer than the run's start (less its grace) stay for the
	// next one.
	if sw.o.Repair {
		n, err := s.forgetPurged(sw.st.Report.Started.Add(-sw.o.Grace), sw.st.Unclean)
		if err != nil {
			sw.st.Report.fail(err)
		}

		sw.st.Report.PurgedForgotten = int64(n)
	}

	rep := sw.st.Report
	rep.Finished = time.Now().UTC()

	return &rep, sw.dropState()
}

// newSweeper opens what a sweep works with.
func (s *Store) newSweeper(ctx context.Context, o SweepOptions, lost func() bool) (*sweeper, func(), error) {
	r := s.ref()

	s.mu.RLock()
	pools := map[string]string{sweepIndexPool: s.cfg.IndexPool, sweepDataPool: s.cfg.DataPool}
	s.mu.RUnlock()

	acct, err := r.conn.OpenIOContext(pools[sweepIndexPool])
	if err != nil {
		r.refs.Done()
		return nil, nil, err
	}

	acct.SetNamespace(model.AccountsNamespace)

	if o.Log == nil {
		o.Log = s.log
	}

	o.SpillShards = min(max(o.SpillShards, 0), 4096)
	if o.SpillShards == 0 {
		o.SpillShards = DefaultSweepSpillShards
	}

	if o.Batch <= 0 {
		o.Batch = sweepBatch
	}

	if o.ClockMargin <= 0 {
		o.ClockMargin = sweepClockMargin
	}

	if o.Repair {
		o.Grace = repairGrace(o.Grace)
	}

	sw := &sweeper{s: s, o: o, log: o.Log.With("component", "sweep"), lost: lost, ctx: ctx, acct: acct, pools: pools}

	return sw, func() { acct.Destroy(); r.refs.Done() }, nil
}

// begin loads the state of a sweep in progress or starts a new one.
func (sw *sweeper) begin() error {
	st, err := sw.loadState()
	if err != nil {
		return err
	}

	// A finished run whose state was not removed starts over as well.
	if st != nil && (sw.o.Restart || st.Pass < 1 || st.Pass > 2 || st.Pool < 0 || st.Pool >= len(sweepPools)) {
		sw.dropSpill(st)

		st = nil
	}

	if st == nil {
		st = &sweepState{Run: newGUID()[:12], Shards: sw.o.SpillShards, Pass: 1,
			Report: SweepReport{Started: time.Now().UTC(), Listed: map[string]int64{}, Remnants: map[string]int64{}}}
		sw.log.Info("sweep started", "run", st.Run, "spill_shards", st.Shards, "repair", sw.o.Repair)
	} else {
		sw.log.Info("sweep continues", "run", st.Run, "pass", st.Pass, "pool", sweepPools[st.Pool])
	}

	st.Report.Run, st.Report.Repair = st.Run, sw.o.Repair
	sw.st = st

	return sw.saveState()
}

func (sw *sweeper) loadState() (*sweepState, error) {
	vals, err := getOmapKeys(sw.acct, sweepStateOID, []string{sweepStateKey})
	if err != nil {
		return nil, fmt.Errorf("sweep state: %w", err)
	}

	v, ok := vals[sweepStateKey]
	if !ok {
		return nil, nil
	}

	var st sweepState
	if err := json.Unmarshal(v, &st); err != nil || st.Run == "" || st.Shards <= 0 {
		return nil, nil // unreadable: start over
	}

	return &st, nil
}

func (sw *sweeper) saveState() error {
	op := rados.CreateWriteOp()
	defer op.Release()

	op.SetOmap(map[string][]byte{sweepStateKey: encode(sw.st)})

	if err := op.Operate(sw.acct, sweepStateOID, rados.OperationNoFlag); err != nil {
		return fmt.Errorf("sweep state: %w", err)
	}

	return nil
}

func (sw *sweeper) dropState() error {
	if err := sw.acct.Delete(sweepStateOID); err != nil && errCode(err) != syscall.ENOENT {
		return fmt.Errorf("sweep state: %w", err)
	}

	sw.log.Info("sweep finished", "run", sw.st.Run, "report", string(encode(sw.st.Report)))

	return nil
}

// dropSpill deletes the spill objects of a run.
func (sw *sweeper) dropSpill(st *sweepState) {
	for _, pool := range sweepPools {
		for n := range st.Shards {
			_ = sw.acct.Delete(spillOID(st.Run, pool, n))
		}
	}
}

// spillOID names spill object n of a pool.
func spillOID(run, pool string, n int) string { return fmt.Sprintf("sweep/%s/%s/%04d", run, pool, n) }

// spillShard is the spill object of a namespace.
func spillShard(ns string, shards int) int {
	h := fnv.New32a()
	_, _ = h.Write([]byte(ns))

	return int(h.Sum32() % uint32(shards))
}

// stopped reports whether the sweep has to stop (lease lost, context
// ended).
func (sw *sweeper) stopped() bool { return sw.lost() || sw.ctx.Err() != nil }

// sweptNamespace reports whether objects of the namespace are looked at:
// accounts and the public folders, not the store's own namespaces nor
// the default one.
func sweptNamespace(ns string) bool {
	return ns != "" && (!model.ReservedNamespace(ns) || model.IsPublic(ns))
}

// --- pass 1 ----------------------------------------------------------------

// listPool lists the pool of the state into its spill objects, from the
// checkpointed position, and moves the state on.
func (sw *sweeper) listPool() error {
	st := sw.st
	pool := sweepPools[st.Pool]

	pg, err := sw.s.poolPGNum(sw.pools[pool])
	if err != nil {
		return err
	}

	ioctx, release, err := sw.s.openPool(sw.pools[pool])
	if err != nil {
		return err
	}
	defer release()

	ioctx.SetNamespace(rados.AllNamespaces)

	it, err := listObjects(ioctx, listSweep)
	if err != nil {
		return err
	}
	defer it.Close()

	sw.seek(it, pg, pool)

	if err := sw.spill(it, pool); err != nil {
		return err
	}

	st.Pool, st.Positioned, st.Token, st.PGNum = st.Pool+1, false, 0, 0
	if st.Pool == len(sweepPools) {
		st.Pass, st.Pool = 2, 0
	}

	return sw.saveState()
}

// seek moves the iterator to the checkpointed position, unless the
// pool's pg_num changed since (then the listing starts over).
func (sw *sweeper) seek(it *rados.Iter, pg int, pool string) {
	st := sw.st

	switch {
	case !st.Positioned:
	case st.PGNum == pg:
		it.Seek(rados.IterToken(st.Token))
	default:
		sw.log.Warn("sweep: pg_num changed, listing the pool again", "pool", pool, "was", st.PGNum, "now", pg)

		st.Report.Restarts++
	}

	st.PGNum = pg
}

// spill writes the names the iterator lists into the spill objects and
// checkpoints every batch.
func (sw *sweeper) spill(it *rados.Iter, pool string) error {
	st := sw.st
	buf := map[int]map[string][]byte{}
	n := 0

	for it.Next() {
		ns, oid := it.Namespace(), it.Value()
		st.Report.Listed[pool]++

		if !sweptNamespace(ns) {
			st.Report.Skipped++
			continue
		}

		shard := spillShard(ns, st.Shards)
		if buf[shard] == nil {
			buf[shard] = map[string][]byte{}
		}

		buf[shard][ns+sweepSep+oid] = nil
		n++

		if n < sw.o.Batch {
			continue
		}

		if err := sw.flushSpill(pool, buf, uint32(it.Token()), true); err != nil {
			return err
		}

		buf, n = map[int]map[string][]byte{}, 0
	}

	if err := it.Err(); err != nil {
		return fmt.Errorf("list %s pool: %w", pool, err)
	}

	return sw.flushSpill(pool, buf, 0, false)
}

// flushSpill writes the buffered names and checkpoints the position
// (positioned: token is one; otherwise the pool is done).
func (sw *sweeper) flushSpill(pool string, buf map[int]map[string][]byte, token uint32, positioned bool) error {
	if sw.stopped() {
		return errSweepStopped
	}

	for shard, keys := range buf {
		op := rados.CreateWriteOp()
		op.SetOmap(keys)
		err := op.Operate(sw.acct, spillOID(sw.st.Run, pool, shard), rados.OperationNoFlag)
		op.Release()

		if err != nil {
			return fmt.Errorf("spill: %w", err)
		}
	}

	sw.st.Token, sw.st.Positioned = token, positioned

	return sw.saveState()
}

// poolPGNum asks the monitors for the pg_num of a pool (mon "allow r").
func (s *Store) poolPGNum(pool string) (int, error) {
	r := s.ref()
	defer r.refs.Done()

	cmd, _ := json.Marshal(map[string]string{"prefix": "osd pool get", "pool": pool, "var": "pg_num", "format": "json"})

	out, _, err := r.conn.MonCommand(cmd)
	if err != nil {
		return 0, fmt.Errorf("pg_num of %s: %w", pool, err)
	}

	var res struct {
		PGNum int `json:"pg_num"`
	}

	if err := json.Unmarshal(out, &res); err != nil {
		return 0, fmt.Errorf("pg_num of %s: %w", pool, err)
	}

	return res.PGNum, nil
}

// openPool opens an IO context of a pool.
func (s *Store) openPool(pool string) (*rados.IOContext, func(), error) {
	r := s.ref()

	ioctx, err := r.conn.OpenIOContext(pool)
	if err != nil {
		r.refs.Done()
		return nil, nil, fmt.Errorf("open pool %q: %w", pool, err)
	}

	return ioctx, func() { ioctx.Destroy(); r.refs.Done() }, nil
}

// --- pass 2 ----------------------------------------------------------------

// evaluatePool works through the spill objects of the state's pool and
// moves the state on.
func (sw *sweeper) evaluatePool() error {
	st := sw.st
	pool := sweepPools[st.Pool]

	for st.Shard < st.Shards {
		if err := sw.evaluateShard(pool); err != nil {
			return err
		}

		_ = sw.acct.Delete(spillOID(st.Run, pool, st.Shard))
		st.Shard, st.After = st.Shard+1, ""

		if err := sw.saveState(); err != nil {
			return err
		}
	}

	st.Pool, st.Shard = st.Pool+1, 0
	if st.Pool == len(sweepPools) {
		st.Pass = 3 // done
	}

	return sw.saveState()
}

// evaluateShard reads one spill object page by page, one namespace at a
// time, and checkpoints after every page.
func (sw *sweeper) evaluateShard(pool string) error {
	st := sw.st
	b := &radosOmap{ioctx: sw.acct}
	oid := spillOID(st.Run, pool, st.Shard)

	var ns *sweepNamespace

	defer func() { ns.close() }()

	for {
		page, more, err := b.ReadPage(oid, st.After, sweepPage)
		if err != nil {
			return fmt.Errorf("read spill %s: %w", oid, err)
		}

		for _, kv := range page {
			name, obj, _ := strings.Cut(kv.Key, sweepSep)

			if ns == nil || ns.name != name {
				ns.finish()
				ns.close()

				ns = sw.openNamespace(name, pool)
			}

			ns.check(obj)

			st.After = kv.Key
		}

		// The chunks of the last blob may go on on the next page.
		done := !more || len(page) == 0
		ns.flush(!done)

		if sw.stopped() {
			return errSweepStopped
		}

		if err := sw.saveState(); err != nil {
			return err
		}

		if done {
			return nil
		}
	}
}

// sweepNamespace is what pass 2 knows of one namespace of one pool.
type sweepNamespace struct {
	sw    *sweeper
	name  string
	data  bool
	u     *User
	skip  bool          // left alone (import lock, error)
	keep  bool          // reported only (no registry, not being purged)
	// opened is when load started: nothing written after it is a
	// remnant.
	opened time.Time
	// dropped: the namespace stopped being cleanable while the sweep
	// looked at it; the rest of it is not looked at.
	dropped bool
	named  *namedObjects // nil: no registry, everything is a remnant
	index map[string]bool
	live  map[string]bool // public folders, read on demand
	// remnants found and not yet handled: index objects, and blobs by
	// GUID with their highest chunk number + 1.
	objects  []string
	blobs    map[string]int
	lastGUID string
}

// openNamespace prepares the evaluation of a namespace.
func (sw *sweeper) openNamespace(name, pool string) *sweepNamespace {
	ns := &sweepNamespace{sw: sw, name: name, data: pool == sweepDataPool, blobs: map[string]int{}}
	rep := &sw.st.Report

	// A namespace a resumed run opens again is counted once.
	if key := pool + sweepSep + name; sw.st.LastNS != key {
		rep.Namespaces++
		sw.st.LastNS = key
	}

	u, err := sw.s.User(name)
	if err != nil {
		rep.fail(fmt.Errorf("namespace %q: %w", name, err))

		ns.skip = true
		ns.unclean()

		return ns
	}

	ns.u = u

	if err := ns.load(); err != nil {
		rep.fail(fmt.Errorf("namespace %q: %w", name, err))

		ns.skip = true
		ns.unclean()
	}

	if sw.o.afterLoad != nil {
		sw.o.afterLoad(name)
	}

	return ns
}

// load reads the data model of the namespace's account. A namespace
// without registry has none; whether its objects are remnants is
// cleanable's to say. opened is taken first: nothing written after it is
// taken for a remnant.
func (ns *sweepNamespace) load() error {
	u, rep := ns.u, &ns.sw.st.Report
	ns.opened = openedLine(time.Now(), ns.sw.o.ClockMargin)

	if !u.objectExists(oidRegistry) {
		ok, reason, err := ns.cleanable()
		if err != nil {
			return err
		}

		if !ok {
			if reason == remnantRegistryMissing {
				rep.RegistryMissing++
			} else {
				rep.Unregistered++
			}

			rep.sample(reason + ": " + ns.name)

			ns.keep = true
		}

		return nil
	}

	if locked, err := u.ImportLocked(); err != nil || locked {
		rep.ImportLocked++

		ns.skip = true

		return err
	}

	named, err := u.enumerate(true)
	if err != nil {
		return err
	}

	ns.named, ns.index = named, named.indexSet()

	return nil
}

// Why a namespace without registry is left alone.
const (
	remnantRegistryMissing = "registry missing" // the directory holds it as active
	remnantUnregistered    = "no registry"      // neither purging nor recorded as purged
)

// openedLine is the line a namespace read at now sets: a margin before it
// for the writers' clocks, in whole seconds as object mtimes are (no
// margin: now itself, tests).
func openedLine(now time.Time, margin time.Duration) time.Time {
	if margin <= 0 {
		return now
	}

	return now.Add(-margin).Truncate(time.Second)
}

// cleanable reports whether the objects of a namespace without registry
// are remnants: the account directory says the account is being purged,
// or a purge recorded it and the directory holds no entry. An active
// entry always wins (a lost registry is no debris, whatever an earlier
// purge of the name recorded); a namespace neither purging nor recorded
// (an account that only kept Sieve scripts before 0.42.0) is left alone.
func (ns *sweepNamespace) cleanable() (bool, string, error) {
	state, ok, err := ns.sw.s.directoryState(ns.name)
	if err != nil {
		return false, "", err
	}

	purged, err := ns.sw.s.purged(ns.name)
	if err != nil {
		return false, "", err
	}

	switch {
	case ok && state == model.AccountPurging, !ok && purged:
		return true, "", nil
	case ok:
		return false, remnantRegistryMissing, nil
	default:
		return false, remnantUnregistered, nil
	}
}

// stillCleanable checks again, before a namespace without registry loses
// objects, that it still has no registry and may be cleaned: the account
// may have been created again while the sweep looked at it.
func (ns *sweepNamespace) stillCleanable() bool {
	if ns.named != nil || ns.keep || ns.dropped || !ns.sw.o.Repair {
		return true
	}

	rep := &ns.sw.st.Report

	if ns.u.objectExists(oidRegistry) {
		rep.Recreated++
		rep.sample("created again: " + ns.name)

		return false
	}

	ok, reason, err := ns.cleanable()

	switch {
	case err != nil:
		rep.fail(fmt.Errorf("namespace %q: %w", ns.name, err))
		return false
	case ok:
		return true
	case reason == remnantRegistryMissing:
		rep.RegistryMissing++
	default:
		rep.Unregistered++
	}

	rep.sample(reason + ": " + ns.name)

	return false
}

// directoryState returns the account's state in the account directory
// (ok false: no entry).
func (s *Store) directoryState(account string) (string, bool, error) {
	if !model.InDirectory(account) {
		return "", false, nil
	}

	ix, release, err := s.AccountIndex()
	if err != nil {
		return "", false, err
	}
	defer release()

	e, ok, err := model.GetAccountEntry(ix, account)
	if err != nil || !ok {
		return "", false, err
	}

	return e.State, true, nil
}

// check looks at one object of the namespace.
func (ns *sweepNamespace) check(oid string) {
	if ns.skip || ns.dropped {
		return
	}

	if ns.named != nil && ns.named.names(ns.index, ns.data, oid) {
		return
	}

	// A full-text shard above the count this process is configured with
	// may be a live one of servers configured with more: reported, never
	// deleted (see shardpost-scrub(1)).
	if !ns.data && ns.named != nil && ftsShardObject(oid) {
		ns.sw.st.Report.Remnants[remnantFTS]++
		ns.sw.st.Report.sample(ns.name + ": " + oid + " (kept)")

		return
	}

	if ns.data {
		if guid, chunk, ok := splitChunkOID(oid); ok {
			ns.blobs[guid] = max(ns.blobs[guid], chunk+1)
			ns.lastGUID = guid

			return
		}
	} else if ns.named != nil && ns.adopt(oid) {
		return
	}

	ns.objects = append(ns.objects, oid)
}

// adopt writes the ps: key of the marks of a live public folder that
// predate the intents; it reports whether oid was such marks.
func (ns *sweepNamespace) adopt(oid string) bool {
	id, ok := strings.CutPrefix(oid, oidPrivateSeen)
	if !ok {
		return false
	}

	if ns.live == nil {
		sr := &scrubRun{u: ns.u, log: ns.sw.log}

		live, err := sr.publicMailboxIDs()
		if err != nil {
			ns.sw.st.Report.fail(err)

			ns.live = map[string]bool{}
		} else {
			ns.live = live
		}
	}

	if !ns.live[id] {
		return false
	}

	if ns.sw.o.Repair {
		if err := ns.u.setIntent(map[string][]byte{pubSeenKey(id): encode(mailboxIntent{Time: time.Now().Unix()})}); err != nil {
			ns.sw.st.Report.fail(err)
			return true
		}
	}

	ns.sw.st.Report.Adopted++

	return true
}

// finish handles the remnants collected so far.
func (ns *sweepNamespace) finish() { ns.flush(false) }

// flush handles the remnants collected so far; with holdLast the blob
// seen last waits (its chunks may go on on the next page, and a blob is
// handled once).
func (ns *sweepNamespace) flush(holdLast bool) {
	if ns == nil || ns.skip {
		return
	}

	if !ns.stillCleanable() {
		// The account came back (or may no longer be cleaned) while the
		// sweep looked at it: what was collected is no remnant, and the
		// rest of the namespace is not looked at.
		ns.unclean()

		ns.blobs, ns.objects, ns.dropped = map[string]int{}, nil, true

		return
	}

	held, last := 0, ""
	if holdLast {
		last = ns.lastGUID
		held = ns.blobs[last]
	}

	for guid, chunks := range ns.blobs {
		if guid != last || !holdLast {
			ns.blobRemnant(guid, chunks)
		}
	}

	for _, oid := range ns.objects {
		ns.objectRemnant(oid)
	}

	ns.blobs, ns.objects = map[string]int{}, nil

	if holdLast && held > 0 {
		ns.blobs[last] = held
	}
}

func (ns *sweepNamespace) close() {
	if ns != nil && ns.u != nil {
		ns.u.Close()
		ns.u = nil
	}
}

// unclean records that the namespace was not cleaned completely: a
// record of its purge must stay for the next run.
func (ns *sweepNamespace) unclean() {
	st := ns.sw.st
	if st.Unclean == nil {
		st.Unclean = map[string]bool{}
	}

	st.Unclean[ns.name] = true
}

// class names the remnant class of an object.
func (ns *sweepNamespace) class(oid string) string {
	switch {
	case ns.named == nil:
		return remnantNamespace
	case ns.data:
		return remnantBlob
	case strings.HasPrefix(oid, mboxPrefix):
		return remnantMailbox
	case strings.HasPrefix(oid, oidPrivateSeen):
		return remnantMarks
	default:
		return remnantOther
	}
}

// cutoff is the time before which a remnant may go.
//
// It is the earlier of now less the grace period and when the namespace
// was opened: an object written after the data model was read is no
// remnant, however long the namespace takes.
func (ns *sweepNamespace) cutoff() time.Time {
	c := time.Now().Add(-ns.sw.o.Grace)
	if !ns.opened.IsZero() && ns.opened.Before(c) {
		return ns.opened
	}

	return c
}

// blobRemnant handles a blob nothing names.
func (ns *sweepNamespace) blobRemnant(guid string, chunks int) {
	rep, u := &ns.sw.st.Report, ns.u

	rep.Remnants[ns.class(chunkOID(guid, 0))]++
	rep.sample(ns.name + ": " + chunkOID(guid, 0))

	st, err := u.data.Stat(chunkOID(guid, 0))

	switch {
	case err != nil && errCode(err) != syscall.ENOENT:
		rep.fail(fmt.Errorf("%s: stat %s: %w", ns.name, guid, err))
		ns.unclean()

		return
	case err == nil && st.ModTime.After(ns.cutoff()):
		rep.Young++

		ns.unclean()

		return
	case !ns.sw.o.Repair || ns.keep:
		return
	}

	size := int64(0)
	if err == nil {
		size, _ = u.BlobSize(guid)
		chunks = max(chunks, u.chunkCount(size))
	}

	// The postings of an account's blob go with it; a namespace without
	// registry has no index worth keeping (its objects go as well).
	gone, cerr := u.collectBlob(guid, size, chunks, size > 0 && ns.named != nil, ns.cutoff())
	if cerr != nil {
		rep.fail(cerr)
		ns.unclean()
	}

	if gone {
		rep.Removed++
	} else {
		ns.unclean()
	}
}

// objectRemnant handles an index object (or another data object) nothing
// names.
func (ns *sweepNamespace) objectRemnant(oid string) {
	rep, u := &ns.sw.st.Report, ns.u

	ioctx := u.idx
	if ns.data {
		ioctx = u.data
	}

	rep.Remnants[ns.class(oid)]++
	rep.sample(ns.name + ": " + oid)

	st, ver, err := u.statVer(ioctx, oid)

	switch {
	case errCode(err) == syscall.ENOENT:
		return
	case err != nil:
		rep.fail(fmt.Errorf("%s: stat %s: %w", ns.name, oid, err))
		ns.unclean()

		return
	case st.ModTime.After(ns.cutoff()):
		rep.Young++

		ns.unclean()

		return
	case !ns.sw.o.Repair || ns.keep:
		return
	}

	// Deleted only at the version that was judged: a write in between
	// leaves the object for the next run.
	switch err := removeAt(ioctx, oid, ver); {
	case err == nil:
		rep.Removed++
	case errCode(err) == syscall.ERANGE || errCode(err) == syscall.EOVERFLOW:
		ns.unclean()
	case errCode(err) != syscall.ENOENT:
		rep.fail(fmt.Errorf("%s: delete %s: %w", ns.name, oid, err))
		ns.unclean()
	}
}

// removeAt deletes an object if it is still at version ver.
func removeAt(ioctx *rados.IOContext, oid string, ver uint64) error {
	op := rados.CreateWriteOp()
	defer op.Release()

	op.AssertVersion(ver)
	op.Remove()

	return op.Operate(ioctx, oid, rados.OperationNoFlag)
}
