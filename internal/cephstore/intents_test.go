//go:build tentacle

package cephstore

import (
	"strings"
	"testing"
	"time"

	"shardpost/internal/model"
)

// Intents (intents.go): every path sets its intent and removes it when
// it is done; a crash in between leaves it for the scrubber, which
// settles it without a listing (scrubintents.go).

// intentsStore is a store whose scrub works from the intents and takes
// every intent as old.
func intentsStore(t *testing.T, source model.OrphanSource) *Store {
	t.Helper()

	return storeWith(t, func(c *Config) {
		c.OrphanSource = source
		c.IntentGrace = time.Nanosecond
	})
}

// scrubNow scrubs with no grace period (everything counts as old).
func scrubNow(t *testing.T, u *User) *ScrubReport {
	t.Helper()

	// Object mtimes have a resolution of a second here and there; make
	// sure what the test wrote is in the past.
	time.Sleep(1100 * time.Millisecond)

	rep, err := u.Scrub(ScrubOptions{})
	if err != nil {
		t.Fatal(err)
	}

	if len(rep.Errors) > 0 {
		t.Fatalf("scrub errors: %v", rep.Errors)
	}

	return rep
}

// mustMailbox creates a mailbox.
func mustMailbox(t *testing.T, u *User, name string) *Mailbox {
	t.Helper()

	if err := u.CreateMailbox(name, nil); err != nil {
		t.Fatal(err)
	}

	mb, err := u.Lookup(name)
	if err != nil {
		t.Fatal(err)
	}

	return mb
}

// deliverN delivers n messages of about size bytes and removes their
// finished blob intents.
func deliverN(t *testing.T, u *User, name string, n, size int) []Meta {
	t.Helper()

	var out []Meta

	for i := range n {
		m, err := u.Deliver(name, testMessage("message "+strings.Repeat("x", i), size), nil, time.Time{})
		if err != nil {
			t.Fatal(err)
		}

		out = append(out, *m)
	}

	u.flushIntents(false)

	return out
}

// deliverOne delivers one message and leaves its finished intent queued.
func deliverOne(t *testing.T, u *User, name string) Meta {
	t.Helper()

	m, err := u.Deliver(name, testMessage("one", 500), nil, time.Time{})
	if err != nil {
		t.Fatal(err)
	}

	return *m
}

// registryKeys returns the registry keys with the prefix.
func registryKeys(t *testing.T, u *User, prefix string) []string {
	t.Helper()

	var out []string
	for k := range omapKeys(t, u, oidRegistry, prefix) {
		out = append(out, k)
	}

	return out
}

// blobExistsT reports whether chunk 0 of the blob exists.
func blobExistsT(u *User, guid string) bool { return u.blobExists(guid) }

// refsOf reads the reference count of a blob.
func refsOf(t *testing.T, u *User, guid string) string {
	t.Helper()

	v, _, err := u.xattrVer(u.data, chunkOID(guid, 0), xattrRefs)
	if err != nil {
		t.Fatalf("refs of %s: %v", guid, err)
	}

	return string(v)
}

func TestDeliverBlobIntent(t *testing.T) {
	u := testUser(t, testStore(t))
	mustMailbox(t, u, "INBOX")

	m := deliverOne(t, u, "INBOX")

	// Set in the reservation, queued for removal after the link.
	if keys := registryKeys(t, u, regBlobPrefix); len(keys) != 1 || keys[0] != blobIntentKey(m.GUID) {
		t.Fatalf("blob intents after the delivery: %v", keys)
	}

	// The next delivery's reservation carries the removal along.
	m2 := deliverOne(t, u, "INBOX")
	if keys := registryKeys(t, u, regBlobPrefix); len(keys) != 1 || keys[0] != blobIntentKey(m2.GUID) {
		t.Fatalf("blob intents after the second delivery: %v", keys)
	}

	u.flushIntents(false)

	if keys := registryKeys(t, u, regBlobPrefix); len(keys) != 0 {
		t.Fatalf("blob intents after the flush: %v", keys)
	}
}

func TestEndDeliveryDropsBlobIntents(t *testing.T) {
	u := testUser(t, testStore(t))
	mustMailbox(t, u, "INBOX")
	mustMailbox(t, u, "Other")

	id, err := u.BeginDelivery()
	if err != nil {
		t.Fatal(err)
	}

	for _, name := range []string{"INBOX", "Other"} {
		guid := newGUID()
		if err := u.RecordDelivery(id, name, guid); err != nil {
			t.Fatal(err)
		}

		if _, err := u.DeliverGUID(name, guid, testMessage("multi", 300), nil, time.Time{}); err != nil {
			t.Fatal(err)
		}
	}

	if err := u.EndDelivery(id); err != nil {
		t.Fatal(err)
	}

	if keys := registryKeys(t, u, regBlobPrefix); len(keys) != 0 {
		t.Fatalf("blob intents after EndDelivery: %v", keys)
	}

	if keys := registryKeys(t, u, regDeliveryPrefix); len(keys) != 0 {
		t.Fatalf("delivery intents after EndDelivery: %v", keys)
	}
}

