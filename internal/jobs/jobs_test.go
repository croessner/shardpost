package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"shardpost/internal/memstore"
	"shardpost/internal/model"
)

// stepKind counts to Steps, checkpointing after each step; gate (if set)
// blocks before a step until closed or the context ends.
type stepKind struct {
	name      string
	planned   bool
	exclusive bool
	steps     int
	gate      chan struct{}
	started   chan string
	sawLock   bool
	mu        sync.Mutex
}

func (k *stepKind) Name() string    { return k.name }
func (k *stepKind) Role() string    { return "repair" }
func (k *stepKind) Planned() bool   { return k.planned }
func (k *stepKind) Exclusive() bool { return k.exclusive }

func (k *stepKind) Plan(_ context.Context, _ *Env, _ model.User, account string, _ json.RawMessage) (any, string, error) {
	return map[string]any{"account": account, "steps": k.steps}, "fp", nil
}

func (k *stepKind) Run(ctx context.Context, run *Run) (any, error) {
	var cp struct{ Done int }

	run.LoadCheckpoint(&cp)

	if k.started != nil {
		k.started <- run.Job.ID
	}

	if k.exclusive {
		locked, _ := run.User.(model.Importer).ImportLocked()

		k.mu.Lock()
		k.sawLock = locked
		k.mu.Unlock()
	}

	for cp.Done < k.steps {
		if k.gate != nil {
			select {
			case <-k.gate:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}

		if err := run.Fence(); err != nil {
			return nil, err
		}

		cp.Done++

		if err := run.Checkpoint(cp); err != nil {
			return nil, err
		}

		_ = run.Progress("count", int64(cp.Done), int64(k.steps))
	}

	return map[string]int{"done": cp.Done}, nil
}

func newTestManager(kv model.AdminKV, store *memstore.Store, instance string, kinds ...Kind) *Manager {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	return NewManager(kv, &Env{Accounts: store, Log: log}, Config{Poll: 10 * time.Millisecond, LeaseTTL: 300 * time.Millisecond}, instance, kinds...)
}

// waitState polls until the job reaches one of the states.
func waitState(t *testing.T, m *Manager, id string, states ...string) *Job {
	t.Helper()

	deadline := time.Now().Add(5 * time.Second)

	for time.Now().Before(deadline) {
		j, err := m.Get(id)
		if err == nil {
			for _, s := range states {
				if j.State == s {
					return j
				}
			}
		}

		time.Sleep(5 * time.Millisecond)
	}

	j, _ := m.Get(id)
	t.Fatalf("job %s stuck in %+v", id, j)

	return nil
}

// runManager runs m until the test ends; the returned stop cancels it
// and waits until its jobs are handed over.
func runManager(t *testing.T, m *Manager) func() {
	t.Helper()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})

	go func() { m.Run(ctx); close(done) }()

	stop := func() { cancel(); <-done }
	t.Cleanup(stop)

	return stop
}

func TestJobRunsToSuccess(t *testing.T) {
	kv, store := memstore.NewAdminKV(), memstore.New()
	k := &stepKind{name: "count", steps: 3}
	m := newTestManager(kv, store, "pod-a", k)

	j, created, err := m.Submit(k, "alice", nil, "", "tester", "")
	if err != nil || !created {
		t.Fatal(err)
	}

	runManager(t, m)

	done := waitState(t, m, j.ID, StateSucceeded)
	if string(done.Result) != `{"done":3}` || done.Progress.Done != 3 || done.Lease.Owner != "pod-a" {
		t.Fatalf("%+v", done)
	}
}

func TestPlanAppliedOnceAndExpires(t *testing.T) {
	kv, store := memstore.NewAdminKV(), memstore.New()
	k := &stepKind{name: "wipe", planned: true, steps: 1}
	m := newTestManager(kv, store, "pod-a", k)
	now := time.Now()
	m.now = func() time.Time { return now }

	p, err := m.CreatePlan(context.Background(), k, "alice", nil, "tester", nil)
	if err != nil || p.Fingerprint != "fp" {
		t.Fatal(err)
	}

	if _, _, err := m.Submit(k, "alice", nil, p.ID, "tester", ""); err != nil {
		t.Fatal(err)
	}

	if _, _, err := m.Submit(k, "alice", nil, p.ID, "tester", ""); !errors.Is(err, ErrPlanUsed) {
		t.Fatalf("second apply: %v", err)
	}

	late, _ := m.CreatePlan(context.Background(), k, "alice", nil, "tester", nil)
	m.now = func() time.Time { return now.Add(2 * time.Hour) }

	if _, _, err := m.Submit(k, "alice", nil, late.ID, "tester", ""); !errors.Is(err, ErrPlanExpired) {
		t.Fatalf("expired plan: %v", err)
	}

	if _, _, err := m.Submit(k, "bob", nil, late.ID, "tester", ""); err == nil {
		t.Fatal("plan applied to another account")
	}
}

