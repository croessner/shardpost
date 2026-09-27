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

	return twoUsersWith(t)
}

// twoUsersWith is twoUsers with the server options adjusted.
func twoUsersWith(t *testing.T, customize ...func(*Options)) (alice, bob *client) {
	t.Helper()

	store := memstore.New()

	opts := Options{Store: store, Auth: auth.Static{"alice": "a", "bob": "b"}, InsecureAuth: true, Autocreate: DefaultSpecialFolders,
		SharedPrefix: DefaultSharedPrefix}
	for _, fn := range customize {
		fn(&opts)
	}

	srv := New(opts)

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
	bob.ok(`SELECT "Shared/alice/Shared"`)
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
	mustHave(t, bob.cmd(`APPEND "Shared/alice/Shared" {5}`), "NOPERM")

	mustHave(t, bob.cmd(`SETACL "Shared/alice/Shared" bob lrswi`), "NOPERM")
}

func TestACLSharedFolders(t *testing.T) {
	alice, bob := twoUsers(t)
	alice.ok("CREATE Shared")
	alice.append("Shared", "x@example.org", "hello", "body")

	// Not shared yet: invisible to bob, MYRIGHTS reports NONEXISTENT.
	mustLack(t, bob.ok(`LIST "" "*"`), "Shared")

	mustHave(t, bob.cmd(`MYRIGHTS "Shared/alice/Shared"`), "NONEXISTENT")

	alice.ok("SETACL Shared bob lrs")

	lines := alice.ok("GETACL Shared")
	mustHave(t, lines, `* ACL Shared alice lrswipkxtea bob lrs`)

	lines = bob.ok(`LIST "" "*"`)
	mustHave(t, lines, `"/" Shared/alice/Shared`, `(\Noselect \HasChildren) "/" Shared/alice`)

	mustHave(t, bob.ok(`MYRIGHTS "Shared/alice/Shared"`), `MYRIGHTS Shared/alice/Shared lrs`)

	mustHave(t, bob.ok(`STATUS "Shared/alice/Shared" (MESSAGES)`), `STATUS Shared/alice/Shared (MESSAGES 1)`)

	checkSharedLimitedRights(t, bob)
	// Alice extends rights with a modifier; bob can now append and flag.
	alice.ok("SETACL Shared bob +wi")

	mustHave(t, bob.ok(`MYRIGHTS "Shared/alice/Shared"`), ` lrswi`)

	bob.append(`"Shared/alice/Shared"`, "bob@example.org", "from bob", "body")
	bob.ok(`SELECT "Shared/alice/Shared"`)
	bob.ok(`STORE 2 +FLAGS (\Flagged)`)
	// Subscription of a shared folder is bob's own state.
	bob.ok(`SUBSCRIBE "Shared/alice/Shared"`)

	mustHave(t, bob.ok(`LIST (SUBSCRIBED) "" "*"`), `\Subscribed \HasNoChildren) "/" Shared/alice/Shared`)
	// Rename on alice's side moves the pointer.
	alice.ok("RENAME Shared Team")

	if lines = bob.ok(`LIST "" "Shared/*"`); !has(lines, `"/" Shared/alice/Team`) || has(lines, "alice/Shared") {
		t.Fatal(lines)
	}
	// Revocation hides the mailbox again.
	alice.ok("DELETEACL Team bob")

	mustLack(t, bob.ok(`LIST "" "*"`), "Shared")

	mustHave(t, bob.cmd(`SELECT "Shared/alice/Team"`), "NONEXISTENT")
	// anyone grants without a pointer; direct access works, LIST stays quiet.
	alice.ok("SETACL Team anyone lr")

	mustHave(t, bob.ok(`MYRIGHTS "Shared/alice/Team"`), " lr")

	mustHave(t, alice.cmd("SETACL Team alice lr"), "CANNOT")

	// A "/" would move the boundary of the grantee's pointer key.
	mustHave(t, alice.cmd(`SETACL Team "bob/x" lr`), "BAD")
	mustHave(t, alice.cmd(`DELETEACL Team "bob/x"`), "BAD")
}

