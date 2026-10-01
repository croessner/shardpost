package cephstore

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/ceph/go-ceph/rados"
)

// Intents (scaling plan B6)
//
// Every write that can leave an object behind which nothing else points
// at records an intent first, in an object the scrubber reads anyway, and
// removes it when the write is complete. A scrub then finds what a crash
// left behind by reading these keys instead of listing the pools:
//
//	registry ("mailboxes"):
//	  "bi:<guid>"       a new blob of a delivery (DeliverGUID), set in the
//	                    reserve CAS, removed once the entry is linked
//	  "bimp:<batch>"    the blobs of an import step, removed after the
//	                    header write of the step
//	  "up:<guid>"       a JMAP upload, removed when it expires
//	  "ci:<id>"         a mailbox object being created, removed in the CAS
//	                    that registers its name
//	  "dm:<id>"         a mailbox being deleted (tombstone with its range),
//	                    removed when its objects are gone
//	  "ps:<id>"         the account has private \Seen marks of the public
//	                    folder id ("pubseen/<id>"); stays while they exist
//	shard 0 of a mailbox ("mbox/<id>"):
//	  "r:<uid>"         an entry removed whose blob reference is not yet
//	                    dropped, set in the header CAS of the removal
//	  "h:retired"       UIDNEXT of a mailbox object retired by DELETE or
//	                    an import replacement (h:uidnext is 0 then)
//
// Older servers read none of these keys (they read m:, d:, i:, c:, v:
// and the header by name), so mixed versions are safe; their writes just
// leave no intents, which only the listing scrub then covers.
const (
	regBlobPrefix       = "bi:"
	regImportBlobPrefix = "bimp:"
	regUploadPrefix     = "up:"
	regCreatePrefix     = "ci:"
	regDeletePrefix     = "dm:"
	regPubSeenPrefix    = "ps:"
	keyRemovePrefix     = "r:"
	keyRetired          = "h:retired"
)

// intentFlushDelay bounds how long a view keeps the keys of finished
// intents before it removes them in a write of its own; until then they
// ride along with the next registry write of the view (see intentBatch).
const intentFlushDelay = 30 * time.Second

// blobIntent is the value of "bi:" and "up:": when the intent was set, the
// size and chunk count of the blob (so that a scrub can unindex and
// delete it without reading anything else) and, for an upload, when it
// expires.
type blobIntent struct {
	Time    int64 `json:"t"`
	Size    int64 `json:"s"`
	Chunks  int   `json:"c"`
	Expires int64 `json:"x,omitempty"`
}

// blobRef names one blob of an import step or a removal.
type blobRef struct {
	GUID   string `json:"g"`
	Size   int64  `json:"s"`
	Chunks int    `json:"c"`
}

// importIntent is the value of "bimp:": the blobs an import step may
// write and the mailbox. Base holds each message's GUID; the GUIDs it may
// fall back to follow from it and the step's salt (importFallback), so
// the value stays at about 35 bytes per message (a step of 10000
// messages, the largest admin.import.max_batch_messages allows, needs
// about 350 KiB, well below the 1 MiB an omap value may have).
type importIntent struct {
	Time    int64    `json:"t"`
	Mailbox string   `json:"m"`
	Salt    string   `json:"s"`
	Base    []string `json:"g"`
}

// importFallbacks is how many fallback GUIDs a message of an import step
// may use (see importBlob).
const importFallbacks = 2

// importFallback is fallback k of a message's base GUID in a step.
func importFallback(salt, base string, k int) string {
	sum := sha256.Sum256([]byte(salt + "/" + base + "/" + strconv.Itoa(k)))

	return hex.EncodeToString(sum[:guidBytes])
}

// guids returns every GUID the step may have written.
func (im importIntent) guids() []string {
	out := make([]string, 0, len(im.Base)*(1+importFallbacks))

	for _, b := range im.Base {
		out = append(out, b)
		for k := 1; k <= importFallbacks; k++ {
			out = append(out, importFallback(im.Salt, b, k))
		}
	}

	return out
}

// mailboxIntent is the value of "ci:" and "dm:": the name and, for a
// deletion, the range of the mailbox as it was read before (the scrubber
// prefers the header of shard 0 while it exists).
type mailboxIntent struct {
	Time      int64  `json:"t"`
	Name      string `json:"n,omitempty"`
	UIDNext   uint32 `json:"u,omitempty"`
	ShardSize uint32 `json:"s,omitempty"`
}

// removeIntent is the value of "r:<uid>".
type removeIntent struct {
	Time int64 `json:"t"`
	blobRef
}

func blobIntentKey(guid string) string { return regBlobPrefix + guid }
func uploadKey(guid string) string     { return regUploadPrefix + guid }
func createKey(id string) string       { return regCreatePrefix + id }
func deleteKey(id string) string       { return regDeletePrefix + id }
func pubSeenKey(id string) string      { return regPubSeenPrefix + id }
func removeKey(uid uint32) string      { return fmt.Sprintf("%s%010d", keyRemovePrefix, uid) }

