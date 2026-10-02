package cephstore

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/ceph/go-ceph/rados"

	"shardpost/internal/model"
)

// The system tools ("shardpost system ...", temp/12 §5)
//
// Besides the init (system.go) the tools move the store's own namespaces
// between the index pool and a system pool and check what they left:
//
//	status   reads fence, mark, run state and registrations
//	migrate  copies the store's own namespaces of the index pool into a
//	         system pool (phases M0 to M7, migrate.go)
//	verify   compares source and target, or the old place with what may
//	         stay there (verify.go)
//	abort    takes a migration back before it was committed (abort.go)
//	revert   copies the system pool back into the index pool (revert.go)
//	cleanup  deletes the quarantine of a migration (cleanup.go)
//
// They open the store as a tool (Config.PlacementTool) and reach the
// pools directly. They never register in the admin store: a heartbeat
// there would change the very records a migration copies. A tool that
// writes holds the tool lease in the fence (fencelease.go) for as long
// as it works, and stops writing at once when the lease is no longer
// valid (sysTool.guard): every write it makes checks the lease first.
//
// State of a migration, in the system pool, namespace "~admin" (outside
// "kv." and "ix.", never copied or compared):
//
//	"system.migrate"             the run state: run, phase, abort_from,
//	                             digest_shards, m4_digest, the original
//	                             values of every poisoned head
//	                             ("head:<ns>/<oid>"), the report
//	"system.migrate/<ns>/<nnn>"  the digest of every object copied, by
//	                             name (nnn = model.IndexShard(name, n),
//	                             n = digest_shards)
//
// and in the index pool the quarantine "<ns>.moved/<run>" (for example
// "~admin.moved/<run>"), where the migration moves the old objects
// before it commits; no server reads it, the listing scrub and the sweep
// skip it.

// SiteSystemMigrate labels the listings of the system tools in
// imapceph_rados_listings_total.
const SiteSystemMigrate = "system_migrate"

// ErrSystemRefused is the error of a system tool whose preconditions do
// not hold (exit code 2): nothing was changed that the next run could not
// take up.
var ErrSystemRefused = errors.New("system tool refused")

// ErrSystemMismatch is the error of a system tool that found the pools
// different from what they must be (exit code 3): a digest that does not
// match, an object that should not be there.
var ErrSystemMismatch = errors.New("system verification failed")

// systemStep is called at the named steps of the system tools; a test
// makes it fail to stop a tool there (an error wrapping ErrLeaseLost
// leaves the lease as a crash would).
var systemStep = func(string) error { return nil }

// Run state keys.
const (
	rsRun          = "run"
	rsPhase        = "phase"
	rsAbortFrom    = "abort_from"
	rsDigestShards = "digest_shards"
	rsM4Digest     = "m4_digest"
	rsFenced       = "fenced_digest"
	rsHeads        = "heads"
	rsHeadPrefix   = "head:"
	rsReport       = "report"
	rsToID         = "to_id"
	rsObjects      = "objects"
	rsBytes        = "bytes"
	rsAt           = "at"
	rsBy           = "by"
	rsAbortedAt    = "aborted_at"
	rsRevertedAt   = "reverted_at"
)

// Phases of the run state. M1 to M7 are those of the migration (temp/12
// §5.3; M2 is never recorded, it lists anew on every run); abort and
// revert mark a run that is being taken back.
const (
	PhaseM1     = "M1"
	PhaseM3     = "M3"
	PhaseM4     = "M4"
	PhaseM5     = "M5"
	PhaseM6     = "M6"
	PhaseM7     = "M7"
	PhaseAbort  = "abort"
	PhaseRevert = "revert"
)

// Fence keys of a migration.
const (
	fkDirection    = "direction"
	fkFenced       = "fenced_digest"
	fkHeads        = "heads"
	fkCleanedAt    = "cleaned_at"
	fkCleaning     = "cleaning_at"
	directionMig   = "migrate"
	directionBack  = "revert"
	poisonFormat   = 1000
	poisonPhasePfx = "moved:"
	// headKeyPhase is the reshard phase key of an index head
	// (sortedindex), which a poisoned head sets to poisonPhasePfx and the
	// system pool's id.
	headKeyPhase = "phase"
)

