//go:build tentacle

package cephstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"shardpost/internal/model"
	"shardpost/internal/outbound"
	"shardpost/internal/sendqueue"
)

// Submission copies (subcopies.go) and the delayed submission queue
// (internal/sendqueue) on the cluster.

// TestSubmissionCopy: a copy is a blob with its sub: intent; it is read
// back, listed, and dropped with its intent.
func TestSubmissionCopy(t *testing.T) {
	u := testUser(t, testStore(t))
	raw := testMessage("delayed", 2500)

	guid, err := u.PutSubmissionCopy("S01", raw, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}

	checkCopy(t, u, guid, raw)

	if err := u.DropSubmissionCopy(guid); err != nil {
		t.Fatal(err)
	}

	if blobExistsT(u, guid) || len(registryKeys(t, u, regSubmissionPrefix)) != 0 {
		t.Fatal("copy or intent left after the drop")
	}

	if err := u.DropSubmissionCopy(guid); err != nil {
		t.Fatalf("a second drop: %v", err)
	}

	if _, err := u.ReadSubmissionCopy(guid); !errors.Is(err, model.ErrNoBlob) {
		t.Fatalf("read of a dropped copy: %v", err)
	}
}

// checkCopy reads a live copy back, lists it, and finds that it keeps
// the account idle.
func checkCopy(t *testing.T, u *User, guid string, raw []byte) {
	t.Helper()

	got, err := u.ReadSubmissionCopy(guid)
	if err != nil || string(got) != string(raw) {
		t.Fatalf("read back: %d bytes, %v", len(got), err)
	}

	list, err := u.SubmissionCopies()
	if err != nil || len(list) != 1 || list[0].GUID != guid || list[0].ID != "S01" || list[0].Size != int64(len(raw)) {
		t.Fatalf("copies %#v %v", list, err)
	}

	if p, err := u.ScrubPending(); err != nil || p != "" {
		t.Fatalf("a live copy keeps the account from idle: %q %v", p, err)
	}
}

// TestScrubKeepsLiveCopies: neither a listing scrub (the second run
// would delete an orphan) nor one from the intents takes a live copy for
// an orphan; an expired one goes with its intent.
func TestScrubKeepsLiveCopies(t *testing.T) {
	for _, source := range []model.OrphanSource{model.OrphanSourceListing, model.OrphanSourceShadow, model.OrphanSourceIntents} {
		t.Run(string(source), func(t *testing.T) {
			u := testUser(t, intentsStore(t, source))
			mustMailbox(t, u, "INBOX")

			live, err := u.PutSubmissionCopy("S01", testMessage("live", 500), time.Now().Add(time.Hour))
			if err != nil {
				t.Fatal(err)
			}

			expired, err := u.PutSubmissionCopy("S02", testMessage("expired", 500), time.Now().Add(-time.Hour))
			if err != nil {
				t.Fatal(err)
			}

			if p, err := u.ScrubPending(); err != nil || !strings.Contains(p, regSubmissionPrefix) {
				t.Fatalf("an expired copy is pending: %q %v", p, err)
			}

			// Shadow drops an intent once the listing has deleted what it
			// names, a run after the deletion: three runs.
			for range 3 {
				scrubNow(t, u)
			}

			if !blobExistsT(u, live) || len(omapKeys(t, u, oidRegistry, submissionKey(live))) != 1 {
				t.Fatal("the scrub took a live copy")
			}

			if blobExistsT(u, expired) || len(omapKeys(t, u, oidRegistry, submissionKey(expired))) != 0 {
				t.Fatal("the scrub left an expired copy")
			}
		})
	}
}

// TestPurgeAndSweepNameCopies: a purge from the data model deletes the
// copies through their intents, and a repairing sweep keeps a live one.
func TestPurgeAndSweepNameCopies(t *testing.T) {
	s := purgeStore(t, false)
	u := testUser(t, s)
	mustMailbox(t, u, "INBOX")

	guid, err := u.PutSubmissionCopy("S01", testMessage("copy", 2500), time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}

	n, err := u.enumerate(true)
	if err != nil {
		t.Fatal(err)
	}

	if _, ok := n.blobs[guid]; !ok {
		t.Fatal("the data model does not name the copy")
	}

	if _, err := u.PurgeNamespace(func() error { return nil }); err != nil {
		t.Fatal(err)
	}

	if left := append(objects(t, u.idx), objects(t, u.data)...); len(left) > 0 {
		t.Fatalf("objects left: %v", left)
	}
}

// accounts opens accounts of the store for the queue.
type accounts struct{ s *Store }

