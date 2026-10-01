package cephstore

import (
	"bytes"
	"encoding/json"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"shardpost/internal/metrics"
	"shardpost/internal/model"
)

// The scrubber's view of the intents (scaling plan B6, intents.go).
//
// An intent older than the intent grace period belongs to a write that
// will not finish any more. What it names is settled against the index
// scan of the run:
//
//   - a blob (bi:, bimp:, up: once expired, the blob of an r:, every blob
//     of a deleted mailbox's entries) that an entry references is fine
//     and the intent goes; one that nothing references is an orphan and
//     is deleted (reference count set to 0 under the version read, then
//     unindexed and deleted), one whose chunk 0 is younger than the scrub
//     grace is left for the next run. A reference count is never
//     decremented on the strength of an intent: the scan counts every
//     reference anew (fixRefs), so a release that did happen before the
//     crash is not repeated.
//   - a deleted mailbox (dm:) whose blobs are settled loses its shards,
//     highest first, shard 0 last, then the tombstone; its range comes
//     from the retired header of shard 0 while that exists, otherwise
//     from the tombstone.
//   - a mailbox object of a creation that never registered its name
//     (ci:) is deleted.
//   - the private \Seen marks of a public folder that is gone (ps:) are
//     deleted.
//
// Which of this the scrub acts on depends on the orphan source
// (model.OrphanSource): with "intents" it does all of it and lists
// nothing; with "listing" and "shadow" the listing stays authoritative
// and the intents are only dropped once what they name is gone or
// referenced, and "shadow" reports where the two disagree.

// DefaultIntentGrace is how old an intent must be before a scrub takes it
// for the leftover of a crash, when the configuration does not say.
const DefaultIntentGrace = time.Hour

// blobCand is a blob some intent names.
type blobCand struct {
	size    int64
	chunks  int
	old     bool // named by an intent older than the grace period
	unindex bool // indexed for search (every blob but an upload)
	// verdict of settleBlobs
	resolved bool
	orphan   bool
}

// deletion is a mailbox deletion found by its tombstone.
type deletion struct {
	id   string
	old  bool
	info *MailboxInfo
	// guids are the blobs of its remaining entries and removal intents;
	// read is false when a shard could not be read.
	guids []string
	read  bool
}

// intentScan is what a scrub collects from the intents.
type intentScan struct {
	reg       registryIntents
	removals  map[string]map[string]removeIntent // mailbox id -> "r:" key -> intent
	blobs     map[string]*blobCand
	deletions []*deletion
	// coveredMbox are the mailbox ids an intent names (any age): their
	// objects are no unknown ones.
	coveredMbox map[string]bool
	// drop are the intent keys to remove, by object.
	drop map[string][]string
}

func newIntentScan() *intentScan {
	return &intentScan{removals: map[string]map[string]removeIntent{}, blobs: map[string]*blobCand{},
		coveredMbox: map[string]bool{}, drop: map[string][]string{}}
}

// blob records a blob named by an intent.
func (in *intentScan) blob(guid string, size int64, chunks int, old, unindex bool) {
	if !model.ValidGUID(guid) {
		return
	}

	c := in.blobs[guid]
	if c == nil {
		c = &blobCand{}
		in.blobs[guid] = c
	}

	c.size, c.chunks = max(c.size, size), max(c.chunks, chunks, 1)
	c.old = c.old || old
	c.unindex = c.unindex || unindex
}

// covers reports whether an intent names the blob.
func (in *intentScan) covers(guid string) bool { return in.blobs[guid] != nil }

// orphanSource is the configured orphan source.
func (u *User) orphanSource() model.OrphanSource {
	if u.store == nil {
		return model.OrphanSourceShadow
	}

	u.store.mu.RLock()
	src := u.store.cfg.OrphanSource
	u.store.mu.RUnlock()

	if !src.Valid() {
		return model.OrphanSourceShadow
	}

	return src
}

// intentGrace is the configured intent grace period.
func (u *User) intentGrace() time.Duration {
	if u.store == nil {
		return DefaultIntentGrace
	}

	u.store.mu.RLock()
	defer u.store.mu.RUnlock()

	return orDefault(u.store.cfg.IntentGrace, DefaultIntentGrace)
}

