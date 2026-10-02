//go:build tentacle

package cephbench

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ceph/go-ceph/rados"

	"shardpost/internal/bench"
	"shardpost/internal/cephstore"
	"shardpost/internal/delivery"
	"shardpost/internal/model"
)

// Tests against the throwaway cluster of hack/ceph-test-cluster.sh (make
// test-docker). The test pools of the cluster stand in for production;
// the bench pools are created here.

const (
	benchIndex = "shardpost-bench-test-index"
	benchData  = "shardpost-bench-test-data"
	ftsShards  = 4
	testMsgs   = 12
	testAccts  = 3
	probeMsgs  = 3
	probeAccts = 2
	contention = 4
	// searchQueries covers every kind and key (bench.SearchQueries).
	searchQueries = 12
	expunges      = 2
	mboxShard     = 4
	chunkSize     = 64 << 10
	createTries   = 60
)

var poolsOnce sync.Once

func discard() *slog.Logger { return slog.New(slog.DiscardHandler) }

func benchConfig() cephstore.Config {
	return cephstore.Config{
		MonHost: os.Getenv("SHARDPOST_TEST_CEPH_MON"), User: "admin", Key: os.Getenv("SHARDPOST_TEST_CEPH_KEY"),
		IndexPool: benchIndex, DataPool: benchData,
		ChunkSize: chunkSize, FTSShards: ftsShards, ShardSize: mboxShard, PlacementTool: true,
	}
}

// benchStore creates the bench pools (once) and opens a store on them.
func benchStore(t *testing.T) (*cephstore.Store, cephstore.Config) {
	t.Helper()

	if os.Getenv("SHARDPOST_TEST_CEPH_MON") == "" {
		t.Skip("no test cluster (run through hack/ceph-test-cluster.sh, as make test-docker does)")
	}

	cfg := benchConfig()

	s, err := cephstore.Open(cfg, discard())
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { Close(s) })

	poolsOnce.Do(func() {
		if err := s.WithConn(func(conn *rados.Conn) error { return createPools(conn, benchIndex, benchData) }); err != nil {
			t.Fatalf("create bench pools: %v", err)
		}

		// Marked before any test writes an account (and with it the
		// account directory) into them.
		if err := checkPools(s, benchIndex, benchData, "", "bench-", true); err != nil {
			t.Fatalf("mark bench pools: %v", err)
		}
	})

	return s, cfg
}

// createPools creates the pools and waits until an object can be written.
func createPools(conn *rados.Conn, pools ...string) error {
	for _, pool := range pools {
		if _, err := conn.GetPoolByName(pool); err != nil {
			if err := conn.MakePool(pool); err != nil {
				return err
			}
		}

		var err error

		for range createTries {
			var ioctx *rados.IOContext
			if ioctx, err = conn.OpenIOContext(pool); err == nil {
				err = ioctx.WriteFull("probe", []byte("x"))
				if err == nil {
					err = ioctx.Delete("probe")
				}

				ioctx.Destroy()
			}

			if err == nil {
				break
			}

			time.Sleep(time.Second)
		}

		if err != nil {
			return err
		}
	}

	return nil
}

func testPrefix(t *testing.T) string {
	t.Helper()

	b := make([]byte, 4)
	_, _ = rand.Read(b)

	return "bench-" + hex.EncodeToString(b) + "-"
}

func guard(prefix string) bench.Guard {
	return bench.Guard{ProdIndex: os.Getenv("SHARDPOST_TEST_CEPH_INDEX_POOL"), ProdData: os.Getenv("SHARDPOST_TEST_CEPH_DATA_POOL"),
		BenchIndex: benchIndex, BenchData: benchData, Prefix: prefix}
}

func deliverer(s *cephstore.Store) bench.Deliverer {
	return &delivery.Service{Store: DeliveryStore{s}, Log: discard()}
}

func populateOptions(prefix string) bench.PopulateOptions {
	sizes, _ := bench.ParseSizes("60:1KiB-8KiB,30:8KiB-32KiB,10:64KiB-200KiB")

	return bench.PopulateOptions{Prefix: prefix, Accounts: testAccts, Messages: testMsgs,
		Mailboxes: bench.ParseMailboxes(bench.DefaultMailboxes), InboxShare: 0.5,
		Gen: bench.Generator{Seed: 9, Sizes: sizes}, Parallel: 2}
}