func TestIdempotencyKey(t *testing.T) {
	kv, store := memstore.NewAdminKV(), memstore.New()
	k := &stepKind{name: "count", steps: 1}
	m := newTestManager(kv, store, "pod-a", k)

	first, created, err := m.Submit(k, "alice", nil, "", "tester", "key-1")
	if err != nil || !created {
		t.Fatal(err)
	}

	again, created, err := m.Submit(k, "alice", nil, "", "tester", "key-1")
	if err != nil || created || again.ID != first.ID {
		t.Fatalf("repeat: %v %v %+v", err, created, again)
	}

	if _, _, err := m.Submit(k, "bob", nil, "", "tester", "key-1"); !errors.Is(err, ErrIdempotency) {
		t.Fatalf("other request, same key: %v", err)
	}

	// Keys are per principal.
	if _, created, err := m.Submit(k, "bob", nil, "", "other", "key-1"); err != nil || !created {
		t.Fatalf("other principal: %v", err)
	}
}

func TestCancel(t *testing.T) {
	kv, store := memstore.NewAdminKV(), memstore.New()
	k := &stepKind{name: "count", steps: 5, gate: make(chan struct{}), started: make(chan string, 1)}
	m := newTestManager(kv, store, "pod-a", k)

	queued, _, _ := m.Submit(k, "alice", nil, "", "tester", "")
	if j, err := m.Cancel(queued.ID); err != nil || j.State != StateCancelled {
		t.Fatalf("cancel queued: %v %+v", err, j)
	}

	running, _, _ := m.Submit(k, "alice", nil, "", "tester", "")
	runManager(t, m)
	<-k.started

	if _, err := m.Cancel(running.ID); err != nil {
		t.Fatal(err)
	}

	close(k.gate) // the next Fence sees the request

	if j := waitState(t, m, running.ID, StateCancelled); j.Progress.Done != 0 {
		t.Fatalf("steps after cancel: %+v", j)
	}

	if _, err := m.Cancel(queued.ID); !errors.Is(err, ErrNotCancelled) {
		t.Fatalf("cancel finished: %v", err)
	}
}

// TestTakeoverResumesFromCheckpoint: a job whose server stopped renewing
// is taken over after the lease expired, resumes from its checkpoint,
// and the old server's fence fails.
func TestTakeoverResumesFromCheckpoint(t *testing.T) {
	kv, store := memstore.NewAdminKV(), memstore.New()
	k := &stepKind{name: "count", steps: 3}
	a := newTestManager(kv, store, "pod-a", k)
	b := newTestManager(kv, store, "pod-b", k)

	j, _, _ := a.Submit(k, "alice", nil, "", "tester", "")

	// pod-a claims, does one step and dies (no heartbeat, no finish).
	claimed, gen, err := a.claim(j.ID)
	if err != nil {
		t.Fatal(err)
	}

	stale := &Run{Job: claimed, m: a, gen: gen}
	if err := stale.Checkpoint(struct{ Done int }{1}); err != nil {
		t.Fatal(err)
	}

	runManager(t, b)

	done := waitState(t, b, j.ID, StateSucceeded)
	if done.Lease.Owner != "pod-b" || done.Attempts != 2 || string(done.Result) != `{"done":3}` {
		t.Fatalf("%+v", done)
	}

	if err := stale.Fence(); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("stale fence: %v", err)
	}
}

