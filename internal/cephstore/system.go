package cephstore

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/ceph/go-ceph/rados"

	"shardpost/internal/kvindex"
	"shardpost/internal/learnindex"
	"shardpost/internal/model"
	"shardpost/internal/sortedindex"
)

// Initialization of a system pool (temp/12 §4.3, "shardpost system
// init"): a new cluster gets its store-wide state in the system pool
// from the start. The fence in the index pool is the arbiter:
//
//  0. fence native or done for this pool and an active mark of this
//     index pool and cluster: nothing to do (InitResult.Already); this
//     step only reads;
//     the tool lease (fencelease.go) is taken: an absent or reverted
//     fence becomes "leased", which the servers treat as no fence; an
//     "initializing" fence for this pool is taken over with its run (an
//     init that crashed);
//  1. the index pool holds nothing of the store's own namespaces (else
//     the cluster exists and needs a migration), the system pool at most
//     the index heads of this same run, or a retired mark of an earlier
//     run with its copies (InitOptions.Confirm names that run);
//  2. a probe object with omap in the system pool (an erasure coded
//     pool fails here);
//  3. the fence to "initializing" by compare-and-swap, with a new run;
//     only now are the confirmed copies of a retired run deleted;
//  4. the completion markers of the backfills in the heads of the empty
//     indexes (admin store, account directory, learning queue), so that
//     admin.kv_index and imapsieve.learning.index in read mode read from
//     the first start without a listing;
//  5. the mark, created exclusively: the commit;
//  6. the fence to "native", giving the lease up.
//
// A refusal (steps 1 and 2) gives the lease up and leaves the fence as
// it was. A crash between 3 and 6 leaves the fence "initializing"; a
// server refuses to start until init has run again and completed (a
// second init takes the lease over once it is stale). Every write of
// steps 3 to 6 needs a valid lease; one that ran out ends the init with
// ErrLeaseLost, and running it again continues.

// SiteSystemInit labels the listings of the init in
// imapceph_rados_listings_total.
const SiteSystemInit = "system_init"

// InitBy is what the init records as its author by default.
const InitBy = "system init"

// ErrInitRefused is the error of every refusal of the init (exit code 2
// of the tool): the pools are not in a state the init may start from.
var ErrInitRefused = errors.New("system init refused")

// InitOptions configure InitSystem.
type InitOptions struct {
	// Confirm is the run of a retired mark in the system pool: its mark
	// and the copies of its run are deleted before the init writes.
	Confirm string
	// By names who runs the init (InitBy when empty).
	By string
	// Log receives the progress (the store's log when nil).
	Log *slog.Logger
}

// InitResult is what InitSystem did.
type InitResult struct {
	// Run identifies the init (the one that was completed, or found).
	Run string `json:"run"`
	// Already is set when the pool was initialized before; nothing was
	// written.
	Already bool `json:"already,omitempty"`
	// Resumed is set when an incomplete init of this pool was completed.
	Resumed bool `json:"resumed,omitempty"`
	// Cleared counts the objects of a retired run that were deleted.
	Cleared int `json:"cleared,omitempty"`
}

// initStep is called after the steps "lease", "fence", "markers" and
// "mark" of an init; a test makes it fail to leave the init there.
var initStep = func(string) error { return nil }

// initMarker is the value of the backfill completion markers the init
// writes: what sortedindex.Backfilled reads (a set "finished").
type initMarker struct {
	Started  time.Time `json:"started"`
	Finished time.Time `json:"finished"`
	By       string    `json:"by"`
	Run      string    `json:"run"`
}

