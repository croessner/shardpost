package learnindex

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"shardpost/internal/config"
	"shardpost/internal/extprograms"
	"shardpost/internal/imapsieve"
	"shardpost/internal/model"
	"shardpost/internal/sortedindex"
)

// wakingBackend adds in-process watch/notify to the memory backend.
type wakingBackend struct {
	*sortedindex.MemBackend

	mu   sync.Mutex
	subs map[string][]chan struct{}
}

func newBackend() *wakingBackend {
	return &wakingBackend{MemBackend: sortedindex.NewMemBackend(), subs: map[string][]chan struct{}{}}
}

func (b *wakingBackend) Watch(oid string) (model.Watch, error) {
	ch := make(chan struct{}, 1)

	b.mu.Lock()
	b.subs[oid] = append(b.subs[oid], ch)
	b.mu.Unlock()

	return &memWatch{ch: ch, close: func() {
		b.mu.Lock()
		defer b.mu.Unlock()

		subs := b.subs[oid]
		for i, c := range subs {
			if c == ch {
				b.subs[oid] = append(subs[:i:i], subs[i+1:]...)
				break
			}
		}
	}}, nil
}

// watchers counts the watches on oid.
func (b *wakingBackend) watchers(oid string) int {
	b.mu.Lock()
	defer b.mu.Unlock()

	return len(b.subs[oid])
}

