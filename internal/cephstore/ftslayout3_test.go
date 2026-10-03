//go:build tentacle

package cephstore

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	"shardpost/internal/fts"
	"shardpost/internal/model"
)

// Cluster tests of layout 3 (stage B14b).

// setLayoutMark sets or removes the cluster mark.
func setLayoutMark(t *testing.T, s *Store, on bool) {
	t.Helper()
	setMark(t, s, model.FTSLayoutMarkKey, on)
}

// setMark sets or removes a cluster mark of the admin store.
func setMark(t *testing.T, s *Store, key string, on bool) {
	t.Helper()

	kv, err := s.AdminKV()
	if err != nil {
		t.Fatal(err)
	}
	defer kv.Close()

	err = kv.Update(key, func([]byte) ([]byte, error) {
		if !on {
			return nil, nil
		}

		return json.Marshal(model.FTSLayoutMark{EnabledAt: time.Now(), Pods: 1})
	})
	if err != nil {
		t.Fatal(err)
	}
}

// layout3Config is the layout 3 configuration of the tests; fn changes
// it.
func layout3Config(fn func(*FTSConfig)) func(*Config) {
	return func(c *Config) {
		c.FTS = FTSConfig{Layout: model.FTSLayout3, ShardsMin: 1, ShardsMax: 32, ShardTargetKeys: 100000, FrozenWait: time.Second}
		if fn != nil {
			fn(&c.FTS)
		}
	}
}

// layout3Store opens a store with fts.layout 3 and sets the cluster mark
// for the test.
func layout3Store(t *testing.T, fn func(*FTSConfig)) *Store {
	t.Helper()

	s := storeWith(t, layout3Config(fn))
	setLayoutMark(t, s, true)
	t.Cleanup(func() { setLayoutMark(t, s, false) })

	return s
}

// layoutOf reads the layout record of the account.
func layoutOf(t *testing.T, u *User) *ftsLayout {
	t.Helper()

	l, err := u.readLayout()
	if err != nil {
		t.Fatal(err)
	}

	return l
}

// docGUID is the GUID of test document i (the same in every account, so
// that results compare across accounts).
func docGUID(i int) string { return fmt.Sprintf("%032x", 0xabc000+i) }

// deliverDocs delivers the layout documents into INBOX under fixed
// GUIDs.
func deliverDocs(t *testing.T, u *User, docs map[string]string) {
	t.Helper()

	for _, k := range slices.Sorted(maps.Keys(docs)) {
		var i int

		_, _ = fmt.Sscanf(k, "g%d", &i)

		if _, err := u.DeliverGUID("INBOX", docGUID(i), []byte(docs[k]), nil, time.Time{}); err != nil {
			t.Fatal(err)
		}
	}
}

// searchState is what the searches of an account answer: IMAP SEARCH
// candidates (substring), word search (JMAP text filters, IMAP word
// mode) and the stored header texts.
func searchState(t *testing.T, u *User) map[string]string {
	t.Helper()

	out := map[string]string{}

	for _, q := range layoutQueries {
		hits, err := u.SearchCandidates(q.fields, q.query)
		if err != nil {
			t.Fatalf("candidates %v %q: %v", q.fields, q.query, err)
		}

		out[fmt.Sprintf("imap %v %q", q.fields, q.query)] = fmt.Sprint(hits == nil, slices.Sorted(maps.Keys(hits)))

		words, err := u.SearchFields(q.fields, q.query)
		if err != nil {
			t.Fatalf("words %v %q: %v", q.fields, q.query, err)
		}

		out[fmt.Sprintf("jmap %v %q", q.fields, q.query)] = fmt.Sprint(slices.Sorted(maps.Keys(words)))
	}

	var guids []string
	for i := range 80 {
		guids = append(guids, docGUID(i))
	}

	texts, err := u.HeaderTexts(guids)
	if err != nil {
		t.Fatal(err)
	}

	for g, text := range texts {
		out["header "+g] = text
	}

	return out
}

// sameSearches fails the test when two accounts answer differently.
func sameSearches(t *testing.T, stage string, want, got map[string]string) {
	t.Helper()

	if len(want) == 0 {
		t.Fatalf("%s: nothing to compare", stage)
	}

	for k, w := range want {
		if got[k] != w {
			t.Errorf("%s: %s: %s, want %s", stage, k, got[k], w)
		}
	}

	for k := range got {
		if _, ok := want[k]; !ok {
			t.Errorf("%s: %s only in one account", stage, k)
		}
	}
}

// mustExist fails the test unless every object exists (or none, with
// exist false).
func mustExist(t *testing.T, u *User, exist bool, oids ...string) {
	t.Helper()

	for _, oid := range oids {
		_, err := u.idx.Stat(oid)
		if (err == nil) != exist {
			t.Errorf("%s: exists %v, want %v (%v)", oid, err == nil, exist, err)
		}
	}
}