// SystemOptions configure the system tools; each one uses those that
// apply to it.
type SystemOptions struct {
	// Context stops the work when it is done (a signal): the tool stops
	// writing, gives its lease up and can be run again to continue.
	Context context.Context
	// By names who runs the tool (recorded in the fence).
	By string
	// Log receives the progress (the store's log when nil).
	Log *slog.Logger
	// Wait takes over the lease of another tool once it has been idle for
	// three minutes (only when that tool's job is known to be gone).
	Wait bool
	// Parallel is the number of objects copied or hashed at a time.
	Parallel int
	// Run is the run the command is about (abort, revert, cleanup).
	Run string
	// Confirm confirms a destructive step by naming its run: the
	// deletion of an earlier run's copies, surplus objects, the
	// quarantine.
	Confirm string
	// Check runs only the preconditions of a migration (M0), reporting
	// the registrations, leases and watchers instead of refusing.
	Check bool
	// ForceRegistrations lets a migration ignore running registrations
	// (only for processes known to be dead; the report names them).
	ForceRegistrations bool
	// DeleteCopies deletes the copies in the system pool on an abort.
	DeleteCopies bool
	// DeleteSurplus deletes objects at the old place that must not be
	// there (verify, abort, revert, cleanup), with Confirm.
	DeleteSurplus bool
	// SourceOnly makes verify check the old place against what may stay
	// there after the migration.
	SourceOnly bool
	// MinAge is how old a migration must be before cleanup deletes its
	// quarantine.
	MinAge time.Duration
	// Deep makes status list the namespaces of both pools.
	Deep bool
}

// DefaultSystemParallel is the default of SystemOptions.Parallel.
const DefaultSystemParallel = 16

// MaxSystemParallel bounds SystemOptions.Parallel.
const MaxSystemParallel = 64

// DefaultCleanupAge is the default of SystemOptions.MinAge.
const DefaultCleanupAge = 7 * 24 * time.Hour

// Finding is one result of a check: what was checked, the object and
// what was found; Blocking findings refuse the command.
type Finding struct {
	Check    string `json:"check"`
	Object   string `json:"object,omitempty"`
	Detail   string `json:"detail"`
	Blocking bool   `json:"blocking"`
}

// SystemReport is what a system tool did and found; it is printed as
// JSON whatever the outcome.
type SystemReport struct {
	Command      string            `json:"command"`
	Run          string            `json:"run,omitempty"`
	Phase        string            `json:"phase,omitempty"`
	IndexPool    *model.PoolRef    `json:"index_pool,omitempty"`
	SystemPool   *model.PoolRef    `json:"system_pool,omitempty"`
	Fence        map[string]string `json:"fence,omitempty"`
	Mark         map[string]string `json:"mark,omitempty"`
	RunState     map[string]string `json:"run_state,omitempty"`
	Objects      int               `json:"objects,omitempty"`
	Bytes        int64             `json:"bytes,omitempty"`
	Copied       int               `json:"copied,omitempty"`
	Kept         int               `json:"kept,omitempty"`
	Replaced     int               `json:"replaced,omitempty"`
	Quarantined  int               `json:"quarantined,omitempty"`
	Poisoned     int               `json:"poisoned,omitempty"`
	Restored     int               `json:"restored,omitempty"`
	Deleted      int               `json:"deleted,omitempty"`
	Digests      map[string]string `json:"digests,omitempty"`
	FencedDigest string            `json:"fenced_digest,omitempty"`
	Heads        []string          `json:"heads,omitempty"`
	Findings     []Finding         `json:"findings,omitempty"`
	Surplus      []string          `json:"surplus,omitempty"`
	Mismatches   []string          `json:"mismatches,omitempty"`
	Registrations []Registration   `json:"registrations,omitempty"`
	Namespaces   []NamespaceCount  `json:"namespaces,omitempty"`
	Done         bool              `json:"done,omitempty"`
	Already      bool              `json:"already,omitempty"`
	Started      time.Time         `json:"started"`
	Finished     time.Time         `json:"finished,omitzero"`
}