func (b *wakingBackend) Notify(oid string) {
	b.mu.Lock()
	defer b.mu.Unlock()

	for _, ch := range b.subs[oid] {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

// NotifyPayload and WatchPayload complete sortedindex.Waker; the
// learning queue uses neither (its notifies carry no data).
func (b *wakingBackend) NotifyPayload(oid string, _ []byte) { b.Notify(oid) }

func (b *wakingBackend) WatchPayload(string) (model.PayloadWatch, error) {
	return nil, errors.New("not watched with payload in this test")
}

type memWatch struct {
	ch    chan struct{}
	once  sync.Once
	close func()
}

func (w *memWatch) Events() <-chan struct{} { return w.ch }
func (w *memWatch) Errors() <-chan error    { return nil }
func (w *memWatch) Close()                  { w.once.Do(w.close) }

// programs records learning runs and answers with scripted exit codes.
type programs struct {
	mu    sync.Mutex
	calls []string
	exits []int
}

func (p *programs) Run(_ context.Context, _, name string, _ []string, input []byte, _ extprograms.Context) (extprograms.Result, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.calls = append(p.calls, name+":"+string(input))

	code := 0
	if len(p.exits) > 0 {
		code, p.exits = p.exits[0], p.exits[1:]
	}

	return extprograms.Result{ExitCode: code}, nil
}

func (p *programs) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()

	return len(p.calls)
}

// fixture is a queue over memory records and a memory backend.
type fixture struct {
	rec *MemRecords
	b   *wakingBackend
	q   *Queue
}

func newFixture(t *testing.T, mode model.KVIndexMode) *fixture {
	t.Helper()
	sortedindex.ForgetHeads()

	f := &fixture{rec: NewMemRecords(), b: newBackend()}
	f.q = New(f.rec, Options{Mode: mode, Backend: f.b})

	return f
}

// with returns a queue on the same records and indexes in another mode.
func (f *fixture) with(o Options) *Queue {
	o.Backend = f.b

	return New(f.rec, o)
}

func worker(q imapsieve.Queue, p *programs) *imapsieve.LearningWorker {
	return imapsieve.NewLearningWorker(config.Learning{MaxMessageBytes: 1 << 20, MaxAttempts: 3}, q, p)
}

func enqueue(t *testing.T, w *imapsieve.LearningWorker, owner string, uid uint32, action string) {
	t.Helper()

	ev := imapsieve.Event{Owner: owner, MailboxID: "mb", Message: model.Meta{UID: uid, ModSeq: 1}}
	if err := w.Enqueue(context.Background(), ev, action, []byte("message")); err != nil {
		t.Fatal(err)
	}
}

// entries returns the due queue entries.
func entries(t *testing.T, q *Queue) []model.DueEntry {
	t.Helper()

	all, err := q.DueQueue().All()
	if err != nil {
		t.Fatal(err)
	}

	return all
}

// checkIndexed checks that every record has exactly its own queue and
// owner entry and nothing else exists.
func checkIndexed(t *testing.T, f *fixture) {
	t.Helper()

	jobs, _ := f.rec.List("")
	n := f.rec.Listings()

	defer func() { f.rec.lists.Store(n - 1) }() // the check's own listing does not count

	es := entries(t, f.q)
	if len(es) != len(jobs) {
		t.Fatalf("queue entries %+v for jobs %+v", es, jobs)
	}

	for _, j := range jobs {
		at := Position(j)
		if !slices.ContainsFunc(es, func(e model.DueEntry) bool { return e.ID == j.ID && model.SameDueKey(e.DueKey, at) }) {
			t.Fatalf("job %s at %v gen %d: entries %+v", j.ID, at.Due, at.Gen, es)
		}

		if _, err := f.q.Owners().Get(OwnerID(j)); err != nil {
			t.Fatalf("owner entry of %s: %v", j.ID, err)
		}
	}

	owners, _, _ := f.q.Owners().List("", 1000)
	if len(owners) != len(jobs) {
		t.Fatalf("owner entries %+v for %d jobs", owners, len(jobs))
	}
}

// TestReadModeWorker: in read mode the worker finds due jobs through the
// queue, retries with backoff, dead-letters, and the queue and owner
// index follow every write; nothing lists the records.
func TestReadModeWorker(t *testing.T) {
	f := newFixture(t, model.KVIndexRead)
	p := &programs{exits: []int{extprograms.ExitTempFail, 0, 1}}
	w := worker(f.q, p)

	enqueue(t, w, "alice", 1, "learn-spam")
	enqueue(t, w, "alice", 1, "learn-spam") // the same event: one job
	checkIndexed(t, f)

	// Attempt 1: tempfail, the job moves to its next attempt.
	w.Step(context.Background())
	checkIndexed(t, f)

	jobs, _ := f.q.Jobs("alice")
	if len(jobs) != 1 || jobs[0].Attempts != 1 || jobs[0].Gen != 1 || !jobs[0].Next.After(time.Now()) {
		t.Fatalf("after a tempfail: %+v", jobs)
	}

	// Not due yet: no attempt.
	w.Step(context.Background())

	if p.count() != 1 {
		t.Fatalf("attempted before its time: %v", p.calls)
	}

	// Made due (an operator, a later time): delivered and removed.
	j := jobs[0]
	j.Next = time.Now().Add(-time.Second)

	if err := f.q.Update(j); err != nil {
		t.Fatal(err)
	}

	w.Step(context.Background())
	checkIndexed(t, f)

	if p.count() != 2 || len(entries(t, f.q)) != 0 {
		t.Fatalf("not delivered: calls %v, entries %+v", p.calls, entries(t, f.q))
	}

	checkDead(t, f, w, p)
}

// checkDead fails a job permanently: it is dead, at DeadDue, never due
// again and never attempted again; nothing lists.
func checkDead(t *testing.T, f *fixture, w *imapsieve.LearningWorker, p *programs) {
	t.Helper()

	enqueue(t, w, "bob", 2, "learn-ham")
	w.Step(context.Background())
	checkIndexed(t, f)

	es := entries(t, f.q)
	if len(es) != 1 || !es[0].Due.Equal(DeadDue) {
		t.Fatalf("dead job entries: %+v", es)
	}

	if due, _ := f.q.DueQueue().Due(time.Now().Add(1000*time.Hour), 10); len(due) != 0 {
		t.Fatalf("a dead job is due: %+v", due)
	}

	w.Step(context.Background())

	if p.count() != 3 {
		t.Fatalf("dead job attempted: %v", p.calls)
	}

	if n := f.rec.Listings(); n != 0 {
		t.Fatalf("read mode listed the records %d times", n)
	}
}

// TestReviveAndAdminView: the admin view reads jobs by owner and id
// without listing, and a retried dead job is due at once.
func TestReviveAndAdminView(t *testing.T) {
	f := newFixture(t, model.KVIndexRead)
	w := worker(f.q, &programs{exits: []int{1, 1}})

	enqueue(t, w, "bob", 2, "learn-ham")
	enqueue(t, w, "carol", 3, "learn-ham")
	w.Step(context.Background())

	all, err := f.q.Jobs("")
	if err != nil || len(all) != 2 {
		t.Fatalf("all jobs: %+v %v", all, err)
	}

	bob, err := f.q.Jobs("bob")
	if err != nil || len(bob) != 1 || bob[0].Owner != "bob" || !bob[0].Dead {
		t.Fatalf("bob's jobs: %+v %v", bob, err)
	}

	if j, err := f.q.Job(bob[0].ID); err != nil || j.ID != bob[0].ID {
		t.Fatalf("job by id: %+v %v", j, err)
	}

	if _, err := f.q.Job("nope"); !errors.Is(err, imapsieve.ErrNoJob) {
		t.Fatalf("missing job: %v", err)
	}

	checkRevive(t, f, bob[0].ID)
}

// checkRevive retries the dead job id: it is due at once, the worker is
// notified, and a second retry is refused; nothing lists.
func checkRevive(t *testing.T, f *fixture, id string) {
	t.Helper()

	watch, _ := f.q.Watch()

	revived, err := f.q.Revive(id)
	if err != nil || revived.Dead || revived.Retries != 1 {
		t.Fatalf("revive: %+v %v", revived, err)
	}

	select {
	case <-watch.Events():
	case <-time.After(time.Second):
		t.Fatal("revive did not notify")
	}

	checkIndexed(t, f)

	if _, err := f.q.Revive(id); !errors.Is(err, imapsieve.ErrNotDead) {
		t.Fatalf("revive of a live job: %v", err)
	}

	if due, _ := f.q.List(); len(due) != 1 || due[0].ID != id {
		t.Fatalf("revived job not due: %+v", due)
	}

	if n := f.rec.Listings(); n != 0 {
		t.Fatalf("read mode listed the records %d times", n)
	}
}

// TestPurgeOwner: the purge finds an account's jobs through the owner
// index in read mode (no listing) and by a listing otherwise.
func TestPurgeOwner(t *testing.T) {
	for _, mode := range []model.KVIndexMode{model.KVIndexRead, model.KVIndexWrite, model.KVIndexOff} {
		t.Run(string(mode), func(t *testing.T) {
			f := newFixture(t, mode)
			w := worker(f.q, &programs{})

			enqueue(t, w, "alice", 1, "a")
			enqueue(t, w, "alice", 2, "a")
			enqueue(t, w, "alice2", 3, "a") // a prefix of no other account's
			enqueue(t, w, "bob", 4, "a")

			n, err := f.q.PurgeOwner("alice")
			if err != nil || n != 2 {
				t.Fatalf("purged %d: %v", n, err)
			}

			listed := f.rec.Listings() > 0

			left, _ := f.rec.List("")
			if len(left) != 2 || slices.ContainsFunc(left, func(j imapsieve.Job) bool { return j.Owner == "alice" }) {
				t.Fatalf("left: %+v", left)
			}

			if mode != model.KVIndexOff {
				checkIndexed(t, f)
			}

			if listed != (mode != model.KVIndexRead) {
				t.Fatalf("listed: %v", listed)
			}
		})
	}
}

// TestWorkerWokenByNotify: in read mode a running worker handles a job
// enqueued after its start at once, woken by the queue notify (its poll
// is a second).
func TestWorkerWokenByNotify(t *testing.T) {
	f := newFixture(t, model.KVIndexRead)
	p := &programs{}
	w := worker(f.q, p)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})

	go func() { w.Run(ctx); close(done) }()

	waitFor(t, f.rec.Held)
	time.Sleep(50 * time.Millisecond)

	woken := wakeups(t)

	enqueue(t, w, "alice", 1, "a")
	waitFor(t, func() bool { return p.count() == 1 })

	if wakeups(t) <= woken {
		t.Fatal("handled by the poll, not woken by the notify")
	}

	cancel()
	<-done

	if f.rec.Held() {
		t.Fatal("the lease outlived the worker")
	}
}

