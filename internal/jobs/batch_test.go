package jobs

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"shardpost/internal/memstore"
	"shardpost/internal/model"
)

// batchAccounts are the accounts of the batch tests (all registered).
var batchAccounts = []string{"alice", "bob", "carol", "dave", "erin"}

// batchSetup registers the accounts in a memstore directory, marks its
// backfill complete (unless unmarked) and returns a running manager.
func batchSetup(t *testing.T, cfg Config, unmarked bool) (*Manager, *memstore.Store) {
	t.Helper()

	store := memstore.New()

	for _, name := range batchAccounts {
		u, _ := store.User(name)
		if err := u.EnsureMailbox("INBOX", nil); err != nil {
			t.Fatal(err)
		}

		if _, err := u.Deliver("INBOX", []byte(testMessage), nil, time.Now()); err != nil {
			t.Fatal(err)
		}
	}

	if !unmarked {
		markBackfilled(t, store)
	}

	cfg.Poll, cfg.LeaseTTL = cmpDur(cfg.Poll, 10*time.Millisecond), cmpDur(cfg.LeaseTTL, 300*time.Millisecond)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	m := NewManager(memstore.NewAdminKV(), &Env{Accounts: store, Directory: store, Log: log}, cfg, "pod-a", Kinds(time.Hour)...)

	return m, store
}

func markBackfilled(t *testing.T, store *memstore.Store) {
	t.Helper()

	ix, release, _ := store.AccountIndex()
	defer release()

	if err := ix.SetMarker(model.AccountBackfillMarker, `{"finished":"2026-10-01T00:00:00Z"}`); err != nil {
		t.Fatal(err)
	}
}

// runBatch plans and applies a batch and waits for one of the states.
func runBatch(t *testing.T, m *Manager, p BatchParams, states ...string) (*Job, batchState) {
	t.Helper()

	if len(states) == 0 {
		states = []string{StateSucceeded}
	}

	j := waitState(t, m, submitBatch(t, m, p).ID, states...)

	var st batchState
	if err := json.Unmarshal(j.Result, &st); err != nil {
		t.Fatalf("result %s: %v", j.Result, err)
	}

	return j, st
}

func submitBatch(t *testing.T, m *Manager, p BatchParams) *Job {
	t.Helper()

	plan, err := m.CreateBatchPlan(p, "tester")
	if err != nil {
		t.Fatalf("plan: %v", err)
	}

	if plan.Account != "" || plan.Kind != KindBatch {
		t.Fatalf("plan %+v", plan)
	}

	k, _ := m.Kind(KindBatch)

	j, _, err := m.Submit(k, "", nil, plan.ID, "tester", "")
	if err != nil {
		t.Fatalf("apply: %v", err)
	}

	return j
}

func TestBatchIndexCheckOverAllAccounts(t *testing.T) {
	m, _ := batchSetup(t, Config{BatchParallel: 2}, false)
	runManager(t, m)

	j, st := runBatch(t, m, BatchParams{Kind: KindIndexCheck})

	if j.Account != "" || st.Accounts != int64(len(batchAccounts)) || st.Done != st.Accounts || st.Failed != 0 {
		t.Fatalf("job %+v result %+v", j, st)
	}

	if st.Cursor != "" || st.Walked {
		t.Fatalf("resume position in the result: %+v", st)
	}
}

func TestBatchRefusals(t *testing.T) {
	m, _ := batchSetup(t, Config{}, true)

	// Without a completed directory backfill accounts could be missed.
	if _, err := m.CreateBatchPlan(BatchParams{Kind: KindIndexCheck}, "tester"); !errors.Is(err, ErrDirectoryNotReady) {
		t.Fatalf("unmarked directory: %v", err)
	}

	m, _ = batchSetup(t, Config{}, false)

	for name, p := range map[string]BatchParams{
		"purge":            {Kind: KindPurge},
		"unknown":          {Kind: "nonsense"},
		"skip_idle":        {Kind: KindFTSRebuild, SkipIdle: true},
		"params":           {Kind: KindIndexCheck, Params: json.RawMessage(`{"x":1}`)},
		"scrub class":      {Kind: KindScrub, Params: json.RawMessage(`{"classes":["bogus"]}`)},
		"batch of batches": {Kind: KindBatch},
	} {
		if _, err := m.CreateBatchPlan(p, "tester"); err == nil {
			t.Errorf("%s: planned", name)
		}
	}

	batch, _ := m.Kind(KindBatch)
	check, _ := m.Kind(KindIndexCheck)

	// Only a batch is account-less, and a batch has no account.
	if _, _, err := m.Submit(check, "", nil, "", "tester", ""); !errors.Is(err, ErrInvalidParams) {
		t.Fatalf("account-less check: %v", err)
	}

	if _, err := m.CreatePlan(t.Context(), batch, "alice", nil, "tester", nil); !errors.Is(err, ErrInvalidParams) {
		t.Fatalf("batch plan for one account: %v", err)
	}

	p, err := m.CreateBatchPlan(BatchParams{Kind: KindIndexCheck}, "tester")
	if err != nil {
		t.Fatal(err)
	}

	if _, _, err := m.Submit(batch, "alice", nil, p.ID, "tester", ""); !errors.Is(err, ErrInvalidParams) {
		t.Fatalf("batch with an account: %v", err)
	}
}

