//go:build tentacle

package cephstore

import (
	"fmt"
	"maps"
	"math/rand"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"shardpost/internal/fts"
	"shardpost/internal/memstore"
	"shardpost/internal/model"
)

// The identifier filter of the dictionary and the text preparation of
// a generation (stage B14h, temp/13 §4, §10), without a cluster.

// idOf is identifier i of the test corpus: 32 runes, letters and digits.
func idOf(i int) string { return fmt.Sprintf("k%031x", uint64(i+1)*0x9e3779b97f4a7c15) }

// longIDOf is a long identifier (60 runes: window terms, markers).
func longIDOf(i int) string {
	return fmt.Sprintf("q%029x%030x", uint64(i+7)*0x51afd7ed558ccd, uint64(i+3)*0xc4ceb9fe1a85ec53)
}

// idDocs are layoutDocs with identifiers in the body: one of 32 runes
// and one of 60 runes per message, a pure number and a long word
// without digits.
func idDocs(n int) map[string]string {
	docs := layoutDocs(n)

	for k, raw := range docs {
		var i int

		_, _ = fmt.Sscanf(k, "g%d", &i)
		docs[k] = raw + fmt.Sprintf("track %s sig %s nr 123456789012345678901234567 wort %s\r\n",
			idOf(i), longIDOf(i), strings.Repeat("abc", 9))
	}

	return docs
}

// filtLoc is a layout 3 generation of the tests.
func filtLoc(s, d, dx int) ftsLoc {
	return ftsLoc{f: model.FTSLayout3, g: "0123456789abcdef", s: s, d: d, h: 1, dx: dx}
}

// TestDictEntriesFiltered: in a generation with the filter a body
// identifier has one start entry "#<tri0>:b:<token>" in the object of
// its first trigram (runes, not bytes); other terms, and every term of
// s, f, t, keep their trigram keys; the count is dictKeysOf; without the
// filter the keys are those of before.
func TestDictEntriesFiltered(t *testing.T) {
	id := "äb1" + strings.Repeat("c2", 11) // 25 runes, 27 bytes
	terms := []string{"b:" + id, "s:" + id, "b:rechnung", "b:" + strings.Repeat("ab", 13), "h:" + id, "b:ab"}

	for _, d := range []int{1, 4, 32} {
		loc := filtLoc(2, d, 25)
		byShard, n := dictEntriesAt(loc, terms)

		checkStartEntry(t, loc, byShard, n, terms, id)

		plain, pn := dictEntriesAt(filtLoc(2, d, 0), terms)
		ref, rn := dictEntriesAt(ftsLoc{f: model.FTSLayout3, g: "0123456789abcdef", s: 2, d: d, h: 1}, terms)

		if !reflect.DeepEqual(plain, ref) || pn != rn || pn != n-1+len(fts.Trigrams(id)) {
			t.Errorf("D=%d: without the filter %d keys, before %d, filtered %d", d, pn, rn, n)
		}
	}

	// layout 2 is never filtered
	if _, n := dictEntries([]string{"b:" + id}); n != len(fts.Trigrams(id)) {
		t.Errorf("layout 2: %d keys", n)
	}
}

// checkStartEntry checks the entries of terms in the filtered loc: the
// count is dictKeysOf, the identifier id has its start entry in the
// object of its first trigram and no trigram key.
func checkStartEntry(t *testing.T, loc ftsLoc, byShard map[string][]string, n int, terms []string, id string) {
	t.Helper()

	want := 0
	for _, term := range terms {
		want += dictKeysOf(loc, term)
	}

	if n != want {
		t.Errorf("D=%d: %d entries, dictKeysOf %d", loc.d, n, want)
	}

	tri := fts.Tri0(id)
	start := startEntry(tri, "b:"+id)

	if !slices.Contains(byShard[loc.dictOID(tri)], start) {
		t.Errorf("D=%d: start entry %q not in %s: %v", loc.d, start, loc.dictOID(tri), byShard)
	}

	for oid, keys := range byShard {
		for _, k := range keys {
			if strings.HasSuffix(k, ":b:"+id) && k != start {
				t.Errorf("D=%d: trigram key %q of a filtered identifier in %s", loc.d, k, oid)
			}
		}
	}
}

