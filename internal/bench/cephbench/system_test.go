//go:build tentacle

package cephbench

import (
	"context"
	"errors"
	"testing"

	"github.com/ceph/go-ceph/rados"

	"shardpost/internal/bench"
	"shardpost/internal/cephstore"
	"shardpost/internal/model"
)

// The bench with a system pool of its own (--system-pool).

// systemBench creates pools of the given names and opens a bench store
// on them (with system pool when system is given).
func systemBench(t *testing.T, index, data, system string) (*cephstore.Store, cephstore.Config) {
	t.Helper()

	base, _ := benchStore(t)

	pools := []string{index, data}
	if system != "" {
		pools = append(pools, system)
	}

	if err := base.WithConn(func(conn *rados.Conn) error { return createPools(conn, pools...) }); err != nil {
		t.Fatal(err)
	}

	cfg := benchConfig()
	cfg.IndexPool, cfg.DataPool, cfg.SystemPool = index, data, system

	s, err := cephstore.Open(cfg, discard())
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { Close(s) })

	return s, cfg
}

func systemGuard(prefix string, cfg cephstore.Config) bench.Guard {
	g := guard(prefix)
	g.BenchIndex, g.BenchData, g.BenchSystem = cfg.IndexPool, cfg.DataPool, cfg.SystemPool

	return g
}

// writeFence writes a placement fence into a pool, as a migrated
// production index pool has one.
func writeFence(t *testing.T, s *cephstore.Store, pool string) {
	t.Helper()

	err := s.WithConn(func(conn *rados.Conn) error {
		ioctx, err := openNS(conn, pool, model.AdminNamespace)
		if err != nil {
			return err
		}
		defer ioctx.Destroy()

		return ioctx.SetOmap(cephstore.FenceOID, map[string][]byte{"state": []byte(cephstore.FenceDone), "to_id": []byte("99")}) // a production fence
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestBenchSystemPool: fresh bench pools with a system pool are marked,
// the system pool is initialized with the same routine as "shardpost
// system init" (a second populate finds it initialized), the placement
// check passes and populate fills the accounts.
func TestBenchSystemPool(t *testing.T) {
	s, cfg := systemBench(t, "shardpost-bench-test-s-index", "shardpost-bench-test-s-data", "shardpost-bench-test-s-system")
	prefix := testPrefix(t)
	ctx := context.Background()

	rep, err := Populate(ctx, systemGuard(prefix, cfg), s, cfg, deliverer(s), populateOptions(prefix))
	if err != nil || rep.Deliveries != testAccts*testMsgs {
		t.Fatalf("populate: %+v %v", rep, err)
	}

	checkSystemBench(t, s, cfg, prefix)

	// Again: the init finds the pool initialized.
	if _, err := Populate(ctx, systemGuard(prefix, cfg), s, cfg, deliverer(s), populateOptions(prefix)); err != nil {
		t.Fatalf("second populate: %v", err)
	}

	m, err := Measure(ctx, MeasureOptions{Guard: systemGuard(prefix, cfg), Config: cfg, Store: s, Every: 1, Parallel: 2,
		TraceDir: t.TempDir(), Log: discard()})
	if err != nil || m.SystemPool != cfg.SystemPool || m.StoreWide[cfg.SystemPool] == nil {
		t.Fatalf("measure: %+v %v", m, err)
	}
}

// checkSystemBench: the three pools are marked, the placement is the
// initialized system pool, and the account directory is there.
func checkSystemBench(t *testing.T, s *cephstore.Store, cfg cephstore.Config, prefix string) {
	t.Helper()

	for _, pool := range []string{cfg.IndexPool, cfg.DataPool, cfg.SystemPool} {
		if !marked(t, s, pool) {
			t.Errorf("%s carries no bench marker", pool)
		}
	}

	if err := s.CheckPlacement(); err != nil {
		t.Fatal(err)
	}

	if state, _ := s.Placement(); state != cephstore.PlacementNative {
		t.Fatalf("placement %q", state)
	}

	if ok, err := s.HasRegistry(bench.AccountName(prefix, 0)); !ok || err != nil {
		t.Fatalf("registry: %v %v", ok, err)
	}

	err := s.WithConn(func(conn *rados.Conn) error {
		in, err := exists(conn, cfg.SystemPool, model.AccountsNamespace, directoryHead)
		if err == nil && !in {
			err = errors.New("no account directory in the system pool")
		}

		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestBenchRefusesFencedIndex: an index pool with a placement fence is a
// production index pool: without the bench marker checkPools refuses it,
// and with the marker but without --system-pool the placement check
// does.
func TestBenchRefusesFencedIndex(t *testing.T) {
	s, cfg := systemBench(t, "shardpost-bench-test-f-index", "shardpost-bench-test-f-data", "")
	prefix := testPrefix(t)

	writeFence(t, s, cfg.IndexPool)

	if err := checkPools(s, cfg.IndexPool, cfg.DataPool, "", prefix, true); !errors.Is(err, ErrNotBenchPool) {
		t.Fatalf("fenced pool without marker: %v", err)
	}

	err := s.WithConn(func(conn *rados.Conn) error { return writeMarkers(conn, prefix, cfg.IndexPool, cfg.DataPool) })
	if err != nil {
		t.Fatal(err)
	}

	if _, err := Populate(context.Background(), systemGuard(prefix, cfg), s, cfg, deliverer(s), populateOptions(prefix)); !errors.Is(err, ErrNotBenchPool) ||
		!errors.Is(err, cephstore.ErrSystemPlacement) {
		t.Fatalf("populate on a fenced bench index pool: %v", err)
	}
}

// TestBenchStoreIsATool: the bench refuses a store that checks the
// placement itself (it sets its pools up first).
func TestBenchStoreIsATool(t *testing.T) {
	_, cfg := benchStore(t)
	prefix := testPrefix(t)

	cfg.PlacementTool = false

	if err := checkConfig(guard(prefix), cfg); !errors.Is(err, bench.ErrGuard) {
		t.Fatalf("a store outside tool mode: %v", err)
	}
}
