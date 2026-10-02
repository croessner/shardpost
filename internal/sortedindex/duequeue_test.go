package sortedindex

import (
	"fmt"
	"math/rand/v2"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"shardpost/internal/model"
)

var t0 = time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

func at(sec int) time.Time { return t0.Add(time.Duration(sec) * time.Second) }

func dueKeys(t *testing.T, q *Queue) []string {
	t.Helper()

	all, err := q.All()
	if err != nil {
		t.Fatal(err)
	}

	out := make([]string, 0, len(all))
	for _, e := range all {
		out = append(out, e.Key())
	}

	slices.Sort(out)

	return out
}

// TestQueueMoveIsOneWrite: a reschedule removes the old key and sets the
// new one in one write on the shard of the id.
func TestQueueMoveIsOneWrite(t *testing.T) {
	b := NewMemBackend()
	q := NewQueue(b, "q", 4)

	if err := q.Move("job-1", nil, &model.DueKey{Due: at(0)}); err != nil {
		t.Fatal(err)
	}

	before := b.Writes.Load()

	if err := q.Move("job-1", []model.DueKey{{Due: at(0)}}, &model.DueKey{Due: at(60), Gen: 1}); err != nil {
		t.Fatal(err)
	}

	if got := b.Writes.Load() - before; got != 1 {
		t.Fatalf("reschedule took %d writes, want 1", got)
	}

	want := []string{model.FormatDueKey("job-1", model.DueKey{Due: at(60), Gen: 1})}
	if got := dueKeys(t, q); !slices.Equal(got, want) {
		t.Fatalf("entries %v, want %v", got, want)
	}

	// A from key equal to the target is kept, not removed after the set.
	if err := q.Move("job-1", []model.DueKey{{Due: at(60), Gen: 1}}, &model.DueKey{Due: at(60), Gen: 1}); err != nil {
		t.Fatal(err)
	}

	if got := dueKeys(t, q); !slices.Equal(got, want) {
		t.Fatalf("entries after a same-key move %v, want %v", got, want)
	}

	// Removing several positions at once (the stale one and the current).
	if err := q.Move("job-1", []model.DueKey{{Due: at(0)}, {Due: at(60), Gen: 1}}, nil); err != nil {
		t.Fatal(err)
	}

	if got := dueKeys(t, q); len(got) != 0 {
		t.Fatalf("entries after removal %v", got)
	}
}

// TestQueueEntriesOfAnIdShareAShard: whatever the due time and
// generation, the entries of one id live in one shard object.
func TestQueueEntriesOfAnIdShareAShard(t *testing.T) {
	b := NewMemBackend()
	q := NewQueue(b, "q", 8)

	for i := range 20 {
		if err := q.Move("same-id", nil, &model.DueKey{Due: at(i * 1000), Gen: uint64(i)}); err != nil {
			t.Fatal(err)
		}
	}

	var shards []string

	for _, o := range b.Objects() {
		if o != HeadOID("q") {
			shards = append(shards, o)
		}
	}

	if want := ShardOID("q", model.IndexShard("same-id", 8)); len(shards) != 1 || shards[0] != want {
		t.Fatalf("shard objects %v, want only %s", shards, want)
	}
}

// TestQueueDueMergesShards: Due returns the due entries of every shard,
// earliest first, without the ones due later, bounded by limit.
func TestQueueDueMergesShards(t *testing.T) {
	q := NewQueue(NewMemBackend(), "q", 4)
	want := seedQueue(t, q, 200, at(200))

	got, err := q.Due(at(200), 1000)
	if err != nil {
		t.Fatal(err)
	}

	if len(got) != len(want) {
		t.Fatalf("%d due, want %d", len(got), len(want))
	}

	for i := range got {
		if got[i].Key() != want[i].Key() || !got[i].Due.Equal(want[i].Due) {
			t.Fatalf("entry %d: %s, want %s", i, got[i].Key(), want[i].Key())
		}
	}

	first, err := q.Due(at(200), 10)
	if err != nil {
		t.Fatal(err)
	}

	if len(first) != 10 || first[9].Key() != want[9].Key() {
		t.Fatalf("limited Due: %d entries, want the first 10", len(first))
	}

	if none, err := q.Due(at(-1), 10); err != nil || len(none) != 0 {
		t.Fatalf("nothing due before the first entry: %v %v", none, err)
	}
}