func TestExclusiveHoldsMaintenanceLock(t *testing.T) {
	kv, store := memstore.NewAdminKV(), memstore.New()
	k := &stepKind{name: "lock", exclusive: true, steps: 1}
	m := newTestManager(kv, store, "pod-a", k)

	j, _, _ := m.Submit(k, "alice", nil, "", "tester", "")
	runManager(t, m)
	waitState(t, m, j.ID, StateSucceeded)

	u, _ := store.User("alice")
	locked, _ := u.(model.Importer).ImportLocked()

	k.mu.Lock()
	defer k.mu.Unlock()

	if !k.sawLock || locked {
		t.Fatalf("lock during run %v, after %v", k.sawLock, locked)
	}
}

// TestShutdownHandsOver: a job interrupted by shutdown is not failed but
// released and finished by another server.
func TestShutdownHandsOver(t *testing.T) {
	kv, store := memstore.NewAdminKV(), memstore.New()
	k := &stepKind{name: "count", steps: 2, gate: make(chan struct{}), started: make(chan string, 2)}
	a := newTestManager(kv, store, "pod-a", k)

	j, _, _ := a.Submit(k, "alice", nil, "", "tester", "")
	stopA := runManager(t, a)

	<-k.started
	stopA()

	if cur, _ := a.Get(j.ID); cur.State != StateRunning || !cur.Lease.Until.Before(time.Now().Add(time.Millisecond)) {
		t.Fatalf("not released: %+v", cur)
	}

	close(k.gate)

	b := newTestManager(kv, store, "pod-b", k)
	runManager(t, b)

	if done := waitState(t, b, j.ID, StateSucceeded); done.Lease.Owner != "pod-b" {
		t.Fatalf("%+v", done)
	}
}

// TestTakeoverKeepsSuccessorLock: after a takeover the old server's run
// ends without releasing the maintenance lock the successor holds.
func TestTakeoverKeepsSuccessorLock(t *testing.T) {
	kv, store := memstore.NewAdminKV(), memstore.New()
	k := &stepKind{name: "lock", exclusive: true, steps: 1, gate: make(chan struct{}), started: make(chan string, 2)}
	a := newTestManager(kv, store, "pod-a", k)
	b := newTestManager(kv, store, "pod-b", k)

	j, _, _ := a.Submit(k, "alice", nil, "", "tester", "")
	runManager(t, a)
	<-k.started

	// pod-a stalls: its lease runs out and pod-b takes over.
	if _, err := a.st.updateJob(j.ID, func(cur *Job) error { cur.Lease.Until = time.Now().Add(-time.Second); return nil }); err != nil {
		t.Fatal(err)
	}

	runManager(t, b)
	<-k.started

	// pod-a's heartbeat notices the takeover and its run ends; the lock
	// must stay with pod-b.
	time.Sleep(300 * time.Millisecond)

	u, _ := store.User("alice")
	if locked, _ := u.(model.Importer).ImportLocked(); !locked {
		t.Fatal("the old server released the successor's lock")
	}

	close(k.gate)

	if done := waitState(t, b, j.ID, StateSucceeded); done.Lease.Owner != "pod-b" {
		t.Fatalf("%+v", done)
	}

	if locked, _ := u.(model.Importer).ImportLocked(); locked {
		t.Fatal("lock not released after the job ended")
	}
}

type panicKind struct{ stepKind }

func (k *panicKind) Run(context.Context, *Run) (any, error) { panic("broken record") }

func TestPanicFailsJob(t *testing.T) {
	kv, store := memstore.NewAdminKV(), memstore.New()
	k := &panicKind{stepKind{name: "panic"}}
	m := newTestManager(kv, store, "pod-a", k)

	j, _, _ := m.Submit(k, "alice", nil, "", "tester", "")
	runManager(t, m)

	if done := waitState(t, m, j.ID, StateFailed); len(done.Errors) != 1 || done.Errors[0] != "job panicked: broken record" {
		t.Fatalf("%+v", done)
	}
}

