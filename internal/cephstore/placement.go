package cephstore

import (
	"errors"
	"fmt"
	"strconv"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/ceph/go-ceph/rados"

	"shardpost/internal/metrics"
	"shardpost/internal/model"
)

// Placement of the store's own namespaces (ceph.system_pool, stage B11)
//
// With Config.SystemPool the index part of the store's own namespaces
// (model.SystemNamespace) lives in a pool of its own. One cluster has one
// placement, and data, not discipline, keeps every process to it:
//
//	index pool,  namespace "~admin":
//	  "system.moved"   the fence: where the system state lives
//	                   (state, to_id, to_name, index_id, fsid, run, ...)
//	system pool, namespace "~admin":
//	  "system.pool"    the mark: this pool holds the system state of that
//	                   index pool (format, state, origin, index_id,
//	                   data_id, system_id, fsid, run, ...)
//
// Both are omap objects outside "kv." and "ix.", so no older server
// takes them for records or indexes. Pools are compared by id and
// cluster fsid; the names are only for messages (a pool can be renamed).
//
// The fence is the only arbiter: "shardpost system init" (and, in a
// later release, "shardpost system migrate") creates it exclusively
// before anything else ("initializing" or "migrating"), writes the mark
// (the commit) and only then the final state ("native" or "done"). A
// tool that could not create the fence writes no mark.
//
// Open reads the fence and, with a system pool, the mark, and decides
// with decidePlacement; it stats a few objects of the index pool only to
// tell a new cluster from an existing one in a refusal, and never lists.
// A long-running process repeats the reads every minute
// (OnPlacementLost): only a placement read positively that contradicts
// the configuration ends it; a read error is logged and shown as
// imapceph_system_placement{state="unknown"}, so that a Ceph outage does
// not turn into a crash loop.

// The placement objects, in model.AdminNamespace.
const (
	// FenceOID is the fence in the index pool.
	FenceOID = "system.moved"
	// MarkOID is the mark in the system pool.
	MarkOID = "system.pool"
	// probeOID is the omap probe of the init in the system pool.
	probeOID = "system.probe"
)

// Fence states.
const (
	FenceInitializing = "initializing"
	FenceMigrating    = "migrating"
	FenceDone         = "done"
	FenceNative       = "native"
	FenceReverted     = "reverted"
	// FenceLeased is the neutral state of a fence a tool holds the lease
	// of before its first real step (fencelease.go): no fence for the
	// servers.
	FenceLeased = "leased"
)

// Mark states, origins and format.
const (
	MarkActive     = "active"
	MarkRetired    = "retired"
	OriginNative   = "native"
	OriginMigrated = "migrated"
	MarkFormat     = 1
)

// Placement states (label "state" of imapceph_system_placement).
const (
	PlacementNone     = "none"     // the store's own namespaces live in the index pool
	PlacementNative   = "native"   // in the system pool, initialized there
	PlacementMigrated = "migrated" // in the system pool, migrated there
	PlacementUnknown  = "unknown"  // the last check could not read
)

// ErrSystemPlacement is the error of every refusal of the placement
// check: the configuration does not match where the cluster keeps the
// store's own namespaces.
var ErrSystemPlacement = errors.New("system pool placement")

// ErrSystemMoved refuses a store without ceph.system_pool on a cluster
// whose system state lives in a system pool.
var ErrSystemMoved = fmt.Errorf("%w: the system state has moved out of the index pool", ErrSystemPlacement)

// Fence is the fence as read (Exists false: there is none).
type Fence struct {
	Exists  bool
	Version uint64
	State   string
	// ToID and ToName are the system pool it points to.
	ToID   int64
	ToName string
	// IndexID and FSID are the index pool and cluster it was written in.
	IndexID int64
	FSID    string
	Run     string
	At      string
	By      string
	// Values are all omap values of the object (the fields of later
	// releases among them).
	Values map[string]string
}

// Omap keys of the fence and the mark.
const (
	pkState     = "state"
	pkToID      = "to_id"
	pkToName    = "to_name"
	pkIndexID   = "index_id"
	pkIndexName = "index_name"
	pkDataID    = "data_id"
	pkDataName  = "data_name"
	pkSystemID  = "system_id"
	pkFSID      = "fsid"
	pkRun       = "run"
	pkAt        = "at"
	pkBy        = "by"
	pkFormat    = "format"
	pkOrigin    = "origin"
	pkObjects   = "objects"
	pkDigest    = "digest"
	pkPools     = "pools"
)