// InitSystem initializes Config.SystemPool for the store's index pool
// (see above). It is the routine of "shardpost system init" and of the
// bench; it only writes into an empty system pool of an index pool
// without system state, and is idempotent.
func (s *Store) InitSystem(o InitOptions) (*InitResult, error) {
	s.mu.RLock()
	cfg := s.cfg
	s.mu.RUnlock()

	if cfg.SystemPool == "" {
		return nil, fmt.Errorf("%w: no system pool configured", ErrInitRefused)
	}

	if err := checkToolClientOptions(cfg.ClientOptions, 1); err != nil {
		return nil, fmt.Errorf("%w: ceph.client_options: %w", ErrInitRefused, err)
	}

	if o.By == "" {
		o.By = InitBy
	}

	if o.Log == nil {
		o.Log = s.log
	}

	in := &initRun{s: s, cfg: cfg, o: o, log: o.Log.With("component", "system_init", "system_pool", cfg.SystemPool)}

	var res *InitResult

	err := s.WithConn(func(conn *rados.Conn) error {
		in.conn = conn

		var err error

		res, err = in.do()

		return err
	})

	return res, err
}

// initRun is one InitSystem.
type initRun struct {
	s    *Store
	cfg  Config
	o    InitOptions
	log  *slog.Logger
	conn *rados.Conn
	v    placementView
	run  string
	// lease is the tool lease in the fence.
	lease *fenceLease
}

// initialized reports whether fence and mark show this pool initialized
// (or migrated) for this index pool and cluster.
func initialized(v placementView, f Fence, m Mark) bool {
	return (f.State == FenceNative || f.State == FenceDone) && f.ToID == v.SystemID && m.Exists && m.State == MarkActive &&
		m.IndexID == v.IndexID && m.SystemID == v.SystemID && m.FSID == v.FSID
}

func (in *initRun) do() (*InitResult, error) {
	v, f, m, err := in.s.placementInputs(in.conn)
	if err != nil {
		return nil, err
	}

	in.v = v

	// Step 0: done before, nothing to write.
	if initialized(v, f, m) {
		in.log.Info("system pool already initialized", pkRun, m.Run, "fence", f.State)
		return &InitResult{Run: m.Run, Already: true}, nil
	}

	in.lease = newFenceLease(in.conn, in.cfg.IndexPool, in.log)
	defer in.lease.close()

	resumed, err := in.takeLease()
	if errors.Is(err, errFenceStop) {
		return in.finishedMeanwhile()
	}

	if err != nil {
		return nil, err
	}

	res := &InitResult{Resumed: resumed}

	if err := in.work(res); err != nil {
		// A refusal gives the lease up (a fence that is still "leased"
		// goes back to what it was), so that nothing is left that a
		// server or the next tool would have to wait for; anything else
		// (a crash, a lost lease) leaves it to be taken over.
		if errors.Is(err, ErrInitRefused) {
			if rerr := in.lease.release(); rerr != nil {
				in.log.Warn("fence lease not given up", "err", rerr)
			}
		}

		return nil, err
	}

	in.log.Info("system pool initialized", "event", "system_init_done", pkRun, in.run, "resumed", resumed, "cleared", res.Cleared)

	return res, nil
}

// takeLease takes the fence lease and reports whether an incomplete
// init of this pool is taken over. errFenceStop: another init finished
// meanwhile.
func (in *initRun) takeLease() (bool, error) {
	f, err := in.lease.acquire(in.leasePlan)
	if errors.Is(err, ErrFenceHeld) {
		return false, fmt.Errorf("%w: %w", ErrInitRefused, err)
	}

	if err != nil {
		return false, err
	}

	resumed := f.State == FenceInitializing
	if resumed {
		in.run = f.Run
		in.log.Info("incomplete system init found, completing it", pkRun, f.Run)
	}

	return resumed, initStep("lease")
}

// leasePlan is what the fence becomes with the init's lease: "leased"
// over an absent, reverted or leased one, unchanged when an init of this
// pool is to be completed.
func (in *initRun) leasePlan(f Fence) (map[string]string, error) {
	switch {
	case f.Exists && (f.State == FenceNative || f.State == FenceDone) && f.ToID == in.v.SystemID:
		return nil, errFenceStop
	case f.Exists && f.State == FenceInitializing && f.ToID == in.v.SystemID:
		return maps.Clone(f.Values), nil
	case !f.Exists || f.State == FenceReverted || f.State == FenceLeased:
		return leasedValues(f, in.o.By), nil
	default:
		return nil, fmt.Errorf("%w: the fence in the index pool has state %s (run %s, pool %q)",
			ErrInitRefused, f.State, f.Run, f.ToName)
	}
}

