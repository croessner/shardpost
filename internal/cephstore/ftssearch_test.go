//go:build tentacle

package cephstore

import (
	"fmt"
	"maps"
	"slices"
	"sort"
	"strings"
	"testing"

	"shardpost/internal/fts"
)

// memIndex is the key layout of the index in memory: the posting shards
// and the dictionary as sorted omap keys, the postings by term.
type memIndex struct {
	keys    map[string][]string
	posts   map[string]map[string]bool
	nShards int
	legacy  bool
	stale   bool // without the version marker
}

func newMemIndex(nShards int, docs map[string]string) *memIndex {
	ix := &memIndex{keys: map[string][]string{}, posts: map[string]map[string]bool{}, nShards: nShards}

	for guid, raw := range docs {
		ix.add(guid, fts.Terms([]byte(raw)))
	}

	return ix
}

func (ix *memIndex) add(guid string, terms []string) {
	var fresh []string

	for _, t := range terms {
		if ix.posts[t] == nil {
			ix.posts[t] = map[string]bool{}
			fresh = append(fresh, t)

			oid := fmt.Sprintf("%s%02d", ftsPrefix, shardNo(t, ix.nShards))
			ix.keys[oid] = append(ix.keys[oid], t)
		}

		ix.posts[t][guid] = true
	}

	byShard, _ := dictEntries(fresh)
	for oid, keys := range byShard {
		ix.keys[oid] = append(ix.keys[oid], keys...)
	}

	for oid := range ix.keys {
		sort.Strings(ix.keys[oid])
	}
}

func (ix *memIndex) list(oid, after, prefix string, limit uint64, fn func(string)) (bool, error) {
	n := uint64(0)

	for _, k := range ix.keys[oid] {
		if k <= after || !strings.HasPrefix(k, prefix) {
			continue
		}

		if n == limit {
			return true, nil
		}

		fn(k)

		n++
	}

	return false, nil
}

func (ix *memIndex) postingsUnion(terms []string) (map[string]bool, error) {
	out := map[string]bool{}

	for _, t := range terms {
		for g := range ix.posts[t] {
			out[g] = true
		}
	}

	return out, nil
}

func (ix *memIndex) legacyDictPresent() (bool, error) { return ix.legacy, nil }
func (ix *memIndex) current() (bool, error)           { return !ix.stale, nil }
func (ix *memIndex) shards() int                      { return ix.nShards }

// candidateDocs are the messages of memstore's candidate test.
var candidateDocs = []string{
	"Subject: hello beautiful world\r\nX-Extra: hello beautiful header\r\n\r\nbody text\r\n",
	"Subject: other\r\n\r\nsome " + strings.Repeat("xyz", 20) + " text\r\n",
	"Subject: third\r\nX-Id: " + strings.Repeat("q", 50) + "\r\n\r\n" + strings.Repeat("abcdefghij", 30) + "\r\n",
}

func runCandidates(t *testing.T, ix *memIndex, fields []string, query string, want []string) {
	t.Helper()

	got, err := searchCandidates(ix, fields, query)
	if err != nil {
		t.Fatal(err)
	}

	switch {
	case want == nil && got != nil:
		t.Errorf("%v %q: %v, want nil", fields, query, got)
	case want != nil && !slices.Equal(slices.Sorted(maps.Keys(got)), want):
		t.Errorf("%v %q: %v, want %v", fields, query, slices.Sorted(maps.Keys(got)), want)
	}
}

