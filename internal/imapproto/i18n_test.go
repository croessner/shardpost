package imapproto

import (
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"shardpost/internal/memstore"
)

// Mailbox names on the wire: modified UTF-7 for sessions that enabled
// neither UTF8=ACCEPT nor IMAP4rev2, UTF-8 for those that did. The store
// holds UTF-8 either way.

// TestMUTF7RoundTrip: a legacy client creates, lists, selects, renames
// and deletes a mailbox under its modified UTF-7 name, and the store
// holds the UTF-8 name.
func TestMUTF7RoundTrip(t *testing.T) {
	c, store := newTestServer(t)
	c.ok("LOGIN test test")

	c.ok("CREATE Entw&APw-rfe")

	u, _ := store.User("test")
	if _, err := u.Lookup("Entwürfe"); err != nil {
		t.Fatalf("stored name is not UTF-8: %v", err)
	}

	mustHave(t, c.ok(`LIST "" "*"`), `"/" Entw&APw-rfe`)
	mustLack(t, c.ok(`LIST "" "*"`), "Entwürfe")

	// A pattern is decoded like a name; wildcards stand for themselves.
	mustHave(t, c.ok(`LIST "" "Entw&APw-*"`), `"/" Entw&APw-rfe`)

	c.append("Entw&APw-rfe", "x@example.org", "draft", "body")
	mustHave(t, c.ok(`STATUS Entw&APw-rfe (MESSAGES)`), `* STATUS Entw&APw-rfe (MESSAGES 1)`)
	c.ok("SELECT Entw&APw-rfe")
	mustHave(t, c.cmd("COPY 1 &AMQ-rger"), "TRYCREATE")
	c.ok("CREATE &AMQ-rger")
	c.ok("COPY 1 &AMQ-rger")
	c.ok("UNSELECT")

	c.ok("SUBSCRIBE &AMQ-rger")
	mustHave(t, c.ok(`LSUB "" "*"`), `"/" &AMQ-rger`)
	mustHave(t, c.ok("MYRIGHTS &AMQ-rger"), "* MYRIGHTS &AMQ-rger ")
	mustHave(t, c.ok("GETACL &AMQ-rger"), "* ACL &AMQ-rger ")
	mustHave(t, c.ok("GETQUOTAROOT &AMQ-rger"), `* QUOTAROOT &AMQ-rger ""`)
	c.ok(`SETMETADATA &AMQ-rger (/private/comment "x")`)
	mustHave(t, c.ok("GETMETADATA &AMQ-rger /private/comment"), `* METADATA &AMQ-rger (/private/comment "x")`)
	mustHave(t, c.ok(`LIST "" "&AMQ-*" RETURN (STATUS (MESSAGES) MYRIGHTS)`),
		`* STATUS &AMQ-rger (MESSAGES 1)`, `* MYRIGHTS &AMQ-rger `)

	c.ok("RENAME &AMQ-rger Gr&APwA3w-e")

	if _, err := u.Lookup("Grüße"); err != nil {
		t.Fatalf("renamed mailbox: %v", err)
	}

	// "&" is "&-": a literal ampersand round-trips.
	c.ok(`CREATE "A&-B"`)

	if _, err := u.Lookup("A&B"); err != nil {
		t.Fatalf("A&B: %v", err)
	}

	mustHave(t, c.ok(`LIST "" "A*"`), `"/" A&-B`)

	c.ok("DELETE Gr&APwA3w-e")
	c.ok("DELETE Entw&APw-rfe")
	mustLack(t, c.ok(`LIST "" "*"`), "Gr&APw", "Entw&APw")
}

