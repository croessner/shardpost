package imapproto

import (
	"bufio"
	"net"
	"strings"
	"testing"

	"shardpost/internal/auth"
	"shardpost/internal/memstore"
)

// twoUsers starts a server with alice and bob and returns a logged-in
// client for each.
func twoUsers(t *testing.T) (alice, bob *client) {
	t.Helper()

	store := memstore.New()
	srv := New(Options{Store: store, Auth: auth.Static{"alice": "a", "bob": "b"}, InsecureAuth: true, Autocreate: DefaultSpecialFolders})

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	go func() { _ = srv.Serve(ln) }()

	t.Cleanup(func() { _ = srv.Close() })

	dial := func(user, pass string) *client {
		nc, err := net.Dial("tcp", ln.Addr().String())
		if err != nil {
			t.Fatal(err)
		}

		c := &client{t: t, conn: nc, br: bufio.NewReader(nc)}
		if g, _ := c.br.ReadString('\n'); !strings.HasPrefix(g, "* OK") {
			t.Fatalf("greeting %q", g)
		}

		c.ok("LOGIN %s %s", user, pass)

		return c
	}

	return dial("alice", "a"), dial("bob", "b")
}

// checkSharedLimitedRights checks what bob may do on alice's Shared with
// rights lrs: \Seen only, reading and copying out, no append or admin.
func checkSharedLimitedRights(t *testing.T, bob *client) {
	t.Helper()

	// s allows \Seen, w is missing for other flags, no t for \Deleted.
	bob.ok(`SELECT "Other Users/alice/Shared"`)
	bob.ok(`STORE 1 +FLAGS (\Seen)`)

	mustHave(t, bob.cmd(`STORE 1 +FLAGS (\Flagged)`), "NOPERM")

	mustHave(t, bob.cmd(`STORE 1 +FLAGS (\Deleted)`), "NOPERM")

	mustHave(t, bob.cmd(`EXPUNGE`), "NOPERM")
	// Reading works, and a copy into bob's own INBOX crosses namespaces.
	mustHave(t, bob.ok(`FETCH 1 (FLAGS BODY.PEEK[HEADER.FIELDS (SUBJECT)])`), "Subject: hello")

	mustHave(t, bob.ok(`COPY 1 INBOX`), "COPYUID")

	bob.ok(`SELECT INBOX`)

	mustHave(t, bob.ok(`FETCH 1 (BODY.PEEK[HEADER.FIELDS (SUBJECT)])`), "Subject: hello")
	// Bob may not append or administer.
	mustHave(t, bob.cmd(`APPEND "Other Users/alice/Shared" {5}`), "NOPERM")

	mustHave(t, bob.cmd(`SETACL "Other Users/alice/Shared" bob lrswi`), "NOPERM")
}

func TestACLSharedFolders(t *testing.T) {
	alice, bob := twoUsers(t)
	alice.ok("CREATE Shared")
	alice.append("Shared", "x@example.org", "hello", "body")

	// Not shared yet: invisible to bob, MYRIGHTS reports NONEXISTENT.
	mustLack(t, bob.ok(`LIST "" "*"`), "Other Users")

	mustHave(t, bob.cmd(`MYRIGHTS "Other Users/alice/Shared"`), "NONEXISTENT")

	alice.ok("SETACL Shared bob lrs")

	lines := alice.ok("GETACL Shared")
	mustHave(t, lines, `* ACL Shared alice lrswipkxtea bob lrs`)

	lines = bob.ok(`LIST "" "*"`)
	mustHave(t, lines, `"Other Users/alice/Shared"`, `(\Noselect \HasChildren) "/" "Other Users/alice"`)

	mustHave(t, bob.ok(`MYRIGHTS "Other Users/alice/Shared"`), `MYRIGHTS "Other Users/alice/Shared" lrs`)

	mustHave(t, bob.ok(`STATUS "Other Users/alice/Shared" (MESSAGES)`), `STATUS "Other Users/alice/Shared" (MESSAGES 1)`)

	checkSharedLimitedRights(t, bob)
	// Alice extends rights with a modifier; bob can now append and flag.
	alice.ok("SETACL Shared bob +wi")

	mustHave(t, bob.ok(`MYRIGHTS "Other Users/alice/Shared"`), ` lrswi`)

	bob.append(`"Other Users/alice/Shared"`, "bob@example.org", "from bob", "body")
	bob.ok(`SELECT "Other Users/alice/Shared"`)
	bob.ok(`STORE 2 +FLAGS (\Flagged)`)
	// Subscription of a shared folder is bob's own state.
	bob.ok(`SUBSCRIBE "Other Users/alice/Shared"`)

	mustHave(t, bob.ok(`LIST (SUBSCRIBED) "" "*"`), `\Subscribed \HasNoChildren) "/" "Other Users/alice/Shared"`)
	// Rename on alice's side moves the pointer.
	alice.ok("RENAME Shared Team")

	if lines = bob.ok(`LIST "" "Other Users/*"`); !has(lines, `"Other Users/alice/Team"`) || has(lines, "alice/Shared") {
		t.Fatal(lines)
	}
	// Revocation hides the mailbox again.
	alice.ok("DELETEACL Team bob")

	mustLack(t, bob.ok(`LIST "" "*"`), "Other Users")

	mustHave(t, bob.cmd(`SELECT "Other Users/alice/Team"`), "NONEXISTENT")
	// anyone grants without a pointer; direct access works, LIST stays quiet.
	alice.ok("SETACL Team anyone lr")

	mustHave(t, bob.ok(`MYRIGHTS "Other Users/alice/Team"`), " lr")

	mustHave(t, alice.cmd("SETACL Team alice lr"), "CANNOT")
}