// TestStartEntryOrder: "#" sorts before every trigram range (letters and
// digits from 0x30 on), is no marker and is told apart from trigram keys.
func TestStartEntryOrder(t *testing.T) {
	k := startEntry("abc", "b:abc123")
	if k != "#abc:b:abc123" || !isStartEntry(k) || isMarker(k) || isStartEntry("abc:b:abc123") {
		t.Fatalf("start entry %q", k)
	}

	for _, tri := range []string{"000", "abc", "äöü", "日本語", "a1b"} {
		if k >= tri+dictSep || strings.HasPrefix(tri, startPrefix) {
			t.Errorf("%q does not sort before %q", k, tri)
		}
	}
}

// filteredPair indexes the documents into an unfiltered and a filtered
// generation of D dictionary objects.
func filteredPair(docs map[string]string, d int) (*memIndex, *memIndex) {
	return newMemIndexAt(filtLoc(2, d, 0), docs), newMemIndexAt(filtLoc(2, d, 25), docs)
}

// candidates runs searchCandidates and fails on an error.
func candidates(t *testing.T, ix candidateIndex, fields []string, query string) map[string]bool {
	t.Helper()

	got, err := searchCandidates(ix, fields, query)
	if err != nil {
		t.Fatal(err)
	}

	return got
}

// TestFilteredSearch (temp/13 §5.1): with D ∈ {1, 4, 32} every search
// but an inner piece of an identifier answers as without the filter; an
// identifier is found as a whole word and by its start (also the start
// of a window of a long one), an inner piece only without the filter.
// The start entry range costs no read of its own; removals take the
// start entries out.
func TestFilteredSearch(t *testing.T) {
	docs := idDocs(30)
	id, long := idOf(3), longIDOf(3)

	for _, d := range []int{1, 4, 32} {
		plain, filt := filteredPair(docs, d)

		for _, q := range layoutQueries {
			sameCandidates(t, plain, filt, q.fields, q.query, false)
		}

		for _, q := range []string{" " + id + " ", id, id[:12], "track " + id[:9], long[:30], long[20:45], " " + long[:40], "123456789012"} {
			sameCandidates(t, plain, filt, []string{fts.FieldBody}, q, true)
		}

		innerPieces(t, plain, filt, "g003", id[6:18], long[5:17])
		sameReads(t, plain, filt, id[:12])

		filt.remove("g003", fts.Terms([]byte(docs["g003"])))

		for oid, keys := range filt.keys {
			for _, k := range keys {
				if isStartEntry(k) && strings.Contains(k, id) {
					t.Errorf("D=%d: start entry %q left in %s", d, k, oid)
				}
			}
		}
	}
}

// sameCandidates fails unless both indexes answer the query alike
// (narrowing, with narrow).
func sameCandidates(t *testing.T, plain, filt *memIndex, fields []string, q string, narrow bool) {
	t.Helper()

	want, got := candidates(t, plain, fields, q), candidates(t, filt, fields, q)
	if !maps.Equal(got, want) || (got == nil) != (want == nil) || (narrow && got == nil) {
		t.Errorf("D=%d %v %q: filtered %v, unfiltered %v", filt.loc.d, fields, q, got, want)
	}
}

// innerPieces: the inner pieces of identifiers of the message guid are
// found without the filter only.
func innerPieces(t *testing.T, plain, filt *memIndex, guid string, queries ...string) {
	t.Helper()

	b := []string{fts.FieldBody}

	for _, q := range queries {
		if got := candidates(t, plain, b, q); !got[guid] {
			t.Errorf("D=%d %q unfiltered: %v", plain.loc.d, q, got)
		}

		if got := candidates(t, filt, b, q); got == nil || got[guid] {
			t.Errorf("D=%d %q filtered: %v, want narrowed without %s", filt.loc.d, q, got, guid)
		}
	}
}

