//go:build tentacle

package cephstore

import (
	"fmt"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	"shardpost/internal/fts"
	"shardpost/internal/model"
)

// layoutDocs are generated messages for the layout comparison: shared
// and rare words, long words with window terms, header values.
func layoutDocs(n int) map[string]string {
	words := []string{"invoice", "rechnung", "meeting", "beautiful", "photosynthesis", "übung", "straße",
		"report", "quarterly", "xylophone", "abcdefghijabcdefghij", "zebra", "lorem", "ipsum"}
	docs := map[string]string{}

	for i := range n {
		var b strings.Builder

		fmt.Fprintf(&b, "Subject: %s %s no%d\r\nX-Ref: ref-%d-%s\r\n\r\n", words[i%len(words)], words[(i*3)%len(words)], i, i, words[(i*5)%len(words)])

		for j := range 20 {
			b.WriteString(words[(i*7+j*11)%len(words)])
			b.WriteString(" ")
		}

		fmt.Fprintf(&b, "unique%dword\r\n", i)
		docs[fmt.Sprintf("g%03d", i)] = b.String()
	}

	return docs
}

// layoutQueries are the searches the layouts must answer alike: words,
// word starts, inner parts, phrases, in every field.
var layoutQueries = []struct {
	fields []string
	query  string
}{
	{[]string{fts.FieldSubject}, "invoice"},
	{[]string{fts.FieldSubject}, "eautiful"},
	{[]string{fts.FieldBody}, "synthes"},
	{[]string{fts.FieldBody}, " quart"},
	{[]string{fts.FieldBody}, "unique1"},
	{[]string{fts.FieldBody}, "ghijabcd"},
	{[]string{fts.FieldBody}, "übu"},
	{[]string{fts.FieldBody}, "rasse"},
	{[]string{fts.FieldBody}, "lorem ipsum"},
	{[]string{fts.FieldHeader}, "ref-1"},
	{[]string{fts.FieldHeader}, "x-ref"},
	{[]string{fts.FieldHeader, fts.FieldBody}, " zebra "},
	{[]string{fts.FieldSubject, fts.FieldBody}, "meet"},
}

// TestLayoutSearchEquivalence: the candidate search over layout 3
// generations of any size (S ∈ {1, 2, 7, 32}, D ∈ {1, 4}) gives the
// sets layout 2 gives, also after removals; only the object names
// differ (N12: the dictionary object comes through candidateIndex).
func TestLayoutSearchEquivalence(t *testing.T) {
	docs := layoutDocs(60)
	ref := newMemIndex(DefaultFTSShards, docs)

	var ixs []*memIndex

	for _, s := range []int{1, 2, 7, 32} {
		for _, d := range []int{1, 4} {
			ixs = append(ixs, newMemIndexAt(ftsLoc{f: 3, g: "0123456789abcdef", s: s, d: d, h: 1}, docs))
		}
	}

	compare := func(stage string) {
		t.Helper()

		for _, q := range layoutQueries {
			want, err := searchCandidates(ref, q.fields, q.query)
			if err != nil {
				t.Fatal(err)
			}

			for _, ix := range ixs {
				got, err := searchCandidates(ix, q.fields, q.query)
				if err != nil {
					t.Fatal(err)
				}

				if !maps.Equal(got, want) || (got == nil) != (want == nil) {
					t.Errorf("%s: S=%d D=%d %v %q: %v, layout 2 %v", stage, ix.loc.s, ix.loc.d, q.fields, q.query,
						slices.Sorted(maps.Keys(got)), slices.Sorted(maps.Keys(want)))
				}
			}
		}
	}

	compare("indexed")

	for i := 0; i < 60; i += 3 {
		guid := fmt.Sprintf("g%03d", i)
		terms := fts.Terms([]byte(docs[guid]))

		ref.remove(guid, terms)

		for _, ix := range ixs {
			ix.remove(guid, terms)
		}
	}

	compare("after removals")
}