func TestCloseFlushesIntents(t *testing.T) {
	s := testStore(t)
	name := testAccount(t)

	u, err := s.User(name)
	if err != nil {
		t.Fatal(err)
	}

	mustMailbox(t, u, "INBOX")
	deliverN(t, u, "INBOX", 1, 200)
	u.Close()

	v, err := s.User(name)
	if err != nil {
		t.Fatal(err)
	}
	defer v.Close()

	deadline := time.Now().Add(10 * time.Second)
	for len(registryKeys(t, v, regBlobPrefix)) > 0 {
		if time.Now().After(deadline) {
			t.Fatal("Close did not remove the finished blob intent")
		}

		time.Sleep(50 * time.Millisecond)
	}
}

func TestExpungeRemovalIntents(t *testing.T) {
	u := testUser(t, testStore(t))
	mb := mustMailbox(t, u, "INBOX")

	// Shard size 4: UIDs 1-4 on shard 0, 5-6 on shard 1.
	metas := deliverN(t, u, "INBOX", 6, 2500)

	if err := u.Expunge("INBOX", []Meta{metas[1], metas[5]}); err != nil {
		t.Fatal(err)
	}

	if keys := omapKeys(t, u, mboxOID(mb.ID), keyRemovePrefix); len(keys) != 0 {
		t.Fatalf("removal intents after the expunge: %v", keys)
	}

	for _, m := range []Meta{metas[1], metas[5]} {
		if blobExistsT(u, m.GUID) {
			t.Errorf("blob of uid %d still there", m.UID)
		}
	}

	// MOVE removes from the source the same way.
	mustMailbox(t, u, "Archive")

	if _, err := u.Move("INBOX", []Meta{metas[0], metas[4]}, "Archive"); err != nil {
		t.Fatal(err)
	}

	if keys := omapKeys(t, u, mboxOID(mb.ID), keyRemovePrefix); len(keys) != 0 {
		t.Fatalf("removal intents after the move: %v", keys)
	}

	if got := refsOf(t, u, metas[0].GUID); got != "1" {
		t.Errorf("refs after the move %s, want 1", got)
	}
}

// TestRemovalCrashRecount: an expunge that died between the removal of
// the entry and the release of its blob leaves "r:"; the scrub counts the
// references anew instead of releasing again, so a release that did
// happen is never repeated.
func TestRemovalCrashRecount(t *testing.T) {
	u := testUser(t, intentsStore(t, model.OrphanSourceIntents))
	a := mustMailbox(t, u, "INBOX")
	mustMailbox(t, u, "Copy")

	m := deliverN(t, u, "INBOX", 1, 300)[0]

	copied, err := u.Copy([]Meta{m}, "Copy")
	if err != nil {
		t.Fatal(err)
	}

	// Crash 1: entry removed, reference not released (refs 2, entries 1).
	if _, err := u.unlink(a, []Meta{m}, "", true); err != nil {
		t.Fatal(err)
	}

	if keys := omapKeys(t, u, mboxOID(a.ID), keyRemovePrefix); len(keys) != 1 {
		t.Fatalf("removal intents after the crash: %v", keys)
	}

	rep := scrubNow(t, u)
	if rep.RefsFixed != 1 || refsOf(t, u, m.GUID) != "1" {
		t.Fatalf("refs fixed %d, refs %s; want 1, 1", rep.RefsFixed, refsOf(t, u, m.GUID))
	}

	if keys := omapKeys(t, u, mboxOID(a.ID), keyRemovePrefix); len(keys) != 0 {
		t.Fatalf("removal intent not dropped: %v", keys)
	}

	// Crash 2: the copy is removed and released, the intent stays. The
	// scrub must not release again: the blob is gone (refs reached 0).
	b, _ := u.Lookup("Copy")

	ures, err := u.unlink(b, copied, "", true)
	if err != nil || len(ures.removed) != 1 {
		t.Fatalf("unlink: %v, %d removed", err, len(ures.removed))
	}

	removed := ures.removed

	if err := u.releaseBlob(&removed[0]); err != nil {
		t.Fatal(err)
	}

	rep = scrubNow(t, u)
	if rep.OrphanBlobs != 0 || rep.IntentsDropped != 1 {
		t.Fatalf("orphans %d, dropped %d; want 0, 1", rep.OrphanBlobs, rep.IntentsDropped)
	}
}

