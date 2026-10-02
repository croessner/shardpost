//go:build tentacle

package cephstore

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"shardpost/internal/fts"
	"shardpost/internal/model"
)

// Cluster tests of the retries of a write within one generation and of
// the clean-ups review round 1 of B14b asked for.

// retryWords are the body words of the retry message: enough of them to
// fall on every posting object of a small generation.
func retryWords() []string {
	out := make([]string, 0, 48)
	for i := range 48 {
		out = append(out, fmt.Sprintf("qz%02dretryword", i))
	}

	return out
}

// freezeStale freezes a posting object with the token of a run no
// conversion names any more (it removed its "next" and crashed before
// the lift): the next write meets an anomaly there, lifts the freeze and
// retries in the same generation.
func freezeStale(t *testing.T, u *User, oid string) {
	t.Helper()

	if err := casOmap(u, u.idx, oid, nil, func(map[string][]byte) (map[string][]byte, []string) {
		return map[string][]byte{markState: []byte(frozenPrefix + "job-dead/1")}, nil
	}); err != nil {
		t.Fatal(err)
	}
}

// dictHas reports whether every dictionary key of the body term is in
// the dictionary of loc (want true) or none is (want false).
func dictHas(t *testing.T, u *User, loc ftsLoc, term string, want bool) {
	t.Helper()

	_, tok, _ := strings.Cut(term, ":")

	for _, tri := range fts.Trigrams(tok) {
		key := tri + dictSep + term
		if _, ok := omapKeys(t, u, loc.dictOID(tri), key)[key]; ok != want {
			t.Errorf("dictionary key %q: present %v, want %v", key, ok, want)
		}
	}
}

// TestLayoutRetryKeepsDictionary (review B1): a write that retries in the
// same generation (here after a stale freeze on one posting object; the
// other objects of the first attempt are written) names every new term
// in the dictionary, so a substring search finds them; a removal that
// retries removes every emptied term from it. For layout 2 (16 posting
// objects, no record) and layout 3 with S = 4.
func TestLayoutRetryKeepsDictionary(t *testing.T) {
	for _, from := range []int{2, 3} {
		t.Run(fmt.Sprintf("layout%d", from), func(t *testing.T) {
			s := testStore(t)
			if from == 3 {
				s = layout3Store(t, func(c *FTSConfig) { c.ShardsMin = 4 })
			}

			u := testUser(t, s)
			mustMailbox(t, u, "INBOX")
			deliverDocs(t, u, layoutDocs(3))

			loc := curLoc(layoutOf(t, u), u.ftsShards)
			if loc.s < 2 {
				t.Fatalf("generation with %d posting objects", loc.s)
			}

			words := retryWords()

			freezeStale(t, u, loc.postingOIDn(0))

			m, err := u.Deliver("INBOX", []byte("Subject: retry\r\n\r\n"+strings.Join(words, " ")+"\r\n"), nil, time.Time{})
			if err != nil {
				t.Fatal(err)
			}

			checkRetryWords(t, u, loc, m.GUID, words, true)

			freezeStale(t, u, loc.postingOIDn(1))

			metas, err := u.ListMessagesByName("INBOX")
			if err != nil {
				t.Fatal(err)
			}

			for _, meta := range metas {
				if meta.GUID == m.GUID {
					if err := u.Expunge("INBOX", []Meta{meta}); err != nil {
						t.Fatal(err)
					}
				}
			}

			checkRetryWords(t, u, loc, m.GUID, words, false)
		})
	}
}

// checkRetryWords checks the dictionary keys of the words and the
// substring search for an inner part of each (found with want, not found
// without).
func checkRetryWords(t *testing.T, u *User, loc ftsLoc, guid string, words []string, want bool) {
	t.Helper()

	u.ftsForget()

	for _, word := range words {
		dictHas(t, u, loc, fts.FieldBody+":"+word, want)

		inner := word[1:9] // "z07retry": an inner part, no word start
		hits, err := u.SearchCandidates([]string{fts.FieldBody}, inner)

		switch {
		case err != nil:
			t.Fatalf("substring %q: %v", inner, err)
		case hits == nil:
			t.Errorf("substring %q: no candidate set", inner)
		case hits[guid] != want:
			t.Errorf("substring %q: found %v, want %v", inner, hits[guid], want)
		}
	}
}