// inPresenceIndex reports whether the account is in ix.fts_layout.
func inPresenceIndex(t *testing.T, s *Store, account string) bool {
	t.Helper()

	ix, release, err := s.FTSLayoutIndex()
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	_, err = ix.Get(account)
	if errors.Is(err, model.ErrNotFound) {
		return false
	}

	if err != nil {
		t.Fatal(err)
	}

	return true
}

// TestLayout3FreshIndex: with fts.layout 3 and the mark a fresh index is
// one generation of S = D = H = 1 objects with their markers, the
// account is in the presence index, the layout 3 version marker is set
// and the document count kept; searches answer as layout 2 does (IMAP
// substring candidates, word search, header texts), also after an
// expunge.
func TestLayout3FreshIndex(t *testing.T) {
	s2 := testStore(t)
	s3 := layout3Store(t, nil)
	docs := layoutDocs(30)

	u2, u3 := testUser(t, s2), testUser(t, s3)

	for _, u := range []*User{u2, u3} {
		mustMailbox(t, u, "INBOX")
		deliverDocs(t, u, docs)
	}

	if l := layoutOf(t, u2); l != nil {
		t.Fatalf("layout 2 account has a record %+v", l)
	}

	checkFreshLayout3(t, u3)

	if !inPresenceIndex(t, s3, u3.name) || inPresenceIndex(t, s2, u2.name) {
		t.Error("presence index")
	}

	sameSearches(t, "fresh", searchState(t, u2), searchState(t, u3))

	for _, u := range []*User{u2, u3} {
		metas, err := u.ListMessagesByName("INBOX")
		if err != nil {
			t.Fatal(err)
		}

		if err := u.Expunge("INBOX", metas[:5]); err != nil {
			t.Fatal(err)
		}
	}

	sameSearches(t, "after expunge", searchState(t, u2), searchState(t, u3))

	if c := omapKeys(t, u3, oidFTSMap, ftsCountKey)[ftsCountKey]; string(c) != "25" {
		t.Errorf("document count %q after expunging 5 of 30", c)
	}
}

// checkFreshLayout3 checks the record, objects and markers of a fresh
// layout 3 index of 30 documents.
func checkFreshLayout3(t *testing.T, u *User) {
	t.Helper()

	l := layoutOf(t, u)
	if l == nil || shapeOf(l) != "3/1/1/1 lg" || len(l.G) != 16 {
		t.Fatalf("record %+v", l)
	}

	loc := curLoc(l, u.ftsShards)
	mustExist(t, u, true, loc.objects()...)
	mustExist(t, u, false, layout2Loc(u.ftsShards).postingOIDs()...)

	p := omapKeys(t, u, loc.postingOIDn(0), "!")
	if string(p[markGen]) != l.G || string(p[markState]) != stateLive || atoiMarker(p[markTerms])*atoiMarker(p[markDict]) == 0 {
		t.Errorf("posting markers %q", p)
	}

	checkMapMarkers(t, u, 3, "30")
}

// shapeOf is a record's layout, counts and lg flag as a string.
func shapeOf(l *ftsLayout) string {
	s := fmt.Sprintf("%d/%d/%d/%d", l.F, l.S, l.D, l.H)
	if l.LG {
		s += " lg"
	}

	return s
}

// checkMapMarkers checks the version markers and the document count
// ("" for none) of the id map of a layout.
func checkMapMarkers(t *testing.T, u *User, layout int, count string) {
	t.Helper()

	m := omapKeys(t, u, oidFTSMap, "")
	have, gone := ftsVersionKey3, ftsVersionKey

	if layout == 2 {
		have, gone = gone, have
	}

	if string(m[have]) != ftsVersion || m[gone] != nil || string(m[ftsCountKey]) != count {
		t.Errorf("id map markers of layout %d: %s=%q %s=%q c=%q", layout, have, m[have], gone, m[gone], m[ftsCountKey])
	}
}

// TestLayout3NeedsMark: without the cluster mark fts.layout 3 creates
// layout 2 indexes (the mark is read when an index is created).
func TestLayout3NeedsMark(t *testing.T) {
	s := storeWith(t, layout3Config(nil))
	setLayoutMark(t, s, false)

	u := testUser(t, s)
	mustMailbox(t, u, "INBOX")

	if _, err := u.Deliver("INBOX", testMessage("nomark", 300), nil, time.Time{}); err != nil {
		t.Fatal(err)
	}

	if l := layoutOf(t, u); l != nil {
		t.Errorf("layout record without the mark: %+v", l)
	}

	if inPresenceIndex(t, s, u.name) {
		t.Error("account in the presence index")
	}
}

