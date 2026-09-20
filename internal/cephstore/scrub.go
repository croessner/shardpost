package cephstore

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/ceph/go-ceph/rados"

	"shardpost/internal/metrics"
	"shardpost/internal/model"
)

// Scrubber
//
// The store is designed so that a crash at any point leaves at most
// harmless debris: an orphan blob (written before its index entry), a
// duplicate index entry after a MOVE, a reference count that is one off,
// quota counters that drifted. The scrubber walks a user's namespace and
// repairs exactly these cases. Namespaces are independent, so scrubbing
// parallelises over users trivially.
//
// Rules, all guarded by a grace period on the object mtime so in-flight
// operations are never touched:
//
//   - blob referenced by no index entry           -> delete blob, unindex
//   - index entry whose blob is missing           -> remove entry
//   - refs xattr != number of referencing entries -> fix xattr
//   - quota counters != sum over entries          -> fix counters
//   - dangling registry entry / unregistered shard -> report only

// ScrubReport summarises one user's scrub.
type ScrubReport struct {
	User             string
	Mailboxes        int
	Messages         int
	OrphanBlobs      int
	MissingBlobs     int
	RefsFixed        int
	MoveIntents      int
	DeliveryIntents  int // abandoned multi-target deliveries rolled back
	OrphanCandidates int // orphan blobs seen for the first time (deleted next run)
	QuotaFixed       bool
	UnknownShards    int
	UnknownRemoved   int  // of those, deleted (older than the grace period)
	ImportSkipped    bool // an import holds the account: nothing was checked
	RetiredRepaired  int  // mailboxes left retired by an interrupted import replacement
	UncoveredEntries int  // entries at or above UIDNEXT (interrupted import) removed
	StaleMarkObjects int  // private \Seen marks of public folders that are gone
	Errors           []string
}

// ScrubOptions tunes a scrub run.
type ScrubOptions struct {
	Grace  time.Duration // objects younger than this are skipped
	DryRun bool
	Log    *slog.Logger
	// Repairs selects the repair classes (Repair*); nil repairs all (the
	// periodic scrub), an empty set none. Findings are reported either way.
	Repairs map[string]bool
	// Fence (if set) is checked before each repair; failing, the repair
	// is skipped.
	Fence func() error
}

// Repair classes of the scrubber (model.Repair*).
const (
	RepairDeliveries = model.RepairDeliveries
	RepairMoves      = model.RepairMoves
	RepairRetired    = model.RepairRetired
	RepairUncovered  = model.RepairUncovered
	RepairEntries    = model.RepairEntries
	RepairRefs       = model.RepairRefs
	RepairOrphans    = model.RepairOrphans
	RepairUnknown    = model.RepairUnknown
	RepairQuota      = model.RepairQuota
)

// Allowed reports whether a run may repair the class.
func (o ScrubOptions) Allowed(class string) bool {
	if o.DryRun || (o.Repairs != nil && !o.Repairs[class]) {
		return false
	}

	return o.Fence == nil || o.Fence() == nil
}

// ListUsers returns all namespaces present in the index pool.
func (s *Store) ListUsers() ([]string, error) {
	r := s.ref()
	defer r.refs.Done()

	s.mu.RLock()
	pool := s.cfg.IndexPool
	s.mu.RUnlock()

	ioctx, err := r.conn.OpenIOContext(pool)
	if err != nil {
		return nil, err
	}
	defer ioctx.Destroy()

	ioctx.SetNamespace(rados.AllNamespaces)

	iter, err := ioctx.Iter()
	if err != nil {
		return nil, err
	}
	defer iter.Close()

	seen := map[string]bool{}
	for iter.Next() {
		seen[iter.Namespace()] = true
	}

	if err := iter.Err(); err != nil {
		return nil, err
	}

	users := make([]string, 0, len(seen))
	for u := range seen {
		// The public namespace holds ordinary mailboxes, blobs and
		// counters, so the scrub covers it like an account; the store's
		// own namespaces (~admin, ~learning) it does not.
		if u != "" && (!model.ReservedNamespace(u) || model.IsPublic(u)) {
			users = append(users, u)
		}
	}

	sort.Strings(users)

	return users, nil
}