// TestLayout3EmptyConversionMarked (review M2): an account converted to
// layout 3 while empty gets the layout 3 version marker with its first
// ids, so substring searches narrow instead of scanning.
func TestLayout3EmptyConversionMarked(t *testing.T) {
	s := layout3Store(t, nil)
	u := testUser(t, s)
	mustMailbox(t, u, "INBOX")

	convert(t, u, model.FTSLayoutRequest{Layout: model.FTSLayout3, Shards: 2})

	if l := layoutOf(t, u); l == nil || l.F != model.FTSLayout3 {
		t.Fatalf("record after converting an empty account: %+v", l)
	}

	m, err := u.Deliver("INBOX", []byte("Subject: firstone\r\n\r\nzzfirstbody\r\n"), nil, time.Time{})
	if err != nil {
		t.Fatal(err)
	}

	checkMapMarkers(t, u, 3, "1")

	u.ftsForget()

	hits, err := u.SearchCandidates([]string{fts.FieldBody}, "firstbod")
	if err != nil || hits == nil || !hits[m.GUID] {
		t.Errorf("substring search after the first delivery: %v %v", hits, err)
	}
}

// TestLayout2WithoutRecordCreates (review M3): a layout 2 index without a
// record creates a missing posting object in the writer's
// compare-and-swap, as 0.44.0 does: no anomaly, no read of the record.
func TestLayout2WithoutRecordCreates(t *testing.T) {
	s := testStore(t)
	u := testUser(t, s)
	mustMailbox(t, u, "INBOX")
	deliverDocs(t, u, layoutDocs(3))

	loc := layout2Loc(u.ftsShards)
	term := "s:createdagain"
	oid := loc.postingOID(term)

	if err := u.deleteObjects([]string{oid}); err != nil {
		t.Fatal(err)
	}

	guid := docGUID(60)

	ids, _, err := u.ensureDocIDs([]string{guid})
	if err != nil {
		t.Fatal(err)
	}

	docsOf, terms, headers := termDocs([]ftsDoc{{guid: guid, raw: []byte("Subject: createdagain\r\n\r\nbody\r\n")}}, ids)
	w := &ftsWrite{wait: u.ftsCfg.frozenWait}
	w.follow(nil, u.ftsShards)

	if err := u.indexWith(w, docsOf, terms, headers); err != nil {
		t.Fatal(err)
	}

	if w.noRecord || w.gens != 0 {
		t.Errorf("the write met an anomaly: noRecord %v, generations %d", w.noRecord, w.gens)
	}

	if !hasTerm(t, u, loc, term) {
		t.Errorf("%s not created with the term", oid)
	}
}

// TestLayoutForeignOldDropped (review M4): a run that crashed after its
// P6 leaves "old" in the record; a run of another token that finds
// nothing to do (due_only) still removes those sources and the entry.
func TestLayoutForeignOldDropped(t *testing.T) {
	s := layout3Store(t, nil)
	u := testUser(t, s)
	mustMailbox(t, u, "INBOX")
	deliverDocs(t, u, layoutDocs(5))

	src := curLoc(layoutOf(t, u), u.ftsShards)

	crashAt(t, u.name, phaseP6)

	if _, err := u.ConvertFTSLayout(model.FTSLayoutRequest{Layout: model.FTSLayout3, Shards: 1, Tok: "job-crashed/1"}); err == nil {
		t.Fatal("the crash hook did not stop the run")
	}

	ftsLayoutHook = nil

	if l := layoutOf(t, u); len(l.Old) != 1 {
		t.Fatalf("record after the crash %+v", l)
	}

	if rep := convert(t, u, model.FTSLayoutRequest{DueOnly: true, Tok: "job-other/1"}); !rep.Noop {
		t.Errorf("due_only after the crash: %+v", rep)
	}

	if l := layoutOf(t, u); len(l.Old) != 0 || l.Next != nil {
		t.Errorf("record after due_only %+v", l)
	}

	mustExist(t, u, false, src.objects()...)

	if hits, err := u.SearchFields([]string{fts.FieldBody}, "lorem"); err != nil || len(hits) == 0 {
		t.Errorf("search after the clean-up: %v %v", hits, err)
	}
}

// TestLayoutScrubMixedHeader (review M6): a layout 2 header object alone
// in a layout 3 account is a mixed layout.
func TestLayoutScrubMixedHeader(t *testing.T) {
	s := layout3Store(t, nil)
	u := testUser(t, s)
	mustMailbox(t, u, "INBOX")
	deliverDocs(t, u, layoutDocs(3))

	if err := u.idx.SetOmap(layout2Loc(u.ftsShards).headerOIDn(0), map[string][]byte{docGUID(1): []byte("x")}); err != nil {
		t.Fatal(err)
	}

	rep, err := u.Scrub(ScrubOptions{})
	if err != nil {
		t.Fatal(err)
	}

	if rep.FTSMixedLayout != 1 {
		t.Errorf("report %+v", *rep)
	}
}
