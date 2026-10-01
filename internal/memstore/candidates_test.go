package memstore

import (
	"strings"
	"testing"
	"time"

	"shardpost/internal/fts"
)

// TestSearchCandidates: the candidates of a substring query are a
// superset of the messages that contain it, and nil where the index
// cannot narrow them (the inside of a word of a field without the
// substring dictionary, a two-letter inner part).
func TestSearchCandidates(t *testing.T) {
	u, err := New().User("alice")
	if err != nil {
		t.Fatal(err)
	}

	if err := u.CreateMailbox("INBOX", nil); err != nil {
		t.Fatal(err)
	}

	long := strings.Repeat("xyz", 20)
	for _, raw := range []string{
		"Subject: hello beautiful world\r\nX-Extra: hello beautiful header\r\n\r\nbody\r\n",
		"Subject: other\r\n\r\nsome " + long + " text\r\n",
	} {
		if _, err := u.Deliver("INBOX", []byte(raw), nil, time.Now()); err != nil {
			t.Fatal(err)
		}
	}

	for _, c := range []struct {
		field, query string
		want         int // -1: nil, the index cannot narrow
	}{
		{fts.FieldSubject, "eautiful worl", 1},
		{fts.FieldSubject, "llo bea", 1},
		{fts.FieldHeader, "eautiful head", 1},
		{fts.FieldHeader, "eautiful", -1},
		{fts.FieldBody, "zxyzx", 1}, // inside a word too long for a term
		{fts.FieldBody, "od", -1},
		{fts.FieldBody, "some xyz", 1},
	} {
		hits, err := u.SearchCandidates([]string{c.field}, c.query)
		if err != nil {
			t.Fatal(err)
		}

		switch {
		case c.want < 0 && hits != nil:
			t.Errorf("%s %q: %v, want nil", c.field, c.query, hits)
		case c.want >= 0 && (hits == nil || len(hits) < c.want):
			t.Errorf("%s %q: %v, want at least %d", c.field, c.query, hits, c.want)
		}
	}
}
