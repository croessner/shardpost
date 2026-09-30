package imapproto

import (
	"strings"
	"testing"
	"time"

	"shardpost/internal/model"
)

// A session opened before a maintenance job locked the account is closed
// at its next command.
func TestSessionClosedWhenAccountLocked(t *testing.T) {
	c, store := newTestServer(t)
	defer func() { _ = c.conn.Close() }()

	c.ok("LOGIN test test")

	u, _ := store.User("test")
	if _, err := u.(model.Importer).ImportLock("job-x", time.Minute); err != nil {
		t.Fatal(err)
	}

	if _, err := c.conn.Write([]byte("t9 SELECT INBOX\r\n")); err != nil {
		t.Fatal(err)
	}

	line, _ := c.br.ReadString('\n')
	if !strings.HasPrefix(line, "* BYE [UNAVAILABLE] Mailbox maintenance") {
		t.Fatalf("got %q, want BYE", line)
	}

	if rest, err := c.br.ReadString('\n'); err == nil {
		t.Fatalf("connection still open: %q", rest)
	}
}

// A session that has a public folder selected is closed at its next
// command once the public namespace is locked (an import into it), the
// way it is when its own account is locked.
func TestSessionClosedWhenSelectedNamespaceLocked(t *testing.T) {
	c, store := newTestServer(t)
	defer func() { _ = c.conn.Close() }()

	pub := publicFolder(t, store, "Announcements", map[string]string{"anyone": "lr"})

	c.ok("LOGIN test test")
	c.ok("SELECT Public/Announcements")

	if _, err := pub.(model.Importer).ImportLock("import-x", time.Minute); err != nil {
		t.Fatal(err)
	}

	if _, err := c.conn.Write([]byte("t9 FETCH 1 FLAGS\r\n")); err != nil {
		t.Fatal(err)
	}

	line, _ := c.br.ReadString('\n')
	if !strings.HasPrefix(line, "* BYE [UNAVAILABLE] Mailbox maintenance") {
		t.Fatalf("got %q, want BYE", line)
	}
}

// A session that only listed the public namespace keeps running when it
// is locked; its view of the namespace is closed, so the next command
// opens it again (and a store that refuses locked namespaces says no).
func TestLockedNamespaceViewIsDropped(t *testing.T) {
	c, store := newTestServer(t)
	defer func() { _ = c.conn.Close() }()

	pub := publicFolder(t, store, "Announcements", map[string]string{"anyone": "lr"})

	c.ok("LOGIN test test")
	c.ok(`LIST "" "*"`)

	if _, err := pub.(model.Importer).ImportLock("import-x", time.Minute); err != nil {
		t.Fatal(err)
	}

	c.ok("SELECT INBOX")
}
