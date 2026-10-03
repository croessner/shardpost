package jmap

import (
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"time"
)

// The way over from the single push pod of before 0.52.0 (stage B18a,
// §4.11): that pod held the admin record jmap/push/lease, renewed every
// 30 s with a TTL of 90 s, and took an unreadable or expired record as
// free. The partitioned pods keep that record occupied as a blocking
// marker, with the holder "partitioned/<instance>", so that an old pod
// that still runs (a rollout, a rollback) waits; and they take no
// partition while an old pod holds the record, so that a rolling update
// hands over without two kinds of worker pushing at once. The record is
// refreshed by every pod that holds a partition, and deleted at the
// orderly end of the last one.

// pushLegacyPrefix marks a holder that is a partitioned pod.
const pushLegacyPrefix = "partitioned/"

// legacyMarker is the pod's side of the record.
type legacyMarker struct {
	w *worker

	mu sync.Mutex
	// until is the expiry of the marker as this pod last wrote or read
	// it; oldHolder is set while an old pod holds the record; checked
	// is when the record was last read.
	until     time.Time
	oldHolder bool
	checked   time.Time
	// known is set once the first read came back: until then the pod
	// waits, so it never takes a partition before it looked.
	known bool
}

// errOldHolder reports a record an old pod holds.
var errOldHolder = errors.New("an old push worker holds the lease")

// waiting reports whether an old pod holds the record at now (reading it
// again every pushLegacyPoll while one does, so the pod notices the
// release within two seconds).
func (m *legacyMarker) waiting(now time.Time) bool {
	m.mu.Lock()
	old, checked, known := m.oldHolder, m.checked, m.known
	m.mu.Unlock()

	if known && !old {
		return false
	}

	if checked.IsZero() || now.Sub(checked) >= pushLegacyPoll {
		m.refreshNow(now, false)
	}

	return true
}

// ensure writes the marker before the pod takes a partition, unless the
// pod wrote it lately.
func (m *legacyMarker) ensure(now time.Time) {
	m.mu.Lock()
	fresh := !m.oldHolder && m.until.Sub(now) >= pushLegacyRefresh
	m.mu.Unlock()

	if !fresh {
		m.refreshNow(now, true)
	}
}

// refresh is the tick's refresh: a pod that holds a partition renews the
// marker once less than pushLegacyRefresh remains on it.
func (m *legacyMarker) refresh(now time.Time) {
	if len(m.w.heldList()) == 0 {
		return
	}

	m.mu.Lock()
	due := m.until.Sub(now) < pushLegacyRefresh
	m.mu.Unlock()

	if due {
		m.refreshNow(now, true)
	}
}

// refreshNow reads the record and, with write, takes or renews it as
// the marker unless an old pod holds it. It runs in a goroutine, so the
// loop does not wait on the store; the result shows at the next look.
func (m *legacyMarker) refreshNow(now time.Time, write bool) {
	m.mu.Lock()
	m.checked = now
	m.mu.Unlock()

	go func() {
		w := m.w
		holder := pushLegacyPrefix + w.o.Instance

		var old bool

		err := w.o.KV.Update(pushLeaseKey, func(cur []byte) ([]byte, error) {
			var l pushLease
			if cur != nil && json.Unmarshal(cur, &l) == nil && l.Holder != "" && !strings.HasPrefix(l.Holder, pushLegacyPrefix) &&
				l.Until.After(time.Now()) {
				old = true

				return nil, errOldHolder
			}

			if !write {
				return nil, errOldHolder // read only
			}

			return json.Marshal(pushLease{Holder: holder, Until: time.Now().Add(pushLegacyTTL)})
		})

		m.mu.Lock()
		defer m.mu.Unlock()

		wasOld := m.oldHolder
		m.oldHolder, m.known = old, true

		if !old && (wasOld || !write) {
			// The pod may take now: a cadence out of turn.
			select {
			case w.retick <- struct{}{}:
			default:
			}
		}

		switch {
		case old:
			if !wasOld {
				w.o.Log.Info("push worker: an old push worker holds the lease; waiting for it to release", "event", "push_legacy_wait")
			}
		case err == nil && write:
			m.until = now.Add(pushLegacyTTL)
		case errors.Is(err, errOldHolder):
		case err != nil:
			w.o.Log.Warn("push worker: legacy lease record", "err", err)
		}
	}()
}

// clear deletes the marker at the orderly end when no other fresh,
// not leaving pod is left; else it is left to its TTL. Deleting too
// early costs duplicates at most (an old pod would take the lease and
// push beside the new ones until they write the marker again).
func (m *legacyMarker) clear() {
	w := m.w
	v := w.members.current()
	now := w.now()

	for name, rec := range v.members {
		if name != w.o.Instance && v.fresh(rec, now, w.o.LeaseTTL) && !rec.Leaving {
			return
		}
	}

	// Any partitioned pod's marker goes: the other pods are gone or
	// leaving.
	_ = w.o.KV.Update(pushLeaseKey, func(cur []byte) ([]byte, error) {
		var l pushLease
		if cur == nil || json.Unmarshal(cur, &l) != nil || !strings.HasPrefix(l.Holder, pushLegacyPrefix) {
			return nil, errOldHolder
		}

		return nil, nil
	})
}