// TestMetadataServerManaged: the entries the JMAP front-end keeps can be
// read over IMAP, not written or removed, in any spelling.
func TestMetadataServerManaged(t *testing.T) {
	alice, _ := twoUsers(t)

	for _, entry := range []string{"/private/vendor/shardpost/jmap/vacation", "/private/vendor/shardpost/jmap/identities",
		"/private/Vendor/ShardPost/JMAP/vacation", "/private/vendor/shardpost/jmap/other"} {
		mustHave(t, alice.cmd(`SETMETADATA "" (`+entry+` "{\"isEnabled\":true}")`), "NOPERM")
		mustHave(t, alice.cmd(`SETMETADATA "" (`+entry+` NIL)`), "NOPERM")
	}

	mustHave(t, alice.cmd(`SETMETADATA "" (/private/comment "x" /private/vendor/shardpost/jmap/vacation "y")`), "NOPERM")
	mustLack(t, alice.ok(`GETMETADATA (DEPTH infinity) "" /private`), "/private/comment", "vendor/shardpost")

	alice.ok(`GETMETADATA "" (/private/vendor/shardpost/jmap/vacation)`)
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

	mustHave(t, bob.ok(`GETMETADATA "Shared/alice/Proj" /shared/comment`), `"team box"`)

	mustHave(t, bob.cmd(`SETMETADATA "Shared/alice/Proj" (/shared/comment "nope")`), "NOPERM")

	bob.ok(`SETMETADATA "Shared/alice/Proj" (/private/comment "bobs note")`)

	mustHave(t, bob.ok(`GETMETADATA "Shared/alice/Proj" /private/comment`), `"bobs note"`)

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

	mustHave(t, alice.cmd(`APPEND Drafts CATENATE (URL "imap:///Shared/bob/INBOX/;UID=1")`), "BADURL")
}

func TestACLRoundFiveFixes(t *testing.T) {
	alice, bob := twoUsers(t)
	alice.ok("CREATE Box")
	alice.append("Box", "x@example.org", "m1", "body")
	alice.ok("SELECT Box")
	alice.ok(`STORE 1 +FLAGS (\Deleted $label1)`)
	alice.ok("SETACL Box bob lrsi")
	// A replacing STORE may not drop \Deleted or keywords without t/w.
	bob.ok(`SELECT "Shared/alice/Box"`)

	mustHave(t, bob.cmd(`STORE 1 FLAGS (\Seen)`), "NOPERM")

	mustHave(t, bob.cmd(`STORE 1 FLAGS (\Seen \Deleted $label1)`), "OK")
	// SELECT reports the rights honestly: read-write with s, limited PERMANENTFLAGS.
	mustHave(t, bob.ok(`SELECT "Shared/alice/Box"`), "READ-WRITE", `PERMANENTFLAGS (\Seen)]`)
	// APPEND with i but without t/w: disallowed flags are dropped, not stored.
	bob.ok(`APPEND "Shared/alice/Box" (\Deleted \Seen $kw) {5+}` + "\r\nhello")
	bob.ok(`SELECT "Shared/alice/Box"`)

	if lines := bob.ok(`FETCH 2 (FLAGS)`); !has(lines, `FLAGS (\Seen`) || has(lines, `\Deleted`) || has(lines, `$kw`) {
		t.Fatal(lines)
	}
	// MYRIGHTS works with i only; existence stays hidden without any right.
	alice.ok("SETACL Box bob i")

	mustHave(t, bob.ok(`MYRIGHTS "Shared/alice/Box"`), ` i`)

	alice.ok("CREATE Secret")
	bob.ok("SELECT INBOX")
	bob.append("INBOX", "x@example.org", "mine", "body")
	bob.ok("SELECT INBOX")

	mustHave(t, bob.cmd(`COPY 1 "Shared/alice/Secret"`), "NONEXISTENT")

	mustHave(t, bob.cmd(`COPY 1 "Shared/alice/Missing"`), "NONEXISTENT")

	mustHave(t, bob.cmd(`CREATE "Shared/alice/Secret/Sub"`), "NONEXISTENT")
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

	mustHave(t, bob.ok(`LIST "" "Shared/*" RETURN (MYRIGHTS)`), `* MYRIGHTS Shared/alice/Box lrs`)
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
	bob.ok(`SELECT "Shared/alice/Box"`)

	mustHave(t, bob.cmd(`REPLACE 1 INBOX {5+}`+"\r\nhello"), "NOPERM")
	// UNAUTHENTICATE returns to the not-authenticated state
	mustHave(t, alice.ok("UNAUTHENTICATE"), "OK")

	if lines = alice.cmd(`LIST "" "*"`); !has(lines, "BAD") && !has(lines, "NO") {
		t.Fatal(lines)
	}

	alice.ok("LOGIN alice a")

	mustHave(t, alice.ok(`LIST "" Box`), "Box")
}

