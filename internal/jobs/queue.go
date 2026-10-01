package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"time"

	"shardpost/internal/kvindex"
	"shardpost/internal/metrics"
	"shardpost/internal/model"
)

// The due queues of the job system (scaling plan B3).
//
// jobs/claimable holds every job that a runner may have to claim, at the
// time it becomes claimable: a queued job at its creation time, a
// running one at the end of its lease, generation Lease.Gen. Every write
// that changes the lease end or the state moves the entry after the
// record, from the position computed from the record as it was read to
// the one of the record as written (Manager.updateJob); submit writes
// the entry before the record. A cancel request does not change the
// position. Due(now) is therefore the set claimable() selects, and the
// compare-and-swap on the record stays the fence.
//
// jobs/expiry holds every job, plan and idempotency key at the time it
// expires (the retention of cleanup()): a job at submit with a
// placeholder at Created + retention (generation 0), moved to
// Finished + retention (generation 1) when it ends; its idempotency key
// with the same positions; a plan at Expires + retention. One worker of
// the cluster (a renewed cls_lock) deletes what is due.

// queues are the job system's due queues (nil when the admin store has
// none or its mode is off).
type queues struct {
	kv        model.IndexedKV
	claimable model.DueQueue
	expiry    model.DueQueue
}

func newQueues(kv model.AdminKV) *queues {
	ikv, ok := kv.(model.IndexedKV)
	if !ok || ikv.IndexMode() == model.KVIndexOff {
		return nil
	}

	c, err := ikv.Queue(model.JobsClaimableQueue)
	if err != nil {
		return nil
	}

	e, err := ikv.Queue(model.JobsExpiryQueue)
	if err != nil {
		return nil
	}

	return &queues{kv: ikv, claimable: c, expiry: e}
}

// reads reports whether readers use the queues and indexes.
func (q *queues) reads() bool { return q != nil && q.kv.IndexMode() == model.KVIndexRead }

// Generations of expiry entries.
const (
	expiryPlaceholder uint64 = 0
	expiryFinal       uint64 = 1
)

// claimKey is the position of j in jobs/claimable (false: none, the job
// has ended).
func claimKey(j *Job) (model.DueKey, bool) {
	switch {
	case j.Terminal():
		return model.DueKey{}, false
	case j.State == StateQueued:
		return model.DueKey{Due: j.Created, Gen: j.Lease.Gen}, true
	default:
		return model.DueKey{Due: j.Lease.Until, Gen: j.Lease.Gen}, true
	}
}

// expiryKey is the position of a job (and its idempotency key) in
// jobs/expiry: Finished + retention once it ended, else the placeholder
// at Created + retention.
func expiryKey(j *Job, retention time.Duration) model.DueKey {
	if j.Terminal() {
		return model.DueKey{Due: j.Finished.Add(retention), Gen: expiryFinal}
	}

	return model.DueKey{Due: j.Created.Add(retention), Gen: expiryPlaceholder}
}

// planExpiryKey is the position of a plan in jobs/expiry.
func planExpiryKey(p *Plan, retention time.Duration) model.DueKey {
	return model.DueKey{Due: p.Expires.Add(retention), Gen: expiryPlaceholder}
}

// updateJob applies fn to the stored job and moves its queue entries to
// match the record as written: extra are further positions of the job's
// claimable entry to remove (a stale entry its reader found).
func (m *Manager) updateJob(id string, extra []model.DueKey, fn func(j *Job) error) (*Job, error) {
	var pre Job

	j, err := m.st.updateJob(id, func(j *Job) error {
		pre = *j
		return fn(j)
	})
	if err != nil {
		return nil, err
	}

	m.afterUpdate(&pre, j, extra)

	return j, nil
}

// afterUpdate moves the queue entries of a job after its record changed
// from pre to post. Errors are logged: the record stands, and readers
// repair an entry that lags behind it.
func (m *Manager) afterUpdate(pre, post *Job, extra []model.DueKey) {
	if m.q == nil {
		return
	}

	m.moveClaimable(pre, post, extra)

	if !pre.Terminal() && post.Terminal() {
		m.expireAt(post, []model.DueKey{expiryKey(pre, m.cfg.Retention)})
	}
}

