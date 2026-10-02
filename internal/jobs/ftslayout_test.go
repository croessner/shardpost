package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"shardpost/internal/memstore"
	"shardpost/internal/model"
)

// fakeLayout is the full-text layout of one account as the job tests see
// it: the token of an unfinished conversion and what was called.
type fakeLayout struct {
	mu      sync.Mutex
	gen     string   // current generation
	old     []string // runs that left sources after their switch
	oldAt   map[string]time.Time
	next    string
	nextAt  time.Time
	toks    []string // tokens of ConvertFTSLayout
	cleared []string // tokens ClearFTSLayoutNext removed
	foreign []string // own tokens of ClearForeignFTSLayoutNext calls
	convert func(req model.FTSLayoutRequest) (model.FTSLayoutReport, error)
}

func (f *fakeLayout) state() (next string, toks, cleared, foreign []string) {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.next, append([]string(nil), f.toks...), append([]string(nil), f.cleared...), append([]string(nil), f.foreign...)
}

type importerUser interface {
	model.User
	model.Importer
}

// layoutUser is a memstore account with a fake layout.
type layoutUser struct {
	importerUser
	f *fakeLayout
}

func (u *layoutUser) FTSLayout() (model.FTSLayoutState, error) {
	u.f.mu.Lock()
	defer u.f.mu.Unlock()

	st := model.FTSLayoutState{Current: model.FTSLayoutShape{Layout: 3, Gen: u.f.gen}, OldToks: append([]string(nil), u.f.old...)}
	if u.f.next != "" {
		st.Next, st.NextTok, st.NextAt = &model.FTSLayoutShape{Layout: 3}, u.f.next, u.f.nextAt
	}

	for _, tok := range u.f.old {
		st.OldAt = append(st.OldAt, u.f.oldAt[tok])
	}

	return st, nil
}

func (u *layoutUser) FTSLayoutDue() (model.FTSLayoutShape, bool, error) {
	return model.FTSLayoutShape{Layout: 3, S: 1, D: 1, H: 1}, false, nil
}

func (u *layoutUser) ConvertFTSLayout(req model.FTSLayoutRequest) (model.FTSLayoutReport, error) {
	u.f.mu.Lock()
	u.f.toks = append(u.f.toks, req.Tok)
	u.f.next = req.Tok
	conv := u.f.convert
	u.f.mu.Unlock()

	if conv == nil {
		u.f.mu.Lock()
		u.f.next = ""
		u.f.mu.Unlock()

		return model.FTSLayoutReport{}, req.Fence()
	}

	rep, err := conv(req)
	if err == nil {
		u.f.mu.Lock()
		u.f.next = ""
		u.f.mu.Unlock()
	}

	return rep, err
}

func (u *layoutUser) ClearFTSLayoutNext(tok string) (bool, error) {
	u.f.mu.Lock()
	defer u.f.mu.Unlock()

	if u.f.next == "" || u.f.next != tok {
		return false, nil
	}

	u.f.cleared, u.f.next = append(u.f.cleared, tok), ""

	return true, nil
}

func (u *layoutUser) ClearForeignFTSLayoutNext(own string) (string, error) {
	u.f.mu.Lock()
	defer u.f.mu.Unlock()

	u.f.foreign = append(u.f.foreign, own)

	if u.f.next == "" || u.f.next == own {
		return "", nil
	}

	tok := u.f.next
	u.f.cleared, u.f.next = append(u.f.cleared, tok), ""

	return tok, nil
}

func (u *layoutUser) ClearFTSLayoutOld(tok string) (bool, error) {
	u.f.mu.Lock()
	defer u.f.mu.Unlock()

	for i, o := range u.f.old {
		if o == tok {
			u.f.old = append(u.f.old[:i:i], u.f.old[i+1:]...)
			u.f.cleared = append(u.f.cleared, tok)

			return true, nil
		}
	}

	return false, nil
}

