package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	dto "github.com/prometheus/client_model/go"

	"shardpost/internal/memstore"
	"shardpost/internal/metrics"
	"shardpost/internal/model"
)

// clock is a settable time source.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func newClock() *clock { return &clock{t: time.Date(2026, 10, 1, 12, 0, 0, 123456789, time.UTC)} }

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.t
}

func (c *clock) add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.t = c.t.Add(d)
}

// entries returns the entries of a queue, sorted by key.
func entries(t *testing.T, kv *memstore.AdminKV, queue string) []model.DueEntry {
	t.Helper()

	all, err := kv.QueueOf(queue).All()
	if err != nil {
		t.Fatal(err)
	}

	slices.SortFunc(all, func(a, b model.DueEntry) int { return strings.Compare(a.Key(), b.Key()) })

	return all
}

// wantEntries checks the entries of id in a queue.
func wantEntries(t *testing.T, kv *memstore.AdminKV, queue, id string, want ...model.DueKey) {
	t.Helper()

	var got []model.DueEntry

	for _, e := range entries(t, kv, queue) {
		if e.ID == id {
			got = append(got, e)
		}
	}

	if len(got) != len(want) {
		t.Fatalf("%s %s: entries %+v, want %+v", queue, id, got, want)
	}

	for i := range got {
		if !model.SameDueKey(got[i].DueKey, want[i]) {
			t.Fatalf("%s %s: entry %d at %v gen %d, want %v gen %d", queue, id, i, got[i].Due, got[i].Gen, want[i].Due, want[i].Gen)
		}
	}
}

func clockManager(kv model.AdminKV, c *clock, instance string, kinds ...Kind) *Manager {
	m := newTestManager(kv, memstore.New(), instance, kinds...)
	m.now = c.now

	return m
}

// TestQueueSubmit: submit writes the claimable entry at the creation
// time (the same instant as the record, read back from the store) and
// the expiry placeholders of the job and its idempotency key; a plan its
// expiry.
func TestQueueSubmit(t *testing.T) {
	kv, c := memstore.NewAdminKV(), newClock()
	k := &stepKind{name: "count", steps: 1, planned: false}
	m := clockManager(kv, c, "pod-a", k)
	ret := m.cfg.Retention

	j, _, err := m.Submit(k, "alice", nil, "", "tester", "key-1")
	if err != nil {
		t.Fatal(err)
	}

	stored, err := m.Get(j.ID)
	if err != nil {
		t.Fatal(err)
	}

	es := entries(t, kv, model.JobsClaimableQueue)
	if len(es) != 1 || es[0].ID != j.ID || es[0].Gen != 0 || es[0].Due.UnixNano() != stored.Created.UnixNano() {
		t.Fatalf("claimable entries %+v, created %v", es, stored.Created)
	}

	if stored.IdemKey == "" || !strings.HasPrefix(stored.IdemKey, prefixIdem) {
		t.Fatalf("idempotency key not in the record: %+v", stored)
	}

	placeholder := model.DueKey{Due: c.now().Add(ret)}
	wantEntries(t, kv, model.JobsExpiryQueue, prefixJob+j.ID, placeholder)
	wantEntries(t, kv, model.JobsExpiryQueue, stored.IdemKey, placeholder)

	planned := &stepKind{name: "wipe", planned: true, steps: 1}
	mp := clockManager(kv, c, "pod-a", planned)

	p, err := mp.CreatePlan(context.Background(), planned, "alice", nil, "tester", nil)
	if err != nil {
		t.Fatal(err)
	}

	wantEntries(t, kv, model.JobsExpiryQueue, prefixPlan+p.ID, model.DueKey{Due: p.Expires.Add(ret)})
}