// Registration is a server registration as status and the checks show
// it.
type Registration struct {
	Instance string `json:"instance"`
	model.ServerRecord
	Running bool `json:"running"`
}

// NamespaceCount is the number of objects of one namespace of a pool
// (status --deep).
type NamespaceCount struct {
	Pool      string `json:"pool"`
	Namespace string `json:"namespace"`
	Objects   int    `json:"objects"`
}

// maxListed bounds the object names a report lists per kind.
const maxListed = 200

// addListed appends name to list up to maxListed names.
func addListed(list []string, name string) []string {
	if len(list) < maxListed {
		return append(list, name)
	}

	if len(list) == maxListed {
		return append(list, "…")
	}

	return list
}

// sysTool is one run of a system tool.
type sysTool struct {
	s    *Store
	conn *rados.Conn
	cfg  Config
	o    SystemOptions
	log  *slog.Logger
	rep  *SystemReport

	indexID int64
	dataID  int64
	fsid    string
	// sysPool and sysID are the system pool the tool works on ("" and
	// -1 while it is not known).
	sysPool string
	sysID   int64

	lease *fenceLease
	// ctx is cancelled when the lease is no longer valid: the workers
	// stop at once.
	ctx    context.Context
	cancel context.CancelFunc
	stopW  chan struct{}
	doneW  chan struct{}
}

// runTool runs fn as the system tool command on the store's connection.
func (s *Store) runTool(command string, o SystemOptions, fn func(*sysTool) error) (*SystemReport, error) {
	s.mu.RLock()
	cfg := s.cfg
	s.mu.RUnlock()

	o.Parallel = min(max(o.Parallel, 1), MaxSystemParallel)
	if o.Log == nil {
		o.Log = s.log
	}

	if o.By == "" {
		o.By = "shardpost system " + command
	}

	rep := &SystemReport{Command: command, Started: time.Now().UTC()}

	if err := checkToolClientOptions(cfg.ClientOptions, o.Parallel); err != nil {
		return rep, fmt.Errorf("%w: ceph.client_options: %w", ErrSystemRefused, err)
	}

	t := &sysTool{s: s, cfg: cfg, o: o, rep: rep, fsid: s.fsid, sysID: -1,
		log: o.Log.With("component", "system_"+command)}

	err := s.WithConn(func(conn *rados.Conn) error {
		t.conn = conn

		if err := t.resolvePools(); err != nil {
			return err
		}

		defer t.stopLease()

		return fn(t)
	})

	rep.Finished = time.Now().UTC()

	return rep, err
}

// resolvePools looks the index and data pool up.
func (t *sysTool) resolvePools() error {
	var err error

	if t.indexID, err = t.conn.GetPoolByName(t.cfg.IndexPool); err != nil {
		return fmt.Errorf("index pool %q: %w", t.cfg.IndexPool, err)
	}

	if t.dataID, err = t.conn.GetPoolByName(t.cfg.DataPool); err != nil {
		return fmt.Errorf("data pool %q: %w", t.cfg.DataPool, err)
	}

	t.rep.IndexPool = &model.PoolRef{ID: t.indexID, Name: t.cfg.IndexPool}

	return nil
}

// setSystemPool sets the system pool by name, refusing the index or the
// data pool.
func (t *sysTool) setSystemPool(name string) error {
	id, err := t.conn.GetPoolByName(name)
	if err != nil {
		return fmt.Errorf("%w: system pool %q: %w", ErrSystemRefused, name, err)
	}

	if id == t.indexID || id == t.dataID {
		return fmt.Errorf("%w: %q is the index or the data pool", ErrSystemRefused, name)
	}

	t.sysPool, t.sysID = name, id
	t.rep.SystemPool = &model.PoolRef{ID: id, Name: name}

	return nil
}

