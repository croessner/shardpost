//go:build tentacle

package cephstore

import (
	"testing"
	"time"

	"shardpost/internal/attcache"
	"shardpost/internal/metrics"
	"shardpost/internal/model"
)

// purgeStore is a store whose purge works from the data model (listing
// says whether it lists instead).
func purgeStore(t *testing.T, listing bool) *Store {
	t.Helper()

	return storeWith(t, func(c *Config) {
		c.PurgeListing = listing
		c.OrphanSource = model.OrphanSourceShadow
	})
}

// populateAll fills an account with something of every kind the store
// keeps, including what crashes leave behind with their intents.
func populateAll(t *testing.T, u *User) {
	t.Helper()

	inbox := mustMailbox(t, u, "INBOX")
	mustMailbox(t, u, "Archive")

	metas := deliverN(t, u, "INBOX", 6, 300) // two shards
	deliverN(t, u, "Archive", 1, 3000)       // three chunks

	if _, err := u.Copy(metas[:2], "Archive"); err != nil {
		t.Fatal(err)
	}

	populateCrashes(t, u, inbox, metas)
	populateObjects(t, u)

	// A listing scrub leaves its candidates and its quota count in the
	// scrub object.
	if _, err := u.Scrub(ScrubOptions{}); err != nil {
		t.Fatal(err)
	}
}

// populateCrashes leaves the intents of crashed writes: a removal whose
// release did not happen, a delivery, an import step, a creation, a
// deletion and a MOVE that stopped half way, and an upload.
func populateCrashes(t *testing.T, u *User, inbox *Mailbox, metas []Meta) {
	t.Helper()

	if _, _, _, err := u.unlink(inbox, metas[5:6], ""); err != nil {
		t.Fatal(err)
	}

	crashedDelivery(t, u, inbox)

	more := []model.ImportMessage{{UID: 100, Raw: testMessage("import", 300)}}
	salt, guids := importGUIDs(more)

	if _, err := u.reserveWith(inbox, Quota{}, map[string][]byte{regImportBlobPrefix + "b": u.importIntent(inbox.ID, salt, guids)}); err != nil {
		t.Fatal(err)
	}

	if _, err := u.prepareImport(more, guids); err != nil {
		t.Fatal(err)
	}

	rec := MailboxRec{ID: newGUID(), UIDValidity: 3}
	if err := u.createIntended(&rec, "Lost", u.shardSize); err != nil {
		t.Fatal(err)
	}

	gone := mustMailbox(t, u, "Gone")
	deliverN(t, u, "Gone", 5, 300)
	crashedDelete(t, u, gone)

	intent := moveIntent{ID: newGUID(), Dest: "x", GUIDs: []string{metas[0].GUID}, UIDs: []uint32{metas[0].UID}}
	if err := casOmap(u, u.idx, mboxOID(inbox.ID), nil, func(map[string][]byte) (map[string][]byte, []string) {
		return map[string][]byte{keyIntentPrefix + intent.ID: encode(intent)}, nil
	}); err != nil {
		t.Fatal(err)
	}

	if _, err := u.PutBlob(testMessage("upload", 2000)); err != nil {
		t.Fatal(err)
	}
}

// populateObjects writes the other objects of an account: METADATA, a
// Sieve script with a vacation record, private \Seen marks of a public
// folder, the front-end index, the trace switch.
func populateObjects(t *testing.T, u *User) {
	t.Helper()

	note := "x"
	steps := []func() error{
		func() error { return u.SetMetadata("INBOX", "", map[string]*string{"/shared/comment": &note}) },
		func() error { return u.PutScript("s", "keep;") },
		func() error { return u.SetActiveScript("s") },
		func() error { _, err := u.VacationSeen("k", time.Hour); return err },
		func() error { return u.SetPrivateSeen(newGUID(), 1, []uint32{1}, true, 1) },
		func() error { return u.SetPublicSubscribed(newGUID(), true) },
		func() error { return u.SetTrace(model.TraceState{Until: time.Now().Add(time.Hour)}) },
		func() error {
			return u.IndexApply(attcache.Space, []string{"a:x"}, func(_ []string, _ map[string][]byte) (map[string][]byte, []string) {
				return map[string][]byte{"a:x": []byte("1")}, nil
			})
		},
	}

	for i, step := range steps {
		if err := step(); err != nil {
			t.Fatalf("step %d: %v", i, err)
		}
	}
}