// convert runs a conversion as the job does (fenced, own token).
func convert(t *testing.T, u *User, req model.FTSLayoutRequest) model.FTSLayoutReport {
	t.Helper()

	if req.Tok == "" {
		req.Tok = "job-" + newGUID()[:12] + "/1"
	}

	rep, err := u.ConvertFTSLayout(req)
	if err != nil {
		t.Fatalf("convert %+v: %v", req, err)
	}

	return rep
}

// TestLayoutConvert: 2→3, reshard 1→4→2, 3→2 keep every search answer;
// the sources are gone after each run, the record and the presence index
// follow, the version marker moves with the layout.
func TestLayoutConvert(t *testing.T) {
	s2 := testStore(t)
	s3 := layout3Store(t, nil)
	docs := layoutDocs(40)

	ref := testUser(t, s2)
	mustMailbox(t, ref, "INBOX")
	deliverDocs(t, ref, docs)

	name := testAccount(t)
	u2 := testUserNamed(t, s2, name)
	mustMailbox(t, u2, "INBOX")
	deliverDocs(t, u2, docs)

	u := testUserNamed(t, s3, name)
	want := searchState(t, ref)

	convertTo3(t, u, s3, want)

	for _, n := range []int{4, 2} {
		reshardTo(t, u, n, want)
	}

	// delivery in the resharded index
	for _, x := range []*User{u, ref} {
		if _, err := x.DeliverGUID("INBOX", docGUID(70), []byte(docs["g001"]), nil, time.Time{}); err != nil {
			t.Fatal(err)
		}
	}

	want = searchState(t, ref)
	sameSearches(t, "delivery after reshard", want, searchState(t, u))

	convertTo2(t, u, s3, want)
}

// convertTo3 converts a layout 2 account of 40 documents and checks the
// result.
func convertTo3(t *testing.T, u *User, s *Store, want map[string]string) {
	t.Helper()

	rep := convert(t, u, model.FTSLayoutRequest{Layout: 3})
	if rep.From.Layout != 2 || rep.To.Layout != 3 || rep.Docs != 40 || rep.Terms == 0 || rep.Headers != 40 {
		t.Fatalf("2→3 %+v", rep)
	}

	if l := layoutOf(t, u); l == nil || l.F != 3 || !l.LG || l.Next != nil || len(l.Old) != 0 {
		t.Fatalf("after 2→3 %+v", l)
	}

	mustExist(t, u, false, layout2Loc(u.ftsShards).objects()...)
	sameSearches(t, "2→3", want, searchState(t, u))

	if !inPresenceIndex(t, s, u.name) {
		t.Error("not in the presence index after 2→3")
	}

	checkMapMarkers(t, u, 3, "40")
}

// reshardTo reshards to n objects of each kind and checks the result.
func reshardTo(t *testing.T, u *User, n int, want map[string]string) {
	t.Helper()

	old := curLoc(layoutOf(t, u), u.ftsShards)

	rep := convert(t, u, model.FTSLayoutRequest{Layout: 3, Shards: n})
	if rep.To.S != n || rep.To.D != n || rep.To.H != n {
		t.Fatalf("reshard to %d: %+v", n, rep)
	}

	mustExist(t, u, false, old.objects()...)
	mustExist(t, u, true, curLoc(layoutOf(t, u), u.ftsShards).objects()...)
	sameSearches(t, fmt.Sprintf("reshard %d", n), want, searchState(t, u))
}

// convertTo2 converts back to layout 2 and checks the result.
func convertTo2(t *testing.T, u *User, s *Store, want map[string]string) {
	t.Helper()

	last := curLoc(layoutOf(t, u), u.ftsShards)

	if rep := convert(t, u, model.FTSLayoutRequest{Layout: 2}); rep.To.Layout != 2 {
		t.Fatalf("3→2 %+v", rep)
	}

	if l := layoutOf(t, u); l != nil {
		t.Fatalf("record after 3→2 %+v", l)
	}

	mustExist(t, u, false, last.objects()...)
	checkMapMarkers(t, u, 2, "")

	if inPresenceIndex(t, s, u.name) {
		t.Error("still in the presence index after 3→2")
	}

	sameSearches(t, "3→2", want, searchState(t, u))

	if rep := convert(t, u, model.FTSLayoutRequest{Layout: 2}); !rep.Noop {
		t.Errorf("layout 2 to layout 2 did something: %+v", rep)
	}
}

