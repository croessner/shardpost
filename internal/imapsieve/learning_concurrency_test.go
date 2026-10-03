package imapsieve

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"shardpost/internal/config"
	"shardpost/internal/extprograms"
	"shardpost/internal/model"
)

// The tests of the learning concurrency (stage B18b, §5.2 and §8.1):
// the bound, never the same job twice at once, the order per account,
// the account semaphore that does not block the pass, the lease loss
// and the shutdown grace.

// syncQueue is a testQueue safe for concurrent use.
type syncQueue struct {
	mu   sync.Mutex
	jobs map[string]Job
	raw  map[string][]byte
	// lease is what Lock hands out; lost makes it report lost.
	lost atomic.Bool
}

func newSyncQueue() *syncQueue { return &syncQueue{jobs: map[string]Job{}, raw: map[string][]byte{}} }

func (q *syncQueue) Put(j Job, b []byte) error {
	q.mu.Lock()
	defer q.mu.Unlock()

	if _, ok := q.jobs[j.ID]; !ok {
		q.jobs[j.ID] = j

		q.raw[j.ID] = append([]byte{}, b...)
	}

	return nil
}

// List returns the live jobs, oldest due first (as the real queue).
func (q *syncQueue) List() ([]Job, error) {
	q.mu.Lock()
	defer q.mu.Unlock()

	out := make([]Job, 0, len(q.jobs))
	for _, j := range q.jobs {
		out = append(out, j)
	}

	sort.Slice(out, func(i, k int) bool { return out[i].Next.Before(out[k].Next) })

	return out, nil
}

func (q *syncQueue) Read(j Job) ([]byte, error) {
	q.mu.Lock()
	defer q.mu.Unlock()

	return q.raw[j.ID], nil
}

func (q *syncQueue) Update(j Job) error {
	q.mu.Lock()
	defer q.mu.Unlock()

	q.jobs[j.ID] = j

	return nil
}

func (q *syncQueue) Delete(j Job) error {
	q.mu.Lock()
	defer q.mu.Unlock()

	delete(q.jobs, j.ID)
	delete(q.raw, j.ID)

	return nil
}

func (q *syncQueue) Lock() (model.Lease, error) { return &syncLease{q: q}, nil }
func (q *syncQueue) Ready(Job) (bool, error)    { return true, nil }
func (q *syncQueue) Cleanup(Job) error          { return nil }

// left is how many jobs are in the queue.
func (q *syncQueue) left() int {
	q.mu.Lock()
	defer q.mu.Unlock()

	return len(q.jobs)
}

type syncLease struct{ q *syncQueue }

func (l *syncLease) Lost() bool { return l.q.lost.Load() }
func (l *syncLease) Release()   {}

// slowPrograms is a learning program that takes its time, records the
// order of its calls and the most it ran at once, and can be held.
type slowPrograms struct {
	mu      sync.Mutex
	delay   time.Duration
	calls   []string // "<owner>:<name>"
	ids     map[string]int
	running int
	peak    int
	// hold, when set, blocks every run until it is closed or the context
	// ends; lost, when set, is called once a run is in progress.
	hold   chan struct{}
	inside func()
}

func newSlowPrograms(delay time.Duration) *slowPrograms {
	return &slowPrograms{delay: delay, ids: map[string]int{}}
}

func (p *slowPrograms) Run(ctx context.Context, _, name string, _ []string, input []byte, c extprograms.Context) (extprograms.Result, error) {
	p.mu.Lock()
	p.running++
	p.peak = max(p.peak, p.running)
	p.calls = append(p.calls, c.Account+":"+name)
	p.ids[string(input)]++
	inside, hold := p.inside, p.hold
	p.mu.Unlock()

	defer func() {
		p.mu.Lock()
		p.running--
		p.mu.Unlock()
	}()

	if inside != nil {
		inside()
	}

	if hold != nil {
		select {
		case <-hold:
		case <-ctx.Done():
			return extprograms.Result{}, ctx.Err()
		}
	}

	select {
	case <-time.After(p.delay):
	case <-ctx.Done():
		return extprograms.Result{}, ctx.Err()
	}

	return extprograms.Result{}, nil
}

// active is how many runs are in progress.
func (p *slowPrograms) active() int {
	p.mu.Lock()
	defer p.mu.Unlock()

	return p.running
}