func (u *layoutUser) HeartbeatFTSLayoutNext(tok string, at time.Time) (bool, error) {
	u.f.mu.Lock()
	defer u.f.mu.Unlock()

	if u.f.next != tok {
		return false, nil
	}

	u.f.nextAt = at

	return true, nil
}

// layoutOpener opens memstore accounts with fake layouts.
type layoutOpener struct {
	s     *memstore.Store
	mu    sync.Mutex
	fakes map[string]*fakeLayout
}

func (o *layoutOpener) fake(name string) *fakeLayout {
	o.mu.Lock()
	defer o.mu.Unlock()

	if o.fakes == nil {
		o.fakes = map[string]*fakeLayout{}
	}

	if o.fakes[name] == nil {
		o.fakes[name] = &fakeLayout{}
	}

	return o.fakes[name]
}

func (o *layoutOpener) User(name string) (model.User, error) {
	u, err := o.s.User(name)
	if err != nil {
		return nil, err
	}

	return &layoutUser{importerUser: u.(importerUser), f: o.fake(name)}, nil
}

// marked is the cluster mark of the tests.
type marked bool

func (m marked) FTSLayout3Enabled() (bool, error) { return bool(m), nil }

// layoutManager is a manager over fake layouts with the mark set.
func layoutManager(t *testing.T) (*Manager, *layoutOpener) {
	t.Helper()

	store := memstore.New()
	o := &layoutOpener{s: store}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	m := NewManager(memstore.NewAdminKV(), &Env{Accounts: o, Directory: store, Log: log, Layout3: marked(true)},
		Config{Poll: 10 * time.Millisecond, LeaseTTL: 300 * time.Millisecond}, "pod-a", Kinds(time.Hour)...)

	return m, o
}

// applyLayout plans and applies fts_layout for the account.
func applyLayout(t *testing.T, m *Manager, account, params string) *Job {
	t.Helper()

	k, _ := m.Kind(KindFTSLayout)

	plan, err := m.CreatePlan(context.Background(), k, account, json.RawMessage(params), "tester", nil)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}

	j, _, err := m.Submit(k, account, nil, plan.ID, "tester", "")
	if err != nil {
		t.Fatalf("apply: %v", err)
	}

	return j
}

// TestFTSLayoutJob: fts_layout is planned, exclusive and in the batch
// kinds; it runs the conversion with the token of its lease generation
// and the job's fence.
func TestFTSLayoutJob(t *testing.T) {
	m, o := layoutManager(t)

	k, err := m.Kind(KindFTSLayout)
	if err != nil || !k.Planned() || !k.Exclusive() || k.Role() != RoleRepair {
		t.Fatalf("kind %v %v", k, err)
	}

	if !strings.Contains(strings.Join(FanOutKinds, ","), KindFTSLayout) {
		t.Error("fts_layout is not a batch kind")
	}

	var fenced bool

	o.fake("alice").convert = func(req model.FTSLayoutRequest) (model.FTSLayoutReport, error) {
		fenced = req.Fence() == nil && req.Layout == 3 && req.Shards == 4
		return model.FTSLayoutReport{}, nil
	}

	runManager(t, m)

	j := waitState(t, m, applyLayout(t, m, "alice", `{"layout":3,"shards":4}`).ID, StateSucceeded)

	_, toks, _, _ := o.fake("alice").state()
	if len(toks) != 1 || toks[0] != model.JobLockToken(lockSession(j.ID), j.Lease.Gen) || !fenced {
		t.Errorf("tokens %v (lease generation %d), fenced %v", toks, j.Lease.Gen, fenced)
	}
}