// TestQueueEveryWriter: claim, heartbeat, postpone, release and the end
// each move the claimable entry to the record's new position; a cancel
// request leaves it; the end moves the expiry entries to Finished +
// retention.
func TestQueueEveryWriter(t *testing.T) {
	kv, c := memstore.NewAdminKV(), newClock()
	k := &stepKind{name: "count", steps: 1}
	m := clockManager(kv, c, "pod-a", k)
	m.cfg.LeaseTTL = time.Minute
	ret := m.cfg.Retention
	log := m.env.Log

	j, _, _ := m.Submit(k, "alice", nil, "", "tester", "key-1")
	created := c.now()

	w, err := kv.QueueOf(model.JobsClaimableQueue).Watch()
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()

	run := leaseMoves(t, kv, m, c, j.ID)
	claimed, gen := run.Job, run.gen

	// Checkpoints, progress and a cancel request (claimable() does not
	// change) do not move the entry.
	unmoved(t, kv, "a checkpoint", func() error { return run.Checkpoint(1) })
	unmoved(t, kv, "a cancel request", func() error { _, err := m.Cancel(j.ID); return err })

	// Drain the notify of the submit, then release: due now, notified.
	select {
	case <-w.Events():
	default:
	}

	c.add(time.Second)
	m.release(run, log)
	wantEntries(t, kv, model.JobsClaimableQueue, j.ID, model.DueKey{Due: c.now(), Gen: 1})
	waitEvent(t, w, "release")

	// The end removes the entry (after the record) and moves the expiry.
	c.add(time.Second)

	if !m.finish(run, claimed, gen, nil, context.Canceled, log) {
		t.Fatal("finish not stored")
	}

	done, _ := m.Get(j.ID)
	if done.State != StateCancelled || created.Equal(c.now()) {
		t.Fatalf("state %s", done.State)
	}

	wantEntries(t, kv, model.JobsClaimableQueue, j.ID)
	wantEntries(t, kv, model.JobsExpiryQueue, prefixJob+j.ID, model.DueKey{Due: c.now().Add(ret), Gen: 1})
	wantEntries(t, kv, model.JobsExpiryQueue, done.IdemKey, model.DueKey{Due: c.now().Add(ret), Gen: 1})
}

// leaseMoves claims the job, renews and postpones it, checking after
// each that the claimable entry is at the end of the lease.
func leaseMoves(t *testing.T, kv *memstore.AdminKV, m *Manager, c *clock, id string) *Run {
	t.Helper()

	ttl := m.cfg.LeaseTTL

	c.add(time.Second)

	claimed, gen, err := m.claim(id, nil)
	if err != nil {
		t.Fatal(err)
	}

	wantEntries(t, kv, model.JobsClaimableQueue, id, model.DueKey{Due: c.now().Add(ttl), Gen: 1})

	run := &Run{Job: claimed, m: m, gen: gen}

	c.add(time.Second)

	if !m.renew(run, false, m.env.Log) {
		t.Fatal("renew failed")
	}

	wantEntries(t, kv, model.JobsClaimableQueue, id, model.DueKey{Due: c.now().Add(ttl), Gen: 1})

	c.add(time.Second)
	m.postpone(run, m.env.Log)
	wantEntries(t, kv, model.JobsClaimableQueue, id, model.DueKey{Due: c.now().Add(ttl), Gen: 1})

	return run
}

// unmoved checks that fn does not move the claimable entries.
func unmoved(t *testing.T, kv *memstore.AdminKV, what string, fn func() error) {
	t.Helper()

	before := entries(t, kv, model.JobsClaimableQueue)

	if err := fn(); err != nil {
		t.Fatal(err)
	}

	if after := entries(t, kv, model.JobsClaimableQueue); !slices.EqualFunc(before, after, func(a, b model.DueEntry) bool { return a.Key() == b.Key() }) {
		t.Fatalf("%s moved the entry: %v -> %v", what, before, after)
	}
}

// waitEvent waits for a notify of the watch.
func waitEvent(t *testing.T, w model.Watch, what string) {
	t.Helper()

	select {
	case <-w.Events():
	case <-time.After(time.Second):
		t.Fatalf("%s did not notify", what)
	}
}

// TestQueueCancelQueued: cancelling a queued job ends it at once: the
// claimable entry is removed and the expiry moved.
func TestQueueCancelQueued(t *testing.T) {
	kv, c := memstore.NewAdminKV(), newClock()
	k := &stepKind{name: "count", steps: 1}
	m := clockManager(kv, c, "pod-a", k)

	j, _, _ := m.Submit(k, "alice", nil, "", "tester", "")

	c.add(time.Minute)

	if _, err := m.Cancel(j.ID); err != nil {
		t.Fatal(err)
	}

	wantEntries(t, kv, model.JobsClaimableQueue, j.ID)
	wantEntries(t, kv, model.JobsExpiryQueue, prefixJob+j.ID, model.DueKey{Due: c.now().Add(m.cfg.Retention), Gen: 1})
}

