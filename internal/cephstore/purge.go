package cephstore

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"syscall"

	"github.com/ceph/go-ceph/rados"

	"shardpost/internal/attcache"
	"shardpost/internal/metrics"
	"shardpost/internal/model"
)

// Purge without listing (scaling plan B9). What an account holds is named
// by its data model, so a purge can delete it object by object instead of
// listing the account's namespaces in both pools:
//
//   - the mailboxes of the registry, of creation intents (ci:) and of
//     deletion tombstones (dm:), each with its shards from the header of
//     shard 0 (or the tombstone), and every shard above that exists;
//   - the blobs of their entries, of the removal intents (r:) and MOVE
//     intents (i:) on shard 0, of the blob, import and upload intents
//     (bi:, bimp:, up:) and of the submission copies (sub:), of the
//     delivery intents (d:) and of the orphan
//     candidates of the scrub object;
//   - the fixed objects: the full-text index (every generation its layout
//     record names, ftx/<gen>/…; the layout 2 objects fts/, ftst/, ftsh/,
//     ftsd/ in any case; ftsmap after them), the
//     front-end indexes (ix/<space>/<nn>), account, metadata, sieve (with
//     the vacation records), scrub;
//   - pubseen/<id> for every ps:<id>;
//   - the registry, last.
//
// What the model cannot name stays: debris that servers before 0.42.0
// left without an intent, objects of an account that was never scrubbed
// since, full-text shards above the configured count after fts_shards was
// lowered. The purge records the account as purged first (purged.go), so
// "shardpost scrub --sweep" may delete what is left in the namespace once
// its registry is gone.

// purgeFixedIndexSpaces are the spaces of the front-end indexes
// (ix/<space>/<nn>) a purge deletes; a new space has to be added here.
var purgeFixedIndexSpaces = []string{attcache.Space}

// purgeProbeShards bounds how many shards above the range of a mailbox a
// purge or a scrub probes for (entries of an interrupted import step, a
// delivery whose entry landed after its mailbox was deleted).
const purgeProbeShards = 64

// lastShard is the highest shard of a mailbox: the last of its range, or
// one above that exists (probed one by one, up to purgeProbeShards).
func (u *User) lastShard(id string, info *MailboxInfo) uint32 {
	last := uint32(0)
	if info != nil {
		last = shardOf(max(info.UIDNext-1, 1), info.ShardSize)
	}

	for probe := last + 1; probe <= last+purgeProbeShards && u.objectExists(u.shardOID(id, probe)); probe++ {
		last = probe
	}

	return last
}

// deleteShardRange deletes the shards of a mailbox from last down to 0.
func (u *User) deleteShardRange(id string, last uint32) error {
	for s := int64(last); s >= 0; s-- {
		if err := u.idx.Delete(u.shardOID(id, uint32(s))); err != nil && errCode(err) != syscall.ENOENT {
			return fmt.Errorf("delete index %s shard %d: %w", id, s, err)
		}
	}

	return nil
}

// namedObjects is what the data model of an account names.
type namedObjects struct {
	// index are the index objects in deletion order: the shards of the
	// mailboxes (highest first), then the other objects; the registry
	// is not among them.
	index []string
	// blobs are the blobs by GUID with their chunk count (0: unknown).
	blobs map[string]int
}

func (n *namedObjects) blob(guid string, chunks int) {
	if model.ValidGUID(guid) {
		n.blobs[guid] = max(n.blobs[guid], chunks)
	}
}

// indexSet returns the index objects as a set.
func (n *namedObjects) indexSet() map[string]bool {
	set := make(map[string]bool, len(n.index)+1)
	for _, oid := range n.index {
		set[oid] = true
	}

	set[oidRegistry] = true

	return set
}

