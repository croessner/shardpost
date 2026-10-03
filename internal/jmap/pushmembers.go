package jmap

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"sort"
	"sync"
	"time"

	"shardpost/internal/model"
	"shardpost/internal/sortedindex"
)

// The membership of the push pods (stage B18a, §4.3): one omap object
// "ix.jmap/push/members" in the admin namespace, one key per pod
// (<instance> -> memberRecord), written every third of the lease and at once
// after every take, release, shed and expiry, each a plain omap set
// without compare-and-swap. A pod reads it at the start of every tick
// and on every control notify, at most one read in flight; what it
// concludes from the record — the share of partitions it should hold,
// which partitions are free, orphaned or being shed — is in §4.4.

// The objects of the partitioned worker in the admin namespace: the
// membership, the control object every pod watches (notified after an
// orderly release), and per partition the lock object, which the
// holder watches for subscription changes (the front-ends notify it
// with the account as payload).
const (
	pushMembersOID = "ix.jmap/push/members"
	pushCtlOID     = "ix.jmap/push/ctl"
	// pushHeartbeatsPerLease is how many times per lease a pod writes its
	// membership record without an event (every 10 s for a lease of 30 s).
	pushHeartbeatsPerLease = 3
	// pushMemberExpiry is after how long a reader deletes a record whose
	// writer is gone (its partitions count as orphaned after
	// lease_seconds already).
	pushMemberExpiry = 10 * time.Minute
	// pushMembersPage is the page of a membership read.
	pushMembersPage = 1024
)

// pushLockName is the lease name of partition p (the lock object is
// sortedindex.LockOID of it: "ix.jmap/push/p<nn>/lock").
func pushLockName(p int) string { return fmt.Sprintf("jmap/push/p%02d", p) }

// pushLockOID is the lock object of partition p.
func pushLockOID(p int) string { return sortedindex.LockOID(pushLockName(p)) }

// PushPartition is the partition of an account among n: the shard of
// its record in the hint index of the subscriptions, which is what the
// worker's scan reads. The front-ends and the worker share this one
// function.
func PushPartition(account string, n int) int { return model.IndexShard(url.PathEscape(account), n) }

// memberRecord is what a pod writes about itself.
type memberRecord struct {
	// Seen is the writer's clock at the write; a reader compares it with
	// its own (NTP on the nodes is a prerequisite).
	Seen time.Time `json:"seen"`
	// Held are the partitions the pod holds, Watched the accounts it
	// watches; MaxHeld and MaxWatched are its bounds (0: none), so that
	// the others can tell whether it has capacity for one more.
	Held       []int  `json:"held"`
	Watched    int    `json:"watched"`
	MaxHeld    int    `json:"max_held,omitempty"`
	MaxWatched int    `json:"max_watched,omitempty"`
	Version    string `json:"version,omitempty"`
	// Leaving marks a pod that ends in order: it is not counted in the
	// share, and its partitions are being shed, not free.
	Leaving bool `json:"leaving,omitempty"`
	// Released records when the pod released a partition in order (for
	// the gap measurement of the next holder); entries older than the
	// lease are dropped.
	Released map[string]time.Time `json:"released,omitempty"`
}

// holds reports whether the record names partition p.
func (m memberRecord) holds(p int) bool { return slices.Contains(m.Held, p) }

// capacity reports whether the pod of the record can take one more
// partition of about est accounts within its bounds.
func (m memberRecord) capacity(est int) bool {
	if m.MaxHeld > 0 && len(m.Held) >= m.MaxHeld {
		return false
	}

	return m.MaxWatched <= 0 || m.Watched+est <= m.MaxWatched
}

// membersView is the membership as last read.
type membersView struct {
	// readAt is when the read completed, on the reader's clock; zero
	// for the view before the first read.
	readAt  time.Time
	members map[string]memberRecord
}

// fresh reports whether a record is of a running pod: seen within the
// lease on the reader's clock at now.
func (v *membersView) fresh(m memberRecord, now time.Time, lease time.Duration) bool {
	return now.Sub(m.Seen) <= lease
}