// TestWatchOnlyWithLease (stage B18b, F5): a running worker without the
// lease does not watch the queue; the holder alone does, so an enqueue
// wakes one process. Once the holder is gone the other takes the lease
// and watches.
func TestWatchOnlyWithLease(t *testing.T) {
	f := newFixture(t, model.KVIndexRead)
	p1, p2 := &programs{}, &programs{}
	w1, w2 := worker(f.q, p1), worker(f.q, p2)
	w2.Config.LockRetrySeconds = 1
	w2.Jitter = func() float64 { return 0.1 }

	ctx1, cancel1 := context.WithCancel(context.Background())
	done1 := make(chan struct{})

	go func() { w1.Run(ctx1); close(done1) }()

	waitFor(t, f.rec.Held)
	waitFor(t, func() bool { return f.b.watchers(sortedindex.ShardOID(model.LearningQueue, 0)) == 1 })

	ctx2, cancel2 := context.WithCancel(context.Background())
	done2 := make(chan struct{})

	go func() { w2.Run(ctx2); close(done2) }()

	defer func() { cancel2(); <-done2 }()

	time.Sleep(300 * time.Millisecond)

	if n := f.b.watchers(sortedindex.ShardOID(model.LearningQueue, 0)); n != 1 {
		t.Fatalf("%d watchers with one holder, want 1 (a worker without the lease does not watch)", n)
	}

	cancel1()
	<-done1

	waitFor(t, func() bool { return f.rec.Held() && f.b.watchers(sortedindex.ShardOID(model.LearningQueue, 0)) == 1 })

	enqueue(t, w2, "alice", 1, "a")
	waitFor(t, func() bool { return p2.count() == 1 })
}