// intentTime reads the time an intent was set (0 for a value that does
// not parse, which makes it old).
func intentTime(v []byte) time.Time {
	var t struct {
		Time int64 `json:"t"`
	}

	_ = json.Unmarshal(v, &t)

	return time.Unix(t.Time, 0)
}

func encode(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}

// newBlobIntent is the "bi:" value of a blob of size bytes.
func (u *User) newBlobIntent(size int64) []byte {
	return encode(blobIntent{Time: time.Now().Unix(), Size: size, Chunks: u.chunkCount(size)})
}

// intentBatch holds the registry keys of intents this view has finished
// and not yet removed. They are removed with the next registry write of
// the view that can carry them (a reservation, the end of a delivery),
// after intentFlushDelay at the latest, and when the view is closed: a
// session that appends many messages pays no write of its own for them,
// and LMTP, which opens a view per delivery, one per delivery.
type intentBatch struct {
	mu    sync.Mutex
	keys  map[string]struct{}
	timer *time.Timer
}

// intentDone queues finished registry intents for removal.
func (u *User) intentDone(keys ...string) {
	if len(keys) == 0 {
		return
	}

	b := &u.intents

	b.mu.Lock()
	defer b.mu.Unlock()

	if b.keys == nil {
		b.keys = map[string]struct{}{}
	}

	for _, k := range keys {
		b.keys[k] = struct{}{}
	}

	if b.timer == nil {
		u.inflight.Add(1)

		b.timer = time.AfterFunc(intentFlushDelay, func() {
			defer u.inflight.Done()

			u.flushIntents(true)
		})
	}
}

// intentAgain drops a key from the batch: the intent is being set again
// (a delivery retried under the same GUID).
func (u *User) intentAgain(key string) {
	b := &u.intents

	b.mu.Lock()
	delete(b.keys, key)
	b.mu.Unlock()
}

// takeIntents empties the batch and returns its keys; a write that fails
// to carry them gives them back with restoreIntents.
func (u *User) takeIntents() []string {
	b := &u.intents

	b.mu.Lock()
	defer b.mu.Unlock()

	if len(b.keys) == 0 {
		return nil
	}

	out := make([]string, 0, len(b.keys))
	for k := range b.keys {
		out = append(out, k)
	}

	b.keys = nil

	return out
}

// restoreIntents puts keys back into the batch.
func (u *User) restoreIntents(keys []string) {
	if len(keys) == 0 {
		return
	}

	b := &u.intents

	b.mu.Lock()
	defer b.mu.Unlock()

	if b.keys == nil {
		b.keys = map[string]struct{}{}
	}

	for _, k := range keys {
		b.keys[k] = struct{}{}
	}
}

// flushIntents removes the queued keys in one write. fromTimer is set
// when the timer runs it (it then clears the timer).
func (u *User) flushIntents(fromTimer bool) {
	if fromTimer {
		u.intents.mu.Lock()
		u.intents.timer = nil
		u.intents.mu.Unlock()
	}

	keys := u.takeIntents()
	if len(keys) == 0 {
		return
	}

	if err := rmKeys(u.idx, oidRegistry, keys); err != nil {
		// Left to the scrubber, which drops intents whose work is done.
		u.log.Warn("finished intents not removed", "keys", len(keys), "err", err)
	}
}

// stopIntentTimer stops the flush timer of a view being closed; Close
// flushes the batch itself.
func (u *User) stopIntentTimer() {
	b := &u.intents

	b.mu.Lock()
	t := b.timer
	b.timer = nil
	b.mu.Unlock()

	if t != nil && t.Stop() {
		u.inflight.Done()
	}
}

// rmKeys removes omap keys of an existing object in one write; an absent
// object is not created (and is no error).
func rmKeys(ioctx *rados.IOContext, oid string, keys []string) error {
	if len(keys) == 0 {
		return nil
	}

	op := rados.CreateWriteOp()
	defer op.Release()

	op.AssertExists()
	op.RmOmapKeys(keys)

	if err := op.Operate(ioctx, oid, rados.OperationNoFlag); err != nil && errCode(err) != syscall.ENOENT {
		return fmt.Errorf("remove keys of %s: %w", oid, err)
	}

	return nil
}

// setIntent sets registry keys in a CAS of their own, together with the
// finished intents of the batch.
func (u *User) setIntent(set map[string][]byte) error {
	done := u.takeIntents()

	err := casOmap(u, u.idx, oidRegistry, nil, func(map[string][]byte) (map[string][]byte, []string) {
		return set, done
	})
	if err != nil {
		u.restoreIntents(done)

		return fmt.Errorf("record intent: %w", err)
	}

	return nil
}

// anyKey reports the first of the prefixes under which the object has a
// key ("" for none; an absent object has none). It is one read op with a
// bounded step per prefix.
func (u *User) anyKey(oid string, prefixes ...string) (string, error) {
	op := rados.CreateReadOp()
	defer op.Release()

	steps := make([]*rados.GetOmapStep, len(prefixes))
	for i, p := range prefixes {
		steps[i] = op.GetOmapValues("", p, 1)
	}

	if err := op.Operate(u.idx, oid, rados.OperationNoFlag); err != nil {
		if errCode(err) == syscall.ENOENT {
			return "", nil
		}

		return "", err
	}

	for i, s := range steps {
		kv, err := s.Next()
		if err != nil {
			return "", err
		}

		if kv != nil {
			return prefixes[i], nil
		}
	}

	return "", nil
}

