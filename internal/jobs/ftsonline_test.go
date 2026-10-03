package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"shardpost/internal/memstore"
	"shardpost/internal/model"
	"shardpost/internal/window"
)

// Tests of the online runs and the reshard window (stage B14i, temp/14
// §8 jobs).

// onlineMarked is a store whose layout 3 mark is set and whose mark of
// online runs is the value.
type onlineMarked bool

func (m onlineMarked) FTSLayout3Enabled() (bool, error) { return true, nil }
func (m onlineMarked) FTSOnlineEnabled() (bool, error)  { return bool(m), nil }

// onlineManager is layoutManager with the mark of online runs (on) and
// fts.online_build (auto).
func onlineManager(t *testing.T, on, auto bool) (*Manager, *layoutOpener) {
	t.Helper()

	m, o := layoutManager(t)
	m.env.FTSOnline = onlineMarked(on)
	m.cfg.FTSBuild.OnlineAuto = auto

	return m, o
}

// planParams plans the kind for the account and returns the plan's
// parameters.
func planParams(t *testing.T, m *Manager, kind, account, params string) (string, error) {
	t.Helper()

	k, _ := m.Kind(kind)

	p, err := m.CreatePlan(context.Background(), k, account, json.RawMessage(params), "tester", nil)
	if err != nil {
		return "", err
	}

	return string(p.Params), nil
}

// TestFTSOnlineNormalize: the plan decides "online" and records it:
// without the mark offline (true refused with 409), with the mark and
// fts.online_build auto online, off offline unless asked; layout 2
// targets and layout 2 accounts run offline (asked: 400); the options of
// an online rebuild need an online run.
func TestFTSOnlineNormalize(t *testing.T) {
	for _, c := range []struct {
		on, auto     bool
		kind, params string
		want         string
		err          error
	}{
		{false, true, KindFTSLayout, `{}`, `{"online":false}`, nil},
		{false, true, KindFTSLayout, `{"online":true}`, "", model.ErrFTSOnlineMark},
		{true, true, KindFTSLayout, `{"due_only":true}`, `{"due_only":true,"online":true}`, nil},
		{true, false, KindFTSLayout, `{}`, `{"online":false}`, nil},
		{true, false, KindFTSLayout, `{"online":true}`, `{"online":true}`, nil},
		{true, true, KindFTSLayout, `{"layout":2}`, `{"layout":2,"online":false}`, nil},
		{true, true, KindFTSLayout, `{"layout":2,"online":true}`, "", ErrInvalidParams},
		{true, true, KindFTSRebuild, `{}`, `{"online":true}`, nil},
		{false, true, KindFTSRebuild, `{}`, `{"online":false}`, nil},
		{true, true, KindFTSRebuild, `{"skip_unreadable":true,"drop_orphan_ids":false}`, `{"online":true,"skip_unreadable":true,"drop_orphan_ids":false}`, nil},
		{false, false, KindFTSRebuild, `{"skip_unreadable":true}`, "", ErrInvalidParams},
		{true, true, KindFTSRebuild, `{"online":false,"skip_unreadable":true}`, "", ErrInvalidParams},
	} {
		m, _ := onlineManager(t, c.on, c.auto)

		got, err := planParams(t, m, c.kind, "alice", c.params)
		if !errors.Is(err, c.err) || (c.err == nil && got != c.want) {
			t.Errorf("mark %v auto %v %s %s: %s %v, want %s %v", c.on, c.auto, c.kind, c.params, got, err, c.want, c.err)
		}
	}

	// an account with a layout 2 index (memstore without a fake layout)
	m, _ := onlineManager(t, true, true)
	m.env.Accounts = memstore.New()

	if got, err := planParams(t, m, KindFTSRebuild, "bob", `{}`); err != nil || got != `{"online":false}` {
		t.Errorf("layout 2, auto: %s %v", got, err)
	}

	if _, err := planParams(t, m, KindFTSRebuild, "bob", `{"online":true}`); !errors.Is(err, ErrInvalidParams) {
		t.Errorf("layout 2, online: %v", err)
	}
}