// TestQueueDueAfterPages: DueAfter from the last key of a page returns
// the next page, so paging reads every due entry once, in order.
func TestQueueDueAfterPages(t *testing.T) {
	q := NewQueue(NewMemBackend(), "q", 4)
	want := seedQueue(t, q, 200, at(200))

	var got []model.DueEntry

	for after := ""; ; {
		page, err := q.DueAfter(at(200), after, 7)
		if err != nil {
			t.Fatal(err)
		}

		got = append(got, page...)

		if len(page) < 7 {
			break
		}

		after = page[len(page)-1].Key()
	}

	if len(got) != len(want) {
		t.Fatalf("%d entries paged, want %d", len(got), len(want))
	}

	for i := range got {
		if got[i].Key() != want[i].Key() {
			t.Fatalf("entry %d: %s, want %s", i, got[i].Key(), want[i].Key())
		}
	}
}

// seedQueue writes n entries at random times and returns the ones due
// at now, in key order.
func seedQueue(t *testing.T, q *Queue, n int, now time.Time) []model.DueEntry {
	t.Helper()

	r := rand.New(rand.NewPCG(1, 2))

	var due []model.DueEntry

	for i := range n {
		e := model.DueEntry{ID: "id-" + strconv.Itoa(i), DueKey: model.DueKey{Due: at(r.IntN(2 * n)), Gen: uint64(r.IntN(3))}}
		if err := q.Move(e.ID, nil, &e.DueKey); err != nil {
			t.Fatal(err)
		}

		if !e.Due.After(now) {
			due = append(due, e)
		}
	}

	slices.SortFunc(due, func(a, b model.DueEntry) int { return strings.Compare(a.Key(), b.Key()) })

	return due
}

// TestQueueDueAtTheBoundary: an entry due exactly now is due.
func TestQueueDueAtTheBoundary(t *testing.T) {
	q := NewQueue(NewMemBackend(), "q", 2)

	if err := q.Move("a/b/c", nil, &model.DueKey{Due: at(5), Gen: 7}); err != nil {
		t.Fatal(err)
	}

	got, err := q.Due(at(5), 10)
	if err != nil || len(got) != 1 || got[0].ID != "a/b/c" || got[0].Gen != 7 || !got[0].Due.Equal(at(5)) {
		t.Fatalf("due at the boundary: %+v %v", got, err)
	}

	if got, _ := q.Due(at(5).Add(-time.Nanosecond), 10); len(got) != 0 {
		t.Fatalf("due one nanosecond early: %+v", got)
	}
}

// TestQueueShardsBounded: a queue never has more than MaxQueueShards.
func TestQueueShardsBounded(t *testing.T) {
	q := NewQueue(NewMemBackend(), "q", 100)

	if err := q.Move("x", nil, &model.DueKey{Due: at(0)}); err != nil {
		t.Fatal(err)
	}

	h, err := q.Head()
	if err != nil || h.Shards != model.MaxQueueShards {
		t.Fatalf("head %+v %v, want %d shards", h, err, model.MaxQueueShards)
	}
}

// TestQueueWatchNotify: without a Waker the queue cannot be watched.
func TestQueueWatchNotify(t *testing.T) {
	q := NewQueue(NewMemBackend(), "q", 2)

	if _, err := q.Watch(); err == nil {
		t.Fatal("watch on a backend without Waker")
	}

	q.Notify() // no-op
}

// TestListOrdered: a k-way merge over the shards pages through every id
// in order, whatever the page size.
func TestListOrdered(t *testing.T) {
	for _, shards := range []int{1, 3, 64} {
		for _, limit := range []int{1, 7, 256} {
			t.Run(fmt.Sprintf("%d/%d", shards, limit), func(t *testing.T) {
				ix := New(NewMemBackend(), "ord", shards)

				var want []string

				for i := range 300 {
					id := fmt.Sprintf("acct-%04d", (i*7919)%1000)
					if err := ix.Set(id, []byte("v")); err != nil {
						t.Fatal(err)
					}

					want = append(want, id)
				}

				slices.Sort(want)
				want = slices.Compact(want)

				var got []string

				after := ""

				for range 2000 {
					page, err := ix.ListOrdered(after, limit)
					if err != nil {
						t.Fatal(err)
					}

					if len(page) == 0 {
						break
					}

					for _, e := range page {
						got = append(got, e.ID)
						after = e.ID
					}
				}

				if !slices.Equal(got, want) {
					t.Fatalf("got %d ids, want %d (first diff around %v)", len(got), len(want), firstDiff(got, want))
				}
			})
		}
	}
}

func firstDiff(a, b []string) int {
	for i := range min(len(a), len(b)) {
		if a[i] != b[i] {
			return i
		}
	}

	return min(len(a), len(b))
}