// TestQueueOff: mode off writes no queue.
func TestQueueOff(t *testing.T) {
	kv := memstore.NewAdminKVMode(model.KVIndexOff)
	k := &stepKind{name: "count", steps: 1}
	m := newTestManager(kv, memstore.New(), "pod-a", k)

	if _, _, err := m.Submit(k, "alice", nil, "", "tester", "k"); err != nil {
		t.Fatal(err)
	}

	if objs := kv.Objects(); len(objs) != 0 {
		t.Fatalf("mode off wrote %v", objs)
	}
}

// TestPollDueRepairsStaleEntries: a reader moves an entry whose record
// is not claimable to the record's position, removes the entry of an
// ended job, leaves an entry without record alone within the grace and
// removes it after, and a claim removes the stale entry it came from.
func TestPollDueRepairsStaleEntries(t *testing.T) {
	kv, c := memstore.NewAdminKVMode(model.KVIndexRead), newClock()
	k := &stepKind{name: "count", steps: 1, gate: make(chan struct{})}
	m := clockManager(kv, c, "pod-a", k)
	q := kv.QueueOf(model.JobsClaimableQueue)
	ttl := m.cfg.LeaseTTL

	sem := make(chan struct{}, 4)

	var wg sync.WaitGroup

	ctx, cancel := context.WithCancel(context.Background())
	defer func() { cancel(); close(k.gate); wg.Wait() }()

	running, ended, ghostDue := staleEntries(t, m, k, c)

	m.pollDue(ctx, sem, &wg)

	cur, _ := m.Get(running.ID)
	wantEntries(t, kv, model.JobsClaimableQueue, running.ID, model.DueKey{Due: cur.Lease.Until, Gen: 1})
	wantEntries(t, kv, model.JobsClaimableQueue, ended.ID)
	wantEntries(t, kv, model.JobsClaimableQueue, "ghost", model.DueKey{Due: ghostDue})

	c.add(model.DueQueueGrace + ttl)

	// The lease ran out: the claim takes the job over, and a stale entry
	// at an older generation is removed with the record's own.
	if err := q.Move(running.ID, nil, &model.DueKey{Due: running.Created}); err != nil {
		t.Fatal(err)
	}

	m.pollDue(ctx, sem, &wg)

	wantEntries(t, kv, model.JobsClaimableQueue, "ghost")

	cur, _ = m.Get(running.ID)
	if cur.Lease.Gen != 2 {
		t.Fatalf("not taken over: %+v", cur.Lease)
	}

	wantEntries(t, kv, model.JobsClaimableQueue, running.ID, model.DueKey{Due: cur.Lease.Until, Gen: 2})
}

// staleEntries sets up a running job with a stale entry at its creation
// time (a crash between the claim's record write and its queue move), an
// entry without record (a submit between entry and record) and an ended
// job whose entry the end did not remove.
func staleEntries(t *testing.T, m *Manager, k Kind, c *clock) (running, ended *Job, ghostDue time.Time) {
	t.Helper()

	q := m.q.claimable
	running, _, _ = m.Submit(k, "alice", nil, "", "tester", "")

	c.add(time.Second)

	if _, _, err := m.claim(running.ID, nil); err != nil {
		t.Fatal(err)
	}

	ghostDue = c.now()
	ended, _, _ = m.Submit(k, "alice", nil, "", "tester", "")

	if _, err := m.Cancel(ended.ID); err != nil {
		t.Fatal(err)
	}

	for id, due := range map[string]time.Time{running.ID: running.Created, "ghost": ghostDue, ended.ID: ended.Created} {
		if err := q.Move(id, nil, &model.DueKey{Due: due}); err != nil {
			t.Fatal(err)
		}
	}

	return running, ended, ghostDue
}

// orderKV records the deletes of records.
type orderKV struct {
	*memstore.AdminKV
	mu      sync.Mutex
	deletes []string
}

func (o *orderKV) Update(key string, fn func([]byte) ([]byte, error)) error {
	return o.AdminKV.Update(key, func(cur []byte) ([]byte, error) {
		next, err := fn(cur)
		if err == nil && next == nil && cur != nil {
			o.mu.Lock()
			o.deletes = append(o.deletes, key)
			o.mu.Unlock()
		}

		return next, err
	})
}

