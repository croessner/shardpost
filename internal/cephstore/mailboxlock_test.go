//go:build tentacle

package cephstore

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"shardpost/internal/model"
)

// Tests of the mailbox lock of mailbox_delete (stage B17, temp/16 §13).

// lockedSetup is a user with INBOX, Archive (one message on shard 0,
// more on shard 1), Archive/old, Other and Trash (ignored), Archive and
// Archive/old locked by job-1.
func lockedSetup(t *testing.T) (*User, []Meta) {
	t.Helper()

	u := testUser(t, recountStore(t))

	for _, n := range []string{"INBOX", "Archive", "Archive/old", "Other", "Trash"} {
		mustMailbox(t, u, n)
	}

	metas := deliverN(t, u, "Archive", 6, 300)
	deliverN(t, u, "INBOX", 2, 300)

	a, _ := u.Lookup("Archive")
	o, _ := u.Lookup("Archive/old")

	if err := u.LockMailboxes("job-1", []model.MailboxLockTarget{{ID: o.ID, Name: o.Name}, {ID: a.ID, Name: a.Name}}, time.Minute); err != nil {
		t.Fatal(err)
	}

	return u, metas
}

// expectErr runs every case and expects want from each.
func expectErr(t *testing.T, cases map[string]func() error, want error) {
	t.Helper()

	for name, fn := range cases {
		if err := fn(); !errors.Is(err, want) {
			t.Errorf("%s: %v, want %v", name, err, want)
		}
	}
}

// TestMailboxLockStoreRefusals: every write into, under or out of a
// locked mailbox is refused, exactly where a registry CAS carries the
// write and from the record elsewhere; INBOX and the own rollback are
// not; the account lock is refused while the lock is before destroying.
func TestMailboxLockStoreRefusals(t *testing.T) {
	u, metas := lockedSetup(t)
	w := view(t, u) // a fresh lookup cache

	inbox, _ := w.Lookup("INBOX")
	im, _ := w.ListMessages(inbox.ID)

	expectErr(t, map[string]func() error{
		"deliver":      func() error { _, err := w.Deliver("Archive", testMessage("x", 300), nil, time.Time{}); return err },
		"copy":         func() error { _, err := w.Copy(im[:1], "Archive"); return err },
		"move in":      func() error { _, err := w.Move("INBOX", im[:1], "Archive"); return err },
		"move out":     func() error { _, err := w.Move("Archive", metas[:1], "Other"); return err },
		"move ignored": func() error { _, err := w.Move("Archive", metas[:1], "Trash"); return err },
		"expunge":      func() error { return w.Expunge("Archive", metas[:1]) },
		"flags": func() error {
			_, err := w.SetFlags("Archive", []uint32{metas[0].UID}, 0, func([]string) []string { return []string{`\Seen`} })
			return err
		},
		"create under": func() error { return w.CreateMailbox("Archive/new", nil) },
		"rename":       func() error { return w.RenameMailbox("Archive", "Elsewhere") },
		"rename under": func() error { return w.RenameMailbox("Other", "Archive/moved") },
		"delete":       func() error { return w.DeleteMailbox("Archive") },
		"probe":        func() error { return w.LockedMailbox("Archive") },
	}, model.ErrMailboxLocked)

	expectErr(t, map[string]func() error{
		"import lock": func() error { _, err := w.ImportLock("imp", time.Minute); return err },
	}, model.ErrImportLocked)

	expectErr(t, map[string]func() error{
		"INBOX":       func() error { _, err := w.Deliver("INBOX", testMessage("y", 300), nil, time.Time{}); return err },
		"ExpungeOwn":  func() error { return w.ExpungeOwn("Archive", metas[5:]) },
		"probe INBOX": func() error { return w.LockedMailbox("INBOX") },
	}, nil)
}