func (p *slowPrograms) snapshot() (calls []string, peak int, ids map[string]int) {
	p.mu.Lock()
	defer p.mu.Unlock()

	ids = map[string]int{}
	for k, v := range p.ids {
		ids[k] = v
	}

	return append([]string(nil), p.calls...), p.peak, ids
}

// concurrentWorker is a worker with n runners over q and p.
func concurrentWorker(n int64, q Queue, p Programs) *LearningWorker {
	return NewLearningWorker(config.Learning{MaxMessageBytes: 100, MaxAttempts: 3, Concurrency: n, ShutdownSeconds: 1}, q, p)
}

// enqueueOwner enqueues a job of owner with uid and action; the input is
// the job's own name, so the programs count runs per job.
func enqueueOwner(t *testing.T, w *LearningWorker, owner string, uid uint32, action string) {
	t.Helper()

	ev := Event{Owner: owner, MailboxID: "mb", Message: model.Meta{UID: uid, ModSeq: 1}}
	if err := w.Enqueue(context.Background(), ev, action, []byte(fmt.Sprintf("%s/%d/%s", owner, uid, action))); err != nil {
		t.Fatal(err)
	}
}

// stepUntilEmpty runs passes every 20 ms until the queue is empty or
// patience is over.
func stepUntilEmpty(t *testing.T, w *LearningWorker, q *syncQueue, patience time.Duration) {
	t.Helper()

	deadline := time.Now().Add(patience)

	for q.left() > 0 {
		if time.Now().After(deadline) {
			t.Fatalf("%d jobs left after %s", q.left(), patience)
		}

		w.Step(context.Background())
		time.Sleep(20 * time.Millisecond)
	}

	waitFor(t, func() bool { jobs, owners := w.Running(); return jobs == 0 && owners == 0 })
}

// waitFor polls cond for up to five seconds.
func waitFor(t *testing.T, cond func() bool) {
	t.Helper()

	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		if cond() {
			return
		}
	}

	t.Fatal("condition not reached")
}

// TestLearningConcurrencyBound: with four runners at most four jobs run
// at once, and more than one; every job runs exactly once although the
// passes list the running jobs again.
func TestLearningConcurrencyBound(t *testing.T) {
	q := newSyncQueue()
	p := newSlowPrograms(150 * time.Millisecond)
	w := concurrentWorker(4, q, p)

	for i := range 12 {
		enqueueOwner(t, w, fmt.Sprintf("owner%d", i), 1, "spam")
	}

	stepUntilEmpty(t, w, q, 10*time.Second)

	_, peak, ids := p.snapshot()
	if peak != 4 {
		t.Errorf("peak concurrency %d, want 4", peak)
	}

	if len(ids) != 12 {
		t.Fatalf("%d jobs ran, want 12", len(ids))
	}

	for id, n := range ids {
		if n != 1 {
			t.Errorf("job %s ran %d times", id, n)
		}
	}
}

// TestLearningOwnerOrder: spam then ham of one account run in that
// order, one after the other, while another account's job runs beside
// them.
func TestLearningOwnerOrder(t *testing.T) {
	q := newSyncQueue()
	p := newSlowPrograms(100 * time.Millisecond)
	w := concurrentWorker(4, q, p)

	enqueueOwner(t, w, "alice", 1, "spam")
	time.Sleep(2 * time.Millisecond) // the due times order the jobs
	enqueueOwner(t, w, "alice", 1, "ham")
	enqueueOwner(t, w, "bob", 1, "spam")

	w.Step(context.Background())

	// The first pass started alice's spam and bob's job, not alice's ham.
	waitFor(t, func() bool { return p.active() == 2 })

	calls, peak, _ := p.snapshot()
	if peak != 2 || !slices.Contains(calls, "alice:spam") || !slices.Contains(calls, "bob:spam") || slices.Contains(calls, "alice:ham") {
		t.Fatalf("first pass: calls %v, peak %d", calls, peak)
	}

	stepUntilEmpty(t, w, q, 10*time.Second)

	calls, _, _ = p.snapshot()
	if slices.Index(calls, "alice:spam") > slices.Index(calls, "alice:ham") {
		t.Errorf("ham before spam: %v", calls)
	}
}