// moveClaimable moves the claimable entry of a job from its position in
// pre (and the extra ones) to its position in post, and wakes the
// runners when it became due earlier.
func (m *Manager) moveClaimable(pre, post *Job, extra []model.DueKey) {
	pk, hadPre := claimKey(pre)
	nk, hasPost := claimKey(post)

	if hadPre && hasPost && model.SameDueKey(pk, nk) && len(extra) == 0 {
		return // a checkpoint, progress or a cancel request
	}

	from := extra
	if hadPre {
		from = append(from, pk)
	}

	var to *model.DueKey
	if hasPost {
		to = &nk
	}

	if err := m.q.claimable.Move(post.ID, from, to); err != nil {
		m.env.Log.Warn("jobs: claimable queue not updated", "job", post.ID, "err", err)
		return
	}

	if hasPost && (!hadPre || nk.Due.Before(pk.Due)) {
		m.q.claimable.Notify() // due earlier: wake the runners
	}
}

// expireAt moves the expiry entries of an ended job (and of its
// idempotency key) from the positions from to its final one.
func (m *Manager) expireAt(j *Job, from []model.DueKey) {
	to := expiryKey(j, m.cfg.Retention)

	if err := m.q.expiry.Move(prefixJob+j.ID, from, &to); err != nil {
		m.env.Log.Warn("jobs: expiry queue not updated", "job", j.ID, "err", err)
	}

	if j.IdemKey != "" {
		if err := m.q.expiry.Move(j.IdemKey, from, &to); err != nil {
			m.env.Log.Warn("jobs: expiry queue not updated", "job", j.ID, "err", err)
		}
	}
}

// stale counts a queue entry that did not match its record.
func stale(queue, action string) { metrics.QueueStale.WithLabelValues(queue, action).Inc() }

// Actions on stale entries (label "action" of
// imapceph_queue_stale_entries_total).
const (
	staleRescheduled = "rescheduled"
	staleRemoved     = "removed"
	staleSkipped     = "skipped"
)

// pollDue claims due jobs from jobs/claimable (read mode): an entry
// whose record is claimable is claimed (the claim removes the entry the
// reader found as well), one whose record is not claimable is moved to
// the record's position, one whose record has ended is removed, and one
// without record is removed after the grace (its submit may still be
// storing the record).
func (m *Manager) pollDue(ctx context.Context, sem chan struct{}, wg *sync.WaitGroup) {
	const page = 100

	for round := 0; round < 10; round++ {
		now := m.now()

		due, err := m.q.claimable.Due(now, page)
		if err != nil {
			m.env.Log.Warn("jobs: due", "err", err)
			return
		}

		progressed := false

		for _, e := range due {
			claimed, moved, full := m.dueEntry(ctx, e, now, sem, wg)
			if full {
				return // busy; later polls continue
			}

			progressed = progressed || claimed || moved
		}

		if len(due) < page || !progressed {
			return
		}
	}
}

// dueEntry handles one due entry of jobs/claimable; it reports whether
// it claimed the job, whether it moved or removed the entry, and whether
// the runner had no room.
func (m *Manager) dueEntry(ctx context.Context, e model.DueEntry, now time.Time, sem chan struct{}, wg *sync.WaitGroup) (claimed, moved, full bool) {
	cur, claim, moved := m.checkEntry(e, now)
	if !claim {
		return false, moved, false
	}

	select {
	case sem <- struct{}{}:
	default:
		return false, false, true
	}

	var extra []model.DueKey
	if !model.SameDueKey(cur, e.DueKey) {
		extra = []model.DueKey{e.DueKey}
	}

	jc, gen, err := m.claim(e.ID, extra)
	if err != nil {
		<-sem
		return false, false, false // another server was faster
	}

	wg.Add(1)

	go func() {
		defer wg.Done()
		defer func() { <-sem }()

		m.execute(ctx, jc, gen)
	}()

	return true, true, false
}

// checkEntry compares a due entry of jobs/claimable with its record: it
// returns the record's position and whether to claim the job, or
// repairs the entry (moved reports a write).
func (m *Manager) checkEntry(e model.DueEntry, now time.Time) (cur model.DueKey, claim, moved bool) {
	j, err := m.st.getJob(e.ID)

	switch {
	case errors.Is(err, ErrNotFound):
		return cur, false, m.dropAfterGrace(e, now)
	case err != nil:
		m.env.Log.Warn("jobs: due job", "job", e.ID, "err", err)
		return cur, false, false
	}

	cur, ok := claimKey(j)

	switch {
	case !ok:
		return cur, false, m.dropEntry(e)
	case e.Gen > j.Lease.Gen:
		// Newer than the record: a writer that writes the entry first is
		// still on its way.
		return cur, false, m.dropAfterGrace(e, now)
	case j.claimable(now):
		return cur, true, false
	case model.SameDueKey(cur, e.DueKey):
		return cur, false, false // the record's own position (clock skew between servers)
	}

	if err := m.q.claimable.Move(e.ID, []model.DueKey{e.DueKey}, &cur); err != nil {
		m.env.Log.Warn("jobs: reschedule", "job", e.ID, "err", err)
		return cur, false, false
	}

	stale(model.JobsClaimableQueue, staleRescheduled)

	return cur, false, true
}

