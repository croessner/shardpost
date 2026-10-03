//go:build tentacle

package cephstore

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"shardpost/internal/fts"
	"shardpost/internal/model"
)

// Tests of the online runs that close the gaps of the review of B14i.

// thawed fails when a posting object of loc is still frozen.
func thawed(t *testing.T, u *User, loc ftsLoc) {
	t.Helper()

	for _, oid := range loc.postingOIDs() {
		if s := string(omapKeys(t, u, oid, markState)[markState]); s != stateLive {
			t.Errorf("%s still %q", oid, s)
		}
	}
}

// found reports whether the body search finds the GUID by word.
func found(t *testing.T, u *User, word, guid string) bool {
	t.Helper()

	hits, err := u.SearchFields([]string{fts.FieldBody}, word)
	if err != nil {
		t.Fatal(err)
	}

	return hits[guid]
}

// TestOnlineLockTakerDuringCopy (review gap a): a lock taker that clears
// the online run while its copy writes (the blind writes assert that
// the targets exist) stops the run with an error and without a switch;
// the record is clean, the source thawed, no record of the run is left
// and the account answers as before.
func TestOnlineLockTakerDuringCopy(t *testing.T) {
	s := onlineStore(t, nil)
	base, _ := onlineDocs()
	u := filledUsers(t, base, s)[0]

	before := searchState(t, u)
	src := curLoc(layoutOf(t, u), u.ftsShards)

	var (
		frozen atomic.Bool
		once   sync.Once
	)

	hookPhases(t, u.name, map[string]func() error{phaseO3: func() error { frozen.Store(true); return nil }})

	fence := func() error {
		if frozen.Load() {
			once.Do(func() {
				if _, err := u.ClearForeignFTSLayoutNext("job-locker/1"); err != nil {
					t.Error(err)
				}
			})
		}

		return nil
	}

	if _, err := u.ConvertFTSLayout(model.FTSLayoutRequest{Online: true, Shards: 4, Tok: "job-copy/1", Fence: fence}); err == nil {
		t.Fatal("the run switched although its targets went during the copy")
	}

	ftsLayoutHook = nil

	if l := cleanRecord(t, u); !curLoc(l, u.ftsShards).same(src) {
		t.Errorf("switched to %+v", l)
	}

	thawed(t, u, src)
	noBuildKeys(t, u)
	sameSearches(t, "after the cleared copy", before, searchState(t, u))
}

// TestOnlineThreeHolds (review gap b): a run that does not converge
// under three holds fails without a switch; the deliveries during the
// holds succeed, and after the job's clean-up the record is clean, the
// source thawed and every delivery found.
func TestOnlineThreeHolds(t *testing.T) {
	s := onlineStore(t, func(c *FTSConfig) { c.BuildCatchupRounds, c.BuildHold = 1, 200*time.Millisecond })
	base, _ := onlineDocs()
	u := filledUsers(t, base, s)[0]
	src := curLoc(layoutOf(t, u), u.ftsShards)

	n := 500

	hookPhases(t, u.name, map[string]func() error{
		// every round, also under a hold (the delivery waits for it)
		phaseO6: func() error {
			n++

			_, err := u.DeliverGUID("INBOX", docGUID(n), []byte(fmt.Sprintf("Subject: flood %d\r\n\r\nholdflood%dword\r\n", n, n)), nil, time.Time{})

			return err
		},
	})

	rep, err := u.ConvertFTSLayout(model.FTSLayoutRequest{Online: true, Shards: 2, Tok: "job-holds/1"})
	if err == nil || !strings.Contains(err.Error(), "3 holds") {
		t.Fatalf("run: %+v %v", rep, err)
	}

	ftsLayoutHook = nil

	if rep.HoldMS < 3*150 {
		t.Errorf("held %d ms", rep.HoldMS)
	}

	if l := layoutOf(t, u); l.Next == nil || l.Next.Hold != 0 || !curLoc(l, u.ftsShards).same(src) {
		t.Errorf("record after the failed run %+v", l)
	}

	// the job's clean-up
	if _, err := u.ClearFTSLayoutNext("job-holds/1"); err != nil {
		t.Fatal(err)
	}

	cleanRecord(t, u)
	thawed(t, u, src)
	noBuildKeys(t, u)

	for j := 501; j <= n; j++ {
		if !found(t, u, fmt.Sprintf("holdflood%dword", j), docGUID(j)) {
			t.Errorf("delivery %d not found", j)
		}
	}
}