// scrubRun carries the state shared by the phases of one Scrub call.
type scrubRun struct {
	u      *User
	o      ScrubOptions
	log    *slog.Logger
	rep    *ScrubReport
	cutoff time.Time
	byID   map[string]Mailbox
}

// scrubIndex is what phase 1 (the index scan) collects.
type scrubIndex struct {
	known map[string]bool // index objects that belong to a registered mailbox
	refs  map[string][]Meta
	quota Quota
}

// scrubFinding counts one finding of the given kind.
func scrubFinding(kind string) { metrics.Scrub.WithLabelValues(kind).Inc() }

// Scrub checks and repairs one user.
func (u *User) Scrub(o ScrubOptions) (*ScrubReport, error) {
	log := o.Log
	if log == nil {
		log = u.log
	}

	rep := &ScrubReport{User: u.name}
	cutoff := time.Now().Add(-o.Grace)

	if u.importing(rep, log) {
		return rep, nil
	}

	boxes, err := u.ListMailboxes()
	if err != nil {
		return nil, err
	}
	// A registry entry that does not parse hides a mailbox: its messages
	// would look like orphans. Count it as an incomplete scan.
	registryOK := u.scrubRegistryOK(log)

	byID := map[string]Mailbox{}
	for _, mb := range boxes {
		byID[mb.ID] = mb
	}

	sr := &scrubRun{u: u, o: o, log: log, rep: rep, cutoff: cutoff, byID: byID}

	// 0a. Abandoned multi-target deliveries (Sieve fileinto into several
	//     mailboxes, MULTIAPPEND). The client never saw a success, so what
	//     was stored is removed again; the client retries.
	sr.rollbackDeliveries()

	// 0. Unfinished MOVEs. An intent older than the grace period is
	//    completed: whatever already reached the destination stays there,
	//    the source entries are removed, the intent is dropped.
	sr.finishMoves(boxes)

	// 1. All index entries of all registered mailboxes.
	idx, complete := sr.scanIndex(boxes, registryOK)

	if !complete {
		log.Warn("scrub: index scan incomplete, skipping repairs", "errors", rep.Errors)
		sr.logDone()

		return rep, nil
	}

	// 2. Reference counts.
	sr.fixRefs(idx.refs)

	// 3. Orphan blobs and unknown index objects.
	if err := sr.removeOrphans(idx.refs); err != nil {
		return nil, err
	}

	unknown, err := sr.collectUnknownIndex(idx.known)
	if err != nil {
		return nil, err
	}

	sr.removeUnknownIndex(unknown)

	// 4. Quota counters.
	sr.fixQuota(idx.quota)

	sr.logDone()

	return rep, nil
}

// importing reports whether an import holds the account (or its lock is
// unreadable): an import writes blobs before the entries that make them
// visible and may leave entries above UIDNEXT until its next step, none
// of which is an orphan then. The scrub is skipped.
func (u *User) importing(rep *ScrubReport, log *slog.Logger) bool {
	locked, err := u.ImportLocked()

	switch {
	case err != nil:
		log.Warn("scrub: import lock not readable, skipped", "err", err)
		rep.Errors = append(rep.Errors, "import lock: "+err.Error())
	case locked:
		log.Info("scrub: account import in progress, skipped")
	default:
		return false
	}

	rep.ImportSkipped = true

	return true
}

// logDone logs the final report of a scrub run.
func (sr *scrubRun) logDone() {
	b, _ := json.Marshal(sr.rep)
	sr.log.Info("scrub done", "report", string(b))
}

// scrubRegistryOK reports whether every registry entry parses (a missing
// registry object counts as fine).
func (u *User) scrubRegistryOK(log *slog.Logger) bool {
	registryOK := true

	if raw, err := u.idx.GetAllOmapValues(oidRegistry, "", regMboxPrefix, omapPageSize); err == nil {
		for k, v := range raw {
			var rec MailboxRec
			if json.Unmarshal(v, &rec) != nil || rec.ID == "" {
				log.Warn("corrupt registry entry", "key", k)

				registryOK = false
			}
		}
	} else if errCode(err) != syscall.ENOENT {
		registryOK = false
	}

	return registryOK
}

