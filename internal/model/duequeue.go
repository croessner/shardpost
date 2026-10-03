package model

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// DueQueue is a completeness index of things that become due at a time
// (scaling plan B1): every record that has to be visited at some point
// has an entry "<due>/<id>/<gen>", so a worker finds what is due by
// reading the heads of the queue's shards instead of listing a pool.
//
// The entries of one id all live in shard IndexShard(id, n), so moving an
// entry (Move) is one write op on one object: it removes the old keys and
// sets the new one atomically. Moves are not compare-and-swap; the
// record stays the fence. Two writers that both move from the same key
// can leave a duplicate with a foreign gen: readers compare an entry with
// its record, skip what does not match and repair or drop it (for an
// entry without record only after DueQueueGrace, because the entry is
// written before its record).
//
// Writers create an entry before its record and remove it after the
// record; a crash in between leaves an entry without record, never a
// record without entry.
type DueQueue interface {
	// Move removes the entries from and sets to (nil: only removes) of
	// id in one write. A key in from equal to to is kept.
	Move(id string, from []DueKey, to *DueKey) error
	// Due returns up to limit entries due at or before now, earliest
	// first.
	Due(now time.Time, limit int) ([]DueEntry, error)
	// DueAfter is Due for the entries whose keys sort after the key
	// after (DueEntry.Key of the last entry of the previous page): a
	// reader that skips entries (a partitioned worker) pages through
	// everything that is due.
	DueAfter(now time.Time, after string, limit int) ([]DueEntry, error)
	// Head returns the head of the queue (shard count, markers).
	Head() (IndexHead, error)
	// Watch watches the queue's shard 0, which writers Notify when an
	// entry becomes due earlier than before.
	Watch() (Watch, error)
	// Notify wakes the watchers of the queue (best effort).
	Notify()
}

// DueKey is the position of an entry: when it is due and the generation
// of the record it was written for.
type DueKey struct {
	Due time.Time
	Gen uint64
}

// DueEntry is one entry of a DueQueue.
type DueEntry struct {
	ID string
	DueKey
}

// Key is the omap key of the entry.
func (e DueEntry) Key() string { return FormatDueKey(e.ID, e.DueKey) }

// Queue names and bounds.
const (
	// JobsClaimableQueue holds every job a runner may have to claim, at
	// the time it becomes claimable (queued: created; running: the end of
	// its lease).
	JobsClaimableQueue = "jobs/claimable"
	// JobsExpiryQueue holds every job, plan and idempotency key at the
	// time it expires.
	JobsExpiryQueue = "jobs/expiry"
	// LearningQueue holds every learning job (namespace "~learning" of
	// the index pool) at the time of its next attempt; a dead job sits
	// at DeadDue, after every live one.
	LearningQueue = "learning"
	// UploadsQueue holds every JMAP upload (namespace "~accounts" of the
	// index pool) at the time it expires; the id is "<account>/<guid>".
	UploadsQueue = "uploads"
	// SubmissionQueue holds every delayed JMAP submission (namespace
	// "~admin" of the index pool) at the time its record has to be
	// visited next: a pending one at its release time, one being sent at
	// the end of its sending timeout, a finished one at once (its
	// clean-up); the id is "<account>/<submission id>".
	SubmissionQueue = "submission"

	// DefaultQueueShards and MaxQueueShards bound the shards of a queue.
	DefaultQueueShards = 4
	MaxQueueShards     = 8
	// DueQueueGrace is how long a reader leaves an entry alone whose
	// record it cannot find or whose generation is newer than the
	// record's: the writer may still be between the two writes.
	DueQueueGrace = 10 * time.Minute
)

// dueDigits is the width of the due time (Unix nanoseconds) in a key, so
// that keys sort by due time.
const dueDigits = 20

// ErrDueKey rejects a key that is not "<due>/<id>/<gen>".
var ErrDueKey = errors.New("invalid queue key")

// FormatDueKey is the key of id at k: "<due 20 digits>/<id>/<gen>". A
// due time before 1970 is stored as 0.
func FormatDueKey(id string, k DueKey) string {
	ns := max(k.Due.UnixNano(), 0)

	return fmt.Sprintf("%0*d/%s/%d", dueDigits, ns, id, k.Gen)
}