// dropAfterGrace removes an entry that does not match its record once
// it is older than the grace, and skips it before.
func (m *Manager) dropAfterGrace(e model.DueEntry, now time.Time) bool {
	if now.Sub(e.Due) < model.DueQueueGrace {
		stale(model.JobsClaimableQueue, staleSkipped)
		return false
	}

	return m.dropEntry(e)
}

// dropEntry removes a due entry of jobs/claimable.
func (m *Manager) dropEntry(e model.DueEntry) bool {
	if err := m.q.claimable.Move(e.ID, []model.DueKey{e.DueKey}, nil); err != nil {
		m.env.Log.Warn("jobs: drop queue entry", "job", e.ID, "err", err)
		return false
	}

	stale(model.JobsClaimableQueue, staleRemoved)

	return true
}

// watchClaimable watches jobs/claimable and returns the channel of its
// notifies and a function that ends the watch. Only a runner that claims
// from the queue (read mode) watches: in write mode a wake-up would cost
// a listing of the admin namespace on every server per submit. nil
// without a watch: the poll remains.
func (m *Manager) watchClaimable() (<-chan struct{}, <-chan error, func()) {
	if !m.q.reads() {
		return nil, nil, func() {}
	}

	w, err := m.q.claimable.Watch()
	if err != nil {
		m.env.Log.Warn("jobs: watch the claimable queue; polling only", "err", err)
		return nil, nil, func() {}
	}

	return w.Events(), w.Errors(), w.Close
}

// --- expiry ---------------------------------------------------------------