func (a accounts) User(name string) (model.User, error) { return a.s.User(name) }

// countingRelay counts what it is handed.
type countingRelay struct {
	calls atomic.Int64
	mu    sync.Mutex
	raw   []string
}

func (r *countingRelay) Submit(_ context.Context, _ *outbound.XClient, _ string, _ []string, raw []byte) (*outbound.Report, error) {
	r.calls.Add(1)
	r.mu.Lock()
	r.raw = append(r.raw, string(raw))
	r.mu.Unlock()

	return &outbound.Report{Final: outbound.Reply{Code: 250, Message: "2.0.0 ok"}}, nil
}

// queueEnv is the submission queue on the cluster's admin store.
func queueEnv(t *testing.T, s *Store, relay sendqueue.Submitter) (*sendqueue.Service, *AdminKV) {
	t.Helper()

	kv, err := s.AdminKV()
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(kv.Close)

	q, err := sendqueue.New(sendqueue.Options{KV: kv, Accounts: accounts{s}, Relay: relay, Poll: 200 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}

	return q, kv
}

// runQueue runs a worker until the returned stop is called.
func runQueue(q *sendqueue.Service) (stop func()) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})

	go func() {
		defer close(done)

		q.Run(ctx)
	}()

	return func() { cancel(); <-done }
}

func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()

	for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		if cond() {
			return
		}
	}

	t.Fatalf("timed out waiting for %s", what)
}

var subN atomic.Int64

func subID() string { return fmt.Sprintf("S%016x%08x", time.Now().UnixNano(), subN.Add(1)) }

// schedule schedules a submission of u released after d.
func schedule(t *testing.T, q *sendqueue.Service, u *User, d time.Duration) *sendqueue.Record {
	t.Helper()

	rec, err := q.Schedule(u, sendqueue.Spec{Account: u.name, ID: subID(), Release: time.Now().Add(d), MailFrom: u.name,
		RcptTo: []string{"bob@test.invalid"}, Raw: testMessage("delayed "+u.name, 1500)})
	if err != nil {
		t.Fatal(err)
	}

	return rec
}

// ended reports whether nothing of rec is left in the queue.
func ended(t *testing.T, q *sendqueue.Service, u *User, rec *sendqueue.Record) bool {
	t.Helper()

	_, err := q.Get(rec.Account, rec.ID)

	return errors.Is(err, sendqueue.ErrNotFound) && !blobExistsT(u, rec.GUID) &&
		len(omapKeys(t, u, oidRegistry, submissionKey(rec.GUID))) == 0
}

// TestSubmissionQueueCluster: schedule, the worker sends once and cleans
// up; a cancel wins over a later release.
func TestSubmissionQueueCluster(t *testing.T) {
	s := testStore(t)
	relay := &countingRelay{}
	q, _ := queueEnv(t, s, relay)
	u := testUser(t, s)

	sent := schedule(t, q, u, 300*time.Millisecond)
	canceled := schedule(t, q, u, time.Hour)

	if list, err := q.List(u.name); err != nil || len(list) != 2 {
		t.Fatalf("list %d %v", len(list), err)
	}

	if _, err := q.Cancel(u.name, canceled.ID, "test"); err != nil {
		t.Fatal(err)
	}

	stop := runQueue(q)
	defer stop()

	waitUntil(t, "the send", func() bool { return relay.calls.Load() == 1 && ended(t, q, u, sent) })

	if !ended(t, q, u, canceled) {
		t.Fatal("the canceled submission left something")
	}
}

// TestSubmissionCancelRaceCluster: each submission of a cancel/send race
// is either canceled or sent, never both, never twice.
func TestSubmissionCancelRaceCluster(t *testing.T) {
	s := testStore(t)
	relay := &countingRelay{}
	q, _ := queueEnv(t, s, relay)
	u := testUser(t, s)

	stop := runQueue(q)
	defer stop()

	const n = 20

	recs := make([]*sendqueue.Record, n)
	for i := range recs {
		recs[i] = schedule(t, q, u, 0)
	}

	var won atomic.Int64

	var wg sync.WaitGroup

	for _, rec := range recs {
		wg.Add(1)

		go func() {
			defer wg.Done()

			if _, err := q.Cancel(u.name, rec.ID, "test"); err == nil {
				won.Add(1)
			}
		}()
	}

	wg.Wait()

	waitUntil(t, "the race to settle", func() bool {
		for _, rec := range recs {
			if !ended(t, q, u, rec) {
				return false
			}
		}

		return true
	})

	time.Sleep(500 * time.Millisecond)

	if got := relay.calls.Load() + won.Load(); got != n {
		t.Fatalf("sent %d + canceled %d != %d", relay.calls.Load(), won.Load(), n)
	}
}

