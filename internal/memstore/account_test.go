package memstore

import (
	"testing"
	"time"

	"shardpost/internal/model"
)

// openUser returns a fresh user view of a fresh store.
func openUser(t *testing.T) model.User {
	t.Helper()

	u, err := New().User("test")
	if err != nil {
		t.Fatal(err)
	}

	return u
}

// state is AccountState or a failed test.
func state(t *testing.T, u model.User) uint64 {
	t.Helper()

	s, err := u.AccountState()
	if err != nil {
		t.Fatalf("account state: %v", err)
	}

	return s
}

// changesSince is AccountChanges or a failed test.
func changesSince(t *testing.T, u model.User, since uint64) []model.AccountChange {
	t.Helper()

	cs, ok, err := u.AccountChanges(since, 100)
	if err != nil {
		t.Fatalf("account changes: %v", err)
	}

	if !ok {
		t.Fatalf("account changes since %d: the log no longer covers it", since)
	}

	return cs
}

// TestAccountLogRecordsEveryKind: a mailbox and the messages in it are
// both objects of the account log, and each change appears once, in the
// order it was made.
func TestAccountLogRecordsEveryKind(t *testing.T) {
	u := openUser(t)

	if got := state(t, u); got != 0 {
		t.Fatalf("a fresh account is at state %d, want 0", got)
	}

	if err := u.CreateMailbox("Archive", nil); err != nil {
		t.Fatal(err)
	}

	m, err := u.Deliver("Archive", []byte("Subject: one\r\n\r\nbody\r\n"), nil, time.Now())
	if err != nil {
		t.Fatal(err)
	}

	if _, err := u.SetFlags("Archive", []uint32{m.UID}, 0, func([]string) []string { return []string{`\Seen`} }); err != nil {
		t.Fatal(err)
	}

	cs := changesSince(t, u, 0)
	if len(cs) != 3 {
		t.Fatalf("the log holds %d entries, want the create, the delivery and the flag change: %+v", len(cs), cs)
	}

	want := []struct {
		kind model.AccountChangeKind
		op   model.AccountOp
	}{
		{model.KindMailbox, model.OpCreated},
		{model.KindEmail, model.OpCreated},
		{model.KindEmail, model.OpUpdated},
	}

	for i, w := range want {
		checkKind(t, i, cs[i], w.kind, w.op)
	}

	if got := cs[1].Msgs; len(got) != 1 || got[0].GUID != m.GUID || got[0].UID != m.UID {
		t.Errorf("the delivery names %+v, want the delivered message", got)
	}

	if got := state(t, u); got != 3 {
		t.Errorf("state is %d, want 3", got)
	}
}

// checkKind checks the kind, the operation and the sequence of an entry.
func checkKind(t *testing.T, i int, got model.AccountChange, kind model.AccountChangeKind, op model.AccountOp) {
	t.Helper()

	if got.Kind != kind || got.Op != op {
		t.Errorf("entry %d is %s/%s, want %s/%s", i, got.Kind, got.Op, kind, op)
	}

	if got.Seq != uint64(i+1) {
		t.Errorf("entry %d carries sequence %d, want %d", i, got.Seq, i+1)
	}
}

// TestAccountLogNamesDestroyedMessages: an expunged message is named by
// its GUID, which is the only identity left once the entry is gone.
func TestAccountLogNamesDestroyedMessages(t *testing.T) {
	u := openUser(t)

	if err := u.CreateMailbox("Trash", nil); err != nil {
		t.Fatal(err)
	}

	m, err := u.Deliver("Trash", []byte("Subject: gone\r\n\r\nbody\r\n"), nil, time.Now())
	if err != nil {
		t.Fatal(err)
	}

	before := state(t, u)

	if err := u.Expunge("Trash", []model.Meta{*m}); err != nil {
		t.Fatal(err)
	}

	cs := changesSince(t, u, before)
	if len(cs) != 1 || cs[0].Op != model.OpDestroyed {
		t.Fatalf("the expunge produced %+v, want one destroyed entry", cs)
	}

	if got := cs[0].Msgs; len(got) != 1 || got[0].GUID != m.GUID {
		t.Errorf("the destroyed entry names %+v, want the GUID %s", got, m.GUID)
	}
}

// TestAccountLogMoveNamesBothMailboxes: a move leaves the message in one
// mailbox and creates it in the other; both are objects of their own.
func TestAccountLogMoveNamesBothMailboxes(t *testing.T) {
	u := openUser(t)

	for _, n := range []string{"INBOX", "Archive"} {
		if err := u.CreateMailbox(n, nil); err != nil {
			t.Fatal(err)
		}
	}

	m, err := u.Deliver("INBOX", []byte("Subject: moving\r\n\r\nbody\r\n"), nil, time.Now())
	if err != nil {
		t.Fatal(err)
	}

	before := state(t, u)

	if _, err := u.Move("INBOX", []model.Meta{*m}, "Archive"); err != nil {
		t.Fatal(err)
	}

	cs := changesSince(t, u, before)
	if len(cs) != 2 {
		t.Fatalf("the move produced %+v, want one entry per mailbox", cs)
	}

	ops := map[model.AccountOp]bool{}
	for _, c := range cs {
		ops[c.Op] = true

		if c.Kind != model.KindEmail {
			t.Errorf("the move produced a %s entry", c.Kind)
		}
	}

	if !ops[model.OpCreated] || !ops[model.OpDestroyed] {
		t.Errorf("the move produced %+v, want a created and a destroyed entry", cs)
	}
}

