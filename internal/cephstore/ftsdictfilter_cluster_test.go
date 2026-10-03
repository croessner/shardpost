//go:build tentacle

package cephstore

import (
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	"shardpost/internal/bench"
	"shardpost/internal/fts"
	"shardpost/internal/model"
)

// Cluster tests of the identifier filter and the text preparation of
// a generation (stage B14h, temp/13 §10).

// setDictFilterMark sets or removes the cluster mark of the filter.
func setDictFilterMark(t *testing.T, s *Store, on bool) {
	t.Helper()
	setMark(t, s, model.FTSDictFilterMarkKey, on)
}

// dictFilterStore is a layout 3 store with the cluster marks of layout
// 3 and (mark) of the dictionary filter; fn changes the configuration.
func dictFilterStore(t *testing.T, mark bool, fn func(*FTSConfig)) *Store {
	t.Helper()

	s := layout3Store(t, fn)
	setDictFilterMark(t, s, mark)
	t.Cleanup(func() { setDictFilterMark(t, s, false) })

	return s
}

// filterOn is the configuration fts.dict_filter "ids" with N = 25.
func filterOn(c *FTSConfig) { c.DictFilter, c.DictIDMinRunes = true, 25 }

// filledUsers are new accounts of the stores, each with the documents
// in INBOX.
func filledUsers(t *testing.T, docs map[string]string, stores ...*Store) []*User {
	t.Helper()

	out := make([]*User, 0, len(stores))

	for _, s := range stores {
		u := testUser(t, s)
		mustMailbox(t, u, "INBOX")
		deliverDocs(t, u, docs)

		out = append(out, u)
	}

	return out
}

// idQueries are the identifier searches over idDocs: inner pieces of
// identifiers (inner) and the rest (whole words, starts, window starts,
// non-identifiers), which a filter must not change.
func idQueries() (rest, inner []string) {
	id, long := idOf(3), longIDOf(3)

	rest = []string{" " + id + " ", id, id[:12], "track " + id[:9], long[:30], " " + long[:40], "123456789012", "cabcabc", " unique3word"}
	inner = []string{id[6:18], long[5:17], idOf(41)[4:16]}

	return rest, inner
}

// idState is what the identifier searches of an account answer: IMAP
// candidates and word search, by query.
func idState(t *testing.T, u *User, queries []string) map[string]string {
	t.Helper()

	out := map[string]string{}
	b := []string{fts.FieldBody}

	for _, q := range queries {
		hits, err := u.SearchCandidates(b, q)
		if err != nil {
			t.Fatal(err)
		}

		out["imap "+q] = fmt.Sprint(hits == nil, slices.Sorted(maps.Keys(hits)))

		words, err := u.SearchFields(b, q)
		if err != nil {
			t.Fatal(err)
		}

		out["jmap "+q] = fmt.Sprint(slices.Sorted(maps.Keys(words)))
	}

	return out
}

// dictKeys are the keys of every dictionary object of the account's
// current generation (all objects: the set does not depend on D) and
// the sum of the "!dn" counters.
func dictKeys(t *testing.T, u *User) (map[string]bool, int64) {
	t.Helper()

	loc := curLoc(layoutOf(t, u), u.ftsShards)
	out := map[string]bool{}

	for _, oid := range loc.dictOIDs() {
		for k := range omapKeys(t, u, oid, "") {
			if !isMarker(k) {
				out[k] = true
			}
		}
	}

	var dn int64
	for _, oid := range loc.postingOIDs() {
		dn += atoiMarker(omapKeys(t, u, oid, markDict)[markDict])
	}

	return out, dn
}

// startEntries counts the start entries among dictionary keys.
func startEntries(keys map[string]bool) int {
	n := 0

	for k := range keys {
		if isStartEntry(k) {
			n++
		}
	}

	return n
}

// versionMarkers are the version markers of the id map ("" absent).
func versionMarkers(t *testing.T, u *User) (v, v3 string) {
	t.Helper()

	m := omapKeys(t, u, oidFTSMap, "")

	return string(m[ftsVersionKey]), string(m[ftsVersionKey3])
}

