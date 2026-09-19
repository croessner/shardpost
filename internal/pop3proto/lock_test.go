package pop3proto

import (
	"strings"
	"testing"
	"time"

	"shardpost/internal/model"
)

// A session opened before a maintenance job locked the account is closed
// at its next command.
func TestSessionClosedWhenAccountLocked(t *testing.T) {
	addr, u := startSession(t)

	c := dial(t, addr)
	defer func() { _ = c.nc.Close() }()

	c.cmd("USER test")

	if r := c.cmd("PASS test"); !strings.HasPrefix(r, "+OK") {
		t.Fatal(r)
	}

	if _, err := u.(model.Importer).ImportLock("job-x", time.Minute); err != nil {
		t.Fatal(err)
	}

	if r := c.cmd("DELE 1"); !strings.HasPrefix(r, "-ERR [SYS/TEMP] Mailbox maintenance") {
		t.Fatalf("got %q", r)
	}

	if rest, err := c.br.ReadString('\n'); err == nil {
		t.Fatalf("connection still open: %q", rest)
	}
}