// sameReads: the start range is read with its trigram range, the search
// costs the reads it costs without the filter.
func sameReads(t *testing.T, plain, filt *memIndex, q string) {
	t.Helper()

	plain.reads.Store(0)
	filt.reads.Store(0)

	candidates(t, plain, []string{fts.FieldBody}, q)
	candidates(t, filt, []string{fts.FieldBody}, q)

	if p, f := plain.reads.Load(), filt.reads.Load(); p != f {
		t.Errorf("D=%d: %d reads filtered, %d unfiltered", filt.loc.d, f, p)
	}
}

// TestFilteredSearchProperty (temp/13 §10): random corpora with
// identifiers answer every query filtered as unfiltered, except where a
// word of the query is an inner piece of an identifier; there exactly
// the messages are missing in which the word starts no term, and the
// memstore emulation of the filter answers the same (parity).
func TestFilteredSearchProperty(t *testing.T) {
	r := rand.New(rand.NewSource(13))
	alnum := []rune("abcdef0123456789xyz")

	word := func(n int) string {
		w := make([]rune, n)
		for i := range w {
			w[i] = alnum[r.Intn(len(alnum))]
		}

		return string(w)
	}

	docs := map[string]string{}

	var words []string

	for i := range 40 {
		var line []string

		for range 8 {
			w := word([]int{4, 12, 24, 25, 30, 41, 70}[r.Intn(7)])
			line = append(line, w)
			words = append(words, w)
		}

		docs[fmt.Sprintf("g%03d", i)] = fmt.Sprintf("Subject: s%d\r\n\r\n%s\r\n", i, strings.Join(line, " "))
	}

	ms := memstoreWith(t, docs, memstore.FTSOptions{DictIDMinRunes: 25})
	b := []string{fts.FieldBody}

	for _, d := range []int{1, 4} {
		plain, filt := filteredPair(docs, d)

		for range 400 {
			w := []rune(words[r.Intn(len(words))])
			i := r.Intn(len(w))
			q := string(w[i:min(len(w), i+3+r.Intn(20))])

			want, got := candidates(t, plain, b, q), candidates(t, filt, b, q)

			for g := range got {
				if !want[g] {
					t.Errorf("D=%d %q: %s only filtered", d, q, g)
				}
			}

			for g := range want {
				if !got[g] && startsTerm(docs[g], q, 25) {
					t.Errorf("D=%d %q: %s missing although the word starts a term or is in one that is no identifier", d, q, g)
				}
			}

			if m := ms.candidates(t, b, q); !maps.Equal(m, got) {
				t.Errorf("D=%d %q: memstore %v, filtered index %v", d, q, m, got)
			}
		}
	}
}

// startsTerm reports whether a message must be found by query word q
// under the filter of n runes: q is in a body term that is no
// identifier, or starts one that is.
func startsTerm(raw, q string, n int) bool {
	for _, term := range fts.Terms([]byte(raw)) {
		f, tok, _ := strings.Cut(term, ":")
		if f != fts.FieldBody {
			continue
		}

		if fts.IsIdentifier(tok, n) {
			if strings.HasPrefix(tok, q) {
				return true
			}
		} else if strings.Contains(tok, q) {
			return true
		}
	}

	return false
}

// msIndex is a memstore account over the documents, with the GUIDs
// mapped to the document names.
type msIndex struct {
	u      model.User
	byGUID map[string]string
}

// memstoreWith delivers the documents into a memstore account with the
// full-text options o.
func memstoreWith(t *testing.T, docs map[string]string, o memstore.FTSOptions) *msIndex {
	t.Helper()

	s := memstore.New()
	s.SetFTSOptions(o)

	u, err := s.User("parity")
	if err != nil {
		t.Fatal(err)
	}

	if err := u.CreateMailbox("INBOX", nil); err != nil {
		t.Fatal(err)
	}

	m := &msIndex{u: u, byGUID: map[string]string{}}

	for _, k := range slices.Sorted(maps.Keys(docs)) {
		meta, err := u.Deliver("INBOX", []byte(docs[k]), nil, time.Now())
		if err != nil {
			t.Fatal(err)
		}

		m.byGUID[meta.GUID] = k
	}

	return m
}

// candidates is the candidate search of the memstore account, by
// document name.
func (m *msIndex) candidates(t *testing.T, fields []string, q string) map[string]bool {
	t.Helper()

	hits, err := m.u.SearchCandidates(fields, q)
	if err != nil {
		t.Fatal(err)
	}

	if hits == nil {
		return nil
	}

	out := map[string]bool{}
	for g := range hits {
		out[m.byGUID[g]] = true
	}

	return out
}