// TestLayoutConvertNeedsMark: 2→3 is refused without the mark.
func TestLayoutConvertNeedsMark(t *testing.T) {
	s := storeWith(t, layout3Config(nil))
	setLayoutMark(t, s, false)

	u := testUser(t, s)
	mustMailbox(t, u, "INBOX")

	if _, err := u.ConvertFTSLayout(model.FTSLayoutRequest{Layout: 3, Tok: "job-x/1"}); !errors.Is(err, model.ErrFTSLayoutMark) {
		t.Fatalf("2→3 without the mark: %v", err)
	}
}

// TestLayoutDueOnly: due_only reads the counters and ends without a
// change while they ask for the shape the index has; a forced shape is
// a change.
func TestLayoutDueOnly(t *testing.T) {
	s := layout3Store(t, nil)
	u := testUser(t, s)
	mustMailbox(t, u, "INBOX")
	deliverDocs(t, u, layoutDocs(10))

	target, due, err := u.FTSLayoutDue()
	if err != nil || due || target.S != 1 {
		t.Fatalf("due %+v %v %v", target, due, err)
	}

	if rep := convert(t, u, model.FTSLayoutRequest{DueOnly: true}); !rep.Noop {
		t.Errorf("due_only without need: %+v", rep)
	}

	// A tiny target makes the counters ask for more objects.
	u.ftsCfg.sizing.target, u.ftsCfg.sizing.dictTarget = 100, 100

	target, due, err = u.FTSLayoutDue()
	if err != nil || !due || target.D < 2 {
		t.Fatalf("due with a small target %+v %v %v", target, due, err)
	}

	rep := convert(t, u, model.FTSLayoutRequest{DueOnly: true})
	if rep.Noop || rep.To.S != target.S || rep.To.D != target.D {
		t.Errorf("due_only reshard %+v, want %+v", rep, target)
	}
}

// staleWriter is an indexing that read the layout record l before a
// conversion and writes after it (§4.3 rule 1).
func staleIndex(t *testing.T, u *User, l *ftsLayout, guid string, raw []byte) error {
	t.Helper()

	ids, _, err := u.ensureDocIDs([]string{guid})
	if err != nil {
		t.Fatal(err)
	}

	w := &ftsWrite{wait: u.ftsCfg.frozenWait}
	w.follow(l, u.ftsShards)

	return u.indexWith(w, &ftsBatch{docs: []ftsDoc{{guid: guid, raw: raw}}, ids: ids})
}

// staleRemove is a removal that read the record l before a conversion.
func staleRemove(t *testing.T, u *User, l *ftsLayout, guid string, raw []byte) error {
	t.Helper()

	id, hasID, _, err := u.docIDOf(guid)
	if err != nil {
		t.Fatal(err)
	}

	w := &ftsWrite{wait: u.ftsCfg.frozenWait}
	w.follow(l, u.ftsShards)

	err = u.removeWith(w, guid, id, hasID, rawTerms(raw))
	if err == nil && hasID {
		err = u.dropDocID(guid, id)
	}

	return err
}

// crashAt makes conversions of the account stop after phase as a crash
// would (no clean-up) until the test ends.
func crashAt(t *testing.T, account, phase string) {
	t.Helper()

	ftsLayoutHook = func(a, p string) error {
		if a == account && p == phase {
			return fmt.Errorf("crash after %s", p)
		}

		return nil
	}

	t.Cleanup(func() { ftsLayoutHook = nil })
}

// hasTerm reports whether a posting object of loc holds the term.
func hasTerm(t *testing.T, u *User, loc ftsLoc, term string) bool {
	t.Helper()

	_, ok := omapKeys(t, u, loc.postingOID(term), term)[term]

	return ok
}

// TestLayoutCrashAfterP6StaleWriter (N1): a conversion stops after P6;
// a writer and a removal that read the record before write nothing into
// the frozen source, everything into the new generation, for a layout 2
// and a layout 3 source; after P7 (the run continued) nothing is
// missing.
func TestLayoutCrashAfterP6StaleWriter(t *testing.T) {
	for _, from := range []int{2, 3} {
		t.Run(fmt.Sprintf("from%d", from), func(t *testing.T) { crashAfterP6(t, from) })
	}
}