// TestSharedPrefixConfigurable: the visible root of the shared
// mailboxes is a setting. A server migrated from another one keeps its
// clients' name ("Other Users/"), and an empty prefix takes the
// namespace away from clients entirely — the mailboxes stay shared in
// the store.
func TestSharedPrefixConfigurable(t *testing.T) {
	for _, tc := range []struct {
		name   string
		prefix string
	}{
		{"the default", DefaultSharedPrefix},
		{"a name of its own", "Other Users/"},
		{"switched off", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			alice, bob := twoUsersWith(t, func(o *Options) { o.SharedPrefix = tc.prefix })

			alice.ok("CREATE Box")
			alice.append("Box", "x@example.org", "hello", "body")
			alice.ok("SETACL Box bob lrs")

			ns := strings.Join(bob.cmd("NAMESPACE"), "\n")
			list := strings.Join(bob.cmd(`LIST "" "*"`), "\n")

			if tc.prefix == "" {
				if !strings.Contains(ns, `(("" "/")) NIL`) {
					t.Errorf("NAMESPACE announces a shared namespace: %s", ns)
				}

				if strings.Contains(list, "alice") {
					t.Errorf("a shared mailbox is listed although the namespace is off: %s", list)
				}

				if lines := strings.Join(bob.cmd(`SELECT "Shared/alice/Box"`), "\n"); !strings.Contains(lines, "NONEXISTENT") {
					t.Errorf("a shared mailbox is reachable although the namespace is off: %s", lines)
				}

				return
			}

			if !strings.Contains(ns, `(("`+tc.prefix+`" "/"))`) {
				t.Errorf("NAMESPACE does not announce %q: %s", tc.prefix, ns)
			}

			if !strings.Contains(list, tc.prefix+"alice/Box") {
				t.Errorf("the shared mailbox is not listed under %q: %s", tc.prefix, list)
			}

			if lines := strings.Join(bob.cmd(`SELECT "%s"`, tc.prefix+"alice/Box"), "\n"); !strings.Contains(lines, "1 EXISTS") {
				t.Errorf("the shared mailbox does not open under %q: %s", tc.prefix, lines)
			}
		})
	}
}

// TestSharedPrefixCollision: an account whose own mailbox is named like
// the root of the shared namespace keeps its own listing — the implicit
// node of the namespace must not double the line. The own mailbox stays
// reachable; a name below the prefix does not, which is why the server
// warns and refuses to create one.
func TestSharedPrefixCollision(t *testing.T) {
	alice, bob := twoUsers(t)

	bob.ok("CREATE Shared")
	bob.append("Shared", "x@example.org", "own mail", "body")

	alice.ok("CREATE Box")
	alice.ok("SETACL Box bob lrs")

	lines := bob.ok(`LIST "" "*"`)

	own := 0

	for _, l := range lines {
		if strings.HasSuffix(l, `"/" Shared`) {
			own++
		}
	}

	if own != 1 {
		t.Errorf("the mailbox named like the namespace is listed %d times:\n%s", own, strings.Join(lines, "\n"))
	}

	mustHave(t, lines, `"/" Shared/alice/Box`)

	if l := strings.Join(bob.cmd("SELECT Shared"), "\n"); !strings.Contains(l, "1 EXISTS") {
		t.Errorf("the account's own mailbox is not reachable: %s", l)
	}
	// A client that walks the hierarchy with % must be told that there
	// is something below the name, or it never asks for it.
	for _, cmd := range []string{`LIST "" "%"`, `LIST "" "%" RETURN (CHILDREN)`} {
		for _, l := range bob.ok("%s", cmd) {
			if strings.HasSuffix(l, `"/" Shared`) && !strings.Contains(l, `\HasChildren`) {
				t.Errorf("%s hides the shared namespace: %s", cmd, l)
			}
		}
	}
}

