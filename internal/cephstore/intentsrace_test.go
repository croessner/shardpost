//go:build tentacle

package cephstore

import (
	"syscall"
	"testing"
	"time"

	"shardpost/internal/model"
)

// Writes still in flight while a scrub runs: what their young intents
// name keeps its reference count and its objects.

// youngStore is a store whose intents stay young for the scrubs of a
// test (the default intent grace of an hour).
func youngStore(t *testing.T, source model.OrphanSource) *Store {
	t.Helper()

	return storeWith(t, func(c *Config) { c.OrphanSource = source })
}

// TestFixRefsSkipsReleaseInFlight: an EXPUNGE removed its entry and has
// not released the blob yet when the scrub counts. Fixing the count to
// the entries would make the release that follows take the blob from
// the copy.
func TestFixRefsSkipsReleaseInFlight(t *testing.T) {
	u := testUser(t, youngStore(t, model.OrphanSourceIntents))
	a := mustMailbox(t, u, "INBOX")
	mustMailbox(t, u, "Copy")

	m := deliverN(t, u, "INBOX", 1, 300)[0]
	if _, err := u.Copy([]Meta{m}, "Copy"); err != nil {
		t.Fatal(err)
	}

	ures, err := u.unlink(a, []Meta{m}, "", true)
	if err != nil {
		t.Fatal(err)
	}

	removed, rkeys := ures.removed, ures.rkeys

	if rep := scrubNow(t, u); rep.RefsFixed != 0 || refsOf(t, u, m.GUID) != "2" {
		t.Fatalf("refs fixed %d, refs %s; want 0, 2", rep.RefsFixed, refsOf(t, u, m.GUID))
	}

	u.releaseRemoved(a, removed, rkeys)

	if !blobExistsT(u, m.GUID) || refsOf(t, u, m.GUID) != "1" {
		t.Fatal("the copy lost its blob")
	}
}

// TestFixRefsSkipsDeletionInFlight: the same for a DELETE between its
// unregistration and the release of its blobs.
func TestFixRefsSkipsDeletionInFlight(t *testing.T) {
	u := testUser(t, youngStore(t, model.OrphanSourceIntents))
	mb := mustMailbox(t, u, "Trash")
	mustMailbox(t, u, "Keep")

	m := deliverN(t, u, "Trash", 1, 300)[0]
	if _, err := u.Copy([]Meta{m}, "Keep"); err != nil {
		t.Fatal(err)
	}

	info, metas, err := u.readForDelete(mb)
	if err != nil {
		t.Fatal(err)
	}

	if _, lost, err := u.unregisterMailbox(mb, &lease{}, info, ""); err != nil || lost {
		t.Fatalf("unregister: %v %v", err, lost)
	}

	if rep := scrubNow(t, u); rep.RefsFixed != 0 || refsOf(t, u, m.GUID) != "2" {
		t.Fatalf("refs fixed %d, refs %s; want 0, 2", rep.RefsFixed, refsOf(t, u, m.GUID))
	}

	if _, err := u.destroyUnregistered(mb, info, metas, "Trash"); err != nil {
		t.Fatal(err)
	}

	if !blobExistsT(u, m.GUID) || refsOf(t, u, m.GUID) != "1" {
		t.Fatal("the copy lost its blob")
	}
}

// TestDeletionProbesAboveRange: an entry that landed on a shard above
// the range the tombstone and the retired header give is found by
// probing, and its blob and shard go.
func TestDeletionProbesAboveRange(t *testing.T) {
	u := testUser(t, intentsStore(t, model.OrphanSourceIntents))
	mb := mustMailbox(t, u, "Late")
	deliverN(t, u, "Late", 6, 300) // shards 0 and 1

	crashedDelete(t, u, mb)

	// A delivery's entry for UID 9 (shard 2) arrived after the retirement.
	guid := newGUID()
	raw := testMessage("late", 300)

	if _, err := u.writeBlob(guid, raw); err != nil {
		t.Fatal(err)
	}

	late := Meta{UID: 9, GUID: guid, Size: int64(len(raw)), Chunks: 1}
	if err := u.writeEntries(u.shardOID(mb.ID, 2), map[string][]byte{uidKey(9): encode(late)}, nil, false); err != nil {
		t.Fatal(err)
	}

	rep := scrubNow(t, u)
	if rep.DeletionsFinished != 1 || blobExistsT(u, guid) {
		t.Fatalf("deletions %d, late blob left %v", rep.DeletionsFinished, blobExistsT(u, guid))
	}

	assertDeleted(t, u, mb)
}

// TestDeletedRangeIgnoresHeaderless: a shard 0 that only a lock or a late
// write created again says nothing about the range.
func TestDeletedRangeIgnoresHeaderless(t *testing.T) {
	u := testUser(t, testStore(t))
	id := newGUID()

	if err := casOmap(u, u.idx, mboxOID(id), nil, func(map[string][]byte) (map[string][]byte, []string) {
		return map[string][]byte{chgKey(5): []byte("{}")}, nil
	}); err != nil {
		t.Fatal(err)
	}

	info, err := u.deletedRange(id, mailboxIntent{UIDNext: 9, ShardSize: 4})
	if err != nil || info.UIDNext != 9 || info.ShardSize != 4 {
		t.Fatalf("range %d/%d, want the tombstone's 9/4", info.UIDNext, info.ShardSize)
	}
}

