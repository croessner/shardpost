//go:build tentacle

package cephstore

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ceph/go-ceph/rados"
	dto "github.com/prometheus/client_model/go"

	"shardpost/internal/imapsieve"
	"shardpost/internal/kvindex"
	"shardpost/internal/learnindex"
	"shardpost/internal/metrics"
	"shardpost/internal/model"
	"shardpost/internal/sortedindex"
)

// Tests of the system pool against the cluster. They work on a pair of
// pools of their own (SHARDPOST_TEST_CEPH_MIG_INDEX_POOL and
// SHARDPOST_TEST_CEPH_MIG_SYSTEM_POOL, created by
// hack/ceph-test-cluster.sh), which every test empties first; one test
// at a time.

var migMu sync.Mutex

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// migConfig is the configuration of the test pools: the store's own
// namespaces in the system pool.
func migConfig(t *testing.T) Config {
	t.Helper()

	testStore(t)

	idx, sys := os.Getenv("SHARDPOST_TEST_CEPH_MIG_INDEX_POOL"), os.Getenv("SHARDPOST_TEST_CEPH_MIG_SYSTEM_POOL")
	if idx == "" || sys == "" {
		t.Skip("no system pool test pools (hack/ceph-test-cluster.sh creates them)")
	}

	if os.Getenv("SHARDPOST_TEST_CEPH_SYSTEM_POOL") != "" {
		t.Skip("covered by the run without a system pool")
	}

	cfg := testConfig()
	cfg.IndexPool, cfg.SystemPool = idx, sys

	return cfg
}

// migPools takes the test pools for the test, empties them and returns
// a store on them opened as a tool, and the configuration.
func migPools(t *testing.T) (*Store, Config) {
	t.Helper()

	cfg := migConfig(t)

	migMu.Lock()
	t.Cleanup(migMu.Unlock)

	tool := cfg
	tool.PlacementTool = true

	s := mustOpen(t, tool)

	emptyPool(t, s, cfg.IndexPool)
	emptyPool(t, s, cfg.SystemPool)
	sortedindex.ForgetHeads()

	return s, cfg
}

// mustOpen opens a store, closed with the test.
func mustOpen(t *testing.T, cfg Config) *Store {
	t.Helper()

	s, err := Open(cfg, quiet())
	if err != nil {
		t.Fatalf("open: %v", err)
	}

	t.Cleanup(func() {
		if s.Drain(10 * time.Second) {
			s.Close()
		}
	})

	return s
}

// emptyPool deletes every object of a test pool.
func emptyPool(t *testing.T, s *Store, pool string) {
	t.Helper()

	err := s.WithConn(func(conn *rados.Conn) error {
		ioctx, err := conn.OpenIOContext(pool)
		if err != nil {
			return err
		}
		defer ioctx.Destroy()

		ioctx.SetNamespace(rados.AllNamespaces)

		it, err := ioctx.Iter()
		if err != nil {
			return err
		}

		type obj struct{ ns, oid string }

		var all []obj

		for it.Next() {
			all = append(all, obj{it.Namespace(), it.Value()})
		}

		it.Close()

		for _, o := range all {
			ioctx.SetNamespace(o.ns)

			if err := ioctx.Delete(o.oid); err != nil && errCode(err) != 2 {
				return err
			}
		}

		return nil
	})
	if err != nil {
		t.Fatalf("empty %s: %v", pool, err)
	}
}

// putOmap writes an object with these omap values (replacing others).
func putOmap(t *testing.T, s *Store, pool, ns, oid string, vals map[string]string) {
	t.Helper()

	err := s.WithConn(func(conn *rados.Conn) error {
		ioctx, err := conn.OpenIOContext(pool)
		if err != nil {
			return err
		}
		defer ioctx.Destroy()

		ioctx.SetNamespace(ns)

		op := rados.CreateWriteOp()
		defer op.Release()

		op.Create(rados.CreateIdempotent)
		op.CleanOmap()
		op.SetOmap(toBytes(vals))

		return op.Operate(ioctx, oid, rados.OperationNoFlag)
	})
	if err != nil {
		t.Fatalf("write %s/%s: %v", ns, oid, err)
	}
}

// names lists namespace ns of pool.
func names(t *testing.T, s *Store, pool, ns string) []string {
	t.Helper()

	var out []string

	err := s.WithConn(func(conn *rados.Conn) error {
		var err error

		out, err = listNamespace(conn, pool, ns)

		return err
	})
	if err != nil {
		t.Fatal(err)
	}

	slices.Sort(out)

	return out
}

// inputs reads the placement as a check does.
func inputs(t *testing.T, s *Store) (placementView, Fence, Mark) {
	t.Helper()

	var (
		v placementView
		f Fence
		m Mark
	)

	err := s.WithConn(func(conn *rados.Conn) error {
		var err error

		v, f, m, err = s.placementInputs(conn)

		return err
	})
	if err != nil {
		t.Fatal(err)
	}

	return v, f, m
}