// TestFTSOnlineExclusive: a job's exclusivity follows its parameters:
// online runs take no lock; a plan of a server before 0.51.0 (no
// "online") is exclusive, and so is an older runner's view of an online
// plan (it does not know the field: kindBase.exclusive).
func TestFTSOnlineExclusive(t *testing.T) {
	m, _ := onlineManager(t, true, true)

	for _, c := range []struct {
		kind, params string
		want         bool
	}{
		{KindFTSLayout, `{"online":true}`, false},
		{KindFTSLayout, `{"online":false}`, true},
		{KindFTSLayout, `{}`, true},
		{KindFTSLayout, `{"layout":7}`, true},
		{KindFTSRebuild, `{"online":true}`, false},
		{KindFTSRebuild, ``, true},
		{KindScrub, `{}`, false},
	} {
		k, _ := m.Kind(c.kind)
		if got := exclusive(k, json.RawMessage(c.params)); got != c.want {
			t.Errorf("%s %s: exclusive %v", c.kind, c.params, got)
		}

		if c.kind != KindScrub && !k.Exclusive() {
			t.Errorf("%s: the static property (what a server before 0.51.0 reads) must stay exclusive", c.kind)
		}
	}
}

// TestFTSOnlineJobNoLock: an online fts_layout runs without the account
// lock and without the settle wait; the request is online with a
// liveness callback that takes another lease generation of the same job
// for ended, and without the window for a run an operator planned.
func TestFTSOnlineJobNoLock(t *testing.T) {
	m, o := onlineManager(t, true, true)
	m.cfg.Settle = time.Minute

	type seen struct {
		locked, online, admit bool
		same, other           bool
	}

	got := make(chan seen, 1)

	o.fake("alice").convert = func(req model.FTSLayoutRequest) (model.FTSLayoutReport, error) {
		u, _ := o.User("alice")
		locked := errors.Is(model.RefuseDuringImport(u), model.ErrImportLocked)

		same, _ := req.ForeignAlive(model.TokenSession(req.Tok)+"/99", time.Time{})
		other, _ := req.ForeignAlive("job-unknown/1", time.Time{})

		got <- seen{locked: locked, online: req.Online, admit: req.Admit != nil, same: same, other: other}

		return model.FTSLayoutReport{Online: true}, nil
	}

	runManager(t, m)

	start := time.Now()

	waitState(t, m, applyLayout(t, m, "alice", `{}`).ID, StateSucceeded)

	if s := <-got; s.locked || !s.online || s.admit || s.same || s.other {
		t.Errorf("run %+v", s)
	}

	if time.Since(start) > 30*time.Second {
		t.Error("the online run waited the settle time")
	}
}

// TestFTSOnlineRunnerFallback (R2.5): an online fts_layout and an online
// fts_rebuild that panic after recording their run leave "next" behind;
// the runner clears it after the final state although the jobs took no
// lock.
func TestFTSOnlineRunnerFallback(t *testing.T) {
	for _, kind := range []string{KindFTSLayout, KindFTSRebuild} {
		t.Run(kind, func(t *testing.T) {
			m, o := onlineManager(t, true, true)
			f := o.fake("alice")

			f.convert = func(model.FTSLayoutRequest) (model.FTSLayoutReport, error) {
				panic("broken run")
			}

			runManager(t, m)

			k, _ := m.Kind(kind)

			plan, err := m.CreatePlan(context.Background(), k, "alice", json.RawMessage(`{}`), "tester", nil)
			if err != nil {
				t.Fatal(err)
			}

			j, _, err := m.Submit(k, "alice", nil, plan.ID, "tester", "")
			if err != nil {
				t.Fatal(err)
			}

			waitState(t, m, j.ID, StateFailed)

			// the runner clears right after it stored the final state
			for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(5 * time.Millisecond) {
				next, toks, cleared, _ := f.state()
				if next == "" && len(toks) == 1 && len(cleared) == 1 && cleared[0] == toks[0] {
					break
				}

				if time.Now().After(deadline) {
					t.Fatalf("next %q toks %v cleared %v", next, toks, cleared)
				}
			}
		})
	}
}