// runExpiry is the expiry worker: it takes the worker lease of
// jobs/expiry when it is free, keeps it while it works, and deletes what
// is due every Config.ExpiryPoll. A lease reported lost (renewal failed,
// or given up at a key rotation) is dropped and taken again.
func (m *Manager) runExpiry(ctx context.Context) {
	var lease model.Lease

	defer func() {
		if lease != nil {
			lease.Release()
		}
	}()

	tick := time.NewTicker(m.cfg.ExpiryPoll)
	defer tick.Stop()

	for {
		if lease != nil && lease.Lost() {
			lease.Release()
			lease = nil
		}

		if lease == nil {
			lease = m.takeExpiryLease()
		}

		if lease != nil {
			m.expireDue(ctx, lease.Lost)
		}

		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

// takeExpiryLease takes the expiry worker's lease (nil when another
// server holds it or the lock failed).
func (m *Manager) takeExpiryLease() model.Lease {
	l, err := m.q.kv.LockQueue(model.JobsExpiryQueue, expiryLockTTL(m.cfg.LeaseTTL))

	switch {
	case err == nil:
		m.env.Log.Info("jobs: expiry worker started", "event", "expiry_lease")
		return l
	case !errors.Is(err, model.ErrLocked):
		m.env.Log.Warn("jobs: expiry lease", "err", err)
	}

	return nil
}

// expiryLockSlack is added to jobs.lease_seconds for the TTL of the
// expiry worker's cls_lock. The lock is renewed every third of its TTL;
// a renewal that waits out the OSD op timeout (rados_osd_op_timeout,
// 60 s) must still land before the lock runs out, so the TTL has to
// exceed period + op timeout: with the slack, TTL/3 < TTL - 60 s holds
// for every allowed lease (10 s and more).
const expiryLockSlack = 2 * time.Minute

// expiryLockTTL is the TTL of the expiry worker's lock.
func expiryLockTTL(lease time.Duration) time.Duration { return lease + expiryLockSlack }

// expireDue deletes the due entries of jobs/expiry and their records,
// page by page, until none is due, the lease is lost or ctx ends.
func (m *Manager) expireDue(ctx context.Context, lost func() bool) {
	const page = 100

	for round := 0; round < 100; round++ {
		now := m.now()

		due, err := m.q.expiry.Due(now, page)
		if err != nil {
			m.env.Log.Warn("jobs: expiry due", "err", err)
			return
		}

		for _, e := range due {
			if lost() || ctx.Err() != nil {
				return
			}

			if err := m.expireEntry(e, now); err != nil {
				m.env.Log.Warn("jobs: expiry", "key", e.ID, "err", err)
			}
		}

		if len(due) < page {
			return
		}
	}
}

// expireEntry handles one due entry of jobs/expiry.
func (m *Manager) expireEntry(e model.DueEntry, now time.Time) error {
	switch {
	case strings.HasPrefix(e.ID, prefixJob):
		return m.expireJob(e, now)
	case strings.HasPrefix(e.ID, prefixIdem):
		return m.expireIdem(e, now)
	case strings.HasPrefix(e.ID, prefixPlan):
		return m.expirePlan(e, now)
	default:
		return m.expiryMove(e, nil)
	}
}

// expiryMove moves an expiry entry to (nil: removes it).
func (m *Manager) expiryMove(e model.DueEntry, to *model.DueKey) error {
	return m.q.expiry.Move(e.ID, []model.DueKey{e.DueKey}, to)
}

// expireJob deletes a job whose retention ended, its idempotency key
// first. A job that has not ended is rescheduled one retention later; an
// ended one whose expiry lies ahead (a placeholder the end did not move)
// is moved there.
func (m *Manager) expireJob(e model.DueEntry, now time.Time) error {
	id := strings.TrimPrefix(e.ID, prefixJob)

	j, err := m.st.getJob(id)

	switch {
	case errors.Is(err, ErrNotFound):
		return m.expiryMove(e, nil)
	case err != nil:
		return err
	case !j.Terminal():
		return m.expiryMove(e, &model.DueKey{Due: now.Add(m.cfg.Retention), Gen: expiryPlaceholder})
	}

	key := expiryKey(j, m.cfg.Retention)
	if now.Before(key.Due) {
		return m.expiryMove(e, &key)
	}

	if j.IdemKey != "" {
		if err := m.deleteIdem(j.IdemKey, j.ID); err != nil {
			return err
		}
	}

	if deleted, err := m.deleteExpiredJob(id, now); err != nil || !deleted {
		return err
	}

	if j.IdemKey != "" {
		_ = m.q.expiry.Move(j.IdemKey, []model.DueKey{key}, nil)
	}

	return m.q.expiry.Move(e.ID, []model.DueKey{e.DueKey, key}, nil)
}

// deleteExpiredJob deletes the job record if it is still ended and
// expired (or already gone); false when it changed meanwhile, so the
// entry stays for the next round.
func (m *Manager) deleteExpiredJob(id string, now time.Time) (bool, error) {
	kept := false

	err := m.st.kv.Update(prefixJob+id, func(cur []byte) ([]byte, error) {
		var c Job

		kept = cur != nil && (json.Unmarshal(cur, &c) != nil || !c.Terminal() || now.Before(expiryKey(&c, m.cfg.Retention).Due))
		if kept {
			return cur, nil
		}

		return nil, nil
	})

	return !kept, err
}

// deleteIdem deletes an idempotency key if it still belongs to jobID
// (absent is fine).
func (m *Manager) deleteIdem(key, jobID string) error {
	return m.st.kv.Update(key, func(cur []byte) ([]byte, error) {
		var idem idempotency
		if cur == nil || json.Unmarshal(cur, &idem) != nil || idem.JobID != jobID {
			return cur, nil
		}

		return nil, nil
	})
}

// expireIdem handles an idempotency key: it expires with its job (the
// job's entry deletes it first). A key whose job was never stored (a
// crash between the two creates) is deleted once its placeholder is due.
func (m *Manager) expireIdem(e model.DueEntry, now time.Time) error {
	var idem idempotency

	err := m.st.get(e.ID, &idem)

	switch {
	case errors.Is(err, ErrNotFound):
		return m.expiryMove(e, nil)
	case err != nil:
		return err
	}

	j, err := m.st.getJob(idem.JobID)

	switch {
	case errors.Is(err, ErrNotFound):
		if err := m.deleteIdem(e.ID, idem.JobID); err != nil {
			return err
		}

		return m.expiryMove(e, nil)
	case err != nil:
		return err
	case !j.Terminal():
		return m.expiryMove(e, &model.DueKey{Due: now.Add(m.cfg.Retention), Gen: expiryPlaceholder})
	}

	key := expiryKey(j, m.cfg.Retention)
	if now.Before(key.Due) {
		return m.expiryMove(e, &key)
	}

	if err := m.deleteIdem(e.ID, j.ID); err != nil {
		return err
	}

	return m.expiryMove(e, nil)
}

// expirePlan deletes a plan whose retention after its expiry ended.
func (m *Manager) expirePlan(e model.DueEntry, now time.Time) error {
	var p Plan

	err := m.st.get(e.ID, &p)

	switch {
	case errors.Is(err, ErrNotFound):
		return m.expiryMove(e, nil)
	case err != nil:
		return err
	}

	key := planExpiryKey(&p, m.cfg.Retention)
	if now.Before(key.Due) {
		return m.expiryMove(e, &key)
	}

	err = m.st.kv.Update(e.ID, func(cur []byte) ([]byte, error) {
		var c Plan
		if cur == nil || json.Unmarshal(cur, &c) != nil || now.Before(planExpiryKey(&c, m.cfg.Retention).Due) {
			return cur, nil
		}

		return nil, nil
	})
	if err != nil {
		return err
	}

	return m.expiryMove(e, nil)
}

// --- backfill ---------------------------------------------------------------

// QueueEntry is a queue entry a record needs (backfill).
type QueueEntry struct {
	Queue string
	ID    string
	Key   model.DueKey
}

// QueueEntries returns the queue entries the admin store record key
// needs (scaling plan B3): a job its claimable position (unless it
// ended) and its expiry, an idempotency key the expiry of its job (a
// short one when the job is missing), a plan its expiry. get reads other
// records (the job of an idempotency key).
func QueueEntries(key string, rec []byte, get func(key string) ([]byte, error), retention time.Duration, now time.Time) ([]QueueEntry, error) {
	switch {
	case strings.HasPrefix(key, prefixJob):
		var j Job
		if err := json.Unmarshal(rec, &j); err != nil {
			return nil, nil //nolint:nilerr // an unreadable record has no position
		}

		out := []QueueEntry{{Queue: model.JobsExpiryQueue, ID: key, Key: expiryKey(&j, retention)}}
		if k, ok := claimKey(&j); ok {
			out = append(out, QueueEntry{Queue: model.JobsClaimableQueue, ID: j.ID, Key: k})
		}

		return out, nil
	case strings.HasPrefix(key, prefixPlan):
		var p Plan
		if err := json.Unmarshal(rec, &p); err != nil {
			return nil, nil //nolint:nilerr // see above
		}

		return []QueueEntry{{Queue: model.JobsExpiryQueue, ID: key, Key: planExpiryKey(&p, retention)}}, nil
	case strings.HasPrefix(key, prefixIdem):
		var idem idempotency
		if err := json.Unmarshal(rec, &idem); err != nil {
			return nil, nil //nolint:nilerr // see above
		}

		k := model.DueKey{Due: now.Add(model.DueQueueGrace), Gen: expiryPlaceholder}

		b, err := get(prefixJob + idem.JobID)

		switch {
		case err == nil:
			var j Job
			if json.Unmarshal(b, &j) == nil {
				k = expiryKey(&j, retention)
			}
		case !errors.Is(err, model.ErrNotFound):
			return nil, err
		}

		return []QueueEntry{{Queue: model.JobsExpiryQueue, ID: key, Key: k}}, nil
	default:
		return nil, nil
	}
}

// listIndexed is Manager.List from the job hint index (read mode): it
// filters on the index values and reads only the records that match.
func (m *Manager) listIndexed(account, state, after string, limit int) ([]Job, string, error) {
	var out []Job

	cursor := ""
	if after != "" {
		cursor = prefixJob + after
	}

	for len(out) <= limit {
		page, err := m.q.kv.ListIndex(prefixJob, cursor, limit)
		if err != nil {
			return nil, "", err
		}

		for _, e := range page {
			cursor = e.Key

			j, err := m.indexedMatch(e, account, state)
			if err != nil {
				return nil, "", err
			}

			if j != nil {
				out = append(out, *j)
			}
		}

		if len(page) == 0 {
			break
		}
	}

	if len(out) > limit {
		return out[:limit], out[limit-1].ID, nil
	}

	return out, "", nil
}

// indexedMatch reads the job of an index entry whose value matches
// account and state ("" = any); nil when it does not match or is gone.
// The index is a hint: the record decides.
func (m *Manager) indexedMatch(e model.KVEntry, account, state string) (*Job, error) {
	matches := func(a, s string) bool { return (account == "" || a == account) && (state == "" || s == state) }

	var v kvindex.JobValue
	if json.Unmarshal(e.Value, &v) != nil || !matches(v.Account, v.State) {
		return nil, nil //nolint:nilerr // an unreadable entry is no match
	}

	j, err := m.st.getJob(strings.TrimPrefix(e.Key, prefixJob))

	switch {
	case errors.Is(err, ErrNotFound):
		return nil, nil // removed meanwhile
	case err != nil:
		return nil, err
	case !matches(j.Account, j.State):
		return nil, nil
	}

	return j, nil
}
