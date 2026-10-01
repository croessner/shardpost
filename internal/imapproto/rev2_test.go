package imapproto

import (
	"fmt"
	"testing"
)

// TestIMAP4rev2Session: after ENABLE IMAP4rev2 the session gets what RFC
// 9051 asks for (appendix E 4, 10-12, 21); a session without it keeps
// the IMAP4rev1 responses.
func TestIMAP4rev2Session(t *testing.T) {
	rev1, _ := newTestServer(t)
	rev2, _ := newTestServerClient(t, rev1)

	rev1.ok("LOGIN test test")
	rev2.ok("LOGIN test test")
	mustHave(t, rev2.ok("ENABLE IMAP4rev2"), "* ENABLED IMAP4rev2")

	rev1.append("INBOX", "a@example.org", "one", "body")
	rev1.append("INBOX", "b@example.org", "two", "body")

	// SELECT: no RECENT and [UNSEEN] for IMAP4rev2, but a LIST. The
	// IMAP4rev1 session looks first: SELECT takes the \Recent marks.
	lines := rev1.ok("EXAMINE INBOX")
	mustHave(t, lines, "* 2 RECENT", "[UNSEEN 1]")
	mustLack(t, lines, `* LIST`)

	lines = rev2.ok("SELECT INBOX")
	mustHave(t, lines, "* 2 EXISTS", `* LIST () "/" INBOX`)
	mustLack(t, lines, "RECENT", "[UNSEEN")

	// No \Recent flag.
	mustLack(t, rev2.ok("FETCH 1:* FLAGS"), `\Recent`)
	mustHave(t, rev1.ok("FETCH 1 FLAGS"), `\Recent`)

	// SEARCH answers ESEARCH, with and without matches, with UID.
	mustHave(t, rev2.ok("SEARCH ALL"), fmt.Sprintf(`* ESEARCH (TAG "t%d") ALL 1:2`, rev2.n))
	mustHave(t, rev2.ok("UID SEARCH ALL"), fmt.Sprintf(`* ESEARCH (TAG "t%d") UID ALL 1:2`, rev2.n))

	lines = rev2.ok("SEARCH SUBJECT nothing-like-this")
	mustHave(t, lines, fmt.Sprintf(`* ESEARCH (TAG "t%d")`, rev2.n))
	mustLack(t, lines, "* SEARCH", "ALL")
	mustHave(t, rev2.ok("SEARCH RETURN (COUNT) ALL"), "COUNT 2")
	mustHave(t, rev1.ok("SEARCH ALL"), "* SEARCH 1 2")

	// STATUS has no RECENT.
	mustHave(t, rev2.ok("STATUS Drafts (MESSAGES RECENT)"), "* STATUS Drafts (MESSAGES 0)")
	mustHave(t, rev1.ok("STATUS Drafts (MESSAGES RECENT)"), "* STATUS Drafts (MESSAGES 0 RECENT 0)")

	// A STORE response carries the UID.
	mustHave(t, rev2.ok(`STORE 2 +FLAGS (\Flagged)`), `* 2 FETCH (UID 2 FLAGS (\Flagged))`)

	// A new message is announced without RECENT.
	rev1.append("INBOX", "c@example.org", "three", "body")

	lines = rev2.ok("NOOP")
	mustHave(t, lines, "* 3 EXISTS")
	mustLack(t, lines, "RECENT")

	// LSUB and CHECK are still answered.
	rev2.ok(`LSUB "" "*"`)
	rev2.ok("CHECK")
}

// TestIMAP4rev2SelectOldName: SELECT of a denormalized name reports the
// normalized one with OLDNAME (RFC 9051 6.3.2).
func TestIMAP4rev2SelectOldName(t *testing.T) {
	c, _ := newTestServer(t)
	c.ok("LOGIN test test")
	c.ok("ENABLE IMAP4rev2")
	c.ok("CREATE \"Grün\"")

	mustHave(t, c.ok("SELECT \"Grün\""), "* LIST () \"/\" \"Grün\" (\"OLDNAME\" (\"Grün\"))")
}