// rollbackDeliveries rolls back abandoned multi-target deliveries.
func (sr *scrubRun) rollbackDeliveries() {
	u, rep, log, o := sr.u, sr.rep, sr.log, sr.o

	intents, err := u.deliveryIntents()
	if err != nil {
		rep.Errors = append(rep.Errors, err.Error())
		return
	}

	for _, in := range intents {
		if time.Since(time.Unix(in.Started, 0)) < deliveryIntentMaxAge {
			continue
		}

		scrubFinding("delivery_intent")

		rep.DeliveryIntents++

		log.Warn("abandoned delivery, rolling back", "intent", in.ID, "entries", len(in.Entries))

		if !o.Allowed(RepairDeliveries) {
			continue
		}
		// Entries name mailbox id and GUID: a mailbox deleted (and
		// perhaps recreated under the same name) since then has taken
		// the message with it; a renamed one is found by id.
		failed := false

		for _, e := range in.Entries {
			if !sr.rollbackEntry(e) {
				failed = true
			}
		}

		if !failed {
			if err := u.EndDelivery(in.ID); err != nil {
				rep.Errors = append(rep.Errors, err.Error())
			}
		}
	}
}

// rollbackEntry expunges the message of one delivery entry. It returns
// false if anything failed.
func (sr *scrubRun) rollbackEntry(e deliveryEntry) bool {
	u, rep := sr.u, sr.rep

	mb, ok := sr.byID[e.MailboxID]
	if !ok {
		return true
	}

	metas, err := u.ListMessages(mb.ID)
	if err != nil {
		rep.Errors = append(rep.Errors, fmt.Sprintf("rollback %s: %v", mb.Name, err))
		return false
	}

	failed := false

	for _, m := range metas {
		if m.GUID != e.GUID {
			continue
		}

		if err := u.Expunge(mb.Name, []Meta{m}); err != nil && !errors.Is(err, ErrNoMailbox) {
			rep.Errors = append(rep.Errors, fmt.Sprintf("rollback %s uid %d: %v", mb.Name, m.UID, err))
			failed = true
		}
	}

	return !failed
}

// finishMoves completes the unfinished MOVEs of all mailboxes.
func (sr *scrubRun) finishMoves(boxes []Mailbox) {
	for _, mb := range boxes {
		intents, err := sr.u.pendingIntents(mb.ID)
		if err != nil {
			sr.rep.Errors = append(sr.rep.Errors, err.Error())
			continue
		}

		for _, in := range intents {
			sr.finishMove(mb, in)
		}
	}
}

// finishMove completes one MOVE intent recorded on mailbox mb.
func (sr *scrubRun) finishMove(mb Mailbox, in moveIntent) {
	u, rep, log, o := sr.u, sr.rep, sr.log, sr.o

	if time.Unix(in.Started, 0).After(sr.cutoff) {
		return
	}

	scrubFinding("move_intent")

	rep.MoveIntents++

	log.Warn("unfinished MOVE", "mailbox", mb.Name, "intent", in.ID, "uids", in.UIDs)

	if !o.Allowed(RepairMoves) {
		return
	}

	dst, ok := sr.byID[in.Dest]
	src := mb

	srcMetas, err := u.GetMessages(src.ID, in.UIDs)
	if err != nil {
		rep.Errors = append(rep.Errors, err.Error())
		return
	}

	if !ok {
		// Destination is gone: the move cannot be completed. Keep the
		// source (the message stays where the user still sees it) and
		// drop only the intent. A reservation Move held for a counted
		// destination stays until fixQuota recounts.
		log.Warn("MOVE destination vanished, keeping source", "intent", in.ID)

		if _, err := u.unlink(&src, nil, in.ID); err != nil {
			rep.Errors = append(rep.Errors, err.Error())
		}

		return
	}

	dm, err := u.ListMessages(dst.ID)
	if err != nil {
		rep.Errors = append(rep.Errors, err.Error())
		return
	}

	inDest := map[string]bool{}
	for _, m := range dm {
		inDest[m.GUID] = true
	}
	// Finish: link what is missing in dest (with its own blob
	// reference, as Move does); only when every source entry is
	// confirmed in dest is the source unlinked and its references
	// released.
	complete := sr.linkMissing(&dst, srcMetas, inDest)

	if !complete {
		return // intent stays, next run retries
	}

	removed, err := u.unlink(&src, srcMetas, in.ID)
	if err != nil {
		rep.Errors = append(rep.Errors, err.Error())
		return
	}

	sr.releaseRemoved(removed)
	// Like Move: leaving a counted mailbox for an ignored one frees the
	// space. The opposite direction was reserved before the intent.
	if err := u.moveUsage(&src, &dst, sizeOf(removed)); err != nil {
		rep.Errors = append(rep.Errors, err.Error())
	}
}

