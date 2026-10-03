//go:build tentacle

package cephstore

import (
	"testing"
	"time"

	"shardpost/internal/fts"
	"shardpost/internal/model"
)

// TestLayoutCachedReaderAcrossConversion (review round 2, M1): a view
// that searched a layout 2 index without a record keeps its cached read
// state across a whole 2→3 conversion (an idle session is not ended by
// the account lock); its next search finds the removed layout 2 objects
// missing, reads the record and answers from the new generation.
func TestLayoutCachedReaderAcrossConversion(t *testing.T) {
	conv := layout3Store(t, nil)
	s := testStore(t)

	u := testUser(t, s)
	mustMailbox(t, u, "INBOX")
	deliverDocs(t, u, layoutDocs(10))

	if l := layoutOf(t, u); l != nil {
		t.Fatalf("layout 2 account with a record %+v", l)
	}

	before := searchState(t, u) // caches the read state without a record

	cu := testUserNamed(t, conv, u.name)
	convert(t, cu, model.FTSLayoutRequest{Layout: model.FTSLayout3, Shards: 2})
	mustExist(t, u, false, layout2Loc(u.ftsShards).postingOIDs()...)

	sameSearches(t, "cached reader after the conversion", before, searchState(t, u))
}

// TestLayoutOldCarriesHeartbeat (review round 2, L2): an import reshard
// that switched keeps its heartbeat in its "old" entry until P7, so a
// clear of stale runs sees it alive.
func TestLayoutOldCarriesHeartbeat(t *testing.T) {
	s := layout3Store(t, nil)
	u := testUser(t, s)
	mustMailbox(t, u, "INBOX")
	deliverDocs(t, u, layoutDocs(5))

	tok := "importsession0123456/n1"
	t1 := time.Now().Add(-time.Minute).Truncate(time.Second)

	switchedImportRun(t, u, tok, t1)

	if l := layoutOf(t, u); l.Next != nil || len(l.Old) != 1 || l.Old[0].Tok != tok || l.Old[0].At != t1.Unix() {
		t.Fatalf("record after the switch %+v", l)
	}

	t2 := time.Now().Truncate(time.Second)
	if ok, err := u.HeartbeatFTSLayoutNext(tok, t2); err != nil || !ok {
		t.Fatalf("heartbeat in old: %v %v", ok, err)
	}

	st, err := u.FTSLayout()
	if err != nil || len(st.OldAt) != 1 || !st.OldAt[0].Equal(t2) {
		t.Errorf("state %+v %v", st, err)
	}

	if ok, _ := u.HeartbeatFTSLayoutNext("importsession0123456/other", t2); ok {
		t.Error("heartbeat of another run accepted")
	}
}

// switchedImportRun leaves an import reshard of tok stopped after its
// switch (P6), with the heartbeat at recorded before it.
func switchedImportRun(t *testing.T, u *User, tok string, at time.Time) {
	t.Helper()

	req := model.FTSLayoutRequest{Layout: model.FTSLayout3, Shards: 2, Tok: tok}

	crashAt(t, u.name, phaseP3)

	if _, err := u.ConvertFTSLayout(req); err == nil {
		t.Fatal("no crash after P3")
	}

	if ok, err := u.HeartbeatFTSLayoutNext(tok, at); err != nil || !ok {
		t.Fatalf("heartbeat in next: %v %v", ok, err)
	}

	crashAt(t, u.name, phaseP6)

	if _, err := u.ConvertFTSLayout(req); err == nil {
		t.Fatal("no crash after P6")
	}

	ftsLayoutHook = nil
}

// TestLayoutRemovalRetryKeepsRefilledTerm (review round 2, L3): a removal
// whose earlier attempt emptied a list (noted) and whose retry reads the
// list non-empty again (a delivery during the frozen wait) keeps the
// term's dictionary keys.
func TestLayoutRemovalRetryKeepsRefilledTerm(t *testing.T) {
	s := layout3Store(t, nil)
	u := testUser(t, s)
	mustMailbox(t, u, "INBOX")

	term := fts.FieldBody + ":zzrefilledword"
	raw := []byte("Subject: refill\r\n\r\nzzrefilledword\r\n")

	gone, err := u.Deliver("INBOX", raw, nil, time.Time{})
	if err != nil {
		t.Fatal(err)
	}

	again, err := u.Deliver("INBOX", raw, nil, time.Time{})
	if err != nil {
		t.Fatal(err)
	}

	id, hasID, _, err := u.docIDOf(gone.GUID)
	if err != nil || !hasID {
		t.Fatalf("doc id %v %v", hasID, err)
	}

	l := layoutOf(t, u)
	w := &ftsWrite{wait: u.ftsCfg.frozenWait}
	w.follow(l, u.ftsShards)
	w.note([]string{term}) // an earlier attempt emptied it

	if err := u.removeWith(w, gone.GUID, id, hasID, rawTerms(raw)); err != nil {
		t.Fatal(err)
	}

	dictHas(t, u, curLoc(l, u.ftsShards), term, true)

	u.ftsForget()

	hits, err := u.SearchCandidates([]string{fts.FieldBody}, "refilledwor")
	if err != nil || hits == nil || !hits[again.GUID] {
		t.Errorf("substring search after the removal: %v %v", hits, err)
	}
}