// TestSharedNamespaceSubscriptionsInList: a subscribed mailbox of
// another account has to be findable the way a client looks for it —
// through CHILDINFO when it asks recursively, and through the
// \Noselect parent of LSUB — whether or not the account happens to own
// a mailbox named like the namespace.
func TestSharedNamespaceSubscriptionsInList(t *testing.T) {
	for _, own := range []bool{false, true} {
		name := "without a mailbox of its own"
		if own {
			name = "with a mailbox named like the namespace"
		}

		t.Run(name, func(t *testing.T) {
			alice, bob := twoUsers(t)

			if own {
				bob.ok("CREATE Shared")
			}

			alice.ok("CREATE Box")
			alice.ok("SETACL Box bob lrs")
			bob.ok(`SUBSCRIBE "Shared/alice/Box"`)

			rec := strings.Join(bob.cmd(`LIST (SUBSCRIBED RECURSIVEMATCH) "" "%%"`), "\n")
			if !strings.Contains(rec, `"/" Shared ("CHILDINFO" ("SUBSCRIBED"))`) {
				t.Errorf("RECURSIVEMATCH does not point at the subscription below Shared:\n%s", rec)
			}

			lsub := strings.Join(bob.cmd(`LSUB "" "%%"`), "\n")
			if !strings.Contains(lsub, `"/" Shared`) {
				t.Errorf("LSUB hides the parent of a subscribed mailbox:\n%s", lsub)
			}
		})
	}
}

// TestChildInfoOnMatchingParent: RFC 5258 3.5, example 8A1 — a mailbox
// that is subscribed itself and has a subscribed child the pattern does
// not list is reported with CHILDINFO as well, so the client knows to
// look further. This holds for the account's own mailboxes and for the
// mailboxes of other accounts.
func TestChildInfoOnMatchingParent(t *testing.T) {
	alice, bob := twoUsers(t)

	// Own: a subscribed parent with a subscribed child.
	bob.ok("CREATE Projekt")
	bob.ok("CREATE Projekt/2026")
	bob.ok("SUBSCRIBE Projekt")
	bob.ok("SUBSCRIBE Projekt/2026")

	// Shared: the same shape below another account.
	alice.ok("CREATE Team")
	alice.ok("CREATE Team/2026")
	alice.ok("SETACL Team bob lrs")
	alice.ok("SETACL Team/2026 bob lrs")
	bob.ok(`SUBSCRIBE "Shared/alice/Team"`)
	bob.ok(`SUBSCRIBE "Shared/alice/Team/2026"`)

	lines := strings.Join(bob.cmd(`LIST (SUBSCRIBED RECURSIVEMATCH) "" "%%"`), "\n")

	for _, want := range []string{
		`"/" Projekt ("CHILDINFO" ("SUBSCRIBED"))`,
		`"/" Shared ("CHILDINFO" ("SUBSCRIBED"))`,
	} {
		if !strings.Contains(lines, want) {
			t.Errorf("missing %s in:\n%s", want, lines)
		}
	}
	// The line above enters the shared tree through its root, which is
	// not a mailbox. This one is: an entry that matches the selection
	// itself and still stands in for a subscribed child below it.
	deepShared := strings.Join(bob.cmd(`LIST (SUBSCRIBED RECURSIVEMATCH) "" "Shared/alice/%%"`), "\n")
	if !strings.Contains(deepShared, `Shared/alice/Team ("CHILDINFO" ("SUBSCRIBED"))`) {
		t.Errorf("no CHILDINFO on a shared mailbox that matches itself:\n%s", deepShared)
	}

	// The child itself is listed by this pattern, so no CHILDINFO
	// there. RFC 5258 3.5 only requires the suppression for a name that
	// does not match; we are stricter, and clients treat the extra
	// CHILDINFO as redundant either way.
	deep := strings.Join(bob.cmd(`LIST (SUBSCRIBED RECURSIVEMATCH) "" "Projekt/%%"`), "\n")
	if strings.Contains(deep, "CHILDINFO") {
		t.Errorf("CHILDINFO although the child is listed itself:\n%s", deep)
	}
}