// TestRemovalCrashNoDoubleDecrement: the release happened, the intent
// stayed, another copy still references the blob; the scrub leaves the
// count alone.
func TestRemovalCrashNoDoubleDecrement(t *testing.T) {
	u := testUser(t, intentsStore(t, model.OrphanSourceIntents))
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

	removed := ures.removed

	if err := u.releaseBlob(&removed[0]); err != nil {
		t.Fatal(err)
	}

	rep := scrubNow(t, u)
	if rep.RefsFixed != 0 || refsOf(t, u, m.GUID) != "1" || !blobExistsT(u, m.GUID) {
		t.Fatalf("refs fixed %d, refs %s, exists %v; want 0, 1, true", rep.RefsFixed, refsOf(t, u, m.GUID), blobExistsT(u, m.GUID))
	}
}

// TestRemovalCrashLastReference: the only entry was removed and the
// release never happened; the scrub finds the blob unreferenced and
// deletes it.
func TestRemovalCrashLastReference(t *testing.T) {
	u := testUser(t, intentsStore(t, model.OrphanSourceIntents))
	a := mustMailbox(t, u, "INBOX")

	m := deliverN(t, u, "INBOX", 1, 2500)[0]
	if _, err := u.unlink(a, []Meta{m}, "", true); err != nil {
		t.Fatal(err)
	}

	rep := scrubNow(t, u)
	if rep.OrphanBlobs != 1 || blobExistsT(u, m.GUID) {
		t.Fatalf("orphans %d, exists %v; want 1, false", rep.OrphanBlobs, blobExistsT(u, m.GUID))
	}

	if keys := omapKeys(t, u, mboxOID(a.ID), keyRemovePrefix); len(keys) != 0 {
		t.Fatalf("removal intent not dropped: %v", keys)
	}
}

func TestBlobIntentCrash(t *testing.T) {
	u := testUser(t, intentsStore(t, model.OrphanSourceIntents))
	mb := mustMailbox(t, u, "INBOX")

	// A delivery that died after writing its blob: intent and blob, no
	// entry.
	guid := newGUID()
	raw := testMessage("crash", 2500)

	if _, err := u.reserveWith(mb, Quota{Bytes: int64(len(raw)), Msgs: 1},
		map[string][]byte{blobIntentKey(guid): u.newBlobIntent(int64(len(raw)), mb.ID)}); err != nil {
		t.Fatal(err)
	}

	if _, err := u.writeBlob(guid, raw); err != nil {
		t.Fatal(err)
	}

	// A delivery that linked but died before its intent went.
	linked := deliverN(t, u, "INBOX", 1, 200)[0]
	if err := u.setIntent(map[string][]byte{blobIntentKey(linked.GUID): u.newBlobIntent(linked.Size, mb.ID)}); err != nil {
		t.Fatal(err)
	}

	rep := scrubNow(t, u)
	if rep.OrphanBlobs != 1 || blobExistsT(u, guid) {
		t.Fatalf("orphans %d, exists %v; want 1, false", rep.OrphanBlobs, blobExistsT(u, guid))
	}

	if !blobExistsT(u, linked.GUID) {
		t.Fatal("the linked blob was deleted")
	}

	if keys := registryKeys(t, u, regBlobPrefix); len(keys) != 0 {
		t.Fatalf("blob intents after the scrub: %v", keys)
	}
}

func TestDeleteMailboxTombstone(t *testing.T) {
	u := testUser(t, testStore(t))
	mb := mustMailbox(t, u, "Trash")
	metas := deliverN(t, u, "Trash", 6, 300)

	if err := u.DeleteMailbox("Trash"); err != nil {
		t.Fatal(err)
	}

	if keys := registryKeys(t, u, regDeletePrefix); len(keys) != 0 {
		t.Fatalf("tombstones after the delete: %v", keys)
	}

	for s := uint32(0); s <= 1; s++ {
		if u.objectExists(u.shardOID(mb.ID, s)) {
			t.Errorf("shard %d left", s)
		}
	}

	for _, m := range metas {
		if blobExistsT(u, m.GUID) {
			t.Errorf("blob of uid %d left", m.UID)
		}
	}
}