// fenceKeys are the omap keys of the fence (with those a migration
// writes).
var fenceKeys = []string{pkState, pkToID, pkToName, pkIndexID, pkFSID, pkRun, pkAt, pkBy,
	"direction", "fenced_digest", "heads", "cleaned_at", keyToolInstance, keyToolEpoch, keyToolSeen, keyPrevState}

// Mark is the mark as read (Exists false: there is none).
type Mark struct {
	Exists    bool
	Version   uint64
	Format    int
	State     string
	Origin    string
	IndexID   int64
	IndexName string
	DataID    int64
	DataName  string
	SystemID  int64
	FSID      string
	Run       string
	At        string
	Values    map[string]string
}

// markKeys are the omap keys of the mark.
var markKeys = []string{pkFormat, pkState, pkOrigin, pkIndexID, pkIndexName, pkDataID, pkDataName,
	pkSystemID, pkFSID, pkRun, pkAt, pkObjects, pkDigest, pkPools}

func parseID(s string) int64 {
	id, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return -1
	}

	return id
}

func strValues(vals map[string][]byte) map[string]string {
	out := make(map[string]string, len(vals))
	for k, v := range vals {
		out[k] = string(v)
	}

	return out
}

func fenceOf(vals map[string][]byte, ver uint64) Fence {
	if ver == 0 {
		return Fence{}
	}

	v := strValues(vals)

	return Fence{Exists: true, Version: ver, State: v[pkState], ToID: parseID(v[pkToID]), ToName: v[pkToName],
		IndexID: parseID(v[pkIndexID]), FSID: v[pkFSID], Run: v[pkRun], At: v[pkAt], By: v[pkBy], Values: v}
}

func markOf(vals map[string][]byte, ver uint64) Mark {
	if ver == 0 {
		return Mark{}
	}

	v := strValues(vals)
	format, _ := strconv.Atoi(v[pkFormat])

	return Mark{Exists: true, Version: ver, Format: format, State: v[pkState], Origin: v[pkOrigin],
		IndexID: parseID(v[pkIndexID]), IndexName: v[pkIndexName], DataID: parseID(v[pkDataID]), DataName: v[pkDataName],
		SystemID: parseID(v[pkSystemID]), FSID: v[pkFSID], Run: v[pkRun], At: v[pkAt], Values: v}
}

// placementView is what the process is configured for, resolved to ids.
type placementView struct {
	// SystemPool is ceph.system_pool ("" none); SystemID its id.
	SystemPool string
	SystemID   int64
	// IndexPool and IndexID, and the cluster's fsid.
	IndexPool string
	IndexID   int64
	FSID      string
}

// probeResult tells a new cluster from an existing one when neither a
// fence nor a mark exists (only to word the refusal).
type probeResult int

const (
	probesUnread probeResult = iota // not read (the periodic check)
	probesEmpty                     // none of the probe objects exists
	probesFound                     // one exists: an existing cluster
)

// errNeedProbes asks the caller of decidePlacement to read the probes.
var errNeedProbes = errors.New("placement: the probes decide the message")

// decidePlacement decides whether a process configured as v may use the
// cluster with fence f and mark m (temp/12 §4.3). It returns the
// placement state (PlacementNone, PlacementNative, PlacementMigrated)
// or a refusal wrapping ErrSystemPlacement. Without a system pool the
// mark is not read and m is ignored.
func decidePlacement(v placementView, f Fence, m Mark, probes probeResult) (string, error) {
	// A reverted fence is what a revert leaves behind for good, a leased
	// one is a tool's that has not changed anything yet: both count as
	// no fence.
	fenced := f.Exists && f.State != FenceReverted && f.State != FenceLeased

	if v.SystemPool == "" {
		if !fenced {
			return PlacementNone, nil
		}

		return "", fmt.Errorf("%w: the fence in the index pool %q (state %s, run %s) says it lives in pool %q (id %d); set ceph.system_pool (CEPH_SYSTEM_POOL) to that pool",
			ErrSystemMoved, v.IndexPool, f.State, f.Run, f.ToName, f.ToID)
	}

	// The mark first: a retired or foreign mark refuses whatever the
	// fence says.
	if err := checkMark(v, m); err != nil {
		return "", err
	}

	if !fenced {
		return "", unfenced(v, m, probes)
	}

	return decideFence(v, f, m)
}