// TestUTF8Names: after ENABLE UTF8=ACCEPT or ENABLE IMAP4rev2 names are
// UTF-8 both ways; an "&" sequence is then part of the name, not an
// encoding.
func TestUTF8Names(t *testing.T) {
	for _, enable := range []string{"UTF8=ACCEPT", "IMAP4rev2"} {
		c, store := newTestServer(t)
		c.ok("LOGIN test test")
		mustHave(t, c.ok("ENABLE %s", enable), "* ENABLED "+enable)
		// Enabled once; a repeat reports nothing new (RFC 9051 6.3.1).
		mustLack(t, c.ok("ENABLE %s", enable), "ENABLED "+enable)

		c.ok(`CREATE "Entwürfe"`)
		// UTF-8 goes out in a quoted string, not as a literal.
		mustHave(t, c.ok(`LIST "" "Entw*"`), `"/" "Entwürfe"`)
		mustHave(t, c.ok(`STATUS "Entwürfe" (MESSAGES)`), `* STATUS "Entwürfe" (MESSAGES 0)`)
		mustHave(t, c.ok(`LIST "" "Entwü%%"`), `"/" "Entwürfe"`)

		c.ok("CREATE Entw&APw-rfe")

		u, _ := store.User("test")
		if _, err := u.Lookup("Entw&APw-rfe"); err != nil {
			t.Fatalf("%s: the name was decoded: %v", enable, err)
		}

		mustHave(t, c.ok(`LIST "" "Entw&*"`), `"/" Entw&APw-rfe`)
	}
}

// TestMUTF7Invalid: a name that is not valid modified UTF-7 (or not in
// its only valid form) is NO [CANNOT] in every command that names a
// mailbox; a LIST pattern or reference is taken as it stands and lists
// nothing (imaptest "atoms"); octets that are not UTF-8 are BAD. Literals
// that follow are consumed.
func TestMUTF7Invalid(t *testing.T) {
	c, store := newTestServer(t)
	c.ok("LOGIN test test")

	for _, cmd := range []string{"CREATE AT&T", "SELECT &AGE-", `CREATE "&APw-&APw-"`,
		"STATUS Entw&APw (MESSAGES)", "RENAME INBOX &U,BTFw", "SUBSCRIBE &-&"} {
		mustHave(t, c.cmd("%s", cmd), " NO [CANNOT] Mailbox name is not valid modified UTF-7")
	}

	if lines := c.ok(`LIST "" "&Jjo!"`); len(lines) != 1 {
		t.Errorf("an invalid pattern listed something: %v", lines)
	}

	c.ok("%s", `LIST "" !#$$&'+,-.0123456789:;<=>?@^_`+"`"+`|[}%*`)

	msg := "Subject: x\r\n\r\nbody\r\n"
	mustHave(t, c.cmd("APPEND &AGE- {%d+}\r\n%s", len(msg), msg), " NO [CANNOT]")
	// The literal was drained: the next command is read as one.
	c.ok("NOOP")

	mustHave(t, c.cmd("CREATE \"Entw\xfcrfe\""), " BAD Mailbox name is not valid UTF-8")

	u, _ := store.User("test")

	boxes, _ := u.ListMailboxes()
	for _, b := range boxes {
		if strings.Contains(b.Name, "&") || strings.Contains(b.Name, "AT") {
			t.Errorf("an invalid name was created: %q", b.Name)
		}
	}
}

// TestRawUTF8FromLegacyClient: a client that never enabled UTF-8 but
// sends a UTF-8 name is understood; it is answered in modified UTF-7.
func TestRawUTF8FromLegacyClient(t *testing.T) {
	c, store := newTestServer(t)
	c.ok("LOGIN test test")

	c.ok(`CREATE "Grüße"`)

	u, _ := store.User("test")
	if _, err := u.Lookup("Grüße"); err != nil {
		t.Fatal(err)
	}

	mustHave(t, c.ok(`LIST "" "*"`), `"/" Gr&APwA3w-e`)
	c.ok(`SELECT "Grüße"`)
}