// TestDeleteMailboxCrash: a delete that died after unregistering leaves
// its tombstone; the scrub releases what the shards hold and deletes them.
func TestDeleteMailboxCrash(t *testing.T) {
	u := testUser(t, intentsStore(t, model.OrphanSourceIntents))
	mb := mustMailbox(t, u, "Trash")
	mustMailbox(t, u, "Keep")

	metas := deliverN(t, u, "Trash", 6, 300)
	if _, err := u.Copy(metas[:1], "Keep"); err != nil { // one blob is shared
		t.Fatal(err)
	}

	crashedDelete(t, u, mb)

	rep := scrubNow(t, u)
	if rep.DeletionsFinished != 1 || rep.OrphanBlobs != 5 {
		t.Fatalf("deletions %d, orphans %d; want 1, 5", rep.DeletionsFinished, rep.OrphanBlobs)
	}

	if !blobExistsT(u, metas[0].GUID) || refsOf(t, u, metas[0].GUID) != "1" {
		t.Fatalf("the shared blob: exists %v, refs %s", blobExistsT(u, metas[0].GUID), refsOf(t, u, metas[0].GUID))
	}

	assertDeleted(t, u, mb)
}

// crashedDelete leaves what a DELETE that died after unregistering and
// retiring the mailbox leaves.
func crashedDelete(t *testing.T, u *User, mb *Mailbox) {
	t.Helper()

	info, _, err := u.readHeader(mb)
	if err != nil {
		t.Fatal(err)
	}

	if _, lost, err := u.unregisterMailbox(mb, &lease{}, info, ""); err != nil || lost {
		t.Fatalf("unregister: %v, lost %v", err, lost)
	}

	if _, _, err := u.retireDeleted(mb, info); err != nil {
		t.Fatal(err)
	}
}

// assertDeleted checks that nothing of a deleted mailbox is left: no
// shard (a listing, which a test may do), no tombstone.
func assertDeleted(t *testing.T, u *User, mb *Mailbox) {
	t.Helper()

	for _, oid := range objects(t, u.idx) {
		if strings.HasPrefix(oid, mboxOID(mb.ID)) {
			t.Fatalf("object %s of the deleted mailbox left", oid)
		}
	}

	if keys := registryKeys(t, u, regDeletePrefix); len(keys) != 0 {
		t.Fatalf("tombstones left: %v", keys)
	}
}

func TestCreateMailboxIntent(t *testing.T) {
	u := testUser(t, intentsStore(t, model.OrphanSourceIntents))
	mustMailbox(t, u, "INBOX")

	if keys := registryKeys(t, u, regCreatePrefix); len(keys) != 0 {
		t.Fatalf("creation intents after CREATE: %v", keys)
	}

	// A creation that died before it registered its name.
	rec := MailboxRec{ID: newGUID(), UIDValidity: 1}
	if err := u.createIntended(&rec, "Lost", u.shardSize); err != nil {
		t.Fatal(err)
	}

	rep := scrubNow(t, u)
	if rep.CreationsUndone != 1 || u.objectExists(mboxOID(rec.ID)) {
		t.Fatalf("creations undone %d, object left %v", rep.CreationsUndone, u.objectExists(mboxOID(rec.ID)))
	}

	if keys := registryKeys(t, u, regCreatePrefix); len(keys) != 0 {
		t.Fatalf("creation intents after the scrub: %v", keys)
	}
}

func TestImportBlobIntent(t *testing.T) {
	u := testUser(t, intentsStore(t, model.OrphanSourceIntents))
	mb := mustMailbox(t, u, "INBOX")

	msgs := []model.ImportMessage{{UID: 1, Raw: testMessage("one", 300)}, {UID: 2, Raw: testMessage("two", 300)}}
	if _, err := u.ImportMessages(mb.ID, msgs); err != nil {
		t.Fatal(err)
	}

	if keys := registryKeys(t, u, regImportBlobPrefix); len(keys) != 0 {
		t.Fatalf("import intents after the step: %v", keys)
	}

	// A step that died after writing its blobs.
	more := []model.ImportMessage{{UID: 3, Raw: testMessage("three", 300)}}
	salt, guids := importGUIDs(more)

	if _, err := u.reserveWith(mb, Quota{Bytes: 300, Msgs: 1},
		map[string][]byte{regImportBlobPrefix + "batch": u.importIntent(mb.ID, salt, guids)}); err != nil {
		t.Fatal(err)
	}

	if _, err := u.prepareImport(more, guids); err != nil {
		t.Fatal(err)
	}

	rep := scrubNow(t, u)
	if rep.OrphanBlobs != 1 || blobExistsT(u, guids[0][0]) {
		t.Fatalf("orphans %d, blob left %v", rep.OrphanBlobs, blobExistsT(u, guids[0][0]))
	}

	if keys := registryKeys(t, u, regImportBlobPrefix); len(keys) != 0 {
		t.Fatalf("import intents after the scrub: %v", keys)
	}
}