// TestFTSLayoutParams: bad parameters and layout 3 without the mark are
// refused (N7: the mark is read when the job plans and starts).
func TestFTSLayoutParams(t *testing.T) {
	m, _ := layoutManager(t)
	k, _ := m.Kind(KindFTSLayout)

	for _, p := range []string{`{"layout":4}`, `{"shards":-1}`, `{"layout":2,"shards":4}`, `{"shards":5000}`} {
		if _, err := m.CreatePlan(context.Background(), k, "alice", json.RawMessage(p), "tester", nil); !errors.Is(err, ErrInvalidParams) {
			t.Errorf("%s: %v", p, err)
		}
	}

	m.env.Layout3 = marked(false)

	if _, err := m.CreatePlan(context.Background(), k, "alice", json.RawMessage(`{"layout":3}`), "tester", nil); !errors.Is(err, model.ErrFTSLayoutMark) {
		t.Errorf("layout 3 without the mark: %v", err)
	}

	if _, err := m.CreatePlan(context.Background(), k, "alice", json.RawMessage(`{"layout":2}`), "tester", nil); err != nil {
		t.Errorf("layout 2 without the mark: %v", err)
	}
}

// TestFTSLayoutCancelClears (R4.4, F5): a cancel request makes the run's
// fence answer Canceled; the kind's defer clears its unfinished
// conversion.
func TestFTSLayoutCancelClears(t *testing.T) {
	m, o := layoutManager(t)
	started := make(chan struct{})

	o.fake("alice").convert = func(req model.FTSLayoutRequest) (model.FTSLayoutReport, error) {
		close(started)

		for {
			if err := req.Fence(); err != nil {
				return model.FTSLayoutReport{}, err
			}

			time.Sleep(5 * time.Millisecond)
		}
	}

	runManager(t, m)

	j := applyLayout(t, m, "alice", `{"layout":3}`)

	<-started

	if _, err := m.Cancel(j.ID); err != nil {
		t.Fatal(err)
	}

	waitState(t, m, j.ID, StateCancelled)

	next, toks, cleared, _ := o.fake("alice").state()
	if next != "" || len(cleared) != 1 || cleared[0] != toks[0] {
		t.Errorf("after cancel: next %q, cleared %v", next, cleared)
	}
}

// TestFTSLayoutLeaseLostKeeps (R4.4, R4.1): after a lost lease neither
// the kind nor the runner clears: the successor treats the conversion as
// foreign.
func TestFTSLayoutLeaseLostKeeps(t *testing.T) {
	m, o := layoutManager(t)

	o.fake("alice").convert = func(model.FTSLayoutRequest) (model.FTSLayoutReport, error) {
		return model.FTSLayoutReport{}, ErrLeaseLost
	}

	j := applyLayout(t, m, "alice", `{"layout":3}`)

	claimed, gen, err := m.claim(j.ID, nil)
	if err != nil {
		t.Fatal(err)
	}

	m.execute(context.Background(), claimed, gen)

	next, _, cleared, _ := o.fake("alice").state()
	if next == "" || len(cleared) != 0 {
		t.Errorf("after a lost lease: next %q, cleared %v", next, cleared)
	}
}

// TestFTSLayoutErrorClears (F5): a run that fails clears its conversion
// itself; the runner's fallback after the final state finds nothing
// left.
func TestFTSLayoutErrorClears(t *testing.T) {
	m, o := layoutManager(t)

	o.fake("alice").convert = func(model.FTSLayoutRequest) (model.FTSLayoutReport, error) {
		return model.FTSLayoutReport{}, errors.New("osd gone")
	}

	runManager(t, m)

	waitState(t, m, applyLayout(t, m, "alice", `{"layout":3}`).ID, StateFailed)

	if next, toks, cleared, _ := o.fake("alice").state(); next != "" || len(cleared) != 1 || cleared[0] != toks[0] {
		t.Errorf("next %q cleared %v", next, cleared)
	}
}

