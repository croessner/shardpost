//go:build tentacle

package cephstore

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"

	"shardpost/internal/metrics"
	"shardpost/internal/model"
)

// counterValue reads a counter.
func counterValue(t *testing.T, c prometheus.Counter) float64 {
	t.Helper()

	var m dto.Metric
	if err := c.Write(&m); err != nil {
		t.Fatal(err)
	}

	return m.GetCounter().GetValue()
}

// crashedDelivery leaves what a delivery that died after its blob write
// leaves: the intent and the blob, no entry.
func crashedDelivery(t *testing.T, u *User, mb *Mailbox) string {
	t.Helper()

	guid := newGUID()
	raw := testMessage("crashed", 300)

	if _, err := u.reserveWith(mb, Quota{}, map[string][]byte{blobIntentKey(guid): u.newBlobIntent(int64(len(raw)), mb.ID)}); err != nil {
		t.Fatal(err)
	}

	if _, err := u.writeBlob(guid, raw); err != nil {
		t.Fatal(err)
	}

	return guid
}

// legacyDebris leaves what a crash before intents existed left: a blob,
// a mailbox object and a mark object nothing points at.
func legacyDebris(t *testing.T, u *User) (guid, mbox, marks string) {
	t.Helper()

	guid = newGUID()
	if _, err := u.writeBlob(guid, testMessage("legacy", 300)); err != nil {
		t.Fatal(err)
	}

	rec := MailboxRec{ID: newGUID(), UIDValidity: 1}
	if err := u.createIndexObjectSized(&rec, u.shardSize); err != nil {
		t.Fatal(err)
	}

	marks = newGUID()
	if err := casOmap(u, u.idx, oidPrivateSeen+marks, nil, func(map[string][]byte) (map[string][]byte, []string) {
		return map[string][]byte{pseenKey(1, 1): []byte("1:1")}, nil
	}); err != nil {
		t.Fatal(err)
	}

	return guid, mboxOID(rec.ID), oidPrivateSeen + marks
}

// TestShadowComparesListingWithIntents: the listing stays authoritative;
// what it finds that no intent names is reported, the debris of crashed
// writes with intents is not.
func TestShadowComparesListingWithIntents(t *testing.T) {
	u := testUser(t, intentsStore(t, model.OrphanSourceShadow))
	mb := mustMailbox(t, u, "INBOX")

	missed := counterValue(t, metrics.ScrubShadowDiff.WithLabelValues("orphan_missed"))

	crashed := crashedDelivery(t, u, mb)
	legacy, legacyMbox, legacyMarks := legacyDebris(t, u)

	rec := MailboxRec{ID: newGUID(), UIDValidity: 2}
	if err := u.createIntended(&rec, "Lost", u.shardSize); err != nil {
		t.Fatal(err)
	}

	if err := u.SetPrivateSeen(newGUID(), 1, []uint32{1}, true, 1); err != nil {
		t.Fatal(err)
	}

	rep := scrubNow(t, u)
	checkShadow(t, rep.Shadow)

	if got := counterValue(t, metrics.ScrubShadowDiff.WithLabelValues("orphan_missed")); got != missed+1 {
		t.Fatalf("orphan_missed counter %v, want %v", got, missed+1)
	}

	// The listing acted: two orphan candidates (deleted next run), both
	// unknown objects and both mark objects removed.
	if rep.OrphanCandidates != 2 || rep.UnknownRemoved != 2 || rep.StaleMarkObjects != 2 {
		t.Fatalf("candidates %d, unknown removed %d, stale marks %d; want 2, 2, 2", rep.OrphanCandidates, rep.UnknownRemoved, rep.StaleMarkObjects)
	}

	if u.objectExists(legacyMbox) || u.objectExists(legacyMarks) {
		t.Fatal("legacy debris left")
	}

	rep = scrubNow(t, u)
	if rep.OrphanBlobs != 2 || blobExistsT(u, crashed) || blobExistsT(u, legacy) {
		t.Fatalf("second run: orphans %d; want both blobs gone", rep.OrphanBlobs)
	}

	// Once what they name is gone, the intents go.
	scrubNow(t, u)
	noIntents(t, u, regBlobPrefix, regCreatePrefix, regPubSeenPrefix)
}

// checkShadow expects one missed object of each kind and nothing extra.
func checkShadow(t *testing.T, s *ShadowReport) {
	t.Helper()

	if s == nil {
		t.Fatal("no shadow report")
	}

	if s.OrphansMissed != 1 || s.OrphansExtra != 0 || s.UnknownMissed != 1 || s.MarksMissed != 1 {
		t.Fatalf("shadow %+v; want one of each missed, no extra", *s)
	}
}

// noIntents fails when the registry holds a key with one of the
// prefixes.
func noIntents(t *testing.T, u *User, prefixes ...string) {
	t.Helper()

	for _, p := range prefixes {
		if keys := registryKeys(t, u, p); len(keys) != 0 {
			t.Errorf("intents %s left: %v", p, keys)
		}
	}
}