func crashAfterP6(t *testing.T, from int) {
	conv := layout3Store(t, nil)

	s := conv
	if from == 2 {
		s = testStore(t) // layout 2 indexes
	}

	u := testUser(t, s)
	mustMailbox(t, u, "INBOX")

	docs := layoutDocs(10)
	deliverDocs(t, u, docs)

	old := layoutOf(t, u)
	src := curLoc(old, u.ftsShards)
	cu := testUserNamed(t, conv, u.name)
	tok := "job-crash/1"

	crashAt(t, u.name, phaseP6)

	if _, err := cu.ConvertFTSLayout(model.FTSLayoutRequest{Layout: 3, Shards: 2, Tok: tok}); err == nil {
		t.Fatal("the crash hook did not stop the run")
	}

	ftsLayoutHook = nil
	now := curLoc(layoutOf(t, u), u.ftsShards)
	guid := docGUID(77)

	if err := staleIndex(t, u, old, guid, []byte("Subject: stalewriter\r\n\r\nzzqqstale body\r\n")); err != nil {
		t.Fatalf("stale writer: %v", err)
	}

	if in, out := hasTerm(t, u, src, "s:stalewriter"), hasTerm(t, u, now, "s:stalewriter"); in || !out {
		t.Errorf("stale writer: in source %v, in target %v", in, out)
	}

	if err := staleRemove(t, u, old, docGUID(1), []byte(docs["g001"])); err != nil {
		t.Fatalf("stale removal: %v", err)
	}

	if hasTerm(t, u, now, "b:unique1word") {
		t.Error("the stale removal left the target")
	}

	// the run continues with its token: P7
	if rep := convert(t, cu, model.FTSLayoutRequest{Layout: 3, Shards: 2, Tok: tok}); !rep.Resumed {
		t.Errorf("not resumed: %+v", rep)
	}

	checkAfterP7(t, u, src, guid)
}

// checkAfterP7 checks a finished conversion: the sources are gone, the
// record names neither a conversion nor a source, guid is found.
func checkAfterP7(t *testing.T, u *User, src ftsLoc, guid string) {
	t.Helper()

	mustExist(t, u, false, src.postingOIDs()...)
	u.ftsForget()

	if hits, err := u.SearchFields([]string{fts.FieldSubject}, "stalewriter"); err != nil || !hits[guid] {
		t.Errorf("after P7: %v %v", hits, err)
	}

	if l := layoutOf(t, u); l.Next != nil || len(l.Old) != 0 {
		t.Errorf("record after P7 %+v", l)
	}
}

// TestLayoutStaleAfterP7: a writer and a reader with the record of a
// generation a finished conversion removed follow the record (ENOENT is
// no "empty" in layout 3).
func TestLayoutStaleAfterP7(t *testing.T) {
	s := layout3Store(t, nil)
	u := testUser(t, s)
	mustMailbox(t, u, "INBOX")
	deliverDocs(t, u, layoutDocs(5))

	old := layoutOf(t, u)

	// the view caches the record for its searches
	if _, err := u.SearchFields([]string{fts.FieldBody}, "lorem"); err != nil {
		t.Fatal(err)
	}

	cu := testUserNamed(t, s, u.name)
	convert(t, cu, model.FTSLayoutRequest{Layout: 3, Shards: 4})

	guid := docGUID(50)
	if err := staleIndex(t, u, old, guid, []byte("Subject: afterp7\r\n\r\nbody\r\n")); err != nil {
		t.Fatalf("stale writer: %v", err)
	}

	if !hasTerm(t, u, curLoc(layoutOf(t, u), u.ftsShards), "s:afterp7") {
		t.Error("the stale writer's terms are not in the current generation")
	}

	mustExist(t, u, false, curLoc(old, u.ftsShards).objects()...)

	hits, err := u.SearchFields([]string{fts.FieldSubject}, "afterp7")
	if err != nil || !hits[guid] {
		t.Errorf("stale reader: %v %v", hits, err)
	}
}

// TestLayoutAbortAfterP3 (B2): a run that fails after P3 and is cleared
// (the job's defer) leaves the index as it was: freezes lifted, targets
// deleted, deliveries go on; a run that crashed (no clear) keeps the
// freeze, a delivery answers a temporary failure after its wait, and
// the run continues with its token.
func TestLayoutAbortAfterP3(t *testing.T) {
	s := layout3Store(t, func(c *FTSConfig) { c.FrozenWait = 300 * time.Millisecond })
	u := testUser(t, s)
	mustMailbox(t, u, "INBOX")
	deliverDocs(t, u, layoutDocs(5))

	src := curLoc(layoutOf(t, u), u.ftsShards)

	crashAt(t, u.name, phaseP3)

	tok := "job-abort/1"
	if _, err := u.ConvertFTSLayout(model.FTSLayoutRequest{Layout: 3, Shards: 2, Tok: tok}); err == nil {
		t.Fatal("no crash")
	}

	ftsLayoutHook = nil
	next := layoutOf(t, u).Next

	scrubSeesConversion(t, u)

	// crashed: frozen, a delivery fails after its wait
	start := time.Now()

	if _, err := u.Deliver("INBOX", testMessage("frozen", 300), nil, time.Time{}); !errors.Is(err, errFTSFrozen) {
		t.Fatalf("delivery into a frozen index: %v", err)
	}

	if d := time.Since(start); d < 250*time.Millisecond {
		t.Errorf("no wait before the failure (%v)", d)
	}

	// cleared: as before
	ok, err := u.ClearFTSLayoutNext(tok)
	if err != nil || !ok {
		t.Fatalf("clear: %v %v", ok, err)
	}

	if l := layoutOf(t, u); l.Next != nil || !curLoc(l, u.ftsShards).same(src) {
		t.Fatalf("record after clear %+v", l)
	}

	mustExist(t, u, false, genLoc(*next, u.ftsShards).objects()...)

	for _, oid := range src.postingOIDs() {
		if st := omapKeys(t, u, oid, markState)[markState]; string(st) != stateLive {
			t.Errorf("%s still %q", oid, st)
		}
	}

	if _, err := u.Deliver("INBOX", testMessage("thawed", 300), nil, time.Time{}); err != nil {
		t.Fatalf("delivery after clear: %v", err)
	}

	if ok, _ := u.ClearFTSLayoutNext(tok); ok {
		t.Error("cleared twice")
	}
}