// TestExpiryMatchesCleanup: the expiry worker deletes the same jobs and
// plans at the same times as cleanup() (Finished + retention, Expires +
// retention), the idempotency key with its job and before it.
func TestExpiryMatchesCleanup(t *testing.T) {
	c := newClock()
	k := &stepKind{name: "count", steps: 1}
	planned := &stepKind{name: "wipe", planned: true, steps: 1}

	ekv := &orderKV{AdminKV: memstore.NewAdminKV()}
	ckv := memstore.NewAdminKVMode(model.KVIndexOff)

	exp := clockManager(ekv, c, "pod-a", k, planned)
	cln := clockManager(ckv, c, "pod-a", k, planned)

	seedExpiry(t, exp, k, planned)
	seedExpiry(t, cln, k, planned)

	for _, step := range []time.Duration{exp.cfg.Retention - time.Second, 2 * time.Second, exp.cfg.PlanTTL} {
		c.add(step)

		exp.expireDue(context.Background(), func() bool { return false })
		cln.cleanup()

		for _, prefix := range []string{prefixJob, prefixPlan} {
			if e, cl := countRecords(t, ekv, prefix), countRecords(t, ckv, prefix); e != cl {
				t.Fatalf("after +%v: %s expiry keeps %d, cleanup %d", step, prefix, e, cl)
			}
		}
	}

	if n := countRecords(t, ekv, prefixJob) + countRecords(t, ekv, prefixPlan) + countRecords(t, ekv, prefixIdem); n != 0 {
		t.Fatalf("%d records left", n)
	}

	if len(ekv.deletes) < 2 || !strings.HasPrefix(ekv.deletes[0], prefixIdem) || !strings.HasPrefix(ekv.deletes[1], prefixJob) {
		t.Fatalf("delete order %v, want the idempotency key first", ekv.deletes)
	}

	if left := entries(t, ekv.AdminKV, model.JobsExpiryQueue); len(left) != 0 {
		t.Fatalf("expiry entries left: %+v", left)
	}
}

// seedExpiry submits and ends a job with an idempotency key and creates a
// plan.
func seedExpiry(t *testing.T, m *Manager, k, planned Kind) {
	t.Helper()

	j, _, err := m.Submit(k, "alice", nil, "", "tester", "key")
	if err != nil {
		t.Fatal(err)
	}

	if _, err := m.Cancel(j.ID); err != nil {
		t.Fatal(err)
	}

	if _, err := m.CreatePlan(context.Background(), planned, "alice", nil, "tester", nil); err != nil {
		t.Fatal(err)
	}
}

func countRecords(t *testing.T, kv model.AdminKV, prefix string) int {
	t.Helper()

	recs, err := kv.List(prefix, "", 100)
	if err != nil {
		t.Fatal(err)
	}

	return len(recs)
}

// TestExpiryPlaceholders: a job that has not ended when its placeholder
// is due is rescheduled; an idempotency key whose job was never stored
// (a crash between the creates) is deleted when its placeholder is due,
// not before.
func TestExpiryPlaceholders(t *testing.T) {
	kv, c := memstore.NewAdminKV(), newClock()
	k := &stepKind{name: "count", steps: 1}
	m := clockManager(kv, c, "pod-a", k)
	ret := m.cfg.Retention

	j, _, _ := m.Submit(k, "alice", nil, "", "tester", "")

	orphan := idemKeyOf("tester", "lost")
	placeholder := model.DueKey{Due: c.now().Add(ret)}

	if err := kv.QueueOf(model.JobsExpiryQueue).Move(orphan, nil, &placeholder); err != nil {
		t.Fatal(err)
	}

	if err := m.st.create(orphan, idempotency{JobID: "never-stored", RequestHash: "h"}); err != nil {
		t.Fatal(err)
	}

	c.add(ret - time.Second)
	m.expireDue(context.Background(), func() bool { return false })

	if _, err := kv.Get(orphan); err != nil {
		t.Fatalf("orphan key deleted before its placeholder: %v", err)
	}

	c.add(2 * time.Second)
	m.expireDue(context.Background(), func() bool { return false })

	if _, err := kv.Get(orphan); !errors.Is(err, model.ErrNotFound) {
		t.Fatalf("orphan key kept: %v", err)
	}

	if _, err := m.Get(j.ID); err != nil {
		t.Fatalf("a queued job expired: %v", err)
	}

	wantEntries(t, kv, model.JobsExpiryQueue, prefixJob+j.ID, model.DueKey{Due: c.now().Add(ret)})
	wantEntries(t, kv, model.JobsExpiryQueue, orphan)
}

