package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"shardpost/internal/memstore"
	"shardpost/internal/model"
)

// dueSetup is a manager with the fts_due worker over fake layouts (F7:
// the queue and lease come from the store, here memstore's counterpart)
// and registered accounts.
func dueSetup(t *testing.T, accounts ...string) (*Manager, *layoutOpener, *memstore.Store, model.DueQueue) {
	t.Helper()

	store := memstore.New()

	for _, a := range accounts {
		u, _ := store.User(a)
		if err := u.EnsureMailbox("INBOX", nil); err != nil {
			t.Fatal(err)
		}
	}

	q, lock, err := store.FTSDueQueue()
	if err != nil {
		t.Fatal(err)
	}

	o := &layoutOpener{s: store}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	m := NewManager(memstore.NewAdminKV(), &Env{Accounts: o, Directory: store, Log: log, Layout3: marked(true), FTSDue: &FTSDue{Queue: q, Lock: lock}},
		Config{Poll: 10 * time.Millisecond, LeaseTTL: 300 * time.Millisecond, ExpiryPoll: 20 * time.Millisecond}, "pod-a", Kinds(time.Hour)...)

	return m, o, store, q
}

// queueDue queues the account for generation gen, due now.
func queueDue(t *testing.T, q model.DueQueue, account, gen string, due time.Time) model.DueEntry {
	t.Helper()

	k := model.DueKey{Due: due, Gen: model.FTSDueGen(gen)}
	if err := q.Move(account, nil, &k); err != nil {
		t.Fatal(err)
	}

	return model.DueEntry{ID: account, DueKey: k}
}

// entriesOf lists the queue's entries of the account.
func entriesOf(t *testing.T, q model.DueQueue, account string) []model.DueEntry {
	t.Helper()

	all, err := q.Due(time.Now().Add(365*24*time.Hour), 1000)
	if err != nil {
		t.Fatal(err)
	}

	var out []model.DueEntry

	for _, e := range all {
		if e.ID == account {
			out = append(out, e)
		}
	}

	return out
}

func notLost() bool { return false }

// fire handles one entry as the worker does.
func fire(t *testing.T, m *Manager, e model.DueEntry) {
	t.Helper()

	if err := m.ftsDueEntry(context.Background(), e, notLost); err != nil {
		t.Fatalf("entry %+v: %v", e, err)
	}
}

// dueRecord reads the worker's record of the account.
func dueRecord(t *testing.T, m *Manager, account string) *model.FTSDueRecord {
	t.Helper()

	var rec model.FTSDueRecord

	err := m.st.get(model.FTSDueRecordKey(account), &rec)
	if errors.Is(err, ErrNotFound) {
		return nil
	}

	if err != nil {
		t.Fatal(err)
	}

	return &rec
}

// TestFTSDueStaleEntries (F1): an entry of another generation and one of
// an account without registry are removed when they fire; nothing is
// planned.
func TestFTSDueStaleEntries(t *testing.T) {
	m, o, _, q := dueSetup(t, "alice")
	o.fake("alice").gen = "g2"

	for _, e := range []model.DueEntry{queueDue(t, q, "alice", "g1", time.Now()), queueDue(t, q, "nobody", "g1", time.Now())} {
		fire(t, m, e)

		if left := entriesOf(t, q, e.ID); len(left) != 0 {
			t.Errorf("%s: entries left %+v", e.ID, left)
		}
	}

	if jobs, _, _ := m.List("", "", "", 10); len(jobs) != 0 {
		t.Errorf("jobs planned: %+v", jobs)
	}
}

// TestFTSDueLocked (F4): an account under a lock is postponed, without
// a job.
func TestFTSDueLocked(t *testing.T) {
	m, o, store, q := dueSetup(t, "alice")
	o.fake("alice").gen = "g1"

	u, _ := store.User("alice")
	if _, err := u.(model.Importer).ImportLock("importsession0123456", time.Minute); err != nil {
		t.Fatal(err)
	}

	e := queueDue(t, q, "alice", "g1", time.Now())
	fire(t, m, e)

	left := entriesOf(t, q, "alice")
	if len(left) != 1 || !left[0].Due.After(time.Now()) {
		t.Errorf("entries %+v", left)
	}

	if jobs, _, _ := m.List("", "", "", 10); len(jobs) != 0 {
		t.Errorf("jobs planned: %+v", jobs)
	}
}