// TestFTSLayoutRunnerFallback (F5): a run that leaves its conversion
// without clearing it (a kind that did not) is cleared by the runner
// after the final state is stored, with exactly its token.
func TestFTSLayoutRunnerFallback(t *testing.T) {
	m, o := layoutManager(t)
	j := applyLayout(t, m, "alice", `{"layout":3}`)

	claimed, gen, err := m.claim(j.ID, nil)
	if err != nil {
		t.Fatal(err)
	}

	run, err := m.prepare(claimed, gen)
	if err != nil {
		t.Fatal(err)
	}

	f := o.fake("alice")
	f.next = run.layoutToken()

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	if !m.finish(run, claimed, gen, nil, errors.New("crashed run"), log) {
		t.Fatal("final state not stored")
	}

	m.clearFinished(run, log)

	if next, _, cleared, _ := f.state(); next != "" || len(cleared) != 1 {
		t.Errorf("fallback: next %q cleared %v", next, cleared)
	}
}

// TestLockTakerClearsForeign (F2, R4.3): an exclusive job that takes the
// account lock clears a foreign unfinished conversion (here one of an
// aborted import session) before it runs.
func TestLockTakerClearsForeign(t *testing.T) {
	m, o := layoutManager(t)
	step := &stepKind{name: "count", exclusive: true, steps: 1}
	m.kinds[step.name] = step

	f := o.fake("alice")
	f.next = "importsession0123456/abcd"

	runManager(t, m)

	j, _, err := m.Submit(step, "alice", nil, "", "tester", "")
	if err != nil {
		t.Fatal(err)
	}

	done := waitState(t, m, j.ID, StateSucceeded)

	next, _, cleared, foreign := f.state()
	if next != "" || len(cleared) != 1 || cleared[0] != "importsession0123456/abcd" ||
		len(foreign) != 1 || foreign[0] != model.JobLockToken(lockSession(j.ID), done.Lease.Gen) {
		t.Errorf("next %q cleared %v foreign %v", next, cleared, foreign)
	}
}

// TestBatchLockClearsForeign (R4.3): the batch clears a foreign
// conversion of each account it locks; a failing account clears its own
// (F5, no runner hook in a batch) and the batch ends partially failed.
func TestBatchLayoutPartial(t *testing.T) {
	m, store := batchSetup(t, Config{}, false)
	o := &layoutOpener{s: store}
	m.env.Accounts, m.env.Layout3 = o, marked(true)

	o.fake("bob").next = "oldsession0123456789/x"
	o.fake("carol").convert = func(model.FTSLayoutRequest) (model.FTSLayoutReport, error) {
		return model.FTSLayoutReport{}, errors.New("carol fails")
	}

	runManager(t, m)

	_, st := runBatch(t, m, BatchParams{Kind: KindFTSLayout, Params: json.RawMessage(`{"layout":3}`)}, StatePartiallyFailed)
	if st.Failed != 1 || st.Done != int64(len(batchAccounts)-1) {
		t.Errorf("batch %+v", st)
	}

	if next, _, cleared, _ := o.fake("bob").state(); next != "" || len(cleared) != 1 || cleared[0] != "oldsession0123456789/x" {
		t.Errorf("bob: next %q cleared %v", next, cleared)
	}

	if next, toks, cleared, _ := o.fake("carol").state(); next != "" || len(cleared) != 1 || cleared[0] != toks[0] {
		t.Errorf("carol: next %q cleared %v", next, cleared)
	}
}