// TestNoSigIndex (c3): a generation with the text preparation "nosig"
// has no terms of the signature fields of attached headers, the other
// fields and the header of the message itself stay; memstore with
// "nosig" answers the same.
func TestNoSigIndex(t *testing.T) {
	fwd := "From: a@example.org\r\nSubject: fwd\r\nMIME-Version: 1.0\r\nContent-Type: multipart/mixed; boundary=b1\r\n\r\n" +
		"--b1\r\nContent-Type: text/plain\r\n\r\nsiehe anhang\r\n--b1\r\nContent-Type: message/rfc822\r\n\r\n" +
		"DKIM-Signature: v=1; b=sigtoken9q7w\r\nReceived: from x by y id recvtoken3k\r\nSubject: innen\r\n\r\ntext innen\r\n--b1--\r\n"
	docs := map[string]string{"g000": fwd}

	all := newMemIndexAt(filtLoc(1, 1, 0), docs)
	nosig := &memIndex{keys: map[string][]string{}, posts: map[string]map[string]bool{}, loc: ftsLoc{f: 3, g: "0123456789abcdef", s: 1, d: 1, h: 1, ah: fts.AttachedHeadersNoSig}}
	nosig.add("g000", fts.TermsWith([]byte(fwd), nosig.loc.text()))

	ms := memstoreWith(t, docs, memstore.FTSOptions{AttachedHeaders: fts.AttachedHeadersNoSig})
	b := []string{fts.FieldBody}

	for _, c := range []struct {
		q       string
		all, ns bool
	}{
		{"sigtoken9q7w", true, false},
		{"recvtoken3k", true, false},
		{" innen ", true, true},
		{"anhang", true, true},
	} {
		if got := candidates(t, all, b, c.q)["g000"]; got != c.all {
			t.Errorf("all %q: %v", c.q, got)
		}

		if got := candidates(t, nosig, b, c.q)["g000"]; got != c.ns {
			t.Errorf("nosig %q: %v", c.q, got)
		}

		if got := ms.candidates(t, b, c.q)["g000"]; got != c.ns {
			t.Errorf("memstore nosig %q: %v", c.q, got)
		}
	}
}

// TestVersionMarkers (temp/13 §4.3): which marker a generation gets and
// which ones a server trusts — every one of this version, but an index
// of the text preparation "nosig" only with fts.attached_headers nosig;
// a rebuild key takes the trust away.
func TestVersionMarkers(t *testing.T) {
	plain, filt := filtLoc(1, 1, 0), filtLoc(1, 1, 25)
	nosig, both := plain, filt
	nosig.ah, both.ah = fts.AttachedHeadersNoSig, fts.AttachedHeadersNoSig

	for _, c := range []struct {
		loc  ftsLoc
		want string
	}{{plain, "2"}, {filt, "3"}, {nosig, "4"}, {both, "4"}, {layout2Loc(16), "2"}} {
		if got := versionFor(c.loc); got != c.want {
			t.Errorf("versionFor(%+v) = %s, want %s", c.loc, got, c.want)
		}
	}

	l2 := layout2Loc(16)

	for _, c := range []struct {
		loc    ftsLoc
		v      string
		reader string
		want   bool
	}{
		{plain, "2", "", true},
		{filt, "3", "", true},
		{plain, "3", "", true}, // a reshard of a version before 0.50.0
		{nosig, "4", "", false},
		{nosig, "4", fts.AttachedHeadersNoSig, true},
		{plain, "4", "", false}, // "ah" lost by a version before 0.50.0
		{nosig, "2", "", false}, // the record says nosig
		{plain, "5", "", false},
		{l2, "2", "", true},
		{l2, "3", "", true},
		{l2, "4", "", false},
		{l2, "4", fts.AttachedHeadersNoSig, true},
	} {
		key := ftsVersionKey3
		if !c.loc.v3() {
			key = ftsVersionKey
		}

		vals := map[string][]byte{key: []byte(c.v)}
		if got := ftsTrusted(vals, c.loc, c.reader); got != c.want {
			t.Errorf("ftsTrusted(%s=%s, ah %q, reader %q) = %v", key, c.v, c.loc.ah, c.reader, got)
		}

		vals[ftsRebuildKey] = []byte("t")
		if ftsTrusted(vals, c.loc, c.reader) {
			t.Errorf("trusted during a rebuild: %s=%s", key, c.v)
		}
	}
}