// TestNewNameIsNetUnicode: CREATE and RENAME refuse control characters
// (RFC 6855 3) and put a name into Normalization Form C (RFC 9051 5.1),
// announcing the normalized name with OLDNAME; the denormalized form
// finds the mailbox in every command, unless a mailbox of exactly that
// name exists from before.
func TestNewNameIsNetUnicode(t *testing.T) {
	const (
		nfd = "Entwürfe"
		nfc = "Entwürfe"
	)

	c, store := newTestServer(t)
	c.ok("LOGIN test test")
	c.ok("ENABLE UTF8=ACCEPT")

	mustHave(t, c.ok(`CREATE "%s"`, nfd), `* LIST () "/" "`+nfc+`" ("OLDNAME" ("`+nfd+`"))`)

	u, _ := store.User("test")
	if _, err := u.Lookup(nfc); err != nil {
		t.Fatalf("not stored in NFC: %v", err)
	}

	mustHave(t, c.ok(`SELECT "%s"`, nfd), "OK [READ-WRITE]")
	mustHave(t, c.ok(`STATUS "%s" (MESSAGES)`, nfd), `* STATUS "`+nfc+`" (MESSAGES 0)`)
	c.ok("UNSELECT")
	c.ok(`RENAME Junk "Grün"`)

	if _, err := u.Lookup("Grün"); err != nil {
		t.Fatalf("rename not normalized: %v", err)
	}

	// RENAME to a denormalized name announces the NFC one too.
	mustHave(t, c.ok("RENAME \"Grün\" \"Blätter\""), "* LIST () \"/\" \"Blätter\" (\"OLDNAME\" (\"Blätter\"))")

	// LIST patterns and references in either form find the NFC name.
	mustHave(t, c.ok("LIST \"\" \"Entwür*\""), "\"/\" \""+nfc+"\"")
	c.ok("CREATE \"Blätter/Eiche\"")
	mustHave(t, c.ok("LIST \"Blätter\" \"%%\""), "\"/\" \"Blätter/Eiche\"")
	mustHave(t, c.ok("LIST \"Blätter/\" \"%%\""), "\"/\" \"Blätter/Eiche\"")

	// NOTIFY names in either form.
	mustHave(t, c.ok(`NOTIFY SET STATUS (MAILBOXES "%s" (MessageNew MessageExpunge))`, nfd), "* STATUS \""+nfc+"\" (MESSAGES 0")
	c.ok("NOTIFY NONE")

	// A name stored denormalized before stays reachable as it is.
	if err := u.EnsureMailbox("Alt/Süd", nil); err != nil {
		t.Fatal(err)
	}

	mustHave(t, c.ok(`STATUS "Alt/Süd" (MESSAGES)`), `* STATUS "Alt/Su`+"̈"+`d" (MESSAGES 0)`)

	mustHave(t, c.cmd("CREATE \"a b\""), "NO [CANNOT]")
	mustHave(t, c.cmd("CREATE \"a\u0085b\""), "NO [CANNOT]")
	mustHave(t, c.cmd("RENAME Trash \"a\u0001b\""), "NO [CANNOT]")

	// Modified UTF-7 can carry control characters too.
	c2, _ := newTestServer(t)
	c2.ok("LOGIN test test")
	mustHave(t, c2.cmd("CREATE tab&AAk-here"), "NO [CANNOT]")
}

// TestMUTF7Namespaces: the shared and public namespaces, and the owner
// part of a shared name, work in both name forms; NAMESPACE encodes its
// prefixes.
func TestMUTF7Namespaces(t *testing.T) {
	alice, bob := twoUsersWith(t, func(o *Options) { o.PublicPrefix = "Öffentlich/" })

	alice.ok(`CREATE "Entwürfe/Ärger"`)
	alice.ok(`SETACL "Entwürfe/Ärger" bob lr`)
	alice.append(`"Entwürfe/Ärger"`, "x@example.org", "hello", "body")

	// bob without UTF-8: modified UTF-7 throughout.
	mustHave(t, bob.ok("NAMESPACE"), `(("Shared/" "/")) (("&ANY-ffentlich/" "/"))`)
	mustHave(t, bob.ok(`LIST "" "Shared/*"`), `"/" Shared/alice/Entw&APw-rfe/&AMQ-rger`, `"/" Shared/alice/Entw&APw-rfe`)
	mustHave(t, bob.ok(`LIST "Shared/alice/" "Entw&APw-rfe/%%"`), `"/" Shared/alice/Entw&APw-rfe/&AMQ-rger`)
	mustHave(t, bob.ok("STATUS Shared/alice/Entw&APw-rfe/&AMQ-rger (MESSAGES)"), "* STATUS Shared/alice/Entw&APw-rfe/&AMQ-rger (MESSAGES 1)")
	mustHave(t, bob.ok("MYRIGHTS Shared/alice/Entw&APw-rfe/&AMQ-rger"), "* MYRIGHTS Shared/alice/Entw&APw-rfe/&AMQ-rger lr")
	bob.ok("SELECT Shared/alice/Entw&APw-rfe/&AMQ-rger")

	// alice with UTF-8: the same mailbox, the same prefix, in UTF-8.
	alice.ok("ENABLE UTF8=ACCEPT")
	mustHave(t, alice.ok("NAMESPACE"), `(("Shared/" "/")) (("Öffentlich/" "/"))`)
	mustHave(t, alice.ok(`GETACL "Entwürfe/Ärger"`), `* ACL "Entwürfe/Ärger" alice lrswipkxtea bob lr`)
}