// releaseRemoved drops the blob references of unlinked entries.
func (sr *scrubRun) releaseRemoved(removed []Meta) {
	for i := range removed {
		if err := sr.u.releaseBlob(&removed[i]); err != nil {
			sr.rep.Errors = append(sr.rep.Errors, err.Error())
		}
	}
}

// linkMissing links every source entry not yet in dst into dst, each with
// its own blob reference. It returns false if any of them failed.
func (sr *scrubRun) linkMissing(dst *Mailbox, srcMetas []Meta, inDest map[string]bool) bool {
	u, rep := sr.u, sr.rep
	complete := true

	for _, m := range srcMetas {
		if inDest[m.GUID] {
			continue
		}

		if _, err := u.blobRef(m.GUID, +1); err != nil {
			rep.Errors = append(rep.Errors, err.Error())
			complete = false

			continue
		}

		if _, err := u.link(dst, relink(m)); err != nil {
			_, _ = u.blobRef(m.GUID, -1)

			rep.Errors = append(rep.Errors, err.Error())
			complete = false
		}
	}

	return complete
}

// scanIndex reads all index entries of all registered mailboxes. The
// returned complete is false as soon as any index could not be read in
// full. Everything destructive (orphan deletion, refcount and quota fixes)
// is then skipped: a missing reference must never look like an orphan.
func (sr *scrubRun) scanIndex(boxes []Mailbox, registryOK bool) (*scrubIndex, bool) {
	idx := &scrubIndex{known: map[string]bool{}, refs: map[string][]Meta{}}
	complete := registryOK

	for _, mb := range boxes {
		if !sr.scanMailbox(mb, idx) {
			complete = false
		}
	}

	return idx, complete
}

// scanMailbox scans one mailbox into idx. It returns false if the mailbox
// could not be read in full.
func (sr *scrubRun) scanMailbox(mb Mailbox, idx *scrubIndex) bool {
	u, rep := sr.u, sr.rep

	if _, ok := u.retired(&mb); ok {
		sr.unregisterRetired(mb)
		return true // it was empty
	}

	info, _, err := u.readHeader(&mb)
	if err != nil {
		rep.Errors = append(rep.Errors, fmt.Sprintf("mailbox %q: %v", mb.Name, err))
		return false
	}

	rep.Mailboxes++

	complete := sr.markShards(&mb, info, idx.known)
	sr.removeUncovered(&mb, info)

	metas, err := u.ListMessages(mb.ID)
	if err != nil {
		rep.Errors = append(rep.Errors, fmt.Sprintf("mailbox %q: %v", mb.Name, err))
		return false
	}

	for _, m := range metas {
		sr.scanMessage(&mb, m, idx)
	}

	return complete
}

// unregisterRetired removes the registry entry of a mailbox whose index
// object an interrupted import replacement retired (the scrub does not
// run while the import holds the account). The mailbox was empty; it is
// gone now, and the next CREATE (or INBOX autoprovisioning) gets a new
// UIDVALIDITY above the recorded one.
func (sr *scrubRun) unregisterRetired(mb Mailbox) {
	u := sr.u

	sr.log.Warn("mailbox retired by an interrupted import", "mailbox", mb.Name)
	scrubFinding("retired_mailbox")

	if !sr.o.Allowed(RepairRetired) {
		return
	}

	err := casOmap(u, u.idx, oidRegistry, []string{regKey(mb.Name)}, func(cur map[string][]byte) (map[string][]byte, []string) {
		var rec MailboxRec
		if v, ok := cur[regKey(mb.Name)]; !ok || json.Unmarshal(v, &rec) != nil || rec.ID != mb.ID {
			return nil, nil
		}

		return nil, []string{regKey(mb.Name)}
	})
	if err != nil {
		sr.rep.Errors = append(sr.rep.Errors, fmt.Sprintf("unregister retired %q: %v", mb.Name, err))
		return
	}

	u.invalidate()

	sr.rep.RetiredRepaired++
}

