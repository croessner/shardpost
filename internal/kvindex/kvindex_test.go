package kvindex

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"shardpost/internal/model"
	"shardpost/internal/sortedindex"
)

// mapKV is a record layer that logs its writes and listings.
type mapKV struct {
	mu    sync.Mutex
	m     map[string][]byte
	log   *[]string
	lists int
}

func (kv *mapKV) Get(key string) ([]byte, error) {
	kv.mu.Lock()
	defer kv.mu.Unlock()

	v, ok := kv.m[key]
	if !ok {
		return nil, model.ErrNotFound
	}

	return slices.Clone(v), nil
}

func (kv *mapKV) Update(key string, fn func([]byte) ([]byte, error)) error {
	kv.mu.Lock()
	defer kv.mu.Unlock()

	next, err := fn(slices.Clone(kv.m[key]))
	if err != nil {
		return err
	}

	if next == nil {
		delete(kv.m, key)
		*kv.log = append(*kv.log, "record-delete "+key)
	} else {
		kv.m[key] = next
		*kv.log = append(*kv.log, "record-write "+key)
	}

	return nil
}

func (kv *mapKV) List(prefix, after string, limit int) ([]model.KVEntry, error) {
	kv.mu.Lock()
	defer kv.mu.Unlock()

	kv.lists++

	var keys []string

	for k := range kv.m {
		if strings.HasPrefix(k, prefix) && k > after {
			keys = append(keys, k)
		}
	}

	sort.Strings(keys)

	var out []model.KVEntry
	for _, k := range keys[:min(limit, len(keys))] {
		out = append(out, model.KVEntry{Key: k, Value: slices.Clone(kv.m[k])})
	}

	return out, nil
}

// logBackend logs the index writes.
type logBackend struct {
	*sortedindex.MemBackend
	log *[]string
	mu  *sync.Mutex
}

func (b logBackend) Write(oid string, ver uint64, set map[string][]byte, rm []string) error {
	b.mu.Lock()
	*b.log = append(*b.log, fmt.Sprintf("index-write %s set=%d rm=%d", oid, len(set), len(rm)))
	b.mu.Unlock()

	return b.MemBackend.Write(oid, ver, set, rm)
}

func newStore(t *testing.T, mode model.KVIndexMode) (*Store, *mapKV, *[]string) {
	t.Helper()

	var (
		log []string
		mu  sync.Mutex
	)

	raw := &mapKV{m: map[string][]byte{}, log: &log}

	return New(raw, Options{Mode: mode, Backend: logBackend{MemBackend: sortedindex.NewMemBackend(), log: &log, mu: &mu}}), raw, &log
}

func put(t *testing.T, s *Store, key string, v any) {
	t.Helper()

	b, _ := json.Marshal(v)

	if err := s.Update(key, func([]byte) ([]byte, error) { return b, nil }); err != nil {
		t.Fatal(err)
	}
}

func del(t *testing.T, s *Store, key string) {
	t.Helper()

	if err := s.Update(key, func([]byte) ([]byte, error) { return nil, nil }); err != nil {
		t.Fatal(err)
	}
}

// TestHintRuleOrder: the record is written before its entry, and the
// entry removed before its record.
func TestHintRuleOrder(t *testing.T) {
	s, _, log := newStore(t, model.KVIndexWrite)

	put(t, s, "job/1", map[string]any{"state": "queued", "account": "a", "kind": "k"})

	if (*log)[0] != "record-write job/1" || !strings.HasPrefix((*log)[len(*log)-1], "index-write ix.job/00 set=1") {
		t.Fatalf("create order: %v", *log)
	}

	v, err := s.Index(collections[0]).Get("1")
	if err != nil {
		t.Fatal(err)
	}

	var jv JobValue
	if json.Unmarshal(v, &jv) != nil || jv.State != "queued" || jv.Account != "a" || jv.Kind != "k" {
		t.Fatalf("index value %s", v)
	}

	*log = nil

	del(t, s, "job/1")

	if !slices.Equal(*log, []string{"index-write ix.job/00 set=0 rm=1", "record-delete job/1"}) {
		t.Fatalf("delete order: %v", *log)
	}

	if _, err := s.Index(collections[0]).Get("1"); !errors.Is(err, model.ErrNotFound) {
		t.Fatalf("entry after delete: %v", err)
	}
}

// TestHintValueOnlyOnChange: a write that does not change the index
// value does not write the index; one that does refreshes it.
func TestHintValueOnlyOnChange(t *testing.T) {
	s, _, log := newStore(t, model.KVIndexWrite)

	put(t, s, "job/1", map[string]any{"state": "running", "account": "a", "lease": map[string]any{"gen": 1}})

	*log = nil

	put(t, s, "job/1", map[string]any{"state": "running", "account": "a", "lease": map[string]any{"gen": 2}})

	if len(*log) != 1 {
		t.Fatalf("a heartbeat wrote the index: %v", *log)
	}

	fin := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	put(t, s, "job/1", map[string]any{"state": "succeeded", "account": "a", "finished": fin})

	v, _ := s.Index(collections[0]).Get("1")

	var jv JobValue
	if json.Unmarshal(v, &jv) != nil || jv.State != "succeeded" || !jv.Finished.Equal(fin) {
		t.Fatalf("index value after the end: %s", v)
	}
}