// TestNFCOtherNamespaces: a denormalized name finds a public folder and
// a mailbox another account shares.
func TestNFCOtherNamespaces(t *testing.T) {
	alice, bob := twoUsersWith(t, func(o *Options) { o.PublicPrefix = DefaultPublicPrefix })

	alice.ok("CREATE \"Grün\"")
	alice.ok("SETACL \"Grün\" bob lr")

	bob.ok("ENABLE UTF8=ACCEPT")
	mustHave(t, bob.ok("STATUS \"Shared/alice/Grün\" (MESSAGES)"), "* STATUS \"Shared/alice/Grün\" (MESSAGES 0)")
	mustHave(t, bob.ok("LIST \"\" \"Shared/alice/Grü*\""), "\"/\" \"Shared/alice/Grün\"")

	c, store := newTestServer(t)
	publicFolder(t, store, "Grün", map[string]string{"anyone": "lr"})
	c.ok("LOGIN test test")
	c.ok("ENABLE UTF8=ACCEPT")
	mustHave(t, c.ok("STATUS \"Public/Grün\" (MESSAGES)"), "* STATUS \"Public/Grün\" (MESSAGES 1)")
}

// TestMUTF7PublicFolder: a public folder with a non-ASCII name.
func TestMUTF7PublicFolder(t *testing.T) {
	c, store := newTestServer(t)
	publicFolder(t, store, "Ärger", map[string]string{"anyone": "lr"})

	c.ok("LOGIN test test")
	mustHave(t, c.ok(`LIST "" "Public/*"`), `"/" Public/&AMQ-rger`)
	mustHave(t, c.ok("STATUS Public/&AMQ-rger (MESSAGES)"), "* STATUS Public/&AMQ-rger (MESSAGES 1)")
	c.ok("EXAMINE Public/&AMQ-rger")
}

// TestMUTF7Notify: NOTIFY takes modified UTF-7 names and reports STATUS
// with them.
func TestMUTF7Notify(t *testing.T) {
	a, _ := newTestServer(t)
	a.ok("LOGIN test test")
	a.ok("CREATE Entw&APw-rfe")

	mustHave(t, a.ok(`NOTIFY SET STATUS (MAILBOXES Entw&APw-rfe (MessageNew MessageExpunge))`), "* STATUS Entw&APw-rfe (MESSAGES 0")
	mustHave(t, a.cmd(`NOTIFY SET (MAILBOXES "AT&T" (MessageNew MessageExpunge))`), "NO [CANNOT]")
}

// TestSearchCharsetAfterUTF8Accept: RFC 6855 3 — after ENABLE
// UTF8=ACCEPT a CHARSET other than UTF-8 or US-ASCII is BAD; those two
// are taken, and before the ENABLE (or with IMAP4rev2 only) any known
// charset is.
func TestSearchCharsetAfterUTF8Accept(t *testing.T) {
	c, _ := newTestServer(t)
	c.ok("LOGIN test test")
	c.ok("ENABLE IMAP4rev2")
	c.ok("SELECT INBOX")
	c.ok("SEARCH CHARSET ISO-8859-1 ALL")
	c.ok("ENABLE UTF8=ACCEPT")
	c.ok("SEARCH CHARSET UTF-8 ALL")
	c.ok("SEARCH CHARSET US-ASCII ALL")
	mustHave(t, c.cmd("SEARCH CHARSET ISO-8859-1 ALL"), " BAD ")
	c.ok("SEARCH ALL")
}