// systemPoolOf sets the system pool the fence points to (by id: the pool
// may have been renamed).
func (t *sysTool) systemPoolOf(f Fence) error {
	name, err := t.conn.GetPoolByID(f.ToID)
	if err != nil {
		return fmt.Errorf("%w: the pool %q (id %d) the fence points to: %w", ErrSystemRefused, f.ToName, f.ToID, err)
	}

	return t.setSystemPool(name)
}

// readFence reads the fence.
func (t *sysTool) readFence() (Fence, error) {
	f, err := (&radosFence{conn: t.conn, pool: t.cfg.IndexPool}).read()
	if err == nil {
		t.rep.Fence = withoutLease(f.Values)
	}

	return f, err
}

// withoutLease is a copy of fence values without the lease fields (for
// the report).
func withoutLease(vals map[string]string) map[string]string {
	if vals == nil {
		return nil
	}

	out := maps.Clone(vals)
	delete(out, keyToolSeen)
	delete(out, keyToolInstance)

	return out
}

// readMark reads the mark of the system pool.
func (t *sysTool) readMark() (Mark, error) {
	vals, ver, err := readObjectOmap(t.conn, t.sysPool, model.AdminNamespace, MarkOID, markKeys)
	if err != nil {
		return Mark{}, fmt.Errorf("read the mark: %w", err)
	}

	m := markOf(vals, ver)
	t.rep.Mark = m.Values

	return m, nil
}

// takeLease takes the tool lease with plan (fencelease.go) and starts
// the watch that cancels the work once the lease is no longer valid.
func (t *sysTool) takeLease(plan func(Fence) (map[string]string, error)) (Fence, error) {
	t.lease = newFenceLease(t.conn, t.cfg.IndexPool, t.log, t.o.Wait)

	f, err := t.lease.acquire(plan)
	if errors.Is(err, ErrFenceHeld) {
		return f, fmt.Errorf("%w: %w", ErrSystemRefused, err)
	}

	if err != nil {
		return f, err
	}

	t.rep.Fence = withoutLease(f.Values)
	t.watchLease()

	return f, systemStep("lease")
}

// leaseWatchEvery is how often the lease watch looks at the lease.
var leaseWatchEvery = time.Second

// watchLease cancels t.ctx as soon as the lease is no longer valid.
func (t *sysTool) watchLease() {
	parent := t.o.Context
	if parent == nil {
		parent = context.Background()
	}

	t.ctx, t.cancel = context.WithCancel(parent)
	t.stopW, t.doneW = make(chan struct{}), make(chan struct{})

	go func() {
		defer close(t.doneW)

		tick := time.NewTicker(min(leaseWatchEvery, toolLeaseValid/10))
		defer tick.Stop()

		for {
			select {
			case <-t.stopW:
				return
			case <-tick.C:
			}

			if !t.lease.valid() {
				t.log.Error("the fence lease is no longer valid, stopping every write", "event", "system_lease_lost")
				t.cancel()

				return
			}
		}
	}()
}

// stopWatch ends the lease watch (before the lease is given up).
func (t *sysTool) stopWatch() {
	if t.stopW == nil {
		return
	}

	close(t.stopW)
	<-t.doneW
	t.stopW = nil
}

// stopLease ends the renewal and the watch (the lease stays as it is).
func (t *sysTool) stopLease() {
	t.stopWatch()

	if t.lease != nil {
		t.lease.close()
	}

	if t.cancel != nil {
		t.cancel()
	}
}

// guard is checked before every write of a tool: the lease must be valid
// and the work not cancelled.
func (t *sysTool) guard() error {
	if t.lease == nil {
		return fmt.Errorf("%w: no lease", ErrLeaseLost)
	}

	if err := t.lease.check(); err != nil {
		return err
	}

	if t.ctx != nil && t.ctx.Err() != nil {
		return fmt.Errorf("interrupted (%w); run it again to continue", context.Cause(t.ctx))
	}

	return nil
}