// checkGeneration checks the current generation of the account: the
// filter of its record, the marker V, start entries exactly with the
// filter and "!dn" equal to the dictionary keys. It returns the keys.
func checkGeneration(t *testing.T, stage string, u *User, dx int, v3 string) map[string]bool {
	t.Helper()

	l := layoutOf(t, u)
	if l == nil || dxRunes(l.DX) != dx || l.Next != nil {
		t.Fatalf("%s: record %+v", stage, l)
	}

	keys, dn := dictKeys(t, u)
	_, v := versionMarkers(t, u)

	if v != v3 || (startEntries(keys) > 0) != (dx > 0) || int64(len(keys)) != dn {
		t.Errorf("%s: V=%q (want %q), %d start entries, %d keys, !dn %d", stage, v, v3, startEntries(keys), len(keys), dn)
	}

	return keys
}

// expungeFirst expunges the first n messages of INBOX (all for n < 0).
func expungeFirst(t *testing.T, u *User, n int) {
	t.Helper()

	metas, err := u.ListMessagesByName("INBOX")
	if err != nil {
		t.Fatal(err)
	}

	if n >= 0 {
		metas = metas[:n]
	}

	if err := u.Expunge("INBOX", metas); err != nil {
		t.Fatal(err)
	}
}

// TestDictFilterDefaultUnchanged: by default (fts.dict_filter none) and
// with "ids" but without the cluster mark a fresh index has no filter,
// the marker "2" and no start entry; a request for the filter without
// the mark is refused.
func TestDictFilterDefaultUnchanged(t *testing.T) {
	for _, c := range []struct {
		name string
		mark bool
		fn   func(*FTSConfig)
	}{{"default with mark", true, nil}, {"ids without mark", false, filterOn}} {
		u := filledUsers(t, idDocs(10), dictFilterStore(t, c.mark, c.fn))[0]

		checkGeneration(t, c.name, u, 0, ftsVersion)

		if l := layoutOf(t, u); l.AH != "" {
			t.Errorf("%s: text preparation %q", c.name, l.AH)
		}

		if c.mark {
			continue
		}

		_, err := u.ConvertFTSLayout(model.FTSLayoutRequest{DictFilter: model.FTSDictFilterIDs, Tok: "job-x/1"})
		if !errors.Is(err, model.ErrFTSDictFilterMark) {
			t.Errorf("%s: filter without the mark: %v", c.name, err)
		}
	}
}

// TestDictFilterFresh: with "ids" and the mark a fresh index has the
// filter in its record, the marker "3", start entries for the
// identifiers and "!dn" equal to the keys written; every search but an
// inner piece of an identifier answers as an unfiltered account's; an
// expunge takes the start entries out.
func TestDictFilterFresh(t *testing.T) {
	users := filledUsers(t, idDocs(20), dictFilterStore(t, true, filterOn), dictFilterStore(t, true, nil))
	u, ref := users[0], users[1]

	keys := checkGeneration(t, "fresh", u, 25, ftsVersionDict)

	if refKeys, _ := dictKeys(t, ref); len(keys) >= len(refKeys) {
		t.Errorf("filtered dictionary %d keys, unfiltered %d", len(keys), len(refKeys))
	}

	rest, inner := idQueries()

	sameSearches(t, "fresh, layout queries", searchState(t, ref), searchState(t, u))
	sameSearches(t, "fresh, identifiers", idState(t, ref, rest), idState(t, u, rest))
	checkInner(t, "fresh", u, ref, inner[:2])

	expungeFirst(t, u, -1)

	if keys, dn := dictKeys(t, u); len(keys) != 0 || dn != 0 {
		t.Errorf("after expunging everything: %d keys (%d start entries), !dn %d", len(keys), startEntries(keys), dn)
	}
}

// checkInner checks the inner pieces of identifiers: the unfiltered
// account finds the message, the filtered one narrows without it.
func checkInner(t *testing.T, stage string, filtered, plain *User, queries []string) {
	t.Helper()

	b := []string{fts.FieldBody}

	for _, q := range queries {
		want, err := plain.SearchCandidates(b, q)
		if err != nil || len(want) == 0 {
			t.Errorf("%s: unfiltered %q: %v %v", stage, q, want, err)
		}

		got, err := filtered.SearchCandidates(b, q)
		if err != nil || got == nil || len(got) >= len(want) {
			t.Errorf("%s: filtered %q: %v (unfiltered %v) %v", stage, q, got, want, err)
		}
	}
}