func TestMetadata(t *testing.T) {
	alice, bob := twoUsers(t)
	// Server entries: private ones are per account, shared ones are global
	// and read-only for accounts.
	alice.ok(`SETMETADATA "" (/private/comment "mine")`)

	lines := alice.ok(`GETMETADATA "" (/shared/comment /private/comment)`)
	mustHave(t, lines, `* METADATA "" (/private/comment "mine")`)

	mustHave(t, alice.cmd(`SETMETADATA "" (/shared/comment "server wide")`), "NOPERM")

	mustLack(t, bob.ok(`GETMETADATA "" (/shared/comment /private/comment)`), "mine")

	alice.ok("CREATE Project")
	alice.ok(`SETMETADATA Project (/shared/comment "team box" /shared/vendor/x/color "red" /private/comment {5+}` + "\r\nhello)")

	mustHave(t, alice.ok(`GETMETADATA (DEPTH infinity) Project /shared`), `/shared/vendor/x/color "red"`, `/shared/comment "team box"`)

	if lines = alice.ok(`GETMETADATA (DEPTH 1) Project /shared`); has(lines, "color") || !has(lines, `/shared/comment`) {
		t.Fatal(lines)
	}

	mustHave(t, alice.ok(`GETMETADATA (MAXSIZE 3) Project /shared/comment`), "METADATA LONGENTRIES 8")
	// Removal with NIL; rename keeps the entries (keyed by id).
	alice.ok(`SETMETADATA Project (/shared/vendor/x/color NIL)`)
	alice.ok("RENAME Project Proj")

	if lines = alice.ok(`GETMETADATA (DEPTH infinity) Proj /shared`); has(lines, "color") || !has(lines, "team box") {
		t.Fatal(lines)
	}
	// Shared mailbox: r reads shared entries, w is needed to set them.
	alice.ok("SETACL Proj bob lr")

	mustHave(t, bob.ok(`GETMETADATA "Other Users/alice/Proj" /shared/comment`), `"team box"`)

	mustHave(t, bob.cmd(`SETMETADATA "Other Users/alice/Proj" (/shared/comment "nope")`), "NOPERM")

	bob.ok(`SETMETADATA "Other Users/alice/Proj" (/private/comment "bobs note")`)

	mustHave(t, bob.ok(`GETMETADATA "Other Users/alice/Proj" /private/comment`), `"bobs note"`)

	mustLack(t, alice.ok(`GETMETADATA Proj /private/comment`), "bobs note")
}

func TestCatenate(t *testing.T) {
	alice, _ := twoUsers(t)
	alice.append("INBOX", "x@example.org", "orig", "original body")
	// Text plus the whole original, then text plus one section.
	lines := alice.ok(`APPEND Drafts (\Draft) CATENATE (TEXT {13+}` + "\r\nX-Note: one\r\n" + ` URL "imap:///INBOX;UIDVALIDITY=1/;UID=1")`)
	mustHave(t, lines, "APPENDUID")

	alice.ok("SELECT Drafts")

	mustHave(t, alice.ok(`FETCH 1 (BODY.PEEK[HEADER.FIELDS (X-NOTE SUBJECT)])`), "X-Note: one", "Subject: orig")

	lines = alice.ok(`APPEND Drafts CATENATE (TEXT {22+}` + "\r\nSubject: part only\r\n\r\n" + ` URL "imap:///INBOX/;UID=1/;SECTION=TEXT")`)
	mustHave(t, lines, "APPENDUID")

	mustHave(t, alice.ok(`FETCH 2 (BODY.PEEK[TEXT])`), "original body")
	// Bad URL: wrong UIDVALIDITY, unknown message, no rights.
	mustHave(t, alice.cmd(`APPEND Drafts CATENATE (URL "imap:///INBOX;UIDVALIDITY=99/;UID=1")`), "BADURL")

	mustHave(t, alice.cmd(`APPEND Drafts CATENATE (URL "imap:///INBOX/;UID=77")`), "BADURL")

	mustHave(t, alice.cmd(`APPEND Drafts CATENATE (URL "imap:///Other%20Users/bob/INBOX/;UID=1")`), "BADURL")
}