// TestFTSDueLifecycle (F4): the first fire plans fts_layout due_only and
// records it; while the job runs the entry waits with a growing
// backoff; a failed job is planned again; a job that switched removes
// entry and record.
func TestFTSDueLifecycle(t *testing.T) {
	m, o, _, q := dueSetup(t, "alice")
	f := o.fake("alice")
	f.gen = "g1"

	e := queueDue(t, q, "alice", "g1", time.Now())
	fire(t, m, e)

	rec := dueRecord(t, m, "alice")
	if rec == nil || rec.Gen != "g1" {
		t.Fatalf("record %+v", rec)
	}

	j, err := m.Get(rec.JobID)
	if err != nil || j.Kind != KindFTSLayout || string(j.Params) != `{"due_only":true}` || j.Principal != ftsDuePrincipal {
		t.Fatalf("job %+v %v", j, err)
	}

	// queued, not terminal: backoff
	e = entriesOf(t, q, "alice")[0]
	fire(t, m, e)

	if r := dueRecord(t, m, "alice"); r.Waits != 1 || r.JobID != rec.JobID {
		t.Errorf("after a wait %+v", r)
	}

	// failed: planned again
	failJob(t, m, rec.JobID)
	fire(t, m, entriesOf(t, q, "alice")[0])

	again := dueRecord(t, m, "alice")
	if again.JobID == rec.JobID {
		t.Fatal("no new job after a failed one")
	}

	// succeeded with a switch: entry and record go
	f.convert = func(model.FTSLayoutRequest) (model.FTSLayoutReport, error) { return model.FTSLayoutReport{}, nil }

	runManagerNoDue(t, m)
	waitState(t, m, again.JobID, StateSucceeded)
	fire(t, m, entriesOf(t, q, "alice")[0])

	if left := entriesOf(t, q, "alice"); len(left) != 0 || dueRecord(t, m, "alice") != nil {
		t.Errorf("after the switch: entries %+v record %+v", left, dueRecord(t, m, "alice"))
	}
}

// runManagerNoDue runs a job runner of another server over the same
// store, without the fts_due worker (the tests fire entries themselves).
func runManagerNoDue(t *testing.T, m *Manager) {
	t.Helper()

	env := *m.env
	env.FTSDue = nil

	kinds := make([]Kind, 0, len(m.kinds))
	for _, k := range m.kinds {
		kinds = append(kinds, k)
	}

	runManager(t, NewManager(m.st.kv, &env, m.cfg, "pod-b", kinds...))
}