// removeUncovered removes entries at or above UIDNEXT from the shard of
// UIDNEXT-1: an interrupted import step wrote them before its header
// update. Readers ignore them, but a later delivery would be assigned
// such a UID and keep the stale entry (it carries a higher modseq).
// Their blobs become orphans and go the usual way. Entries on shards
// above are unknown objects and removed as such.
//
// A delivery writes its header (UIDNEXT) before its entry, so reading
// the header again after the entries covers every entry a delivery has
// written; the removal only takes values that are still unchanged.
func (sr *scrubRun) removeUncovered(mb *Mailbox, info *MailboxInfo) {
	u := sr.u
	oid := u.shardOID(mb.ID, shardOf(max(info.UIDNext-1, 1), info.ShardSize))

	seen := u.uncovered(mb, oid, info.UIDNext)
	if len(seen) == 0 {
		return
	}

	sr.log.Warn("entries above UIDNEXT (interrupted import)", "mailbox", mb.Name, "count", len(seen))
	scrubFinding("uncovered_entry")

	// An import that resumed since the scrub started writes such entries
	// before its header: leave them to it.
	if locked, err := u.ImportLocked(); !sr.o.Allowed(RepairUncovered) || err != nil || locked {
		return
	}

	removed := 0

	err := casOmap(u, u.idx, oid, slices.Collect(maps.Keys(seen)), func(cur map[string][]byte) (map[string][]byte, []string) {
		var rm []string

		for k, v := range cur {
			if bytes.Equal(v, seen[k]) {
				rm = append(rm, k)
			}
		}

		removed = len(rm)

		return nil, rm
	})
	if err != nil {
		sr.rep.Errors = append(sr.rep.Errors, fmt.Sprintf("remove uncovered entries of %q: %v", mb.Name, err))
		return
	}

	sr.rep.UncoveredEntries += removed
}

// uncovered returns the entries of shard oid at or above UIDNEXT, as
// read before the header is read again.
func (u *User) uncovered(mb *Mailbox, oid string, uidNext uint32) map[string][]byte {
	vals, err := u.idx.GetOmapValues(oid, uidKey(uidNext-1), keyMsgPrefix, omapPageSize)
	if err != nil || len(vals) == 0 {
		return nil
	}

	fresh, _, err := u.readHeader(mb)
	if err != nil {
		return nil
	}

	seen := map[string][]byte{}

	for k, v := range vals {
		if uidOfKey(k) >= fresh.UIDNext {
			seen[k] = v
		}
	}

	return seen
}

// markShards records the shard objects of mb as known. It returns false
// if an intermediate shard object is missing.
func (sr *scrubRun) markShards(mb *Mailbox, info *MailboxInfo, known map[string]bool) bool {
	u := sr.u
	complete := true

	last := shardOf(max(info.UIDNext-1, 1), info.ShardSize)
	for s := uint32(0); s <= last; s++ {
		known[u.shardOID(mb.ID, s)] = true
		// An intermediate shard object that is missing has either
		// never been written (a single crashed delivery) or was lost;
		// we cannot tell, so no destructive repair this run.
		if s > 0 && s < last {
			if _, err := u.idx.Stat(u.shardOID(mb.ID, s)); err != nil && errCode(err) == syscall.ENOENT {
				sr.log.Warn("missing intermediate shard", "mailbox", mb.Name, "shard", s)

				complete = false
			}
		}
	}

	return complete
}

