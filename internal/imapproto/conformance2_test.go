package imapproto

import (
	"strings"
	"testing"

	"shardpost/internal/model"
)

// Regressions from the imaptest suite (second run): each case mirrors
// one failing test group.

func TestThreadOrderedSubjectRootIsEarliest(t *testing.T) {
	c, _ := newTestServer(t)
	c.ok("LOGIN test test")
	c.ok("CREATE os")

	if n := appendMbox(t, c, "os", "testdata/thread-orderedsubject2.mbox"); n != 8 {
		t.Fatalf("appended %d", n)
	}

	c.ok("SELECT os")

	mustHave(t, c.ok("THREAD ORDEREDSUBJECT US-ASCII ALL"), "* THREAD (4 (2)(8)(6))(3 (1)(7)(5))")
}

func TestSortAddressesGroupsAndComments(t *testing.T) {
	c, _ := newTestServer(t)
	c.ok("LOGIN test test")
	c.ok("CREATE sa")
	appendMbox(t, c, "sa", "testdata/sort-addresses.mbox")
	c.ok("SELECT sa")

	for _, key := range []string{"FROM", "TO", "CC"} {
		if lines := c.ok("SORT (%s) US-ASCII ALL", key); !has(lines, "* SORT 3 1 2") {
			t.Errorf("%s: %v", key, lines)
		}
	}
}

func TestSearchAddressesParsed(t *testing.T) {
	c, _ := newTestServer(t)
	c.ok("LOGIN test test")
	c.ok("CREATE se")
	appendMbox(t, c, "se", "testdata/search-addresses.mbox")
	c.ok("SELECT se")
	// message 3 has "<user-from (comment)@ (comment) domain.org>"
	mustHave(t, c.ok("SEARCH FROM user-from@domain.org"), "* SEARCH 1 2 3 4 6 7")

	mustHave(t, c.ok("SEARCH HEADER From user-from@domain.org"), "* SEARCH 1 2 3 4 6 7")
	// an empty value only asks whether that header exists
	mustHave(t, c.ok(`SEARCH HEADER To ""`), "* SEARCH 1 2 3 4 6 7")

	mustHave(t, c.ok(`SEARCH HEADER Bcc ""`), "* SEARCH 1 2 3 4")
}

func TestFetchEnvelopeDefaultsAndGroups(t *testing.T) {
	c, _ := newTestServer(t)
	c.ok("LOGIN test test")
	c.ok("CREATE fe")
	appendMbox(t, c, "fe", "testdata/fetch-envelope.mbox")
	c.ok("SELECT fe")
	lines := c.ok("FETCH 1:* ENVELOPE")

	want := []string{
		`* 2 FETCH (ENVELOPE ("Thu, 15 Feb 2007 01:02:03 +0200" NIL ((NIL NIL "user" "domain")) ((NIL NIL "user" "domain")) ((NIL NIL "user" "domain")) NIL NIL NIL NIL NIL))`,
		`* 4 FETCH (ENVELOPE ("Thu, 15 Feb 2007 01:02:03 +0200" NIL (("Real Name" NIL "user" "domain")) (("Real Name" NIL "user" "domain")) (("Real Name" NIL "user" "domain")) ((NIL NIL "group" NIL)(NIL NIL "g1" "d1.org")(NIL NIL "g2" "d2.org")(NIL NIL NIL NIL)(NIL NIL "group2" NIL)(NIL NIL "g3" "d3.org")(NIL NIL NIL NIL)) ((NIL NIL "group" NIL)(NIL NIL NIL NIL)(NIL NIL "group2" NIL)(NIL NIL NIL NIL)) NIL NIL NIL))`,
		`* 5 FETCH (ENVELOPE ("Thu, 15 Feb 2007 01:02:03 +0200" NIL (("Real Name" NIL "user" "domain")) (("Real Name" NIL "user" "domain")) (("Real Name" NIL "user" "domain")) NIL NIL NIL NIL NIL))`,
		// The obsolete source route is the at-domain-list (adl).
		`* 6 FETCH (ENVELOPE ("Thu, 15 Feb 2007 01:02:03 +0200" NIL ((NIL "@route" "user" "domain")) ((NIL "@route" "user" "domain")) ((NIL "@route" "user" "domain")) NIL NIL NIL NIL NIL))`,
	}
	for _, w := range want {
		if !has(lines, w) {
			t.Errorf("missing %s\nin %s", w, strings.Join(lines, "\n"))
		}
	}
}