// TestWorkerLease: the worker keeps its lease between passes, a second
// worker gets nothing to do, and a lost lease (a key rotation) is taken
// again.
func TestWorkerLease(t *testing.T) {
	f := newFixture(t, model.KVIndexRead)
	p1, p2 := &programs{}, &programs{}
	w1, w2 := worker(f.q, p1), worker(f.q, p2)

	enqueue(t, w1, "alice", 1, "a")
	w1.Step(context.Background())

	if !f.rec.Held() {
		t.Fatal("the lease was not kept")
	}

	enqueue(t, w1, "alice", 2, "a")
	w2.Step(context.Background())

	if p2.count() != 0 {
		t.Fatal("a second worker ran beside the lease holder")
	}

	f.rec.Revoke() // rotation: lost, freed
	w1.Step(context.Background())

	if p1.count() != 2 || !f.rec.Held() {
		t.Fatalf("not taken again after the rotation: calls %v held %v", p1.calls, f.rec.Held())
	}

	w1.Release()

	if f.rec.Held() {
		t.Fatal("released lease still held")
	}
}

// TestStaleEntries: an entry without record is left alone within the
// grace and removed after it; an entry the record does not match (a
// server without the index rewrote the job) is moved to the record.
func TestStaleEntries(t *testing.T) {
	now := time.Now()
	clock := func() time.Time { return now }
	f := newFixture(t, model.KVIndexRead)
	q := f.with(Options{Mode: model.KVIndexRead, Now: clock})

	ghost := model.DueKey{Due: now.Add(-time.Minute)}
	if err := q.DueQueue().Move("ghost", nil, &ghost); err != nil {
		t.Fatal(err)
	}

	// An older server rewrote a job: its Gen is gone and its entry stays
	// where the index put it.
	j := imapsieve.Job{ID: "j1", Owner: "alice", Next: now.Add(-time.Second), Size: 3, Gen: 4}
	f.rec.Put(j, []byte("msg"))

	old := model.DueKey{Due: now.Add(-time.Hour), Gen: 4}
	if err := q.DueQueue().Move("j1", nil, &old); err != nil {
		t.Fatal(err)
	}

	j.Gen = 0
	f.rec.Put(j, []byte("msg"))

	due, err := q.List()
	if err != nil || len(due) != 1 || due[0].ID != "j1" {
		t.Fatalf("due: %+v %v", due, err)
	}

	es := entries(t, q)
	if len(es) != 2 || !slices.ContainsFunc(es, func(e model.DueEntry) bool { return e.ID == "j1" && model.SameDueKey(e.DueKey, Position(j)) }) {
		t.Fatalf("entries after the repair: %+v", es)
	}

	now = now.Add(model.DueQueueGrace)

	if _, err := q.List(); err != nil {
		t.Fatal(err)
	}

	if es := entries(t, q); len(es) != 1 || es[0].ID != "j1" {
		t.Fatalf("the ghost entry stayed: %+v", es)
	}
}

// TestGateWithoutBackfill: configured for read mode without the backfill
// marker the queue behaves as in write mode (listing) until a backfill
// completes; the backfill finds what servers without the index wrote.
func TestGateWithoutBackfill(t *testing.T) {
	now := time.Now()
	f := newFixture(t, model.KVIndexWrite)

	// Written by a server without the index.
	f.rec.Put(imapsieve.Job{ID: "old", Owner: "alice", Next: now.Add(-time.Minute), Size: 3}, []byte("msg"))

	var closed atomic.Int32

	q := f.with(Options{Mode: model.KVIndexRead, RequireBackfill: true, Now: func() time.Time { return now }})
	q.gate.Closed = func(error) { closed.Add(1) }

	if q.Mode() != model.KVIndexWrite || closed.Load() != 1 {
		t.Fatalf("mode %s without marker (closed logged %d)", q.Mode(), closed.Load())
	}

	if due, _ := q.List(); len(due) != 1 || f.rec.Listings() == 0 {
		t.Fatalf("not found by a listing: %+v", due)
	}

	if _, err := q.Watch(); !errors.Is(err, imapsieve.ErrNoWatch) {
		t.Fatalf("watch before the backfill: %v", err)
	}

	if err := backfill(q); err != nil {
		t.Fatal(err)
	}

	// The marker is looked for again after the recheck interval.
	if q.Mode() != model.KVIndexWrite {
		t.Fatal("rechecked before the interval")
	}

	now = now.Add(sortedindex.BackfillRecheck)

	if q.Mode() != model.KVIndexRead {
		t.Fatal("read mode not reached after the backfill")
	}

	f.rec.lists.Store(0)

	if due, _ := q.List(); len(due) != 1 || due[0].ID != "old" || f.rec.Listings() != 0 {
		t.Fatalf("after the backfill: %+v, %d listings", due, f.rec.Listings())
	}
}