// scanMessage accounts one index entry and removes it if its blob is
// missing.
func (sr *scrubRun) scanMessage(mb *Mailbox, m Meta, idx *scrubIndex) {
	u, rep := sr.u, sr.rep

	rep.Messages++

	idx.refs[m.GUID] = append(idx.refs[m.GUID], m)
	if u.rules.Counts(mb) {
		idx.quota.Bytes += m.Size
		idx.quota.Msgs++
	}
	// entry without blob
	st, err := u.data.Stat(chunkOID(m.GUID, 0))
	if err != nil && errCode(err) == syscall.ENOENT {
		scrubFinding("missing_blob")

		rep.MissingBlobs++

		sr.log.Warn("index entry without blob", "mailbox", mb.Name, "uid", m.UID, "guid", m.GUID)

		if sr.o.Allowed(RepairEntries) {
			if _, err := u.unlink(mb, []Meta{m}, ""); err != nil {
				rep.Errors = append(rep.Errors, err.Error())
			}
		}

		return
	}

	_ = st
}

// fixRefs repairs reference counts. The fix is a compare-and-swap against
// the version the mismatch was observed at: if a COPY or EXPUNGE touched
// the blob in between, nothing is written and the next run re-evaluates.
func (sr *scrubRun) fixRefs(refs map[string][]Meta) {
	u, rep, log := sr.u, sr.rep, sr.log

	for guid, entries := range refs {
		val, ver, err := u.xattrVer(u.data, chunkOID(guid, 0), xattrRefs)
		if err != nil {
			continue
		}

		have, _ := strconv.Atoi(string(val))
		if have == len(entries) {
			continue
		}

		st, err := u.data.Stat(chunkOID(guid, 0))
		if err != nil || st.ModTime.After(sr.cutoff) {
			continue
		}

		scrubFinding("refs_mismatch")

		rep.RefsFixed++

		log.Warn("refcount mismatch", "guid", guid, "xattr", have, "entries", len(entries))

		if sr.o.Allowed(RepairRefs) {
			op := rados.CreateWriteOp()
			op.AssertVersion(ver)
			op.SetXattr(xattrRefs, []byte(strconv.Itoa(len(entries))))

			if err := op.Operate(u.data, chunkOID(guid, 0), rados.OperationNoFlag); err != nil {
				log.Info("refcount changed concurrently, not fixed", "guid", guid)
			}

			op.Release()
		}
	}
}

// collectOrphans lists the blobs no index entry references: guid ->
// number of chunks (highest chunk seen + 1).
func (sr *scrubRun) collectOrphans(refs map[string][]Meta) (map[string]int, error) {
	iter, err := sr.u.data.Iter()
	if err != nil {
		return nil, err
	}

	orphans := map[string]int{} // guid -> highest chunk seen

	for iter.Next() {
		oid := iter.Value()
		if !strings.HasPrefix(oid, msgPrefix) {
			continue
		}

		rest := strings.TrimPrefix(oid, msgPrefix)

		guid, chunkStr, _ := strings.Cut(rest, "/")
		if _, ok := refs[guid]; ok {
			continue
		}

		c, _ := strconv.Atoi(chunkStr)
		if c+1 > orphans[guid] {
			orphans[guid] = c + 1
		}
	}

	iter.Close()

	return orphans, nil
}

// removeOrphans deletes orphan blobs. An orphan is only deleted when two
// consecutive runs saw it unreferenced and the blob's object version did
// not change in between (no COPY/refcount write touched it). The scan and
// the deletion are not under a common lock; the second look replaces it.
// Candidates are remembered on the scrub object of the namespace.
func (sr *scrubRun) removeOrphans(refs map[string][]Meta) error {
	u, rep, log := sr.u, sr.rep, sr.log

	orphans, err := sr.collectOrphans(refs)
	if err != nil {
		return err
	}

	seen, err := u.orphanCandidates()
	if err != nil {
		return err
	}

	confirmed := map[string]uint64{}
	forget := vanishedCandidates(seen, orphans)

	for guid, chunks := range orphans {
		st, err := u.data.Stat(chunkOID(guid, 0))
		if err == nil && st.ModTime.After(sr.cutoff) {
			continue // a delivery in flight
		}

		_, ver, verr := u.xattrVer(u.data, chunkOID(guid, 0), xattrRefs)
		if verr != nil {
			continue
		}

		if prev, ok := seen[guid]; !ok || prev != ver {
			confirmed[guid] = ver // first sighting (or changed): remember, delete next run
			rep.OrphanCandidates++

			continue
		}

		forget = append(forget, guid)

		scrubFinding("orphan_blob")

		rep.OrphanBlobs++

		log.Warn("orphan blob", "guid", guid, "chunks", chunks)

		if sr.o.Allowed(RepairOrphans) {
			sr.deleteOrphan(guid, chunks, st)
		}
	}

	if sr.o.Allowed(RepairOrphans) {
		if err := u.rememberOrphans(confirmed, forget); err != nil {
			rep.Errors = append(rep.Errors, err.Error())
		}
	}

	return nil
}