// TestInstallGenMarker: a fresh index gets the marker of its generation
// and the generation's properties in its record; none while a rebuild
// runs.
func TestInstallGenMarker(t *testing.T) {
	for _, loc := range []ftsLoc{filtLoc(1, 1, 0), filtLoc(1, 1, 25), {f: 3, g: "0123456789abcdef", s: 1, d: 1, h: 1, ah: fts.AttachedHeadersNoSig}} {
		checkInstallGen(t, loc)

		set := map[string][]byte{}
		markFreshIndex3(map[string][]byte{}, set, loc)

		if string(set[ftsVersionKey3]) != versionFor(loc) {
			t.Errorf("markFreshIndex3(%+v) = %q", loc, set[ftsVersionKey3])
		}
	}

	// an unknown rule version filters nothing here
	if dxRunes(&ftsDX{V: 2, N: 25}) != 0 || dxRunes(nil) != 0 || dxRunes(newDX(25)) != 25 || newDX(0) != nil {
		t.Error("dxRunes")
	}
}

// checkInstallGen checks the record and marker installGen writes for
// loc, and that it writes no marker during a rebuild.
func checkInstallGen(t *testing.T, loc ftsLoc) {
	t.Helper()

	set := map[string][]byte{}
	l := installGen(map[string][]byte{}, set, loc, 1)

	if string(set[ftsVersionKey3]) != versionFor(loc) || dxRunes(l.DX) != loc.dx || l.AH != loc.ah {
		t.Errorf("installGen(%+v): V=%q record %+v", loc, set[ftsVersionKey3], l)
	}

	got, err := parseLayout(set[ftsLayoutKey])
	if err != nil || !curLoc(got, 16).sameProps(loc) {
		t.Errorf("record %s: %v", set[ftsLayoutKey], err)
	}

	set = map[string][]byte{}
	installGen(map[string][]byte{ftsRebuildKey: nil}, set, loc, 1)

	if _, ok := set[ftsVersionKey3]; ok {
		t.Errorf("installGen during a rebuild set V")
	}
}

// TestSwitchMarkers (temp/13 §4.3): the switch of a conversion sets the
// target's marker from its properties when the source's was trusted
// (3→3, 2→3), and 3→2 writes "2" — "4" for postings of "nosig" — where a
// version before 0.50.0 copied "3".
func TestSwitchMarkers(t *testing.T) {
	src3, tgt3 := filtLoc(1, 1, 25), filtLoc(2, 2, 0)
	tgt3.g = "fedcba9876543210"

	rec := &ftsLayout{F: 3, G: src3.g, S: 1, D: 1, H: 1, DX: newDX(25)}

	for _, c := range []struct {
		name     string
		cur      map[string][]byte
		src, tgt ftsLoc
		wantV    string // "" for removed
	}{
		{"3→3 filtered to plain", map[string][]byte{ftsVersionKey3: []byte("3")}, src3, tgt3, "2"},
		{"3→3 plain to filtered", map[string][]byte{ftsVersionKey3: []byte("2")}, tgt3, src3, "3"},
		{"3→3 untrusted", map[string][]byte{}, src3, tgt3, ""},
		{"2→3 filtered", map[string][]byte{ftsVersionKey: []byte("2")}, layout2Loc(16), src3, "3"},
		{"2→3 untrusted", map[string][]byte{ftsVersionKey: []byte("1")}, layout2Loc(16), src3, ""},
	} {
		checkSwitchTo3(t, c.name, rec, c.cur, c.src, c.tgt, c.wantV)
	}

	nosig := src3
	nosig.ah = fts.AttachedHeadersNoSig

	for _, c := range []struct {
		v    string
		src  ftsLoc
		want string
	}{{"3", src3, "2"}, {"2", src3, "2"}, {"4", src3, "4"}, {"4", nosig, "4"}, {"", src3, ""}} {
		cur := map[string][]byte{}
		if c.v != "" {
			cur[ftsVersionKey3] = []byte(c.v)
		}

		set, rm := switchTo2(cur, c.src)
		if got := string(set[ftsVersionKey]); got != c.want || !slices.Contains(rm, ftsVersionKey3) || !slices.Contains(rm, ftsLayoutKey) {
			t.Errorf("switchTo2(V=%q): v=%q rm %v", c.v, got, rm)
		}
	}
}