// checkMark refuses a mark that is retired, unknown or of another index
// pool, system pool or cluster.
func checkMark(v placementView, m Mark) error {
	switch {
	case !m.Exists:
		return nil
	case m.State == MarkRetired:
		return fmt.Errorf("%w: system pool %q was withdrawn (mark retired by run %s); clear ceph.system_pool, or run \"shardpost system init\" or \"shardpost system migrate\" anew",
			ErrSystemPlacement, v.SystemPool, m.Run)
	case m.State != MarkActive:
		return fmt.Errorf("%w: the mark of system pool %q has the unknown state %q", ErrSystemPlacement, v.SystemPool, m.State)
	case m.IndexID != v.IndexID || m.FSID != v.FSID || m.SystemID != v.SystemID:
		return fmt.Errorf("%w: the mark of system pool %q belongs to index pool id %d, system pool id %d of cluster %s, not to index pool %q (id %d), system pool id %d of cluster %s",
			ErrSystemPlacement, v.SystemPool, m.IndexID, m.SystemID, m.FSID, v.IndexPool, v.IndexID, v.SystemID, v.FSID)
	default:
		return nil
	}
}

// unfenced is the refusal of a system pool without a fence: a mark
// without a fence, or a pool nobody prepared (the probes tell a new
// cluster from an existing one; errNeedProbes when they are unread).
func unfenced(v placementView, m Mark, probes probeResult) error {
	switch {
	case m.Exists:
		return fmt.Errorf("%w: inconsistent placement: system pool %q carries an active mark (run %s) but the index pool %q has no fence; run \"shardpost system status\"",
			ErrSystemPlacement, v.SystemPool, m.Run, v.IndexPool)
	case probes == probesUnread:
		return errNeedProbes
	case probes == probesEmpty:
		return fmt.Errorf("%w: system pool %q is not initialized and the index pool holds no system state (a new cluster): run \"shardpost system init --pool %s\"",
			ErrSystemPlacement, v.SystemPool, v.SystemPool)
	default:
		return fmt.Errorf("%w: system pool %q is not initialized and the index pool %q holds the system state of an existing cluster: run \"shardpost system migrate --to %s\"",
			ErrSystemPlacement, v.SystemPool, v.IndexPool, v.SystemPool)
	}
}

// decideFence decides by a fence in effect (the mark is checked).
func decideFence(v placementView, f Fence, m Mark) (string, error) {
	if f.ToID != v.SystemID {
		return "", fmt.Errorf("%w: the fence in the index pool (state %s, run %s) points to pool %q (id %d), not to ceph.system_pool %q (id %d)",
			ErrSystemPlacement, f.State, f.Run, f.ToName, f.ToID, v.SystemPool, v.SystemID)
	}

	switch f.State {
	case FenceInitializing:
		return "", fmt.Errorf("%w: the initialization of system pool %q (run %s) is incomplete: run \"shardpost system init --pool %s\" again",
			ErrSystemPlacement, v.SystemPool, f.Run, v.SystemPool)
	case FenceMigrating:
		return "", fmt.Errorf("%w: a migration of the system state to %q (run %s) is under way; the store cannot be used until it is done",
			ErrSystemPlacement, v.SystemPool, f.Run)
	case FenceDone, FenceNative:
	default:
		return "", fmt.Errorf("%w: the fence in the index pool has the unknown state %q", ErrSystemPlacement, f.State)
	}

	if !m.Exists {
		return "", fmt.Errorf("%w: inconsistent placement: the fence (state %s, run %s) says the system state lives in %q, but that pool carries no mark",
			ErrSystemPlacement, f.State, f.Run, v.SystemPool)
	}

	if f.State == FenceDone {
		return PlacementMigrated, nil
	}

	return PlacementNative, nil
}

// probeObjects are the objects of the index pool (namespace, object)
// whose presence shows the system state of an existing cluster there:
// stats only, never a listing.
var probeObjects = [][2]string{
	{model.AccountsNamespace, "ix.accounts/head"},
	{model.AdminNamespace, "ix.job/head"},
	{model.AdminNamespace, "ix.jobs/claimable/head"},
	{model.AdminNamespace, "auth-flush"},
	{model.LearningNamespace, "ix.learning/head"},
	{model.AdminNamespace, "kv." + model.FTSLayoutMarkKey},
}

