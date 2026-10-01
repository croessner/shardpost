package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"shardpost/internal/memstore"
	"shardpost/internal/model"
)

func dirEntry(t *testing.T, store *memstore.Store, account string) (model.AccountEntry, bool) {
	t.Helper()

	ix, release, _ := store.AccountIndex()
	defer release()

	e, ok, err := model.GetAccountEntry(ix, account)
	if err != nil {
		t.Fatal(err)
	}

	return e, ok
}

// TestPurgeRemovesDirectoryEntry: after a purge the account has neither
// registry nor directory entry; the name is free again.
func TestPurgeRemovesDirectoryEntry(t *testing.T) {
	m, store := kindsSetup(t)

	if _, ok := dirEntry(t, store, "alice"); !ok {
		t.Fatal("alice not registered")
	}

	_, j := planAndApply(t, m, KindPurge, "alice", nil)

	var cp purgeCheckpoint
	if err := json.Unmarshal(j.Checkpoint, &cp); err != nil || cp.Phase != 5 {
		t.Fatalf("checkpoint %+v %v", cp, err)
	}

	if _, ok := dirEntry(t, store, "alice"); ok {
		t.Fatal("entry kept after the purge")
	}

	if ok, _ := store.HasRegistry("alice"); ok {
		t.Fatal("registry kept after the purge")
	}

	if _, ok := dirEntry(t, store, "bob"); !ok {
		t.Fatal("bob lost his entry")
	}
}

// submitPurge plans and submits a purge of account without running it.
func submitPurge(t *testing.T, m *Manager, account string) *Job {
	t.Helper()

	k, _ := m.Kind(KindPurge)

	p, err := m.CreatePlan(context.Background(), k, account, nil, "tester", nil)
	if err != nil {
		t.Fatal(err)
	}

	j, _, err := m.Submit(k, account, nil, p.ID, "tester", "")
	if err != nil {
		t.Fatal(err)
	}

	return j
}

// purgeAndDie lets server m claim the purge job, take the lock, mark the
// account, delete its namespace, checkpoint phase and die.
func purgeAndDie(t *testing.T, m *Manager, store *memstore.Store, j *Job, phase int) {
	t.Helper()

	claimed, gen, err := m.claim(j.ID, nil)
	if err != nil {
		t.Fatal(err)
	}

	u, _ := store.User(j.Account)
	stale := &Run{Job: claimed, m: m, gen: gen, User: u, Env: m.env}

	if err := lockAccount(u, j.ID, time.Minute); err != nil {
		t.Fatal(err)
	}

	if err := purgeObjects(stale, purgeSummary{}, &purgeCheckpoint{}); err != nil {
		t.Fatal(err)
	}

	if err := stale.Checkpoint(purgeCheckpoint{Phase: phase}); err != nil {
		t.Fatal(err)
	}

	if _, err := m.st.updateJob(j.ID, func(cur *Job) error { cur.Lease.Until = time.Now().Add(-time.Second); return nil }); err != nil {
		t.Fatal(err)
	}
}

// TestPurgeRestartAfterRegistryDeleted: a server dies after it deleted
// the registry. The successor's maintenance lock creates an empty
// registry again (its own session passes the purging state); the last
// phase deletes it and the entry, so no ghost account stays. Until then,
// late writers are refused.
func TestPurgeRestartAfterRegistryDeleted(t *testing.T) {
	// Before and after the checkpoint of purgeObjects.
	for name, phase := range map[string]int{"before checkpoint": 3, "after checkpoint": 4} {
		t.Run(name, func(t *testing.T) {
			store, kv := memstore.New(), memstore.NewAdminKV()
			alice, _ := store.User("alice")
			_ = alice.EnsureMailbox("INBOX", nil)

			a := newTestManager(kv, store, "pod-a", Kinds(time.Hour)...)
			j := submitPurge(t, a, "alice")
			purgeAndDie(t, a, store, j, phase)

			if e, ok := dirEntry(t, store, "alice"); !ok || e.State != model.AccountPurging || e.Session != lockSession(j.ID) {
				t.Fatalf("entry %+v %v", e, ok)
			}

			late, _ := store.User("alice")
			if _, err := late.BeginDelivery(); !errors.Is(err, model.ErrImportLocked) {
				t.Fatalf("late delivery: %v", err)
			}

			b := newTestManager(kv, store, "pod-b", Kinds(time.Hour)...)
			runManager(t, b)

			if done := waitState(t, b, j.ID, StateSucceeded); done.Lease.Owner != "pod-b" {
				t.Fatalf("%+v", done)
			}

			if ok, _ := store.HasRegistry("alice"); ok {
				t.Fatal("ghost registry after the restarted purge")
			}

			if _, ok := dirEntry(t, store, "alice"); ok {
				t.Fatal("entry kept after the restarted purge")
			}
		})
	}
}