// fenceVals are the values of a fence written by hand.
func fenceVals(v placementView, state string) map[string]string {
	return map[string]string{pkState: state, pkToID: itoa(v.SystemID), pkToName: v.SystemPool, pkIndexID: itoa(v.IndexID),
		pkFSID: v.FSID, pkRun: "hand", pkAt: time.Now().UTC().Format(time.RFC3339), pkBy: "test"}
}

// markVals are the values of a mark written by hand.
func markVals(v placementView, state string) map[string]string {
	return map[string]string{"format": "1", "state": state, "origin": OriginMigrated, "index_id": itoa(v.IndexID),
		"system_id": itoa(v.SystemID), "fsid": v.FSID, "run": "hand"}
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

// serverConfig is the configuration of a server on the test pools.
func serverConfig(cfg Config) Config {
	cfg.PlacementTool = false
	cfg.KVIndex, cfg.LearningIndex = model.KVIndexRead, model.KVIndexRead

	return cfg
}

// shortLease makes the tool lease quick for a test.
func shortLease(t *testing.T) {
	t.Helper()

	renew, valid, takeover, poll := toolLeaseRenew, toolLeaseValid, toolLeaseTakeover, toolLeasePoll
	toolLeaseRenew, toolLeaseValid, toolLeaseTakeover, toolLeasePoll = 50*time.Millisecond, 2*time.Second, 300*time.Millisecond, 20*time.Millisecond

	t.Cleanup(func() {
		toolLeaseRenew, toolLeaseValid, toolLeaseTakeover, toolLeasePoll = renew, valid, takeover, poll
	})
}

func listings(t *testing.T, site string) float64 {
	t.Helper()

	var m dto.Metric
	if err := metrics.RadosListings.WithLabelValues(site).Write(&m); err != nil {
		t.Fatal(err)
	}

	return m.GetCounter().GetValue()
}

// mustInit initializes the system pool of the test pools.
func mustInit(t *testing.T, tool *Store) *InitResult {
	t.Helper()

	res, err := tool.InitSystem(InitOptions{})
	if err != nil {
		t.Fatalf("init: %v", err)
	}

	return res
}

// wantRefusal opens a store with cfg and expects a refusal containing
// want.
func wantRefusal(t *testing.T, cfg Config, want string) {
	t.Helper()

	s, err := Open(cfg, quiet())
	if err == nil {
		s.Drain(10 * time.Second)
		s.Close()

		t.Fatalf("opened, want a refusal with %q", want)
	}

	if !strings.Contains(err.Error(), want) {
		t.Fatalf("%v, want a refusal with %q", err, want)
	}
}

// onlyFence checks that the index pool holds nothing of the store's own
// namespaces but the fence.
func onlyFence(t *testing.T, s *Store, cfg Config) {
	t.Helper()

	for _, ns := range model.SystemNamespaces() {
		got := names(t, s, cfg.IndexPool, ns)

		want := []string(nil)
		if ns == model.AdminNamespace {
			want = []string{FenceOID}
		}

		if !slices.Equal(got, want) {
			t.Errorf("index pool %s holds %v", ns, got)
		}
	}
}

// hasPrefix checks that namespace ns of pool holds an object with the
// prefix.
func hasPrefix(t *testing.T, s *Store, pool, ns, prefix string) {
	t.Helper()

	got := names(t, s, pool, ns)
	if !slices.ContainsFunc(got, func(n string) bool { return strings.HasPrefix(n, prefix) }) {
		t.Errorf("%s %s lacks %s: %v", pool, ns, prefix, got)
	}
}

// TestInitSystem: an init of empty pools sets the fence native and gives
// the lease up, writes the mark, and keeps nothing but the fence in the
// index pool. Before it a server refuses, naming the init.
func TestInitSystem(t *testing.T) {
	tool, cfg := migPools(t)

	wantRefusal(t, serverConfig(cfg), "shardpost system init --pool")

	res := mustInit(t, tool)
	if res.Run == "" || res.Already || res.Resumed {
		t.Fatalf("init: %+v", res)
	}

	checkNative(t, tool, res.Run)
	onlyFence(t, tool, cfg)
}

// checkNative: the fence is native for the system pool, without a lease,
// and the mark active for this index pool and cluster, both of run.
func checkNative(t *testing.T, tool *Store, run string) {
	t.Helper()

	v, f, m := inputs(t, tool)
	if f.State != FenceNative || f.ToID != v.SystemID || f.Run != run || f.Values[keyToolInstance] != "" {
		t.Fatalf("fence %+v", f)
	}

	if m.State != MarkActive || m.Origin != OriginNative || m.Run != run || m.IndexID != v.IndexID || m.FSID != v.FSID {
		t.Fatalf("mark %+v", m)
	}
}

// TestInitMarkers: the completion markers are in the format the gates
// read, in heads with the shard counts of new indexes.
func TestInitMarkers(t *testing.T) {
	tool, _ := migPools(t)
	mustInit(t, tool)

	for _, mk := range []struct {
		ns, name string
		shards   int
		key      string
	}{
		{model.AdminNamespace, kvindex.MarkerCollection().Name, kvindex.MarkerCollection().Shards, kvindex.MarkerKV},
		{model.AccountsNamespace, model.AccountIndexName, model.AccountIndexShards, model.AccountBackfillMarker},
		{model.LearningNamespace, model.LearningQueue, model.DefaultQueueShards, learnindex.MarkerBackfill},
	} {
		ix, release, err := tool.openIndex(mk.ns, mk.name, mk.shards)
		if err != nil {
			t.Fatal(err)
		}

		done, err := sortedindex.Backfilled(ix, mk.key)
		h, _ := ix.Head()

		release()

		if !done || err != nil || h.Shards != mk.shards {
			t.Errorf("marker %s in %s: %v %v, %d shards", mk.key, mk.ns, done, err, h.Shards)
		}
	}
}

// TestInitTwice: a second init finds the pool initialized and writes
// nothing.
func TestInitTwice(t *testing.T) {
	tool, _ := migPools(t)
	res := mustInit(t, tool)
	_, f, m := inputs(t, tool)

	again := mustInit(t, tool)
	_, f2, m2 := inputs(t, tool)

	if !again.Already || again.Run != res.Run || f2.Version != f.Version || m2.Version != m.Version {
		t.Fatalf("second init: %+v (fence %d->%d, mark %d->%d)", again, f.Version, f2.Version, m.Version, m2.Version)
	}
}

// TestServerAfterInit: a server starts on an initialized system pool, in
// read mode from the start, without a listing.
func TestServerAfterInit(t *testing.T) {
	tool, cfg := migPools(t)
	mustInit(t, tool)

	v, _, _ := inputs(t, tool)
	before := listings(t, listAdminKV) + listings(t, learnindex.SiteAll)
	s := mustOpen(t, serverConfig(cfg))

	if state, pool := s.Placement(); state != PlacementNative || pool == nil || pool.ID != v.SystemID {
		t.Fatalf("placement %q %+v", state, pool)
	}

	kv, err := s.AdminKV()
	if err != nil {
		t.Fatal(err)
	}
	defer kv.Close()

	q, err := s.LearningQueue()
	if err != nil {
		t.Fatal(err)
	}

	if kv.IndexMode() != model.KVIndexRead || q.Mode() != model.KVIndexRead {
		t.Fatalf("modes: admin store %s, learning %s", kv.IndexMode(), q.Mode())
	}

	_, kerr := kv.List("job/", "", 10)
	_, qerr := q.List()

	if after := listings(t, listAdminKV) + listings(t, learnindex.SiteAll); kerr != nil || qerr != nil || after != before {
		t.Fatalf("%v listings in read mode (%v, %v)", after-before, kerr, qerr)
	}
}

// TestInitRefusesExistingCluster: an index pool that holds system state
// is refused (migrate it), the refusal leaves no fence, and servers
// without a system pool still start.
func TestInitRefusesExistingCluster(t *testing.T) {
	tool, cfg := migPools(t)

	putOmap(t, tool, cfg.IndexPool, model.AccountsNamespace, "ix.accounts/head", map[string]string{"shards": "256"})

	if _, err := tool.InitSystem(InitOptions{}); !errors.Is(err, ErrInitRefused) || !strings.Contains(err.Error(), "system migrate") {
		t.Fatalf("init on an existing cluster: %v", err)
	}

	if _, f, _ := inputs(t, tool); f.Exists {
		t.Fatalf("a refused init left a fence: %+v", f)
	}

	wantRefusal(t, serverConfig(cfg), "shardpost system migrate --to")

	plain := serverConfig(cfg)
	plain.SystemPool = ""
	mustOpen(t, plain)
}

// TestInitKeepsRevertedFence: a refusal over a reverted fence leaves it
// as it was (the lease's "leased" goes back).
func TestInitKeepsRevertedFence(t *testing.T) {
	tool, cfg := migPools(t)
	v, _, _ := inputs(t, tool)

	putOmap(t, tool, cfg.IndexPool, model.AccountsNamespace, "ix.accounts/head", map[string]string{"shards": "256"})
	putOmap(t, tool, cfg.IndexPool, model.AdminNamespace, FenceOID, fenceVals(v, FenceReverted))

	if _, err := tool.InitSystem(InitOptions{}); !errors.Is(err, ErrInitRefused) {
		t.Fatalf("init over a reverted fence on an existing cluster: %v", err)
	}

	if _, f, _ := inputs(t, tool); f.State != FenceReverted || f.Run != "hand" || f.Values[keyToolInstance] != "" {
		t.Fatalf("reverted fence after a refusal: %+v", f)
	}

	plain := serverConfig(cfg)
	plain.SystemPool = ""
	mustOpen(t, plain)
}

// TestInitRefusesUsedSystemPool: a system pool that holds something is
// refused, and the refusal leaves no fence.
func TestInitRefusesUsedSystemPool(t *testing.T) {
	tool, cfg := migPools(t)

	putOmap(t, tool, cfg.SystemPool, model.AdminNamespace, "kv.foreign", map[string]string{"x": "y"})

	if _, err := tool.InitSystem(InitOptions{}); !errors.Is(err, ErrInitRefused) || !strings.Contains(err.Error(), "not empty") {
		t.Fatalf("init into a used system pool: %v", err)
	}

	if _, f, _ := inputs(t, tool); f.Exists {
		t.Fatalf("a refused init left a fence: %+v", f)
	}
}

// TestInitRetiredRun: a retired run is refused without its confirmation
// and cleared with it.
func TestInitRetiredRun(t *testing.T) {
	tool, cfg := migPools(t)
	v, _, _ := inputs(t, tool)

	putOmap(t, tool, cfg.SystemPool, model.AdminNamespace, "kv.foreign", map[string]string{"x": "y"})
	putOmap(t, tool, cfg.SystemPool, model.AdminNamespace, MarkOID, markVals(v, MarkRetired))
	putOmap(t, tool, cfg.IndexPool, model.AdminNamespace, FenceOID, fenceVals(v, FenceReverted))

	if _, err := tool.InitSystem(InitOptions{}); !errors.Is(err, ErrInitRefused) || !strings.Contains(err.Error(), "retired") {
		t.Fatalf("init over a retired run without confirmation: %v", err)
	}

	// Confirmed, but nothing says the other object belongs to that run.
	if _, err := tool.InitSystem(InitOptions{Confirm: "hand"}); !errors.Is(err, ErrInitRefused) || !strings.Contains(err.Error(), "do not belong") {
		t.Fatalf("init over a retired run with foreign objects: %v", err)
	}

	// The run state of its migration says they do.
	putOmap(t, tool, cfg.SystemPool, model.AdminNamespace, runStateOID, map[string]string{pkRun: "hand"})

	res, err := tool.InitSystem(InitOptions{Confirm: "hand"})
	if err != nil || res.Cleared != 3 {
		t.Fatalf("init over a confirmed retired run: %+v %v", res, err)
	}

	if got := names(t, tool, cfg.SystemPool, model.AdminNamespace); slices.Contains(got, "kv.foreign") {
		t.Fatalf("the copies of the retired run are left: %v", got)
	}

	mustOpen(t, serverConfig(cfg))
}

// TestInitCrashResumes: an init that stops after the fence, after the
// markers or after the mark leaves the fence initializing, a server
// refuses with the hint to run it again, and a second init (which takes
// the stale lease over) completes it with the same run.
func TestInitCrashResumes(t *testing.T) {
	shortLease(t)

	for _, step := range []string{"fence", "markers", "mark"} {
		t.Run(step, func(t *testing.T) { crashAndResume(t, step) })
	}
}

func crashAndResume(t *testing.T, step string) {
	tool, cfg := migPools(t)

	crash := errors.New("crash")
	initStep = func(s string) error {
		if s == step {
			return crash
		}

		return nil
	}

	t.Cleanup(func() { initStep = func(string) error { return nil } })

	if _, err := tool.InitSystem(InitOptions{}); !errors.Is(err, crash) {
		t.Fatalf("crash: %v", err)
	}

	// The failed init gave its lease up and left the fence initializing.
	_, f, _ := inputs(t, tool)
	if f.State != FenceInitializing || f.Values[keyToolInstance] != "" {
		t.Fatalf("fence after the crash: %+v", f)
	}

	wantRefusal(t, serverConfig(cfg), "incomplete")

	initStep = func(string) error { return nil }

	res := mustInit(t, tool)
	if !res.Resumed || res.Run != f.Run {
		t.Fatalf("second init: %+v", res)
	}

	mustOpen(t, serverConfig(cfg))
}

// TestInitStaleLease: a lease left by a tool that died refuses an init
// at once, naming the holder; with Wait the init takes it over once the
// holder has been idle and completes the unfinished run.
func TestInitStaleLease(t *testing.T) {
	shortLease(t)

	tool, cfg := migPools(t)
	v, _, _ := inputs(t, tool)

	dead := fenceVals(v, FenceInitializing)
	dead[keyToolInstance], dead[keyToolEpoch], dead[keyToolSeen] = "dead-host-1", "3", time.Now().UTC().Format(time.RFC3339Nano)
	putOmap(t, tool, cfg.IndexPool, model.AdminNamespace, FenceOID, dead)

	_, err := tool.InitSystem(InitOptions{})
	if !errors.Is(err, ErrInitRefused) || !errors.Is(err, ErrFenceHeld) || !strings.Contains(err.Error(), "dead-host-1") {
		t.Fatalf("init against a held lease: %v", err)
	}

	if _, f, _ := inputs(t, tool); f.Values[keyToolInstance] != "dead-host-1" {
		t.Fatalf("a refused init touched a foreign lease: %+v", f)
	}

	res, err := tool.InitSystem(InitOptions{Wait: true})
	if err != nil || !res.Resumed || res.Run != "hand" {
		t.Fatalf("init that waits: %+v %v", res, err)
	}

	checkNative(t, tool, "hand")
}

// TestOpenReadError: a placement that cannot be read at startup refuses
// a short-lived command, while a long-running process (with the
// periodic check) starts in the state "unknown".
func TestOpenReadError(t *testing.T) {
	tool, cfg := migPools(t)
	mustInit(t, tool)

	timeout := errors.New("timed out")
	placementReadFail.Store(&timeout)

	t.Cleanup(func() { placementReadFail.Store(nil) })

	if _, err := Open(serverConfig(cfg), quiet()); !errors.Is(err, errPlacementRead) {
		t.Fatalf("a command without the periodic check: %v", err)
	}

	s, _ := watched(t, serverConfig(cfg))

	if state, _ := s.Placement(); state != PlacementUnknown {
		t.Fatalf("placement %q", state)
	}
}

// TestInitParallel: two inits at the same time leave exactly one mark;
// each ends in order (done, found done, or refused while the other one
// works).
func TestInitParallel(t *testing.T) {
	shortLease(t)

	tool, cfg := migPools(t)

	other := cfg
	other.PlacementTool = true
	second := mustOpen(t, other)

	var (
		wg   sync.WaitGroup
		errs [2]error
		runs [2]string
	)

	for i, s := range []*Store{tool, second} {
		wg.Go(func() {
			res, err := s.InitSystem(InitOptions{})
			if err == nil {
				runs[i] = res.Run
			}

			errs[i] = err
		})
	}

	wg.Wait()

	if errs[0] != nil && errs[1] != nil || slices.ContainsFunc(errs[:], func(err error) bool { return err != nil && !errors.Is(err, ErrInitRefused) }) {
		t.Fatalf("results %v", errs)
	}

	_, f, m := inputs(t, tool)
	if f.State != FenceNative || !m.Exists || slices.ContainsFunc(runs[:], func(r string) bool { return r != "" && r != m.Run }) {
		t.Fatalf("runs %v, fence %+v, mark %+v", runs, f, m)
	}
}

// TestPlacementRefusals: Open refuses every placement that does not
// match the configuration, and accepts a reverted or leased fence
// without a system pool.
func TestPlacementRefusals(t *testing.T) {
	tool, cfg := migPools(t)
	v, _, _ := inputs(t, tool)

	plain := serverConfig(cfg)
	plain.SystemPool = ""

	for _, tc := range []struct {
		fence, mark string
		cfg         Config
		want        string
	}{
		{FenceNative, "", serverConfig(cfg), "carries no mark"},
		{"", MarkActive, serverConfig(cfg), "carries an active mark"},
		{FenceDone, MarkRetired, serverConfig(cfg), "withdrawn"},
		{FenceMigrating, "", serverConfig(cfg), "under way"},
		{FenceNative, MarkActive, plain, "set ceph.system_pool"},
		{FenceInitializing, "", plain, "set ceph.system_pool"},
		{FenceReverted, "", plain, ""},
		{FenceLeased, "", plain, ""},
		{FenceDone, MarkActive, serverConfig(cfg), ""},
	} {
		setPlacement(t, tool, cfg, v, tc.fence, tc.mark)

		if tc.want != "" {
			wantRefusal(t, tc.cfg, tc.want)
			continue
		}

		s, err := Open(tc.cfg, quiet())
		if err != nil {
			t.Fatalf("fence %q, mark %q, system pool %q: %v", tc.fence, tc.mark, tc.cfg.SystemPool, err)
		}

		s.Drain(10 * time.Second)
		s.Close()
	}
}

// setPlacement empties the test pools and writes a fence and a mark of
// these states ("" none).
func setPlacement(t *testing.T, tool *Store, cfg Config, v placementView, fence, mark string) {
	t.Helper()

	emptyPool(t, tool, cfg.IndexPool)
	emptyPool(t, tool, cfg.SystemPool)

	if fence != "" {
		putOmap(t, tool, cfg.IndexPool, model.AdminNamespace, FenceOID, fenceVals(v, fence))
	}

	if mark != "" {
		putOmap(t, tool, cfg.SystemPool, model.AdminNamespace, MarkOID, markVals(v, mark))
	}
}

// TestPlacementForeign: a mark of another cluster, and a system pool
// that is the index pool, are refused.
func TestPlacementForeign(t *testing.T) {
	tool, cfg := migPools(t)
	v, _, _ := inputs(t, tool)

	setPlacement(t, tool, cfg, v, FenceNative, "")

	foreign := markVals(v, MarkActive)
	foreign[pkFSID] = "00000000-0000-0000-0000-000000000000"
	putOmap(t, tool, cfg.SystemPool, model.AdminNamespace, MarkOID, foreign)

	wantRefusal(t, serverConfig(cfg), "belongs to")

	same := serverConfig(cfg)
	same.SystemPool = cfg.IndexPool

	if _, err := Open(same, quiet()); !errors.Is(err, errPoolIDs) {
		t.Errorf("system pool = index pool: %v", err)
	}
}

// watched opens a server with a quick periodic placement check whose
// contradictions arrive on the returned channel.
func watched(t *testing.T, cfg Config) (*Store, <-chan error) {
	t.Helper()

	lost := make(chan error, 1)
	cfg.PlacementEvery = 20 * time.Millisecond
	cfg.OnPlacementLost = func(err error) { lost <- err }

	return mustOpen(t, cfg), lost
}

// noLoss waits a few checks and fails on a contradiction.
func noLoss(t *testing.T, lost <-chan error, what string) {
	t.Helper()

	time.Sleep(200 * time.Millisecond)

	select {
	case err := <-lost:
		t.Fatalf("%s ended the process: %v", what, err)
	default:
	}
}

// loss waits for a contradiction wrapping want.
func loss(t *testing.T, lost <-chan error, want error, what string) {
	t.Helper()

	select {
	case err := <-lost:
		if !errors.Is(err, want) {
			t.Fatalf("%s: lost with %v", what, err)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("%s did not end the process", what)
	}
}

// TestPlacementWatch: the periodic check ends the process only on a
// placement read positively: a read error is "unknown" and nothing
// else; a migration that begins is a contradiction. The check ends in
// Drain.
func TestPlacementWatch(t *testing.T) {
	tool, cfg := migPools(t)
	mustInit(t, tool)

	s, lost := watched(t, serverConfig(cfg))

	timeout := errors.New("timed out")
	placementReadFail.Store(&timeout)

	noLoss(t, lost, "a read error")

	if state, _ := s.Placement(); state != PlacementUnknown {
		t.Errorf("placement after a read error: %q", state)
	}

	placementReadFail.Store(nil)

	v, _, _ := inputs(t, tool)
	putOmap(t, tool, cfg.IndexPool, model.AdminNamespace, FenceOID, fenceVals(v, FenceMigrating))

	loss(t, lost, ErrSystemPlacement, "a migrating fence")

	if !s.Drain(10 * time.Second) {
		t.Fatal("not drained")
	}

	select {
	case <-s.place.done:
	default:
		t.Fatal("the placement check still runs after Drain")
	}
}

// TestPlacementWatchWithoutSystemPool: without a system pool a reverted
// fence is no contradiction, any other is; the check ends in Close.
func TestPlacementWatchWithoutSystemPool(t *testing.T) {
	tool, cfg := migPools(t)
	v, _, _ := inputs(t, tool)

	p := serverConfig(cfg)
	p.SystemPool = ""

	s, lost := watched(t, p)

	putOmap(t, tool, cfg.IndexPool, model.AdminNamespace, FenceOID, fenceVals(v, FenceReverted))
	noLoss(t, lost, "a reverted fence")

	putOmap(t, tool, cfg.IndexPool, model.AdminNamespace, FenceOID, fenceVals(v, FenceNative))
	loss(t, lost, ErrSystemMoved, "a native fence")

	s.Close()

	select {
	case <-s.place.done:
	default:
		t.Fatal("the placement check still runs after Close")
	}
}

// TestPoolRenamed: pools are compared by id, so a renamed system pool is
// still the one the mark names.
func TestPoolRenamed(t *testing.T) {
	tool, cfg := migPools(t)
	mustInit(t, tool)

	renamed := cfg.SystemPool + "-renamed"
	rename := func(from, to string) error {
		return tool.WithConn(func(conn *rados.Conn) error {
			_, info, err := conn.MonCommand([]byte(`{"prefix":"osd pool rename","srcpool":"` + from + `","destpool":"` + to + `"}`))
			if err != nil {
				return errors.New(err.Error() + ": " + info)
			}

			return nil
		})
	}

	if err := rename(cfg.SystemPool, renamed); err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() {
		if err := rename(renamed, cfg.SystemPool); err != nil {
			t.Errorf("rename back: %v", err)
		}
	})

	c := serverConfig(cfg)
	c.SystemPool = renamed

	mustOpen(t, c)
}

// systemWrites writes through a server on the system pool what the
// store keeps in its own namespaces: an admin record, a directory entry
// (an account's registry), a learning job, an upload and a reshard queue
// entry, server metadata. It returns the account and the job.
func systemWrites(t *testing.T, s *Store) (*User, string) {
	t.Helper()

	kv, err := s.AdminKV()
	if err != nil {
		t.Fatal(err)
	}
	defer kv.Close()

	if err := kv.Update("routing/x", func([]byte) ([]byte, error) { return []byte(`{}`), nil }); err != nil {
		t.Fatal(err)
	}

	u := testUser(t, s) // its registry registers it in the directory
	mustMailbox(t, u, "INBOX")

	q, err := s.LearningQueue()
	if err != nil {
		t.Fatal(err)
	}

	job := imapsieve.Job{ID: newGUID(), Owner: u.name, MailboxID: "m", Next: time.Now(), Action: "learn"}
	if err := q.Put(job, []byte("message")); err != nil {
		t.Fatal(err)
	}

	queueWrites(t, s, u.name)

	server, err := s.User(model.ServerNamespace)
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()

	val := "global"
	if err := server.SetMetadata("", "", map[string]*string{"/shared/imapsieve/script": &val}); err != nil {
		t.Fatal(err)
	}

	return u, job.ID
}

// queueWrites puts an entry into the uploads and the reshard queue.
func queueWrites(t *testing.T, s *Store, account string) {
	t.Helper()

	uq, err := s.uploadsQueue()
	if err != nil {
		t.Fatal(err)
	}

	if err := uq.Move(uploadEntryID(account, newGUID()), nil, &model.DueKey{Due: time.Now()}); err != nil {
		t.Fatal(err)
	}

	dq, _, err := s.FTSDueQueue()
	if err != nil {
		t.Fatal(err)
	}

	if err := dq.Move(account, nil, &model.DueKey{Due: time.Now(), Gen: 1}); err != nil {
		t.Fatal(err)
	}
}

// TestSystemRouting: with a system pool the admin store, the directory,
// the queues, the learning indexes and the server metadata live there;
// the index pool keeps nothing of them but the fence, and the learning
// copy stays in the data pool.
func TestSystemRouting(t *testing.T) {
	tool, cfg := migPools(t)
	mustInit(t, tool)

	s := mustOpen(t, serverConfig(cfg))
	u, job := systemWrites(t, s)

	onlyFence(t, tool, cfg)

	hasPrefix(t, tool, cfg.SystemPool, model.AdminNamespace, "kv.routing/x")
	hasPrefix(t, tool, cfg.SystemPool, model.AccountsNamespace, "ix.accounts/")
	hasPrefix(t, tool, cfg.SystemPool, model.AccountsNamespace, "ix.uploads/")
	hasPrefix(t, tool, cfg.SystemPool, model.AccountsNamespace, "ix.fts_due/")
	hasPrefix(t, tool, cfg.SystemPool, model.LearningNamespace, "ix.learning/")
	hasPrefix(t, tool, cfg.SystemPool, model.ServerNamespace, oidMetadata)
	hasPrefix(t, tool, cfg.DataPool, model.LearningNamespace, learningOID(job))

	// The registry stays an account's object of the index pool, and the
	// store's own namespaces have none.
	stat, release, err := s.RegistryStatter()
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	if _, ok, err := stat(u.name); !ok || err != nil {
		t.Errorf("registry of %s: %v %v", u.name, ok, err)
	}

	if _, _, err := stat(model.AdminNamespace); !errors.Is(err, model.ErrNamespace) {
		t.Errorf("registry of the admin namespace: %v", err)
	}
}

// TestSweepWithSystemPool: the sweep keeps its lock, state and spill in
// the index pool, reads the directory from the system pool and leaves
// the quarantine of a migration alone.
func TestSweepWithSystemPool(t *testing.T) {
	tool, cfg := migPools(t)
	mustInit(t, tool)

	s := mustOpen(t, serverConfig(cfg))

	u := testUser(t, s)
	mustMailbox(t, u, "INBOX")
	deliverN(t, u, "INBOX", 2, 1500)

	putOmap(t, tool, cfg.IndexPool, "~admin.moved/r1", "kv.old", map[string]string{"x": "y"})

	// A lock of the same name in the system pool does not stop it.
	other, err := s.holdLockIn(cfg.SystemPool, model.AccountsNamespace, sweepLockOID, "sweep", "test", time.Minute, false)
	if err != nil {
		t.Fatal(err)
	}

	// Without --repair: the data pool is shared with the other tests,
	// whose accounts have no registry in this index pool.
	rep, err := s.Sweep(context.Background(), SweepOptions{})
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}

	other.Release()

	for _, sample := range rep.Samples {
		if strings.Contains(sample, ".moved/") || strings.Contains(sample, u.name) {
			t.Errorf("sweep reported %q", sample)
		}
	}

	checkSweepPools(t, s, tool, cfg)

	if metas, err := u.ListMessagesByName("INBOX"); err != nil || len(metas) != 2 {
		t.Fatalf("the account lost messages: %d %v", len(metas), err)
	}
}