// names reports whether the model names an object of the pool.
func (n *namedObjects) names(index map[string]bool, data bool, oid string) bool {
	if !data {
		return index[oid]
	}

	guid, chunk, ok := splitChunkOID(oid)
	if !ok {
		return false
	}

	chunks, ok := n.blobs[guid]

	return ok && (chunks == 0 || chunk < chunks)
}

// splitChunkOID splits "msg/<guid>/<n>".
func splitChunkOID(oid string) (string, int, bool) {
	rest, ok := strings.CutPrefix(oid, msgPrefix)
	if !ok {
		return "", 0, false
	}

	guid, n, ok := strings.Cut(rest, "/")
	if !ok {
		return "", 0, false
	}

	c, err := strconv.Atoi(n)

	return guid, c, err == nil
}

// errModelIncomplete is returned by enumerate when the data model cannot
// be read in full: a registry record that does not parse would hide a
// mailbox, an unreadable header its range.
var errModelIncomplete = errors.New("the data model of the account cannot be read in full")

// enumerate names every object of the account (see the comment above).
// strict (the sweep) also refuses a registered mailbox whose shard 0 is
// missing: its range is unknown, and what lies above shard 0 must not
// look like debris.
func (u *User) enumerate(strict bool) (*namedObjects, error) {
	reg, err := u.idx.GetAllOmapValues(oidRegistry, "", "", omapPageSize)
	if err != nil && errCode(err) != syscall.ENOENT {
		return nil, fmt.Errorf("read registry: %w", err)
	}

	n := &namedObjects{blobs: map[string]int{}}
	mailboxes := map[string]mailboxIntent{}

	others, registered, err := u.registryNames(reg, n, mailboxes)
	if err != nil {
		return nil, err
	}

	ids := make([]string, 0, len(mailboxes))
	for id := range mailboxes {
		ids = append(ids, id)
	}

	sort.Strings(ids)

	for _, id := range ids {
		if strict && registered[id] && !u.objectExists(mboxOID(id)) {
			return nil, fmt.Errorf("%w: shard 0 of the registered mailbox %s is missing", errModelIncomplete, id)
		}

		if err := u.enumerateMailbox(id, mailboxes[id], n); err != nil {
			return nil, err
		}
	}

	if err := u.enumerateCandidates(n); err != nil {
		return nil, err
	}

	fixed, err := u.fixedObjects()
	if err != nil {
		return nil, err
	}

	n.index = append(n.index, others...)
	n.index = append(n.index, fixed...)

	return n, nil
}

// registryNames reads the registry: the mailboxes (registered, being
// created, being deleted with their tombstone) go to mailboxes, the blobs
// of intents to n; it returns the further index objects it names
// (pubseen/<id>) and the registered mailbox ids. A mailbox record that
// does not parse is an error.
func (u *User) registryNames(reg map[string][]byte, n *namedObjects, mailboxes map[string]mailboxIntent) ([]string, map[string]bool, error) {
	var others []string

	registered := map[string]bool{}

	for k, v := range reg {
		switch {
		case strings.HasPrefix(k, regMboxPrefix):
			var rec MailboxRec
			if json.Unmarshal(v, &rec) != nil || rec.ID == "" {
				return nil, nil, fmt.Errorf("%w: registry record %q", errModelIncomplete, k)
			}

			addMailbox(mailboxes, rec.ID)
			registered[rec.ID] = true
		case strings.HasPrefix(k, regCreatePrefix):
			addMailbox(mailboxes, strings.TrimPrefix(k, regCreatePrefix))
		case strings.HasPrefix(k, regDeletePrefix):
			var tomb mailboxIntent

			_ = json.Unmarshal(v, &tomb)
			mailboxes[strings.TrimPrefix(k, regDeletePrefix)] = tomb
		case strings.HasPrefix(k, regPubSeenPrefix):
			others = append(others, oidPrivateSeen+strings.TrimPrefix(k, regPubSeenPrefix))
		default:
			registryBlobs(k, v, n)
		}
	}

	return others, registered, nil
}

