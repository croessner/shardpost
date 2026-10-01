package imapproto

import (
	"fmt"
	"strings"
	"testing"
)

// esearchLine returns the untagged ESEARCH line of a response, "" if
// there is none.
func esearchLine(lines []string) string {
	for _, l := range lines {
		if l = strings.TrimRight(l, "\r\n"); strings.HasPrefix(l, "* ESEARCH ") {
			return l
		}
	}

	return ""
}

// mustESearch runs a command that must succeed and checks its ESEARCH
// line exactly: `* ESEARCH (TAG "<tag>")` followed by want.
func mustESearch(t *testing.T, c *client, cmd, want string) {
	t.Helper()

	lines := c.ok("%s", cmd)

	exp := fmt.Sprintf(`* ESEARCH (TAG "t%d")`, c.n)
	if want != "" {
		exp += " " + want
	}

	if got := esearchLine(lines); got != exp {
		t.Fatalf("%s: got %q want %q (%v)", cmd, got, exp, lines)
	}

	mustLack(t, lines, "* SORT")
}

// newESortBox selects a mailbox whose sequence numbers and UIDs differ:
// seq 1 = UID 2 "c", seq 2 = UID 3 "a", seq 3 = UID 4 "b".
func newESortBox(t *testing.T) *client {
	t.Helper()

	c, _ := newTestServer(t)
	c.ok("LOGIN test test")
	c.ok("CREATE es")

	for _, s := range []string{"gone", "c", "a", "b"} {
		c.append("es", "x@example.org", s, "body "+s)
	}

	c.ok("SELECT es")
	c.ok(`STORE 1 +FLAGS.SILENT (\Deleted)`)
	c.ok("EXPUNGE")

	return c
}

// TestESortReturnOptions: SORT RETURN answers ESEARCH in sort order
// (RFC 5267 3.1): MIN/MAX are the first/last sorted message, ALL is the
// sorted list with only ascending runs as ranges, () means ALL.
func TestESortReturnOptions(t *testing.T) {
	c := newESortBox(t)

	mustHave(t, c.ok("CAPABILITY"), " ESORT")

	// Classic SORT is unchanged.
	mustHave(t, c.ok("SORT (SUBJECT) UTF-8 ALL"), "* SORT 2 3 1")

	mustESearch(t, c, "SORT RETURN (MIN) (SUBJECT) UTF-8 ALL", "MIN 2")
	mustESearch(t, c, "SORT RETURN (MAX) (SUBJECT) UTF-8 ALL", "MAX 1")
	mustESearch(t, c, "SORT RETURN (COUNT) (SUBJECT) UTF-8 ALL", "COUNT 3")
	mustESearch(t, c, "SORT RETURN (ALL) (SUBJECT) UTF-8 ALL", "ALL 2:3,1")
	mustESearch(t, c, "SORT RETURN () (SUBJECT) UTF-8 ALL", "ALL 2:3,1")
	mustESearch(t, c, "sort return (min max all count) (subject) utf-8 all", "MIN 2 MAX 1 ALL 2:3,1 COUNT 3")

	// A descending run is never a range (3.2).
	mustESearch(t, c, "SORT RETURN (ALL) (REVERSE ARRIVAL) UTF-8 ALL", "ALL 3,2,1")
	mustESearch(t, c, "SORT RETURN (ALL) (ARRIVAL) UTF-8 ALL", "ALL 1:3")

	// UID SORT: UIDs, in sort order, with the UID marker.
	mustHave(t, c.ok("UID SORT (SUBJECT) UTF-8 ALL"), "* SORT 3 4 2")
	mustESearch(t, c, "UID SORT RETURN (MIN MAX ALL COUNT) (SUBJECT) UTF-8 ALL", "UID MIN 3 MAX 2 ALL 3:4,2 COUNT 3")
	mustESearch(t, c, "UID SORT RETURN (ALL) (REVERSE SUBJECT) UTF-8 ALL", "UID ALL 2,4,3")

	// Search keys narrow the sorted result.
	mustESearch(t, c, "SORT RETURN (ALL) (REVERSE SUBJECT) UTF-8 NOT SUBJECT a", "ALL 1,3")
}

// TestESortEmptyResult: no match gives only COUNT 0 (RFC 4731 3.1).
func TestESortEmptyResult(t *testing.T) {
	c := newESortBox(t)

	mustESearch(t, c, "SORT RETURN (MIN MAX ALL COUNT) (SUBJECT) UTF-8 SUBJECT nothing-like-this", "COUNT 0")
	mustESearch(t, c, "UID SORT RETURN (MIN MAX ALL COUNT) (SUBJECT) UTF-8 SUBJECT nothing-like-this", "UID COUNT 0")
	mustESearch(t, c, "SORT RETURN (ALL) (SUBJECT) UTF-8 SUBJECT nothing-like-this", "")
}

