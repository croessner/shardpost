package memstore

import (
	"errors"
	"testing"
	"time"

	"shardpost/internal/config"
	"shardpost/internal/model"
	"shardpost/internal/quota"
)

const lockMessage = "Subject: hi\r\n\r\nbody\r\n"

// lockedUser is a user with INBOX, Archive (locked by job-1) and
// Archive/old (locked too), and one message in each.
func lockedUser(t *testing.T) (model.User, *model.Meta) {
	t.Helper()

	store := New()
	u, _ := store.User("alice")

	for _, n := range []string{"INBOX", "Archive", "Archive/old", "Other"} {
		if err := u.EnsureMailbox(n, nil); err != nil {
			t.Fatal(err)
		}
	}

	m, err := u.Deliver("Archive", []byte(lockMessage), nil, time.Now())
	if err != nil {
		t.Fatal(err)
	}

	if _, err := u.Deliver("INBOX", []byte(lockMessage), nil, time.Now()); err != nil {
		t.Fatal(err)
	}

	a, _ := u.Lookup("Archive")
	o, _ := u.Lookup("Archive/old")

	targets := []model.MailboxLockTarget{{ID: o.ID, Name: o.Name}, {ID: a.ID, Name: a.Name}}
	if err := u.(model.MailboxLocker).LockMailboxes("job-1", targets, time.Minute); err != nil {
		t.Fatal(err)
	}

	return u, m
}

// TestMailboxLockRefusals: every write into a locked mailbox, under it
// or out of it is refused with model.ErrMailboxLocked; INBOX goes on.
func TestMailboxLockRefusals(t *testing.T) {
	u, m := lockedUser(t)
	inbox, _ := u.Lookup("INBOX")

	im, _ := u.ListMessages(inbox.ID)

	cases := map[string]func() error{
		"deliver": func() error { _, err := u.Deliver("Archive", []byte(lockMessage), nil, time.Now()); return err },
		"copy":    func() error { _, err := u.Copy(im, "Archive"); return err },
		"move in": func() error { _, err := u.Move("INBOX", im, "Archive"); return err },
		"move out": func() error {
			_, err := u.Move("Archive", []model.Meta{*m}, "Other")
			return err
		},
		"expunge": func() error { return u.Expunge("Archive", []model.Meta{*m}) },
		"flags": func() error {
			_, err := u.SetFlags("Archive", []uint32{m.UID}, 0, func([]string) []string { return []string{`\Seen`} })
			return err
		},
		"create under": func() error { return u.CreateMailbox("Archive/new", nil) },
		"rename":       func() error { return u.RenameMailbox("Archive", "Elsewhere") },
		"rename under": func() error { return u.RenameMailbox("Other", "Archive/moved") },
		"delete":       func() error { return u.DeleteMailbox("Archive") },
	}

	for name, fn := range cases {
		if err := fn(); !errors.Is(err, model.ErrMailboxLocked) {
			t.Errorf("%s: %v, want ErrMailboxLocked", name, err)
		}
	}

	if _, err := u.Deliver("INBOX", []byte(lockMessage), nil, time.Now()); err != nil {
		t.Errorf("INBOX refused: %v", err)
	}

	// The rollback of a delivery removes its own copy regardless.
	if err := u.(model.OwnExpunger).ExpungeOwn("Archive", []model.Meta{*m}); err != nil {
		t.Errorf("ExpungeOwn: %v", err)
	}

	r := u.(model.MailboxLockReader)
	if err := r.LockedMailbox("Archive"); !errors.Is(err, model.ErrMailboxLocked) {
		t.Errorf("LockedMailbox: %v", err)
	}

	if err := r.LockedMailbox("INBOX"); err != nil {
		t.Errorf("LockedMailbox INBOX: %v", err)
	}

	if err := r.LockedMailbox("Nope"); !errors.Is(err, model.ErrNoMailbox) {
		t.Errorf("LockedMailbox missing: %v", err)
	}
}