// TestIntentsModeDoesNotList: a scrub from the intents lists nothing.
func TestIntentsModeDoesNotList(t *testing.T) {
	u := testUser(t, intentsStore(t, model.OrphanSourceIntents))
	mb := mustMailbox(t, u, "INBOX")
	deliverN(t, u, "INBOX", 2, 300)
	crashedDelivery(t, u, mb)

	before := map[string]float64{}
	for _, site := range []string{listScrubOrphans, listScrubUnknown} {
		before[site] = counterValue(t, metrics.RadosListings.WithLabelValues(site))
	}

	rep := scrubNow(t, u)
	if rep.OrphanBlobs != 1 {
		t.Fatalf("orphans %d, want 1", rep.OrphanBlobs)
	}

	for site, n := range before {
		if got := counterValue(t, metrics.RadosListings.WithLabelValues(site)); got != n {
			t.Errorf("listing %s counted %v times, want %v", site, got, n)
		}
	}
}

// TestListingModeDropsSettledIntents: with the listing, the intents of
// debris the listing removed go once it is gone.
func TestListingModeDropsSettledIntents(t *testing.T) {
	u := testUser(t, intentsStore(t, model.OrphanSourceListing))
	mb := mustMailbox(t, u, "INBOX")
	guid := crashedDelivery(t, u, mb)

	scrubNow(t, u)
	scrubNow(t, u) // the two-run rule deletes the blob

	if blobExistsT(u, guid) {
		t.Fatal("blob not deleted by the listing")
	}

	if keys := registryKeys(t, u, regBlobPrefix); len(keys) != 0 {
		t.Fatalf("blob intents left: %v", keys)
	}
}

// TestDeleteMailboxLateDelivery: a delivery that slipped in between the
// read of the messages and the unregistration is released as well.
func TestDeleteMailboxLateDelivery(t *testing.T) {
	u := testUser(t, testStore(t))
	mb := mustMailbox(t, u, "Box")
	deliverN(t, u, "Box", 2, 300)

	info, metas, err := u.readForDelete(mb)
	if err != nil {
		t.Fatal(err)
	}

	late := deliverN(t, u, "Box", 3, 300) // UIDs 3-5, on shards 0 and 1

	if _, lost, err := u.unregisterMailbox(mb, &lease{}, info, ""); err != nil || lost {
		t.Fatalf("unregister: %v, lost %v", err, lost)
	}

	if _, err := u.destroyUnregistered(mb, info, metas, "Box"); err != nil {
		t.Fatal(err)
	}

	for _, m := range late {
		if blobExistsT(u, m.GUID) {
			t.Errorf("blob of the late uid %d left", m.UID)
		}
	}

	assertDeleted(t, u, mb)
}

// TestImportReplaceIntents: replacing a mailbox leaves neither the
// creation intent of the new object nor the tombstone of the old one.
func TestImportReplaceIntents(t *testing.T) {
	u := testUser(t, testStore(t))
	old := mustMailbox(t, u, "INBOX")

	mb, err := u.ImportMailbox(model.ImportMailbox{Name: "INBOX", UIDValidity: old.UIDValidity + 100})
	if err != nil {
		t.Fatal(err)
	}

	if mb.ID == old.ID || u.objectExists(mboxOID(old.ID)) {
		t.Fatalf("not replaced: id %s, old object left %v", mb.ID, u.objectExists(mboxOID(old.ID)))
	}

	for _, p := range []string{regCreatePrefix, regDeletePrefix} {
		if keys := registryKeys(t, u, p); len(keys) != 0 {
			t.Errorf("intents %s left: %v", p, keys)
		}
	}
}

// TestRetiredMailboxTombstone: a mailbox left retired by an interrupted
// replacement is unregistered with a tombstone, from which its object is
// removed (in the same run here: the intent grace is a nanosecond).
func TestRetiredMailboxTombstone(t *testing.T) {
	u := testUser(t, intentsStore(t, model.OrphanSourceIntents))
	mb := mustMailbox(t, u, "INBOX")

	if _, err := u.retireEmpty(mb, model.ImportMailbox{Name: "INBOX", UIDValidity: 9}); err != nil {
		t.Fatal(err)
	}

	first := scrubNow(t, u)
	if first.RetiredRepaired != 1 {
		t.Fatalf("retired repaired %d, want 1", first.RetiredRepaired)
	}

	second := scrubNow(t, u)
	if first.DeletionsFinished+second.DeletionsFinished != 1 || u.objectExists(mboxOID(mb.ID)) {
		t.Fatalf("deletions %d+%d, object left %v", first.DeletionsFinished, second.DeletionsFinished, u.objectExists(mboxOID(mb.ID)))
	}

	if keys := registryKeys(t, u, regDeletePrefix); len(keys) != 0 {
		t.Fatalf("tombstones left: %v", keys)
	}
}

// TestIntentsModeForgetsListingCandidates: after the switch from the
// listing, the candidates a listing run remembered go, so they do not
// keep the account from being idle.
func TestIntentsModeForgetsListingCandidates(t *testing.T) {
	u := testUser(t, intentsStore(t, model.OrphanSourceShadow))
	mustMailbox(t, u, "INBOX")
	legacyDebris(t, u)

	if rep := scrubNow(t, u); rep.OrphanCandidates != 1 {
		t.Fatalf("candidates %d, want 1", rep.OrphanCandidates)
	}

	v, err := intentsStore(t, model.OrphanSourceIntents).User(u.name)
	if err != nil {
		t.Fatal(err)
	}
	defer v.Close()

	scrubNow(t, v)

	if p, err := v.ScrubPending(); err != nil || p != "" {
		t.Fatalf("pending %q, %v; want nothing", p, err)
	}
}