// scrubSeesConversion: the batch scrub must not skip an account with an
// unfinished conversion (N11).
func scrubSeesConversion(t *testing.T, u *User) {
	t.Helper()

	u.flushIntents(false) // the deliveries' blob intents go in the background

	if p, err := u.ScrubPending(); err != nil || p != "fts conversion" {
		t.Errorf("scrub pending %q %v", p, err)
	}
}

// TestLayoutStaleFreezeLifted: a freeze no conversion names (its run
// removed the record's "next" but crashed before lifting it) is lifted
// by the next writer in its own write.
func TestLayoutStaleFreezeLifted(t *testing.T) {
	s := layout3Store(t, nil)
	u := testUser(t, s)
	mustMailbox(t, u, "INBOX")
	deliverDocs(t, u, layoutDocs(3))

	loc := curLoc(layoutOf(t, u), u.ftsShards)

	if err := casOmap(u, u.idx, loc.postingOIDn(0), nil, func(map[string][]byte) (map[string][]byte, []string) {
		return map[string][]byte{markState: []byte(frozenPrefix + "job-dead/1")}, nil
	}); err != nil {
		t.Fatal(err)
	}

	if _, err := u.Deliver("INBOX", testMessage("lifted", 300), nil, time.Time{}); err != nil {
		t.Fatalf("delivery: %v", err)
	}

	if st := omapKeys(t, u, loc.postingOIDn(0), markState)[markState]; string(st) != stateLive {
		t.Errorf("state %q", st)
	}
}

// TestLayoutLateFreezeOfTakenOverRun (R5.1, R4.1): run A (job x, lease
// generation 1) hangs after P3; run B (same job, generation 2) treats
// A's "next" as foreign, freezes with its own token; A's late P3 write
// freezes a source object again with A's token. A writer that meets it
// while B's "next" stands waits instead of lifting it, fails after its
// wait, and after B's P6 writes into B's generation; A's late clear
// lifts nothing of B, and no write is lost.
func TestLayoutLateFreezeOfTakenOverRun(t *testing.T) {
	s := layout3Store(t, func(c *FTSConfig) { c.FrozenWait = 200 * time.Millisecond })
	u := testUser(t, s)
	mustMailbox(t, u, "INBOX")
	deliverDocs(t, u, layoutDocs(5))

	src := curLoc(layoutOf(t, u), u.ftsShards)
	tokA, tokB := model.JobLockToken("job-x", 1), model.JobLockToken("job-x", 2)

	crashAt(t, u.name, phaseP3)

	if _, err := u.ConvertFTSLayout(model.FTSLayoutRequest{Layout: 3, Shards: 2, Tok: tokA}); err == nil {
		t.Fatal("no crash")
	}

	targetA := genLoc(*layoutOf(t, u).Next, u.ftsShards)

	// B runs until after its P5; A's late P3 write happens in between.
	ftsLayoutHook = func(a, p string) error {
		if a != u.name || p != phaseP5 {
			return nil
		}

		return lateFreezeOfA(t, u, src, tokA)
	}

	rep, err := u.ConvertFTSLayout(model.FTSLayoutRequest{Layout: 3, Shards: 2, Tok: tokB})
	if err != nil {
		t.Fatalf("run B: %v", err)
	}

	if rep.Cleared != tokA {
		t.Errorf("B cleared %q, want A's run", rep.Cleared)
	}

	mustExist(t, u, false, targetA.objects()...)

	guid := docGUID(60)
	if _, err := u.DeliverGUID("INBOX", guid, []byte("Subject: afterb\r\n\r\nx\r\n"), nil, time.Time{}); err != nil {
		t.Fatalf("delivery after B: %v", err)
	}

	if hits, err := u.SearchFields([]string{fts.FieldSubject}, "afterb"); err != nil || !hits[guid] {
		t.Errorf("search after B: %v %v", hits, err)
	}

	allFound(t, u, 5)
}