// checkSweepPools: the sweep's lock in the index pool stops a sweep; the
// quarantine is untouched; no sweep state or spill is in the system
// pool (the lock object there is the test's own).
func checkSweepPools(t *testing.T, s, tool *Store, cfg Config) {
	t.Helper()

	held, err := s.holdLockIn(cfg.IndexPool, model.AccountsNamespace, sweepLockOID, "sweep", "test", time.Minute, false)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := s.Sweep(context.Background(), SweepOptions{}); !errors.Is(err, ErrLocked) {
		t.Fatalf("sweep while the index pool lock is held: %v", err)
	}

	held.Release()

	if got := names(t, tool, cfg.IndexPool, "~admin.moved/r1"); !slices.Equal(got, []string{"kv.old"}) {
		t.Fatalf("the quarantine was swept: %v", got)
	}

	for _, oid := range names(t, tool, cfg.SystemPool, model.AccountsNamespace) {
		if strings.HasPrefix(oid, "sweep/") && oid != sweepLockOID {
			t.Errorf("sweep object in the system pool: %s", oid)
		}
	}
}

// TestRotateAndDrainSystemPool: the store's own views on the system pool
// follow a key rotation (their watches fail with ErrRotated) and are
// closed by Drain (ErrHandleClosed).
func TestRotateAndDrainSystemPool(t *testing.T) {
	tool, cfg := migPools(t)
	mustInit(t, tool)

	c := serverConfig(cfg)

	s, err := Open(c, quiet())
	if err != nil {
		t.Fatal(err)
	}

	q, err := s.LearningQueue()
	if err != nil {
		t.Fatal(err)
	}

	if pool := handlePool(s, model.LearningNamespace); pool != cfg.SystemPool {
		t.Errorf("the learning view is on %q", pool)
	}

	w, err := q.Watch()
	if err != nil {
		t.Fatal(err)
	}

	if err := s.Rotate(c.Key, ""); err != nil {
		t.Fatal(err)
	}

	select {
	case err := <-w.Errors():
		if !errors.Is(err, ErrRotated) {
			t.Fatalf("watch error %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the watch did not fail on the rotation")
	}

	w.Close()

	if !s.Drain(10 * time.Second) {
		t.Fatal("not drained")
	}

	if _, err := q.List(); !errors.Is(err, ErrHandleClosed) {
		t.Fatalf("queue after Drain: %v", err)
	}

	s.Close()
}

// handlePool is the pool of the index context of the store's handle of
// namespace ns ("" none).
func handlePool(s *Store, ns string) string {
	s.heldMu.Lock()
	defer s.heldMu.Unlock()

	for h := range s.handles {
		if h.ns == ns {
			u, done, err := h.get()
			if err != nil {
				return ""
			}

			pool, _ := u.idx.GetPoolName()

			done()

			return pool
		}
	}

	return ""
}

// TestServerMetadataCached: the server metadata is read once per TTL; a
// change through this store is seen at once, one through another store
// after the TTL.
func TestServerMetadataCached(t *testing.T) {
	a := storeWith(t, func(*Config) {})
	b := storeWith(t, func(*Config) {})

	now := time.Now()
	a.serverMeta.Now = func() time.Time { return now }

	set := func(s *Store, v string) {
		t.Helper()

		u, err := s.User(model.ServerNamespace)
		if err != nil {
			t.Fatal(err)
		}
		defer u.Close()

		if err := u.SetMetadata("", "", map[string]*string{"/shared/test-cache": &v}); err != nil {
			t.Fatal(err)
		}
	}

	get := func(want string) {
		t.Helper()

		u, err := a.User(model.ServerNamespace)
		if err != nil {
			t.Fatal(err)
		}
		defer u.Close()

		if m, err := u.GetMetadata("", ""); err != nil || m["/shared/test-cache"] != want {
			t.Fatalf("got %q (%v), want %q", m["/shared/test-cache"], err, want)
		}
	}

	// A local change is seen at once, another server's within the TTL
	// not, after it.
	set(a, "one")
	get("one")
	set(b, "two")
	get("one")

	now = now.Add(model.ServerMetadataTTL)

	get("two")
	set(a, "three")
	get("three")
}