// failJob stores a failed final state for a queued job.
func failJob(t *testing.T, m *Manager, id string) {
	t.Helper()

	if _, err := m.updateJob(id, nil, func(j *Job) error {
		j.State, j.Finished = StateFailed, m.now()
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// TestFTSDueNoop (R4.6): a job that found nothing to do leaves the
// record with noop_at and moves the entry 6 hours on; a later fire
// within the 6 hours moves it again instead of planning.
func TestFTSDueNoop(t *testing.T) {
	m, o, _, q := dueSetup(t, "alice")
	o.fake("alice").gen = "g1"
	o.fake("alice").convert = func(req model.FTSLayoutRequest) (model.FTSLayoutReport, error) {
		return model.FTSLayoutReport{Noop: req.DueOnly}, nil
	}

	fire(t, m, queueDue(t, q, "alice", "g1", time.Now()))

	rec := dueRecord(t, m, "alice")

	runManagerNoDue(t, m)
	waitState(t, m, rec.JobID, StateSucceeded)

	fire(t, m, entriesOf(t, q, "alice")[0])

	r := dueRecord(t, m, "alice")
	left := entriesOf(t, q, "alice")

	if r == nil || r.NoopAt.IsZero() || len(left) != 1 || left[0].Due.Before(time.Now().Add(5*time.Hour)) {
		t.Fatalf("after the no-op: record %+v entries %+v", r, left)
	}

	// The writers queue it again (their memory expired): moved again.
	fire(t, m, queueDue(t, q, "alice", "g1", time.Now()))

	if jobs, _, _ := m.List("alice", "", "", 10); len(jobs) != 1 {
		t.Errorf("%d jobs after a no-op, want 1", len(jobs))
	}
}

// TestFTSDueDuplicates (F11): two entries of one account (another
// server's, an expired memory) plan one job; the second waits.
func TestFTSDueDuplicates(t *testing.T) {
	m, o, _, q := dueSetup(t, "alice")
	o.fake("alice").gen = "g1"

	a := queueDue(t, q, "alice", "g1", time.Now().Add(-time.Second))
	b := queueDue(t, q, "alice", "g1", time.Now())

	fire(t, m, a)
	fire(t, m, b)

	if jobs, _, _ := m.List("alice", "", "", 10); len(jobs) != 1 {
		t.Errorf("%d jobs for duplicate entries", len(jobs))
	}

	if left := entriesOf(t, q, "alice"); len(left) != 2 {
		t.Errorf("entries %+v", left)
	}
}

// TestFTSDueLeaseLost (R4.7): a worker that loses its lease between the
// submit and the record leaves the entry; the next worker plans a second
// job, which the record names and which ends as a no-op.
func TestFTSDueLeaseLost(t *testing.T) {
	m, o, _, q := dueSetup(t, "alice")
	f := o.fake("alice")
	f.gen = "g1"

	runs := 0
	f.convert = func(model.FTSLayoutRequest) (model.FTSLayoutReport, error) {
		runs++
		return model.FTSLayoutReport{Noop: runs > 1}, nil
	}

	e := queueDue(t, q, "alice", "g1", time.Now())
	calls := 0

	if err := m.ftsDueEntry(context.Background(), e, func() bool { calls++; return calls > 2 }); err != nil {
		t.Fatal(err)
	}

	if dueRecord(t, m, "alice") != nil || len(entriesOf(t, q, "alice")) != 1 {
		t.Fatal("a worker without its lease changed the record or the entry")
	}

	first, _, _ := m.List("alice", "", "", 10)

	fire(t, m, e)

	rec := dueRecord(t, m, "alice")
	jobs, _, _ := m.List("alice", "", "", 10)

	if len(first) != 1 || len(jobs) != 2 || rec == nil || rec.JobID == first[0].ID {
		t.Fatalf("jobs %d record %+v", len(jobs), rec)
	}

	runManagerNoDue(t, m)

	noops := 0

	for _, j := range jobs {
		if ftsLayoutNoop(waitState(t, m, j.ID, StateSucceeded)) {
			noops++
		}
	}

	// whichever ran second found the reshard done
	if noops != 1 {
		t.Errorf("%d of the two jobs were no-ops", noops)
	}
}

// TestFTSDueWorkerRuns (F7): the runner starts the worker, which takes
// the lease from the store and works off the queue; another server
// waits for the lease.
func TestFTSDueWorkerRuns(t *testing.T) {
	m, o, store, q := dueSetup(t, "alice")
	o.fake("alice").gen = "g1"

	queueDue(t, q, "alice", "g1", time.Now())

	_, lock, _ := store.FTSDueQueue()

	runManager(t, m)

	deadline := time.Now().Add(5 * time.Second)
	for dueRecord(t, m, "alice") == nil && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}

	if dueRecord(t, m, "alice") == nil {
		t.Fatal("the worker did not plan")
	}

	if _, err := lock(time.Minute); !errors.Is(err, model.ErrLocked) {
		t.Errorf("the lease was free while the worker ran: %v", err)
	}
}

// TestPurgeDeletesDueRecord (R4.9): the record is one of the account's
// admin store records a purge deletes.
func TestPurgeDeletesDueRecord(t *testing.T) {
	keys := model.AccountRecordKeys("odd/name@example.org")

	found := false
	for _, k := range keys {
		found = found || k == model.FTSDueRecordKey("odd/name@example.org")
	}

	if !found {
		t.Errorf("purge keys %v", keys)
	}

	b, _ := json.Marshal(model.FTSDueRecord{JobID: "x", Gen: "g"})
	if len(b) == 0 {
		t.Fatal("record")
	}
}
