package imapproto

import (
	"strings"
	"testing"
	"time"

	"shardpost/internal/memstore"
	"shardpost/internal/model"
)

// The mailbox lock of mailbox_delete (stage B17): a locked mailbox is
// refused with NO [UNAVAILABLE], a session that has it selected is
// closed with BYE, every other session goes on.

// lockMailbox locks the account's mailbox as a mailbox_delete job would.
func lockMailbox(t *testing.T, store *memstore.Store, account, name string) {
	t.Helper()

	u, _ := store.User(account)

	mb, err := u.Lookup(name)
	if err != nil {
		t.Fatal(err)
	}

	if err := u.(model.MailboxLocker).LockMailboxes("job-1", []model.MailboxLockTarget{{ID: mb.ID, Name: mb.Name}}, time.Minute); err != nil {
		t.Fatal(err)
	}
}

// unavailable asserts a NO [UNAVAILABLE] answer.
func unavailable(t *testing.T, lines []string, what string) {
	t.Helper()

	if last := lines[len(lines)-1]; !strings.Contains(last, "NO [UNAVAILABLE] Mailbox is being deleted") {
		t.Errorf("%s: %q", what, last)
	}
}

// TestLockedMailboxRefused: SELECT, EXAMINE, STATUS, APPEND, COPY, MOVE,
// CREATE under, RENAME and DELETE of a locked mailbox are refused; INBOX
// and the other mailboxes are not.
func TestLockedMailboxRefused(t *testing.T) {
	c, store := newTestServer(t)
	defer func() { _ = c.conn.Close() }()

	c.ok("LOGIN test test")
	c.ok("CREATE Projects")
	c.ok("CREATE Other")
	c.ok("APPEND INBOX {13}\r\nSubject: hi\r\n\r\n")

	lockMailbox(t, store, "test", "Projects")

	unavailable(t, c.cmd("SELECT Projects"), "SELECT")
	unavailable(t, c.cmd("EXAMINE Projects"), "EXAMINE")
	unavailable(t, c.cmd("STATUS Projects (MESSAGES)"), "STATUS")
	unavailable(t, c.cmd("APPEND Projects {13}\r\nSubject: hi\r\n\r\n"), "APPEND")
	unavailable(t, c.cmd("CREATE Projects/sub"), "CREATE under")
	unavailable(t, c.cmd("RENAME Projects Elsewhere"), "RENAME")
	unavailable(t, c.cmd("RENAME Other Projects/moved"), "RENAME under")
	unavailable(t, c.cmd("DELETE Projects"), "DELETE")

	c.ok("SELECT INBOX")
	unavailable(t, c.cmd("COPY 1 Projects"), "COPY")
	unavailable(t, c.cmd("MOVE 1 Projects"), "MOVE")
	c.ok("COPY 1 Other")
	c.ok("STORE 1 +FLAGS (\\Seen)")
	c.ok("NOOP")

	u, _ := store.User("test")
	if err := u.(model.MailboxLocker).UnlockMailboxes("job-1"); err != nil {
		t.Fatal(err)
	}

	c.ok("STATUS Projects (MESSAGES)")
}

// TestSelectedMailboxLockedBye: a session that has the locked mailbox
// selected gets BYE at its next command; one with INBOX selected goes
// on.
func TestSelectedMailboxLockedBye(t *testing.T) {
	c, store := newTestServer(t)
	defer func() { _ = c.conn.Close() }()

	c.ok("LOGIN test test")
	c.ok("CREATE Projects")
	c.ok("SELECT Projects")

	other, _ := newTestServerClient(t, c)
	defer func() { _ = other.conn.Close() }()

	other.ok("LOGIN test test")
	other.ok("SELECT INBOX")

	lockMailbox(t, store, "test", "Projects")

	if _, err := c.conn.Write([]byte("t9 NOOP\r\n")); err != nil {
		t.Fatal(err)
	}

	line, _ := c.br.ReadString('\n')
	if !strings.HasPrefix(line, "* BYE [UNAVAILABLE] Selected mailbox is being deleted") {
		t.Fatalf("got %q, want BYE", line)
	}

	if rest, err := c.br.ReadString('\n'); err == nil {
		t.Fatalf("connection still open: %q", rest)
	}

	other.ok("NOOP")
	other.ok("FETCH 1:* FLAGS")
}

// TestSelectedMailboxDeletedBye: a mailbox another session deleted
// closes the session that has it selected, after a registry read
// confirmed it (RFC 2180 3.3); a transient error does not.
func TestSelectedMailboxDeletedBye(t *testing.T) {
	c, store := newTestServer(t)
	defer func() { _ = c.conn.Close() }()

	c.ok("LOGIN test test")
	c.ok("CREATE Projects")
	c.ok("SELECT Projects")

	u, _ := store.User("test")
	if err := u.DeleteMailbox("Projects"); err != nil {
		t.Fatal(err)
	}

	if _, err := c.conn.Write([]byte("t9 NOOP\r\n")); err != nil {
		t.Fatal(err)
	}

	line, _ := c.br.ReadString('\n')
	if !strings.HasPrefix(line, "* BYE [UNAVAILABLE] Selected mailbox was deleted") {
		t.Fatalf("got %q, want BYE", line)
	}
}

// TestIdleSelectedMailboxLockedBye: an idling session on the locked
// mailbox is woken and closed with BYE, without a tagged answer to the
// IDLE.
func TestIdleSelectedMailboxLockedBye(t *testing.T) {
	c, store := newTestServer(t)
	defer func() { _ = c.conn.Close() }()

	c.ok("LOGIN test test")
	c.ok("CREATE Projects")
	c.ok("SELECT Projects")

	if _, err := c.conn.Write([]byte("t9 IDLE\r\n")); err != nil {
		t.Fatal(err)
	}

	if line, _ := c.br.ReadString('\n'); !strings.HasPrefix(line, "+") {
		t.Fatalf("IDLE: %q", line)
	}

	lockMailbox(t, store, "test", "Projects")

	u, _ := store.User("test")
	mb, _ := u.Lookup("Projects")
	u.(model.MailboxLocker).WakeSelected(mb.ID)

	_ = c.conn.SetReadDeadline(time.Now().Add(5 * time.Second))

	line, err := c.br.ReadString('\n')
	if err != nil || !strings.HasPrefix(line, "* BYE [UNAVAILABLE] Selected mailbox is being deleted") {
		t.Fatalf("got %q (%v), want BYE", line, err)
	}

	if rest, err := c.br.ReadString('\n'); err == nil {
		t.Fatalf("a line after BYE: %q", rest)
	}
}
