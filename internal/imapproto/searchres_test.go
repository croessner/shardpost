package imapproto

import (
	"strings"
	"testing"

	"shardpost/internal/memstore"
)

// TestSavedResultInUIDSets: "$" (RFC 5182) stands for the saved result
// wherever a UID set is taken — UID EXPUNGE and the UID search key.
func TestSavedResultInUIDSets(t *testing.T) {
	c, _ := newTestServer(t)
	c.ok("LOGIN test test")
	c.append("INBOX", "alice@example.org", "Invoice 4711", "Please find the invoices attached.")
	c.append("INBOX", "bob@example.org", "Lunch", "Pizza or sushi?")
	c.ok("SELECT INBOX")

	c.ok("SEARCH RETURN (SAVE) SUBJECT invoic")

	mustHave(t, c.ok("SEARCH UID $"), "* SEARCH 1")
	mustHave(t, c.ok("UID SEARCH NOT UID $"), "* SEARCH 2")

	c.ok(`STORE 1:2 +FLAGS.SILENT (\Deleted)`)

	lines := c.ok("UID EXPUNGE $")
	mustHave(t, lines, "* 1 EXPUNGE")

	if has(lines, "* 2 EXPUNGE") {
		t.Fatalf("UID EXPUNGE $ removed more than the saved result: %v", lines)
	}

	mustHave(t, c.ok("FETCH 1 (UID)"), "UID 2")
}

// TestReplaceCatenateTooBig: a REPLACE whose CATENATE parts add up to more
// than the literal limit is TOOBIG, as the same APPEND is, not BADURL.
func TestReplaceCatenateTooBig(t *testing.T) {
	c, _ := newTestServer(t, func(o *Options, _ *memstore.Store) { o.MaxLiteral = 64 })
	c.ok("LOGIN test test")

	msg := "Subject: s\r\n\r\n" + strings.Repeat("x", 20) + "\r\n"
	c.ok("APPEND INBOX {%d+}\r\n%s", len(msg), msg)
	c.ok("SELECT INBOX")

	url := `URL "imap:///INBOX/;UID=1"`

	mustHave(t, c.cmd(`APPEND INBOX CATENATE (`+url+` `+url+`)`), "NO [TOOBIG]")
	mustHave(t, c.cmd(`REPLACE 1 INBOX CATENATE (`+url+` `+url+`)`), "NO [TOOBIG]")
	mustHave(t, c.cmd(`REPLACE 1 INBOX CATENATE (URL "imap:///INBOX/;UID=9")`), "NO [BADURL imap:///INBOX/;UID=9]")
	mustHave(t, c.cmd(`REPLACE 1 INBOX CATENATE (FOO "x")`), " BAD ")
}