func TestESearchMinMaxModseq(t *testing.T) {
	c, _ := newTestServer(t)
	c.ok("LOGIN test test")
	c.ok("CREATE ec")

	for i := 0; i < 4; i++ {
		c.append("ec", "a@b", "m", "x")
	}

	c.ok("SELECT ec")
	c.ok("STORE 1 +FLAGS \\Seen")
	c.ok("STORE 3 +FLAGS \\Seen")
	c.ok("STORE 2 +FLAGS \\Seen")
	c.ok("STORE 4 +FLAGS \\Seen")

	mod := func(seq int) string {
		l := find(c.ok("FETCH %d (MODSEQ)", seq), "MODSEQ (")
		return l[strings.Index(l, "MODSEQ (")+8 : strings.LastIndex(l, ")")-1]
	}
	if lines := c.ok(`SEARCH RETURN (MIN) 1:3 MODSEQ "/flags/\\seen" all 1`); !has(lines, "MIN 1 MODSEQ "+mod(1)) {
		t.Fatal(lines)
	}

	if lines := c.ok(`SEARCH RETURN (MAX) 1:3 MODSEQ "/flags/\\seen" all 1`); !has(lines, "MAX 3 MODSEQ "+mod(3)) {
		t.Fatal(lines)
	}

	if lines := c.ok(`SEARCH RETURN () 1:3 MODSEQ "/flags/\\seen" all 1`); !has(lines, "ALL 1:3 MODSEQ "+mod(2)) {
		t.Fatal(lines)
	}
}

func TestMoveIntoSelectedMailbox(t *testing.T) {
	c, _ := newTestServer(t)
	c.ok("LOGIN test test")
	c.ok("CREATE mv")
	c.append("mv", "a@b", "one", "x")
	c.ok("SELECT mv")
	lines := c.ok("MOVE 1 mv")

	var order []string

	for _, l := range lines {
		switch {
		case strings.Contains(l, "COPYUID"):
			order = append(order, "copyuid")
		case strings.HasSuffix(l, "EXPUNGE"):
			order = append(order, l)
		case strings.HasSuffix(l, "EXISTS"):
			order = append(order, l)
		}
	}

	if got := strings.Join(order, ","); got != "copyuid,* 1 EXPUNGE,* 1 EXISTS" {
		t.Fatalf("got %q in %v", got, lines)
	}

	mustHave(t, c.ok("FETCH 1 UID"), "UID 2")
}

func TestListChildinfoOnlyForUnlistedChildren(t *testing.T) {
	c, _ := newTestServer(t)
	c.ok("LOGIN test test")
	c.ok("CREATE l/test2")
	c.ok("CREATE l/test2/test1")
	c.ok("SUBSCRIBE l/test2/test1")

	mustHave(t, c.ok(`LIST (SUBSCRIBED RECURSIVEMATCH) "" l/test2`), `* LIST () "/" l/test2 ("CHILDINFO" ("SUBSCRIBED"))`)

	lines := c.ok(`LIST (SUBSCRIBED RECURSIVEMATCH) "" (l/test2 l/test2/test1)`)
	if has(lines, "CHILDINFO") || !strings.Contains(find(lines, "l/test2/test1"), `\Subscribed`) {
		t.Fatal(lines)
	}
	// a deleted mailbox keeps its subscription (\NonExistent)
	c.ok("CREATE l/test6")
	c.ok("SUBSCRIBE l/test6")
	c.ok("DELETE l/test6")

	if lines := c.ok(`LIST (SUBSCRIBED) "" l/*`); !has(lines, `\NonExistent`) || !strings.Contains(find(lines, "l/test6"), `\Subscribed`) {
		t.Fatal(lines)
	}
}