// TestSubmissionRestartNotResent: a record left in sending by a server
// that stopped is final with an unknown result after the sending
// timeout, and the relay is never asked.
func TestSubmissionRestartNotResent(t *testing.T) {
	s := testStore(t)
	relay := &countingRelay{}
	q, kv := queueEnv(t, s, relay)
	u := testUser(t, s)

	rec := schedule(t, q, u, time.Hour)

	// What a worker that died while sending left: the record in sending
	// since two hours, its entry at the end of the sending timeout.
	since := time.Now().Add(-2 * time.Hour)

	err := kv.Update(sendqueue.RecordKey(u.name, rec.ID), func(cur []byte) ([]byte, error) {
		var r sendqueue.Record
		if err := json.Unmarshal(cur, &r); err != nil {
			return nil, err
		}

		r.State, r.Gen, r.SendingSince, r.Attempts = sendqueue.StateSending, r.Gen+1, since, 1

		return json.Marshal(&r)
	})
	if err != nil {
		t.Fatal(err)
	}

	queue, err := kv.Queue(model.SubmissionQueue)
	if err != nil {
		t.Fatal(err)
	}

	pos := model.DueKey{Due: since.Add(sendqueue.DefaultSendingTimeout), Gen: rec.Gen + 1}
	if err := queue.Move(sendqueue.EntryID(u.name, rec.ID), []model.DueKey{{Due: rec.Due, Gen: rec.Gen}}, &pos); err != nil {
		t.Fatal(err)
	}

	stop := runQueue(q)
	defer stop()

	waitUntil(t, "the interrupted submission to end", func() bool { return ended(t, q, u, rec) })

	if relay.calls.Load() != 0 {
		t.Fatal("an interrupted submission was sent again")
	}
}

// TestSubmissionLeaseHandover: while one worker holds the partition's
// cls_lock another does not send; when the first stops (and unlocks),
// the second takes over.
func TestSubmissionLeaseHandover(t *testing.T) {
	s := testStore(t)
	first, second := &countingRelay{}, &countingRelay{}
	q1, _ := queueEnv(t, s, first)
	q2, _ := queueEnv(t, s, second)
	u := testUser(t, s)

	stop1 := runQueue(q1)

	time.Sleep(300 * time.Millisecond) // q1 takes the lease

	stop2 := runQueue(q2)
	defer stop2()

	a := schedule(t, q1, u, 0)

	waitUntil(t, "the first worker", func() bool { return ended(t, q1, u, a) })

	if first.calls.Load() != 1 || second.calls.Load() != 0 {
		t.Fatalf("calls %d and %d", first.calls.Load(), second.calls.Load())
	}

	stop1()

	b := schedule(t, q2, u, 0)

	waitUntil(t, "the second worker", func() bool { return ended(t, q2, u, b) })

	if first.calls.Load() != 1 || second.calls.Load() != 1 {
		t.Fatalf("calls %d and %d", first.calls.Load(), second.calls.Load())
	}
}

// TestSubmissionPurge: PurgeAccount and the namespace purge remove the
// records, the entries and the copies of an account.
func TestSubmissionPurge(t *testing.T) {
	s := purgeStore(t, false)
	q, kv := queueEnv(t, s, &countingRelay{})
	u := testUser(t, s)
	mustMailbox(t, u, "INBOX")

	recs := []*sendqueue.Record{schedule(t, q, u, time.Hour), schedule(t, q, u, 2*time.Hour)}

	n, err := q.PurgeAccount(u, u.name)
	if err != nil || n != 2 {
		t.Fatalf("purge %d %v", n, err)
	}

	if _, err := u.PurgeNamespace(func() error { return nil }); err != nil {
		t.Fatal(err)
	}

	queue, err := kv.Queue(model.SubmissionQueue)
	if err != nil {
		t.Fatal(err)
	}

	due, err := queue.DueAfter(time.Now().Add(1000*24*time.Hour), "", 1000)
	if err != nil {
		t.Fatal(err)
	}

	for _, rec := range recs {
		if _, err := q.Get(u.name, rec.ID); !errors.Is(err, sendqueue.ErrNotFound) {
			t.Errorf("record left: %v", err)
		}

		for _, e := range due {
			if e.ID == sendqueue.EntryID(u.name, rec.ID) {
				t.Errorf("entry left: %s", e.Key())
			}
		}
	}

	if left := append(objects(t, u.idx), objects(t, u.data)...); len(left) > 0 {
		t.Fatalf("objects left: %v", left)
	}
}
