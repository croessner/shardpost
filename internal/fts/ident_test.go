package fts

import (
	"strings"
	"testing"
)

// TestIsIdentifier (temp/13 §10): the bounds n − 1/n, digits only,
// letters only, runes not bytes, 40-rune windows with and without a
// digit.
func TestIsIdentifier(t *testing.T) {
	for _, c := range []struct {
		tok  string
		n    int
		want bool
	}{
		{"a1b2c3d4e5f6g7h8i9j0k1l2m", 25, true},
		{"a1b2c3d4e5f6g7h8i9j0k1l2", 25, false},
		{"1234567890123456789012345", 25, false},
		{"abcdefghijklmnopqrstuvwxy", 25, false},
		{"ä1ö2ü3ä4ö5ü6ä7ö8ü9ä0ö1ü2ß", 25, true}, // 25 runes, more bytes
		{"ä1ö2ü3ä4ö5ü6", 13, false},              // 12 runes, 18 bytes
		{strings.Repeat("ab", 19) + "c9", 40, true},
		{strings.Repeat("ab", 20), 40, false},
		{"a1", 0, false},
	} {
		if got := IsIdentifier(c.tok, c.n); got != c.want {
			t.Errorf("IsIdentifier(%q, %d) = %v", c.tok, c.n, got)
		}
	}
}

// TestDictKeys: trigram keys as the dictionary has them, none for the
// header field, one for a body identifier with a length bound, never for
// the fields s, f, t.
func TestDictKeys(t *testing.T) {
	id := "a1b2c3d4e5f6g7h8i9j0k1l2m3n4o5p6" // 32 runes, 30 trigrams

	for _, c := range []struct {
		term string
		n    int
		want int
	}{
		{"b:rechnung", 0, 6},
		{"b:rechnung", 13, 6},
		{"h:rechnung", 0, 0},
		{"b:" + id, 0, 30},
		{"b:" + id, 25, 1},
		{"b:" + id, 33, 30},
		{"s:" + id, 25, 30},
		{"f:" + id, 25, 30},
		{"t:" + id, 25, 30},
		{"b:ab", 0, 0},
	} {
		if got := DictKeys(c.term, c.n); got != c.want {
			t.Errorf("DictKeys(%q, %d) = %d, want %d", c.term, c.n, got, c.want)
		}
	}
}