// TestMailboxLockSessions: the same session locks again, another is
// refused, an unregistration needs the lock, the unlock frees all.
func TestMailboxLockSessions(t *testing.T) {
	u, _ := lockedUser(t)
	ml := u.(model.MailboxLocker)
	a, _ := u.Lookup("Archive")

	targets := []model.MailboxLockTarget{{ID: a.ID, Name: a.Name}}

	if err := ml.LockMailboxes("job-1", targets, time.Minute); err != nil {
		t.Fatalf("same session: %v", err)
	}

	if err := ml.LockMailboxes("job-2", targets, time.Minute); !errors.Is(err, model.ErrMailboxLocked) {
		t.Fatalf("other session: %v", err)
	}

	if err := ml.LockMailboxes("job-2", []model.MailboxLockTarget{{ID: "nope", Name: "Other"}}, time.Minute); !errors.Is(err, model.ErrNoMailbox) {
		t.Fatalf("wrong id: %v", err)
	}

	if _, err := ml.UnregisterForDelete(a.ID, "job-2"); err == nil {
		t.Fatal("unregistered under another session's lock")
	}

	if done, err := ml.UnregisterForDelete(a.ID, "job-1"); err != nil || !done {
		t.Fatalf("own session: %v %v", done, err)
	}

	if err := ml.UnlockMailboxes("job-1"); err != nil {
		t.Fatal(err)
	}

	if err := u.CreateMailbox("Archive/old/x", nil); err != nil {
		t.Fatalf("still locked after the unlock: %v", err)
	}
}

// TestMailboxLockPhases: a lock before the phase destroying keeps an
// online recount from starting; destroying does not.
func TestMailboxLockPhases(t *testing.T) {
	u, _ := lockedUser(t)
	ml := u.(model.MailboxLocker)
	rc := u.(model.QuotaOnlineRecounter)

	if err := ml.SetMailboxLockPhase("job-1", model.MailboxLockUnregistering); err != nil {
		t.Fatal(err)
	}

	if _, err := rc.RecountQuotaOnline(model.QuotaRecountRequest{Tok: "job-9/1"}); err == nil {
		t.Fatal("an online recount started beside a mailbox lock")
	}

	if err := ml.SetMailboxLockPhase("job-1", model.MailboxLockDestroying); err != nil {
		t.Fatal(err)
	}

	if _, err := rc.RecountQuotaOnline(model.QuotaRecountRequest{Tok: "job-9/1"}); err != nil {
		t.Fatalf("destroying blocks the recount: %v", err)
	}

	if err := ml.SetMailboxLockPhase("job-none", model.MailboxLockDestroying); err == nil {
		t.Fatal("a phase set on a lock that is not there")
	}
}

// ruledUser is a user with a rule that ignores Trash, one message in
// Trash and one in INBOX; it returns the usage the INBOX message makes.
func ruledUser(t *testing.T) (model.User, model.Quota) {
	t.Helper()

	store := New()
	store.SetQuotaRules(quota.NewPolicy(config.Quota{Rules: []config.QuotaRule{{Mailbox: "Trash", Ignore: true}}}))

	u, _ := store.User("alice")
	for _, n := range []string{"INBOX", "Trash"} {
		if err := u.EnsureMailbox(n, nil); err != nil {
			t.Fatal(err)
		}
	}

	if _, err := u.Deliver("Trash", []byte(lockMessage), nil, time.Now()); err != nil {
		t.Fatal(err)
	}

	m, err := u.Deliver("INBOX", []byte(lockMessage), nil, time.Now())
	if err != nil {
		t.Fatal(err)
	}

	return u, model.Quota{Bytes: m.Size, Msgs: 1}
}

// TestRecountWithRules: a mailbox a rule ignores counts nowhere, in the
// usage and in the offline recount.
func TestRecountWithRules(t *testing.T) {
	u, want := ruledUser(t)

	if usage, _ := u.QuotaUsage(); usage != want {
		t.Fatalf("usage %+v, want %+v", usage, want)
	}

	if counted, stored, err := u.(model.QuotaRecounter).RecountQuota(false); err != nil || counted != want || stored != want {
		t.Fatalf("recount %+v %+v %v", counted, stored, err)
	}
}

// TestRecountOnlineMemstore: the online recount counts with the rules
// and reports the online mode; under an import lock it is busy.
func TestRecountOnlineMemstore(t *testing.T) {
	u, want := ruledUser(t)
	rc := u.(model.QuotaOnlineRecounter)

	rep, err := rc.RecountQuotaOnline(model.QuotaRecountRequest{Tok: "job-1/1"})
	if err != nil || rep.Counted != want || rep.Result != model.QuotaRecountOK || rep.Mode != model.QuotaRecountOnline {
		t.Fatalf("online recount %+v %v", rep, err)
	}

	if _, err := u.(model.Importer).ImportLock("import-1", time.Minute); err != nil {
		t.Fatal(err)
	}

	var busy *model.QuotaRecountBusyError
	if _, err := rc.RecountQuotaOnline(model.QuotaRecountRequest{Tok: "job-1/2"}); !errors.As(err, &busy) || busy.Reason != model.BusyImportLock {
		t.Fatalf("under an import lock: %v", err)
	}
}