func TestUploadExpires(t *testing.T) {
	s := testStore(t)
	u := testUser(t, s)

	guid, err := u.PutBlob(testMessage("upload", 2500))
	if err != nil {
		t.Fatal(err)
	}

	if keys := registryKeys(t, u, regUploadPrefix); len(keys) != 1 || !queued(t, s, uploadEntryID(u.name, guid)) {
		t.Fatalf("upload intents %v, or no queue entry", keys)
	}

	// Not due yet.
	if done, _, err := u.ExpireUpload(guid, time.Now()); err != nil || done {
		t.Fatalf("expired early: %v %v", done, err)
	}

	n, err := s.ExpireUploads(time.Now().Add(DefaultUploadTTL+time.Minute), func() bool { return false })
	if err != nil || n < 1 {
		t.Fatalf("expired %d: %v", n, err)
	}

	if blobExistsT(u, guid) || len(registryKeys(t, u, regUploadPrefix)) != 0 {
		t.Fatal("upload left after its expiry")
	}

	if queued(t, s, uploadEntryID(u.name, guid)) {
		t.Fatal("queue entry left")
	}
}

// queued reports whether the uploads queue has an entry of id.
func queued(t *testing.T, s *Store, id string) bool {
	t.Helper()

	q, err := s.uploadsQueue()
	if err != nil {
		t.Fatal(err)
	}

	all, err := q.All()
	if err != nil {
		t.Fatal(err)
	}

	for _, e := range all {
		if e.ID == id {
			return true
		}
	}

	return false
}

func TestPubSeenIntent(t *testing.T) {
	u := testUser(t, intentsStore(t, model.OrphanSourceIntents))
	gone := newGUID() // a public folder that does not exist (any more)

	if err := u.SetPrivateSeen(gone, 7, []uint32{1, 2}, true, 3); err != nil {
		t.Fatal(err)
	}

	if keys := registryKeys(t, u, regPubSeenPrefix); len(keys) != 1 {
		t.Fatalf("pubseen intents: %v", keys)
	}

	rep := scrubNow(t, u)
	if rep.StaleMarkObjects != 1 || u.objectExists(oidPrivateSeen+gone) {
		t.Fatalf("stale marks %d, object left %v", rep.StaleMarkObjects, u.objectExists(oidPrivateSeen+gone))
	}

	if keys := registryKeys(t, u, regPubSeenPrefix); len(keys) != 0 {
		t.Fatalf("pubseen intents after the scrub: %v", keys)
	}
}

func TestScrubPendingSeesIntents(t *testing.T) {
	u := testUser(t, testStore(t))
	mb := mustMailbox(t, u, "INBOX")
	m := deliverOne(t, u, "INBOX")

	if p, err := u.ScrubPending(); err != nil || !strings.Contains(p, regBlobPrefix) {
		t.Fatalf("pending %q, %v; want the blob intent", p, err)
	}

	u.flushIntents(false)

	if p, err := u.ScrubPending(); err != nil || p != "" {
		t.Fatalf("pending %q, %v; want nothing", p, err)
	}

	if _, err := u.unlink(mb, []Meta{m}, "", true); err != nil {
		t.Fatal(err)
	}

	if p, err := u.ScrubPending(); err != nil || !strings.Contains(p, keyRemovePrefix) {
		t.Fatalf("pending %q, %v; want the removal intent", p, err)
	}
}

func TestStaleMetadata(t *testing.T) {
	u := testUser(t, testStore(t))
	mustMailbox(t, u, "INBOX")
	mustMailbox(t, u, "Gone")

	v := "note"
	for _, name := range []string{"INBOX", "Gone"} {
		if err := u.SetMetadata(name, "", map[string]*string{"/shared/comment": &v}); err != nil {
			t.Fatal(err)
		}
	}

	if err := u.DeleteMailbox("Gone"); err != nil {
		t.Fatal(err)
	}

	rep := scrubNow(t, u)
	if rep.StaleMetadata != 1 {
		t.Fatalf("stale metadata %d, want 1", rep.StaleMetadata)
	}

	if got, err := u.GetMetadata("INBOX", ""); err != nil || got["/shared/comment"] != "note" {
		t.Fatalf("metadata of INBOX: %v, %v", got, err)
	}

	if keys := omapKeys(t, u, oidMetadata, ""); len(keys) != 1 {
		t.Fatalf("metadata keys left: %v", keys)
	}
}
