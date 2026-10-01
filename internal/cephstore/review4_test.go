//go:build tentacle

package cephstore

import (
	"context"
	"testing"
	"time"

	"shardpost/internal/model"
)

// TestSweepKeepsRecreatedAccount: an account purged and created again
// loses its purge record; and a record that stayed anyway never outweighs
// an active directory entry, so a lost registry is not taken for debris.
func TestSweepKeepsRecreatedAccount(t *testing.T) {
	s := purgeStore(t, false)
	u := testUser(t, s)
	mustMailbox(t, u, "INBOX")
	deliverN(t, u, "INBOX", 1, 300)

	if _, err := u.PurgeNamespace(func() error { return nil }); err != nil {
		t.Fatal(err)
	}

	u.invalidate()
	mb := mustMailbox(t, u, "INBOX") // created again
	m := deliverN(t, u, "INBOX", 2, 300)[1]

	if purged, err := s.purged(u.name); err != nil || purged {
		t.Fatalf("purge record after the re-creation: %v %v", purged, err)
	}

	// A record left over (a repair path, an older server) and a lost
	// registry: the active entry wins.
	if err := u.markPurged(); err != nil {
		t.Fatal(err)
	}

	if err := u.idx.Delete(oidRegistry); err != nil {
		t.Fatal(err)
	}

	time.Sleep(1100 * time.Millisecond)

	if _, err := s.Sweep(context.Background(), SweepOptions{Repair: true, Restart: true}); err != nil {
		t.Fatal(err)
	}

	if !blobExistsT(u, m.GUID) || !u.objectExists(mboxOID(mb.ID)) {
		t.Fatal("the sweep deleted an account whose directory entry is active")
	}
}

// TestSweepKeepsRecordOfUncleanNamespace: a purged namespace with objects
// the sweep left (younger than its grace) keeps its record.
func TestSweepKeepsRecordOfUncleanNamespace(t *testing.T) {
	s := purgeStore(t, false)
	u := testUser(t, s)
	mustMailbox(t, u, "INBOX")
	legacy, _, _ := legacyDebris(t, u)

	if _, err := u.PurgeNamespace(func() error { return nil }); err != nil {
		t.Fatal(err)
	}

	ix, release, err := s.AccountIndex()
	if err != nil {
		t.Fatal(err)
	}

	if err := ix.Remove(u.name); err != nil { // the purge's last phase
		t.Fatal(err)
	}

	release()

	// The record must be older than the run's start less the grace, or
	// it would stay for that reason alone.
	if err := setPurgedAt(s, u.name, time.Now().Add(-2*time.Hour)); err != nil {
		t.Fatal(err)
	}

	if _, err := s.Sweep(context.Background(), SweepOptions{Repair: true, Restart: true, Grace: time.Hour}); err != nil {
		t.Fatal(err)
	}

	if !blobExistsT(u, legacy) {
		t.Fatal("the sweep deleted a young object")
	}

	if purged, err := s.purged(u.name); err != nil || !purged {
		t.Fatalf("the record of an unclean namespace went: %v %v", purged, err)
	}
}

// setPurgedAt writes a purge record with the given time.
func setPurgedAt(s *Store, account string, at time.Time) error {
	ix, release, err := s.PurgedIndex()
	if err != nil {
		return err
	}
	defer release()

	return ix.Set(account, encode(purgedRecord{At: at.Unix()}))
}

// TestDeleteKeepsTombstoneForHigherShards: shard 0 is gone but a higher
// shard is left; the tombstone stays and the scrubber removes the shard.
func TestDeleteKeepsTombstoneForHigherShards(t *testing.T) {
	u := testUser(t, intentsStore(t, model.OrphanSourceIntents))
	mb := mustMailbox(t, u, "Half")
	metas := deliverN(t, u, "Half", 6, 300)

	if err := u.idx.Delete(mboxOID(mb.ID)); err != nil {
		t.Fatal(err)
	}

	if err := u.DeleteMailbox("Half"); err != nil {
		t.Fatal(err)
	}

	if len(registryKeys(t, u, regDeletePrefix)) != 1 || !u.objectExists(u.shardOID(mb.ID, 1)) {
		t.Fatal("the tombstone went although a higher shard is left")
	}

	rep := scrubNow(t, u)
	if rep.DeletionsFinished != 1 || u.objectExists(u.shardOID(mb.ID, 1)) || blobExistsT(u, metas[5].GUID) {
		t.Fatalf("deletions %d; the higher shard or its blob is left", rep.DeletionsFinished)
	}
}