// TestLayoutNames: layout 3 names its objects by generation and number,
// layout 2 keeps the names before 0.45.0; a term, trigram and GUID land
// in one object of each kind.
func TestLayoutNames(t *testing.T) {
	l3 := ftsLoc{f: 3, g: "9c1e00aa00bb00cc", s: 4, d: 2, h: 1}

	want := []string{"ftx/9c1e00aa00bb00cc/p0", "ftx/9c1e00aa00bb00cc/p1", "ftx/9c1e00aa00bb00cc/p2", "ftx/9c1e00aa00bb00cc/p3"}
	if got := l3.postingOIDs(); !slices.Equal(got, want) {
		t.Errorf("posting objects %v", got)
	}

	if got := l3.dictOID("voi"); got != fmt.Sprintf("ftx/9c1e00aa00bb00cc/d%d", shardNo("voi", 2)) {
		t.Errorf("dictionary object %s", got)
	}

	if got := l3.headerOID("abc"); got != "ftx/9c1e00aa00bb00cc/h0" {
		t.Errorf("header object %s", got)
	}

	l2 := layout2Loc(16)
	if l2.postingOID("s:x") != fmt.Sprintf("fts/%02d", shardNo("s:x", 16)) || l2.dictOID("voi") != dictShardOID("voi") ||
		l2.headerOID("abc") != fmt.Sprintf("ftsh/%02d", shardNo("abc", 16)) {
		t.Error("layout 2 names changed")
	}

	if n := len(l2.objects()); n != 16+256+16+16 {
		t.Errorf("layout 2 has %d objects", n)
	}

	if n := len(l3.objects()); n != 4+2+1 {
		t.Errorf("layout 3 generation has %d objects", n)
	}
}

// TestDictScanBundled: the ranges of the trigrams that fall into one
// dictionary object are read in one read per round (D = 1: one read).
func TestDictScanBundled(t *testing.T) {
	docs := layoutDocs(20)

	for _, d := range []int{1, 4} {
		ix := newMemIndexAt(ftsLoc{f: 3, g: "0123456789abcdef", s: 2, d: d, h: 1}, docs)
		tris := fts.Trigrams("hotosynthes")

		found, complete, err := dictMatches(ix, fts.FieldBody, "hotosynthes", tris)
		if err != nil || !complete || !slices.Contains(found, "b:photosynthesis") {
			t.Fatalf("D=%d: %v %v %v", d, found, complete, err)
		}

		objects := map[string]bool{}
		for _, tri := range tris {
			objects[ix.dictOID(tri)] = true
		}

		if n := int(ix.reads.Load()); n != len(objects) || (d == 1 && n != 1) {
			t.Errorf("D=%d: %d reads for %d objects", d, n, len(objects))
		}
	}
}

// TestLayoutRecord: the record round-trips; a record of an unknown
// layout is refused; no record is layout 2.
func TestLayoutRecord(t *testing.T) {
	l := testRecord()

	got, err := parseLayout(l.encode())
	if err != nil || got.G != l.G || got.Next.Tok != "job-x/3" || len(got.Old) != 1 {
		t.Fatalf("round trip %+v %v", got, err)
	}

	if _, err := parseLayout([]byte(`{"f":4}`)); err == nil {
		t.Error("layout 4 accepted")
	}

	if l, err := parseLayout(nil); l != nil || err != nil {
		t.Error("no record is not layout 2")
	}
}

// TestLayoutNamedGens: every generation a record holds is named, layout
// 2 until its objects are gone.
func TestLayoutNamedGens(t *testing.T) {
	l := testRecord()

	gens := nameGens(l, 16)
	if len(gens) != 3 || !gens[0].same(curLoc(l, 16)) || gens[1].g != "bbbbbbbbbbbbbbbb" || gens[2].v3() {
		t.Errorf("named generations %+v", gens)
	}

	l.LG, l.Old = true, nil

	if gens := nameGens(l, 16); len(gens) != 2 {
		t.Errorf("with lg %+v", gens)
	}
}

