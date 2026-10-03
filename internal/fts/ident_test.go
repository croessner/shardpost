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
		// 25 runes, more bytes; 12 runes, 18 bytes
		{"ä1ö2ü3ä4ö5ü6ä7ö8ü9ä0ö1ü2ß", 25, true},
		{"ä1ö2ü3ä4ö5ü6", 13, false},
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

// TestDictIdentifier: only body terms, never s, f, t or h, with the
// bound of IsIdentifier.
func TestDictIdentifier(t *testing.T) {
	id := "a1b2c3d4e5f6g7h8i9j0k1l2m3n4o5p6"

	for _, c := range []struct {
		term string
		n    int
		want bool
	}{
		{"b:" + id, 25, true},
		{"b:" + id, 0, false},
		{"b:" + id, 33, false},
		{"s:" + id, 25, false},
		{"f:" + id, 25, false},
		{"t:" + id, 25, false},
		{"h:" + id, 25, false},
		{"b:" + strings.Repeat("ab", 20), 25, false},
	} {
		if got := DictIdentifier(c.term, c.n); got != c.want {
			t.Errorf("DictIdentifier(%q, %d) = %v", c.term, c.n, got)
		}
	}
}

// TestTri0: the first three runes, not bytes; nothing for a shorter
// token; the first trigram Trigrams returns.
func TestTri0(t *testing.T) {
	for _, c := range []struct{ tok, want string }{
		{"abcdef", "abc"}, {"äöüxyz", "äöü"}, {"ab", ""}, {"", ""}, {"ä1ö", "ä1ö"},
	} {
		if got := Tri0(c.tok); got != c.want {
			t.Errorf("Tri0(%q) = %q, want %q", c.tok, got, c.want)
		}

		if tris := Trigrams(c.tok); len(tris) > 0 && tris[0] != c.want {
			t.Errorf("Trigrams(%q)[0] = %q, Tri0 %q", c.tok, tris[0], c.want)
		}
	}
}