// gatherIntents reads the intents of the registry and of the deleted
// mailboxes and collects the blobs they name. The removal intents of the
// registered mailboxes were read by the index scan.
func (sr *scrubRun) gatherIntents() (*intentScan, error) {
	in := sr.intents

	reg, err := sr.u.readIntents(regBlobPrefix, regImportBlobPrefix, regUploadPrefix, regCreatePrefix, regDeletePrefix,
		regPubSeenPrefix, regDeliveryPrefix)
	if err != nil {
		return nil, err
	}

	in.reg = reg

	sr.gatherBlobIntents()
	sr.gatherRemovals()

	for id := range reg[regCreatePrefix] {
		in.coveredMbox[id] = true
	}

	for id, v := range reg[regDeletePrefix] {
		in.coveredMbox[id] = true

		if _, live := sr.byID[id]; live {
			in.drop[oidRegistry] = append(in.drop[oidRegistry], deleteKey(id)) // a re-created id: nothing to delete
			continue
		}

		sr.gatherDeletion(id, v)
	}

	return in, nil
}

// gatherBlobIntents collects the blobs of bi:, bimp:, up: and d:.
func (sr *scrubRun) gatherBlobIntents() {
	in, now := sr.intents, time.Now()

	for guid, v := range in.reg[regBlobPrefix] {
		var bi blobIntent

		_ = json.Unmarshal(v, &bi)
		in.blob(guid, bi.Size, bi.Chunks, sr.oldIntent(time.Unix(bi.Time, 0)), true)
	}

	for _, v := range in.reg[regImportBlobPrefix] {
		var im importIntent

		_ = json.Unmarshal(v, &im)

		old := sr.oldIntent(time.Unix(im.Time, 0))
		for _, b := range im.Blobs {
			in.blob(b.GUID, b.Size, b.Chunks, old, true)
		}
	}

	for guid, v := range in.reg[regUploadPrefix] {
		var up blobIntent

		_ = json.Unmarshal(v, &up)
		// An upload is due when it expired; until then it is no orphan.
		in.blob(guid, up.Size, up.Chunks, up.Expires < now.Unix() && sr.oldIntent(time.Unix(up.Expires, 0)), false)
	}

	// The blobs of delivery intents are covered by their bi: (and rolled
	// back by rollbackDeliveries); they count for the comparison only.
	for _, v := range in.reg[regDeliveryPrefix] {
		var d deliveryIntent

		_ = json.Unmarshal(v, &d)

		for _, e := range d.Entries {
			in.blob(e.GUID, 0, 0, false, true)
		}
	}
}

// gatherRemovals collects the blobs of the removal intents the index
// scan read from the registered mailboxes.
func (sr *scrubRun) gatherRemovals() {
	in := sr.intents

	for _, keys := range in.removals {
		for _, ri := range keys {
			in.blob(ri.GUID, ri.Size, ri.Chunks, sr.oldIntent(time.Unix(ri.Time, 0)), true)
		}
	}
}

// oldIntent reports whether an intent set at t is past the grace period.
func (sr *scrubRun) oldIntent(t time.Time) bool { return !t.After(sr.icut) }

// gatherDeletion reads what is left of a deleted mailbox: its range (the
// retired header of shard 0, else the tombstone), the blobs of the
// entries on its shards and of the removal intents on shard 0.
func (sr *scrubRun) gatherDeletion(id string, v []byte) {
	u, in := sr.u, sr.intents

	var tomb mailboxIntent

	_ = json.Unmarshal(v, &tomb)

	d := &deletion{id: id, old: sr.oldIntent(time.Unix(tomb.Time, 0)), read: true}
	d.info = u.deletedRange(id, tomb)

	in.deletions = append(in.deletions, d)

	last := shardOf(max(d.info.UIDNext-1, 1), d.info.ShardSize)
	for s := uint32(0); s <= last; s++ {
		vals, err := u.idx.GetAllOmapValues(u.shardOID(id, s), "", keyMsgPrefix, omapPageSize)
		if err != nil && errCode(err) != syscall.ENOENT {
			sr.rep.Errors = append(sr.rep.Errors, fmt.Sprintf("deleted mailbox %s shard %d: %v", id, s, err))
			d.read = false

			continue
		}

		for _, raw := range vals {
			var m Meta
			if json.Unmarshal(raw, &m) == nil {
				d.guids = append(d.guids, m.GUID)
				in.blob(m.GUID, m.Size, m.Chunks, d.old, true)
			}
		}
	}

	rs, err := u.removeIntents(id)
	if err != nil {
		d.read = false
	}

	for _, ri := range rs {
		d.guids = append(d.guids, ri.GUID)
		in.blob(ri.GUID, ri.Size, ri.Chunks, d.old, true)
	}
}