// TestAutocreateIsConfigurable: which mailboxes an account gets, and
// whether they are subscribed, comes from the configuration. A mailbox
// that is added later reaches the accounts that log in after it; one
// that the account unsubscribed stays unsubscribed.
func TestAutocreateIsConfigurable(t *testing.T) {
	no := []SpecialFolder{{Name: "Ablage", Use: `\Archive`}, {Name: "Entwürfe", Use: `\Drafts`, Subscribe: true}}

	c, store := newTestServer(t, func(o *Options, _ *memstore.Store) { o.Autocreate = no })
	defer func() { _ = c.conn.Close() }()

	c.ok("LOGIN test test")

	lines := strings.Join(c.cmd(`LIST "" "*"`), "\n")
	for _, want := range []string{`"/" INBOX`, `"/" Ablage`, `"/" Entwürfe`} {
		if !strings.Contains(lines, want) {
			t.Errorf("missing %s in:\n%s", want, lines)
		}
	}

	if strings.Contains(lines, "Drafts") || strings.Contains(lines, "Junk") {
		t.Errorf("mailboxes that are not configured were created:\n%s", lines)
	}

	// Subscribed only where the configuration says so.
	lsub := strings.Join(c.cmd(`LSUB "" "*"`), "\n")
	if !strings.Contains(lsub, "Entwürfe") || strings.Contains(lsub, "Ablage") {
		t.Errorf("subscriptions do not follow the configuration:\n%s", lsub)
	}

	// The account unsubscribes one and logs in again: it stays that way.
	c.ok(`UNSUBSCRIBE "Entwürfe"`)
	c.ok("LOGOUT")

	again, _ := newTestServerClient(t, c)
	defer func() { _ = again.conn.Close() }()

	again.ok("LOGIN test test")

	if lsub := strings.Join(again.cmd(`LSUB "" "*"`), "\n"); strings.Contains(lsub, "Entwürfe") {
		t.Errorf("the login subscribed a mailbox the account had unsubscribed:\n%s", lsub)
	}

	// A mailbox added to the configuration later is created at the next
	// login, for an account that exists already: same store, longer list.
	u, _ := store.User("test")
	if _, err := u.Lookup("Später"); err == nil {
		t.Fatal("the mailbox exists before it is configured")
	}

	third, _ := newTestServer(t, func(o *Options, _ *memstore.Store) {
		o.Store = store

		o.Autocreate = append(append([]SpecialFolder{}, no...), SpecialFolder{Name: "Später", Subscribe: true})
	})
	defer func() { _ = third.conn.Close() }()

	third.ok("LOGIN test test")

	if lines := strings.Join(third.cmd(`LIST "" "*"`), "\n"); !strings.Contains(lines, "Später") {
		t.Errorf("a mailbox added to the configuration did not reach an existing account:\n%s", lines)
	}
}

// TestTrailingDataIsRefused: a command that carries more than its
// arguments is a syntax error. It used to be accepted, and that was
// dangerous rather than lenient: "SUBSCRIBE Entwürfe" stops at the
// umlaut, because an unquoted name is an atom, and the server would
// have subscribed "Entw" and answered OK.
func TestTrailingDataIsRefused(t *testing.T) {
	c, store := newTestServer(t)
	defer func() { _ = c.conn.Close() }()

	c.ok("LOGIN test test")

	for _, cmd := range []string{"SUBSCRIBE INBOX nonsense", "UNSUBSCRIBE INBOX nonsense", "SELECT INBOX nonsense", "DELETE Junk nonsense", "SUBSCRIBE Entwürfe"} {
		if lines := strings.Join(c.cmd("%s", cmd), "\n"); !strings.Contains(lines, "BAD") {
			t.Errorf("%q was accepted: %s", cmd, lines)
		}
	}

	// Quoted, the same name is perfectly fine.
	c.ok(`CREATE "Entwürfe"`)
	c.ok(`SUBSCRIBE "Entwürfe"`)

	u, _ := store.User("test")

	boxes, err := u.ListMailboxes()
	if err != nil {
		t.Fatal(err)
	}

	for _, b := range boxes {
		if b.Name == "Entw" {
			t.Error("a truncated mailbox name was created")
		}
	}
}
