package memstore

import (
	"testing"
	"time"

	"shardpost/internal/fts"
)

// TestHeaderFieldMatching: subject, addresses and body match by
// substring. The field of all header values matches a token as a prefix
// of a word, and by substring only where the word is in the subject or
// an address as well.
func TestHeaderFieldMatching(t *testing.T) {
	u, err := New().User("alice")
	if err != nil {
		t.Fatal(err)
	}

	if err := u.CreateMailbox("INBOX", nil); err != nil {
		t.Fatal(err)
	}

	raw := "From: carol@example.org\r\nTo: alice@example.org\r\nSubject: Invoice 4711\r\n" +
		"Message-ID: <k3j9x7qq2@mail.example.org>\r\nList-Id: dovecot.lists.example.net\r\n\r\nPlease find the statement attached.\r\n"
	if _, err := u.Deliver("INBOX", []byte(raw), nil, time.Now()); err != nil {
		t.Fatal(err)
	}

	for _, c := range []struct {
		field, query string
		want         bool
	}{
		{fts.FieldSubject, "voice", true},
		{fts.FieldBody, "tatemen", true},
		{fts.FieldFrom, "arol", true},
		{fts.FieldHeader, "voice", true},     // substring of a subject word
		{fts.FieldHeader, "xample", true},    // substring of an address word
		{fts.FieldHeader, "k3j9x7qq2", true}, // a whole word of another header
		{fts.FieldHeader, "dove", true},      // prefix of a word of another header
		{fts.FieldHeader, "ovecot", false},   // inside a word of another header
		{fts.FieldHeader, "3j9x", false},
	} {
		hits, err := u.SearchField(c.field, c.query)
		if err != nil {
			t.Fatal(err)
		}

		if got := len(hits) == 1; got != c.want {
			t.Errorf("%s %q: found %v, want %v", c.field, c.query, got, c.want)
		}
	}
}
