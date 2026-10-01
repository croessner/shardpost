//go:build tentacle

package cephstore

import (
	"context"
	"errors"
	"strconv"
	"testing"
	"time"

	"shardpost/internal/model"
)

// TestDeleteMailboxWithoutShard0: a registered mailbox whose shard 0 is
// gone is deleted without anything to release.
func TestDeleteMailboxWithoutShard0(t *testing.T) {
	u := testUser(t, testStore(t))
	mb := mustMailbox(t, u, "Lost")

	if err := u.idx.Delete(mboxOID(mb.ID)); err != nil {
		t.Fatal(err)
	}

	if err := u.DeleteMailbox("Lost"); err != nil {
		t.Fatal(err)
	}

	u.invalidate()

	if _, err := u.Lookup("Lost"); !errors.Is(err, ErrNoMailbox) {
		t.Fatalf("lookup: %v", err)
	}

	if keys := registryKeys(t, u, regDeletePrefix); len(keys) != 0 {
		t.Fatalf("tombstone left: %v", keys)
	}
}

// TestImportSharedSourceGUID: messages of one step that carry the same
// source GUID with different content each keep their own blob.
func TestImportSharedSourceGUID(t *testing.T) {
	u := testUser(t, testStore(t))
	mb := mustMailbox(t, u, "INBOX")
	guid := newGUID()

	msgs := make([]model.ImportMessage, 0, 4)
	for i := range 4 {
		msgs = append(msgs, model.ImportMessage{UID: uint32(i + 1), GUID: guid, Raw: testMessage("same id", 300+700*i)})
	}

	if _, err := u.ImportMessages(mb.ID, msgs); err != nil {
		t.Fatal(err)
	}

	metas, err := u.ListMessages(mb.ID)
	if err != nil || len(metas) != 4 {
		t.Fatalf("entries %d: %v", len(metas), err)
	}

	for i := range metas {
		raw, err := u.Read(&metas[i])
		if err != nil || len(raw) != len(msgs[i].Raw) {
			t.Fatalf("uid %d: %d bytes, %v; want %d", metas[i].UID, len(raw), err, len(msgs[i].Raw))
		}
	}
}

// TestDeleteWithEntryInFlight: a delivery's entry that may still land on
// a higher shard keeps the shards and the tombstone for the scrubber.
func TestDeleteWithEntryInFlight(t *testing.T) {
	u := testUser(t, testStore(t))
	mb := mustMailbox(t, u, "Busy")
	deliverN(t, u, "Busy", 6, 300) // shards 0 and 1

	info, _, err := u.readHeader(mb)
	if err != nil {
		t.Fatal(err)
	}

	// A delivery announced UID 7 and has not written its entry yet.
	next := info.HighestModSeq + 1
	pending := changeRec{UIDs: []uint32{7}, Pending: true, Time: time.Now().Unix()}

	if err := casOmap(u, u.idx, mboxOID(mb.ID), nil, func(map[string][]byte) (map[string][]byte, []string) {
		return map[string][]byte{chgKey(next): encode(pending), keyModSeq: []byte(strconv.FormatUint(next, 10))}, nil
	}); err != nil {
		t.Fatal(err)
	}

	if err := u.DeleteMailbox("Busy"); err != nil {
		t.Fatal(err)
	}

	if !u.objectExists(u.shardOID(mb.ID, 1)) || len(registryKeys(t, u, regDeletePrefix)) != 1 {
		t.Fatal("the shards or the tombstone went while an entry was on its way")
	}
}

// TestSweepCleansAfterPurge: what a purge without listing cannot name
// (debris without an intent) goes with the next repairing sweep, which
// then forgets the purge.
func TestSweepCleansAfterPurge(t *testing.T) {
	s := purgeStore(t, false)
	u := testUser(t, s)
	mustMailbox(t, u, "INBOX")
	deliverN(t, u, "INBOX", 2, 300)
	legacy, _, _ := legacyDebris(t, u)

	if _, err := u.PurgeNamespace(func() error { return nil }); err != nil {
		t.Fatal(err)
	}

	if !blobExistsT(u, legacy) {
		t.Fatal("the purge found the legacy blob after all; the test proves nothing")
	}

	if purged, err := s.purged(u.name); err != nil || !purged {
		t.Fatalf("not recorded as purged: %v", err)
	}

	time.Sleep(1100 * time.Millisecond)

	if _, err := s.Sweep(context.Background(), SweepOptions{Repair: true, Restart: true}); err != nil {
		t.Fatal(err)
	}

	if left := append(objects(t, u.idx), objects(t, u.data)...); len(left) > 0 {
		t.Fatalf("objects left after the sweep: %v", left)
	}

	if purged, _ := s.purged(u.name); purged {
		t.Fatal("the purge record stayed")
	}
}

// TestPendingWaitsForUploadGrace: an upload that expired within the
// intent grace is not yet the scrub's.
func TestPendingWaitsForUploadGrace(t *testing.T) {
	u := testUser(t, testStore(t))
	now := time.Now().Unix()

	if err := u.setIntent(map[string][]byte{uploadKey(newGUID()): encode(blobIntent{Time: now - 10, Expires: now - 1})}); err != nil {
		t.Fatal(err)
	}

	if p, err := u.ScrubPending(); err != nil || p != "" {
		t.Fatalf("pending %q %v for an upload just expired", p, err)
	}
}