// TestGuardOnCluster: the production pools, a pool in use by a server
// and pools without marker are refused.
func TestGuardOnCluster(t *testing.T) {
	s, cfg := benchStore(t)
	prefix := testPrefix(t)
	ctx := context.Background()

	prod := cfg
	prod.IndexPool, prod.DataPool = os.Getenv("SHARDPOST_TEST_CEPH_INDEX_POOL"), os.Getenv("SHARDPOST_TEST_CEPH_DATA_POOL")

	g := guard(prefix)
	g.BenchIndex, g.BenchData = prod.IndexPool, prod.DataPool

	if _, err := Populate(ctx, g, s, prod, deliverer(s), populateOptions(prefix)); !errors.Is(err, bench.ErrGuard) {
		t.Fatalf("populate into the production pools: %v", err)
	}

	// The store must be configured for the guarded pools.
	if _, err := Populate(ctx, guard(prefix), s, prod, deliverer(s), populateOptions(prefix)); !errors.Is(err, bench.ErrGuard) {
		t.Fatalf("populate with a store on other pools: %v", err)
	}

	// Pools that carry no marker cannot be measured.
	other := guard(prefix)
	other.ProdIndex, other.ProdData = "prod-index", "prod-data"
	other.BenchIndex, other.BenchData = prod.IndexPool, prod.DataPool

	ps, err := cephstore.Open(prod, discard())
	if err != nil {
		t.Fatal(err)
	}
	defer Close(ps)

	_, err = Measure(ctx, MeasureOptions{Guard: other, Config: prod, Store: ps, TraceDir: t.TempDir()})
	if !errors.Is(err, ErrNotBenchPool) {
		t.Fatalf("measure of unmarked pools: %v", err)
	}
}

// TestRefusesPoolInUse: an index pool with an account directory and no
// marker is not taken over.
func TestRefusesPoolInUse(t *testing.T) {
	s, _ := benchStore(t)

	err := s.WithConn(func(conn *rados.Conn) error {
		return createPools(conn, "shardpost-bench-test-used")
	})
	if err != nil {
		t.Fatal(err)
	}

	err = s.WithConn(func(conn *rados.Conn) error {
		ioctx, err := openNS(conn, "shardpost-bench-test-used", model.AccountsNamespace)
		if err != nil {
			return err
		}
		defer ioctx.Destroy()

		return ioctx.WriteFull(directoryHead, []byte("x"))
	})
	if err != nil {
		t.Fatal(err)
	}

	if err := checkPools(s, "shardpost-bench-test-used", benchData, "", "bench-", true); !errors.Is(err, ErrNotBenchPool) {
		t.Fatalf("got %v, want a refusal", err)
	}
}

// TestMarkingPerPool: an empty pool is marked, a pool with any object and
// no marker is refused, and the error names the pool.
func TestMarkingPerPool(t *testing.T) {
	s, _ := benchStore(t)

	const empty, used = "shardpost-bench-test-p1", "shardpost-bench-test-p2"

	if err := s.WithConn(func(conn *rados.Conn) error { return createPools(conn, empty, used) }); err != nil {
		t.Fatal(err)
	}

	foreign(t, s, used, true)

	err := checkPools(s, empty, used, "", "bench-", true)
	if !errors.Is(err, ErrNotBenchPool) || !strings.Contains(err.Error(), used) {
		t.Fatalf("got %v, want a refusal of %s", err, used)
	}

	if !marked(t, s, empty) || marked(t, s, used) {
		t.Fatalf("markers: %s %v, %s %v", empty, marked(t, s, empty), used, marked(t, s, used))
	}

	// measure names the pool without marker.
	if err := checkPools(s, empty, used, "", "bench-", false); err == nil || !strings.Contains(err.Error(), used) {
		t.Fatalf("measure check: %v", err)
	}

	// Once the other user's object is gone, the pool is marked too.
	foreign(t, s, used, false)

	if err := checkPools(s, empty, used, "", "bench-", true); err != nil || !marked(t, s, used) {
		t.Fatalf("second populate check: %v", err)
	}
}