// ParseDueKey splits a queue key.
func ParseDueKey(key string) (DueEntry, error) {
	if len(key) < dueDigits+3 || key[dueDigits] != '/' {
		return DueEntry{}, fmt.Errorf("%w %q", ErrDueKey, key)
	}

	ns, err := strconv.ParseInt(key[:dueDigits], 10, 64)
	if err != nil {
		return DueEntry{}, fmt.Errorf("%w %q", ErrDueKey, key)
	}

	rest := key[dueDigits+1:]

	i := strings.LastIndexByte(rest, '/')
	if i <= 0 {
		return DueEntry{}, fmt.Errorf("%w %q", ErrDueKey, key)
	}

	gen, err := strconv.ParseUint(rest[i+1:], 10, 64)
	if err != nil {
		return DueEntry{}, fmt.Errorf("%w %q", ErrDueKey, key)
	}

	return DueEntry{ID: rest[:i], DueKey: DueKey{Due: time.Unix(0, ns).UTC(), Gen: gen}}, nil
}

// SameDueKey reports whether a and b name the same entry position (the
// due time compared at key resolution).
func SameDueKey(a, b DueKey) bool {
	return a.Gen == b.Gen && max(a.Due.UnixNano(), 0) == max(b.Due.UnixNano(), 0)
}

// KVIndexMode is how the admin store uses its hint indexes and queues
// (configuration admin.kv_index).
type KVIndexMode string

// Modes of the admin store indexes.
const (
	// KVIndexOff writes no index and no queue (the behaviour before the
	// indexes existed).
	KVIndexOff KVIndexMode = "off"
	// KVIndexWrite keeps the indexes and queues up to date; readers still
	// list the namespace.
	KVIndexWrite KVIndexMode = "write"
	// KVIndexRead also reads from them: listings of indexed collections
	// come from the hint index and the job runner claims from the queue.
	// It needs a completed "shardpost index backfill kv" that started
	// after the last server without the indexes stopped.
	KVIndexRead KVIndexMode = "read"
)

// ParseKVIndexMode checks a configured mode ("" = KVIndexWrite).
func ParseKVIndexMode(s string) (KVIndexMode, error) {
	switch m := KVIndexMode(s); m {
	case "":
		return KVIndexWrite, nil
	case KVIndexOff, KVIndexWrite, KVIndexRead:
		return m, nil
	default:
		return "", fmt.Errorf("admin.kv_index: unknown mode %q (off, write, read)", s)
	}
}

// IndexedKV is an admin store with hint indexes over its collections and
// the due queues of the job system (scaling plan B3). The records stay
// objects of their own in every mode.
type IndexedKV interface {
	AdminKV
	// IndexMode is the configured mode.
	IndexMode() KVIndexMode
	// Queue opens the due queue name (JobsClaimableQueue,
	// JobsExpiryQueue).
	Queue(name string) (DueQueue, error)
	// ListIndex returns up to limit entries of the hint index of the
	// collection prefix names (for example "job/") whose keys sort after
	// after, in key order, with their full admin store keys.
	// ErrNotIndexed when prefix is not exactly a collection.
	ListIndex(prefix, after string, limit int) ([]KVEntry, error)
	// LockQueue takes the exclusive worker lease of a queue (a cls_lock
	// that is renewed while held). ErrLocked when another worker holds
	// it. A key rotation of the store gives the lease up and marks it
	// lost; its holder takes it again.
	LockQueue(name string, ttl time.Duration) (Lease, error)
	// LockQueueKeep is LockQueue for a lease that outlives a key rotation
	// (the push worker's partitions, stage B18a): the store leaves it
	// standing, and its holder moves it to the new connection itself
	// (LeaseRetaker) when the store tells it (a rotation hook). The lease
	// implements LeaseDeadline.
	LockQueueKeep(name string, ttl time.Duration) (Lease, error)
}

// LeaseDeadline is implemented by a lease that knows until when it
// holds: the time its last successful take or renewal was sent plus the
// TTL, measured on the holder's monotonic clock. A holder that stops
// its work before that time is up can never overlap with a successor.
type LeaseDeadline interface {
	ValidUntil() time.Time
}

// LeaseRetaker is implemented by a lease that can be moved to a new
// connection after a key rotation: Retake unlocks through the old
// connection, locks again through the new one with a new cookie and
// renews there on. An error means the lease is lost (another holder
// was quicker in the gap).
type LeaseRetaker interface {
	Retake() error
}

// ErrNotIndexed is returned by ListIndex for a prefix that is not an
// indexed collection.
var ErrNotIndexed = errors.New("not an indexed collection")
