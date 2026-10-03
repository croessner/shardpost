//go:build tentacle

package cephstore

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"shardpost/internal/model"
)

// TestRemovalBetweenEntriesAndIntents: a removal whose header CAS lands
// after the scrub read the entries and before it read the removal
// intents is seen by its intent, so its blob keeps its count.
func TestRemovalBetweenEntriesAndIntents(t *testing.T) {
	u := testUser(t, youngStore(t, model.OrphanSourceIntents))
	a := mustMailbox(t, u, "INBOX")
	mustMailbox(t, u, "Copy")

	m := deliverN(t, u, "INBOX", 1, 300)[0]
	if _, err := u.Copy([]Meta{m}, "Copy"); err != nil {
		t.Fatal(err)
	}

	var (
		removed []Meta
		rkeys   []string
	)

	time.Sleep(1100 * time.Millisecond)

	rep, err := u.Scrub(ScrubOptions{afterList: func(mb Mailbox) {
		if mb.ID != a.ID || removed != nil {
			return
		}

		if ures, uerr := u.unlink(a, []Meta{m}, "", true); uerr != nil {
			t.Error(uerr)
		} else {
			removed, rkeys = ures.removed, ures.rkeys
		}
	}})
	if err != nil {
		t.Fatal(err)
	}

	if rep.RefsFixed != 0 || refsOf(t, u, m.GUID) != "2" {
		t.Fatalf("refs fixed %d, refs %s; want 0, 2", rep.RefsFixed, refsOf(t, u, m.GUID))
	}

	u.releaseRemoved(a, removed, rkeys)

	if !blobExistsT(u, m.GUID) || refsOf(t, u, m.GUID) != "1" {
		t.Fatal("the copy lost its blob")
	}
}

// TestDeleteAfterForeignExpunge: an EXPUNGE of another session between
// the read of the messages and the unregistration released its entry;
// DELETE must not release it again.
func TestDeleteAfterForeignExpunge(t *testing.T) {
	u := testUser(t, testStore(t))
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

	if err := u.Expunge("Trash", []Meta{m}); err != nil {
		t.Fatal(err)
	}

	if _, lost, err := u.unregisterMailbox(mb, &lease{}, info, ""); err != nil || lost {
		t.Fatalf("unregister: %v %v", err, lost)
	}

	if _, err := u.destroyUnregistered(mb, info, metas, "Trash"); err != nil {
		t.Fatal(err)
	}

	if !blobExistsT(u, m.GUID) || refsOf(t, u, m.GUID) != "1" {
		t.Fatal("the copy lost its blob: released twice")
	}

	assertDeleted(t, u, mb)
}

// TestDeleteLeavesForeignRemoval: a removal of another session that has
// not released yet keeps the shards and the tombstone for the scrubber.
func TestDeleteLeavesForeignRemoval(t *testing.T) {
	u := testUser(t, testStore(t))
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

	ures, err := u.unlink(mb, []Meta{m}, "", true) // the other session, not released yet
	if err != nil {
		t.Fatal(err)
	}

	removed, rkeys := ures.removed, ures.rkeys

	if _, lost, err := u.unregisterMailbox(mb, &lease{}, info, ""); err != nil || lost {
		t.Fatalf("unregister: %v %v", err, lost)
	}

	if _, err := u.destroyUnregistered(mb, info, metas, "Trash"); err != nil {
		t.Fatal(err)
	}

	if refsOf(t, u, m.GUID) != "2" || !u.objectExists(mboxOID(mb.ID)) || len(registryKeys(t, u, regDeletePrefix)) != 1 {
		t.Fatal("the delete did not leave the foreign removal to the scrubber")
	}

	u.releaseRemoved(mb, removed, rkeys)

	if refsOf(t, u, m.GUID) != "1" {
		t.Fatalf("refs %s after the foreign release, want 1", refsOf(t, u, m.GUID))
	}
}

// TestPutScriptCreatesRegistry: an account that only stores Sieve
// scripts has a registry and a directory entry.
func TestPutScriptCreatesRegistry(t *testing.T) {
	s := testStore(t)
	u := testUser(t, s)

	if err := u.PutScript("s", "keep;"); err != nil {
		t.Fatal(err)
	}

	if !u.objectExists(oidRegistry) {
		t.Fatal("no registry")
	}

	if state, ok, err := s.directoryState(u.name); err != nil || !ok || state != model.AccountActive {
		t.Fatalf("directory entry %q %v %v", state, ok, err)
	}
}