// addMailbox adds a mailbox id without a tombstone (a tombstone of the
// same id stays).
func addMailbox(mailboxes map[string]mailboxIntent, id string) {
	if _, ok := mailboxes[id]; !ok {
		mailboxes[id] = mailboxIntent{}
	}
}

// registryBlobs adds the blobs a registry intent names.
func registryBlobs(k string, v []byte, n *namedObjects) {
	switch {
	case strings.HasPrefix(k, regBlobPrefix), strings.HasPrefix(k, regUploadPrefix), strings.HasPrefix(k, regSubmissionPrefix):
		var bi blobIntent

		_ = json.Unmarshal(v, &bi)

		_, guid, _ := strings.Cut(k, ":")
		n.blob(guid, bi.Chunks)
	case strings.HasPrefix(k, regImportBlobPrefix):
		var im importIntent

		_ = json.Unmarshal(v, &im)

		for _, g := range im.guids() {
			n.blob(g, 0)
		}
	case strings.HasPrefix(k, regDeliveryPrefix):
		var d deliveryIntent

		_ = json.Unmarshal(v, &d)

		for _, e := range d.Entries {
			n.blob(e.GUID, 0)
		}
	}
}

// enumerateMailbox names the shards of a mailbox, highest first, and the
// blobs of their entries and of the intents on shard 0.
func (u *User) enumerateMailbox(id string, tomb mailboxIntent, n *namedObjects) error {
	info, err := u.deletedRange(id, tomb)
	if err != nil {
		return err
	}

	last := u.lastShard(id, info)

	for s := int64(last); s >= 0; s-- {
		oid := u.shardOID(id, uint32(s))
		n.index = append(n.index, oid)

		vals, err := u.idx.GetAllOmapValues(oid, "", keyMsgPrefix, omapPageSize)
		if err != nil && errCode(err) != syscall.ENOENT {
			return fmt.Errorf("read %s: %w", oid, err)
		}

		for _, v := range vals {
			var m Meta
			if json.Unmarshal(v, &m) == nil {
				n.blob(m.GUID, m.Chunks)
			}
		}
	}

	return u.enumerateShard0Intents(id, n)
}

// enumerateShard0Intents adds the blobs of the removal and MOVE intents
// of a mailbox.
func (u *User) enumerateShard0Intents(id string, n *namedObjects) error {
	removals, err := u.removeIntents(id)
	if err != nil {
		return fmt.Errorf("read removal intents of %s: %w", id, err)
	}

	for _, ri := range removals {
		n.blob(ri.GUID, ri.Chunks)
	}

	moves, err := u.pendingIntents(id)
	if err != nil {
		return fmt.Errorf("read move intents of %s: %w", id, err)
	}

	for _, mi := range moves {
		for _, g := range mi.GUIDs {
			n.blob(g, 0)
		}
	}

	return nil
}

// enumerateCandidates adds the orphan candidates a listing scrub
// remembered.
func (u *User) enumerateCandidates(n *namedObjects) error {
	seen, err := u.orphanCandidates()
	if err != nil {
		return fmt.Errorf("read scrub state: %w", err)
	}

	for guid := range seen {
		n.blob(guid, 0)
	}

	return nil
}

// fixedObjects are the index objects every account may have: the
// full-text index as its layout record names it — every generation
// (current, "next", "old") before the id map, and the layout 2 objects
// always, also when the record says they are gone: a layout 2 writer
// that slept through a conversion may have left one, and deleting an
// absent object is cheap (one read, N13) — the front-end indexes and the
// account's single objects. An id map that cannot be read leaves the
// model incomplete.
func (u *User) fixedObjects() ([]string, error) {
	lay, err := u.readLayout()
	if err != nil {
		return nil, fmt.Errorf("%w: full-text layout record: %w", errModelIncomplete, err)
	}

	var oids []string

	for _, loc := range nameGens(lay, u.ftsShards) {
		if loc.v3() {
			oids = append(oids, loc.objects()...)
		}
	}

	oids = append(oids, u.layout2Objects()...)

	for _, space := range purgeFixedIndexSpaces {
		for s := range uint32(indexShards) {
			oids = append(oids, indexOID(space, s))
		}
	}

	return append(oids, oidFTSMap, oidAccount, oidMetadata, oidSieve, oidScrub), nil
}

