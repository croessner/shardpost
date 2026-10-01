package backfill

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	"shardpost/internal/imapsieve"
	"shardpost/internal/learnindex"
	"shardpost/internal/model"
	"shardpost/internal/sortedindex"
)

// legacyLearning returns a queue whose records were written by servers
// without the indexes: a due job, a dead one and a job of another owner.
func legacyLearning(t *testing.T, mode model.KVIndexMode, n int) (*learnindex.Queue, *learnindex.MemRecords) {
	t.Helper()
	sortedindex.ForgetHeads()

	rec := learnindex.NewMemRecords()
	now := time.Now()

	for i := range n {
		j := imapsieve.Job{ID: fmt.Sprintf("j%04d", i), Owner: "alice", Next: now.Add(-time.Minute), Size: 3}

		switch i % 3 {
		case 1:
			j.Dead = true
		case 2:
			j.Owner = "bob"
		}

		rec.Put(j, []byte("msg"))
	}

	return learnindex.New(rec, learnindex.Options{Mode: mode, Backend: sortedindex.NewMemBackend()}), rec
}

// indexedLike checks that every job has its queue and owner entry.
func indexedLike(t *testing.T, q *learnindex.Queue, rec *learnindex.MemRecords) {
	t.Helper()

	jobs, _ := rec.List("")

	entries, err := q.DueQueue().All()
	if err != nil {
		t.Fatal(err)
	}

	if len(entries) != len(jobs) {
		t.Fatalf("%d entries for %d jobs", len(entries), len(jobs))
	}

	for _, j := range jobs {
		at := learnindex.Position(j)
		if !slices.ContainsFunc(entries, func(e model.DueEntry) bool { return e.ID == j.ID && model.SameDueKey(e.DueKey, at) }) {
			t.Fatalf("no entry of %s", j.ID)
		}

		if _, err := q.Owners().Get(learnindex.OwnerID(j)); err != nil {
			t.Fatalf("no owner entry of %s: %v", j.ID, err)
		}
	}
}

// TestLearningBackfill: the backfill indexes every job (one listing),
// sets the marker, and a second run changes nothing.
func TestLearningBackfill(t *testing.T) {
	q, rec := legacyLearning(t, model.KVIndexWrite, 7)

	rep, err := Learning(q, LearningOptions{})
	if err != nil || rep.Jobs != 7 || rep.Finished.IsZero() {
		t.Fatalf("backfill %+v %v", rep, err)
	}

	indexedLike(t, q, rec)

	if done, ok, _ := LearningBackfilled(q); !ok || !done.Finished.Equal(rep.Finished) {
		t.Fatalf("marker %+v %v", done, ok)
	}

	if rec.Listings() != 2 { // the backfill's and indexedLike's
		t.Fatalf("listings: %d", rec.Listings())
	}

	if _, err := Learning(q, LearningOptions{}); err != nil {
		t.Fatal(err)
	}

	indexedLike(t, q, rec)
}

// TestInspectLearning: the inspection shows the marker, the queue summary
// and, for a job, its record and index entries.
func TestInspectLearning(t *testing.T) {
	q, _ := legacyLearning(t, model.KVIndexWrite, 7)

	if _, err := Learning(q, LearningOptions{}); err != nil {
		t.Fatal(err)
	}

	out, err := InspectLearning(q, []string{"j0001", "none"}, time.Now())
	if err != nil {
		t.Fatal(err)
	}

	if out.Backfill == nil || out.Queue.Entries != 7 || out.Dead != 2 || out.Queue.Due != 5 || out.Owners != 7 || len(out.Jobs) != 2 {
		t.Fatalf("inspection %+v", out)
	}

	checkInspectedJobs(t, out.Jobs)
}

// checkInspectedJobs checks the jobs TestInspectLearning asked for: a dead
// one with its entries, and one that does not exist.
func checkInspectedJobs(t *testing.T, jobs []LearningJobInspect) {
	t.Helper()

	if j := jobs[0]; j.Record == nil || !j.Record.Dead || len(j.Entries) != 1 || !j.Owner {
		t.Fatalf("inspected job %+v", j)
	}

	if j := jobs[1]; j.Record != nil || len(j.Entries) != 0 || j.Owner {
		t.Fatalf("inspected missing job %+v", j)
	}
}

// TestLearningBackfillResumes: a run continues after the position of an
// interrupted one and keeps its start time; a run whose lock is lost
// stops without a marker; off refuses.
func TestLearningBackfillResumes(t *testing.T) {
	q, rec := legacyLearning(t, model.KVIndexWrite, progressEvery+10)
	calls := 0

	_, err := Learning(q, LearningOptions{Lost: func() bool { calls++; return calls > progressEvery+3 }})
	if !errors.Is(err, ErrLockLost) {
		t.Fatalf("lost lock: %v", err)
	}

	if _, ok, _ := LearningBackfilled(q); ok {
		t.Fatal("a stopped backfill set the completion marker")
	}

	h, _ := q.DueQueue().Head()

	var pos progress
	if err := json.Unmarshal([]byte(h.Markers[learnindex.MarkerProgress]), &pos); err != nil || pos.After != fmt.Sprintf("j%04d", progressEvery-1) {
		t.Fatalf("progress %q", h.Markers[learnindex.MarkerProgress])
	}

	rep, err := Learning(q, LearningOptions{})
	if err != nil || !rep.Resumed || rep.Jobs != 10 || !rep.Started.Equal(pos.Started) {
		t.Fatalf("resumed %+v %v", rep, err)
	}

	indexedLike(t, q, rec)

	if h, _ := q.DueQueue().Head(); h.Markers[learnindex.MarkerProgress] != "" {
		t.Fatal("progress kept after completion")
	}

	off, _ := legacyLearning(t, model.KVIndexOff, 1)
	if _, err := Learning(off, LearningOptions{}); err == nil {
		t.Fatal("backfill with the index off")
	}
}

// TestLearningBackfillSweepsOrphans: the backfill removes queue entries
// whose job is gone at the dead position and when due for longer than
// the grace; a young one (a job being created) stays.
func TestLearningBackfillSweepsOrphans(t *testing.T) {
	q, rec := legacyLearning(t, model.KVIndexWrite, 3)
	now := time.Now()

	for id, due := range map[string]time.Time{"dead": learnindex.DeadDue, "old": now.Add(-model.DueQueueGrace - time.Minute), "young": now} {
		if err := q.DueQueue().Move(id, nil, &model.DueKey{Due: due, Gen: 2}); err != nil {
			t.Fatal(err)
		}
	}

	rep, err := Learning(q, LearningOptions{Now: func() time.Time { return now }})
	if err != nil || rep.Removed != 2 {
		t.Fatalf("backfill %+v %v", rep, err)
	}

	entries, _ := q.DueQueue().All()

	var ids []string

	for _, e := range entries {
		ids = append(ids, e.ID)
	}

	jobs, _ := rec.List("")
	if len(ids) != len(jobs)+1 || !slices.Contains(ids, "young") || slices.Contains(ids, "dead") || slices.Contains(ids, "old") {
		t.Fatalf("entries after the sweep: %v", ids)
	}
}
