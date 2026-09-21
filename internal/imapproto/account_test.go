package imapproto

import (
	"strings"
	"testing"

	"shardpost/internal/memstore"
	"shardpost/internal/model"
)

// accountLog returns the account change log of the test user since the
// beginning of the session.
func accountLog(t *testing.T, store *memstore.Store, since uint64) []model.AccountChange {
	t.Helper()

	u, err := store.User("test")
	if err != nil {
		t.Fatal(err)
	}

	cs, ok, err := u.AccountChanges(since, 1000)
	if err != nil || !ok {
		t.Fatalf("account changes: ok=%v err=%v", ok, err)
	}

	return cs
}

// TestAccountLogFollowsAnIMAPSession: what an IMAP session does to an
// account shows up in the account change log, once per change and in the
// order it happened. This is the check that the log is wired into the
// paths clients actually use, not only into the store API.
func TestAccountLogFollowsAnIMAPSession(t *testing.T) {
	c, store := newTestServer(t)

	c.ok("LOGIN test test")

	// Autocreation of the default mailboxes happens at login, so the
	// session starts counting from whatever state that left behind.
	u, err := store.User("test")
	if err != nil {
		t.Fatal(err)
	}

	start, err := u.AccountState()
	if err != nil {
		t.Fatal(err)
	}

	c.ok("CREATE Work")
	c.ok("SUBSCRIBE Work")
	c.append("Work", "a@example.org", "one", "body")
	c.ok("SELECT Work")
	c.ok(`UID STORE 1 +FLAGS (\Seen)`)
	c.ok("UID COPY 1 INBOX")
	c.ok(`UID STORE 1 +FLAGS (\Deleted)`)
	c.ok("EXPUNGE")

	got := accountLog(t, store, start)

	want := []struct {
		kind model.AccountChangeKind
		op   model.AccountOp
		name string
	}{
		{model.KindMailbox, model.OpCreated, "Work"}, // CREATE
		{model.KindMailbox, model.OpUpdated, "Work"}, // SUBSCRIBE
		{model.KindEmail, model.OpCreated, ""},       // APPEND
		{model.KindEmail, model.OpUpdated, ""},       // STORE \Seen
		{model.KindEmail, model.OpCreated, ""},       // COPY into INBOX
		{model.KindEmail, model.OpUpdated, ""},       // STORE \Deleted
		{model.KindEmail, model.OpDestroyed, ""},     // EXPUNGE
	}

	if len(got) != len(want) {
		t.Fatalf("the session produced %d entries, want %d:\n%s", len(got), len(want), formatLog(got))
	}

	for i, w := range want {
		checkEntry(t, i, got[i], w.kind, w.op, w.name)
	}

	// The mailbox of the CREATE is the one the message entries name.
	for i := 2; i < 4; i++ {
		if got[i].Mailbox != got[0].Mailbox {
			t.Errorf("entry %d names mailbox %q, want the created %q", i, got[i].Mailbox, got[0].Mailbox)
		}
	}

	if got[4].Mailbox == got[0].Mailbox {
		t.Errorf("the COPY into INBOX names the source mailbox %q", got[4].Mailbox)
	}
}

// checkEntry checks one entry against what the command should have left.
func checkEntry(t *testing.T, i int, got model.AccountChange,
	kind model.AccountChangeKind, op model.AccountOp, name string,
) {
	t.Helper()

	if got.Kind != kind || got.Op != op {
		t.Errorf("entry %d is %s/%s, want %s/%s", i, got.Kind, got.Op, kind, op)
	}

	if name != "" && got.Name != name {
		t.Errorf("entry %d names %q, want %q", i, got.Name, name)
	}

	if got.Mailbox == "" {
		t.Errorf("entry %d does not name its mailbox", i)
	}

	if got.Kind == model.KindEmail && len(got.Msgs) == 0 {
		t.Errorf("entry %d is an email change without a message", i)
	}
}

// TestAccountLogDeleteTakesTheMessagesWithIt: deleting a mailbox
// destroys the mailbox and everything in it, and both are reported.
func TestAccountLogDeleteTakesTheMessagesWithIt(t *testing.T) {
	c, store := newTestServer(t)

	c.ok("LOGIN test test")
	c.ok("CREATE Scratch")
	c.append("Scratch", "a@example.org", "one", "body")

	u, err := store.User("test")
	if err != nil {
		t.Fatal(err)
	}

	start, err := u.AccountState()
	if err != nil {
		t.Fatal(err)
	}

	c.ok("DELETE Scratch")

	got := accountLog(t, store, start)
	if len(got) != 2 {
		t.Fatalf("DELETE produced %d entries, want the messages and the mailbox:\n%s", len(got), formatLog(got))
	}

	for _, c := range got {
		if c.Op != model.OpDestroyed {
			t.Errorf("DELETE produced a %s entry", c.Op)
		}

		if c.Mailbox == "" {
			t.Errorf("a %s entry does not name its mailbox", c.Kind)
		}
	}
}

// formatLog renders a log for a failure message.
func formatLog(cs []model.AccountChange) string {
	var b strings.Builder

	for _, c := range cs {
		b.WriteString("  ")
		b.WriteString(string(c.Kind))
		b.WriteString("/")
		b.WriteString(string(c.Op))
		b.WriteString(" ")
		b.WriteString(c.Name)
		b.WriteString("\n")
	}

	return b.String()
}