// deletedRange is the range of a deleted mailbox: from its (retired)
// header while shard 0 exists, else from the tombstone.
func (u *User) deletedRange(id string, tomb mailboxIntent) *MailboxInfo {
	info := &MailboxInfo{Mailbox: Mailbox{Name: tomb.Name, MailboxRec: MailboxRec{ID: id}}, UIDNext: max(tomb.UIDNext, 1), ShardSize: tomb.ShardSize}

	hv, ver, err := u.getKeysVer(u.idx, mboxOID(id), []string{keyUIDNext, keyRetired, keyShardSize})
	if err == nil && ver != 0 {
		info.UIDNext = max(retiredUIDNext(hv), 1)

		if ss, _ := strconv.ParseUint(string(hv[keyShardSize]), 10, 32); ss > 0 {
			info.ShardSize = uint32(ss)
		}
	}

	if info.ShardSize == 0 {
		info.ShardSize = DefaultShardSize
	}

	return info
}

// settleIntents settles what the intents name (see the comment at the
// top); with act it repairs, otherwise it only drops what is settled.
func (sr *scrubRun) settleIntents(refs map[string][]Meta, act bool) {
	in := sr.intents

	sr.settleBlobs(refs, act)
	sr.settleDeletions(act)
	sr.settleCreations(act)
	sr.settleBlobKeys()
	sr.settlePubSeen(act)

	if sr.mayWrite() {
		for oid, keys := range in.drop {
			if err := rmKeys(sr.u.idx, oid, keys); err != nil {
				sr.rep.Errors = append(sr.rep.Errors, err.Error())
			} else {
				sr.rep.IntentsDropped += len(keys)
			}
		}
	}
}

// mayWrite reports whether the run may write at all (no dry run, the
// fence holds); dropping settled intents needs no more.
func (sr *scrubRun) mayWrite() bool {
	return !sr.o.DryRun && (sr.o.Fence == nil || sr.o.Fence() == nil)
}

// settleBlobs decides every blob an old intent names.
func (sr *scrubRun) settleBlobs(refs map[string][]Meta, act bool) {
	guids := make([]string, 0, len(sr.intents.blobs))
	for g := range sr.intents.blobs {
		guids = append(guids, g)
	}

	sort.Strings(guids)

	for _, guid := range guids {
		c := sr.intents.blobs[guid]
		if !c.old {
			continue
		}

		if len(refs[guid]) > 0 {
			c.resolved = true
			continue
		}

		sr.settleOrphan(guid, c, act)
	}
}

// settleOrphan decides a blob no entry references.
func (sr *scrubRun) settleOrphan(guid string, c *blobCand, act bool) {
	u := sr.u

	st, err := u.data.Stat(chunkOID(guid, 0))

	switch {
	case errCode(err) == syscall.ENOENT:
		// Never written, or deleted: what may be left of its other chunks
		// goes now with act, else with the listing.
		switch {
		case c.chunks <= 1 || !act:
			c.resolved = true
		case sr.o.Allowed(RepairOrphans):
			c.resolved = u.deleteBlobAll(guid, c.chunks)
		}

		return
	case err != nil:
		sr.rep.Errors = append(sr.rep.Errors, fmt.Sprintf("stat blob %s: %v", guid, err))
		return
	case st.ModTime.After(sr.cutoff):
		return // written or touched within the grace period: next run
	}

	c.orphan = true

	if !act {
		return
	}

	scrubFinding("orphan_blob")

	sr.rep.OrphanBlobs++

	sr.log.Warn("orphan blob (intent)", "guid", guid, "chunks", c.chunks)

	if !sr.o.Allowed(RepairOrphans) {
		return
	}

	gone, err := u.collectBlob(guid, c.size, c.chunks, c.unindex)
	if err != nil {
		sr.rep.Errors = append(sr.rep.Errors, err.Error())
	}

	c.resolved = gone
}