// TestESortSave: RETURN (SAVE) on SORT fills "$" (RFC 5182): alone it
// sends no ESEARCH, with only MIN/MAX "$" holds those messages, with ALL
// or COUNT every match.
func TestESortSave(t *testing.T) {
	c := newESortBox(t)

	lines := c.ok("SORT RETURN (SAVE) (SUBJECT) UTF-8 NOT SUBJECT b")
	mustLack(t, lines, "* ESEARCH", "* SORT")
	mustHave(t, c.ok("UID SEARCH UID $"), "* SEARCH 2 3")
	mustHave(t, c.ok("FETCH $ (UID)"), "* 1 FETCH (UID 2)", "* 2 FETCH (UID 3)")

	mustESearch(t, c, "SORT RETURN (SAVE MIN) (SUBJECT) UTF-8 ALL", "MIN 2")
	mustHave(t, c.ok("UID SEARCH UID $"), "* SEARCH 3")

	mustESearch(t, c, "UID SORT RETURN (SAVE MAX) (SUBJECT) UTF-8 ALL", "UID MAX 2")
	mustHave(t, c.ok("UID SEARCH UID $"), "* SEARCH 2")

	mustESearch(t, c, "SORT RETURN (SAVE MIN MAX) (SUBJECT) UTF-8 ALL", "MIN 2 MAX 1")
	mustHave(t, c.ok("UID SEARCH UID $"), "* SEARCH 2 3")

	mustESearch(t, c, "SORT RETURN (SAVE MIN COUNT) (SUBJECT) UTF-8 NOT SUBJECT c", "MIN 2 COUNT 2")
	mustHave(t, c.ok("UID SEARCH UID $"), "* SEARCH 3 4")

	// One match: MIN and MAX are the same message, saved once.
	mustESearch(t, c, "SORT RETURN (SAVE MIN MAX) (SUBJECT) UTF-8 SUBJECT b", "MIN 3 MAX 3")
	mustHave(t, c.ok("UID SEARCH UID $"), "* SEARCH 4")

	// An empty result empties "$".
	c.ok("SORT RETURN (SAVE) (SUBJECT) UTF-8 SUBJECT nothing-like-this")
	mustLack(t, c.ok("UID SEARCH UID $"), "* SEARCH ")

	// SEARCH follows the same SAVE rules.
	mustLack(t, c.ok("SEARCH RETURN (SAVE) ALL"), "* ESEARCH")
	mustESearch(t, c, "SEARCH RETURN (SAVE MAX) ALL", "MAX 3")
	mustHave(t, c.ok("UID SEARCH UID $"), "* SEARCH 4")
}

// TestESortBad: an unknown return option, a missing option list or a
// stray atom is BAD, for SORT and SEARCH alike (RFC 4466 2.6.1); CONTEXT
// and PARTIAL are not offered.
func TestESortBad(t *testing.T) {
	c := newESortBox(t)

	for _, cmd := range []string{
		"SORT RETURN (FOO) (SUBJECT) UTF-8 ALL",
		"SORT RETURN (UPDATE) (SUBJECT) UTF-8 ALL",
		"SORT RETURN (PARTIAL 1:5) (SUBJECT) UTF-8 ALL",
		"SORT RETURN MIN (SUBJECT) UTF-8 ALL",
		"SORT FOO (SUBJECT) UTF-8 ALL",
		"UID SORT RETURN (MIN FOO) (SUBJECT) UTF-8 ALL",
		"SEARCH RETURN (FOO) ALL",
		"SEARCH RETURN MIN ALL",
	} {
		mustHave(t, c.cmd("%s", cmd), " BAD ")
	}
}

// TestESortModseq: a MODSEQ key adds the highest modseq to SORT (RFC 7162
// 3.1.9) and to the ESORT result (3.1.10).
func TestESortModseq(t *testing.T) {
	c := newESortBox(t)

	lines := c.ok("SORT (SUBJECT) UTF-8 MODSEQ 1")
	mustHave(t, lines, "* SORT 2 3 1 (MODSEQ ")

	lines = c.ok("SORT RETURN (ALL) (SUBJECT) UTF-8 MODSEQ 1")
	if l := esearchLine(lines); !strings.Contains(l, "ALL 2:3,1 MODSEQ ") {
		t.Fatal(lines)
	}

	lines = c.ok("SORT (SUBJECT) UTF-8 MODSEQ 1 SUBJECT nothing-like-this")
	mustLack(t, lines, "MODSEQ")
}

// TestESortIMAP4rev2: ENABLE IMAP4rev2 changes SEARCH, not SORT (RFC 5256
// is not part of RFC 9051): without RETURN it stays * SORT.
func TestESortIMAP4rev2(t *testing.T) {
	c := newESortBox(t)

	mustHave(t, c.ok("ENABLE IMAP4rev2"), "* ENABLED IMAP4rev2")

	lines := c.ok("SORT (SUBJECT) UTF-8 ALL")
	mustHave(t, lines, "* SORT 2 3 1")
	mustLack(t, lines, "* ESEARCH")

	mustESearch(t, c, "SORT RETURN (ALL) (SUBJECT) UTF-8 ALL", "ALL 2:3,1")
}

func TestFormatOrderedSet(t *testing.T) {
	cases := map[string][]uint32{
		"":             {},
		"5":            {5},
		"1:3":          {1, 2, 3},
		"3,2,1":        {3, 2, 1},
		"4:6,2,9:11,1": {4, 5, 6, 2, 9, 10, 11, 1},
		"7,7":          {7, 7},
	}
	for want, in := range cases {
		if got := formatOrderedSet(in); got != want {
			t.Errorf("%v: got %q want %q", in, got, want)
		}
	}
}
