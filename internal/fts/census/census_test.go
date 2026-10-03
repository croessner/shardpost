package census

import (
	"encoding/json"
	"strings"
	"testing"

	"shardpost/internal/fts"
)

// TestCensus: terms are counted by field, class and length; the
// projection per bound equals fts.DictKeys with that bound; nothing of a
// term reaches the result.
func TestCensus(t *testing.T) {
	id32 := "a1b2c3d4e5f6a7b8c9d0e1f2a3b4c5d6"
	terms := []string{"b:rechnung", "b:2026", "b:" + id32, "s:" + id32, "h:" + id32, "b:" + strings.Repeat("x9", 20), "b:~~", "f:ab"}

	c := New()
	for _, term := range terms {
		c.Add(term)
	}

	r := c.Result()

	var want int64
	for _, term := range terms {
		want += int64(fts.DictKeys(term, 0))
	}

	if r.Terms != int64(len(terms)) || r.DictKeys != want {
		t.Errorf("totals %d %d, want %d %d", r.Terms, r.DictKeys, len(terms), want)
	}

	for _, b := range r.ByIdent {
		var keys int64
		for _, term := range terms {
			keys += int64(fts.DictKeys(term, b.MinRunes))
		}

		if b.DictKeys != keys {
			t.Errorf("bound %d: %d dictionary keys, want %d", b.MinRunes, b.DictKeys, keys)
		}
	}

	if first, last := r.ByIdent[0], r.ByIdent[len(r.ByIdent)-1]; first.MinRunes != 13 || last.MinRunes != 40 ||
		first.Identifiers != 2 || last.Identifiers != 1 {
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

	b, _ := json.Marshal(r)
	for _, term := range terms {
		_, tok, _ := strings.Cut(term, ":")
		if len(tok) > 3 && strings.Contains(string(b), tok) {
			t.Errorf("the result names the term %q", term)
		}
	}
}
