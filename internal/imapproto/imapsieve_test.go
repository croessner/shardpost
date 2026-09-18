package imapproto

import (
	"fmt"
	"io"
	"log/slog"
	"shardpost/internal/config"
	"shardpost/internal/imapsieve"
	"shardpost/internal/memstore"
	"strings"
	"testing"
)

// installEventScript stores a script that flags APPEND, COPY and \Seen
// events and attaches it to INBOX and Archive.
func installEventScript(t *testing.T, c *client, s *memstore.Store) {
	t.Helper()

	u, _ := s.User("test")

	script := `require ["imapsieve","environment","imap4flags"]; if environment :is "imap.cause" "APPEND" {addflag "appended";} if environment :is "imap.cause" "COPY" {addflag "copied";} if allof(environment :is "imap.cause" "FLAG", environment :contains "imap.changedflags" "\\seen") {addflag "read-event";}`
	if err := u.PutScript("events", script); err != nil {
		t.Fatal(err)
	}

	for _, b := range []string{"INBOX", "Archive"} {
		c.cmd(`SETMETADATA %s ("/shared/imapsieve/script" "events")`, b)
	}
}

func TestIMAPSieveWireEvents(t *testing.T) {
	c, s := newTestServer(t, func(o *Options, s *memstore.Store) {
		o.IMAPSieve = imapsieve.New(config.IMAPSieve{Enabled: true, UserScripts: true, ManageSieveURL: "sieve://localhost:4190", MaxParallel: 1, TimeoutMS: 500}, s, slog.New(slog.NewTextHandler(io.Discard, nil)))
	})
	defer func() { _ = c.conn.Close() }()

	c.cmd("LOGIN test test")
	installEventScript(t, c, s)

	caps := strings.Join(c.cmd("CAPABILITY"), " ")
	if !strings.Contains(caps, "IMAPSIEVE=sieve://localhost:4190") {
		t.Fatal("missing capability")
	}

	msg := "Subject: wire\r\n\r\nbody"

	lines := c.cmd("APPEND INBOX {%d+}\r\n%s", len(msg), msg)
	if !strings.Contains(strings.Join(lines, " "), "OK") {
		t.Fatal(lines)
	}

	c.cmd("SELECT INBOX")

	out := strings.Join(c.cmd("UID FETCH 1 FLAGS"), " ")
	if !strings.Contains(out, "appended") {
		t.Fatal(out)
	}

	out = strings.Join(c.cmd("UID FETCH 1 BODY[]"), " ")

	out += strings.Join(c.cmd("UID FETCH 1 FLAGS"), " ")
	if !strings.Contains(out, "read-event") {
		t.Fatal(out)
	}

	c.cmd("UID COPY 1 Archive")
	c.cmd("SELECT Archive")

	out = strings.Join(c.cmd("UID FETCH 1 FLAGS"), " ")
	if !strings.Contains(out, "copied") {
		t.Fatal(out)
	}

	c.cmd("SELECT INBOX")
	c.cmd("UID MOVE 1 Archive")
	c.cmd("SELECT Archive")

	out = strings.Join(c.cmd("UID FETCH 2 FLAGS"), " ")
	if !strings.Contains(out, "copied") {
		t.Fatal(out)
	}
	// No-op STORE does not trigger FLAG processing; the script only changes on APPEND/COPY/Seen.
	if got := c.cmd("UID STORE 1 +FLAGS (copied)"); !strings.Contains(fmt.Sprint(got), "OK") {
		t.Fatal(got)
	}
}