// TestExpiryRetryReturnsJobWhileItExists: the idempotency key returns the
// job until both expire together.
func TestExpiryRetryReturnsJobWhileItExists(t *testing.T) {
	kv, c := memstore.NewAdminKV(), newClock()
	k := &stepKind{name: "count", steps: 1}
	m := clockManager(kv, c, "pod-a", k)

	j, _, _ := m.Submit(k, "alice", nil, "", "tester", "key")
	if _, err := m.Cancel(j.ID); err != nil {
		t.Fatal(err)
	}

	c.add(m.cfg.Retention - time.Second)
	m.expireDue(context.Background(), func() bool { return false })

	again, created, err := m.Submit(k, "alice", nil, "", "tester", "key")
	if err != nil || created || again.ID != j.ID {
		t.Fatalf("retry before expiry: %v %v", err, again)
	}

	c.add(2 * time.Second)
	m.expireDue(context.Background(), func() bool { return false })

	fresh, created, err := m.Submit(k, "alice", nil, "", "tester", "key")
	if err != nil || !created || fresh.ID == j.ID {
		t.Fatalf("retry after expiry: %v %v %v", err, created, fresh)
	}
}

// failKV fails the creation of records with a prefix.
type failKV struct {
	*memstore.AdminKV
	prefix string
}

func (f *failKV) Update(key string, fn func([]byte) ([]byte, error)) error {
	if strings.HasPrefix(key, f.prefix) {
		return errors.New("injected")
	}

	return f.AdminKV.Update(key, fn)
}

// TestQueueEntriesWithoutRecord: a retry of an idempotency key writes no
// placeholder, and a plan or job whose record could not be stored leaves
// no queue entry.
func TestQueueEntriesWithoutRecord(t *testing.T) {
	kv := memstore.NewAdminKV()
	k := &stepKind{name: "count", steps: 1}
	m := newTestManager(kv, memstore.New(), "pod-a", k)

	if _, _, err := m.Submit(k, "alice", nil, "", "tester", "key"); err != nil {
		t.Fatal(err)
	}

	before := entries(t, kv, model.JobsExpiryQueue)

	if _, created, err := m.Submit(k, "alice", nil, "", "tester", "key"); err != nil || created {
		t.Fatalf("retry: %v %v", created, err)
	}

	if after := entries(t, kv, model.JobsExpiryQueue); len(after) != len(before) {
		t.Fatalf("a retry wrote expiry entries: %d -> %d", len(before), len(after))
	}

	planned := &stepKind{name: "wipe", planned: true, steps: 1}

	for _, prefix := range []string{prefixPlan, prefixJob} {
		empty := memstore.NewAdminKV()
		fm := newTestManager(&failKV{AdminKV: empty, prefix: prefix}, memstore.New(), "pod-a", k, planned)

		if prefix == prefixPlan {
			_, err := fm.CreatePlan(context.Background(), planned, "alice", nil, "tester", nil)
			if err == nil {
				t.Fatal("plan stored")
			}
		} else if _, _, err := fm.Submit(k, "alice", nil, "", "tester", ""); err == nil {
			t.Fatal("job stored")
		}

		for _, q := range []string{model.JobsClaimableQueue, model.JobsExpiryQueue} {
			if left := entries(t, empty, q); len(left) != 0 {
				t.Fatalf("%s: entries without record in %s: %+v", prefix, q, left)
			}
		}
	}
}