// settled reports whether every blob of guids is settled.
func (sr *scrubRun) settled(guids []string) bool {
	for _, g := range guids {
		if c := sr.intents.blobs[g]; c != nil && !c.resolved {
			return false
		}
	}

	return true
}

// settleDeletions finishes the deletions whose blobs are settled: with
// act it deletes the shards; otherwise a deletion is settled once the
// listing removed shard 0.
func (sr *scrubRun) settleDeletions(act bool) {
	for _, d := range sr.intents.deletions {
		if !d.old {
			continue
		}

		sr.rep.IntentsOpen++

		if !d.read || !sr.settled(d.guids) {
			continue
		}

		switch {
		case act && sr.o.Allowed(RepairUnknown):
			mb := Mailbox{Name: d.info.Name, MailboxRec: MailboxRec{ID: d.id}}
			if err := sr.u.deleteShards(&mb, d.info, d.id); err != nil {
				sr.rep.Errors = append(sr.rep.Errors, err.Error())
				continue
			}

			scrubFinding("deletion_finished")

			sr.rep.DeletionsFinished++
		case act || sr.u.objectExists(mboxOID(d.id)):
			continue
		}

		sr.intents.drop[oidRegistry] = append(sr.intents.drop[oidRegistry], deleteKey(d.id))
	}
}

// settleCreations removes the mailbox objects of creations that never
// registered their name (with act; otherwise the listing does). The
// intent of a registered mailbox is a leftover and just goes.
func (sr *scrubRun) settleCreations(act bool) {
	u, in := sr.u, sr.intents

	for id, v := range in.reg[regCreatePrefix] {
		_, live := sr.byID[id]

		switch {
		case live:
		case !sr.oldIntent(intentTime(v)):
			continue
		case act && sr.o.Allowed(RepairUnknown):
			sr.rep.IntentsOpen++

			if err := u.idx.Delete(mboxOID(id)); err != nil && errCode(err) != syscall.ENOENT {
				sr.rep.Errors = append(sr.rep.Errors, fmt.Sprintf("remove %s: %v", mboxOID(id), err))
				continue
			}

			scrubFinding("creation_undone")

			sr.rep.CreationsUndone++
		default:
			sr.rep.IntentsOpen++

			if act || u.objectExists(mboxOID(id)) {
				continue
			}
		}

		in.drop[oidRegistry] = append(in.drop[oidRegistry], createKey(id))
	}
}

// settleBlobKeys drops the old blob intents (bi:, bimp:, up:, r:) whose
// blobs are all settled.
func (sr *scrubRun) settleBlobKeys() {
	in := sr.intents

	for guid := range in.reg[regBlobPrefix] {
		sr.settleKey(oidRegistry, blobIntentKey(guid), in.blobs[guid])
	}

	for guid := range in.reg[regUploadPrefix] {
		sr.settleKey(oidRegistry, uploadKey(guid), in.blobs[guid])
	}

	for batch, v := range in.reg[regImportBlobPrefix] {
		var im importIntent

		_ = json.Unmarshal(v, &im)

		if sr.oldIntent(time.Unix(im.Time, 0)) {
			sr.rep.IntentsOpen++

			if sr.settled(importGUIDsOf(im)) {
				in.drop[oidRegistry] = append(in.drop[oidRegistry], regImportBlobPrefix+batch)
			}
		}
	}

	for id, keys := range in.removals {
		for key, ri := range keys {
			if ri.GUID == "" { // unreadable
				in.drop[mboxOID(id)] = append(in.drop[mboxOID(id)], key)
				continue
			}

			if sr.oldIntent(time.Unix(ri.Time, 0)) {
				sr.rep.IntentsOpen++

				if sr.settled([]string{ri.GUID}) {
					in.drop[mboxOID(id)] = append(in.drop[mboxOID(id)], key)
				}
			}
		}
	}
}