// intentPrefixes are the registry prefixes of intents a scrub has to look
// at (ps: is not one: it stays while the marks exist; up: only once it
// expired, see expiredUpload).
var intentPrefixes = []string{regDeliveryPrefix, regBlobPrefix, regImportBlobPrefix, regCreatePrefix, regDeletePrefix}

// uploadPage bounds the upload intents ScrubPending reads.
const uploadPage = 64

// expiredUpload reports whether the first uploadPage upload intents hold
// one that expired (a live upload keeps no account from being idle; the
// expiry worker removes it when it is due, the scrub only if the worker
// did not get to it).
func (u *User) expiredUpload(now time.Time) (bool, error) {
	vals, err := u.idx.GetOmapValues(oidRegistry, "", regUploadPrefix, uploadPage)
	if err != nil {
		if errCode(err) == syscall.ENOENT {
			return false, nil
		}

		return false, err
	}

	for _, v := range vals {
		var in blobIntent
		if json.Unmarshal(v, &in) != nil || in.Expires <= now.Unix() {
			return true, nil
		}
	}

	return false, nil
}

// registryIntents are the intents of the registry, by prefix and then by
// the key without prefix.
type registryIntents map[string]map[string][]byte

// readIntents reads the intents of the registry (one read per prefix,
// paged).
func (u *User) readIntents(prefixes ...string) (registryIntents, error) {
	out := registryIntents{}

	for _, p := range prefixes {
		vals, err := u.idx.GetAllOmapValues(oidRegistry, "", p, omapPageSize)
		if err != nil {
			if errCode(err) == syscall.ENOENT {
				return out, nil
			}

			return nil, fmt.Errorf("read intents %s: %w", p, err)
		}

		m := make(map[string][]byte, len(vals))
		for k, v := range vals {
			m[strings.TrimPrefix(k, p)] = v
		}

		out[p] = m
	}

	return out, nil
}

// removeIntents reads the "r:" keys of a mailbox's shard 0.
func (u *User) removeIntents(id string) (map[string]removeIntent, error) {
	vals, err := u.idx.GetAllOmapValues(mboxOID(id), "", keyRemovePrefix, omapPageSize)
	if err != nil {
		if errCode(err) == syscall.ENOENT {
			return nil, nil
		}

		return nil, err
	}

	out := make(map[string]removeIntent, len(vals))

	for k, v := range vals {
		var ri removeIntent
		if json.Unmarshal(v, &ri) == nil && ri.GUID != "" {
			out[k] = ri
		} else {
			out[k] = removeIntent{} // unreadable: dropped by the scrub
		}
	}

	return out, nil
}

// removalSet returns the "r:" keys of a removal: one per entry that the
// header CAS removed from shard 0 or announced for a higher shard, with
// the blob each entry referenced.
func removalSet(now int64, metas []Meta) map[string][]byte {
	set := make(map[string][]byte, len(metas))

	for i := range metas {
		m := &metas[i]
		set[removeKey(m.UID)] = encode(removeIntent{Time: now, blobRef: blobRef{GUID: m.GUID, Size: m.Size, Chunks: m.Chunks}})
	}

	return set
}

// releaseRemoved drops the blob references of entries an unlink removed
// and then their "r:" keys (of rkeys, the keys the removal set). The key
// of an entry whose release failed stays (the scrubber recounts its
// blob), and so does the key of an entry the removal announced but
// another session removed first: that session releases the blob and
// drops the key (both set the same one), and dropping it here could take
// it away before that release.
func (u *User) releaseRemoved(mb *Mailbox, removed []Meta, rkeys []string) int {
	failed := 0
	done := map[string]bool{}

	for i := range removed {
		if err := u.releaseBlob(&removed[i]); err != nil {
			u.log.Warn("orphan blob", "guid", removed[i].GUID, "err", err)

			failed++

			continue
		}

		done[removeKey(removed[i].UID)] = true
	}

	keep := rkeys[:0:0]

	for _, k := range rkeys {
		if done[k] {
			keep = append(keep, k)
		}
	}

	if err := rmKeys(u.idx, mboxOID(mb.ID), keep); err != nil {
		u.log.Warn("removal intents not dropped", "mailbox", mb.Name, "err", err)
	}

	return failed
}

// retiredUIDNext reads UIDNEXT of a header that may be retired: the
// value kept under h:retired then.
func retiredUIDNext(vals map[string][]byte) uint32 {
	n, _ := strconv.ParseUint(string(vals[keyUIDNext]), 10, 32)
	if n == 0 {
		n, _ = strconv.ParseUint(string(vals[keyRetired]), 10, 32)
	}

	return uint32(n)
}