// TestLearningOwnerSemaphoreDoesNotBlock: an account with three jobs
// holds one runner at a time; six other accounts start at once, the
// pass ends in well under the program's time, the skipped jobs follow
// in order, and the semaphore map is empty at the end.
func TestLearningOwnerSemaphoreDoesNotBlock(t *testing.T) {
	q := newSyncQueue()
	p := newSlowPrograms(300 * time.Millisecond)
	w := concurrentWorker(8, q, p)

	for i := range 3 {
		enqueueOwner(t, w, "busy", uint32(i+1), "spam")
		time.Sleep(2 * time.Millisecond)
	}

	for i := range 6 {
		enqueueOwner(t, w, fmt.Sprintf("other%d", i), 1, "spam")
	}

	start := time.Now()

	w.Step(context.Background())

	if took := time.Since(start); took > 100*time.Millisecond {
		t.Fatalf("the pass took %s", took)
	}

	waitFor(t, func() bool { return p.active() == 7 })

	if jobs, owners := w.Running(); jobs != 7 || owners != 7 {
		t.Fatalf("running %d jobs of %d owners, want 7 and 7", jobs, owners)
	}

	stepUntilEmpty(t, w, q, 10*time.Second)

	calls, _, _ := p.snapshot()

	var busy []string

	for _, c := range calls {
		if c == "busy:spam" {
			busy = append(busy, c)
		}
	}

	if len(busy) != 3 {
		t.Fatalf("busy ran %d times: %v", len(busy), calls)
	}
}

// TestLearningLeaseLostUnderRunner: a lease lost while a job runs ends
// the job without completing it; the job stays for the next holder.
func TestLearningLeaseLostUnderRunner(t *testing.T) {
	q := newSyncQueue()
	p := newSlowPrograms(50 * time.Millisecond)
	p.inside = func() { q.lost.Store(true) }
	w := concurrentWorker(2, q, p)

	enqueueOwner(t, w, "alice", 1, "spam")
	w.Step(context.Background())

	waitFor(t, func() bool { jobs, _ := w.Running(); return jobs == 0 })

	if q.left() != 1 {
		t.Fatalf("the job was completed under a lost lease: %d left", q.left())
	}
}

// TestLearningShutdownGrace: running jobs get the shutdown grace when
// Run ends; one that outlasts it is cut off and stays in the queue.
func TestLearningShutdownGrace(t *testing.T) {
	q := newSyncQueue()
	p := newSlowPrograms(0)
	p.hold = make(chan struct{})
	w := concurrentWorker(2, q, p)

	enqueueOwner(t, w, "alice", 1, "spam")

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})

	go func() { defer close(done); w.Run(ctx) }()

	waitFor(t, func() bool { return p.active() == 1 })

	start := time.Now()

	cancel()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not end")
	}

	// The grace of a second passed before the job was cut off.
	if took := time.Since(start); took < time.Second || took > 3*time.Second {
		t.Errorf("Run ended after %s, want about the grace of a second", took)
	}

	if q.left() != 1 {
		t.Fatalf("a cut-off job left the queue: %d left", q.left())
	}
}

// pagedQueue is a syncQueue that lists in pages of size jobs (as the
// real queue does in pages of 100), with the job ID as the cursor.
type pagedQueue struct {
	*syncQueue
	size int
}

func (q *pagedQueue) ListAfter(after string) ([]Job, string, error) {
	all, _ := q.List()

	start := 0

	if after != "" {
		for i, j := range all {
			if j.ID == after {
				start = i + 1
			}
		}
	}

	end := min(start+q.size, len(all))
	page := all[start:end]

	next := ""
	if len(page) == q.size {
		next = page[len(page)-1].ID
	}

	return page, next, nil
}

// TestLearningPagesPastABusyOwner: the first page holds only jobs of one
// account; the pass reads on and starts the other accounts at once,
// instead of leaving them behind the busy account's backlog.
func TestLearningPagesPastABusyOwner(t *testing.T) {
	q := &pagedQueue{syncQueue: newSyncQueue(), size: 3}
	p := newSlowPrograms(300 * time.Millisecond)
	w := concurrentWorker(8, q, p)

	for i := range 6 {
		enqueueOwner(t, w, "busy", uint32(i+1), "spam")
		time.Sleep(2 * time.Millisecond)
	}

	for i := range 3 {
		enqueueOwner(t, w, fmt.Sprintf("other%d", i), 1, "spam")
	}

	w.Step(context.Background())

	waitFor(t, func() bool { return p.active() == 4 })

	if jobs, owners := w.Running(); jobs != 4 || owners != 4 {
		t.Fatalf("running %d jobs of %d owners, want 4 and 4", jobs, owners)
	}

	stepUntilEmpty(t, w, q.syncQueue, 15*time.Second)
}