// vanishedCandidates returns the remembered orphan candidates that are
// no orphans any more (referenced again or gone).
func vanishedCandidates(seen map[string]uint64, orphans map[string]int) []string {
	forget := []string{}

	for guid := range seen {
		if _, ok := orphans[guid]; !ok {
			forget = append(forget, guid) // referenced again or gone
		}
	}

	return forget
}

// deleteOrphan removes a confirmed orphan blob and its FTS postings. st
// is the stat of chunk 0.
func (sr *scrubRun) deleteOrphan(guid string, chunks int, st rados.ObjectStat) {
	u := sr.u
	m := Meta{GUID: guid, Chunks: chunks}

	if st.Size > 0 {
		// Size for readBlob: sum of chunk sizes.
		var size int64

		for n := 0; n < chunks; n++ {
			if cs, err := u.data.Stat(chunkOID(guid, n)); err == nil {
				size += int64(cs.Size)
			}
		}

		m.Size = size
		if err := u.ftsRemove(&m); err != nil {
			sr.log.Debug("fts remove for orphan failed", "guid", guid, "err", err)
		}
	}

	u.deleteBlob(guid, chunks)
}

// collectUnknownIndex lists index objects no registry entry points to:
// left behind when a process died between creating a mailbox object and
// registering its name, or between unregistering and deleting. Nothing
// can reach them (mailbox ids are random).
func (sr *scrubRun) collectUnknownIndex(known map[string]bool) ([]string, error) {
	iter, err := sr.u.idx.Iter()
	if err != nil {
		return nil, err
	}

	var unknown, marks []string

	for iter.Next() {
		oid := iter.Value()

		switch {
		case strings.HasPrefix(oid, mboxPrefix) && !known[oid]:
			unknown = append(unknown, oid)
		case strings.HasPrefix(oid, oidPrivateSeen):
			marks = append(marks, oid)
		}
	}

	iter.Close()

	sr.removeStaleMarks(marks)

	return unknown, nil
}

// removeStaleMarks deletes the private \Seen marks of public folders
// that no longer exist. The marks live in this account's namespace, so
// nobody else can reach them: deleting the folder cannot, and neither
// can the account that deleted it.
func (sr *scrubRun) removeStaleMarks(oids []string) {
	if len(oids) == 0 || sr.u.name == model.PublicNamespace {
		return
	}

	live, err := sr.publicMailboxIDs()
	if err != nil {
		sr.log.Warn("scrub: public folders not readable, marks kept", "err", err)
		sr.rep.Errors = append(sr.rep.Errors, "public folders: "+err.Error())

		return
	}

	for _, oid := range oids {
		if live[strings.TrimPrefix(oid, oidPrivateSeen)] {
			continue
		}

		scrubFinding("stale_marks")

		sr.rep.StaleMarkObjects++

		sr.log.Warn("private seen marks of a public folder that is gone", "oid", oid)

		if !sr.o.Allowed(RepairUnknown) {
			continue
		}

		if err := sr.u.idx.Delete(oid); err != nil && errCode(err) != syscall.ENOENT {
			sr.rep.Errors = append(sr.rep.Errors, fmt.Sprintf("remove %s: %v", oid, err))
		}
	}
}

// publicMailboxIDs returns the ids of the public folders that exist.
func (sr *scrubRun) publicMailboxIDs() (map[string]bool, error) {
	pub, err := sr.u.store.User(model.PublicNamespace)
	if err != nil {
		return nil, err
	}
	defer pub.Close()

	boxes, err := pub.ListMailboxes()
	if err != nil {
		return nil, err
	}

	live := make(map[string]bool, len(boxes))
	for _, mb := range boxes {
		live[mb.ID] = true
	}

	return live, nil
}