func TestListPaging(t *testing.T) {
	k := &stepKind{name: "count", steps: 1}
	m := newTestManager(memstore.NewAdminKV(), memstore.New(), "pod-a", k)

	for _, a := range []string{"alice", "bob", "alice", "alice", "bob", "alice"} {
		if _, _, err := m.Submit(k, a, nil, "", "tester", ""); err != nil {
			t.Fatal(err)
		}
	}

	first, next, err := m.List("alice", "", "", 2)
	if err != nil || len(first) != 2 || next == "" {
		t.Fatalf("first page: %v %d %q", err, len(first), next)
	}

	// Exactly full last page: no next.
	last, next, err := m.List("alice", "", next, 2)
	if err != nil || len(last) != 2 || next != "" {
		t.Fatalf("last page: %v %d %q", err, len(last), next)
	}

	if all, next, _ := m.List("", StateQueued, "", 10); len(all) != 6 || next != "" {
		t.Fatalf("all: %d %q", len(all), next)
	}
}

func TestPlanFreedWhenJobNotStored(t *testing.T) {
	kv := memstore.NewAdminKV()
	k := &stepKind{name: "count", steps: 1, planned: true}
	m := newTestManager(kv, memstore.New(), "pod-a", k)

	p, err := m.CreatePlan(context.Background(), k, "alice", nil, "tester", nil)
	if err != nil {
		t.Fatal(err)
	}

	j := &Job{ID: "fixed", Kind: k.name, Account: "alice"}
	if err := m.st.create(prefixJob+j.ID, j); err != nil {
		t.Fatal(err)
	}

	// The job id exists already: storing fails after the plan was claimed.
	if err := m.queue(j, p.ID); err == nil {
		t.Fatal("queue succeeded")
	}

	m.unclaimPlan(p.ID, j.ID)

	if got, _ := m.GetPlan(p.ID); got.AppliedJob != "" {
		t.Fatalf("plan still claimed by %s", got.AppliedJob)
	}
}

// TestWaitsForAccountLock: an exclusive job on an account locked by an
// import waits (not failed, attempt not counted) and runs once the lock
// is gone; a cancel while waiting ends it cancelled.
func TestWaitsForAccountLock(t *testing.T) {
	kv, store := memstore.NewAdminKV(), memstore.New()
	k := &stepKind{name: "lock", exclusive: true, steps: 1}
	m := newTestManager(kv, store, "pod-a", k)

	u, _ := store.User("alice")
	imp := u.(model.Importer)

	if _, err := imp.ImportLock("import-1", time.Minute); err != nil {
		t.Fatal(err)
	}

	waiting, _, _ := m.Submit(k, "alice", nil, "", "tester", "")
	cancelled, _, _ := m.Submit(k, "alice", nil, "", "tester", "")
	runManager(t, m)

	time.Sleep(700 * time.Millisecond) // two lease periods

	if _, err := m.Cancel(cancelled.ID); err != nil {
		t.Fatal(err)
	}

	for _, id := range []string{waiting.ID, cancelled.ID} {
		if j, _ := m.Get(id); j.State == StateFailed || j.Attempts > 1 {
			t.Fatalf("waiting job: %s attempts %d %v", j.State, j.Attempts, j.Errors)
		}
	}

	if err := imp.ImportUnlock("import-1"); err != nil {
		t.Fatal(err)
	}

	waitState(t, m, waiting.ID, StateSucceeded)
	waitState(t, m, cancelled.ID, StateCancelled)
}

// dropKind drops the lock (as purge does) and runs longer than the lock
// lives without renewal.
type dropKind struct{ held bool }

func (k *dropKind) Name() string    { return "drop" }
func (k *dropKind) Role() string    { return "delete" }
func (k *dropKind) Planned() bool   { return false }
func (k *dropKind) Exclusive() bool { return true }

func (k *dropKind) Plan(context.Context, *Env, model.User, string, json.RawMessage) (any, string, error) {
	return nil, "", nil
}

func (k *dropKind) Run(ctx context.Context, run *Run) (any, error) {
	run.DropLock()

	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-time.After(time.Second): // > 2 × LeaseTTL
	}

	k.held, _ = run.User.(model.Importer).ImportLocked()

	return nil, nil
}

func TestDroppedLockStillRenewed(t *testing.T) {
	kv, store := memstore.NewAdminKV(), memstore.New()
	k := &dropKind{}
	m := newTestManager(kv, store, "pod-a", k)

	j, _, _ := m.Submit(k, "alice", nil, "", "tester", "")
	stop := runManager(t, m)
	waitState(t, m, j.ID, StateSucceeded)
	stop()

	if !k.held {
		t.Fatal("maintenance lock expired while the job ran")
	}
}