// expectRepair repairs the account and checks the action.
func expectRepair(t *testing.T, kv model.AdminKV, store *memstore.Store, account, want string) {
	t.Helper()

	rep, err := RepairAccountEntry(kv, store, account)
	if err != nil || rep.Action != want {
		t.Fatalf("%s: %+v %v, want %s", account, rep, err, want)
	}
}

// TestRepairAfterFailedPurge: an entry left purging by a purge that no
// longer runs is removed without registry and activated with one; while
// the purge runs, it stays.
func TestRepairAfterFailedPurge(t *testing.T) {
	store, kv := memstore.New(), memstore.NewAdminKV()
	m := newTestManager(kv, store, "pod-a", Kinds(time.Hour)...)

	for _, name := range []string{"gone", "back"} {
		u, _ := store.User(name)
		_ = u.EnsureMailbox("INBOX", nil)
	}

	j := submitPurge(t, m, "gone")
	session := lockSession(j.ID)

	gone, _ := store.User("gone")
	_ = gone.(model.DirectoryPurger).MarkPurging(session)
	_, _ = gone.(model.NamespacePurger).PurgeNamespace(func() error { return nil })

	back, _ := store.User("back")
	_ = back.(model.DirectoryPurger).MarkPurging(session)

	// The job is queued: a purge still runs.
	expectRepair(t, kv, store, "gone", model.RepairRunning)
	expectRepair(t, kv, store, "back", model.RepairRunning)

	if _, err := m.st.updateJob(j.ID, func(cur *Job) error { cur.State = StateFailed; return nil }); err != nil {
		t.Fatal(err)
	}

	expectRepair(t, kv, store, "gone", model.RepairRemoved)

	if _, ok := dirEntry(t, store, "gone"); ok {
		t.Fatal("entry of gone kept")
	}

	expectRepair(t, kv, store, "back", model.RepairActivated)

	if e, ok := dirEntry(t, store, "back"); !ok || e.State != model.AccountActive {
		t.Fatalf("back: %+v %v", e, ok)
	}

	expectRepair(t, kv, store, "back", model.RepairNone)
}

// TestSecondPurgeFailsFastWhenLeftPurging: a purge that ended after it
// deleted the registry leaves the account purging; another exclusive
// job cannot take the lock and says what to do instead of waiting.
func TestSecondPurgeFailsFastWhenLeftPurging(t *testing.T) {
	store, kv := memstore.New(), memstore.NewAdminKV()
	alice, _ := store.User("alice")
	_ = alice.EnsureMailbox("INBOX", nil)

	m := newTestManager(kv, store, "pod-a", Kinds(time.Hour)...)
	first := submitPurge(t, m, "alice")
	purgeAndDie(t, m, store, first, 4)

	if _, err := m.st.updateJob(first.ID, func(cur *Job) error { cur.State = StateFailed; return nil }); err != nil {
		t.Fatal(err)
	}

	k := &stepKind{name: "lock", exclusive: true, steps: 1}
	m2 := newTestManager(kv, store, "pod-b", k)
	j, _, _ := m2.Submit(k, "alice", nil, "", "tester", "")
	runManager(t, m2)

	done := waitState(t, m2, j.ID, StateFailed)
	if len(done.Errors) == 0 || done.Errors[0] != ErrLeftPurging.Error() {
		t.Fatalf("errors %v", done.Errors)
	}
}

func TestPurgeRunning(t *testing.T) {
	kv := memstore.NewAdminKV()

	for session, want := range map[string]bool{"": false, "import-1": false, "job-unknown": false} {
		if got, err := PurgeRunning(kv, session); err != nil || got != want {
			t.Errorf("%q: %v %v", session, got, err)
		}
	}
}