// TestDictFilterRoundTrip (temp/13 §10): unfiltered → filtered →
// unfiltered with fts_layout, deliveries and expunges in the filtered
// phase. Each direction derives the dictionary exactly from the
// postings: after the way back the account answers every search as one
// that was never filtered — also for the messages delivered while it
// was — and has exactly its dictionary keys. due_only does nothing while
// the filter is the one asked for.
func TestDictFilterRoundTrip(t *testing.T) {
	s := dictFilterStore(t, true, nil)
	users := filledUsers(t, idDocs(40), s, s)
	u, ref := users[0], users[1]
	rest, inner := idQueries()

	rep := convert(t, u, model.FTSLayoutRequest{DictFilter: model.FTSDictFilterIDs})
	if rep.To.DictIDMinRunes != 25 || rep.Noop {
		t.Fatalf("to filtered %+v", rep)
	}

	// "3": a version before 0.50.0 trusts only "2"
	keys := checkGeneration(t, "filtered", u, 25, ftsVersionDict)
	if int64(len(keys)) != rep.Dict {
		t.Errorf("filtered: %d keys, report %d", len(keys), rep.Dict)
	}

	if rep := convert(t, u, model.FTSLayoutRequest{DictFilter: model.FTSDictFilterIDs, DueOnly: true}); !rep.Noop {
		t.Errorf("due_only with the filter it has: %+v", rep)
	}

	sameSearches(t, "filtered", searchState(t, ref), searchState(t, u))
	sameSearches(t, "filtered, identifiers", idState(t, ref, rest), idState(t, u, rest))
	checkInner(t, "filtered", u, ref, inner[:2])

	filteredPhase(t, u, ref)
	checkInner(t, "filtered, after deliveries", u, ref, inner[2:])
	checkGeneration(t, "filtered, after deliveries", u, 25, ftsVersionDict)

	if rep := convert(t, u, model.FTSLayoutRequest{DictFilter: model.FTSDictFilterNone}); rep.To.DictIDMinRunes != 0 || rep.Noop {
		t.Fatalf("back %+v", rep)
	}

	checkGeneration(t, "back", u, 0, ftsVersion)

	all := append(append([]string{}, rest...), inner...)

	sameSearches(t, "back, layout queries", searchState(t, ref), searchState(t, u))
	sameSearches(t, "back, identifiers", idState(t, ref, all), idState(t, u, all))

	got, _ := dictKeys(t, u)
	if want, _ := dictKeys(t, ref); !maps.Equal(got, want) {
		t.Errorf("dictionary after the way back: %d keys, never filtered %d", len(got), len(want))
	}
}

// filteredPhase delivers five documents to both accounts and expunges
// their first three messages.
func filteredPhase(t *testing.T, users ...*User) {
	t.Helper()

	more := idDocs(45)

	for i := 40; i < 45; i++ {
		for _, x := range users {
			if _, err := x.DeliverGUID("INBOX", docGUID(i), []byte(more[fmt.Sprintf("g%03d", i)]), nil, time.Time{}); err != nil {
				t.Fatal(err)
			}
		}
	}

	for _, x := range users {
		expungeFirst(t, x, 3)
	}
}

// TestDictFilterDueOnly (temp/13 §4.8): due_only is due when the
// filter differs from the configuration; the plan estimates the
// dictionary objects of the target from a sample.
func TestDictFilterDueOnly(t *testing.T) {
	s := dictFilterStore(t, true, nil)
	u := filledUsers(t, idDocs(20), s)[0]

	if _, due, err := u.FTSLayoutDue(); err != nil || due {
		t.Fatalf("unfiltered with none: due %v %v", due, err)
	}

	u.ftsCfg.dictFilter = true

	target, due, err := u.FTSLayoutDue()
	if err != nil || !due || target.DictIDMinRunes != 25 {
		t.Fatalf("unfiltered with ids: %+v %v %v", target, due, err)
	}

	if rep := convert(t, u, model.FTSLayoutRequest{DueOnly: true}); rep.Noop || rep.To.DictIDMinRunes != 25 {
		t.Fatalf("due_only to the filter %+v", rep)
	}

	if rep := convert(t, u, model.FTSLayoutRequest{DueOnly: true}); !rep.Noop {
		t.Errorf("due_only again %+v", rep)
	}

	// the cluster mark gone: "" asks for no filter
	setDictFilterMark(t, s, false)

	if rep := convert(t, u, model.FTSLayoutRequest{DueOnly: true}); rep.Noop || rep.To.DictIDMinRunes != 0 {
		t.Errorf("due_only without the mark %+v", rep)
	}
}