// TestExpiryStopsWithContext: a cancelled context stops the expiry
// before its next deletion.
func TestExpiryStopsWithContext(t *testing.T) {
	kv, c := memstore.NewAdminKV(), newClock()
	k := &stepKind{name: "count", steps: 1}
	m := clockManager(kv, c, "pod-a", k)

	j, _, _ := m.Submit(k, "alice", nil, "", "tester", "")
	if _, err := m.Cancel(j.ID); err != nil {
		t.Fatal(err)
	}

	c.add(2 * m.cfg.Retention)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	m.expireDue(ctx, func() bool { return false })

	if _, err := m.Get(j.ID); err != nil {
		t.Fatalf("deleted after the context ended: %v", err)
	}
}

// TestRunWakesOnSubmit: in read mode, with a poll far away, a runner
// claims a job submitted after its start at once, woken by the queue
// notify. In write mode it does not watch (a wake-up would cost a
// listing per submit on every server) and finds the job by its poll.
func TestRunWakesOnSubmit(t *testing.T) {
	for mode, poll := range map[model.KVIndexMode]time.Duration{model.KVIndexRead: time.Hour, model.KVIndexWrite: 20 * time.Millisecond} {
		t.Run(string(mode), func(t *testing.T) {
			kv, store := memstore.NewAdminKVMode(mode), memstore.New()
			k := &stepKind{name: "count", steps: 1}
			m := newTestManager(kv, store, "pod-a", k)
			m.cfg.Poll = poll

			runManager(t, m)
			time.Sleep(50 * time.Millisecond) // the first poll is over

			woken := wakeups(t)

			j, _, err := newTestManager(kv, store, "api", k).Submit(k, "alice", nil, "", "tester", "")
			if err != nil {
				t.Fatal(err)
			}

			waitState(t, m, j.ID, StateSucceeded)

			if notified := wakeups(t) > woken; notified != (mode == model.KVIndexRead) {
				t.Fatalf("woken by a notify: %v", notified)
			}
		})
	}
}

// wakeups reads imapceph_job_wakeups_total{cause="notify"}.
func wakeups(t *testing.T) float64 {
	t.Helper()

	var m dto.Metric
	if err := metrics.JobWakeups.WithLabelValues(wakeNotify).Write(&m); err != nil {
		t.Fatal(err)
	}

	return m.GetCounter().GetValue()
}

// TestReadModeEndToEnd: in read mode the runner claims from the queue,
// cleanup and List read the hint index, and nothing lists the records.
func TestReadModeEndToEnd(t *testing.T) {
	kv, store := memstore.NewAdminKVMode(model.KVIndexRead), memstore.New()
	k := &stepKind{name: "count", steps: 2}
	m := newTestManager(kv, store, "pod-a", k)

	var ids []string

	for _, acct := range []string{"alice", "bob", "alice"} {
		j, _, err := m.Submit(k, acct, nil, "", "tester", "")
		if err != nil {
			t.Fatal(err)
		}

		ids = append(ids, j.ID)
	}

	runManager(t, m)

	for _, id := range ids {
		waitState(t, m, id, StateSucceeded)
	}

	alice := []string{ids[0], ids[2]}
	slices.Sort(alice)

	// The hint index follows the record (the entry is written after it):
	// wait until both of alice's entries show the end.
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		if list, _, err := m.List("alice", StateSucceeded, "", 10); err == nil && len(list) == len(alice) {
			break
		}
	}

	checkList(t, m, "alice", StateSucceeded, "", 1, alice[:1], alice[0])
	checkList(t, m, "alice", "", alice[0], 10, alice[1:], "")
	checkList(t, m, "bob", StateQueued, "", 10, nil, "")

	if n := kv.Listings(); n != 0 {
		t.Fatalf("read mode listed the records %d times", n)
	}

	if len(entries(t, kv, model.JobsClaimableQueue)) != 0 {
		t.Fatalf("claimable entries of ended jobs: %+v", entries(t, kv, model.JobsClaimableQueue))
	}
}

