package sortedindex

import (
	"errors"
	"fmt"
	"sort"
	"strconv"
	"sync"
	"testing"

	"shardpost/internal/model"
)

func newIndex(t *testing.T, shards int) (*Index, *MemBackend) {
	t.Helper()

	b := NewMemBackend()

	return New(b, "test", shards), b
}

func TestGetSetRemove(t *testing.T) {
	ix, b := newIndex(t, 8)

	if _, err := ix.Get("alice"); !errors.Is(err, model.ErrNotFound) {
		t.Fatalf("get before set: %v", err)
	}

	if len(b.Objects()) != 0 {
		t.Fatalf("a read created objects: %v", b.Objects())
	}

	if err := ix.Set("alice", []byte("1")); err != nil {
		t.Fatal(err)
	}

	if v, err := ix.Get("alice"); err != nil || string(v) != "1" {
		t.Fatalf("get: %q %v", v, err)
	}

	want := ShardOID("test", model.IndexShard("alice", 8))
	if objs := b.Objects(); len(objs) != 2 || objs[0] != want || objs[1] != HeadOID("test") {
		t.Fatalf("objects %v, want head and %s", objs, want)
	}

	if err := ix.Remove("alice"); err != nil {
		t.Fatal(err)
	}

	if _, err := ix.Get("alice"); !errors.Is(err, model.ErrNotFound) {
		t.Fatalf("get after remove: %v", err)
	}

	if err := ix.Remove("nobody"); err != nil {
		t.Fatalf("remove of an absent id: %v", err)
	}
}

func TestHeadCreatedOnceWithDefaults(t *testing.T) {
	ix, _ := newIndex(t, 16)

	h, err := ix.Head()
	if err != nil || h.Shards != 16 || h.Phase != model.IndexPhaseStable || h.Format != model.IndexFormat {
		t.Fatalf("default head %+v %v", h, err)
	}

	if err := ix.Set("a", []byte("x")); err != nil {
		t.Fatal(err)
	}

	// A second index with another default reads the stored head.
	other := New(ix.b, "test", 4)

	ForgetHeads()

	if h, err := other.Head(); err != nil || h.Shards != 16 {
		t.Fatalf("stored head %+v %v", h, err)
	}

	if v, err := other.Get("a"); err != nil || string(v) != "x" {
		t.Fatalf("entry through the stored head: %q %v", v, err)
	}
}

func TestMarkers(t *testing.T) {
	ix, _ := newIndex(t, 2)

	if err := ix.SetMarker("backfill", "done"); err != nil {
		t.Fatal(err)
	}

	h, err := ix.Head()
	if err != nil || h.Markers["backfill"] != "done" || h.Shards != 2 {
		t.Fatalf("head %+v %v", h, err)
	}
}

func TestReshardPhaseRefusesWrites(t *testing.T) {
	ix, b := newIndex(t, 2)

	ForgetHeads()

	if err := b.Write(HeadOID("test"), 0, map[string][]byte{keyShards: []byte("2"), keyPhase: []byte("copying:4")}, nil); err != nil {
		t.Fatal(err)
	}

	if err := ix.Set("a", []byte("x")); !errors.Is(err, model.ErrIndexReshard) {
		t.Fatalf("write during reshard: %v", err)
	}

	if _, err := ix.Get("a"); !errors.Is(err, model.ErrNotFound) {
		t.Fatalf("reads go on: %v", err)
	}
}

func TestNewerFormatRefused(t *testing.T) {
	ix, b := newIndex(t, 2)

	ForgetHeads()

	if err := b.Write(HeadOID("test"), 0, map[string][]byte{keyShards: []byte("2"), keyFormat: []byte("99")}, nil); err != nil {
		t.Fatal(err)
	}

	if _, err := ix.Get("a"); !errors.Is(err, model.ErrIndexFormat) {
		t.Fatalf("read of a newer format: %v", err)
	}
}

// TestHeadRefusesNewerFormat: Head (which a backfill gate reads its
// marker through) refuses a head of a newer format as the cached head
// does, so a gate never takes the markers of a head that moved away (a
// system pool migration leaves format 1000 behind). The gate stays
// closed and reports the error once.
func TestHeadRefusesNewerFormat(t *testing.T) {
	ix, b := newIndex(t, 2)

	ForgetHeads()

	if err := b.Write(HeadOID("test"), 0, map[string][]byte{keyShards: []byte("2"), keyFormat: []byte("1000"),
		keyPhase: []byte("moved:7"), keyMarker + "backfill.kv": []byte(`{"finished":"2026-10-01T00:00:00Z"}`)}, nil); err != nil {
		t.Fatal(err)
	}

	if _, err := ix.Head(); !errors.Is(err, model.ErrIndexFormat) {
		t.Fatalf("Head of a newer format: %v", err)
	}

	if done, err := Backfilled(ix, "backfill.kv"); done || !errors.Is(err, model.ErrIndexFormat) {
		t.Fatalf("backfilled %v, %v", done, err)
	}

	closed := 0

	var cerr error

	g := &Gate{Head: ix, Marker: "backfill.kv", Closed: func(err error) { closed++; cerr = err }}

	for range 3 {
		if g.Open() {
			t.Fatal("the gate opened on a head of a newer format")
		}
	}

	if closed != 1 || !errors.Is(cerr, model.ErrIndexFormat) {
		t.Fatalf("closed %d times, err %v", closed, cerr)
	}
}