// readObjectOmap reads keys of oid in namespace ns of pool on a private
// context: the values, the object's version (0: no object).
func readObjectOmap(conn *rados.Conn, pool, ns, oid string, keys []string) (map[string][]byte, uint64, error) {
	ioctx, err := conn.OpenIOContext(pool)
	if err != nil {
		return nil, 0, fmt.Errorf("open pool %q: %w", pool, err)
	}
	defer ioctx.Destroy()

	ioctx.SetNamespace(ns)

	return getOmapKeysVer(ioctx, oid, keys)
}

// placementInputs resolves the configuration to ids and reads the fence
// and (with a system pool) the mark.
func (s *Store) placementInputs(conn *rados.Conn) (placementView, Fence, Mark, error) {
	s.mu.RLock()
	cfg := s.cfg
	s.mu.RUnlock()

	v := placementView{SystemPool: cfg.SystemPool, IndexPool: cfg.IndexPool, FSID: s.fsid}

	var (
		f   Fence
		m   Mark
		err error
	)

	if v.SystemPool != "" {
		if v.IndexID, v.SystemID, err = systemPoolIDs(conn, cfg); err != nil {
			return v, f, m, err
		}
	}

	vals, ver, err := readObjectOmap(conn, cfg.IndexPool, model.AdminNamespace, FenceOID, fenceKeys)
	if err != nil {
		return v, f, m, fmt.Errorf("read the fence: %w", err)
	}

	f = fenceOf(vals, ver)

	if v.SystemPool != "" {
		vals, ver, err := readObjectOmap(conn, cfg.SystemPool, model.AdminNamespace, MarkOID, markKeys)
		if err != nil {
			return v, f, m, fmt.Errorf("read the mark: %w", err)
		}

		m = markOf(vals, ver)
	}

	return v, f, m, nil
}

// errPoolIDs marks a configuration whose pools are not distinct.
var errPoolIDs = errors.New("ceph.system_pool must be a pool of its own")

// systemPoolIDs returns the ids of the index and the system pool and
// refuses a system pool that is the index or the data pool (by id: one
// pool under two names is still one pool).
func systemPoolIDs(conn *rados.Conn, cfg Config) (int64, int64, error) {
	ids := map[string]int64{}

	for _, pool := range []string{cfg.IndexPool, cfg.DataPool, cfg.SystemPool} {
		id, err := conn.GetPoolByName(pool)
		if err != nil {
			return 0, 0, fmt.Errorf("pool %q: %w", pool, err)
		}

		ids[pool] = id
	}

	if ids[cfg.SystemPool] == ids[cfg.IndexPool] || ids[cfg.SystemPool] == ids[cfg.DataPool] {
		return 0, 0, fmt.Errorf("%w: %w (%q is the index or the data pool)", ErrSystemPlacement, errPoolIDs, cfg.SystemPool)
	}

	return ids[cfg.IndexPool], ids[cfg.SystemPool], nil
}

// readProbes stats the probe objects of the index pool.
func readProbes(conn *rados.Conn, index string) (probeResult, error) {
	ioctx, err := conn.OpenIOContext(index)
	if err != nil {
		return probesUnread, fmt.Errorf("open pool %q: %w", index, err)
	}
	defer ioctx.Destroy()

	for _, p := range probeObjects {
		ioctx.SetNamespace(p[0])

		_, err := ioctx.Stat(p[1])

		switch {
		case err == nil:
			return probesFound, nil
		case errCode(err) != syscall.ENOENT:
			return probesUnread, fmt.Errorf("stat %s/%s: %w", p[0], p[1], err)
		}
	}

	return probesEmpty, nil
}

// errPlacementRead wraps what kept a check from reading the placement.
var errPlacementRead = errors.New("placement unknown")

// placementReadFail, when set by a test, fails the reads of a check
// with this error: a cluster that does not answer.
var placementReadFail atomic.Pointer[error]