// TestDictFilterFactor (temp/13 §4.8, §10): the sample estimate of the
// dictionary keys of another filter is within 10 % of the exact count
// after the copy, on the bench corpus with identifiers.
func TestDictFilterFactor(t *testing.T) {
	sizes, err := bench.ParseSizes("100:1KiB-8KiB")
	if err != nil {
		t.Fatal(err)
	}

	g := bench.Generator{Seed: 13, Sizes: sizes, Corpus: bench.Corpus{IDs: true}}
	docs := map[string]string{}

	for i := range 300 {
		raw, _ := g.Message("factor", i)
		docs[fmt.Sprintf("g%03d", i)] = string(raw)
	}

	u := filledUsers(t, docs, dictFilterStore(t, true, func(c *FTSConfig) { c.ShardsMin = 8 }))[0]

	cur := curLoc(layoutOf(t, u), u.ftsShards)
	_, before := dictKeys(t, u)

	f, err := u.dictFactor(cur, 25)
	if err != nil {
		t.Fatal(err)
	}

	rep := convert(t, u, model.FTSLayoutRequest{DictFilter: model.FTSDictFilterIDs, Shards: cur.s})
	est := float64(before) * f

	if d := est/float64(rep.Dict) - 1; d > 0.1 || d < -0.1 {
		t.Errorf("estimate %.0f (factor %.3f of %d), exact %d", est, f, before, rep.Dict)
	}
}

// TestDictFilterMixedWriters (temp/13 §4.4, §5.2): a writer of a version
// before 0.50.0 in a filtered generation writes trigram keys (a
// superset: the inner piece is found), a removal of such a version
// leaves the start entry (an empty lookup); the next fts_layout derives
// the dictionary anew and clears both.
func TestDictFilterMixedWriters(t *testing.T) {
	u := filledUsers(t, idDocs(5), dictFilterStore(t, true, filterOn))[0]
	b := []string{fts.FieldBody}

	old := layoutOf(t, u)
	old.DX = nil // what a version before 0.50.0 sees

	late := docGUID(77)
	if err := staleIndex(t, u, old, late, []byte(fmt.Sprintf("Subject: old\r\n\r\nolder %s\r\n", idOf(77)))); err != nil {
		t.Fatal(err)
	}

	if hits, err := u.SearchCandidates(b, idOf(77)[5:17]); err != nil || !hits[late] {
		t.Errorf("inner piece after an old writer: %v %v", hits, err)
	}

	// an old removal of a filtered message leaves its start entry
	gone := docGUID(1)
	entry := startEntry(fts.Tri0(idOf(1)), "b:"+idOf(1))

	if err := staleRemove(t, u, old, gone, []byte(idDocs(5)["g001"])); err != nil {
		t.Fatal(err)
	}

	if keys, _ := dictKeys(t, u); !keys[entry] {
		t.Fatal("the old removal took the start entry")
	}

	if hits, err := u.SearchCandidates(b, idOf(1)); err != nil || hits == nil || hits[gone] {
		t.Errorf("start entry without postings: %v %v", hits, err)
	}

	convert(t, u, model.FTSLayoutRequest{Shards: 2})

	keys := checkGeneration(t, "after fts_layout", u, 25, ftsVersionDict)
	if keys[entry] || trigramKeysOf(keys, "b:"+idOf(77)) > 0 {
		t.Errorf("after fts_layout: stale start entry %v, %d trigram keys of an identifier", keys[entry], trigramKeysOf(keys, "b:"+idOf(77)))
	}
}

// trigramKeysOf counts the trigram keys of term among dictionary keys.
func trigramKeysOf(keys map[string]bool, term string) int {
	n := 0

	for k := range keys {
		if strings.HasSuffix(k, dictSep+term) && !isStartEntry(k) {
			n++
		}
	}

	return n
}

// TestDictFilterCrashResume: a conversion to the filter that stops after
// P2, P5 or P6 is resumed by its token with the target it recorded
// ("next" carries the filter), whatever the resumed request says; a
// writer that meets the switch in between indexes into the target by
// its filter (temp/13 §10: crash hooks, generation change mid-indexing
// with a change of dx).
func TestDictFilterCrashResume(t *testing.T) {
	for _, phase := range []string{phaseP2, phaseP5, phaseP6} {
		t.Run(phase, func(t *testing.T) { crashResume(t, phase) })
	}
}