// finishedMeanwhile is the result of an init that found the fence
// finished by another one while it waited for the lease.
func (in *initRun) finishedMeanwhile() (*InitResult, error) {
	v, f, m, err := in.s.placementInputs(in.conn)
	if err != nil {
		return nil, err
	}

	if initialized(v, f, m) {
		in.log.Info("system pool initialized by another init meanwhile", pkRun, m.Run)
		return &InitResult{Run: m.Run, Already: true}, nil
	}

	return nil, fmt.Errorf("%w: the fence changed while waiting (state %s, run %s)", ErrInitRefused, f.State, f.Run)
}

// work runs steps 1 to 6 under the lease.
func (in *initRun) work(res *InitResult) error {
	wipe, err := in.check()
	if err != nil {
		return err
	}

	// Step 3.
	if err := in.startFence(); err != nil {
		return err
	}

	if err := initStep("fence"); err != nil {
		return err
	}

	res.Run = in.run

	if wipe {
		n, err := in.clearSystemPool()
		res.Cleared = n

		if err != nil {
			return err
		}
	}

	return in.commit()
}

// check runs steps 1 and 2, on what it reads under the lease; wipe is
// set for a confirmed retired run.
func (in *initRun) check() (wipe bool, err error) {
	_, f, m, err := in.s.placementInputs(in.conn)
	if err != nil {
		return false, err
	}

	if err := in.checkIndexPool(); err != nil {
		return false, err
	}

	if wipe, err = in.checkSystemPool(f, m); err != nil {
		return false, err
	}

	return wipe, in.probe()
}

// commit runs steps 4 to 6: the markers, the mark, the final fence.
func (in *initRun) commit() error {
	if err := in.markers(); err != nil {
		return err
	}

	if err := initStep("markers"); err != nil {
		return err
	}

	if err := in.mark(); err != nil {
		return err
	}

	if err := initStep("mark"); err != nil {
		return err
	}

	return in.finishFence()
}

// listNamespace lists the objects of namespace ns of pool.
func listNamespace(conn *rados.Conn, pool, ns string) ([]string, error) {
	ioctx, err := conn.OpenIOContext(pool)
	if err != nil {
		return nil, fmt.Errorf("open pool %q: %w", pool, err)
	}
	defer ioctx.Destroy()

	ioctx.SetNamespace(ns)

	it, err := listObjects(ioctx, SiteSystemInit)
	if err != nil {
		return nil, fmt.Errorf("list %s/%s: %w", pool, ns, err)
	}
	defer it.Close()

	var out []string

	for it.Next() {
		out = append(out, it.Value())
	}

	return out, it.Err()
}

// placementObject reports whether oid of namespace ns is one of the
// placement objects ("system.*" in the admin namespace), which the
// checks of the init leave to the fence and mark logic.
func placementObject(ns, oid string) bool {
	return ns == model.AdminNamespace && strings.HasPrefix(oid, "system.")
}

// checkIndexPool refuses an index pool that holds anything of the
// store's own namespaces besides the fence and the sweep's objects (an
// existing cluster: its state is migrated, not initialized).
func (in *initRun) checkIndexPool() error {
	res, err := readProbes(in.conn, in.cfg.IndexPool)
	if err != nil {
		return err
	}

	if res == probesFound {
		return fmt.Errorf("%w: the index pool %q holds the system state of an existing cluster: use \"shardpost system migrate --to %s\"",
			ErrInitRefused, in.cfg.IndexPool, in.cfg.SystemPool)
	}

	for _, ns := range model.SystemNamespaces() {
		oids, err := listNamespace(in.conn, in.cfg.IndexPool, ns)
		if err != nil {
			return err
		}

		for _, oid := range oids {
			if placementObject(ns, oid) || (ns == model.AccountsNamespace && strings.HasPrefix(oid, "sweep/")) {
				continue
			}

			return fmt.Errorf("%w: the index pool %q holds %s/%s of the store's own state (an existing cluster): use \"shardpost system migrate --to %s\"",
				ErrInitRefused, in.cfg.IndexPool, ns, oid, in.cfg.SystemPool)
		}
	}

	return nil
}