// TestMailboxLockRecordWriters: a subscription, a sort order and an ACL
// change carry the lock over; another session may not lock the
// targets; a renewal keeps them; destroying frees the account lock; the
// unlock frees the mailboxes.
func TestMailboxLockRecordWriters(t *testing.T) {
	u, _ := lockedSetup(t)
	w := view(t, u)

	expectErr(t, map[string]func() error{
		"subscribe": func() error { return w.SetSubscribed("Archive", true) },
		"sortorder": func() error { return w.SetSortOrder("Archive", 7) },
		"acl":       func() error { return w.SetACL("Archive", "bob", "lr") },
	}, nil)

	w.invalidate()

	if mb, _ := w.Lookup("Archive"); !mb.Lock.Held("", time.Now()) || !mb.Subscribed || mb.SortOrder != 7 {
		t.Fatalf("record writers lost the lock: %+v", mb.MailboxRec)
	}

	a, _ := w.Lookup("Archive")
	if err := w.LockMailboxes("job-2", []model.MailboxLockTarget{{ID: a.ID, Name: a.Name}}, time.Minute); !errors.Is(err, model.ErrMailboxLocked) {
		t.Fatalf("second session: %v", err)
	}

	expectErr(t, map[string]func() error{
		"renew":      func() error { return u.RenewMailboxLocks("job-1", 2*time.Minute) },
		"destroying": func() error { return u.SetMailboxLockPhase("job-1", model.MailboxLockDestroying) },
	}, nil)

	if _, err := w.ImportLock("imp", time.Minute); err != nil {
		t.Fatalf("import lock while destroying: %v", err)
	}

	_ = w.ImportUnlock("imp")

	if err := u.UnlockMailboxes("job-1"); err != nil {
		t.Fatal(err)
	}

	w.invalidate()

	if err := w.CreateMailbox("Archive/new", nil); err != nil {
		t.Fatalf("after the unlock: %v", err)
	}
}