// TestFTSOnlineRebuildRequest: an online fts_rebuild runs the layout's
// online run in the mode reindex with its options (drop_orphan_ids by
// default).
func TestFTSOnlineRebuildRequest(t *testing.T) {
	m, o := onlineManager(t, true, true)
	got := make(chan model.FTSLayoutRequest, 1)

	o.fake("alice").convert = func(req model.FTSLayoutRequest) (model.FTSLayoutReport, error) {
		got <- req
		return model.FTSLayoutReport{Online: true, Mode: model.FTSModeReindex}, nil
	}

	runManager(t, m)

	k, _ := m.Kind(KindFTSRebuild)

	plan, err := m.CreatePlan(context.Background(), k, "alice", json.RawMessage(`{"skip_unreadable":true}`), "tester", nil)
	if err != nil {
		t.Fatal(err)
	}

	j, _, err := m.Submit(k, "alice", nil, plan.ID, "tester", "")
	if err != nil {
		t.Fatal(err)
	}

	waitState(t, m, j.ID, StateSucceeded)

	if req := <-got; !req.Online || !req.Rebuild || !req.SkipUnreadable || !req.DropOrphanIDs || req.Tok == "" {
		t.Errorf("request %+v", req)
	}
}

// winClock is a settable time for the manager.
type winClock struct{ t atomic.Int64 }

func (c *winClock) now() time.Time      { return time.Unix(0, c.t.Load()) }
func (c *winClock) set(t time.Time)     { c.t.Store(t.UnixNano()) }
func (c *winClock) add(d time.Duration) { c.t.Add(int64(d)) }

// windowAt sets a window 02:00-05:00 UTC and the manager's clock to the
// given time of 2026-10-03.
func windowAt(t *testing.T, m *Manager, h, mi int) *winClock {
	t.Helper()

	w, err := window.Parse("02:00-05:00", "UTC")
	if err != nil {
		t.Fatal(err)
	}

	m.cfg.FTSBuild.Window = w

	c := &winClock{}
	c.set(time.Date(2026, 10, 3, h, mi, 0, 0, time.UTC))
	m.now = c.now

	return c
}

// windowDue is a due setup with a window 02:00-05:00 UTC, the clock at
// the given time and an account whose index reports 1000 documents and a
// load of 1.6.
func windowDue(t *testing.T, h, mi int) (*Manager, *fakeLayout, model.DueQueue, *winClock) {
	t.Helper()

	m, o, _, q := dueSetup(t, "alice")
	f := o.fake("alice")
	f.gen = "g1"
	f.due = model.FTSLayoutShape{Layout: 3, S: 2, D: 2, H: 1, Docs: 1000, Load: 1.6}

	return m, f, q, windowAt(t, m, h, mi)
}

// nextWindow is the start of the window after 2026-10-03 12:00.
var nextWindow = time.Date(2026, 10, 4, 2, 0, 0, 0, time.UTC)

// onlyEntry returns the account's one entry or fails.
func onlyEntry(t *testing.T, q model.DueQueue) model.DueEntry {
	t.Helper()

	es := entriesOf(t, q, "alice")
	if len(es) != 1 {
		t.Fatalf("entries %+v", es)
	}

	return es[0]
}

// TestFTSDueWindowOutside (§4.12 worker step 1): outside the window an
// entry waits for the next window (start plus the account's spread)
// unless the account is urgent, which is planned by the urgent
// principal (its run is not admitted by the window again).
func TestFTSDueWindowOutside(t *testing.T) {
	m, f, q, c := windowDue(t, 12, 0)

	fire(t, m, queueDue(t, q, "alice", "g1", c.now()))

	if e := onlyEntry(t, q); dueRecord(t, m, "alice") != nil || e.Due.Before(nextWindow) || !e.Due.Before(nextWindow.Add(90*time.Minute)) {
		t.Fatalf("outside the window: record %+v entry %+v", dueRecord(t, m, "alice"), e)
	}

	f.due.Load = 1.9

	fire(t, m, onlyEntry(t, q))

	rec := dueRecord(t, m, "alice")
	if rec == nil {
		t.Fatal("urgent account not planned")
	}

	if j, err := m.Get(rec.JobID); err != nil || j.Principal != ftsDueUrgentPrincipal {
		t.Fatalf("urgent job %+v %v", j, err)
	}
}