// testRecord is a record with a conversion in progress and a source left.
func testRecord() *ftsLayout {
	return &ftsLayout{F: 3, G: "aaaaaaaaaaaaaaaa", S: 1, D: 1, H: 1, Dict: true,
		Next: &ftsGen{F: 3, G: "bbbbbbbbbbbbbbbb", S: 2, D: 2, H: 1, Tok: "job-x/3"},
		Old:  []ftsGen{{F: 2, Tok: "job-y/1"}}}
}

// TestLayoutSizing: object counts follow the key counts as powers of two
// within the bounds.
func TestLayoutSizing(t *testing.T) {
	z := ftsSizing{min: 1, max: 32, target: 100000}.withDefaults()

	for _, c := range []struct {
		keys   int64
		n      int
		capped bool
	}{
		{0, 1, false}, {99999, 1, false}, {100001, 2, false}, {250000, 4, false}, {3_200_000, 32, false}, {3_300_000, 32, true},
	} {
		if n, capped := z.count(c.keys); n != c.n || capped != c.capped {
			t.Errorf("count(%d) = %d %v, want %d %v", c.keys, n, capped, c.n, c.capped)
		}
	}
}

// TestLayoutShapeFor: a fresh index without a hint has the lower bound;
// with one, the estimate of the bench corpus sizes it.
func TestLayoutShapeFor(t *testing.T) {
	z := ftsSizing{min: 1, max: 32, target: 100000}.withDefaults()

	if s, d, h := z.shapeFor(0); s != 1 || d != 1 || h != 1 {
		t.Errorf("no hint: %d %d %d", s, d, h)
	}

	if s, d, h := z.shapeFor(5000); s != 1 || d != 8 || h != 1 {
		t.Errorf("5000 messages: %d %d %d", s, d, h)
	}

	terms, dict := estimateKeys(500)
	if terms != 45007 || dict != 258988 {
		t.Errorf("estimate at a calibration point: %d %d", terms, dict)
	}

	if _, d2 := estimateKeys(20000); d2 <= dict {
		t.Error("the estimate does not grow beyond the last point")
	}
}

// TestDueGen: the reshard queue names a generation by a hash that
// ParseDueKey gives back.
func TestDueGen(t *testing.T) {
	gen := model.FTSDueGen("9c1e00aa00bb00cc")

	e, err := model.ParseDueKey(model.FormatDueKey("a@example.org", model.DueKey{Due: time.Unix(1, 0), Gen: gen}))
	if err != nil || e.ID != "a@example.org" || e.Gen != gen || gen == model.FTSDueGen("9c1e00aa00bb00cd") {
		t.Fatalf("%+v %v", e, err)
	}
}

// TestMarkersChecked: markers are not terms (every full enumeration
// skips them); a missing layout 3 object, a foreign generation and a
// freeze are anomalies.
func TestMarkersChecked(t *testing.T) {
	if !isMarker(markGen) || !isMarker(markDict) || isMarker("b:!x") {
		t.Error("isMarker")
	}

	loc := ftsLoc{f: 3, g: "aaaaaaaaaaaaaaaa", s: 1, d: 1, h: 1}

	for _, c := range []struct {
		exists bool
		cur    map[string][]byte
		kind   int
	}{
		{false, nil, anomMissing},
		{true, map[string][]byte{}, anomMissing},
		{true, map[string][]byte{markGen: []byte("bbbbbbbbbbbbbbbb")}, anomForeign},
		{true, map[string][]byte{markGen: []byte(loc.g), markState: []byte("frozen:job-1/2")}, anomFrozen},
		{true, map[string][]byte{markGen: []byte(loc.g), markState: []byte(stateLive)}, -1},
	} {
		an := checkMarkers(loc, "ftx/x/p0", c.exists, c.cur)
		if (an == nil) != (c.kind < 0) || (an != nil && an.kind != c.kind) {
			t.Errorf("%+v: %+v", c, an)
		}
	}
}