func TestUpdateSemantics(t *testing.T) {
	ix, b := newIndex(t, 4)

	if err := ix.Update("a", func(cur []byte) ([]byte, error) {
		if cur != nil {
			t.Errorf("current of an absent entry: %q", cur)
		}

		return []byte("1"), nil
	}); err != nil {
		t.Fatal(err)
	}

	writes := b.Writes.Load()

	if err := ix.Update("a", func(cur []byte) ([]byte, error) { return cur, nil }); err != nil {
		t.Fatal(err)
	}

	if b.Writes.Load() != writes {
		t.Error("an unchanged value was written")
	}

	stop := errors.New("stop")
	if err := ix.Update("a", func([]byte) ([]byte, error) { return nil, stop }); !errors.Is(err, stop) {
		t.Fatalf("error from fn: %v", err)
	}

	if err := ix.Update("a", func([]byte) ([]byte, error) { return nil, nil }); err != nil {
		t.Fatal(err)
	}

	if _, err := ix.Get("a"); !errors.Is(err, model.ErrNotFound) {
		t.Fatalf("nil did not remove: %v", err)
	}
}

// TestConcurrentUpdatesDoNotLoseWrites: the update is a compare-and-swap
// on the shard object, so increments of entries in one shard all land.
func TestConcurrentUpdatesDoNotLoseWrites(t *testing.T) {
	ix, _ := newIndex(t, 1)

	var wg sync.WaitGroup

	for i := 0; i < 20; i++ {
		wg.Add(1)

		go func() {
			defer wg.Done()

			err := New(ix.b, "test", 1).Update("n", func(cur []byte) ([]byte, error) {
				n, _ := strconv.Atoi(string(cur))
				return []byte(strconv.Itoa(n + 1)), nil
			})
			if err != nil {
				t.Error(err)
			}
		}()
	}

	wg.Wait()

	if v, err := ix.Get("n"); err != nil || string(v) != "20" {
		t.Fatalf("counter %q %v", v, err)
	}
}

func TestListPagesInShardOrder(t *testing.T) {
	ix, _ := newIndex(t, 7)

	var ids []string

	for i := 0; i < 50; i++ {
		id := fmt.Sprintf("user%02d", i)
		ids = append(ids, id)

		if err := ix.Set(id, []byte(id)); err != nil {
			t.Fatal(err)
		}
	}

	for _, limit := range []int{1, 3, 10, 50, 100} {
		got := listAll(t, ix, limit)

		// (shard, id) order: within a shard sorted, shards ascending.
		for i := 1; i < len(got); i++ {
			a, b := model.IndexShard(got[i-1], 7), model.IndexShard(got[i], 7)
			if a > b || (a == b && got[i-1] >= got[i]) {
				t.Fatalf("limit %d: %s (shard %d) before %s (shard %d)", limit, got[i-1], a, got[i], b)
			}
		}

		sort.Strings(got)

		if fmt.Sprint(got) != fmt.Sprint(ids) {
			t.Fatalf("limit %d: listed %v", limit, got)
		}
	}
}

// listAll pages through the index and returns the ids in listing order.
func listAll(t *testing.T, ix *Index, limit int) []string {
	t.Helper()

	var (
		got    []string
		cursor string
	)

	for {
		page, next, err := ix.List(cursor, limit)
		if err != nil {
			t.Fatal(err)
		}

		if len(page) > limit {
			t.Fatalf("limit %d: page of %d", limit, len(page))
		}

		for _, e := range page {
			if string(e.Value) != e.ID {
				t.Fatalf("value of %s: %q", e.ID, e.Value)
			}

			got = append(got, e.ID)
		}

		if next == "" {
			return got
		}

		cursor = next
	}
}

func TestListEmptyAndBadCursor(t *testing.T) {
	ix, _ := newIndex(t, 4)

	page, next, err := ix.List("", 10)
	if err != nil || len(page) != 0 || next != "" {
		t.Fatalf("empty index: %v %q %v", page, next, err)
	}

	for _, c := range []string{"x", "x:y", "-1:a"} {
		if _, _, err := ix.List(c, 10); !errors.Is(err, model.ErrIndexCursor) {
			t.Errorf("cursor %q: %v", c, err)
		}
	}

	if page, next, err := ix.List("9:", 10); err != nil || len(page) != 0 || next != "" {
		t.Fatalf("cursor past the last shard: %v %q %v", page, next, err)
	}
}

// TestShardFunctionIsStable: the shard of an id is part of the stored
// layout; these values must never change.
func TestShardFunctionIsStable(t *testing.T) {
	for id, want := range map[string]int{"": 197} { // the FNV-1a offset basis 0x811c9dc5
		if got := model.IndexShard(id, 256); got != want {
			t.Errorf("shard of %q: %d, want %d", id, got, want)
		}
	}

	// FNV-1a 32 of "a" is 0xe40c292c.
	if got := model.IndexShard("a", 1000); got != int(uint32(0xe40c292c)%1000) {
		t.Errorf("shard of \"a\": %d", got)
	}

	if model.IndexShard("anything", 1) != 0 || model.IndexShard("anything", 0) != 0 {
		t.Error("a single shard is shard 0")
	}
}