// stripLock rewrites the record of name without the lock field, as a
// server before 0.52.0 does.
func stripLock(t *testing.T, u *User, name string) {
	t.Helper()

	err := casOmap(u, u.idx, oidRegistry, []string{regKey(name)}, func(cur map[string][]byte) (map[string][]byte, []string) {
		var rec struct {
			ID          string `json:"id"`
			UIDValidity uint32 `json:"uidvalidity"`
		}

		_ = json.Unmarshal(cur[regKey(name)], &rec)

		return map[string][]byte{regKey(name): encode(rec)}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestMailboxLockUnregister: the unregistration needs the session's
// lock (a record rewritten without the field aborts it fail safe); it
// retires the header, and the id is not reused while the tombstone
// lives.
func TestMailboxLockUnregister(t *testing.T) {
	u, _ := lockedSetup(t)
	a, _ := u.Lookup("Archive")
	o, _ := u.Lookup("Archive/old")

	if _, err := u.UnregisterForDelete(a.ID, "job-2"); err == nil {
		t.Fatal("unregistered under another session")
	}

	stripLock(t, u, "Archive/old")

	if _, err := u.UnregisterForDelete(o.ID, "job-1"); err == nil {
		t.Fatal("unregistered without the lock")
	}

	if _, err := u.Lookup("Archive/old"); err != nil {
		t.Fatal("deleted without the lock")
	}

	if done, err := u.UnregisterForDelete(a.ID, "job-1"); err != nil || !done {
		t.Fatalf("own session: %v %v", done, err)
	}

	if _, err := u.Lookup("Archive"); !errors.Is(err, ErrNoMailbox) {
		t.Fatal("still registered")
	}

	// The header is retired: no late delivery links.
	if _, _, err := u.listRange(a.ID); !errors.Is(err, ErrNoMailbox) {
		t.Fatalf("header not retired: %v", err)
	}

	// The id is not reused while the tombstone lives.
	if err := u.createIntended(&MailboxRec{ID: a.ID, UIDValidity: 1}, "Again", 4); !errors.Is(err, errIDDeleting) {
		t.Fatalf("id reused: %v", err)
	}
}

// TestMailboxLockFinishDeletion: the destruction from the tombstone
// releases blobs and usage, is idempotent, and a gone mailbox
// unregisters to nothing.
func TestMailboxLockFinishDeletion(t *testing.T) {
	u, metas := lockedSetup(t)
	a, _ := u.Lookup("Archive")

	if done, err := u.UnregisterForDelete(a.ID, "job-1"); err != nil || !done {
		t.Fatalf("unregister: %v %v", done, err)
	}

	msgs, bytes, err := u.FinishDeletion(a.ID)
	if err != nil || msgs != int64(len(metas)) || bytes != sizeOf(metas).Bytes {
		t.Fatalf("finish: %d %d %v", msgs, bytes, err)
	}

	if msgs, _, err := u.FinishDeletion(a.ID); err != nil || msgs != 0 {
		t.Fatalf("second finish: %d %v", msgs, err)
	}

	noBlobs(t, u, metas)
	exact(t, u, "after the deletion")

	if done, err := u.UnregisterForDelete(a.ID, "job-1"); err != nil || done {
		t.Fatalf("a gone mailbox: %v %v", done, err)
	}
}

// noBlobs fails when a blob of metas is still there.
func noBlobs(t *testing.T, u *User, metas []Meta) {
	t.Helper()

	for _, m := range metas {
		if blobExistsT(u, m.GUID) {
			t.Errorf("blob %s left", m.GUID)
		}
	}
}

// TestMailboxLockBarrier: the barrier waits for a reservation intent
// that names a target and for a MOVE intent that points at one or sits
// on one, whatever its age; a dead reservation intent is passed.
func TestMailboxLockBarrier(t *testing.T) {
	u, _ := lockedSetup(t)
	a, _ := u.Lookup("Archive")
	other, _ := u.Lookup("Other")

	targets := []model.MailboxLockTarget{{ID: a.ID, Name: a.Name}}
	barrier := func(dead time.Duration) string {
		reason, _, err := u.MailboxLockBarrier(targets, time.Now().Add(100*time.Millisecond), dead)
		if err != nil {
			t.Fatal(err)
		}

		return reason
	}

	if reason := barrier(time.Minute); reason != "" {
		t.Fatalf("quiet: %s", reason)
	}

	bi := blobIntentKey(newGUID())
	if err := u.setIntent(map[string][]byte{bi: u.newBlobIntent(100, a.ID)}); err != nil {
		t.Fatal(err)
	}

	if reason := barrier(time.Minute); reason != model.BusyIntent {
		t.Fatalf("reservation into the target: %s", reason)
	}

	if reason := barrier(time.Nanosecond); reason != "" {
		t.Fatalf("dead reservation: %s", reason)
	}

	_ = rmKeys(u.idx, oidRegistry, []string{bi})

	moveIntentOn(t, u, other.ID, a.ID)

	if reason := barrier(time.Nanosecond); reason != model.BusyMoveIntent {
		t.Fatalf("old move into the target: %s", reason)
	}

	_ = rmKeys(u.idx, mboxOID(other.ID), nil)

	for k := range omapKeys(t, u, mboxOID(other.ID), keyIntentPrefix) {
		_ = rmKeys(u.idx, mboxOID(other.ID), []string{k})
	}

	moveIntentOn(t, u, a.ID, other.ID)

	if reason := barrier(time.Nanosecond); reason != model.BusyMoveIntent {
		t.Fatalf("old move out of the target: %s", reason)
	}
}

// moveIntentOn writes an old MOVE intent on the mailbox src towards
// dest.
func moveIntentOn(t *testing.T, u *User, src, dest string) {
	t.Helper()

	in := moveIntent{ID: newGUID(), Dest: dest, Started: time.Now().Add(-2 * time.Hour).Unix()}

	if err := casOmap(u, u.idx, mboxOID(src), nil, func(map[string][]byte) (map[string][]byte, []string) {
		return map[string][]byte{keyIntentPrefix + in.ID: encode(in)}, nil
	}); err != nil {
		t.Fatal(err)
	}
}

// TestMailboxLockGateRace: a MOVE between counted mailboxes whose gate
// runs after the lock is aborted (nothing linked, the intent gone); the
// scrub sees a mailbox lock and skips the account.
func TestMailboxLockGateRace(t *testing.T) {
	u := testUser(t, recountStore(t))
	mustMailbox(t, u, "INBOX")
	a := mustMailbox(t, u, "Archive")
	metas := deliverN(t, u, "INBOX", 2, 300)

	mover := view(t, u)
	mover.hooks = &recountHooks{step: func(step, _ string) {
		if step == hookBeforeGate {
			if err := u.LockMailboxes("job-1", []model.MailboxLockTarget{{ID: a.ID, Name: a.Name}}, time.Minute); err != nil {
				t.Error(err)
			}
		}
	}}

	if _, err := mover.Move("INBOX", metas[:1], "Archive"); !errors.Is(err, ErrMailboxLocked) {
		t.Fatalf("move through the gate: %v", err)
	}

	if msgs, _ := u.ListMessages(a.ID); len(msgs) != 0 {
		t.Fatal("linked despite the lock")
	}

	inbox, _ := u.Lookup("INBOX")
	if intents, _ := u.pendingIntents(inbox.ID); len(intents) != 0 {
		t.Fatalf("intent left: %+v", intents)
	}

	rep, err := u.Scrub(ScrubOptions{})
	if err != nil || rep.OnlineSkipped != skipMailboxLock {
		t.Fatalf("scrub under a mailbox lock: %+v %v", rep, err)
	}

	if err := u.UnlockMailboxes("job-1"); err != nil {
		t.Fatal(err)
	}
}