// finish gives the lease up after err: on every error but a lost lease
// (which belongs to another tool now, or to nobody after a crash), and
// on success when the command did not give it up with its last write.
func (t *sysTool) finish(err error) error {
	t.stopWatch()

	if t.lease == nil || errors.Is(err, ErrLeaseLost) {
		return err
	}

	if rerr := t.lease.release(); rerr != nil {
		t.log.Warn("fence lease not given up", "err", rerr)

		if err == nil {
			err = rerr
		}
	}

	return err
}

// updateFence changes the fence under the lease (release: and gives the
// lease up with the same write).
func (t *sysTool) updateFence(mutate func(map[string]string) error, release bool) error {
	if release {
		t.stopWatch()
	}

	if err := t.lease.update(mutate, release); err != nil {
		return err
	}

	f, err := t.readFence()
	if err != nil {
		return err
	}

	t.rep.Fence = withoutLease(f.Values)

	return nil
}

// openCtx opens a context on pool, namespace ns.
func (t *sysTool) openCtx(pool, ns string) (*rados.IOContext, error) {
	c, err := t.conn.OpenIOContext(pool)
	if err != nil {
		return nil, fmt.Errorf("open pool %q: %w", pool, err)
	}

	c.SetNamespace(ns)

	return c, nil
}

// --- the objects of the store's own namespaces -------------------------------

// quarantineNS is the quarantine of namespace ns for run in the index
// pool.
func quarantineNS(ns, run string) string { return ns + ".moved/" + run }

// isHead reports whether oid is the head of a sorted index or queue.
func isHead(oid string) bool { return strings.HasPrefix(oid, "ix.") && strings.HasSuffix(oid, "/head") }

// isSweep reports whether oid of ns belongs to the sweep, which stays in
// the index pool.
func isSweep(ns, oid string) bool {
	return ns == model.AccountsNamespace && strings.HasPrefix(oid, "sweep/")
}

// movable reports whether oid of ns is part of what a migration moves:
// the store's own namespaces without the placement objects and the
// sweep.
func movable(ns, oid string) bool {
	return model.SystemNamespace(ns) && !placementObject(ns, oid) && !isSweep(ns, oid)
}

// objName is the name of an object in reports and digests: namespace
// and object.
func objName(ns, oid string) string { return ns + "/" + oid }

// splitName is the inverse of objName (the namespaces have no "/").
func splitName(name string) (string, string) {
	ns, oid, _ := strings.Cut(name, "/")
	return ns, oid
}

// listing is the objects of a pool by namespace.
type listing map[string][]string

// set is the listing as a set of namespace/object names.
func (l listing) set() map[string]bool {
	out := map[string]bool{}

	for ns, oids := range l {
		for _, oid := range oids {
			out[objName(ns, oid)] = true
		}
	}

	return out
}

// listPool lists every namespace of pool and keeps what keep accepts.
func (t *sysTool) listPool(pool string, keep func(ns, oid string) bool) (listing, error) {
	c, err := t.openCtx(pool, rados.AllNamespaces)
	if err != nil {
		return nil, err
	}
	defer c.Destroy()

	it, err := listObjects(c, SiteSystemMigrate)
	if err != nil {
		return nil, fmt.Errorf("list %s: %w", pool, err)
	}
	defer it.Close()

	out := listing{}

	for it.Next() {
		if keep(it.Namespace(), it.Value()) {
			out[it.Namespace()] = append(out[it.Namespace()], it.Value())
		}
	}

	if err := it.Err(); err != nil {
		return nil, fmt.Errorf("list %s: %w", pool, err)
	}

	for ns := range out {
		slices.Sort(out[ns])
	}

	return out, nil
}

// movableIn lists what a migration moves in pool (the four namespaces
// without placement objects and sweep).
func (t *sysTool) movableIn(pool string) (listing, error) {
	return t.listPool(pool, movable)
}