// layout2Objects are the objects of a layout 2 index, of at least the
// default shard count (an account indexed before fts_shards was lowered).
func (u *User) layout2Objects() []string {
	shards := max(u.ftsShards, DefaultFTSShards)
	oids := make([]string, 0, 3*shards+dictShards)

	for i := range shards {
		oids = append(oids, fmt.Sprintf("%s%02d", ftsPrefix, i), fmt.Sprintf("%s%02d", headerPrefix, i),
			fmt.Sprintf("%s%02d", legacyDictPrefix, i))
	}

	for i := range dictShards {
		oids = append(oids, dictShardOID3(i))
	}

	return oids
}

// dictShardOID3 names dictionary shard i ("ftst/<nnn>").
func dictShardOID3(i int) string { return fmt.Sprintf("%s%03d", dictPrefix, i) }

// blobChunks returns the chunk count of a blob whose index record is not
// at hand (from its length; 1 when it has none or is gone).
func (u *User) blobChunks(guid string) int {
	size, err := u.BlobSize(guid)
	if err != nil {
		return 1
	}

	return u.chunkCount(size)
}

// purgeListing reports whether purges list the namespaces (the purge
// before 0.42.0).
func (u *User) purgeListing() bool {
	if u.store == nil {
		return false
	}

	u.store.mu.RLock()
	defer u.store.mu.RUnlock()

	return u.store.cfg.PurgeListing
}

// PurgeNamespace implements model.NamespacePurger: every object of the
// account's namespaces in the data and index pools is deleted, the
// registry (which holds the maintenance lock) last, so protocols keep
// refusing the account until the very end. With purge.listing the
// objects come from a listing of both namespaces, as before 0.42.0, and
// the objects the data model does not name are reported; otherwise from
// the data model (see the comment at the top).
func (u *User) PurgeNamespace(fence func() error) (int, error) {
	// Recorded first: what the purge cannot name, the sweep then removes.
	if err := u.markPurged(); err != nil {
		return 0, err
	}

	named, err := u.enumerate(false)

	if u.purgeListing() {
		if err != nil {
			u.log.Warn("purge: data model not readable, nothing to compare the listing with", "err", err)

			named = nil
		}

		return u.purgeByListing(fence, named)
	}

	if err != nil {
		return 0, err
	}

	// Marks of live public folders written before 0.42.0 have no ps:
	// key; left behind, an account created again under the name would
	// see them as its own.
	named.index = append(named.index, u.publicMarks()...)

	return u.purgeNamed(fence, named)
}

// publicMarks names the \Seen mark objects of every live public folder
// (most do not exist; deleting them costs a round trip each).
func (u *User) publicMarks() []string {
	if u.store == nil || model.IsPublic(u.name) {
		return nil
	}

	live, err := (&scrubRun{u: u, log: u.log}).publicMailboxIDs()
	if err != nil {
		u.log.Warn("purge: public folders not readable, their marks are left to the sweep", "err", err)
		return nil
	}

	oids := make([]string, 0, len(live))
	for id := range live {
		oids = append(oids, oidPrivateSeen+id)
	}

	sort.Strings(oids)

	return oids
}

// purgeBatch is how many objects a purge deletes between two fences.
const purgeBatch = 100