// checkPlacement reads the placement and decides it; with probes it
// reads the probe objects when they decide the wording of a refusal,
// without it (the periodic check) a missing fence and mark is a
// contradiction as it stands. The gauge follows the result. A read
// error wraps errPlacementRead.
func (s *Store) checkPlacement(probes bool) (string, error) {
	r := s.ref()
	defer r.refs.Done()

	var (
		v   placementView
		f   Fence
		m   Mark
		err error
	)

	if p := placementReadFail.Load(); p != nil {
		err = *p
	} else {
		v, f, m, err = s.placementInputs(r.conn)
	}

	if errors.Is(err, ErrSystemPlacement) {
		return "", err
	}

	if err != nil {
		s.notePlacement(PlacementUnknown)
		return "", fmt.Errorf("%w: %w", errPlacementRead, err)
	}

	state, err := decidePlacement(v, f, m, probesUnread)
	if errors.Is(err, errNeedProbes) {
		if !probes {
			return "", fmt.Errorf("%w: neither the fence in the index pool nor the mark of system pool %q exists any more",
				ErrSystemPlacement, v.SystemPool)
		}

		res, perr := readProbes(r.conn, v.IndexPool)
		if perr != nil {
			s.notePlacement(PlacementUnknown)
			return "", fmt.Errorf("%w: %w", errPlacementRead, perr)
		}

		state, err = decidePlacement(v, f, m, res)
	}

	if err == nil {
		s.notePlacement(state)
	}

	return state, err
}

// notePlacement records the state of the last check (Placement, the
// gauge).
func (s *Store) notePlacement(state string) {
	s.placeState.Store(&state)
	metrics.SetSystemPlacement(state)
}

// Placement is the placement state the last check found ("" when the
// store was opened as a tool and checked nothing) and the system pool
// (nil without ceph.system_pool).
func (s *Store) Placement() (string, *model.PoolRef) {
	state := ""
	if p := s.placeState.Load(); p != nil {
		state = *p
	}

	pool, err := s.SystemPool()
	if err != nil {
		s.mu.RLock()
		pool = &model.PoolRef{ID: -1, Name: s.cfg.SystemPool}
		s.mu.RUnlock()
	}

	return state, pool
}

// CheckPlacement checks the placement as Open does (it is how a store
// opened with Config.PlacementTool, the bench's, checks it once it set
// its pools up): nil when the configuration matches the cluster, else a
// refusal wrapping ErrSystemPlacement or a read error.
func (s *Store) CheckPlacement() error {
	_, err := s.checkPlacement(true)
	return err
}

// SystemPool is the system pool the store works with, by id and name
// (nil without ceph.system_pool).
func (s *Store) SystemPool() (*model.PoolRef, error) {
	s.mu.RLock()
	cfg := s.cfg
	s.mu.RUnlock()

	if cfg.SystemPool == "" {
		return nil, nil
	}

	var ref *model.PoolRef

	err := s.WithConn(func(conn *rados.Conn) error {
		id, err := conn.GetPoolByName(cfg.SystemPool)
		if err != nil {
			return fmt.Errorf("pool %q: %w", cfg.SystemPool, err)
		}

		ref = &model.PoolRef{ID: id, Name: cfg.SystemPool}

		return nil
	})

	return ref, err
}

// placementWatch is the periodic placement check of a store and the
// state of the last check.
type placementWatch struct {
	once sync.Once
	stop chan struct{}
	done chan struct{}
}

// startPlacementWatch checks the placement every interval until the
// store drains or closes; lost is called once, for a contradiction read
// positively, and the check ends then.
func (s *Store) startPlacementWatch(every time.Duration, lost func(error)) {
	s.place.stop = make(chan struct{})
	s.place.done = make(chan struct{})

	go func() {
		defer close(s.place.done)

		t := time.NewTicker(every)
		defer t.Stop()

		for {
			select {
			case <-s.place.stop:
				return
			case <-t.C:
			}

			_, err := s.checkPlacement(false)

			switch {
			case err == nil:
			case errors.Is(err, errPlacementRead):
				s.log.Warn("system placement could not be checked", "event", "system_placement_unknown", "err", err)
			default:
				s.log.Error("system placement contradicts the configuration", "event", "system_placement_lost", "err", err)
				lost(err)

				return
			}
		}
	}()
}

// stopPlacementWatch ends the periodic check and waits for it (Drain,
// Close; a store without one returns at once).
func (s *Store) stopPlacementWatch() {
	if s.place.stop == nil {
		return
	}

	s.place.once.Do(func() { close(s.place.stop) })
	<-s.place.done
}