// deleteObject deletes oid of ns in pool under the lease (an absent
// object is no error).
func (t *sysTool) deleteObject(c *rados.IOContext, oid string) error {
	if err := t.guard(); err != nil {
		return err
	}

	if err := c.Delete(oid); err != nil && errCode(err) != syscall.ENOENT {
		return fmt.Errorf("delete %s: %w", oid, err)
	}

	return nil
}

// deleteNamespaces deletes every object of the namespaces of pool that
// match keep, under the lease; it returns the count.
func (t *sysTool) deleteListed(pool string, l listing) (int, error) {
	n := 0

	for ns, oids := range l {
		c, err := t.openCtx(pool, ns)
		if err != nil {
			return n, err
		}

		for _, oid := range oids {
			if err := t.deleteObject(c, oid); err != nil {
				c.Destroy()
				return n, err
			}

			n++
		}

		c.Destroy()
	}

	return n, nil
}

// deleteQuarantine deletes the quarantine of run in the index pool.
func (t *sysTool) deleteQuarantine(run string) (int, error) {
	n, err := deleteQuarantineOf(t.conn, t.cfg.IndexPool, run, t.guard)
	if n > 0 {
		t.log.Info("quarantine deleted", rsRun, run, rsObjects, n)
	}

	return n, err
}

// isQuarantine reports whether ns is the quarantine of run.
func isQuarantine(ns, run string) bool {
	return slices.ContainsFunc(model.SystemNamespaces(), func(sys string) bool { return ns == quarantineNS(sys, run) })
}

// deleteQuarantineOf deletes the quarantine of run in pool; guard is
// checked before every deletion.
func deleteQuarantineOf(conn *rados.Conn, pool, run string, guard func() error) (int, error) {
	objs, err := listAllOf(conn, pool, func(ns, _ string) bool { return isQuarantine(ns, run) })
	if err != nil {
		return 0, err
	}

	return deleteAllOf(conn, pool, objs, guard)
}

// listAllOf lists every namespace of pool: the (namespace, object) pairs
// keep accepts.
func listAllOf(conn *rados.Conn, pool string, keep func(ns, oid string) bool) ([][2]string, error) {
	c, err := conn.OpenIOContext(pool)
	if err != nil {
		return nil, fmt.Errorf("open pool %q: %w", pool, err)
	}
	defer c.Destroy()

	c.SetNamespace(rados.AllNamespaces)

	it, err := listObjects(c, SiteSystemMigrate)
	if err != nil {
		return nil, fmt.Errorf("list %s: %w", pool, err)
	}
	defer it.Close()

	var objs [][2]string

	for it.Next() {
		if keep(it.Namespace(), it.Value()) {
			objs = append(objs, [2]string{it.Namespace(), it.Value()})
		}
	}

	if err := it.Err(); err != nil {
		return nil, fmt.Errorf("list %s: %w", pool, err)
	}

	return objs, nil
}

// deleteAllOf deletes the (namespace, object) pairs of pool in order,
// after guard each.
func deleteAllOf(conn *rados.Conn, pool string, objs [][2]string, guard func() error) (int, error) {
	c, err := conn.OpenIOContext(pool)
	if err != nil {
		return 0, fmt.Errorf("open pool %q: %w", pool, err)
	}
	defer c.Destroy()

	n := 0

	for _, o := range objs {
		if err := guard(); err != nil {
			return n, err
		}

		c.SetNamespace(o[0])

		if err := c.Delete(o[1]); err != nil && errCode(err) != syscall.ENOENT {
			return n, fmt.Errorf("delete %s: %w", objName(o[0], o[1]), err)
		}

		n++
	}

	return n, nil
}

// clearSystemNamespaces deletes everything in the store's namespaces of
// the system pool (the copies of an earlier run, with their run state
// and mark) and, for run, its quarantine in the index pool.
func (t *sysTool) clearSystemNamespaces(run string) (int, error) {
	n, err := clearRun(t.conn, t.cfg.IndexPool, t.sysPool, run, t.guard)
	if n > 0 {
		t.log.Info("earlier content deleted", rsRun, run, rsObjects, n)
	}

	return n, err
}