func TestBatchRoleAndExclusiveFromKind(t *testing.T) {
	m, _ := batchSetup(t, Config{}, false)
	batch, _ := m.Kind(KindBatch)

	for kind, want := range map[string]struct {
		role      string
		exclusive bool
	}{
		KindIndexCheck:  {RoleDiagnose, false},
		KindScrub:       {RoleRepair, false},
		KindFTSRebuild:  {RoleRepair, true},
		KindAttachments: {RoleRepair, false},
	} {
		params, _ := json.Marshal(BatchParams{Kind: kind})
		if got := m.RoleOf(batch, params); got != want.role {
			t.Errorf("%s: role %s", kind, got)
		}

		if got := m.ExclusiveOf(batch, params); got != want.exclusive {
			t.Errorf("%s: exclusive %v", kind, got)
		}
	}

	if batch.Exclusive() {
		t.Fatal("the batch itself takes a lock")
	}

	if got := m.RoleOf(batch, json.RawMessage(`{"kind":"purge"}`)); got != RoleDelete {
		t.Fatalf("unknown sub-kind: role %s", got)
	}
}

// lockBob takes bob's account lock as an import would.
func lockBob(t *testing.T, store *memstore.Store) model.Importer {
	t.Helper()

	bob, _ := store.User("bob")
	imp := bob.(model.Importer)

	if _, err := imp.ImportLock("import-1", time.Minute); err != nil {
		t.Fatal(err)
	}

	return imp
}

// TestBatchExclusiveDefersLockedAccounts: an exclusive kind takes the
// maintenance lock per account; an account an import holds is put aside,
// tried again at the end, and fails when it stays locked.
func TestBatchExclusiveDefersLockedAccounts(t *testing.T) {
	m, store := batchSetup(t, Config{BatchParallel: 2, LeaseTTL: 150 * time.Millisecond}, false)
	lockBob(t, store)
	runManager(t, m)

	j, st := runBatch(t, m, BatchParams{Kind: KindFTSRebuild}, StatePartiallyFailed)

	if st.Done != int64(len(batchAccounts)-1) || st.Failed != 1 || st.Rounds != batchRetryRounds || len(st.Errors) != 1 ||
		st.Errors[0].Account != "bob" || len(st.Deferred) != 0 || len(j.Errors) != 1 {
		t.Fatalf("result %+v, job errors %v", st, j.Errors)
	}

	// The locks of the others were released again.
	for _, name := range batchAccounts {
		u, _ := store.User(name)
		if locked, _ := u.(model.Importer).ImportLocked(); locked != (name == "bob") {
			t.Errorf("%s locked: %v", name, locked)
		}
	}
}

// TestBatchRetriesDeferredAccounts: an account released before the end
// is done in a retry round.
func TestBatchRetriesDeferredAccounts(t *testing.T) {
	m, store := batchSetup(t, Config{LeaseTTL: 200 * time.Millisecond}, false)
	imp := lockBob(t, store)

	runManager(t, m)

	j := submitBatch(t, m, BatchParams{Kind: KindFTSRebuild})

	waitFor(t, func() bool {
		cur, _ := m.Get(j.ID)

		var cp batchState

		_ = json.Unmarshal(cur.Checkpoint, &cp)

		return cp.Walked && slices.Contains(cp.Deferred, "bob")
	})

	if err := imp.ImportUnlock("import-1"); err != nil {
		t.Fatal(err)
	}

	done := waitState(t, m, j.ID, StateSucceeded)

	var st batchState

	_ = json.Unmarshal(done.Result, &st)

	if st.Done != int64(len(batchAccounts)) || st.Failed != 0 || st.Rounds == 0 {
		t.Fatalf("result %+v", st)
	}
}

