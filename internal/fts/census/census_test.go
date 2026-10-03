package census

import (
	"encoding/json"
	"strings"
	"testing"

	"shardpost/internal/fts"
)

// censusTerms are the terms of the census tests: words, numbers,
// identifiers in several fields, a 40-rune window, a marker.
var censusTerms = []string{"b:rechnung", "b:2026", "b:a1b2c3d4e5f6a7b8c9d0e1f2a3b4c5d6", "s:a1b2c3d4e5f6a7b8c9d0e1f2a3b4c5d6",
	"h:a1b2c3d4e5f6a7b8c9d0e1f2a3b4c5d6", "b:" + strings.Repeat("x9", 20), "b:~~", "f:ab"}

// dictKeysOf sums fts.DictKeys with a bound over terms.
func dictKeysOf(terms []string, bound int) int64 {
	var n int64
	for _, term := range terms {
		n += int64(fts.DictKeys(term, bound))
	}

	return n
}

// TestCensus: terms are counted by field, class and length; the
// projection per bound equals fts.DictKeys with that bound.
func TestCensus(t *testing.T) {
	c := New()
	for _, term := range censusTerms {
		c.Add(term)
	}

	r := c.Result()

	if r.Terms != int64(len(censusTerms)) || r.DictKeys != dictKeysOf(censusTerms, 0) {
		t.Errorf("totals %d %d", r.Terms, r.DictKeys)
	}

	for _, b := range r.ByIdent {
		if want := dictKeysOf(censusTerms, b.MinRunes); b.DictKeys != want {
			t.Errorf("bound %d: %d dictionary keys, want %d", b.MinRunes, b.DictKeys, want)
		}
	}

	first, last := r.ByIdent[0], r.ByIdent[len(r.ByIdent)-1]
	if [4]int64{int64(first.MinRunes), int64(last.MinRunes), first.Identifiers, last.Identifiers} != [4]int64{13, 40, 2, 1} {
		t.Errorf("bounds %+v … %+v", first, last)
	}

	classes := map[string]bool{}
	for _, row := range r.Rows {
		classes[row.Field+"/"+row.Class+"/"+row.Length] = true
	}

	for _, k := range []string{"b/letters/4-8", "b/digits/4-8", "b/mixed/25-32", "s/mixed/25-32", "h/mixed/25-32", "b/mixed/40", "b/other/2-3", "f/letters/2-3"} {
		if !classes[k] {
			t.Errorf("class %s missing in %v", k, classes)
		}
	}
}

// TestCensusNamesNoTerm: nothing of a term reaches the result.
func TestCensusNamesNoTerm(t *testing.T) {
	c := New()
	for _, term := range censusTerms {
		c.Add(term)
	}

	b, _ := json.Marshal(c.Result())

	for _, term := range censusTerms {
		if _, tok, _ := strings.Cut(term, ":"); len(tok) > 3 && strings.Contains(string(b), tok) {
			t.Errorf("the result names the term %q", term)
		}
	}
}