// names are the instance names of the fresh, not leaving members, the
// set whose stability the settle time measures.
func (v *membersView) names(now time.Time, lease time.Duration) []string {
	var out []string

	for name, m := range v.members {
		if v.fresh(m, now, lease) && !m.Leaving {
			out = append(out, name)
		}
	}

	sort.Strings(out)

	return out
}

// membership reads and writes the membership object. Reads are at most
// one in flight: a tick or a notify that meets a running read waits for
// its result instead of starting a second (no pile of reads on a hung
// object). Writes run one after the other in a goroutine of their own,
// so the order of the held lists is kept and no caller blocks on the
// store.
type membership struct {
	omap     model.AdminOmap
	instance string
	lease    time.Duration
	now      func() time.Time
	log      interface{ Warn(string, ...any) }

	mu      sync.Mutex
	view    *membersView
	reading chan struct{} // closed when the read in flight ends (nil: none)
	// prev is the view before the current one (for "changed since").
	prev *membersView

	writes chan memberWrite
	// closed is set by close (under wmu, which is held across the send
	// of a write, apart from mu so that a full queue does not hold up
	// the reads): a write after it is refused with errMembersClosed
	// instead of sending on the closed channel.
	wmu    sync.Mutex
	closed bool
	// writeHook, when set, is called with every record about to be
	// written (tests record the order of writes, unlocks and notifies).
	writeHook func(rec *memberRecord)
}

// memberWrite is one queued write: the record (nil: delete the key) and
// the channel that gets its result.
type memberWrite struct {
	rec  *memberRecord
	done chan error
}

func newMembership(omap model.AdminOmap, instance string, lease time.Duration, now func() time.Time,
	log interface{ Warn(string, ...any) },
) *membership {
	m := &membership{omap: omap, instance: instance, lease: lease, now: now, log: log,
		view: &membersView{members: map[string]memberRecord{}}, writes: make(chan memberWrite, 64)}

	go m.writer()

	return m
}

// current is the view last read.
func (m *membership) current() *membersView {
	m.mu.Lock()
	defer m.mu.Unlock()

	return m.view
}

// age is how old the current view is at now (a very large value before
// the first read).
func (m *membership) age(now time.Time) time.Duration {
	v := m.current()
	if v.readAt.IsZero() {
		return time.Duration(1<<62 - 1)
	}

	return now.Sub(v.readAt)
}

// read starts a read unless one is in flight and returns a channel that
// is closed when the read (the running one, or the new one) ends. The
// caller waits on it as long as it wants to; the view it then takes
// with current() is the newest there is.
func (m *membership) read() <-chan struct{} {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.reading != nil {
		return m.reading
	}

	done := make(chan struct{})
	m.reading = done

	go m.readOnce(done)

	return done
}

// readOnce reads the object into the view.
func (m *membership) readOnce(done chan struct{}) {
	defer func() {
		m.mu.Lock()
		m.reading = nil
		m.mu.Unlock()
		close(done)
	}()

	members, err := m.load()
	if err != nil {
		m.log.Warn("push worker: read the membership", "err", err)

		return
	}

	m.mu.Lock()
	m.prev, m.view = m.view, &membersView{readAt: m.now(), members: members}
	m.mu.Unlock()
}

// load reads every record and deletes the ones whose writer is long
// gone.
func (m *membership) load() (map[string]memberRecord, error) {
	members := map[string]memberRecord{}

	var expired []string

	now := m.now()
	after := ""

	for {
		page, more, err := m.omap.ReadPage(pushMembersOID, after, pushMembersPage)
		if err != nil {
			return nil, err
		}

		for _, kv := range page {
			after = kv.Key

			var rec memberRecord
			if json.Unmarshal(kv.Value, &rec) != nil {
				expired = append(expired, kv.Key)

				continue
			}

			if now.Sub(rec.Seen) > pushMemberExpiry {
				expired = append(expired, kv.Key)

				continue
			}

			members[kv.Key] = rec
		}

		if !more {
			break
		}
	}

	if len(expired) > 0 {
		_ = m.omap.Write(pushMembersOID, model.OmapAnyVersion, nil, expired)
	}

	return members, nil
}