// TestFTSDueWindowInside (§4.12 worker steps 2 and 3): inside the window
// at most build_max_running jobs of the worker run, a run that does not
// fit into what is left waits for the next window, one longer than the
// whole window is not planned (a day later again), one that fits is.
func TestFTSDueWindowInside(t *testing.T) {
	m, f, q, c := windowDue(t, 2, 10)

	m.ftsDueStarted("still-running")

	_ = m.st.kv.Update(prefixJob+"still-running", func([]byte) ([]byte, error) {
		return json.Marshal(Job{ID: "still-running", State: StateRunning})
	})

	fire(t, m, queueDue(t, q, "alice", "g1", c.now()))

	if e := onlyEntry(t, q); !e.Due.Equal(c.now().Add(ftsDueBusyWait)) || dueRecord(t, m, "alice") != nil {
		t.Fatalf("over build_max_running: %+v", e)
	}

	_ = m.st.kv.Update(prefixJob+"still-running", func([]byte) ([]byte, error) { return nil, nil })

	// 100000 × 3 ms = 5 min do not fit into the last minute
	f.due.Docs = 100000

	c.set(time.Date(2026, 10, 3, 4, 59, 0, 0, time.UTC))
	fire(t, m, onlyEntry(t, q))

	if e := onlyEntry(t, q); e.Due.Before(nextWindow) || dueRecord(t, m, "alice") != nil {
		t.Fatalf("end of the window: %+v", e)
	}

	f.due.Docs = 10_000_000

	c.set(time.Date(2026, 10, 3, 2, 30, 0, 0, time.UTC))
	fire(t, m, onlyEntry(t, q))

	// once a day: at the next window start plus the spread
	if e := onlyEntry(t, q); e.Due.Before(nextWindow) || !e.Due.Before(nextWindow.Add(90*time.Minute)) || dueRecord(t, m, "alice") != nil {
		t.Fatalf("window too small: %+v", e)
	}

	f.due.Docs = 1000

	fire(t, m, onlyEntry(t, q))

	if dueRecord(t, m, "alice") == nil {
		t.Error("not planned inside the window")
	}
}

// TestFTSDueUrgentInside (Fable review): an urgent account is planned
// inside the window too, although build_max_running is reached and its
// estimate is longer than the whole window; the factor is
// fts.reshard_urgent_factor.
func TestFTSDueUrgentInside(t *testing.T) {
	m, f, q, c := windowDue(t, 2, 10)
	m.cfg.FTSBuild.UrgentFactor = 1.7
	f.due.Load, f.due.Docs = 1.75, 10_000_000

	m.ftsDueStarted("still-running")

	_ = m.st.kv.Update(prefixJob+"still-running", func([]byte) ([]byte, error) {
		return json.Marshal(Job{ID: "still-running", State: StateRunning})
	})

	fire(t, m, queueDue(t, q, "alice", "g1", c.now()))

	rec := dueRecord(t, m, "alice")
	if rec == nil {
		t.Fatal("urgent account not planned inside the window")
	}

	if j, err := m.Get(rec.JobID); err != nil || j.Principal != ftsDueUrgentPrincipal {
		t.Fatalf("urgent job %+v %v", j, err)
	}
}

// TestFTSDueBusy (R2.10): a planned job whose online run found the
// account busy is handled like a locked account (a backoff, not a
// no-op); the next fire plans a new job.
func TestFTSDueBusy(t *testing.T) {
	// mark_off (Fable review): the mark of online runs went after the
	// plan; the same backoff, then a new (offline) plan
	for _, deferred := range []string{model.FTSDeferredBusy, model.FTSDeferredMarkOff} {
		t.Run(deferred, func(t *testing.T) { dueDeferred(t, deferred) })
	}
}