// checkSwitchTo3 checks switchTo3: the marker V (wantV, "" removed), v
// removed after 2→3, the target's properties in the record and the
// source's in its "old" entry.
func checkSwitchTo3(t *testing.T, name string, rec *ftsLayout, cur map[string][]byte, src, tgt ftsLoc, wantV string) {
	t.Helper()

	set, rm := switchTo3(cur, rec, src, tgt, "tok", convertStats{})

	if got := string(set[ftsVersionKey3]); got != wantV || (wantV == "" && !slices.Contains(rm, ftsVersionKey3)) {
		t.Errorf("%s: V=%q rm %v", name, got, rm)
	}

	if !src.v3() && !slices.Contains(rm, ftsVersionKey) {
		t.Errorf("%s: v not removed", name)
	}

	l, _ := parseLayout(set[ftsLayoutKey])
	if l == nil || dxRunes(l.DX) != tgt.dx || l.AH != tgt.ah || len(l.Old) != 1 || dxRunes(l.Old[0].DX) != src.dx {
		t.Errorf("%s: record %s", name, set[ftsLayoutKey])
	}
}

// TestVersionDrift: the scrub's finding fts_version_drift.
func TestVersionDrift(t *testing.T) {
	rec := func(l *ftsLayout) []byte { return l.encode() }
	plain := &ftsLayout{F: 3, G: "0123456789abcdef", S: 1, D: 1, H: 1}
	filt := &ftsLayout{F: 3, G: "0123456789abcdef", S: 1, D: 1, H: 1, DX: newDX(25)}
	nosig := &ftsLayout{F: 3, G: "0123456789abcdef", S: 1, D: 1, H: 1, AH: fts.AttachedHeadersNoSig}

	for _, c := range []struct {
		cur        map[string][]byte
		kind       int
		key, value string
	}{
		{map[string][]byte{ftsVersionKey: []byte("3")}, driftFixable, ftsVersionKey, "2"},
		{map[string][]byte{ftsVersionKey: []byte("2")}, driftNone, "", ""},
		{map[string][]byte{ftsVersionKey: []byte("4")}, driftNone, "", ""},
		{map[string][]byte{ftsLayoutKey: rec(plain), ftsVersionKey3: []byte("3")}, driftFixable, ftsVersionKey3, "2"},
		{map[string][]byte{ftsLayoutKey: rec(filt), ftsVersionKey3: []byte("3")}, driftNone, "", ""},
		{map[string][]byte{ftsLayoutKey: rec(plain), ftsVersionKey3: []byte("4")}, driftReport, ftsVersionKey3, ""},
		{map[string][]byte{ftsLayoutKey: rec(plain), ftsVersionKey3: []byte("2")}, driftNone, "", ""},
		// a version before 0.50.0 marked an empty filtered or nosig generation
		{map[string][]byte{ftsLayoutKey: rec(filt), ftsVersionKey3: []byte("2")}, driftFixable, ftsVersionKey3, "3"},
		{map[string][]byte{ftsLayoutKey: rec(nosig), ftsVersionKey3: []byte("2")}, driftFixable, ftsVersionKey3, "4"},
		{map[string][]byte{ftsLayoutKey: rec(nosig), ftsVersionKey3: []byte("4")}, driftNone, "", ""},
	} {
		if kind, key, value := versionDrift(c.cur, 16); kind != c.kind || key != c.key || value != c.value {
			t.Errorf("versionDrift(%q) = %d %q %q, want %d %q %q", c.cur, kind, key, value, c.kind, c.key, c.value)
		}
	}
}
