package imapproto

import (
	"strings"
	"testing"

	"shardpost/internal/memstore"
)

// searchCase is a SEARCH and the exact untagged reply it must give.
type searchCase struct{ search, want string }

// runSearches appends the messages to a new mailbox, selects it and
// checks the searches.
func runSearches(t *testing.T, c *client, mailbox string, msgs []string, cases []searchCase) {
	t.Helper()

	c.ok("CREATE %s", mailbox)

	for _, m := range msgs {
		c.ok("APPEND %s {%d+}\r\n%s", mailbox, len(m), m)
	}

	c.ok("SELECT %s", mailbox)

	for _, tc := range cases {
		if lines := c.ok("SEARCH CHARSET UTF-8 %s", literals(tc.search)); !hasExact(lines, tc.want) {
			t.Errorf("SEARCH %s: %v, want %q", tc.search, lines, tc.want)
		}
	}
}

// TestSearchImaptestBody mirrors imaptest's search-body: full words and
// substrings of them, in the body and (for TEXT) in any header.
func TestSearchImaptestBody(t *testing.T) {
	c, _ := newTestServer(t)
	c.ok("LOGIN test test")

	date := "Date: Sat, 24 Mar 2007 23:00:00 +0200\r\n"
	msgs := []string{
		date + "\r\nasdfghjkl zxcvbnm\r\n",
		date + "Subject: qwertyuiop\r\n\r\nasdfghjkl\r\n",
		date + "X-Header: qwertyuiop\r\n\r\nzxcvbnm\r\n",
		date + "\r\nzxcvbnm qwertyuiop\r\n",
	}

	runSearches(t, c, "sb", msgs, []searchCase{
		{"TEXT asdfghjkl", "* SEARCH 1 2"},
		{"TEXT zxcvbnm", "* SEARCH 1 3 4"},
		{"TEXT qwertyuiop", "* SEARCH 2 3 4"},
		{"BODY asdfghjkl", "* SEARCH 1 2"},
		{"BODY zxcvbnm", "* SEARCH 1 3 4"},
		{"BODY qwertyuiop", "* SEARCH 4"},
		{"TEXT sdfghjk", "* SEARCH 1 2"},
		{"TEXT xcvbn", "* SEARCH 1 3 4"},
		{"TEXT wertyuio", "* SEARCH 2 3 4"},
		{"BODY sdfghjk", "* SEARCH 1 2"},
		{"BODY xcvbn", "* SEARCH 1 3 4"},
		{"BODY wertyuio", "* SEARCH 4"},
	})
}

// TestSearchImaptestHeader mirrors imaptest's search-header: phrases
// that start and end inside words, in the subject and in another header
// field that occurs twice.
func TestSearchImaptestHeader(t *testing.T) {
	c, _ := newTestServer(t)
	c.ok("LOGIN test test")

	msgs := []string{
		"Subject: hello beautiful world\r\n\r\nbody\r\n",
		"X-Extra: hello beautiful header\r\nX-Extra: another one\r\n\r\nbody\r\n",
	}

	runSearches(t, c, "sh", msgs, []searchCase{
		{"SUBJECT hello", "* SEARCH 1"},
		{"SUBJECT beautiful", "* SEARCH 1"},
		{"SUBJECT world", "* SEARCH 1"},
		{`SUBJECT "hello beautiful"`, "* SEARCH 1"},
		{`SUBJECT "hello beautiful world"`, "* SEARCH 1"},
		{`SUBJECT "eautiful worl"`, "* SEARCH 1"},
		{`HEADER subject ""`, "* SEARCH 1"},
		{`NOT HEADER subject ""`, "* SEARCH 2"},
		{`HEADER x-extra ""`, "* SEARCH 2"},
		{`NOT HEADER x-extra ""`, "* SEARCH 1"},
		{"HEADER x-extra hello", "* SEARCH 2"},
		{`HEADER x-extra "hello beautiful"`, "* SEARCH 2"},
		{`HEADER x-extra "eautiful head"`, "* SEARCH 2"},
		{`HEADER x-extra "another"`, "* SEARCH 2"},
		{`HEADER x-extra "x-extra"`, "* SEARCH"}, // the value, not the name
	})
}