func TestKeywordsAnnouncedBeforeFetch(t *testing.T) {
	c, _ := newTestServer(t)
	c.ok("LOGIN test test")
	c.ok("CREATE kw")
	c.append("kw", "a@b", "one", "x")
	c.ok("SELECT kw")
	lines := c.ok("STORE 1 +FLAGS ($hello NIL)")
	fl, fe := lastIndexes(lines, func(l string) bool {
		return strings.HasPrefix(l, "* FLAGS (") && strings.Contains(l, "$hello") && strings.Contains(strings.ToLower(l), "nil")
	}, func(l string) bool { return strings.Contains(l, "FETCH (FLAGS") })

	if fl < 0 || fe < 0 || fl > fe {
		t.Fatalf("FLAGS at %d, FETCH at %d: %v", fl, fe, lines)
	}
	// another session that appends a keyword: announced on poll first
	d, _ := newTestServerClient(t, c)
	d.ok("LOGIN test test")
	d.ok("APPEND kw ($world) {12+}\r\nSubject: x\r\n\r\n")
	lines = c.ok("NOOP")
	fl, fe = lastIndexes(lines, func(l string) bool {
		return strings.HasPrefix(l, "* FLAGS (") && strings.Contains(l, "$world")
	}, func(l string) bool { return strings.HasSuffix(l, "EXISTS") })

	if fl < 0 || fe < 0 || fl > fe {
		t.Fatalf("FLAGS at %d, EXISTS at %d: %v", fl, fe, lines)
	}
}

// lastIndexes returns the index of the last line matching a and of the
// last line matching b (-1 if none).
func lastIndexes(lines []string, a, b func(string) bool) (int, int) {
	ia, ib := -1, -1

	for i, l := range lines {
		if a(l) {
			ia = i
		}

		if b(l) {
			ib = i
		}
	}

	return ia, ib
}

func TestStatusRecentAfterSessionSawIt(t *testing.T) {
	c, _ := newTestServer(t)
	c.ok("LOGIN test test")
	c.ok("CREATE rc")
	c.ok("SELECT rc")
	d, _ := newTestServerClient(t, c)
	d.ok("LOGIN test test")
	d.append("rc", "a@b", "one", "x")

	mustHave(t, d.ok("STATUS rc (RECENT)"), "RECENT 1")

	mustHave(t, c.ok("NOOP"), "* 1 EXISTS")

	mustHave(t, d.ok("STATUS rc (RECENT)"), "RECENT 0")
	// the session that saw it keeps \Recent for the message
	mustHave(t, c.ok("FETCH 1 FLAGS"), `\Recent`)
}

func TestAppendBinaryReencoded(t *testing.T) {
	c, _ := newTestServer(t)
	c.ok("LOGIN test test")
	c.ok("CREATE bin")

	msg := "From: user@domain.org\r\nDate: Sat, 24 Mar 2007 23:00:00 +0200\r\nMime-Version: 1.0\r\nContent-Type: application/octet-stream\r\nContent-Transfer-Encoding: binary\r\n\r\n\x00-\xe4\r\n"
	c.ok("APPEND bin ~{%d+}\r\n%s", len(msg), msg)
	c.ok("SELECT bin")
	lines := c.ok("FETCH 1 (BODY.PEEK[1] BODY.PEEK[HEADER.FIELDS (Content-Transfer-Encoding)])")

	joined := strings.Join(lines, "\n")
	if strings.Contains(joined, "\x00") || !strings.Contains(joined, "AC3k") || !strings.Contains(strings.ToLower(joined), "content-transfer-encoding: base64") {
		t.Fatalf("%q", joined)
	}
	// BINARY still yields the decoded bytes
	if lines := c.ok("FETCH 1 (BINARY.PEEK[1])"); !strings.Contains(strings.Join(lines, "\n"), "\x00-\xe4") {
		t.Fatalf("%q", lines)
	}
}