// TestAccountLogRenameNamesEveryChild: a rename changes the name of the
// whole subtree, and a client that tracks mailboxes has to hear about
// each of them.
func TestAccountLogRenameNamesEveryChild(t *testing.T) {
	u := openUser(t)

	for _, n := range []string{"Work", "Work/Bills", "Work/Bills/2026"} {
		if err := u.CreateMailbox(n, nil); err != nil {
			t.Fatal(err)
		}
	}

	before := state(t, u)

	if err := u.RenameMailbox("Work", "Jobs"); err != nil {
		t.Fatal(err)
	}

	cs := changesSince(t, u, before)
	if len(cs) != 3 {
		t.Fatalf("the rename produced %d entries, want one per mailbox of the subtree: %+v", len(cs), cs)
	}

	names := map[string]bool{}
	for _, c := range cs {
		names[c.Name] = true
	}

	for _, want := range []string{"Jobs", "Jobs/Bills", "Jobs/Bills/2026"} {
		if !names[want] {
			t.Errorf("the rename did not name %q; it named %v", want, names)
		}
	}
}

// TestAccountLogUnchangedWritesNothing: a STORE that changes no flag and
// a SUBSCRIBE that is already in force are not changes.
func TestAccountLogUnchangedWritesNothing(t *testing.T) {
	u := openUser(t)

	if err := u.CreateMailbox("INBOX", nil); err != nil {
		t.Fatal(err)
	}

	m, err := u.Deliver("INBOX", []byte("Subject: quiet\r\n\r\nbody\r\n"), []string{`\Seen`}, time.Now())
	if err != nil {
		t.Fatal(err)
	}

	if err := u.SetSubscribed("INBOX", true); err != nil {
		t.Fatal(err)
	}

	before := state(t, u)

	if _, err := u.SetFlags("INBOX", []uint32{m.UID}, 0, func(old []string) []string { return old }); err != nil {
		t.Fatal(err)
	}

	if err := u.SetSubscribed("INBOX", true); err != nil {
		t.Fatal(err)
	}

	if got := state(t, u); got != before {
		t.Errorf("state moved from %d to %d although nothing changed", before, got)
	}
}

// TestAccountChangesTruncated: a token the log no longer covers is
// answered with ok=false, so the caller resynchronises instead of being
// handed a delta with a hole in it.
func TestAccountChangesTruncated(t *testing.T) {
	u := openUser(t)

	for _, n := range []string{"A", "B", "C", "D", "E", "F"} {
		if err := u.CreateMailbox(n, nil); err != nil {
			t.Fatal(err)
		}
	}

	inner, ok := u.(*view)
	if !ok {
		t.Fatalf("the store handed out a %T, not a view", u)
	}

	// As if the entries 1 to 4 had been trimmed away.
	inner.acct.logStart = 5

	if _, ok, err := u.AccountChanges(1, 10); err != nil || ok {
		t.Errorf("a token below the start of the log was accepted (ok=%v, err=%v)", ok, err)
	}

	if _, ok, err := u.AccountChanges(4, 10); err != nil || !ok {
		t.Errorf("the oldest token the log still covers was refused (ok=%v, err=%v)", ok, err)
	}

	if _, ok, err := u.AccountChanges(999, 10); err != nil || ok {
		t.Errorf("a token from the future was accepted (ok=%v, err=%v)", ok, err)
	}
}

// TestAccountChangesPaging: a caller that asks for fewer entries than
// there are gets them oldest first and continues from the last sequence.
func TestAccountChangesPaging(t *testing.T) {
	u := openUser(t)

	for _, n := range []string{"A", "B", "C", "D"} {
		if err := u.CreateMailbox(n, nil); err != nil {
			t.Fatal(err)
		}
	}

	first, ok, err := u.AccountChanges(0, 2)
	if err != nil || !ok || len(first) != 2 {
		t.Fatalf("first page: %+v ok=%v err=%v", first, ok, err)
	}

	if first[0].Seq != 1 || first[1].Seq != 2 {
		t.Fatalf("the first page is %d,%d, want 1,2", first[0].Seq, first[1].Seq)
	}

	rest := changesSince(t, u, first[1].Seq)
	if len(rest) != 2 || rest[0].Seq != 3 {
		t.Fatalf("the rest is %+v, want the entries 3 and 4", rest)
	}
}