// removeUnknownIndex removes unknown index objects older than the grace
// period; younger ones may belong to a CREATE in progress.
func (sr *scrubRun) removeUnknownIndex(unknown []string) {
	u, rep, log := sr.u, sr.rep, sr.log

	for _, oid := range unknown {
		scrubFinding("unknown_shard")

		rep.UnknownShards++

		st, err := u.idx.Stat(oid)
		if err != nil || st.ModTime.After(sr.cutoff) {
			// unreadable or young (a CREATE in progress): report only
			log.Warn("index object without registry entry, kept", "oid", oid, "err", err)
			continue
		}

		log.Warn("index object without registry entry", "oid", oid)

		if sr.o.Allowed(RepairUnknown) {
			if err := u.idx.Delete(oid); err != nil && errCode(err) != syscall.ENOENT {
				rep.Errors = append(rep.Errors, fmt.Sprintf("remove %s: %v", oid, err))
			} else {
				rep.UnknownRemoved++
			}
		}
	}
}

// fixQuota repairs the quota counters. Deliveries in flight hold
// reservations the index does not show yet, so a difference is only
// corrected when two consecutive runs counted the same value and the
// stored counter still differs from it (a live user's counters keep
// moving).
func (sr *scrubRun) fixQuota(quota Quota) {
	u, rep, log := sr.u, sr.rep, sr.log

	cur, err := u.QuotaUsage()
	if err == nil && (cur.Bytes != quota.Bytes || cur.Msgs != quota.Msgs) {
		prev, _ := u.scrubState(scrubKeyQuotaCounted)

		counted := fmt.Sprintf("%d/%d", quota.Bytes, quota.Msgs)
		if prev == counted {
			scrubFinding("quota_drift")

			rep.QuotaFixed = true

			log.Warn("quota drift", "counted", quota, "stored", cur)

			if sr.o.Allowed(RepairQuota) {
				if err := u.SetQuotaUsage(quota); err != nil {
					rep.Errors = append(rep.Errors, err.Error())
				}
			}
		} else {
			log.Info("quota differs from index, confirming next run", "counted", quota, "stored", cur)
		}

		if sr.o.Allowed(RepairQuota) {
			_ = u.setScrubState(scrubKeyQuotaCounted, counted)
		}
	}
}

// scrubKeyQuotaCounted holds the quota usage counted by the previous run.
const scrubKeyQuotaCounted = "q:counted"

// oidScrub holds scrubber state of a namespace: o:<guid> -> object
// version of chunk 0 when the blob was first seen as an orphan.
const oidScrub = "scrub"

func (u *User) orphanCandidates() (map[string]uint64, error) {
	vals, err := u.idx.GetAllOmapValues(oidScrub, "", "o:", omapPageSize)
	if err != nil {
		if errCode(err) == syscall.ENOENT {
			return map[string]uint64{}, nil
		}

		return nil, err
	}

	out := map[string]uint64{}

	for k, v := range vals {
		ver, _ := strconv.ParseUint(string(v), 10, 64)
		out[strings.TrimPrefix(k, "o:")] = ver
	}

	return out, nil
}

func (u *User) rememberOrphans(add map[string]uint64, forget []string) error {
	if len(add) == 0 && len(forget) == 0 {
		return nil
	}

	return casOmap(u, u.idx, oidScrub, nil, func(_ map[string][]byte) (map[string][]byte, []string) {
		set := map[string][]byte{}
		for g, v := range add {
			set["o:"+g] = []byte(strconv.FormatUint(v, 10))
		}

		var rm []string
		for _, g := range forget {
			rm = append(rm, "o:"+g)
		}

		return set, rm
	})
}

func (u *User) scrubState(key string) (string, error) {
	vals, err := getOmapKeys(u.idx, oidScrub, []string{key})
	if err != nil {
		if errCode(err) == syscall.ENOENT {
			return "", nil
		}

		return "", err
	}

	return string(vals[key]), nil
}

func (u *User) setScrubState(key, val string) error {
	return casOmap(u, u.idx, oidScrub, nil, func(_ map[string][]byte) (map[string][]byte, []string) {
		return map[string][]byte{key: []byte(val)}, nil
	})
}