// TestCleanupOnlyWithoutReadMode: cleanup() (a listing) runs on the poll
// in write mode and not at all in read mode, where the expiry worker
// alone deletes what its queue holds. The ended job here has no expiry
// entry (an older server finished it before the backfill), so only
// cleanup() would delete it.
func TestCleanupOnlyWithoutReadMode(t *testing.T) {
	for _, mode := range []model.KVIndexMode{model.KVIndexRead, model.KVIndexWrite} {
		t.Run(string(mode), func(t *testing.T) {
			kv := memstore.NewAdminKVMode(mode)
			old := time.Now().Add(-30 * 24 * time.Hour)

			b, _ := json.Marshal(Job{ID: "old", Kind: "count", Account: "alice", State: StateSucceeded, Created: old, Finished: old})

			if err := kv.Update(prefixJob+"old", func([]byte) ([]byte, error) { return b, nil }); err != nil {
				t.Fatal(err)
			}

			m := newTestManager(kv, memstore.New(), "pod-a", &stepKind{name: "count", steps: 1})
			stop := runManager(t, m)

			time.Sleep(100 * time.Millisecond) // several polls
			stop()

			_, err := kv.Get(prefixJob + "old")
			if deleted := errors.Is(err, model.ErrNotFound); deleted != (mode == model.KVIndexWrite) {
				t.Fatalf("deleted by cleanup: %v", deleted)
			}

			if listed := kv.Listings() > 0; listed != (mode == model.KVIndexWrite) {
				t.Fatalf("listed: %v", listed)
			}
		})
	}
}

// checkList checks a page of Manager.List.

func checkList(t *testing.T, m *Manager, account, state, after string, limit int, want []string, wantNext string) {
	t.Helper()

	list, next, err := m.List(account, state, after, limit)
	if err != nil {
		t.Fatal(err)
	}

	got := make([]string, 0, len(list))
	for _, j := range list {
		got = append(got, j.ID)
	}

	if !slices.Equal(got, want) || next != wantNext {
		t.Fatalf("List(%s, %s, %s, %d) = %v next %q, want %v next %q", account, state, after, limit, got, next, want, wantNext)
	}
}

// TestExpiryWorkerLease: one expiry worker holds the lease; when it is
// lost (a key rotation) the worker takes it again.
func TestExpiryWorkerLease(t *testing.T) {
	kv := memstore.NewAdminKV()
	k := &stepKind{name: "count", steps: 1}
	m := newTestManager(kv, memstore.New(), "pod-a", k)
	m.cfg.ExpiryPoll = 5 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})

	go func() { m.runExpiry(ctx); close(done) }()

	held := func() bool {
		l, err := kv.LockQueue(model.JobsExpiryQueue, time.Minute)
		if err == nil {
			l.Release()
			return false
		}

		return errors.Is(err, model.ErrLocked)
	}

	waitFor(t, held)

	kv.RevokeLocks()
	waitFor(t, held)

	cancel()
	<-done

	if held() {
		t.Fatal("the lease outlived the worker")
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()

	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		if cond() {
			return
		}
	}

	t.Fatal("condition not reached")
}

// TestQueueEntriesForBackfill: what the backfill writes for a record
// equals what the writers wrote.
func TestQueueEntriesForBackfill(t *testing.T) {
	kv, c := memstore.NewAdminKV(), newClock()
	k := &stepKind{name: "count", steps: 1}
	m := clockManager(kv, c, "pod-a", k)
	ret := m.cfg.Retention

	if _, _, err := m.Submit(k, "alice", nil, "", "tester", "a"); err != nil {
		t.Fatal(err)
	}

	ended, _, _ := m.Submit(k, "alice", nil, "", "tester", "b")

	c.add(time.Hour)

	if _, err := m.Cancel(ended.ID); err != nil {
		t.Fatal(err)
	}

	var want, got []string

	for _, q := range []string{model.JobsClaimableQueue, model.JobsExpiryQueue} {
		for _, e := range entries(t, kv, q) {
			want = append(want, q+" "+e.Key())
		}
	}

	keys, _ := kv.Keys()
	for _, key := range keys {
		rec, _ := kv.Get(key)

		es, err := QueueEntries(key, rec, kv.Get, ret, c.now())
		if err != nil {
			t.Fatal(err)
		}

		for _, e := range es {
			got = append(got, e.Queue+" "+model.FormatDueKey(e.ID, e.Key))
		}
	}

	slices.Sort(want)
	slices.Sort(got)

	if !slices.Equal(got, want) {
		t.Fatalf("backfill entries\n%v\nwriters wrote\n%v", got, want)
	}

	if es, err := QueueEntries(prefixJob+"x", json.RawMessage("{"), kv.Get, ret, c.now()); err != nil || len(es) != 0 {
		t.Fatalf("unreadable record: %v %v", es, err)
	}
}