// Review round 6 regressions.

func TestPartialRangesRejectedNotCrashing(t *testing.T) {
	c, _ := newTestServer(t)
	c.ok("LOGIN test test")
	c.ok("CREATE pr")
	c.append("pr", "a@b", "one", "x")
	c.ok("SELECT pr")

	for _, cmd := range []string{"FETCH 1 BODY[]<-1.1>", "FETCH 1 BODY[]<1.-1>"} {
		if lines := c.cmd(cmd); !strings.Contains(lines[len(lines)-1], " BAD") {
			t.Fatalf("%s: %v", cmd, lines)
		}
	}

	mustHave(t, c.ok("FETCH 1 BODY.PEEK[]<1.9223372036854775807>"), "BODY[]<1>")

	c.ok("FETCH 1 BODY.PEEK[]<9223372036854775807.1>") // out of range: empty, no crash
}

func TestSearchToCcBccDistinct(t *testing.T) {
	c, _ := newTestServer(t)
	c.ok("LOGIN test test")
	c.ok("CREATE tcc")

	msg := "From: a@x.org\r\nTo: to@x.org\r\nCc: cc@x.org\r\nSubject: s\r\n\r\nb\r\n"
	c.ok("APPEND tcc {%d+}\r\n%s", len(msg), msg)
	c.ok("SELECT tcc")

	if lines := c.ok("SEARCH TO cc@x.org"); !has(lines, "* SEARCH") || has(lines, "* SEARCH 1") {
		t.Fatal(lines)
	}

	mustHave(t, c.ok("SEARCH CC cc@x.org"), "* SEARCH 1")

	mustLack(t, c.ok("SEARCH FROM to@x.org"), "* SEARCH 1")
}

func TestNestedModseqInSearchResponse(t *testing.T) {
	c, _ := newTestServer(t)
	c.ok("LOGIN test test")
	c.ok("CREATE nm")
	c.append("nm", "a@b", "one", "x")
	c.ok("SELECT nm")

	mustHave(t, c.ok("SEARCH RETURN (MIN) OR (MODSEQ 1) ALL"), "MODSEQ")

	if lines := c.ok("SEARCH NOT MODSEQ 999999"); !strings.Contains(find(lines, "* SEARCH"), "MODSEQ") {
		t.Fatal(lines)
	}
}

func TestExamineDoesNotClaimRecent(t *testing.T) {
	c, _ := newTestServer(t)
	c.ok("LOGIN test test")
	c.ok("CREATE ex")
	c.ok("EXAMINE ex")
	d, _ := newTestServerClient(t, c)
	d.ok("LOGIN test test")
	d.append("ex", "a@b", "one", "x")
	c.ok("NOOP")

	mustHave(t, d.ok("STATUS ex (RECENT)"), "RECENT 1")
}

func TestReplaceFlagsKeepsProtectedFlags(t *testing.T) {
	// rights lrs: \Seen may change, \Deleted (needs d) and \Flagged
	// (needs w) not; the current state carries a \Deleted the session
	// never saw
	got := replaceFlags("lrs", []string{`\seen`, `\flagged`})([]string{`\deleted`, `\answered`})

	want := []string{`\seen`, `\deleted`, `\answered`} // \deleted needs t, \answered needs w: both kept
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("got %v want %v", got, want)
	}

	if got := replaceFlags(model.AllRights, []string{`\seen`})([]string{`\deleted`}); len(got) != 1 || got[0] != `\seen` {
		t.Fatal(got)
	}
}
