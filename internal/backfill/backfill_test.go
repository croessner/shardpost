package backfill

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"shardpost/internal/memstore"
	"shardpost/internal/model"
)

// legacyStore has accounts whose registries predate the directory: their
// entries are removed after they were created.
func legacyStore(t *testing.T, names ...string) (*memstore.Store, model.SortedIndex) {
	t.Helper()

	store := memstore.New()

	for _, name := range names {
		u, _ := store.User(name)
		if err := u.EnsureMailbox("INBOX", nil); err != nil {
			t.Fatal(err)
		}
	}

	ix, _, _ := store.AccountIndex()

	for _, name := range names {
		if err := ix.Remove(name); err != nil {
			t.Fatal(err)
		}
	}

	return store, ix
}

func count(t *testing.T, ix model.SortedIndex) int {
	t.Helper()

	n, cursor := 0, ""

	for {
		page, next, err := ix.List(cursor, 7)
		if err != nil {
			t.Fatal(err)
		}

		n += len(page)

		if next == "" {
			return n
		}

		cursor = next
	}
}

func TestBackfillEntersAccountsOnce(t *testing.T) {
	store, ix := legacyStore(t, "alice", "bob", model.PublicNamespace)

	// Namespaces without registry and the store's own are not entered.
	_, _ = store.User("noregistry")
	srv, _ := store.User(model.ServerNamespace)
	_ = srv.EnsureMailbox("INBOX", nil)

	rep, err := Accounts(store, memstore.NewAdminKV(), Options{})
	if err != nil {
		t.Fatal(err)
	}

	if rep.Accounts != 3 || rep.Added != 3 || count(t, ix) != 3 {
		t.Fatalf("report %+v, %d entries", rep, count(t, ix))
	}

	if e, ok, _ := model.GetAccountEntry(ix, "alice"); !ok || e.State != model.AccountActive || e.Created.IsZero() {
		t.Fatalf("entry %+v %v", e, ok)
	}

	checkMarker(t, ix, 3)

	// Again: nothing to add, the entries stay as they are.
	before, _ := ix.Get("alice")

	rep, err = Accounts(store, memstore.NewAdminKV(), Options{})
	if err != nil || rep.Added != 0 || rep.Accounts != 3 {
		t.Fatalf("second run %+v %v", rep, err)
	}

	if after, _ := ix.Get("alice"); string(after) != string(before) {
		t.Fatalf("entry rewritten: %s -> %s", before, after)
	}
}

// checkMarker checks the completion marker of a finished backfill.
func checkMarker(t *testing.T, ix model.SortedIndex, added int) {
	t.Helper()

	h, err := ix.Head()
	if err != nil {
		t.Fatal(err)
	}

	var marker Report
	if err := json.Unmarshal([]byte(h.Markers[MarkerAccounts]), &marker); err != nil || marker.Finished.IsZero() || marker.Added != added {
		t.Fatalf("marker %q %v", h.Markers[MarkerAccounts], err)
	}

	if h.Markers[markerProgress] != "" {
		t.Fatalf("progress left: %q", h.Markers[markerProgress])
	}
}

func TestBackfillResumes(t *testing.T) {
	names := make([]string, 0, 1200)
	for i := 0; i < 1200; i++ {
		names = append(names, fmt.Sprintf("user%04d", i))
	}

	store, ix := legacyStore(t, names...)

	// An interrupted run got as far as user0599.
	started := time.Date(2026, 10, 1, 8, 0, 0, 0, time.UTC)
	b, _ := json.Marshal(progress{Started: started, After: "user0599"})

	if err := ix.SetMarker(markerProgress, string(b)); err != nil {
		t.Fatal(err)
	}

	rep, err := Accounts(store, memstore.NewAdminKV(), Options{})
	if err != nil {
		t.Fatal(err)
	}

	if !rep.Resumed || !rep.Started.Equal(started) || rep.Added != 600 {
		t.Fatalf("resumed run %+v", rep)
	}

	if _, ok, _ := model.GetAccountEntry(ix, "user0599"); ok {
		t.Fatal("a name before the resume position was entered")
	}

	// --restart starts over and enters the rest.
	rep, err = Accounts(store, memstore.NewAdminKV(), Options{Restart: true})
	if err != nil || rep.Resumed || rep.Added != 600 || count(t, ix) != 1200 {
		t.Fatalf("restart %+v %v (%d entries)", rep, err, count(t, ix))
	}
}

func TestBackfillRepairs(t *testing.T) {
	store, ix := legacyStore(t)

	for _, name := range []string{"gone", "back"} {
		u, _ := store.User(name)
		_ = u.EnsureMailbox("INBOX", nil)
		_ = u.(model.DirectoryPurger).MarkPurging("job-20261001T000000Z-000000000000")
	}

	gone, _ := store.User("gone")
	_, _ = gone.(model.NamespacePurger).PurgeNamespace(func() error { return nil })

	// No such job: the purges do not run.
	rep, err := Accounts(store, memstore.NewAdminKV(), Options{})
	if err != nil {
		t.Fatal(err)
	}

	if rep.Repaired[model.RepairRemoved] != 1 || rep.Repaired[model.RepairActivated] != 1 {
		t.Fatalf("repairs %+v", rep.Repaired)
	}

	if _, ok, _ := model.GetAccountEntry(ix, "gone"); ok {
		t.Fatal("entry of a purged account kept")
	}

	if e, _, _ := model.GetAccountEntry(ix, "back"); e.State != model.AccountActive {
		t.Fatalf("back: %+v", e)
	}
}

func TestBackfillStopsWhenLockLost(t *testing.T) {
	store, ix := legacyStore(t, "alice")

	if _, err := Accounts(store, memstore.NewAdminKV(), Options{Lost: func() bool { return true }}); err != ErrLockLost {
		t.Fatalf("lost lock: %v", err)
	}

	if _, ok, _ := model.GetAccountEntry(ix, "alice"); ok {
		t.Fatal("wrote without the lock")
	}
}