// checkScrubRecords checks that every account has a complete, clean
// scrub record.
func checkScrubRecords(t *testing.T, store *memstore.Store) {
	t.Helper()

	for _, name := range batchAccounts {
		e, _ := dirEntry(t, store, name)
		if rec, ok := e.ScrubOf(); !ok || !rec.Complete || rec.Findings != 0 {
			t.Fatalf("%s: scrub record %s", name, e.Scrub)
		}
	}
}

// TestBatchScrubSkipsIdleAccounts: a scrub batch records each account's
// state; the next one with skip_idle leaves the unchanged accounts alone
// and scrubs the changed one.
func TestBatchScrubSkipsIdleAccounts(t *testing.T) {
	m, store := batchSetup(t, Config{ScrubParallel: 3}, false)
	runManager(t, m)

	p := BatchParams{Kind: KindScrub, SkipIdle: true}
	all := int64(len(batchAccounts))

	if _, st := runBatch(t, m, p); st.Done != all || st.Skipped != 0 {
		t.Fatalf("first run %+v", st)
	}

	checkScrubRecords(t, store)

	if _, st := runBatch(t, m, p); st.Skipped != all || st.Done != 0 {
		t.Fatalf("second run %+v", st)
	}

	carol, _ := store.User("carol")
	if _, err := carol.Deliver("INBOX", []byte(testMessage), nil, time.Now()); err != nil {
		t.Fatal(err)
	}

	if _, st := runBatch(t, m, p); st.Done != 1 || st.Skipped != all-1 {
		t.Fatalf("third run %+v", st)
	}

	// Without skip_idle every account is scrubbed.
	if _, st := runBatch(t, m, BatchParams{Kind: KindScrub}); st.Done != all {
		t.Fatalf("full run %+v", st)
	}
}

// TestBatchScrubOfClassesLeavesNoRecord: a scrub of selected classes
// cannot make the next batch skip an account.
func TestBatchScrubOfClassesLeavesNoRecord(t *testing.T) {
	m, store := batchSetup(t, Config{}, false)
	runManager(t, m)

	runBatch(t, m, BatchParams{Kind: KindScrub, Params: json.RawMessage(`{"classes":["refs"]}`)})

	if e, _ := dirEntry(t, store, "alice"); len(e.Scrub) != 0 {
		t.Fatalf("partial scrub recorded %s", e.Scrub)
	}
}

// TestBatchScrubIntervalBoundsSkipping: an idle account is scrubbed again
// once its record is older than the interval.
func TestBatchScrubIntervalBoundsSkipping(t *testing.T) {
	m, _ := batchSetup(t, Config{ScrubInterval: time.Hour}, false)

	var offset atomic.Int64

	m.now = func() time.Time { return time.Now().Add(time.Duration(offset.Load())) }

	runManager(t, m)

	p := BatchParams{Kind: KindScrub, SkipIdle: true}
	runBatch(t, m, p)

	offset.Store(int64(2 * time.Hour))

	if _, st := runBatch(t, m, p); st.Done != int64(len(batchAccounts)) {
		t.Fatalf("after the interval %+v", st)
	}
}

// TestBatchResumesFromCheckpoint: a server that takes a batch over goes
// on after the last page its predecessor checkpointed.
func TestBatchResumesFromCheckpoint(t *testing.T) {
	m, store := batchSetup(t, Config{}, false)

	ix, release, _ := store.AccountIndex()
	first, next, err := ix.List("", 2)

	release()

	if err != nil || len(first) != 2 || next == "" {
		t.Fatalf("directory %v %q %v", first, next, err)
	}

	j := submitBatch(t, m, BatchParams{Kind: KindIndexCheck})

	// The predecessor did the first two accounts.
	cp, _ := json.Marshal(batchState{Kind: KindIndexCheck, Cursor: next, Accounts: 2, Done: 2})

	if _, err := m.updateJob(j.ID, nil, func(cur *Job) error { cur.Checkpoint = cp; return nil }); err != nil {
		t.Fatal(err)
	}

	runManager(t, m)

	done := waitState(t, m, j.ID, StateSucceeded)

	var st batchState

	_ = json.Unmarshal(done.Result, &st)

	if st.Accounts != int64(len(batchAccounts)) || st.Done != st.Accounts {
		t.Fatalf("result %+v", st)
	}
}