// checkSystemPool refuses a system pool that holds anything but the
// index heads of the init run the fence names (one that did not
// finish). A retired mark of an earlier run and its copies are allowed
// when InitOptions.Confirm names that run: wipe is set then, and they
// are deleted after step 3.
func (in *initRun) checkSystemPool(f Fence, m Mark) (wipe bool, err error) {
	run := in.unfinished(f)

	foreign, err := in.foreignObjects(run)
	if err != nil {
		return false, err
	}

	switch {
	case m.Exists && m.State == MarkRetired:
		if in.o.Confirm != m.Run {
			return false, fmt.Errorf("%w: system pool %q holds the retired mark of run %s and %d objects of it; confirm that run to delete them",
				ErrInitRefused, in.cfg.SystemPool, m.Run, len(foreign))
		}

		return true, nil
	case m.Exists && (m.State != MarkActive || run == "" || m.Run != run):
		return false, fmt.Errorf("%w: system pool %q carries a mark (state %s, run %s) that no init of it left unfinished",
			ErrInitRefused, in.cfg.SystemPool, m.State, m.Run)
	case len(foreign) > 0:
		return false, fmt.Errorf("%w: system pool %q is not empty: %d objects in the store's namespaces, first %s",
			ErrInitRefused, in.cfg.SystemPool, len(foreign), foreign[0])
	}

	return false, nil
}

// unfinished is the run of an init of this pool that f shows unfinished
// ("" none).
func (in *initRun) unfinished(f Fence) string {
	if f.Exists && f.State == FenceInitializing && f.ToID == in.v.SystemID {
		return f.Run
	}

	return ""
}

// foreignObjects lists the objects of the store's namespaces in the
// system pool that are neither the mark, a probe left over, nor an index
// head of run.
func (in *initRun) foreignObjects(run string) ([]string, error) {
	var foreign []string

	for _, ns := range model.SystemNamespaces() {
		oids, err := listNamespace(in.conn, in.cfg.SystemPool, ns)
		if err != nil {
			return nil, err
		}

		for _, oid := range oids {
			switch {
			case ns == model.AdminNamespace && (oid == MarkOID || oid == probeOID):
			case run != "" && !placementObject(ns, oid) && in.ownHead(ns, oid, run):
			default:
				foreign = append(foreign, ns+"/"+oid)
			}
		}
	}

	return foreign, nil
}

// headKeys are the keys of an index head besides its markers
// (sortedindex).
var headKeys = map[string]bool{"shards": true, pkFormat: true, "phase": true}

// ownHead reports whether oid is an index head the init run wrote: only
// the head keys and backfill markers of that run.
func (in *initRun) ownHead(ns, oid, run string) bool {
	if !strings.HasPrefix(oid, "ix.") || !strings.HasSuffix(oid, "/head") {
		return false
	}

	ioctx, err := in.conn.OpenIOContext(in.cfg.SystemPool)
	if err != nil {
		return false
	}
	defer ioctx.Destroy()

	ioctx.SetNamespace(ns)

	vals, err := ioctx.GetAllOmapValues(oid, "", "", omapPageSize)
	if err != nil {
		return false
	}

	for k, v := range vals {
		switch {
		case headKeys[k]:
		case strings.HasPrefix(k, "mark:backfill."):
			var mk initMarker
			if json.Unmarshal(v, &mk) != nil || mk.Run != run {
				return false
			}
		default:
			return false
		}
	}

	return true
}