// clearRun deletes an earlier run: its quarantine in the index pool
// first (none for run ""), then every object of the store's namespaces
// in the system pool, the mark and the run state last. A deletion that
// stopped is taken up when the run is confirmed again: its run state
// names it until the end. guard is checked before every deletion.
func clearRun(conn *rados.Conn, indexPool, sysPool, run string, guard func() error) (int, error) {
	n := 0

	if run != "" {
		q, err := deleteQuarantineOf(conn, indexPool, run, guard)
		if n += q; err != nil {
			return n, err
		}
	}

	objs, err := listAllOf(conn, sysPool, func(ns, _ string) bool { return model.SystemNamespace(ns) })
	if err != nil {
		return n, err
	}

	last := func(o [2]string) int {
		if o[0] == model.AdminNamespace && (o[1] == MarkOID || o[1] == runStateOID) {
			return 1
		}

		return 0
	}

	slices.SortStableFunc(objs, func(a, b [2]string) int { return last(a) - last(b) })

	d, err := deleteAllOf(conn, sysPool, objs, guard)

	return n + d, err
}

// --- the run state -------------------------------------------------------------

// runState is the run state as read (Exists false: none).
type runState struct {
	Exists bool
	Values map[string]string
}

// readRunState reads the run state of the system pool.
func (t *sysTool) readRunState() (runState, error) {
	c, err := t.openCtx(t.sysPool, model.AdminNamespace)
	if err != nil {
		return runState{}, err
	}
	defer c.Destroy()

	vals, err := c.GetAllOmapValues(runStateOID, "", "", omapPageSize)
	if errCode(err) == syscall.ENOENT {
		return runState{}, nil
	}

	if err != nil {
		return runState{}, fmt.Errorf("read the run state: %w", err)
	}

	rs := runState{Exists: true, Values: strValues(vals)}
	t.rep.RunState = rs.summary()

	return rs, nil
}

// summary is the run state without the original heads and the report.
func (rs runState) summary() map[string]string {
	out := map[string]string{}

	for k, v := range rs.Values {
		if !strings.HasPrefix(k, rsHeadPrefix) && k != rsReport {
			out[k] = v
		}
	}

	return out
}

// writeRunState sets values of the run state under the lease, in one
// write (create: the object may be created).
func (t *sysTool) writeRunState(vals map[string]string, create bool) error {
	if err := t.guard(); err != nil {
		return err
	}

	c, err := t.openCtx(t.sysPool, model.AdminNamespace)
	if err != nil {
		return err
	}
	defer c.Destroy()

	op := rados.CreateWriteOp()
	defer op.Release()

	if create {
		op.Create(rados.CreateIdempotent)
	} else {
		op.AssertExists()
	}

	op.SetOmap(toBytes(vals))

	if err := op.Operate(c, runStateOID, rados.OperationNoFlag); err != nil {
		return fmt.Errorf("write the run state: %w", err)
	}

	t.rep.RunState = runState{Values: mergedSummary(t.rep.RunState, vals)}.Values

	return nil
}

// mergedSummary merges vals into a report's run state summary.
func mergedSummary(sum, vals map[string]string) map[string]string {
	out := maps.Clone(sum)
	if out == nil {
		out = map[string]string{}
	}

	for k, v := range vals {
		if !strings.HasPrefix(k, rsHeadPrefix) && k != rsReport {
			out[k] = v
		}
	}

	return out
}

// phaseAtLeast reports whether phase p has reached want (M1 < M3 < M4 <
// M5 < M6 < M7; abort and revert reach nothing).
func phaseAtLeast(p, want string) bool {
	order := []string{PhaseM1, PhaseM3, PhaseM4, PhaseM5, PhaseM6, PhaseM7}

	i, j := slices.Index(order, p), slices.Index(order, want)

	return i >= 0 && j >= 0 && i >= j
}

// nowText is the time of a record.
func nowText() string { return time.Now().UTC().Format(time.RFC3339) }