// TestSearchSubstringEdges: what word lookups alone would miss — the
// inside of a word, a phrase over two words, punctuation, one and two
// characters, a word longer than any index term and digits in a word —
// and what they would wrongly find: a stem that is not a substring, and
// words in the wrong order.
func TestSearchSubstringEdges(t *testing.T) {
	c, _ := newTestServer(t)
	c.ok("LOGIN test test")

	long := strings.Repeat("abcdefghij", 6) // one word of 60 letters
	msgs := []string{
		"Subject: beautiful day\r\n\r\nthe foo-bar invoice, ref abc123def\r\n",
		"Subject: other\r\n\r\nX " + long + " Y\r\n",
		"Subject: third\r\n\r\nbar foo invoices\r\n",
	}

	runSearches(t, c, "se", msgs, []searchCase{
		{"BODY eautifu", "* SEARCH"},
		{"SUBJECT eautifu", "* SEARCH 1"},
		{`BODY "foo-bar inv"`, "* SEARCH 1"},
		{`BODY "o-ba"`, "* SEARCH 1"},
		{`BODY "foo bar"`, "* SEARCH"},
		{`BODY "bar foo"`, "* SEARCH 3"},
		{"BODY c12", "* SEARCH 1"},
		{"BODY 123d", "* SEARCH 1"},
		{"BODY nv", "* SEARCH 1 3"},
		{"BODY i", "* SEARCH 1 2 3"},
		{"BODY invoices", "* SEARCH 3"},
		{"BODY " + long[5:50], "* SEARCH 2"},
		{"BODY " + long[13:31], "* SEARCH 2"},
		{"TEXT " + long[13:31], "* SEARCH 2"},
		{`BODY "` + long[40:] + ` y"`, "* SEARCH 2"},
		{"TEXT eautiful", "* SEARCH 1"},
		{"TEXT ubjec", "* SEARCH 1 2 3"}, // header names are header text
	})
}

// TestSearchMIMEText: TEXT and BODY find the file name of an attachment
// (RFC 2231 in Latin-1, RFC 2047) and the header and text of an attached
// message, through the index and directly.
func TestSearchMIMEText(t *testing.T) {
	c, _ := newTestServer(t)
	c.ok("LOGIN test test")

	msgs := []string{
		"Subject: att\r\nMIME-Version: 1.0\r\nContent-Type: multipart/mixed; boundary=b\r\n\r\n" +
			"--b\r\nContent-Type: text/plain\r\n\r\nsee attached\r\n" +
			"--b\r\nContent-Type: application/pdf; name=\"=?utf-8?q?Angebot_K=C3=B6ln.pdf?=\"\r\n" +
			"Content-Disposition: attachment; filename*=iso-8859-1''Rechnung%20M%E4rz.pdf\r\n" +
			"Content-Transfer-Encoding: base64\r\n\r\nJVBERi0xLjQK\r\n--b--\r\n",
		"Subject: fwd\r\nMIME-Version: 1.0\r\nContent-Type: multipart/mixed; boundary=b\r\n\r\n" +
			"--b\r\nContent-Type: text/plain\r\n\r\nforwarded below\r\n" +
			"--b\r\nContent-Type: message/rfc822\r\n\r\n" +
			"From: Inner Sender <inner@example.org>\r\nSubject: =?iso-8859-1?q?Gr=FC=DFe?=\r\n\r\nquarterly numbers\r\n" +
			"--b--\r\n",
	}

	runSearches(t, c, "mime", msgs, []searchCase{
		{"BODY märz", "* SEARCH 1"},
		{"TEXT köln.pdf", "* SEARCH 1"},
		{"OR BODY rechnung BODY nothinghere", "* SEARCH 1"},
		{"BODY jvberi", "* SEARCH"}, // base64 content is not text
		{"BODY quarterly", "* SEARCH 2"},
		{"BODY grüße", "* SEARCH 2"},
		{"TEXT inner@example", "* SEARCH 2"},
		{"OR TEXT uarterl TEXT nothinghere", "* SEARCH 2"},
		{"FROM inner", "* SEARCH"}, // the outer From only
	})
}

// TestSearchWordMode: with WordSearch the index answers SUBJECT, BODY
// and TEXT alone, by word (in any order); HEADER <other> is still
// checked against the message.
func TestSearchWordMode(t *testing.T) {
	c, _ := newTestServer(t, func(o *Options, _ *memstore.Store) { o.WordSearch = true })
	c.ok("LOGIN test test")

	msgs := []string{
		"Subject: one\r\nX-Extra: hello beautiful header\r\n\r\nthe invoice\r\n",
		"Subject: two\r\n\r\nbar foo\r\n",
	}

	runSearches(t, c, "wm", msgs, []searchCase{
		{`BODY "foo bar"`, "* SEARCH 2"}, // words in any order
		{`BODY "invoice the"`, "* SEARCH 1"},
		{`HEADER x-extra "eautiful head"`, "* SEARCH 1"},
		{`HEADER x-extra "header hello"`, "* SEARCH"},
	})
}