// clearSystemPool deletes everything in the store's namespaces of the
// system pool (a confirmed retired run), under the lease.
func (in *initRun) clearSystemPool() (int, error) {
	ioctx, err := in.conn.OpenIOContext(in.cfg.SystemPool)
	if err != nil {
		return 0, fmt.Errorf("open pool %q: %w", in.cfg.SystemPool, err)
	}
	defer ioctx.Destroy()

	n := 0

	for _, ns := range model.SystemNamespaces() {
		oids, err := listNamespace(in.conn, in.cfg.SystemPool, ns)
		if err != nil {
			return n, err
		}

		ioctx.SetNamespace(ns)

		for _, oid := range oids {
			if err := in.lease.check(); err != nil {
				return n, err
			}

			if err := ioctx.Delete(oid); err != nil && errCode(err) != syscall.ENOENT {
				return n, fmt.Errorf("delete %s/%s: %w", ns, oid, err)
			}

			n++
		}
	}

	in.log.Info("retired system pool content deleted", pkRun, in.o.Confirm, pkObjects, n)

	return n, nil
}

// probe writes and removes an object with omap in the system pool.
func (in *initRun) probe() error {
	if err := in.lease.check(); err != nil {
		return err
	}

	ioctx, err := in.conn.OpenIOContext(in.cfg.SystemPool)
	if err != nil {
		return fmt.Errorf("open pool %q: %w", in.cfg.SystemPool, err)
	}
	defer ioctx.Destroy()

	ioctx.SetNamespace(model.AdminNamespace)

	op := rados.CreateWriteOp()
	defer op.Release()

	op.Create(rados.CreateIdempotent)
	op.SetOmap(map[string][]byte{"probe": []byte(time.Now().UTC().Format(time.RFC3339))})

	if err := op.Operate(ioctx, probeOID, rados.OperationNoFlag); err != nil {
		if errCode(err) == syscall.EOPNOTSUPP {
			return fmt.Errorf("%w: pool %q does not keep omap (an erasure coded pool?); the system pool must be replicated",
				ErrInitRefused, in.cfg.SystemPool)
		}

		return fmt.Errorf("probe pool %q: %w", in.cfg.SystemPool, err)
	}

	if err := ioctx.Delete(probeOID); err != nil && errCode(err) != syscall.ENOENT {
		return fmt.Errorf("probe pool %q: %w", in.cfg.SystemPool, err)
	}

	return nil
}

// fenceRetries bounds the compare-and-swap rounds on the fence.
const fenceRetries = 16

// startFence sets the fence to "initializing" with a new run (step 3);
// a fence taken over in that state keeps its run.
func (in *initRun) startFence() error {
	if in.run != "" {
		return nil
	}

	run := newGUID()[:12]

	err := in.lease.update(func(vals map[string]string) error {
		if vals[pkState] != FenceLeased {
			return fmt.Errorf("%w: the fence changed under the lease (state %s)", ErrInitRefused, vals[pkState])
		}

		epoch := vals[keyToolEpoch]
		clear(vals)

		maps.Copy(vals, map[string]string{pkState: FenceInitializing, pkToID: strconv.FormatInt(in.v.SystemID, 10),
			pkToName: in.cfg.SystemPool, pkIndexID: strconv.FormatInt(in.v.IndexID, 10), pkFSID: in.v.FSID,
			pkRun: run, pkAt: time.Now().UTC().Format(time.RFC3339), pkBy: in.o.By, keyToolEpoch: epoch})

		return nil
	}, false)
	if err != nil {
		return err
	}

	in.run = run
	in.log.Info("fence set", pkState, FenceInitializing, pkRun, run)

	return nil
}

// markers writes the completion markers of the backfills (step 4).
func (in *initRun) markers() error {
	now := time.Now().UTC()

	b, err := json.Marshal(initMarker{Started: now, Finished: now, By: in.o.By, Run: in.run})
	if err != nil {
		return err
	}

	set := func(ns, name string, shards int, key string) error {
		if err := in.lease.check(); err != nil {
			return err
		}

		ix, release, err := in.s.openIndex(ns, name, shards)
		if err != nil {
			return err
		}
		defer release()

		if err := ix.SetMarker(key, string(b)); err != nil {
			return fmt.Errorf("marker %s in %s/%s: %w", key, ns, sortedindex.HeadOID(name), err)
		}

		return nil
	}

	kv := kvindex.MarkerCollection()
	if err := set(model.AdminNamespace, kv.Name, kv.Shards, kvindex.MarkerKV); err != nil {
		return err
	}

	if err := set(model.AccountsNamespace, model.AccountIndexName, model.AccountIndexShards, model.AccountBackfillMarker); err != nil {
		return err
	}

	shards := in.cfg.LearningQueueShards
	if shards <= 0 {
		shards = model.DefaultQueueShards
	}

	return set(model.LearningNamespace, model.LearningQueue, min(shards, model.MaxQueueShards), learnindex.MarkerBackfill)
}