// legacyLiveMarks writes \Seen marks of a live public folder the way
// servers before 0.42.0 did (no ps: key) and returns the folder id.
func legacyLiveMarks(t *testing.T, s *Store, u *User) string {
	t.Helper()

	pub, err := s.User(model.PublicNamespace)
	if err != nil {
		t.Fatal(err)
	}
	defer pub.Close()

	folder := mustMailbox(t, pub, "marks-"+newGUID()[:8])

	if err := casOmap(u, u.idx, oidPrivateSeen+folder.ID, nil, func(map[string][]byte) (map[string][]byte, []string) {
		return map[string][]byte{pseenKey(folder.UIDValidity, 1): []byte("1:1")}, nil
	}); err != nil {
		t.Fatal(err)
	}

	return folder.ID
}

// TestPurgeLeavesNoObject: a purge from the data model deletes every
// object of a fully populated account, debris of crashes with intents
// included, without a listing.
func TestPurgeLeavesNoObject(t *testing.T) {
	s := purgeStore(t, false)
	u := testUser(t, s)
	populateAll(t, u)
	legacyLiveMarks(t, s, u)

	before := len(objects(t, u.idx)) + len(objects(t, u.data))

	listed := counterValue(t, metrics.RadosListings.WithLabelValues(listPurgeObjects))

	n, err := u.PurgeNamespace(func() error { return nil })
	if err != nil {
		t.Fatal(err)
	}

	if got := counterValue(t, metrics.RadosListings.WithLabelValues(listPurgeObjects)); got != listed {
		t.Fatalf("the purge listed (%v -> %v)", listed, got)
	}

	left := append(objects(t, u.idx), objects(t, u.data)...)
	if len(left) > 0 {
		t.Fatalf("%d of %d objects left: %v", len(left), before, left)
	}

	if n != before {
		t.Errorf("purge counted %d objects, there were %d", n, before)
	}
}

// TestPurgeListingReportsUnnamed: with purge.listing the purge lists as
// before and reports what the data model does not name (debris from
// before the intents).
func TestPurgeListingReportsUnnamed(t *testing.T) {
	u := testUser(t, purgeStore(t, true))
	populateAll(t, u)
	_, legacyMbox, legacyMarks := legacyDebris(t, u)

	unnamed := counterValue(t, metrics.PurgeUnnamed)

	if _, err := u.PurgeNamespace(func() error { return nil }); err != nil {
		t.Fatal(err)
	}

	// The legacy blob, its mailbox object and its marks object.
	if got := counterValue(t, metrics.PurgeUnnamed); got != unnamed+3 {
		t.Fatalf("unnamed objects %v, want %v (%s, %s and a blob)", got-unnamed, 3, legacyMbox, legacyMarks)
	}

	if left := append(objects(t, u.idx), objects(t, u.data)...); len(left) > 0 {
		t.Fatalf("objects left: %v", left)
	}
}

// TestPurgeRestartAfterRegistry: a purge that starts again after it
// deleted the registry (its lock session created an empty one) finds
// nothing more and deletes that registry.
func TestPurgeRestartAfterRegistry(t *testing.T) {
	u := testUser(t, purgeStore(t, false))
	populateAll(t, u)

	if _, err := u.PurgeNamespace(func() error { return nil }); err != nil {
		t.Fatal(err)
	}

	if _, err := u.ImportLock("job-1", time.Minute); err != nil {
		t.Fatal(err)
	}

	if _, err := u.PurgeNamespace(func() error { return nil }); err != nil {
		t.Fatal(err)
	}

	if left := objects(t, u.idx); len(left) > 0 {
		t.Fatalf("objects left: %v", left)
	}
}