// TestClearStaleLayout: the jobs scrub's fallback clears the conversion
// of a job that does not run any more and of an import reshard whose
// heartbeat is old; it leaves a running one alone.
func TestClearStaleLayout(t *testing.T) {
	m, o := layoutManager(t)
	now := time.Now()

	u, _ := o.User("alice")
	f := o.fake("alice")

	running := applyLayout(t, m, "alice", `{"layout":3}`)

	claimed, gen, err := m.claim(running.ID, nil)
	if err != nil {
		t.Fatal(err)
	}

	for _, c := range []struct {
		tok   string
		at    time.Time
		clear bool
	}{
		{model.JobLockToken(lockSession(claimed.ID), gen), time.Time{}, false},
		{model.JobLockToken(lockSession("20261001T000000Z-000000000000"), 1), time.Time{}, true},
		{"importsession0123456/n1", now.Add(-30 * time.Second), false},
		{"importsession0123456/n2", now.Add(-2 * ImportReshardFresh), true},
	} {
		f.mu.Lock()
		f.next, f.nextAt = c.tok, c.at
		f.mu.Unlock()

		tok, err := ClearStaleLayout(m.st.kv, u, now)
		if err != nil || (tok != "") != c.clear {
			t.Errorf("%s: cleared %q %v, want %v", c.tok, tok, err, c.clear)
		}
	}

	// sources a crashed run left after its switch go as well (its job
	// ended), those of a running one stay
	ended := model.JobLockToken(lockSession("20261001T000000Z-000000000001"), 1)
	live := model.JobLockToken(lockSession(claimed.ID), gen)

	f.mu.Lock()
	f.next, f.old = "", []string{ended, live}
	f.mu.Unlock()

	if tok, err := ClearStaleLayout(m.st.kv, u, now); err != nil || tok != ended {
		t.Errorf("old entries: cleared %q %v", tok, err)
	}

	f.mu.Lock()
	left := append([]string(nil), f.old...)
	f.mu.Unlock()

	if len(left) != 1 || left[0] != live {
		t.Errorf("old entries left %v", left)
	}
}

// TestClearStaleLayoutImportOld (review round 2, L2): an import reshard
// between its switch and P7 renews the heartbeat in its "old" entry: it
// counts as alive while fresh and is cleared when stale.
func TestClearStaleLayoutImportOld(t *testing.T) {
	m, o := layoutManager(t)
	now := time.Now()

	u, _ := o.User("alice")
	f := o.fake("alice")
	beating, gone := "importsession0123456/n3", "importsession0123456/n4"

	f.mu.Lock()
	f.old = []string{beating, gone}
	f.oldAt = map[string]time.Time{beating: now.Add(-30 * time.Second), gone: now.Add(-2 * ImportReshardFresh)}
	f.mu.Unlock()

	if tok, err := ClearStaleLayout(m.st.kv, u, now); err != nil || tok != gone {
		t.Errorf("import reshard old entries: cleared %q %v", tok, err)
	}
}

// dictLayoutUser is a layout 3 account (fake layout) that can convert
// its dictionary (memstore).
type dictLayoutUser struct {
	*layoutUser
	conv model.FTSDictConverter
}

func (u dictLayoutUser) LegacyFTSDict() (int, error) { return u.conv.LegacyFTSDict() }

func (u dictLayoutUser) ConvertFTSDict(fence func() error, progress func(done, total int)) (model.FTSDictReport, error) {
	return u.conv.ConvertFTSDict(fence, progress)
}

// dictLayoutOpener opens dictLayoutUser views.
type dictLayoutOpener struct{ *layoutOpener }

func (o dictLayoutOpener) User(name string) (model.User, error) {
	u, err := o.s.User(name)
	if err != nil {
		return nil, err
	}

	return dictLayoutUser{layoutUser: &layoutUser{importerUser: u.(importerUser), f: o.fake(name)}, conv: u.(model.FTSDictConverter)}, nil
}

// TestFTSDictConvertLayout3: fts_dict_convert is a legacy kind for
// layout 2 indexes; planning it for a layout 3 account is refused with
// a message that says why.
func TestFTSDictConvertLayout3(t *testing.T) {
	m, o := layoutManager(t)
	m.env.Accounts = dictLayoutOpener{o}

	k, _ := m.Kind(KindFTSDict)

	_, err := m.CreatePlan(context.Background(), k, "alice", nil, "tester", nil)
	if !errors.Is(err, model.ErrFTSDictLayout3) || !strings.Contains(err.Error(), "legacy") {
		t.Fatalf("plan on layout 3: %v", err)
	}
}