// lateFreezeOfA is A's late P3 write during B's run, a delivery that
// meets it, and A's late clear.
func lateFreezeOfA(t *testing.T, u *User, src ftsLoc, tokA string) error {
	err := casOmap(u, u.idx, src.postingOIDn(0), nil, func(map[string][]byte) (map[string][]byte, []string) {
		return map[string][]byte{markState: []byte(frozenPrefix + tokA)}, nil
	})
	if err != nil {
		return err
	}

	if _, derr := u.Deliver("INBOX", testMessage("duringb", 300), nil, time.Time{}); !errors.Is(derr, errFTSFrozen) {
		return fmt.Errorf("delivery during B: %v, want a temporary failure", derr)
	}

	// A's late defer: B's "next" is not A's; nothing is lifted.
	if ok, err := u.ClearFTSLayoutNext(tokA); ok || err != nil {
		return fmt.Errorf("A's clear: %v %v", ok, err)
	}

	if st := omapKeys(t, u, src.postingOIDn(0), markState)[markState]; string(st) != frozenPrefix+tokA {
		return fmt.Errorf("source state %q after A's clear", st)
	}

	return nil
}

// allFound fails the test unless the first n test documents are found.
func allFound(t *testing.T, u *User, n int) {
	t.Helper()

	all, err := u.SearchFields([]string{fts.FieldBody}, "unique")
	if err != nil {
		t.Fatal(err)
	}

	for i := range n {
		if !all[docGUID(i)] {
			t.Errorf("document %d lost", i)
		}
	}
}

// TestLayoutFrozenWaitOnce (F6): an indexing waits once for a frozen
// index, centrally, not once per posting object.
func TestLayoutFrozenWaitOnce(t *testing.T) {
	s := layout3Store(t, func(c *FTSConfig) { c.FrozenWait = 300 * time.Millisecond; c.ShardsMin = 4 })
	u := testUser(t, s)
	mustMailbox(t, u, "INBOX")
	deliverDocs(t, u, layoutDocs(3))

	if l := layoutOf(t, u); l.S != 4 {
		t.Fatalf("shards_min 4: %+v", l)
	}

	crashAt(t, u.name, phaseP3)

	if _, err := u.ConvertFTSLayout(model.FTSLayoutRequest{Layout: 3, Shards: 1, Tok: "job-wait/1"}); err == nil {
		t.Fatal("no crash")
	}

	start := time.Now()

	if _, err := u.Deliver("INBOX", testMessage("waitonce", 2000), nil, time.Time{}); !errors.Is(err, errFTSFrozen) {
		t.Fatalf("delivery: %v", err)
	}

	if d := time.Since(start); d > time.Second {
		t.Errorf("waited %v for 4 frozen objects (once is 300ms)", d)
	}
}

// TestLayoutTooManyGenerations (F6): an indexing that has to follow a
// third change of generation fails.
func TestLayoutTooManyGenerations(t *testing.T) {
	w := &ftsWrite{}
	u := &User{ftsShards: 4}

	for i := range maxGenSwitches {
		if err := u.followGen(w, &ftsLayout{F: 3, G: fmt.Sprintf("%016d", i), S: 1, D: 1, H: 1}, "generation"); err != nil {
			t.Fatalf("switch %d: %v", i+1, err)
		}
	}

	if err := u.followGen(w, &ftsLayout{F: 3, G: "x", S: 1, D: 1, H: 1}, "generation"); !errors.Is(err, errFTSRetries) {
		t.Errorf("third switch: %v", err)
	}
}