// backfill fills the indexes the way backfill.Learning does (that
// package's tests cover it; this one only needs its effect).
func backfill(q *Queue) error {
	jobs, err := q.Records().List(SiteBackfill)
	if err != nil {
		return err
	}

	for _, j := range jobs {
		at := Position(j)
		if err := q.DueQueue().Move(j.ID, nil, &at); err != nil {
			return err
		}

		if err := q.Owners().Set(OwnerID(j), []byte{}); err != nil {
			return err
		}
	}

	b, _ := json.Marshal(map[string]time.Time{"finished": time.Now()})

	return q.DueQueue().SetMarker(MarkerBackfill, string(b))
}

// TestWriteModeListsAndIndexes: write mode keeps the indexes but reads
// from listings; off writes none.
func TestWriteModeListsAndIndexes(t *testing.T) {
	f := newFixture(t, model.KVIndexWrite)
	w := worker(f.q, &programs{exits: []int{1}})

	enqueue(t, w, "alice", 1, "a")
	checkIndexed(t, f)
	w.Step(context.Background())
	checkIndexed(t, f)

	if f.rec.Listings() == 0 {
		t.Fatal("write mode did not list")
	}

	if _, err := f.q.Watch(); !errors.Is(err, imapsieve.ErrNoWatch) {
		t.Fatalf("watch in write mode: %v", err)
	}

	off := newFixture(t, model.KVIndexOff)
	enqueue(t, worker(off.q, &programs{}), "alice", 1, "a")

	if objs := off.b.Objects(); len(objs) != 0 {
		t.Fatalf("off wrote %v", objs)
	}
}

// TestPutExisting: a repeated enqueue of a job that moved on leaves the
// job's own entry and no duplicate.
func TestPutExisting(t *testing.T) {
	f := newFixture(t, model.KVIndexRead)
	w := worker(f.q, &programs{exits: []int{extprograms.ExitTempFail}})

	enqueue(t, w, "alice", 1, "a")
	w.Step(context.Background()) // moved to its next attempt
	enqueue(t, w, "alice", 1, "a")
	checkIndexed(t, f)
}

// wakeups reads imap_imapsieve_learning_wakeups_total.
func wakeups(t *testing.T) float64 {
	t.Helper()

	mfs, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatal(err)
	}

	for _, mf := range mfs {
		if mf.GetName() == "imap_imapsieve_learning_wakeups_total" {
			return mf.GetMetric()[0].GetCounter().GetValue()
		}
	}

	return 0
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()

	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		if cond() {
			return
		}
	}

	t.Fatal("condition not reached")
}

// TestDeleteWithoutRecord: deleting a job whose record is gone removes
// its entries at the dead position (whatever their generation) and
// leaves those of other ids alone.
func TestDeleteWithoutRecord(t *testing.T) {
	f := newFixture(t, model.KVIndexRead)

	for _, e := range []model.DueEntry{
		{ID: "a", DueKey: model.DueKey{Due: DeadDue, Gen: 1}},
		{ID: "a", DueKey: model.DueKey{Due: DeadDue, Gen: 3}},
		{ID: "ab", DueKey: model.DueKey{Due: DeadDue, Gen: 1}},
	} {
		if err := f.q.DueQueue().Move(e.ID, nil, &e.DueKey); err != nil {
			t.Fatal(err)
		}
	}

	if err := f.q.Delete(imapsieve.Job{ID: "a"}); err != nil {
		t.Fatal(err)
	}

	if es := entries(t, f.q); len(es) != 1 || es[0].ID != "ab" {
		t.Fatalf("entries after the delete: %+v", es)
	}
}