// TestAppendUTF8: the APPEND, CATENATE and REPLACE form "UTF8
// (literal8)" (RFC 6855 4) needs ENABLE UTF8=ACCEPT.
func TestAppendUTF8(t *testing.T) {
	c, _ := newTestServer(t)
	c.ok("LOGIN test test")

	msg := "From: Jürgen <j@example.org>\r\nSubject: Grüße\r\n\r\nHallo\r\n"
	mustHave(t, c.cmd("APPEND INBOX UTF8 (~{%d+}\r\n%s)", len(msg), msg), " BAD ")
	c.ok("NOOP")

	c.ok("ENABLE UTF8=ACCEPT")
	mustHave(t, c.ok("APPEND INBOX (\\Seen) UTF8 (~{%d+}\r\n%s)", len(msg), msg), "APPENDUID")
	mustHave(t, c.ok(`APPEND INBOX CATENATE (UTF8 (~{%d+}`+"\r\n%s"+`) URL "/INBOX/;UID=1/;SECTION=TEXT")`, len(msg), msg), "APPENDUID")

	c.ok("SELECT INBOX")
	mustHave(t, c.ok("FETCH 1:2 (FLAGS BODY.PEEK[HEADER.FIELDS (SUBJECT)])"), "Subject: Grüße", `\Seen`)
	mustHave(t, c.ok("FETCH 2 BODY.PEEK[TEXT]"), "Hallo")

	mustHave(t, c.ok("REPLACE 2 INBOX UTF8 (~{%d+}\r\n%s)", len(msg), msg), "REPLACE completed")
}

// TestSaveDateSearch: the RFC 8514 keys compare the save date, not the
// internal date, on the date alone. The days are taken from the save
// date the server reports, so the test does not depend on midnight.
func TestSaveDateSearch(t *testing.T) {
	c, _ := newTestServer(t)
	c.ok("LOGIN test test")

	msg := "Subject: old\r\n\r\nbody\r\n"
	c.ok(`APPEND INBOX "01-Jan-2020 10:00:00 +0000" {%d+}`+"\r\n%s", len(msg), msg)
	c.ok("SELECT INBOX")

	line := find(c.ok("FETCH 1 SAVEDATE"), "SAVEDATE")

	i := strings.Index(line, `SAVEDATE "`)
	if i < 0 {
		t.Fatalf("no save date: %q", line)
	}

	saved, err := time.Parse(fetchDateLayout, line[i+len(`SAVEDATE "`):i+len(`SAVEDATE "`)+len(fetchDateLayout)])
	if err != nil {
		t.Fatal(err)
	}

	day := saved.Format("2-Jan-2006")
	next := saved.AddDate(0, 0, 1).Format("2-Jan-2006")

	mustHave(t, c.ok("SEARCH SAVEDON %s", day), "* SEARCH 1")
	mustHave(t, c.ok("SEARCH SAVEDSINCE %s", day), "* SEARCH 1")
	mustHave(t, c.ok("SEARCH SAVEDBEFORE %s", next), "* SEARCH 1")
	mustHave(t, c.ok("SEARCH SAVEDATESUPPORTED"), "* SEARCH 1")
	mustHave(t, c.ok("SEARCH ON 1-Jan-2020"), "* SEARCH 1")

	for _, q := range []string{"SAVEDBEFORE " + day, "SAVEDON 1-Jan-2020", "SAVEDSINCE " + next, "NOT SAVEDATESUPPORTED"} {
		if lines := c.ok("SEARCH %s", q); has(lines, "* SEARCH 1") {
			t.Errorf("%s matched: %v", q, lines)
		}
	}
}

// TestQuotaSet: QUOTASET is advertised exactly when SETQUOTA is allowed.
func TestQuotaSet(t *testing.T) {
	c, _ := newTestServer(t)
	mustHave(t, c.ok("LOGIN test test"), "QUOTASET")

	d, _ := newTestServer(t, func(o *Options, _ *memstore.Store) { o.QuotaSelfService = false })
	mustLack(t, d.ok("LOGIN test test"), "QUOTASET")
	mustLack(t, d.ok("CAPABILITY"), "QUOTASET")
}

// TestStatusDeletedStorage: RFC 9208 4.1.4, in units of 1024 octets.
func TestStatusDeletedStorage(t *testing.T) {
	c, _ := newTestServer(t)
	c.ok("LOGIN test test")

	big := "Subject: big\r\n\r\n" + strings.Repeat("x", 3000) + "\r\n" // 3018 octets: 3 units of 1024
	c.ok("APPEND INBOX {%d+}\r\n%s", len(big), big)
	c.ok("APPEND INBOX {%d+}\r\n%s", len(big), big)

	mustHave(t, c.ok("STATUS INBOX (DELETED DELETED-STORAGE)"), "* STATUS INBOX (DELETED 0 DELETED-STORAGE 0)")

	c.ok("SELECT INBOX")
	c.ok(`STORE 1 +FLAGS (\Deleted)`)
	mustHave(t, c.ok("STATUS INBOX (MESSAGES DELETED DELETED-STORAGE)"), "* STATUS INBOX (MESSAGES 2 DELETED 1 DELETED-STORAGE 3)")
}

