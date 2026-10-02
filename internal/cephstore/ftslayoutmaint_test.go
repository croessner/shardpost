//go:build tentacle

package cephstore

import (
	"slices"
	"strings"
	"testing"

	"shardpost/internal/fts"
	"shardpost/internal/model"
)

// layoutWithLeftovers is a layout 3 account with an unfinished
// conversion (crashed after P3) and an orphan generation nothing names
// (a creation that lost its race and crashed before deleting).
func layoutWithLeftovers(t *testing.T, s *Store) (*User, ftsLoc, ftsLoc) {
	t.Helper()

	u := testUser(t, s)
	mustMailbox(t, u, "INBOX")
	deliverDocs(t, u, layoutDocs(5))

	crashAt(t, u.name, phaseP3)

	if _, err := u.ConvertFTSLayout(model.FTSLayoutRequest{Layout: 3, Shards: 2, Tok: "job-left/1"}); err == nil {
		t.Fatal("no crash")
	}

	ftsLayoutHook = nil

	// the crashed run's P5 had begun: its targets exist
	next := genLoc(*layoutOf(t, u).Next, u.ftsShards)
	if err := u.createGen(next); err != nil {
		t.Fatal(err)
	}

	orphan := ftsLoc{f: model.FTSLayout3, g: newFTSGen(), s: 1, d: 1, h: 1}
	if err := u.createGen(orphan); err != nil {
		t.Fatal(err)
	}

	return u, next, orphan
}

// TestLayoutPurgeAndNaming (N13, B3): the data model names every
// generation the record holds (the sweep's and the purge's enumeration,
// one more read for the record); an orphan generation is not named, so
// the sweep takes it for a remnant. A purge leaves no object and takes
// the account out of the presence index.
func TestLayoutPurgeAndNaming(t *testing.T) {
	s := layout3Store(t, nil)
	u, next, orphan := layoutWithLeftovers(t, s)

	checkNamed(t, u, next, orphan)

	// the orphan is what the purge cannot name; delete it as the sweep would
	if err := u.deleteObjects(orphan.objects()); err != nil {
		t.Fatal(err)
	}

	if _, err := u.PurgeNamespace(func() error { return nil }); err != nil {
		t.Fatal(err)
	}

	if left := objects(t, u.idx); len(left) > 0 {
		t.Errorf("objects left: %v", left)
	}

	if inPresenceIndex(t, s, u.name) {
		t.Error("purged account still in the presence index")
	}
}

// checkNamed: the enumeration names the current generation and the
// conversion's targets before the id map, not the orphan, not the layout
// 2 objects of a record that says they are gone.
func checkNamed(t *testing.T, u *User, next, orphan ftsLoc) {
	t.Helper()

	named, err := u.enumerate(true)
	if err != nil {
		t.Fatal(err)
	}

	index := map[string]bool{}
	for _, oid := range named.index {
		index[oid] = true
	}

	cur := curLoc(layoutOf(t, u), u.ftsShards)

	for _, oid := range append(cur.objects(), next.objects()...) {
		if !index[oid] {
			t.Errorf("%s is not named", oid)
		}
	}

	for _, oid := range append(orphan.objects(), layout2Loc(u.ftsShards).postingOIDn(0)) {
		if index[oid] {
			t.Errorf("%s is named", oid)
		}
	}

	if i, j := slices.Index(named.index, cur.postingOIDn(0)), slices.Index(named.index, oidFTSMap); i < 0 || j < i {
		t.Errorf("id map at %d, generation at %d", j, i)
	}
}

// TestLayoutScrubFindings (§5.4): the scrub reports a missing object of
// the current generation and layout 2 objects in a layout 3 account
// (repair: fts_rebuild), and keeps the presence index in step.
func TestLayoutScrubFindings(t *testing.T) {
	s := layout3Store(t, nil)
	u := testUser(t, s)
	mustMailbox(t, u, "INBOX")
	deliverDocs(t, u, layoutDocs(3))

	loc := curLoc(layoutOf(t, u), u.ftsShards)

	if err := u.deleteObjects([]string{loc.dictOIDn(0)}); err != nil {
		t.Fatal(err)
	}

	if err := u.idx.SetOmap(layout2Loc(u.ftsShards).postingOIDn(0), map[string][]byte{"b:old": nil}); err != nil {
		t.Fatal(err)
	}

	if err := u.unmarkLayout3(); err != nil {
		t.Fatal(err)
	}

	rep, err := u.Scrub(ScrubOptions{})
	if err != nil {
		t.Fatal(err)
	}

	if rep.FTSShardsMissing != 1 || rep.FTSMixedLayout != 1 || rep.FTSPresenceFixed != 1 || rep.Findings() < 2 {
		t.Errorf("report %+v", *rep)
	}

	if !inPresenceIndex(t, s, u.name) {
		t.Error("presence entry not restored")
	}

	// a search over the missing dictionary object fails in the same
	// generation (no "missing = empty" in layout 3)
	if _, err := u.SearchFields([]string{fts.FieldBody}, "ipsu"); err == nil || !strings.Contains(err.Error(), "missing") {
		t.Errorf("search with a missing object: %v", err)
	}
}

// TestLayoutRebuild (N13): fts_rebuild deletes every named generation
// and both layouts and builds the index again in the layout the
// configuration and the mark allow, the presence index following.
func TestLayoutRebuild(t *testing.T) {
	s := layout3Store(t, nil)
	u, next, orphan := layoutWithLeftovers(t, s)

	old := curLoc(layoutOf(t, u), u.ftsShards)
	noop := func() error { return nil }

	if err := u.RebuildFTS(noop, func(int, int) {}); err != nil {
		t.Fatal(err)
	}

	l := layoutOf(t, u)
	if l == nil || l.F != 3 || l.G == old.g || l.Next != nil {
		t.Fatalf("record after rebuild %+v", l)
	}

	mustExist(t, u, false, append(old.objects(), next.objects()...)...)
	mustExist(t, u, true, orphan.postingOIDn(0)) // unnamed: the sweep's

	if !inPresenceIndex(t, s, u.name) {
		t.Error("not in the presence index after a layout 3 rebuild")
	}

	if hits, err := u.SearchFields([]string{fts.FieldBody}, "unique2word"); err != nil || !hits[docGUID(2)] {
		t.Errorf("search after rebuild: %v %v", hits, err)
	}

	setLayoutMark(t, s, false)

	if err := u.RebuildFTS(noop, func(int, int) {}); err != nil {
		t.Fatal(err)
	}

	if l := layoutOf(t, u); l != nil {
		t.Errorf("record after a rebuild without the mark: %+v", l)
	}

	if inPresenceIndex(t, s, u.name) {
		t.Error("still in the presence index after a layout 2 rebuild")
	}
}
