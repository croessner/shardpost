package memstore

import (
	"errors"
	"sync"
	"testing"
	"time"

	"shardpost/internal/model"
)

func dirEntry(t *testing.T, s *Store, account string) (model.AccountEntry, bool) {
	t.Helper()

	ix, release, _ := s.AccountIndex()
	defer release()

	e, ok, err := model.GetAccountEntry(ix, account)
	if err != nil {
		t.Fatal(err)
	}

	return e, ok
}

func TestUserRejectsInvalidNamespaces(t *testing.T) {
	s := New()

	for _, bad := range []string{"", "~jmap", "~"} {
		if _, err := s.User(bad); !errors.Is(err, model.ErrNamespace) {
			t.Errorf("User(%q): %v", bad, err)
		}
	}

	for _, ok := range []string{"alice", "~public", "~server", "~admin", "~learning", "~accounts"} {
		if _, err := s.User(ok); err != nil {
			t.Errorf("User(%q): %v", ok, err)
		}
	}
}

// TestFirstMutationRegisters: opening a user registers nothing; the
// first mutation that creates a registry does, once.
func TestFirstMutationRegisters(t *testing.T) {
	s := New()
	u, _ := s.User("alice")

	if _, ok := dirEntry(t, s, "alice"); ok {
		t.Fatal("registered on open")
	}

	writes := s.IndexBackend(model.AccountsNamespace).Writes.Load()

	if err := u.CreateMailbox("INBOX", nil); err != nil {
		t.Fatal(err)
	}

	e, ok := dirEntry(t, s, "alice")
	if !ok || e.State != model.AccountActive || e.Created.IsZero() {
		t.Fatalf("entry %+v %v", e, ok)
	}

	after := s.IndexBackend(model.AccountsNamespace).Writes.Load()

	for _, name := range []string{"Sent", "Drafts"} {
		_ = u.CreateMailbox(name, nil)
	}

	if _, err := u.Deliver("INBOX", []byte("Subject: x\r\n\r\nx\r\n"), nil, time.Now()); err != nil {
		t.Fatal(err)
	}

	if got := s.IndexBackend(model.AccountsNamespace).Writes.Load(); got != after || after-writes != 2 {
		t.Fatalf("directory writes: %d for the first mutation (head and entry), %d after", after-writes, got-after)
	}
}

func TestConcurrentCreatorsRegisterOnce(t *testing.T) {
	s := New()

	var wg sync.WaitGroup

	for i := 0; i < 16; i++ {
		wg.Add(1)

		go func() {
			defer wg.Done()

			u, _ := s.User("carol")
			if err := u.EnsureMailbox("INBOX", nil); err != nil {
				t.Error(err)
			}
		}()
	}

	wg.Wait()

	if _, ok := dirEntry(t, s, "carol"); !ok {
		t.Fatal("not registered")
	}

	ix, release, _ := s.AccountIndex()
	defer release()

	page, _, err := ix.List("", 10)
	if err != nil || len(page) != 1 {
		t.Fatalf("entries %v %v", page, err)
	}
}

func TestReservedNamesNotRegistered(t *testing.T) {
	s := New()

	for _, name := range []string{model.ServerNamespace, model.AdminNamespace, model.LearningNamespace} {
		u, _ := s.User(name)
		if err := u.CreateMailbox("INBOX", nil); err != nil {
			t.Fatal(err)
		}

		if _, ok := dirEntry(t, s, name); ok {
			t.Errorf("%s registered", name)
		}
	}

	pub, _ := s.User(model.PublicNamespace)
	if err := pub.CreateMailbox("Team", nil); err != nil {
		t.Fatal(err)
	}

	if _, ok := dirEntry(t, s, model.PublicNamespace); !ok {
		t.Error("~public not registered")
	}
}

// purgedWithoutRegistry leaves alice as the purge job does between
// deleting her namespace and removing her entry.
func purgedWithoutRegistry(t *testing.T, s *Store) model.User {
	t.Helper()

	u, _ := s.User("alice")
	if err := u.CreateMailbox("INBOX", nil); err != nil {
		t.Fatal(err)
	}

	if err := u.(model.DirectoryPurger).MarkPurging("job-1"); err != nil {
		t.Fatal(err)
	}

	if _, err := u.(model.NamespacePurger).PurgeNamespace(func() error { return nil }); err != nil {
		t.Fatal(err)
	}

	if ok, _ := s.HasRegistry("alice"); ok {
		t.Fatal("registry survived the purge")
	}

	return u
}

func TestPurgingRefusesLateWriters(t *testing.T) {
	s := New()
	purgedWithoutRegistry(t, s)

	late, _ := s.User("alice")

	if _, err := late.BeginDelivery(); !errors.Is(err, model.ErrImportLocked) {
		t.Errorf("late delivery: %v", err)
	}

	if err := late.CreateMailbox("INBOX", nil); !errors.Is(err, model.ErrImportLocked) {
		t.Errorf("late session: %v", err)
	}

	imp := late.(model.Importer)

	if _, err := imp.ImportLock("import-1", time.Minute); !errors.Is(err, model.ErrImportLocked) {
		t.Errorf("another session's import lock: %v", err)
	}

	if ok, _ := s.HasRegistry("alice"); ok {
		t.Fatal("a refused writer created the registry")
	}

	// The purge job's own session (a restart) passes.
	if _, err := imp.ImportLock("job-1", time.Minute); err != nil {
		t.Fatalf("the purge's session: %v", err)
	}

	if err := late.(model.DirectoryPurger).FinishPurge(); err != nil {
		t.Fatal(err)
	}

	if ok, _ := s.HasRegistry("alice"); ok {
		t.Fatal("ghost registry after the purge")
	}

	if _, ok := dirEntry(t, s, "alice"); ok {
		t.Fatal("entry kept")
	}

	// From now on the name is free for a new account.
	if err := late.CreateMailbox("INBOX", nil); err != nil {
		t.Fatalf("new account under the old name: %v", err)
	}

	if e, ok := dirEntry(t, s, "alice"); !ok || e.State != model.AccountActive {
		t.Fatalf("new entry %+v %v", e, ok)
	}
}

func TestGrantToPurgingAccountRefused(t *testing.T) {
	s := New()
	bob, _ := s.User("bob")
	_ = bob.CreateMailbox("Bobs", nil)
	_ = bob.SetACL("Bobs", "alice", "lr")

	purgedWithoutRegistry(t, s)

	if err := bob.SetACL("Bobs", "alice", "lrw"); !errors.Is(err, model.ErrImportLocked) {
		t.Fatalf("SETACL: %v", err)
	}

	mod := bob.(model.ACLModifier)

	if _, _, err := mod.ModifyACL("Bobs", "alice", -1, func(string) string { return "lrs" }); !errors.Is(err, model.ErrImportLocked) {
		t.Fatalf("ModifyACL: %v", err)
	}

	if acl, _ := bob.GetACL("Bobs"); acl["alice"] != "lr" {
		t.Fatalf("ACL written: %v", acl)
	}

	// A removal goes through and writes no pointer.
	if _, _, err := mod.ModifyACL("Bobs", "alice", -1, func(string) string { return "" }); err != nil {
		t.Fatalf("revoke: %v", err)
	}

	if acl, _ := bob.GetACL("Bobs"); acl["alice"] != "" {
		t.Fatalf("not revoked: %v", acl)
	}

	if ok, _ := s.HasRegistry("alice"); ok {
		t.Fatal("the revoke created the registry")
	}

	// Others are not affected.
	if err := bob.SetACL("Bobs", "carol", "lr"); err != nil {
		t.Fatalf("grant to an active account: %v", err)
	}
}