// dueDeferred is TestFTSDueBusy for one outcome of the job.
func dueDeferred(t *testing.T, deferred string) {
	t.Helper()

	m, o, _, q := dueSetup(t, "alice")
	f := o.fake("alice")
	f.gen = "g1"

	fire(t, m, queueDue(t, q, "alice", "g1", time.Now()))

	rec := dueRecord(t, m, "alice")

	res, _ := json.Marshal(model.FTSLayoutReport{Online: true, Deferred: deferred})

	if _, err := m.updateJob(rec.JobID, nil, func(j *Job) error {
		j.State, j.Finished, j.Result = StateSucceeded, m.now(), res
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	start := time.Now()

	fire(t, m, entriesOf(t, q, "alice")[0])

	after := dueRecord(t, m, "alice")
	es := entriesOf(t, q, "alice")

	if after.JobID != rec.JobID || !after.Deferred || after.NoopAt != (time.Time{}) || len(es) != 1 ||
		es[0].Due.Before(start.Add(model.FTSDueBackoff)) || es[0].Due.After(time.Now().Add(model.FTSDueNoopBackoff)) {
		t.Fatalf("busy: record %+v entries %+v", after, es)
	}

	fire(t, m, es[0])

	if again := dueRecord(t, m, "alice"); again.JobID == rec.JobID || again.Deferred {
		t.Errorf("no new job after the backoff: %+v", again)
	}
}

// TestBatchWindow (§4.12 batch): outside the window a batch of
// fts_rebuild gives itself back until the next window starts (the
// attempt does not count) and then runs its accounts; with anytime it
// runs at once.
func TestBatchWindow(t *testing.T) {
	m, _ := batchSetup(t, Config{}, false)
	c := windowAt(t, m, 12, 0)

	runManager(t, m)

	j := submitBatch(t, m, BatchParams{Kind: KindFTSRebuild})

	deadline := time.Now().Add(5 * time.Second)

	for {
		got, err := m.Get(j.ID)
		if err == nil && got.Progress.Phase == "waiting for the reshard window" {
			if !got.Lease.Until.Equal(time.Date(2026, 10, 4, 2, 0, 0, 0, time.UTC)) || got.Attempts != 0 || got.State != StateRunning {
				t.Fatalf("postponed job %+v", got)
			}

			break
		}

		if time.Now().After(deadline) {
			t.Fatalf("not postponed: %+v", got)
		}

		time.Sleep(5 * time.Millisecond)
	}

	c.set(time.Date(2026, 10, 4, 2, 0, 1, 0, time.UTC))

	done := waitState(t, m, j.ID, StateSucceeded)

	var st batchState
	if err := json.Unmarshal(done.Result, &st); err != nil || st.Done != int64(len(batchAccounts)) {
		t.Errorf("batch %+v %v", st, err)
	}

	c.set(time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC))

	if _, st := runBatch(t, m, BatchParams{Kind: KindFTSRebuild, Anytime: true}); st.Done != int64(len(batchAccounts)) {
		t.Errorf("anytime batch %+v", st)
	}
}

// TestBatchOnlineOutcomes (R2.11, R2.10): a batch of online runs counts
// an account without a layout 3 index as unsupported (not failed), tries
// a busy one again at the end, and fails one whose estimate is longer
// than the window.
func TestBatchOnlineOutcomes(t *testing.T) {
	m, store := batchSetup(t, Config{}, false)
	o := &layoutOpener{s: store}
	m.env.Accounts, m.env.Layout3, m.env.FTSOnline = o, marked(true), onlineMarked(true)

	o.fake("bob").convert = func(model.FTSLayoutRequest) (model.FTSLayoutReport, error) {
		return model.FTSLayoutReport{Online: true, Unsupported: true}, nil
	}

	var busy atomic.Int32

	o.fake("carol").convert = func(model.FTSLayoutRequest) (model.FTSLayoutReport, error) {
		if busy.Add(1) == 1 {
			return model.FTSLayoutReport{Online: true, Deferred: model.FTSDeferredBusy}, nil
		}

		return model.FTSLayoutReport{Online: true}, nil
	}

	o.fake("dave").convert = func(model.FTSLayoutRequest) (model.FTSLayoutReport, error) {
		return model.FTSLayoutReport{Online: true, Deferred: model.FTSDeferredTooSmall}, nil
	}

	runManager(t, m)

	_, st := runBatch(t, m, BatchParams{Kind: KindFTSLayout, Params: json.RawMessage(`{"online":true}`)}, StatePartiallyFailed)
	if st.Unsupported != 1 || st.Failed != 1 || st.Done != int64(len(batchAccounts)-2) || busy.Load() != 2 || st.Rounds != 1 {
		t.Errorf("batch %+v, carol ran %d times", st, busy.Load())
	}
}

// TestFTSOnlineAdmit: the run of the fts_due worker and of a batch is
// admitted by the window: outside it put off, longer than it too small.
func TestFTSOnlineAdmit(t *testing.T) {
	m, _ := layoutManager(t)
	c := windowAt(t, m, 2, 0)

	if err := m.admitBuild(model.FTSModeReindex, 1000); err != nil {
		t.Errorf("fits: %v", err)
	}

	if err := m.admitBuild(model.FTSModeReindex, 10_000_000); !errors.Is(err, model.ErrFTSWindowTooSmall) {
		t.Errorf("too long: %v", err)
	}

	c.add(170 * time.Minute)

	if err := m.admitBuild(model.FTSModeReindex, 100_000); !errors.Is(err, model.ErrFTSBuildWindow) {
		t.Errorf("rest of the window: %v", err)
	}
}