// changed reports whether the current view differs from the one before
// it in what matters to a waiting taker: the held lists and the
// members.
func (m *membership) changed() bool {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.prev == nil || m.prev.readAt.IsZero() {
		return true
	}

	if len(m.prev.members) != len(m.view.members) {
		return true
	}

	for name, rec := range m.view.members {
		old, ok := m.prev.members[name]
		if !ok || old.Leaving != rec.Leaving || !slices.Equal(old.Held, rec.Held) {
			return true
		}
	}

	return false
}

// errMembersClosed is the result of a write after the membership was
// closed (a release that outlasted the shutdown).
var errMembersClosed = errors.New("the membership is closed")

// write queues the pod's record (nil deletes it) and returns the
// channel its result arrives on; after close the result is
// errMembersClosed at once.
func (m *membership) write(rec *memberRecord) <-chan error {
	done := make(chan error, 1)

	m.wmu.Lock()
	defer m.wmu.Unlock()

	if m.closed {
		done <- errMembersClosed

		return done
	}

	if rec != nil && m.writeHook != nil {
		m.writeHook(rec)
	}

	m.writes <- memberWrite{rec: rec, done: done}

	return done
}

// writer runs the queued writes in order.
func (m *membership) writer() {
	for w := range m.writes {
		var err error

		if w.rec == nil {
			err = m.omap.Write(pushMembersOID, model.OmapAnyVersion, nil, []string{m.instance})
		} else {
			b, _ := json.Marshal(w.rec)
			err = m.omap.Write(pushMembersOID, model.OmapAnyVersion, map[string][]byte{m.instance: b}, nil)
		}

		if err != nil {
			m.log.Warn("push worker: write the membership", "err", err)
		}

		w.done <- err
	}
}

// close ends the writer once the queued writes are done; later writes
// are refused. It may be called more than once.
func (m *membership) close() {
	m.wmu.Lock()
	defer m.wmu.Unlock()

	if !m.closed {
		m.closed = true
		close(m.writes)
	}
}

// pushTargets is the share of n partitions each of the sorted members
// should hold: floor(n / len) for all, plus one for the first n mod len
// (ceil for all would leave the last one empty when len does not divide
// n: 3 × 22 > 64).
func pushTargets(n int, names []string) map[string]int {
	out := map[string]int{}
	if len(names) == 0 {
		return out
	}

	base, extra := n/len(names), n%len(names)

	for i, name := range names {
		out[name] = base
		if i < extra {
			out[name]++
		}
	}

	return out
}

// partitionClass is how a pod classifies a partition it does not hold,
// from the view it last read (§4.4, second table).
type partitionClass int

const (
	classHeld     partitionClass = iota // a fresh member holds it: no try
	classShedding                       // a fresh, leaving member holds it: no try
	classFree                           // no fresh member holds it: try within the target
	classOrphan                         // a stale member holds it and the view is fresh: try at once, over the target
	classUnknown                        // a stale member holds it, the view is older than a cadence: no try
)

// classifyPartition classifies partition p from the view at now; age is the age
// of the view and take the cadence.
func classifyPartition(v *membersView, p int, now time.Time, lease, age, take time.Duration) partitionClass {
	stale := false

	for _, rec := range v.members {
		if !rec.holds(p) {
			continue
		}

		switch {
		case !v.fresh(rec, now, lease):
			stale = true
		case rec.Leaving:
			return classShedding
		default:
			return classHeld
		}
	}

	switch {
	case !stale:
		return classFree
	case age <= take:
		return classOrphan
	default:
		return classUnknown
	}
}

// unowned counts the partitions of n no fresh member holds.
func unowned(v *membersView, n int, now time.Time, lease time.Duration) int {
	held := map[int]bool{}

	for _, rec := range v.members {
		if !v.fresh(rec, now, lease) {
			continue
		}

		for _, p := range rec.Held {
			held[p] = true
		}
	}

	return n - len(held)
}
