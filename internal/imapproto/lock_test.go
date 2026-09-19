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