// TestRBDInitialisedPool: the objects "rbd pool init" leaves in a new
// pool (Rook initialises every CephBlockPool for RBD) do not make it a
// pool in use; any other object in the default namespace does.
func TestRBDInitialisedPool(t *testing.T) {
	s, _ := benchStore(t)

	const index, data, other = "shardpost-bench-test-r1", "shardpost-bench-test-r2", "shardpost-bench-test-r3"

	defaultObjects(t, s, index, "rbd_info", "rbd_trash")
	defaultObjects(t, s, data, "rbd_info", "rbd_trash")

	if err := checkPools(s, index, data, "", "bench-", true); err != nil || !marked(t, s, index) || !marked(t, s, data) {
		t.Fatalf("RBD-initialised pools: %v", err)
	}

	// Another object in the default namespace still counts.
	defaultObjects(t, s, other, "rbd_directory")

	if err := checkPools(s, index, other, "", "bench-", true); !errors.Is(err, ErrNotBenchPool) {
		t.Fatalf("got %v, want a refusal of %s", err, other)
	}
}

// defaultObjects creates a pool if needed and writes objects into its
// default namespace.
func defaultObjects(t *testing.T, s *cephstore.Store, pool string, oids ...string) {
	t.Helper()

	err := s.WithConn(func(conn *rados.Conn) error {
		if err := createPools(conn, pool); err != nil {
			return err
		}

		ioctx, err := openNS(conn, pool, "")
		if err != nil {
			return err
		}
		defer ioctx.Destroy()

		for _, oid := range oids {
			if err := ioctx.WriteFull(oid, []byte("x")); err != nil {
				return err
			}
		}

		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// foreign writes (or removes) an object of somebody else in a pool.
func foreign(t *testing.T, s *cephstore.Store, pool string, write bool) {
	t.Helper()

	err := s.WithConn(func(conn *rados.Conn) error {
		ioctx, err := openNS(conn, pool, "someone")
		if err != nil {
			return err
		}
		defer ioctx.Destroy()

		if write {
			return ioctx.WriteFull("data", []byte("x"))
		}

		return ioctx.Delete("data")
	})
	if err != nil {
		t.Fatal(err)
	}
}

// marked reports whether a pool carries the bench marker.
func marked(t *testing.T, s *cephstore.Store, pool string) bool {
	t.Helper()

	var ok bool

	err := s.WithConn(func(conn *rados.Conn) error {
		var err error

		ok, err = hasMarker(conn, pool)

		return err
	})
	if err != nil {
		t.Fatal(err)
	}

	return ok
}

// TestObjecterLogFormat checks that the client library writes the
// objecter log in the format ParseObjecterLog reads.
func TestObjecterLogFormat(t *testing.T) {
	_, cfg := benchStore(t)

	path := filepath.Join(t.TempDir(), "objecter.log")
	cfg.ClientOptions = bench.ObjecterOptions(path)

	s, err := cephstore.Open(cfg, discard())
	if err != nil {
		t.Fatal(err)
	}

	account := bench.AccountName(testPrefix(t), 0)

	u, err := s.User(account)
	if err != nil {
		t.Fatal(err)
	}

	if err := u.EnsureMailbox("INBOX", nil); err != nil {
		t.Fatal(err)
	}

	u.Close()

	var ids map[int64]string

	err = s.WithConn(func(conn *rados.Conn) error {
		var err error

		ids, err = poolIDs(conn, benchIndex, benchData)

		return err
	})
	if err != nil {
		t.Fatal(err)
	}

	Close(s)

	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()

	c, unparsed, err := bench.ParseObjecterLog(f, ids, func(ns string) bool { return ns == account })
	if err != nil || unparsed != 0 {
		t.Fatalf("%d lines of an unknown format, %v", unparsed, err)
	}

	if reg := c[benchIndex][bench.ClassRegistry]; reg == nil || reg.Writes == 0 {
		t.Errorf("no registry write counted: %+v", c[benchIndex])
	}

	if mb := c[benchIndex][bench.ClassMbox]; mb == nil || mb.Ops == 0 {
		t.Errorf("no mailbox operation counted: %+v", c[benchIndex])
	}
}

// TestPopulateAndMeasure fills a few accounts and checks that the
// inventory finds what the data model makes of them.
func TestPopulateAndMeasure(t *testing.T) {
	s, cfg := benchStore(t)
	prefix := testPrefix(t)
	ctx := context.Background()

	rep, err := Populate(ctx, guard(prefix), s, cfg, deliverer(s), populateOptions(prefix))
	if err != nil {
		t.Fatal(err)
	}

	if rep.Deliveries != testAccts*testMsgs || rep.Failures != 0 {
		t.Fatalf("populate %+v", rep)
	}

	// A re-run finds the accounts complete.
	if rep, err = Populate(ctx, guard(prefix), s, cfg, deliverer(s), populateOptions(prefix)); err != nil || rep.AccountsComplete != testAccts {
		t.Fatalf("re-run %+v %v", rep, err)
	}

	m, err := Measure(ctx, MeasureOptions{Guard: guard(prefix), Config: cfg, Store: s, NewDeliverer: deliverer,
		Gen: populateOptions(prefix).Gen, Every: 1, Parallel: 4, ProbeAccounts: probeAccts, ProbeMessages: probeMsgs,
		Contention: contention, Search: searchQueries, Expunge: expunges,
		EC: bench.ECEstimate{DataChunks: 2, CodingChunks: 1, StripeUnit: 4096, MinAlloc: 4096}, Extrapolate: 1000,
		TraceDir: t.TempDir(), Log: discard()})
	if err != nil {
		t.Fatal(err)
	}

	checkInventory(t, m)
	checkProbe(t, m)
	checkContention(t, m)
	checkOpens(t, m)
	checkSearch(t, m)
	checkExpunge(t, m)

	checkSearchStable(t, s, cfg, prefix, m.Search.Fingerprint)

	var out strings.Builder
	if err := bench.WriteMeasureTable(&out, m); err != nil {
		t.Fatal(err)
	}

	t.Log("\n" + out.String())
}

// checkSearchStable runs the search probe alone twice more: the
// accounts changed after the first search (deliveries, expunges), so
// the second finds something else, and the third, on unchanged
// accounts, exactly what the second found.
func checkSearchStable(t *testing.T, s *cephstore.Store, cfg cephstore.Config, prefix, first string) {
	t.Helper()

	search := func() string {
		m, err := Measure(context.Background(), MeasureOptions{Guard: guard(prefix), Config: cfg, Store: s, Every: 1,
			Parallel: 4, ProbeAccounts: probeAccts, Search: searchQueries, TraceDir: t.TempDir(), Log: discard()})
		if err != nil || m.Search == nil || m.Search.Mismatch {
			t.Fatalf("search probe %+v: %v", m, err)
		}

		return m.Search.Fingerprint
	}

	second, third := search(), search()
	if second == first || third != second {
		t.Errorf("search fingerprints %s, %s, %s: want the second to differ from the first and the third to equal it",
			first, second, third)
	}
}

// exactly checks that every account has n objects of a class.
func exactly(t *testing.T, m *bench.MeasureReport, pool, class string, n int64) {
	t.Helper()

	c := m.Inventory.PerAccount[pool][class].Objects
	if c.Min != n || c.Max != n {
		t.Errorf("%s %s: %+v objects per account, want %d", pool, class, c, n)
	}
}

// within checks that every account has lo to hi objects of a class.
func within(t *testing.T, m *bench.MeasureReport, pool, class string, lo, hi int64) {
	t.Helper()

	c := m.Inventory.PerAccount[pool][class].Objects
	if c.Min < lo || c.Max > hi {
		t.Errorf("%s %s: %+v objects per account, want %d to %d", pool, class, c, lo, hi)
	}
}

func checkInventory(t *testing.T, m *bench.MeasureReport) {
	t.Helper()

	inv := m.Inventory
	if inv.Accounts != testAccts || inv.MessagesPerAccount.Min != testMsgs || inv.MessagesPerAccount.Max != testMsgs {
		t.Fatalf("inventory of %d accounts, messages %+v", inv.Accounts, inv.MessagesPerAccount)
	}

	exactly(t, m, benchIndex, bench.ClassRegistry, 1)
	exactly(t, m, benchIndex, bench.ClassAccount, 1)
	exactly(t, m, benchIndex, bench.ClassFTSMap, 1)
	exactly(t, m, benchIndex, bench.ClassMbox, 6)
	// Every message has some terms in every one of the few posting
	// shards; the header store is sharded by GUID.
	exactly(t, m, benchIndex, bench.ClassFTS, ftsShards)
	within(t, m, benchIndex, bench.ClassFTSH, 1, ftsShards)
	within(t, m, benchIndex, bench.ClassFTST, 50, 256) // 256 dictionary shards
	// One chunk per message below the chunk size, more above.
	within(t, m, benchData, bench.ClassMsg, testMsgs, 4*testMsgs)

	if m.DataRaw.Raw <= m.DataRaw.Logical || m.Extrapolated == nil || m.Extrapolated.Accounts != 1000 {
		t.Errorf("raw estimate %+v, extrapolation %+v", m.DataRaw, m.Extrapolated)
	}

	if m.StoreWide[benchIndex][bench.ClassStoreIx] == nil {
		t.Errorf("the account directory is missing from the store-wide objects: %+v", m.StoreWide)
	}

	t.Logf("index objects per account %+v, data %+v", inv.PerAccount[benchIndex][bench.ClassTotal].Objects,
		inv.PerAccount[benchData][bench.ClassTotal].Objects)
}

func checkProbe(t *testing.T, m *bench.MeasureReport) {
	t.Helper()

	d := m.Delivery
	if d == nil || d.Deliveries != probeAccts*probeMsgs || d.Latency.Count != probeAccts*probeMsgs {
		t.Fatalf("delivery probe %+v", d)
	}

	if w := d.Ops[benchData][bench.ClassMsg].Writes; w < 1 {
		t.Errorf("blob writes per delivery %v", w)
	}

	if w := d.Ops[benchIndex][bench.ClassFTS].Writes; w < 1 || w > ftsShards {
		t.Errorf("posting shard writes per delivery %v", w)
	}

	if d.Ops[benchIndex][bench.ClassRegistry].Ops < 1 || d.Ops[benchIndex][bench.ClassMbox].Ops < 1 {
		t.Errorf("registry or mailbox untouched: %+v", d.Ops[benchIndex])
	}

	// A posting shard costs one read, the compare-and-swap's own, and
	// the id map one compare-and-swap; a delivery into an index with
	// ids does not look up whether the index is fresh (the CAS reads
	// "n").
	if o := d.Ops[benchIndex][bench.ClassFTS]; o.Reads > o.Writes {
		t.Errorf("posting shard reads per delivery %v above the writes %v", o.Reads, o.Writes)
	}

	if o := d.Ops[benchIndex][bench.ClassFTSMap]; o.Reads > 1 || o.Writes > 1 {
		t.Errorf("id map ops per delivery %+v, want at most one read and one write", o)
	}

	t.Logf("ops per delivery %+v", d.Total)
}

// checkContention: the deliveries of the contention rounds all arrive,
// and their retries are counted by object class.
func checkContention(t *testing.T, m *bench.MeasureReport) {
	t.Helper()

	d := m.Contention
	if d == nil || d.Parallel != contention || d.Deliveries != probeAccts*contention*probeMsgs || d.PerSecond <= 0 {
		t.Fatalf("contention probe %+v", d)
	}

	for class := range d.CASRetries.ByClass {
		if class == "" {
			t.Errorf("CAS retries without a class: %+v", d.CASRetries)
		}
	}

	t.Logf("contention: %.1f deliveries/s, CAS retries %+v, ops per delivery %+v", d.PerSecond, d.CASRetries, d.Total)
}

// checkSearch: every query ran in every probe account, frequent words
// are found, and the two rounds agree.
func checkSearch(t *testing.T, m *bench.MeasureReport) {
	t.Helper()

	r := m.Search
	if r == nil || r.Count != probeAccts*searchQueries || len(r.Queries) != searchQueries || r.Mismatch || r.Total.Reads <= 0 {
		t.Fatalf("search probe %+v", r)
	}

	found := 0

	for _, q := range r.Queries {
		if q.Words > 0 || q.Candidates > 0 {
			found++
		}
	}

	if found == 0 {
		t.Errorf("no query found anything: %+v", r.Queries)
	}

	t.Logf("search: %d of %d queries found messages, ops per query %+v, fingerprint %s", found, len(r.Queries), r.Total, r.Fingerprint)
}

// checkExpunge: the expunges happened and wrote to the posting shards.
func checkExpunge(t *testing.T, m *bench.MeasureReport) {
	t.Helper()

	r := m.Expunge
	if r == nil || r.Count != probeAccts*expunges {
		t.Fatalf("expunge probe %+v", r)
	}

	if w := r.Ops[benchIndex][bench.ClassFTS].Writes; w < 1 {
		t.Errorf("posting shard writes per expunge %v", w)
	}

	t.Logf("expunge: ops per expunge %+v", r.Total)
}

func checkOpens(t *testing.T, m *bench.MeasureReport) {
	t.Helper()

	if m.UserOpen == nil || m.UserOpen.Count != probeAccts || m.SessionOpen == nil || m.SessionOpen.Total.Ops < 1 {
		t.Fatalf("open rounds %+v %+v", m.UserOpen, m.SessionOpen)
	}

	// Store.User() only opens the I/O contexts; it talks to no OSD.
	if m.UserOpen.Total.Ops != 0 {
		t.Errorf("Store.User() costs %+v operations", m.UserOpen.Total)
	}
}