// TestAccountLogTrim: the log is bounded, and a token below what is left
// is refused rather than answered with a delta that has a hole in it.
func TestAccountLogTrim(t *testing.T) {
	old := accountLogRetention
	accountLogRetention = 3

	t.Cleanup(func() { accountLogRetention = old })

	u := openUser(t)

	for _, n := range []string{"A", "B", "C", "D", "E"} {
		if err := u.CreateMailbox(n, nil); err != nil {
			t.Fatal(err)
		}
	}

	cs := changesSince(t, u, 2)
	if len(cs) != 3 || cs[0].Seq != 3 {
		t.Fatalf("the log holds %+v, want the last three entries", cs)
	}

	if _, ok, err := u.AccountChanges(1, 10); err != nil || ok {
		t.Errorf("a token below the trimmed start was accepted (ok=%v, err=%v)", ok, err)
	}
}

// TestAccountLogACLChange: changing the rights of a mailbox changes what
// a client may do with it, so it is a change to the mailbox.
func TestAccountLogACLChange(t *testing.T) {
	u := openUser(t)

	if err := u.CreateMailbox("Shared", nil); err != nil {
		t.Fatal(err)
	}

	mod, ok := u.(interface {
		ModifyACL(name, identifier string, expectGen int64, change func(old string) string) (string, uint64, error)
	})
	if !ok {
		t.Fatalf("the store handed out a %T without ModifyACL", u)
	}

	before := state(t, u)

	if _, _, err := mod.ModifyACL("Shared", "other", -1, func(string) string { return "lr" }); err != nil {
		t.Fatal(err)
	}

	cs := changesSince(t, u, before)
	if len(cs) != 1 || cs[0].Kind != model.KindMailbox || cs[0].Op != model.OpUpdated {
		t.Fatalf("the rights change produced %+v, want one updated mailbox entry", cs)
	}

	before = state(t, u)

	if _, _, err := mod.ModifyACL("Shared", "other", -1, func(string) string { return "lr" }); err != nil {
		t.Fatal(err)
	}

	if got := state(t, u); got != before {
		t.Errorf("state moved from %d to %d although the rights did not change", before, got)
	}
}

// TestAccountLogDeleteListsTheMessages: deleting a mailbox records its
// messages as destroyed in an ordinary entry. A message that slipped in
// after the list was read has its own entry, so the list is a delta; a
// Partial entry would make every reader resynchronise after an ordinary
// deletion, most often of an empty mailbox.
func TestAccountLogDeleteListsTheMessages(t *testing.T) {
	u := openUser(t)

	for _, n := range []string{"Scratch", "Empty"} {
		if err := u.CreateMailbox(n, nil); err != nil {
			t.Fatal(err)
		}
	}

	if _, err := u.Deliver("Scratch", []byte("Subject: one\r\n\r\nbody\r\n"), nil, time.Now()); err != nil {
		t.Fatal(err)
	}

	for name, want := range map[string]int{"Scratch": 1, "Empty": 0} {
		before := state(t, u)

		if err := u.DeleteMailbox(name); err != nil {
			t.Fatal(err)
		}

		cs := changesSince(t, u, before)
		if len(cs) != 2 {
			t.Fatalf("DELETE %s produced %+v, want the messages and the mailbox", name, cs)
		}

		if cs[0].Kind != model.KindEmail || cs[0].Partial || len(cs[0].Msgs) != want {
			t.Errorf("the message entry of DELETE %s is %+v, want an ordinary one listing %d", name, cs[0], want)
		}

		if cs[1].Kind != model.KindMailbox || cs[1].Op != model.OpDestroyed {
			t.Errorf("the second entry is %+v, want the destroyed mailbox", cs[1])
		}
	}
}

// TestAccountLogDestroyedCarriesTheExpungeModSeq: the entry names the
// mailbox MODSEQ the removal was written at, not the one the messages
// last carried — a reader that resolves it against the mailbox change
// log would otherwise look in the wrong place.
func TestAccountLogDestroyedCarriesTheExpungeModSeq(t *testing.T) {
	u := openUser(t)

	if err := u.CreateMailbox("INBOX", nil); err != nil {
		t.Fatal(err)
	}

	m, err := u.Deliver("INBOX", []byte("Subject: one\r\n\r\nbody\r\n"), nil, time.Now())
	if err != nil {
		t.Fatal(err)
	}

	before := state(t, u)

	if err := u.Expunge("INBOX", []model.Meta{*m}); err != nil {
		t.Fatal(err)
	}

	cs := changesSince(t, u, before)
	if len(cs) != 1 {
		t.Fatalf("the expunge produced %+v, want one entry", cs)
	}

	if cs[0].ModSeq <= m.ModSeq {
		t.Errorf("the entry carries modseq %d, want one above the message's %d", cs[0].ModSeq, m.ModSeq)
	}
}