// TestLayoutHeaderBeforePostings (F14): the header text is written
// before the posting objects in both layouts: an indexing that stops at
// a missing posting object has written it already.
func TestLayoutHeaderBeforePostings(t *testing.T) {
	for _, layout := range []int{2, 3} {
		s := testStore(t)
		if layout == 3 {
			s = layout3Store(t, nil)
		}

		u := testUser(t, s)
		mustMailbox(t, u, "INBOX")
		deliverDocs(t, u, layoutDocs(2))

		l := layoutOf(t, u)
		loc := curLoc(l, u.ftsShards)

		// the posting objects go (layout 3: missing in the named
		// generation; layout 2: a record that makes them anomalies)
		if err := u.deleteObjects(loc.postingOIDs()); err != nil {
			t.Fatal(err)
		}

		if layout == 2 {
			if err := casOmap(u, u.idx, oidFTSMap, nil, func(map[string][]byte) (map[string][]byte, []string) {
				return map[string][]byte{ftsLayoutKey: (&ftsLayout{F: 2, Next: &ftsGen{F: 3, G: "aaaaaaaaaaaaaaaa", S: 1, D: 1, H: 1, Tok: "job-h/1"}}).encode()}, nil
			}); err != nil {
				t.Fatal(err)
			}
		}

		guid := docGUID(40)

		_, err := u.DeliverGUID("INBOX", guid, []byte("Subject: headerfirst\r\n\r\nbody\r\n"), nil, time.Time{})
		if !errors.Is(err, errFTSShardMissing) {
			t.Fatalf("layout %d: delivery without posting objects: %v", layout, err)
		}

		// the undo removed it again; the order shows in the attempt
		// itself, so index directly and look before the undo
		ids, lay, err := u.ensureDocIDs([]string{guid})
		if err != nil {
			t.Fatal(err)
		}

		docsOf, terms, headers := termDocs([]ftsDoc{{guid: guid, raw: []byte("Subject: headerfirst\r\n\r\nbody\r\n")}}, ids, fts.TextOptions{})
		w := &ftsWrite{}
		w.follow(lay, u.ftsShards)

		if err := u.indexInto(w, docsOf, terms, headers); asAnomaly(err) == nil {
			t.Fatalf("layout %d: no anomaly: %v", layout, err)
		}

		if _, ok := omapKeys(t, u, loc.headerOID(guid), guid)[guid]; !ok {
			t.Errorf("layout %d: the header text was not written before the postings", layout)
		}
	}
}

// TestLayoutUndoFrozenNoWait (F13): the undo of a delivery, the upload
// expiry and the scrubber's orphans do not wait for a frozen index; the
// id is freed in any case.
func TestLayoutUndoFrozenNoWait(t *testing.T) {
	s := layout3Store(t, func(c *FTSConfig) { c.FrozenWait = 5 * time.Second })
	u := testUser(t, s)
	mustMailbox(t, u, "INBOX")
	deliverDocs(t, u, layoutDocs(3))

	crashAt(t, u.name, phaseP3)

	if _, err := u.ConvertFTSLayout(model.FTSLayoutRequest{Layout: 3, Shards: 2, Tok: "job-undo/1"}); err == nil {
		t.Fatal("no crash")
	}

	ftsLayoutHook = nil

	metas, err := u.ListMessagesByName("INBOX")
	if err != nil {
		t.Fatal(err)
	}

	byGUID := map[string]Meta{}
	for _, x := range metas {
		byGUID[x.GUID] = x
	}

	m, m2 := byGUID[docGUID(1)], byGUID[docGUID(2)]

	start := time.Now()
	_ = u.ftsRemoveWait(&m, 0)

	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("waited %v", d)
	}

	if _, ok := omapKeys(t, u, oidFTSMap, docKey(m.GUID))[docKey(m.GUID)]; ok {
		t.Error("the id was not freed")
	}

	// a blob that cannot be read (N9): the id goes too
	u.deleteBlobAll(m2.GUID, m2.Chunks)

	u.ftsCfg.frozenWait = 0
	if err := u.ftsRemove(&m2); err == nil {
		t.Error("removal without a blob reported no error")
	}

	if _, ok := omapKeys(t, u, oidFTSMap, docKey(m2.GUID))[docKey(m2.GUID)]; ok {
		t.Error("the id of an unreadable blob was not freed")
	}
}

// TestLayoutParallelCreators (F10): two writers that create the first
// generation of a fresh index at once: one record wins, the loser's
// objects go, the presence entry stays.
func TestLayoutParallelCreators(t *testing.T) {
	s := layout3Store(t, nil)
	name := testAccount(t)

	setup := testUserNamed(t, s, name)
	mustMailbox(t, setup, "INBOX")

	guids := deliverConcurrently(t, s, name, 4, 1)
	u := testUserNamed(t, s, name)

	l := layoutOf(t, u)
	if l == nil || l.F != 3 {
		t.Fatalf("record %+v", l)
	}

	if !inPresenceIndex(t, s, name) {
		t.Error("presence entry lost")
	}

	it, err := listObjects(u.idx, "test")
	if err != nil {
		t.Fatal(err)
	}
	defer it.Close()

	for it.Next() {
		if oid := it.Value(); strings.HasPrefix(oid, ftxPrefix) && !strings.HasPrefix(oid, ftxPrefix+l.G+"/") {
			t.Errorf("object of a lost creation left: %s", oid)
		}
	}

	hits, err := u.SearchFields([]string{fts.FieldBody}, "lorem")
	if err != nil || len(hits) != len(guids) {
		t.Errorf("search %d of %d: %v", len(hits), len(guids), err)
	}
}