// purgeNamed deletes what the data model names: the blobs first (their
// entries name them until the shards go), then the index objects (the
// shards highest first), the registry last.
func (u *User) purgeNamed(fence func() error, named *namedObjects) (int, error) {
	d := &purgeDeleter{fence: fence}

	guids := make([]string, 0, len(named.blobs))
	for g := range named.blobs {
		guids = append(guids, g)
	}

	sort.Strings(guids)

	for _, guid := range guids {
		chunks := named.blobs[guid]
		if chunks == 0 {
			chunks = u.blobChunks(guid)
		}

		for c := range chunks {
			if err := d.delete(u.data, chunkOID(guid, c)); err != nil {
				return d.removed, err
			}
		}
	}

	for _, oid := range named.index {
		if err := d.delete(u.idx, oid); err != nil {
			return d.removed, err
		}
	}

	return d.removed + 1, u.purgeRegistry(fence, d.removed)
}

// purgeRegistry deletes the registry (with the maintenance lock in it).
// Before, with the full-text index gone, the account leaves the presence
// index of layout 3 (after its objects, as the completeness rule wants).
func (u *User) purgeRegistry(fence func() error, removed int) error {
	if err := fence(); err != nil {
		return err
	}

	if err := u.unmarkLayout3(); err != nil {
		return err
	}

	if err := u.idx.Delete(oidRegistry); err != nil && errCode(err) != syscall.ENOENT {
		return fmt.Errorf("delete registry: %w", err)
	}

	u.invalidate()

	u.log.Info("purge: objects deleted", "objects", removed+1)

	return nil
}

// purgeDeleter deletes objects, fencing before every batch, and counts
// the ones that were there.
type purgeDeleter struct {
	fence   func() error
	n       int
	removed int
}

func (d *purgeDeleter) delete(ioctx *rados.IOContext, oid string) error {
	if d.n%purgeBatch == 0 {
		if err := d.fence(); err != nil {
			return err
		}
	}

	d.n++

	switch err := ioctx.Delete(oid); {
	case err == nil:
		d.removed++
	case errCode(err) != syscall.ENOENT:
		return fmt.Errorf("delete %s: %w", oid, err)
	}

	return nil
}

// purgeByListing is the purge before 0.42.0: every object a listing of
// the two namespaces finds goes, the registry last. With named (the data
// model, read before) it counts and logs the objects the model does not
// name, which a purge without listing would leave behind.
func (u *User) purgeByListing(fence func() error, named *namedObjects) (int, error) {
	var index map[string]bool
	if named != nil {
		index = named.indexSet()
	}

	removed, unnamed := 0, 0

	var samples []string

	for _, pool := range []struct {
		ioctx *rados.IOContext
		data  bool
	}{{u.data, true}, {u.idx, false}} {
		n, err := purgeObjects(pool.ioctx, fence, oidRegistry, func(oid string) {
			if named != nil && !named.names(index, pool.data, oid) {
				unnamed++

				if len(samples) < shadowSamples {
					samples = append(samples, oid)
				}
			}
		})
		removed += n

		if err != nil {
			return removed, err
		}
	}

	if unnamed > 0 {
		metrics.PurgeUnnamed.Add(float64(unnamed))
		u.log.Warn("purge: objects the data model does not name", "objects", unnamed, "samples", samples)
	}

	return removed + 1, u.purgeRegistry(fence, removed)
}

// purgeObjects deletes every object of the IO context's namespace except
// keep, in one listing, fencing before each batch; seen is called with
// every object it deletes.
func purgeObjects(ioctx *rados.IOContext, fence func() error, keep string, seen func(oid string)) (int, error) {
	it, err := listObjects(ioctx, listPurgeObjects)
	if err != nil {
		return 0, err
	}
	defer it.Close()

	removed := 0

	for it.Next() {
		oid := it.Value()
		if oid == keep {
			continue
		}

		if removed%purgeBatch == 0 {
			if err := fence(); err != nil {
				return removed, err
			}
		}

		seen(oid)

		if err := ioctx.Delete(oid); err != nil && errCode(err) != syscall.ENOENT {
			return removed, fmt.Errorf("delete %s: %w", oid, err)
		}

		removed++
	}

	return removed, it.Err()
}
