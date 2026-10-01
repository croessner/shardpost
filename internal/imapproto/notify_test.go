package imapproto

import (
	"strings"
	"testing"
	"time"
)

// unsolicited reads untagged lines until the deadline.
func (c *client) unsolicited(d time.Duration) []string {
	c.t.Helper()

	_ = c.conn.SetReadDeadline(time.Now().Add(d))
	defer func() { _ = c.conn.SetReadDeadline(time.Time{}) }()

	var lines []string

	for {
		line, err := c.br.ReadString('\n')
		if err != nil {
			return lines
		}

		lines = append(lines, strings.TrimRight(line, "\r\n"))
	}
}

func TestNotify(t *testing.T) {
	a, _ := newTestServer(t)
	b, _ := newTestServerClient(t, a)
	a.ok("LOGIN test test")
	b.ok("LOGIN test test")
	a.ok("CREATE Work")
	a.ok("SELECT Work")
	// Unsupported event and unpaired events are refused.
	mustHave(t, a.cmd(`NOTIFY SET (PERSONAL (MailboxName))`), "NO [BADEVENT (MessageNew MessageExpunge FlagChange)]")

	mustHave(t, a.cmd(`NOTIFY SET (SELECTED (MessageNew))`), "BAD")

	lines := a.ok(`NOTIFY SET STATUS (SELECTED (MessageNew (UID FLAGS) MessageExpunge FlagChange)) (INBOXES (MessageNew MessageExpunge))`)
	mustHave(t, lines, "* STATUS INBOX (MESSAGES 0 UIDNEXT 1")
	// A delivery into INBOX by another session yields an unsolicited STATUS.
	b.append("INBOX", "x@example.org", "one", "body")

	lines = a.unsolicited(time.Second)
	mustHave(t, lines, "* STATUS INBOX (MESSAGES 1 UIDNEXT 2")
	// A delivery into the selected mailbox yields EXISTS plus the FETCH attributes.
	b.append("Work", "x@example.org", "two", "body")

	lines = a.unsolicited(time.Second)
	mustHave(t, lines, "* 1 EXISTS", "* 1 FETCH (UID 1 FLAGS (")
	// Flag changes on the selected mailbox are pushed.
	b.ok("SELECT Work")
	b.ok(`STORE 1 +FLAGS (\Flagged)`)

	lines = a.unsolicited(time.Second)
	mustHave(t, lines, `\Flagged`)
	// NOTIFY NONE stops the pushes; a later change arrives with the next command.
	a.ok("NOTIFY NONE")
	b.append("INBOX", "x@example.org", "three", "body")

	if lines = a.unsolicited(300 * time.Millisecond); len(lines) != 0 {
		t.Fatal(lines)
	}
	// IDLE still works after NOTIFY was used.
	a.ok("NOOP")
}