// markValues are the values of the mark of this init.
func (in *initRun) markValues() (map[string][]byte, error) {
	dataID, err := in.conn.GetPoolByName(in.cfg.DataPool)
	if err != nil {
		return nil, fmt.Errorf("pool %q: %w", in.cfg.DataPool, err)
	}

	id := func(n int64) []byte { return []byte(strconv.FormatInt(n, 10)) }

	return map[string][]byte{pkFormat: []byte(strconv.Itoa(MarkFormat)), pkState: []byte(MarkActive), pkOrigin: []byte(OriginNative),
		pkIndexID: id(in.v.IndexID), pkIndexName: []byte(in.cfg.IndexPool), pkDataID: id(dataID), pkDataName: []byte(in.cfg.DataPool),
		pkSystemID: id(in.v.SystemID), pkFSID: []byte(in.v.FSID), pkRun: []byte(in.run),
		pkAt: []byte(time.Now().UTC().Format(time.RFC3339)), pkObjects: []byte("0"), pkPools: []byte("{}")}, nil
}

// mark creates the mark (step 5, the commit); one this run wrote before
// is accepted.
func (in *initRun) mark() error {
	vals, err := in.markValues()
	if err != nil {
		return err
	}

	ioctx, err := in.conn.OpenIOContext(in.cfg.SystemPool)
	if err != nil {
		return fmt.Errorf("open pool %q: %w", in.cfg.SystemPool, err)
	}
	defer ioctx.Destroy()

	ioctx.SetNamespace(model.AdminNamespace)

	op := rados.CreateWriteOp()
	defer op.Release()

	op.Create(rados.CreateExclusive)
	op.SetOmap(vals)

	if err := in.lease.check(); err != nil {
		return err
	}

	err = op.Operate(ioctx, MarkOID, rados.OperationNoFlag)

	switch {
	case err == nil:
		in.log.Info("mark written", pkRun, in.run)
		return nil
	case errCode(err) != syscall.EEXIST:
		return fmt.Errorf("write the mark: %w", err)
	default:
		return in.ownMark(ioctx)
	}
}

// ownMark accepts an existing mark only when this run wrote it.
func (in *initRun) ownMark(ioctx *rados.IOContext) error {
	got, ver, err := getOmapKeysVer(ioctx, MarkOID, markKeys)
	if err != nil {
		return fmt.Errorf("read the mark: %w", err)
	}

	m := markOf(got, ver)
	if !m.Exists || m.State != MarkActive || m.Origin != OriginNative || m.Run != in.run ||
		m.IndexID != in.v.IndexID || m.SystemID != in.v.SystemID || m.FSID != in.v.FSID {
		return fmt.Errorf("%w: system pool %q carries a mark of another init (state %s, run %s)", ErrInitRefused, in.cfg.SystemPool, m.State, m.Run)
	}

	return nil
}

// finishFence sets the fence to "native" and gives the lease up
// (step 6).
func (in *initRun) finishFence() error {
	err := in.lease.update(func(vals map[string]string) error {
		if vals[pkState] != FenceInitializing || vals[pkRun] != in.run {
			return fmt.Errorf("%w: the fence changed under the lease (state %s, run %s)", ErrInitRefused, vals[pkState], vals[pkRun])
		}

		vals[pkState], vals[pkAt] = FenceNative, time.Now().UTC().Format(time.RFC3339)

		return nil
	}, true)
	if err != nil {
		return err
	}

	in.log.Info("fence set", pkState, FenceNative, pkRun, in.run)

	return nil
}