// TestBatchCancelBetweenAccounts: a cancel request stops the batch at the
// next account.
func TestBatchCancelBetweenAccounts(t *testing.T) {
	m, _ := batchSetup(t, Config{Settle: 150 * time.Millisecond}, false)
	runManager(t, m)

	j := submitBatch(t, m, BatchParams{Kind: KindFTSRebuild})

	waitState(t, m, j.ID, StateRunning)
	time.Sleep(200 * time.Millisecond)

	if _, err := m.Cancel(j.ID); err != nil {
		t.Fatal(err)
	}

	done := waitState(t, m, j.ID, StateCancelled)

	var st batchState

	_ = json.Unmarshal(done.Result, &st)

	if st.Accounts >= int64(len(batchAccounts)) {
		t.Fatalf("not stopped: %+v", st)
	}
}

// TestBatchSkipsPurgedAccounts: an entry left purging without registry is
// gone; with registry it counts as active.
func TestBatchSkipsPurgedAccounts(t *testing.T) {
	m, store := batchSetup(t, Config{}, false)

	ix, release, _ := store.AccountIndex()
	_ = model.MarkAccountPurging(ix, "dave", "job-x", "", time.Now())
	_ = model.MarkAccountPurging(ix, "ghost", "job-y", "", time.Now())

	release()

	runManager(t, m)

	_, st := runBatch(t, m, BatchParams{Kind: KindIndexCheck})

	if st.Gone != 1 || st.Done != int64(len(batchAccounts)) {
		t.Fatalf("result %+v", st)
	}
}

func TestBatchStateBounded(t *testing.T) {
	var st batchState

	long := string(make([]byte, 2*maxBatchErrText))

	for i := range 2 * maxBatchErrors {
		st.fail(fmt.Sprintf("a%d", i), long)
	}

	results := make([]acctResult, 0, maxBatchDeferred+10)
	for i := range maxBatchDeferred + 10 {
		results = append(results, acctResult{account: fmt.Sprintf("d%d", i), out: outDeferred, findings: 1})
	}

	st.add(results)

	b, _ := json.Marshal(st)

	switch {
	case len(st.Errors) != maxBatchErrors || st.ErrorsDropped != int64(maxBatchErrors+10) || st.Failed != int64(2*maxBatchErrors+10):
		t.Fatalf("errors %d dropped %d failed %d", len(st.Errors), st.ErrorsDropped, st.Failed)
	case len(st.Deferred) != maxBatchDeferred || len(st.FindingAccounts) != maxBatchFindings:
		t.Fatalf("deferred %d findings %d", len(st.Deferred), len(st.FindingAccounts))
	case len(b) > model.MaxAdminValue/16:
		t.Fatalf("record of %d bytes", len(b))
	case len(st.Errors[0].Error) > maxBatchErrText+3:
		t.Fatalf("error text of %d bytes", len(st.Errors[0].Error))
	}
}

// TestSubmitBatchOnce: the nightly scrub does not start a second batch
// while the previous one has not ended.
func TestSubmitBatchOnce(t *testing.T) {
	m, _ := batchSetup(t, Config{}, false)
	p := BatchParams{Kind: KindScrub, SkipIdle: true}

	first, created, err := m.SubmitBatchOnce(p, "cron")
	if err != nil || !created {
		t.Fatalf("first: %v %v", created, err)
	}

	again, created, err := m.SubmitBatchOnce(p, "cron")
	if err != nil || created || again.ID != first.ID {
		t.Fatalf("second while queued: %v %v %v", again, created, err)
	}

	// Another kind is not held back.
	if _, created, err := m.SubmitBatchOnce(BatchParams{Kind: KindIndexCheck}, "cron"); err != nil || !created {
		t.Fatalf("index_check: %v %v", created, err)
	}

	runManager(t, m)
	waitState(t, m, first.ID, StateSucceeded)

	if _, created, err := m.SubmitBatchOnce(p, "cron"); err != nil || !created {
		t.Fatalf("after the end: %v %v", created, err)
	}
}

// TestSubmitBatchOnceRace: of two batches submitted at the same time the
// newer one is cancelled.
func TestSubmitBatchOnceRace(t *testing.T) {
	m, _ := batchSetup(t, Config{}, false)
	p := BatchParams{Kind: KindScrub, SkipIdle: true}

	// The other submitter got its batch in first, after this one looked.
	older, created, err := m.SubmitBatchOnce(p, "cron-a")
	if err != nil || !created {
		t.Fatal(err)
	}

	plan, _ := m.CreateBatchPlan(p, "cron-b")
	k, _ := m.Kind(KindBatch)

	newer, _, err := m.Submit(k, "", nil, plan.ID, "cron-b", "")
	if err != nil {
		t.Fatal(err)
	}

	js, err := m.unfinishedBatches(KindScrub)
	if err != nil || len(js) != 2 || js[0].ID != older.ID || js[1].ID != newer.ID {
		t.Fatalf("unfinished %v %v", js, err)
	}
}