// TestHintOffWritesNothing: mode off keeps no index.
func TestHintOffWritesNothing(t *testing.T) {
	s, _, log := newStore(t, model.KVIndexOff)

	put(t, s, "job/1", map[string]any{"state": "queued"})

	for _, l := range *log {
		if strings.HasPrefix(l, "index-write") {
			t.Fatalf("mode off wrote the index: %v", *log)
		}
	}
}

// TestPushValue: verified when one subscription is, expires the latest.
func TestPushValue(t *testing.T) {
	e1 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	e2 := e1.Add(time.Hour)

	rec, _ := json.Marshal(map[string]any{"account": "a", "subs": []map[string]any{
		{"id": "1", "expires": e2, "verified": false}, {"id": "2", "expires": e1, "verified": true},
	}})

	var v PushValue
	if err := json.Unmarshal(pushValue(rec), &v); err != nil || !v.Verified || !v.Expires.Equal(e2) {
		t.Fatalf("push value %+v %v", v, err)
	}
}

// TestReadModeListsFromIndex: in read mode a listing of a collection
// comes from the hint index (in key order, also across 64 shards), equals
// the listing of the records, skips and removes an entry without record,
// and lists nothing.
func TestReadModeListsFromIndex(t *testing.T) {
	s, raw, _ := newStore(t, model.KVIndexRead)

	for i := range 150 {
		put(t, s, fmt.Sprintf("jmap/push/acct/user%03d", (i*37)%150), map[string]any{"account": "x"})
		put(t, s, fmt.Sprintf("job/%03d", i), map[string]any{"state": "queued"})
	}

	put(t, s, "plan/x", map[string]any{"kind": "k"})

	// An entry whose record is gone (a crash between the two deletes).
	if err := s.Index(collections[3]).Set("ghost", []byte("{}")); err != nil {
		t.Fatal(err)
	}

	for _, prefix := range []string{"jmap/push/acct/", "job/"} {
		want, err := raw.List(prefix, "", 1000)
		if err != nil {
			t.Fatal(err)
		}

		lists := raw.lists
		got := listPages(t, s, prefix, 40)

		if raw.lists != lists {
			t.Fatalf("%s: read mode listed the records", prefix)
		}

		if !slices.EqualFunc(got, want, func(a, b model.KVEntry) bool { return a.Key == b.Key && string(a.Value) == string(b.Value) }) {
			t.Fatalf("%s: %d records, want %d in key order", prefix, len(got), len(want))
		}
	}

	if _, err := s.Index(collections[3]).Get("ghost"); !errors.Is(err, model.ErrNotFound) {
		t.Fatalf("the entry without record stayed: %v", err)
	}

	// A prefix that is not exactly a collection lists.
	lists := raw.lists
	if _, err := s.List("job/0", "", 10); err != nil || raw.lists != lists+1 {
		t.Fatalf("sub-prefix in read mode: %v (lists %d)", err, raw.lists-lists)
	}

	if _, err := s.ListIndex("jmap/", "", 10); !errors.Is(err, model.ErrNotIndexed) {
		t.Fatalf("ListIndex of a non-collection: %v", err)
	}
}

// listPages lists a prefix page by page.
func listPages(t *testing.T, s *Store, prefix string, size int) []model.KVEntry {
	t.Helper()

	var out []model.KVEntry

	after := ""

	for {
		page, err := s.List(prefix, after, size)
		if err != nil {
			t.Fatal(err)
		}

		out = append(out, page...)

		if len(page) < size {
			return out
		}

		after = page[len(page)-1].Key
	}
}

// TestWriteModeLists: write mode keeps the listing path for readers.
func TestWriteModeLists(t *testing.T) {
	s, raw, _ := newStore(t, model.KVIndexWrite)

	put(t, s, "job/1", map[string]any{"state": "queued"})

	if _, err := s.List("job/", "", 10); err != nil || raw.lists != 1 {
		t.Fatalf("write mode did not list: %v", err)
	}
}

// TestRefreshRepairsAMissingEntry: Reindex adds the entry of a record an
// older server wrote, and removes the entry of a record that is gone.
func TestRefreshRepairsAMissingEntry(t *testing.T) {
	s, raw, _ := newStore(t, model.KVIndexWrite)

	raw.m["idem/abc"] = []byte(`{"job_id":"j1"}`)

	if err := s.Reindex("idem/abc"); err != nil {
		t.Fatal(err)
	}

	ix := s.Index(collections[2])

	v, err := ix.Get("abc")
	if err != nil || string(v) != `{"job_id":"j1"}` {
		t.Fatalf("entry %s %v", v, err)
	}

	delete(raw.m, "idem/abc")

	if err := s.Reindex("idem/abc"); err != nil {
		t.Fatal(err)
	}

	if _, err := ix.Get("abc"); !errors.Is(err, model.ErrNotFound) {
		t.Fatalf("entry of a deleted record: %v", err)
	}
}

// TestCollectionOf names the collections and ids.
func TestCollectionOf(t *testing.T) {
	for key, want := range map[string]string{"job/1": "job", "plan/2": "plan", "idem/3": "idem", "jmap/push/acct/a%40b": "jmap/push/acct"} {
		c, id, ok := CollectionOf(key)
		if !ok || c.Name != want || c.Prefix()+id != key {
			t.Errorf("%s: %v %q %v", key, c, id, ok)
		}
	}

	for _, key := range []string{"job/", "jmap/push/seen/a", "jmap/submission/a", "jmap/push/lease", "jobs"} {
		if _, _, ok := CollectionOf(key); ok {
			t.Errorf("%s is indexed", key)
		}
	}
}