// TestEnumerateRefusesIncompleteModel: a registry record that does not
// parse, or a registered mailbox without shard 0 (for the sweep), is an
// error rather than a smaller model; the sweep then leaves the account
// alone.
func TestEnumerateRefusesIncompleteModel(t *testing.T) {
	u := testUser(t, testStore(t))
	mb := mustMailbox(t, u, "INBOX")

	if err := u.idx.Delete(mboxOID(mb.ID)); err != nil {
		t.Fatal(err)
	}

	if _, err := u.enumerate(false); err != nil {
		t.Fatalf("lenient: %v", err)
	}

	if _, err := u.enumerate(true); !errors.Is(err, errModelIncomplete) {
		t.Fatalf("strict: %v, want errModelIncomplete", err)
	}

	if err := casOmap(u, u.idx, oidRegistry, nil, func(map[string][]byte) (map[string][]byte, []string) {
		return map[string][]byte{regKey("Broken"): []byte("{not json")}, nil
	}); err != nil {
		t.Fatal(err)
	}

	if _, err := u.enumerate(false); !errors.Is(err, errModelIncomplete) {
		t.Fatalf("broken record: %v, want errModelIncomplete", err)
	}
}

// TestImportIntentStaysSmall: the intent of the largest import step fits
// an omap value with room to spare, and the fallbacks follow from the
// salt.
func TestImportIntentStaysSmall(t *testing.T) {
	u := testUser(t, testStore(t))
	msgs := make([]model.ImportMessage, 10000)

	salt, guids := importGUIDs(msgs)
	if v := u.importIntent("id", salt, guids); len(v) > 512<<10 {
		t.Fatalf("intent of %d bytes", len(v))
	}

	var im importIntent
	if err := json.Unmarshal(u.importIntent("id", salt, guids[:1]), &im); err != nil {
		t.Fatal(err)
	}

	if all := im.guids(); len(all) != 3 || all[1] != guids[0][1] || all[2] != guids[0][2] {
		t.Fatalf("fallbacks %v, want %v", all, guids[0])
	}

	if other, _ := importGUIDs(msgs[:1]); importFallback(other, 0, guids[0][0], 1) == guids[0][1] {
		t.Fatal("the fallbacks do not depend on the salt")
	}
}

// TestUncoveredRemovalReleases: entries above UIDNEXT go with their
// reference, under a removal intent that is gone afterwards.
func TestUncoveredRemovalReleases(t *testing.T) {
	u := testUser(t, intentsStore(t, model.OrphanSourceIntents))
	mb := mustMailbox(t, u, "INBOX")

	guid := newGUID()
	raw := testMessage("ahead", 300)

	if _, err := u.writeBlob(guid, raw); err != nil {
		t.Fatal(err)
	}

	m := Meta{UID: 3, GUID: guid, Size: int64(len(raw)), Chunks: 1}

	if err := casOmap(u, u.idx, mboxOID(mb.ID), nil, func(map[string][]byte) (map[string][]byte, []string) {
		return map[string][]byte{uidKey(3): encode(m)}, nil
	}); err != nil {
		t.Fatal(err)
	}

	rep := scrubNow(t, u)
	if rep.UncoveredEntries != 1 || blobExistsT(u, guid) {
		t.Fatalf("uncovered %d, blob left %v", rep.UncoveredEntries, blobExistsT(u, guid))
	}

	if keys := omapKeys(t, u, mboxOID(mb.ID), keyRemovePrefix); len(keys) != 0 {
		t.Fatalf("removal intents left: %v", keys)
	}
}

// TestCreationIntentOfDeletedMailbox: an old creation intent of a
// mailbox whose deletion is still young leaves its shard 0 to the
// deletion.
func TestCreationIntentOfDeletedMailbox(t *testing.T) {
	u := testUser(t, youngStore(t, model.OrphanSourceIntents))
	mb := mustMailbox(t, u, "Box")
	deliverN(t, u, "Box", 2, 300)

	if err := u.setIntent(map[string][]byte{createKey(mb.ID): encode(mailboxIntent{Time: 1})}); err != nil {
		t.Fatal(err)
	}

	crashedDelete(t, u, mb)
	scrubNow(t, u)

	if !u.objectExists(mboxOID(mb.ID)) {
		t.Fatal("the creation intent took shard 0 of a mailbox being deleted")
	}
}

// TestPendingIgnoresLiveUploads: an upload that has not expired keeps no
// account from being idle; one that expired does.
func TestPendingIgnoresLiveUploads(t *testing.T) {
	u := testUser(t, testStore(t))

	if _, err := u.PutBlob(testMessage("upload", 300)); err != nil {
		t.Fatal(err)
	}

	if p, err := u.ScrubPending(); err != nil || p != "" {
		t.Fatalf("pending %q %v with a live upload", p, err)
	}

	if err := u.setIntent(map[string][]byte{uploadKey(newGUID()): encode(blobIntent{Time: 1, Expires: 2})}); err != nil {
		t.Fatal(err)
	}

	if p, err := u.ScrubPending(); err != nil || p == "" {
		t.Fatalf("pending %q %v with an expired upload", p, err)
	}
}