// TestCreateMailboxID: RFC 8474 4.1, the id comes with CREATE's OK.
func TestCreateMailboxID(t *testing.T) {
	c, _ := newTestServer(t)
	c.ok("LOGIN test test")

	lines := c.ok("CREATE Projects")

	last := lines[len(lines)-1]
	if !strings.Contains(last, "OK [MAILBOXID (") {
		t.Fatalf("no MAILBOXID: %v", lines)
	}

	id := last[strings.Index(last, "(")+1 : strings.Index(last, ")")]
	mustHave(t, c.ok("STATUS Projects (MAILBOXID)"), "MAILBOXID ("+id+")")
}

// TestPreviewLazy: RFC 8970 — PREVIEW (LAZY) returns the stored preview,
// an empty preview is "" and never NIL without LAZY, unknown modifiers
// are BAD.
func TestPreviewLazy(t *testing.T) {
	c, _ := newTestServer(t)
	c.ok("LOGIN test test")
	c.append("INBOX", "x@example.org", "one", "Preview text here")

	empty := "Subject: empty\r\n\r\n"
	c.ok("APPEND INBOX {%d+}\r\n%s", len(empty), empty)
	c.ok("SELECT INBOX")

	mustHave(t, c.ok("FETCH 1 (UID PREVIEW (LAZY))"), `PREVIEW "Preview text here"`)
	mustHave(t, c.ok("FETCH 1 PREVIEW (LAZY)"), `* 1 FETCH (PREVIEW "Preview text here")`)
	mustHave(t, c.ok("FETCH 1 (PREVIEW (lazy) FLAGS)"), `PREVIEW "Preview text here"`, "FLAGS (")
	mustHave(t, c.ok("FETCH 2 (PREVIEW)"), `* 2 FETCH (PREVIEW "")`)
	mustHave(t, c.ok("FETCH 2 (PREVIEW (LAZY))"), `* 2 FETCH (PREVIEW "")`)
	mustHave(t, c.cmd("FETCH 1 (PREVIEW (EAGER))"), " BAD ")
	mustHave(t, c.cmd("FETCH 1 (PREVIEW ())"), " BAD ")
	// A lone PREVIEW followed by FETCH modifiers keeps them modifiers.
	c.ok("FETCH 1 PREVIEW (CHANGEDSINCE 1)")
}

// TestBadURLCarriesURL: RFC 4469 4.1, the failing URL is in the code.
func TestBadURLCarriesURL(t *testing.T) {
	c, _ := newTestServer(t)
	c.ok("LOGIN test test")
	c.append("INBOX", "x@example.org", "one", "body")

	mustHave(t, c.cmd(`APPEND INBOX CATENATE (URL "/INBOX/;UID=1" URL "/INBOX/;UID=42")`), "NO [BADURL /INBOX/;UID=42] ")
	mustHave(t, c.cmd(`APPEND INBOX CATENATE (URL "/IN]BOX/;UID=1")`), "NO [BADURL /IN%5DBOX/;UID=1] ")

	// A long URL is cut.
	long := "/INBOX/;UID=1/;SECTION=" + strings.Repeat("ü", 1500)
	line := find(c.cmd(`APPEND INBOX CATENATE (URL {%d+}`+"\r\n%s)", len(long), long), "BADURL")

	code := line[strings.Index(line, "[BADURL ")+len("[BADURL ") : strings.Index(line, "] ")]
	if len(code) > maxBadURL+8 || !strings.HasSuffix(code, "...") || !utf8.ValidString(code) {
		t.Fatalf("BADURL not cut: %d %q", len(code), code[len(code)-10:])
	}
}

