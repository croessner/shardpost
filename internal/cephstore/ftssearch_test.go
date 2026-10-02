//go:build tentacle

package cephstore

import (
	"fmt"
	"maps"
	"slices"
	"sort"
	"strings"
	"sync/atomic"
	"testing"

	"shardpost/internal/fts"
)

// memIndex is the key layout of the index in memory: the posting
// objects and the dictionary of one generation (either layout) as
// sorted omap keys, the postings by term.
type memIndex struct {
	keys   map[string][]string
	posts  map[string]map[string]bool
	loc    ftsLoc
	legacy bool
	stale  bool // without the version marker
	// reads counts the reads of scan (one per object and round).
	reads atomic.Int64
}

func newMemIndex(nShards int, docs map[string]string) *memIndex {
	return newMemIndexAt(layout2Loc(nShards), docs)
}

// newMemIndexAt is newMemIndex over the generation loc.
func newMemIndexAt(loc ftsLoc, docs map[string]string) *memIndex {
	ix := &memIndex{keys: map[string][]string{}, posts: map[string]map[string]bool{}, loc: loc}

	for _, guid := range slices.Sorted(maps.Keys(docs)) {
		ix.add(guid, fts.Terms([]byte(docs[guid])))
	}

	return ix
}

func (ix *memIndex) add(guid string, terms []string) {
	var fresh []string

	for _, t := range terms {
		if ix.posts[t] == nil {
			ix.posts[t] = map[string]bool{}
			fresh = append(fresh, t)

			oid := ix.loc.postingOID(t)
			ix.keys[oid] = append(ix.keys[oid], t)
		}

		ix.posts[t][guid] = true
	}

	byShard, _ := dictEntriesAt(ix.loc, fresh)
	for oid, keys := range byShard {
		ix.keys[oid] = append(ix.keys[oid], keys...)
	}

	for oid := range ix.keys {
		sort.Strings(ix.keys[oid])
		ix.keys[oid] = slices.Compact(ix.keys[oid])
	}
}

// remove takes a message out as ftsRemove does: its id out of the lists
// of its terms, the terms whose lists became empty out of the posting
// objects and the dictionary.
func (ix *memIndex) remove(guid string, terms []string) {
	var gone []string

	for _, t := range terms {
		if ix.posts[t] == nil {
			continue
		}

		delete(ix.posts[t], guid)

		if len(ix.posts[t]) == 0 {
			delete(ix.posts, t)
			gone = append(gone, t)
			ix.drop(ix.loc.postingOID(t), t)
		}
	}

	byShard, _ := dictEntriesAt(ix.loc, gone)
	for oid, keys := range byShard {
		for _, k := range keys {
			ix.drop(oid, k)
		}
	}
}

func (ix *memIndex) drop(oid, key string) {
	ix.keys[oid] = slices.DeleteFunc(ix.keys[oid], func(k string) bool { return k == key })
}

func (ix *memIndex) scan(oid string, reqs []*scanReq) error {
	ix.reads.Add(1)

	for _, q := range reqs {
		n := uint64(0)
		q.more = false

		for _, k := range ix.keys[oid] {
			if k <= q.after || !strings.HasPrefix(k, q.prefix) {
				continue
			}

			if n == q.limit {
				q.more = true
				break
			}

			q.fn(k)

			n++
		}
	}

	return nil
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
func (ix *memIndex) postingOIDs() []string            { return ix.loc.postingOIDs() }
func (ix *memIndex) dictOID(tri string) string        { return ix.loc.dictOID(tri) }

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
// before 0.35.0 narrows no inner part; an index that cannot be trusted
// (no version marker, or a rebuild key) narrows nothing.
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
	runCandidates(t, ix, []string{fts.FieldBody}, " body ", nil)
	runCandidates(t, ix, []string{fts.FieldSubject}, " s ", nil)
}

// TestIndexVersionMarker: the index is trusted with the marker and
// without a running rebuild; a rebuild key, also one a stopped rebuild
// left, takes the trust away. Only a fresh index — no ids, no rebuild,
// no posting shard of an older index — gets the marker with its first
// ids.
func TestIndexVersionMarker(t *testing.T) {
	marker := []byte(ftsVersion)

	for _, c := range []struct {
		vals map[string][]byte
		want bool
	}{
		{map[string][]byte{ftsVersionKey: marker}, true},
		{map[string][]byte{}, false},
		{map[string][]byte{ftsVersionKey: []byte("1")}, false},
		{map[string][]byte{ftsVersionKey: marker, ftsRebuildKey: nil}, false},
		{map[string][]byte{ftsRebuildKey: nil}, false},
	} {
		if got := indexCurrent(c.vals); got != c.want {
			t.Errorf("indexCurrent(%v) = %v", c.vals, got)
		}
	}

	// a rebuild finishes only over its own key
	if !rebuildOwned(map[string][]byte{ftsRebuildKey: []byte("t1")}, "t1") ||
		rebuildOwned(map[string][]byte{ftsRebuildKey: []byte("t2")}, "t1") ||
		rebuildOwned(map[string][]byte{}, "t1") {
		t.Error("rebuildOwned")
	}

	for _, c := range []struct {
		cur   map[string][]byte
		fresh bool
		want  bool
	}{
		{map[string][]byte{}, true, true},
		{map[string][]byte{}, false, false}, // posting shards of an older index
		{map[string][]byte{"n": []byte("7")}, true, false},
		{map[string][]byte{ftsRebuildKey: nil}, true, false}, // a delivery during a rebuild
	} {
		set := map[string][]byte{}
		markFreshIndex(c.cur, set, c.fresh)

		if _, got := set[ftsVersionKey]; got != c.want {
			t.Errorf("markFreshIndex(%v, fresh %v) set the marker: %v", c.cur, c.fresh, got)
		}
	}
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