func crashResume(t *testing.T, phase string) {
	u := filledUsers(t, idDocs(10), dictFilterStore(t, true, nil))[0]
	old := layoutOf(t, u)
	tok := "job-crashdx/1"

	crashAt(t, u.name, phase)

	if _, err := u.ConvertFTSLayout(model.FTSLayoutRequest{DictFilter: model.FTSDictFilterIDs, Shards: 2, Tok: tok}); err == nil {
		t.Fatal("the crash hook did not stop the run")
	}

	ftsLayoutHook = nil

	if phase == phaseP6 {
		lateWriterFollows(t, u, old)
	} else if l := layoutOf(t, u); l.Next == nil || dxRunes(l.Next.DX) != 25 {
		t.Fatalf("next after %s: %+v", phase, l.Next)
	}

	if rep := convert(t, u, model.FTSLayoutRequest{DictFilter: model.FTSDictFilterNone, Shards: 2, Tok: tok}); !rep.Resumed {
		t.Errorf("not resumed: %+v", rep)
	}

	if l := layoutOf(t, u); len(l.Old) != 0 {
		t.Errorf("sources left %+v", l.Old)
	}

	checkGeneration(t, "resumed", u, 25, ftsVersionDict)
}

// lateWriterFollows: after the switch a writer of the old record follows
// into the filtered target and writes its start entry there.
func lateWriterFollows(t *testing.T, u *User, old *ftsLayout) {
	t.Helper()

	if err := staleIndex(t, u, old, docGUID(77), []byte("Subject: late\r\n\r\nlate "+idOf(77)+"\r\n")); err != nil {
		t.Fatal(err)
	}

	now := curLoc(layoutOf(t, u), u.ftsShards)
	entry := startEntry(fts.Tri0(idOf(77)), "b:"+idOf(77))

	if keys, _ := dictKeys(t, u); now.dx != 25 || !keys[entry] {
		t.Errorf("writer after the switch: dx %d, start entry %v", now.dx, keys[entry])
	}
}

// nosigMessage is a forward with signature and trace fields in the
// attached header.
func nosigMessage(i int) []byte {
	return []byte(fmt.Sprintf("From: a@example.org\r\nSubject: fwd %d\r\nMIME-Version: 1.0\r\nContent-Type: multipart/mixed; boundary=b1\r\n\r\n"+
		"--b1\r\nContent-Type: text/plain\r\n\r\nsiehe anhang\r\n--b1\r\nContent-Type: message/rfc822\r\n\r\n"+
		"DKIM-Signature: v=1; b=sigtok%dq7w\r\nReceived: from x by y id rcvtok%dk\r\nSubject: innen\r\n\r\ntext innen\r\n--b1--\r\n", i, i, i))
}

// checkNoSig checks an index of the text preparation "nosig" with the
// marker want in v (layout 2) or V: a "nosig" server trusts it and the
// signatures are not in it; a server with "all" does not trust it (IMAP
// narrows nothing: slow, but right).
func checkNoSig(t *testing.T, stage string, u *User, layout int, want string) {
	t.Helper()

	b := []string{fts.FieldBody}

	if v, v3 := versionMarkers(t, u); (layout == 3 && v3 != want) || (layout == 2 && v != want) {
		t.Errorf("%s: v=%q V=%q, want %q", stage, v, v3, want)
	}

	u.ftsForget()

	if hits, err := u.SearchCandidates(b, "innen"); err != nil || len(hits) != 5 {
		t.Errorf("%s: nosig reader %v %v", stage, hits, err)
	}

	if hits, _ := u.SearchCandidates(b, "sigtok1q7w"); hits == nil || len(hits) != 0 {
		t.Errorf("%s: the signature is indexed: %v", stage, hits)
	}

	u.ftsCfg.ah = ""
	u.ftsForget()

	if hits, err := u.SearchCandidates(b, "innen"); err != nil || hits != nil {
		t.Errorf("%s: an all reader trusts a nosig index: %v %v", stage, hits, err)
	}

	u.ftsCfg.ah = fts.AttachedHeadersNoSig
	u.ftsForget()
}