// TestMUTF7MoreResponses: the \Noselect parent of LSUB, the server
// entries of GETMETADATA "" and an invalid LIST reference, which lists
// nothing instead of failing.
func TestMUTF7MoreResponses(t *testing.T) {
	c, _ := newTestServer(t)
	c.ok("LOGIN test test")
	c.ok("CREATE Entw&APw-rfe/x")
	c.ok("SUBSCRIBE Entw&APw-rfe/x")
	c.ok("UNSUBSCRIBE Entw&APw-rfe")

	mustHave(t, c.ok(`LSUB "" "%%"`), `* LSUB (\Noselect) "/" Entw&APw-rfe`)
	mustHave(t, c.ok(`LSUB "" "*"`), `"/" Entw&APw-rfe/x`)

	c.ok(`SETMETADATA "" (/private/comment "x")`)
	mustHave(t, c.ok(`GETMETADATA "" /private/comment`), `* METADATA "" (/private/comment "x")`)

	c.ok(`LIST "&Jjo!" "*"`)
	mustHave(t, c.ok(`LIST "" ("INBOX" "AT&T")`), `* LIST (\HasNoChildren) "/" INBOX`)
}

// TestSortDisplay: SORT=DISPLAY is advertised, DISPLAYFROM sorts on the
// display name with mailbox@host as the fallback (RFC 5957 3).
func TestSortDisplay(t *testing.T) {
	c, _ := newTestServer(t)
	mustHave(t, c.ok("LOGIN test test"), "SORT=DISPLAY")

	c.append("INBOX", "Zed <a@example.org>", "one", "body")
	c.append("INBOX", "b@example.org", "two", "body")
	c.append("INBOX", "alpha <z@example.org>", "three", "body")
	c.append("INBOX", "Bz <x@example.org>", "four", "body")
	c.ok("SELECT INBOX")

	// alpha, b@example.org, bz, zed
	mustHave(t, c.ok("SORT (DISPLAYFROM) UTF-8 ALL"), "* SORT 3 2 4 1")
	mustHave(t, c.ok("SORT (REVERSE DISPLAYFROM) UTF-8 ALL"), "* SORT 1 4 2 3")
	// To is the same for all: arrival order decides.
	mustHave(t, c.ok("SORT (DISPLAYTO) UTF-8 ALL"), "* SORT 1 2 3 4")
}

// TestBinaryUnknownCTE: RFC 3516 4.2, a section whose encoding the
// server cannot decode fails BINARY and BINARY.SIZE with UNKNOWN-CTE.
func TestBinaryUnknownCTE(t *testing.T) {
	c, _ := newTestServer(t)
	c.ok("LOGIN test test")

	single := "Subject: uu\r\nContent-Type: application/octet-stream\r\nContent-Transfer-Encoding: x-uuencode\r\n\r\nbegin 644 x\r\nend\r\n"
	c.ok("APPEND INBOX {%d+}\r\n%s", len(single), single)

	multi := "Subject: mp\r\nMIME-Version: 1.0\r\nContent-Type: multipart/mixed; boundary=b\r\n\r\n" +
		"--b\r\nContent-Type: text/plain\r\nContent-Transfer-Encoding: base64\r\n\r\naGFsbG8=\r\n" +
		"--b\r\nContent-Type: application/x-odd\r\nContent-Transfer-Encoding: x-odd\r\n\r\nzzz\r\n--b--\r\n"
	c.ok("APPEND INBOX {%d+}\r\n%s", len(multi), multi)
	c.ok("SELECT INBOX")

	mustHave(t, c.cmd("FETCH 1 BINARY.PEEK[1]"), "NO [UNKNOWN-CTE]")
	mustHave(t, c.cmd("FETCH 1 BINARY.SIZE[1]"), "NO [UNKNOWN-CTE]")
	mustHave(t, c.cmd("FETCH 2 BINARY.PEEK[2]"), "NO [UNKNOWN-CTE]")
	mustHave(t, c.ok("FETCH 2 BINARY.PEEK[1]"), "hallo")
	mustHave(t, c.ok("FETCH 1 BODY.PEEK[1]"), "begin 644 x")
	// The whole message has no encoding of its own.
	c.ok("FETCH 2 BINARY.PEEK[]")

	// A FETCH that would set \Seen fails before it marks anything.
	mustHave(t, c.cmd("FETCH 1:2 BINARY[1]"), "NO [UNKNOWN-CTE]")
	mustLack(t, c.ok("FETCH 1:2 FLAGS"), `\Seen`)

	// One message that decodes: read once for the check and the answer.
	mustHave(t, c.ok("FETCH 2 BINARY[1]"), "hallo", `\Seen`)
}