func TestACLRoundFiveFixes(t *testing.T) {
	alice, bob := twoUsers(t)
	alice.ok("CREATE Box")
	alice.append("Box", "x@example.org", "m1", "body")
	alice.ok("SELECT Box")
	alice.ok(`STORE 1 +FLAGS (\Deleted $label1)`)
	alice.ok("SETACL Box bob lrsi")
	// A replacing STORE may not drop \Deleted or keywords without t/w.
	bob.ok(`SELECT "Other Users/alice/Box"`)

	mustHave(t, bob.cmd(`STORE 1 FLAGS (\Seen)`), "NOPERM")

	mustHave(t, bob.cmd(`STORE 1 FLAGS (\Seen \Deleted $label1)`), "OK")
	// SELECT reports the rights honestly: read-write with s, limited PERMANENTFLAGS.
	mustHave(t, bob.ok(`SELECT "Other Users/alice/Box"`), "READ-WRITE", `PERMANENTFLAGS (\Seen)]`)
	// APPEND with i but without t/w: disallowed flags are dropped, not stored.
	bob.ok(`APPEND "Other Users/alice/Box" (\Deleted \Seen $kw) {5+}` + "\r\nhello")
	bob.ok(`SELECT "Other Users/alice/Box"`)

	if lines := bob.ok(`FETCH 2 (FLAGS)`); !has(lines, `FLAGS (\Seen`) || has(lines, `\Deleted`) || has(lines, `$kw`) {
		t.Fatal(lines)
	}
	// MYRIGHTS works with i only; existence stays hidden without any right.
	alice.ok("SETACL Box bob i")

	mustHave(t, bob.ok(`MYRIGHTS "Other Users/alice/Box"`), ` i`)

	alice.ok("CREATE Secret")
	bob.ok("SELECT INBOX")
	bob.append("INBOX", "x@example.org", "mine", "body")
	bob.ok("SELECT INBOX")

	mustHave(t, bob.cmd(`COPY 1 "Other Users/alice/Secret"`), "NONEXISTENT")

	mustHave(t, bob.cmd(`COPY 1 "Other Users/alice/Missing"`), "NONEXISTENT")

	mustHave(t, bob.cmd(`CREATE "Other Users/alice/Secret/Sub"`), "NONEXISTENT")
	// Relative CATENATE URL; bare /private cannot be set.
	mustHave(t, alice.ok(`APPEND Box CATENATE (URL "/Box/;UID=1")`), "APPENDUID")

	mustHave(t, alice.cmd(`SETMETADATA Box (/private "x")`), "BAD")
}

func TestImapExtensionsRoundSix(t *testing.T) {
	alice, bob := twoUsers(t)
	alice.ok("CREATE Box")
	alice.append("Box", "x@example.org", "m1", "body")
	alice.ok("SETACL Box bob lrs")
	// LIST-MYRIGHTS
	mustHave(t, alice.ok(`LIST "" Box RETURN (MYRIGHTS)`), "* MYRIGHTS Box lrswipkxtea")

	mustHave(t, bob.ok(`LIST "" "Other Users/*" RETURN (MYRIGHTS)`), `* MYRIGHTS "Other Users/alice/Box" lrs`)
	// WITHIN
	alice.ok("SELECT Box")

	mustHave(t, alice.ok("SEARCH YOUNGER 3600"), "* SEARCH 1")

	mustLack(t, alice.ok("SEARCH OLDER 3600"), "* SEARCH 1")
	// REPLACE: new message stored, original expunged
	lines := alice.ok(`REPLACE 1 Box (\Seen) {36+}` + "\r\nSubject: replaced\r\n\r\nnew body here\r\n")
	mustHave(t, lines, "[APPENDUID", "* 1 EXPUNGE")

	alice.ok("NOOP") // picks up the EXISTS for the replacement

	mustHave(t, alice.ok("FETCH 1 (FLAGS BODY.PEEK[HEADER.FIELDS (SUBJECT)])"), "Subject: replaced")

	mustHave(t, alice.ok("STATUS Box (MESSAGES)"), "(MESSAGES 1)")
	// bob has no t/e on the shared mailbox: REPLACE denied
	bob.ok(`SELECT "Other Users/alice/Box"`)

	mustHave(t, bob.cmd(`REPLACE 1 INBOX {5+}`+"\r\nhello"), "NOPERM")
	// UNAUTHENTICATE returns to the not-authenticated state
	mustHave(t, alice.ok("UNAUTHENTICATE"), "OK")

	if lines = alice.cmd(`LIST "" "*"`); !has(lines, "BAD") && !has(lines, "NO") {
		t.Fatal(lines)
	}

	alice.ok("LOGIN alice a")

	mustHave(t, alice.ok(`LIST "" Box`), "Box")
}