// TestNoSigGenerations (c3): with fts.attached_headers nosig and the
// mark a fresh index, a rebuild and a conversion keep the text
// preparation in the record and set the marker "4"; fts_layout keeps the
// preparation (it copies the postings), 3→2 leaves "4", 2→3 takes
// "nosig" back from it; a rebuild with "all" makes the index exact for
// "all" again.
func TestNoSigGenerations(t *testing.T) {
	s := dictFilterStore(t, true, func(c *FTSConfig) { c.AttachedHeaders = fts.AttachedHeadersNoSig })
	u := nosigUser(t, s, 5)

	if l := layoutOf(t, u); l == nil || l.AH != fts.AttachedHeadersNoSig || l.DX != nil {
		t.Fatalf("fresh record %+v", l)
	}

	checkNoSig(t, "fresh", u, 3, ftsVersionText)
	rebuild(t, u)
	checkNoSig(t, "rebuild", u, 3, ftsVersionText)

	if rep := convert(t, u, model.FTSLayoutRequest{Shards: 2}); rep.To.AttachedHeaders != fts.AttachedHeadersNoSig {
		t.Errorf("3→3 %+v", rep)
	}

	checkNoSig(t, "3→3", u, 3, ftsVersionText)
	convert(t, u, model.FTSLayoutRequest{Layout: 2})
	checkNoSig(t, "3→2", u, 2, ftsVersionText)

	if rep := convert(t, u, model.FTSLayoutRequest{Layout: 3}); rep.To.AttachedHeaders != fts.AttachedHeadersNoSig {
		t.Errorf("2→3 %+v", rep)
	}

	checkNoSig(t, "2→3", u, 3, ftsVersionText)

	// back to all: a rebuild reads the messages again
	u.ftsCfg.ah = ""
	rebuild(t, u)

	if l := layoutOf(t, u); l == nil || l.AH != "" {
		t.Fatalf("record after the rebuild with all %+v", l)
	}

	if _, V := versionMarkers(t, u); V != ftsVersion {
		t.Errorf("marker after the rebuild with all %q", V)
	}

	if hits, err := u.SearchCandidates([]string{fts.FieldBody}, "sigtok1q7w"); err != nil || !hits[docGUID(1)] {
		t.Errorf("all: the signature is not found: %v %v", hits, err)
	}
}

// nosigUser is a new account of s with n forwards (nosigMessage).
func nosigUser(t *testing.T, s *Store, n int) *User {
	t.Helper()

	u := testUser(t, s)
	mustMailbox(t, u, "INBOX")

	for i := range n {
		if _, err := u.DeliverGUID("INBOX", docGUID(i), nosigMessage(i), nil, time.Time{}); err != nil {
			t.Fatal(err)
		}
	}

	return u
}

// rebuild runs fts_rebuild on the account.
func rebuild(t *testing.T, u *User) {
	t.Helper()

	if err := u.RebuildFTS(func() error { return nil }, func(int, int) {}); err != nil {
		t.Fatal(err)
	}
}

// TestNoSigNeedsMark: without the mark "nosig" gives new generations
// the default preparation, which a "nosig" server trusts (its terms are
// a superset).
func TestNoSigNeedsMark(t *testing.T) {
	u := nosigUser(t, dictFilterStore(t, false, func(c *FTSConfig) { c.AttachedHeaders = fts.AttachedHeadersNoSig }), 2)

	if l := layoutOf(t, u); l == nil || l.AH != "" {
		t.Fatalf("record %+v", l)
	}

	if hits, err := u.SearchCandidates([]string{fts.FieldBody}, "sigtok1q7w"); err != nil || !hits[docGUID(1)] {
		t.Errorf("superset index: %v %v", hits, err)
	}
}

// TestVersionDriftScrub: a layout 2 marker "3" (a version before 0.50.0
// converted a filtered generation back) is reported and set to "2"; a
// dry run only reports.
func TestVersionDriftScrub(t *testing.T) {
	u := filledUsers(t, layoutDocs(3), testStore(t))[0]

	if err := u.idx.SetOmap(oidFTSMap, map[string][]byte{ftsVersionKey: []byte(ftsVersionDict)}); err != nil {
		t.Fatal(err)
	}

	rep, err := u.Scrub(ScrubOptions{DryRun: true})
	if err != nil || rep.FTSVersionDrift != 1 {
		t.Fatalf("dry run %+v %v", rep, err)
	}

	if v, _ := versionMarkers(t, u); v != ftsVersionDict {
		t.Fatalf("the dry run changed the marker to %q", v)
	}

	rep, err = u.Scrub(ScrubOptions{})
	if err != nil || rep.FTSVersionDrift != 1 || rep.Findings() < 1 {
		t.Fatalf("scrub %+v %v", rep, err)
	}

	if v, _ := versionMarkers(t, u); v != ftsVersion {
		t.Errorf("marker after the scrub %q", v)
	}

	if rep, err := u.Scrub(ScrubOptions{}); err != nil || rep.FTSVersionDrift != 0 {
		t.Errorf("second scrub %+v %v", rep, err)
	}
}