// TestSearchCandidates runs the cases of memstore's TestSearchCandidates
// over the key layout of this store: whole words by key, word starts by
// a scan of the posting shards, inner parts through the dictionary,
// window terms and markers of long words.
func TestSearchCandidates(t *testing.T) {
	docs := map[string]string{}
	for i, raw := range candidateDocs {
		docs[fmt.Sprintf("g%d", i)] = raw
	}

	ix := newMemIndex(DefaultFTSShards, docs)

	for _, c := range []struct {
		fields []string
		query  string
		want   []string
	}{
		{[]string{fts.FieldSubject}, "eautiful worl", []string{"g0"}},
		{[]string{fts.FieldSubject}, "llo bea", []string{"g0"}},
		{[]string{fts.FieldHeader}, "eautiful head", []string{"g0"}},
		{[]string{fts.FieldHeader}, "eautiful", nil},
		{[]string{fts.FieldHeader}, "x-extra", []string{"g0"}},
		{[]string{fts.FieldHeader}, ": " + strings.Repeat("q", 30), []string{"g2"}},
		{[]string{fts.FieldHeader}, ": " + strings.Repeat("q", 45), nil},
		{[]string{fts.FieldBody}, "zxyzx", []string{"g1", "g2"}},
		{[]string{fts.FieldBody}, "od", nil},
		{[]string{fts.FieldBody}, "some xyz", []string{"g1"}},
		{[]string{fts.FieldBody}, "ext", []string{"g0", "g1", "g2"}},
		{[]string{fts.FieldBody}, "hijabcdefghijabcdefghija", []string{"g1", "g2"}},
		{[]string{fts.FieldBody}, "jabcd", []string{"g2"}},
		{[]string{fts.FieldHeader, fts.FieldBody}, "text", nil},
		{[]string{fts.FieldHeader, fts.FieldBody}, " text ", []string{"g0", "g1"}},
	} {
		runCandidates(t, ix, c.fields, c.query, c.want)
	}
}

// TestSearchCandidatesLimits: a word start shared by more than
// maxCandidates terms, read over many pages, does not narrow; one with
// fewer is complete across the shards. The dictionary of the layout
// before 0.35.0 and an index without the version marker narrow only
// what they can answer.
func TestSearchCandidatesLimits(t *testing.T) {
	ix := newMemIndex(DefaultFTSShards, map[string]string{"g0": "Subject: s\r\n\r\nbody\r\n"})

	var many []string
	for i := 0; i <= maxCandidates; i++ {
		many = append(many, fmt.Sprintf("b:pre%05d", i))
	}

	ix.add("g1", many)

	var few []string
	for i := 0; i < 3*omapPageSize; i++ {
		few = append(few, fmt.Sprintf("b:few%05d", i))
	}

	ix.add("g2", few)

	runCandidates(t, ix, []string{fts.FieldBody}, " pre", nil)
	runCandidates(t, ix, []string{fts.FieldBody}, " few", []string{"g2"})

	if found, complete, _ := prefixKeys(ix, "b:few"); !complete || len(found) != len(few) {
		t.Errorf("prefix scan: %d terms, complete %v", len(found), complete)
	}

	ix.legacy = true
	runCandidates(t, ix, []string{fts.FieldBody}, "ody", nil)
	runCandidates(t, ix, []string{fts.FieldBody}, " bod", []string{"g0"})

	ix.legacy, ix.stale = false, true
	runCandidates(t, ix, []string{fts.FieldBody}, " bod", nil)
	runCandidates(t, ix, []string{fts.FieldBody}, " body ", []string{"g0"})
	runCandidates(t, ix, []string{fts.FieldHeader}, " subject ", nil)
	runCandidates(t, ix, []string{fts.FieldBody}, " bödy ", nil)
}

// TestHeaderTextCodec: the header store keeps a text compressed and
// gives it back; a text that is too long is not kept.
func TestHeaderTextCodec(t *testing.T) {
	text := fts.HeaderText([]byte("Subject: =?iso-8859-1?q?Gr=FC=DFe?=\r\nX-A: b\r\n\r\nbody"))

	v, ok := encodeHeaderText(text)
	if !ok || len(v) == 0 {
		t.Fatal("not encoded")
	}

	if got, ok := decodeHeaderText(v); !ok || got != text || !strings.Contains(got, "subject: grüße") {
		t.Errorf("decoded %q", got)
	}

	if _, ok := encodeHeaderText(strings.Repeat("a", fts.MaxHeaderTextBytes+1)); ok {
		t.Error("an over-long text was kept")
	}

	if _, ok := decodeHeaderText([]byte{9, 1, 2}); ok {
		t.Error("an unknown format was decoded")
	}
}