// TestTombstoneOfStaleSnapshot: a mailbox the run's snapshot still has but
// that is unregistered when the intents are read is a deletion, not a
// re-created id.
func TestTombstoneOfStaleSnapshot(t *testing.T) {
	u := testUser(t, intentsStore(t, model.OrphanSourceIntents))
	mb := mustMailbox(t, u, "Box")
	crashedDelete(t, u, mb)

	time.Sleep(1100 * time.Millisecond)

	sr := &scrubRun{u: u, log: u.log, rep: &ScrubReport{}, icut: time.Now(), byID: map[string]Mailbox{mb.ID: *mb},
		intents: newIntentScan()}

	in, err := sr.gatherIntents()
	if err != nil {
		t.Fatal(err)
	}

	if len(in.deletions) != 1 || len(in.drop[oidRegistry]) != 0 {
		t.Fatalf("deletions %d, drops %v; want the deletion, no drop", len(in.deletions), in.drop)
	}
}

// TestReleaseKeepsForeignRemovalIntent: a removal that announced an
// entry another session removed first leaves that session's intent.
func TestReleaseKeepsForeignRemovalIntent(t *testing.T) {
	u := testUser(t, testStore(t))
	mb := mustMailbox(t, u, "INBOX")
	metas := deliverN(t, u, "INBOX", 6, 300)
	m := metas[5] // shard 1

	ures, err := u.unlink(mb, []Meta{m}, "", true) // session A, not released yet
	if err != nil || len(ures.removed) != 1 {
		t.Fatalf("unlink: %v, %d", err, len(ures.removed))
	}

	removed, rkeys := ures.removed, ures.rkeys

	if err := u.Expunge("INBOX", []Meta{m}); err != nil { // session B, stale snapshot
		t.Fatal(err)
	}

	if keys := omapKeys(t, u, mboxOID(mb.ID), keyRemovePrefix); len(keys) != 1 {
		t.Fatalf("removal intents %v; want A's", keys)
	}

	u.releaseRemoved(mb, removed, rkeys)

	if keys := omapKeys(t, u, mboxOID(mb.ID), keyRemovePrefix); len(keys) != 0 || blobExistsT(u, m.GUID) {
		t.Fatalf("after A's release: intents %v, blob left %v", keys, blobExistsT(u, m.GUID))
	}
}

// TestCreationIntentStaysOnConflict: a creation that finds its id taken
// keeps the intent (it may be another creator's).
func TestCreationIntentStaysOnConflict(t *testing.T) {
	u := testUser(t, testStore(t))
	rec := MailboxRec{ID: newGUID(), UIDValidity: 1}

	if err := u.createIndexObjectSized(&rec, 4); err != nil {
		t.Fatal(err)
	}

	if err := u.createIntended(&rec, "X", 4); errCode(err) != syscall.EEXIST {
		t.Fatalf("createIntended: %v, want EEXIST", err)
	}

	if keys := registryKeys(t, u, regCreatePrefix); len(keys) != 1 {
		t.Fatalf("creation intents %v, want one", keys)
	}
}

// TestCollectBlobHonoursCutoff: a blob touched after the cutoff stays.
func TestCollectBlobHonoursCutoff(t *testing.T) {
	u := testUser(t, testStore(t))
	guid := newGUID()

	if _, err := u.writeBlob(guid, testMessage("young", 300)); err != nil {
		t.Fatal(err)
	}

	gone, err := u.collectBlob(guid, 300, 1, false, time.Now().Add(-time.Hour))
	if err != nil || gone || !blobExistsT(u, guid) {
		t.Fatalf("collected a young blob: %v %v", gone, err)
	}
}

// TestUncoveredEntriesKeepTheirBlobs: entries an import left above
// UIDNEXT that the run may not remove keep their blobs, although the
// import's intent is old and no counted entry references them.
func TestUncoveredEntriesKeepTheirBlobs(t *testing.T) {
	u := testUser(t, intentsStore(t, model.OrphanSourceIntents))
	mb := mustMailbox(t, u, "INBOX")
	msgs := []model.ImportMessage{{UID: 5, Raw: testMessage("ahead", 300)}}
	salt, guids := importGUIDs(msgs)

	if _, err := u.reserveWith(mb, Quota{}, map[string][]byte{regImportBlobPrefix + "b": u.importIntent(mb.ID, salt, guids)}); err != nil {
		t.Fatal(err)
	}

	ps, err := u.prepareImport(msgs, guids)
	if err != nil {
		t.Fatal(err)
	}

	// The step wrote its entry on shard 0 and stopped before the header.
	if err := casOmap(u, u.idx, mboxOID(mb.ID), nil, func(map[string][]byte) (map[string][]byte, []string) {
		return map[string][]byte{uidKey(5): encode(ps[0].meta)}, nil
	}); err != nil {
		t.Fatal(err)
	}

	time.Sleep(1100 * time.Millisecond)

	repairs := map[string]bool{RepairOrphans: true, RepairRefs: true}
	if _, err := u.Scrub(ScrubOptions{Repairs: repairs}); err != nil {
		t.Fatal(err)
	}

	if !blobExistsT(u, ps[0].meta.GUID) {
		t.Fatal("the blob of an uncovered entry was collected")
	}
}

// TestRepairGrace: without a listing the scrub and the repairing sweep
// keep a minimum grace period.
func TestRepairGrace(t *testing.T) {
	saved := minRepairGrace
	defer func() { minRepairGrace = saved }()

	minRepairGrace = 10 * time.Minute

	if g := repairGrace(0); g != 10*time.Minute {
		t.Fatalf("grace %v, want the minimum", g)
	}

	if g := repairGrace(time.Hour); g != time.Hour {
		t.Fatalf("grace %v, want the configured hour", g)
	}
}
