package imapproto

import (
	"fmt"
	"strings"
	"testing"
)

// TestSearchConvertedText: body text in Latin-1, Windows-1252 and
// unlabelled 8-bit, an HTML part and encoded or raw 8-bit header words
// are found by their UTF-8 spelling, through the index and through the
// direct path (keys under NOT/OR) alike, and markup is not text.
func TestSearchConvertedText(t *testing.T) {
	c, _ := newTestServer(t)
	c.ok("LOGIN test test")
	c.ok("CREATE cs")

	msgs := []string{
		"From: a@example.org\r\nSubject: qp\r\nMIME-Version: 1.0\r\n" +
			"Content-Type: text/plain; charset=ISO-8859-1\r\nContent-Transfer-Encoding: quoted-printable\r\n\r\n" +
			"Gr=FC=DFe aus K=F6ln\r\n",
		"From: a@example.org\r\nSubject: cp\r\nMIME-Version: 1.0\r\n" +
			"Content-Type: text/plain; charset=windows-1252\r\n\r\n\x84Bestellbest\xe4tigung\x93 Stra\xdfe\r\n",
		"From: a@example.org\r\nSubject: raw\r\n\r\nM\xfcller hat angerufen\r\n",
		"From: a@example.org\r\nSubject: html\r\nMIME-Version: 1.0\r\nContent-Type: text/html; charset=us-ascii\r\n\r\n" +
			"<html><style>p{color:red}</style><script>var hiddenscript=1;</script>" +
			"<p>K&auml;se&nbsp;und&nbsp;Wurst</p></html>\r\n",
		"From: =?iso-8859-1?q?J=FCrgen?= <j@example.org>\r\n" +
			"Subject: =?windows-1252?Q?Angebot_f=FCr_Stra=DFe?=\r\nX-Note: \xc4rger\r\n\r\nbody\r\n",
	}

	for _, m := range msgs {
		c.ok("APPEND cs {%d+}\r\n%s", len(m), m)
	}

	c.ok("SELECT cs")

	cases := []struct{ search, want string }{
		{"BODY grüße", "* SEARCH 1"},
		{"BODY köln", "* SEARCH 1"},
		{"BODY bestellbestätigung", "* SEARCH 2"},
		{"TEXT straße", "* SEARCH 2 5"},
		{"BODY müller", "* SEARCH 3"},
		{"BODY käse", "* SEARCH 4"},
		{"BODY wurst", "* SEARCH 4"},
		{"BODY hiddenscript", "* SEARCH"},
		{"BODY color", "* SEARCH"},
		{"SUBJECT angebot", "* SEARCH 5"},
		{"FROM jürgen", "* SEARCH 5"},
		{"HEADER X-Note ärger", "* SEARCH 5"},
		{"TEXT ärger", "* SEARCH 5"},
		// the direct path: the same keys inside OR/NOT
		{"OR BODY grüße BODY nothinghere", "* SEARCH 1"},
		{"OR BODY käse BODY müller", "* SEARCH 3 4"},
		{"OR TEXT bestellbestätigung TEXT nothinghere", "* SEARCH 2"},
		{"OR SUBJECT straße SUBJECT nothinghere", "* SEARCH 5"},
		{"OR TEXT ärger TEXT nothinghere", "* SEARCH 5"},
		{"OR BODY hiddenscript BODY nothinghere", "* SEARCH"},
		{"NOT BODY grüße", "* SEARCH 2 3 4 5"},
	}

	for _, tc := range cases {
		lines := c.ok("SEARCH CHARSET UTF-8 %s", literals(tc.search))
		if !hasExact(lines, tc.want) {
			t.Errorf("SEARCH %s: %v, want %q", tc.search, lines, tc.want)
		}
	}
}

// hasExact reports whether one line is exactly want.
func hasExact(lines []string, want string) bool {
	for _, l := range lines {
		if l == want {
			return true
		}
	}

	return false
}

// literals sends every word of a search that is not ASCII as a
// non-synchronizing literal.
func literals(search string) string {
	words := strings.Fields(search)
	for i, w := range words {
		for _, r := range w {
			if r > 0x7f {
				words[i] = fmt.Sprintf("{%d+}\r\n%s", len(w), w)

				break
			}
		}
	}

	return strings.Join(words, " ")
}