// settleKey drops the intent key of one blob once it is old and settled.
func (sr *scrubRun) settleKey(oid, key string, c *blobCand) {
	if c == nil || !c.old {
		return
	}

	sr.rep.IntentsOpen++

	if c.resolved {
		sr.intents.drop[oid] = append(sr.intents.drop[oid], key)
	}
}

func importGUIDsOf(im importIntent) []string {
	out := make([]string, 0, len(im.Blobs))
	for _, b := range im.Blobs {
		out = append(out, b.GUID)
	}

	return out
}

// settlePubSeen removes the private \Seen marks of public folders that
// are gone (with act; otherwise the listing does) and their ps: keys.
func (sr *scrubRun) settlePubSeen(act bool) {
	marks := sr.intents.reg[regPubSeenPrefix]
	if len(marks) == 0 || sr.u.name == model.PublicNamespace {
		return
	}

	live, err := sr.publicMailboxIDs()
	if err != nil {
		sr.log.Warn("scrub: public folders not readable, marks kept", "err", err)
		sr.rep.Errors = append(sr.rep.Errors, "public folders: "+err.Error())

		return
	}

	for id := range marks {
		if live[id] {
			continue
		}

		oid := oidPrivateSeen + id

		switch {
		case act && sr.o.Allowed(RepairUnknown):
			if err := sr.u.idx.Delete(oid); err != nil && errCode(err) != syscall.ENOENT {
				sr.rep.Errors = append(sr.rep.Errors, fmt.Sprintf("remove %s: %v", oid, err))
				continue
			}

			scrubFinding("stale_marks")

			sr.rep.StaleMarkObjects++
		case act || sr.u.objectExists(oid):
			continue
		}

		sr.intents.drop[oidRegistry] = append(sr.intents.drop[oidRegistry], pubSeenKey(id))
	}
}

// objectExists reports whether an index object exists (an error counts
// as existing: nothing is dropped on a guess).
func (u *User) objectExists(oid string) bool {
	_, err := u.idx.Stat(oid)
	return errCode(err) != syscall.ENOENT
}

// ShadowReport is what a scrub in shadow mode found by listing that the
// intents do not name, and the other way round.
type ShadowReport struct {
	// OrphansMissed are orphan blobs (older than the grace period) no
	// intent names: pre-0.42.0 debris (for the sweep) or a write path
	// without an intent.
	OrphansMissed int
	// OrphansExtra are blobs the intents take for orphans that the
	// listing does not: must stay 0.
	OrphansExtra int
	// UnknownMissed are index objects without registry entry no
	// creation or deletion intent names.
	UnknownMissed int
	// MarksMissed are \Seen mark objects of gone public folders without
	// a ps: key.
	MarksMissed int
	// Samples name a few of the objects behind the counts.
	Samples []string `json:",omitempty"`
}

// shadowSamples bounds the samples of a shadow report.
const shadowSamples = 10

func (s *ShadowReport) sample(what string) {
	if len(s.Samples) < shadowSamples {
		s.Samples = append(s.Samples, what)
	}
}

// compareShadow compares the listing's findings with the intents and
// records the differences in the report, the metric and the log.
func (sr *scrubRun) compareShadow(orphans map[string]bool, unknown, staleMarks []string) {
	s := &ShadowReport{}

	sr.shadowBlobs(s, orphans)
	sr.shadowObjects(s, unknown, staleMarks)

	for kind, n := range map[string]int{"orphan_missed": s.OrphansMissed, "orphan_extra": s.OrphansExtra,
		"unknown_missed": s.UnknownMissed, "marks_missed": s.MarksMissed} {
		if n > 0 {
			metrics.ScrubShadowDiff.WithLabelValues(kind).Add(float64(n))
		}
	}

	if s.OrphansMissed+s.OrphansExtra+s.UnknownMissed+s.MarksMissed > 0 {
		sr.log.Warn("scrub shadow: listing and intents differ", "orphans_missed", s.OrphansMissed, "orphans_extra", s.OrphansExtra,
			"unknown_missed", s.UnknownMissed, "marks_missed", s.MarksMissed, "samples", s.Samples)
	}

	sr.rep.Shadow = s
}