// TestOnlineSoftStateIntegration (review gap c, R2.1): a writer that
// wrote through the soft freeze of online run A does not write through
// the hard freeze of the offline run B that replaced A after a lock
// taker cleared it: it waits and fails temporarily; after B's switch
// both of its documents are found.
func TestOnlineSoftStateIntegration(t *testing.T) {
	s := onlineStore(t, func(c *FTSConfig) { c.FrozenWait = 300 * time.Millisecond })
	base, _ := onlineDocs()
	u := filledUsers(t, base, s)[0]

	crashed(t, u, model.FTSLayoutRequest{Online: true, Shards: 2, Tok: "job-a/1"}, phaseO3)

	docs := map[string][]byte{docGUID(600): []byte("Subject: x\r\n\r\nsoftstatexword\r\n"), docGUID(601): []byte("Subject: y\r\n\r\nsoftstateyword\r\n")}
	for g, raw := range docs {
		if _, err := u.writeBlob(g, raw); err != nil {
			t.Fatal(err)
		}
	}

	w := writeThroughA(t, u, docGUID(600), docs[docGUID(600)])

	// a lock taker clears A, the offline run B freezes hard
	if _, err := u.ClearForeignFTSLayoutNext("job-locker/1"); err != nil {
		t.Fatal(err)
	}

	crashed(t, u, model.FTSLayoutRequest{Layout: 3, Shards: 2, Tok: "job-b/1"}, phaseP3)

	// the same operation goes on with its soft state of A
	b, err := u.allocDocIDs([]string{docGUID(601)})
	if err != nil {
		t.Fatal(err)
	}

	w.ids = b.out

	err = u.indexWith(w, &ftsBatch{docs: []ftsDoc{{guid: docGUID(601), raw: docs[docGUID(601)]}}, ids: b.out})
	if !errors.Is(err, errFTSFrozen) {
		t.Fatalf("W wrote through B's hard freeze: %v", err)
	}

	// B goes on and switches; W tries again (a delivery's retry)
	if _, err := u.ConvertFTSLayout(model.FTSLayoutRequest{Layout: 3, Shards: 2, Tok: "job-b/1"}); err != nil {
		t.Fatal(err)
	}

	if err := u.ftsIndex(docGUID(601), docs[docGUID(601)]); err != nil {
		t.Fatal(err)
	}

	cleanRecord(t, u)

	if !found(t, u, "softstatexword", docGUID(600)) || !found(t, u, "softstateyword", docGUID(601)) {
		t.Error("a document of W is missing after B's switch")
	}
}

// crashed runs the conversion until it stops after phase as a crash
// would.
func crashed(t *testing.T, u *User, req model.FTSLayoutRequest, phase string) {
	t.Helper()

	crashAt(t, u.name, phase)

	if _, err := u.ConvertFTSLayout(req); err == nil {
		t.Fatalf("no crash of %s after %s", req.Tok, phase)
	}

	ftsLayoutHook = nil
}

// writeThroughA indexes a document (its blob written) through the soft
// freeze of the online run job-a/1 and returns the write with its soft
// state.
func writeThroughA(t *testing.T, u *User, guid string, raw []byte) *ftsWrite {
	t.Helper()

	a, err := u.allocDocIDs([]string{guid})
	if err != nil || a.built != "job-a/1" {
		t.Fatalf("alloc under A: %+v %v", a, err)
	}

	w := &ftsWrite{wait: u.ftsCfg.frozenWait, built: a.built, ids: a.out}
	w.follow(a.lay, u.ftsShards)

	if err := u.indexWith(w, &ftsBatch{docs: []ftsDoc{{guid: guid, raw: raw}}, ids: a.out}); err != nil || w.softTok != "job-a/1" {
		t.Fatalf("write through A: %v, soft %q", err, w.softTok)
	}

	return w
}

// TestOnlineMarkOff (review finding 4): a run whose mark was removed
// after its plan ends without a change and without an error.
func TestOnlineMarkOff(t *testing.T) {
	s := onlineStore(t, nil)
	base, _ := onlineDocs()
	u := filledUsers(t, base, s)[0]

	setMark(t, s, model.FTSOnlineMarkKey, false)

	rep, err := u.ConvertFTSLayout(model.FTSLayoutRequest{Online: true, Shards: 2, Tok: "job-off/1"})
	if err != nil || rep.Deferred != model.FTSDeferredMarkOff {
		t.Fatalf("run without the mark: %+v %v", rep, err)
	}

	cleanRecord(t, u)
}

// TestOnlineUnreadable (review finding 2): a live message whose blob
// cannot be read fails the rebuild after its attempts; with
// skip_unreadable it is skipped and counted, and the rebuild does not
// make the index trusted (no marker, the rebuild key as it was).
func TestOnlineUnreadable(t *testing.T) {
	s := onlineStore(t, nil)
	base, _ := onlineDocs()
	u := filledUsers(t, base, s)[0]

	if err := u.data.Truncate(chunkOID(docGUID(102), 0), 3); err != nil {
		t.Fatal(err)
	}

	if _, err := u.ConvertFTSLayout(model.FTSLayoutRequest{Online: true, Rebuild: true, Tok: "job-read/1"}); err == nil {
		t.Fatal("the rebuild ignored an unreadable message")
	}

	if _, err := u.ClearFTSLayoutNext("job-read/1"); err != nil {
		t.Fatal(err)
	}

	rep, err := u.ConvertFTSLayout(model.FTSLayoutRequest{Online: true, Rebuild: true, Tok: "job-read/2", SkipUnreadable: true})
	if err != nil || rep.Unreadable != 1 {
		t.Fatalf("rebuild with skip_unreadable: %+v %v", rep, err)
	}

	cleanRecord(t, u)

	if vals := omapKeys(t, u, oidFTSMap, ""); vals[ftsVersionKey3] != nil || vals[ftsRebuildKey] != nil {
		t.Errorf("marker %q, rebuild key %q after a rebuild that skipped a message", vals[ftsVersionKey3], vals[ftsRebuildKey])
	}
}