// shadowBlobs compares the orphan blobs.
func (sr *scrubRun) shadowBlobs(s *ShadowReport, orphans map[string]bool) {
	in := sr.intents

	for guid := range orphans {
		if !in.covers(guid) {
			s.OrphansMissed++
			s.sample("orphan " + guid)
		}
	}

	for guid, c := range in.blobs {
		if c.orphan && !orphans[guid] {
			s.OrphansExtra++
			s.sample("extra " + guid)
		}
	}
}

// shadowObjects compares the unknown index objects and the stale marks.
func (sr *scrubRun) shadowObjects(s *ShadowReport, unknown, staleMarks []string) {
	in := sr.intents

	for _, oid := range unknown {
		if !in.coveredMbox[mailboxIDOf(oid)] && !sr.importedShard(oid) {
			s.UnknownMissed++
			s.sample("unknown " + oid)
		}
	}

	for _, oid := range staleMarks {
		if _, ok := in.reg[regPubSeenPrefix][strings.TrimPrefix(oid, oidPrivateSeen)]; !ok {
			s.MarksMissed++
			s.sample("marks " + oid)
		}
	}
}

// mailboxIDOf returns the mailbox id of an index object "mbox/<id>[/n]".
func mailboxIDOf(oid string) string {
	id, _, _ := strings.Cut(strings.TrimPrefix(oid, mboxPrefix), "/")
	return id
}

// importedShard reports whether an unknown higher shard belongs to a
// registered mailbox an import step announced (entries above UIDNEXT of
// an interrupted step).
func (sr *scrubRun) importedShard(oid string) bool {
	id := mailboxIDOf(oid)
	if _, live := sr.byID[id]; !live {
		return false
	}

	for _, v := range sr.intents.reg[regImportBlobPrefix] {
		var im importIntent
		if json.Unmarshal(v, &im) == nil && im.Mailbox == id {
			return true
		}
	}

	return false
}

// metadataMailbox matches the mailbox id of a METADATA key (metadata.go):
// "s:<id>:/…" or "p:<accessor>:<id>:/…".
var metadataMailbox = regexp.MustCompile(`^(?:s|p:.*?):([0-9a-f]{32}):/`)

// cleanMetadata removes the METADATA entries of mailboxes that are gone.
// The registry is read after the entries: an entry can only be written
// for a registered mailbox, and ids are never used again, so an id that
// the later read does not find belongs to a deleted mailbox.
func (sr *scrubRun) cleanMetadata() {
	stale, err := sr.staleMetadata()
	if err != nil {
		sr.rep.Errors = append(sr.rep.Errors, "metadata: "+err.Error())
		return
	}

	if len(stale) == 0 {
		return
	}

	scrubFinding("stale_metadata")

	sr.rep.StaleMetadata += len(stale)

	sr.log.Warn("metadata of deleted mailboxes", "entries", len(stale))

	if !sr.o.Allowed(RepairUnknown) {
		return
	}

	err = casOmap(sr.u, sr.u.idx, oidMetadata, slices.Collect(maps.Keys(stale)), func(cur map[string][]byte) (map[string][]byte, []string) {
		var rm []string

		for k, v := range cur {
			if bytes.Equal(v, stale[k]) {
				rm = append(rm, k)
			}
		}

		return nil, rm
	})
	if err != nil {
		sr.rep.Errors = append(sr.rep.Errors, "metadata: "+err.Error())
	}
}

// staleMetadata returns the METADATA entries of mailboxes that are gone,
// with their values.
func (sr *scrubRun) staleMetadata() (map[string][]byte, error) {
	u := sr.u

	vals, err := u.idx.GetAllOmapValues(oidMetadata, "", "", omapPageSize)
	if err != nil {
		if errCode(err) == syscall.ENOENT {
			return nil, nil
		}

		return nil, err
	}

	boxes, err := u.ListMailboxes()
	if err != nil {
		return nil, err
	}

	live := make(map[string]bool, len(boxes))
	for i := range boxes {
		live[boxes[i].ID] = true
	}

	stale := map[string][]byte{}

	for k, v := range vals {
		if m := metadataMailbox.FindStringSubmatch(k); m != nil && !live[m[1]] {
			stale[k] = v
		}
	}

	return stale, nil
}
