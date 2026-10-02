package sendqueue

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"shardpost/internal/memstore"
	"shardpost/internal/model"
	"shardpost/internal/outbound"
)

// fakeRelay counts what it is handed.
type fakeRelay struct {
	mu     sync.Mutex
	calls  int
	raw    []string
	as     []*outbound.XClient
	err    error
	report *outbound.Report
	// hold, when set, blocks Submit until it is closed.
	hold chan struct{}
	// onCall, when set, runs at every call.
	onCall func()
}

func (f *fakeRelay) Submit(_ context.Context, as *outbound.XClient, _ string, _ []string, raw []byte) (*outbound.Report, error) {
	if f.hold != nil {
		<-f.hold
	}

	if f.onCall != nil {
		f.onCall()
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	f.calls++
	f.raw = append(f.raw, string(raw))
	f.as = append(f.as, as)

	if f.err != nil {
		return nil, f.err
	}

	if f.report != nil {
		return f.report, nil
	}

	return &outbound.Report{Final: outbound.Reply{Code: 250, Message: "2.0.0 queued as X"}}, nil
}

func (f *fakeRelay) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.calls
}

// fakeBook records the settled submissions.
type fakeBook struct {
	mu      sync.Mutex
	settled map[string]*Record
}

func (b *fakeBook) Settle(rec *Record) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.settled == nil {
		b.settled = map[string]*Record{}
	}

	b.settled[rec.ID] = rec

	return nil
}

func (b *fakeBook) get(id string) *Record {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.settled[id]
}

// clock is a settable clock.
type clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.now
}

func (c *clock) add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.now = c.now.Add(d)
}

// env is a queue over the in-memory stores.
type env struct {
	store *memstore.Store
	kv    *memstore.AdminKV
	relay *fakeRelay
	book  *fakeBook
	clock *clock
	svc   *Service
}

func newEnv(t *testing.T, mod ...func(*Options)) *env {
	t.Helper()

	e := &env{store: memstore.New(), kv: memstore.NewAdminKV(), relay: &fakeRelay{}, book: &fakeBook{},
		clock: &clock{now: time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)}}

	o := Options{KV: e.kv, Accounts: e.store, Relay: e.relay, Book: e.book, Now: e.clock.Now, RetryBase: time.Minute,
		SendingTimeout: time.Hour, Grace: 10 * time.Minute}
	for _, m := range mod {
		m(&o)
	}

	svc, err := New(o)
	if err != nil {
		t.Fatal(err)
	}

	e.svc = svc

	return e
}

func (e *env) user(t *testing.T, name string) model.User {
	t.Helper()

	u, err := e.store.User(name)
	if err != nil {
		t.Fatal(err)
	}

	return u
}

var idN atomic.Int64

func newID() string { return fmt.Sprintf("S%024x", idN.Add(1)) }

// schedule schedules a submission of account due after d.
func (e *env) schedule(t *testing.T, account string, d time.Duration) *Record {
	t.Helper()

	rec, err := e.svc.Schedule(e.user(t, account), Spec{Account: account, ID: newID(), Release: e.clock.Now().Add(d),
		MailFrom: account, RcptTo: []string{"bob@example.net"}, XClient: true, Login: account, ClientAddr: "192.0.2.1",
		Raw: []byte("Subject: hi\r\n\r\nbody\r\n")})
	if err != nil {
		t.Fatal(err)
	}

	return rec
}

// worker is a worker holding every partition.
func (e *env) worker(t *testing.T) *worker {
	t.Helper()

	w := &worker{s: e.svc, held: map[int]model.Lease{}}
	w.leases(true)

	if len(w.held) != e.svc.o.Partitions {
		t.Fatalf("held %d of %d partitions", len(w.held), e.svc.o.Partitions)
	}

	t.Cleanup(w.releaseAll)

	return w
}

func (e *env) entries(t *testing.T) []model.DueEntry {
	t.Helper()

	out, err := e.svc.q.DueAfter(e.clock.Now().Add(1000*24*time.Hour), "", 1000)
	if err != nil {
		t.Fatal(err)
	}

	return out
}

func (e *env) copies(t *testing.T, account string) []model.SubmissionCopy {
	t.Helper()

	list, err := e.user(t, account).(model.SubmissionCopies).SubmissionCopies()
	if err != nil {
		t.Fatal(err)
	}

	return list
}

// gone checks that nothing of a submission is left but the JMAP view.
func (e *env) gone(t *testing.T, rec *Record) {
	t.Helper()

	if _, err := e.svc.Get(rec.Account, rec.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("record left: %v", err)
	}

	if n := len(e.entries(t)); n != 0 {
		t.Errorf("%d queue entries left", n)
	}

	if n := len(e.copies(t, rec.Account)); n != 0 {
		t.Errorf("%d copies left", n)
	}
}

// TestScheduleAndSend: a submission waits until its release time, is
// sent once with the copy and the XCLIENT data, and leaves nothing
// behind but the settled JMAP view.
func TestScheduleAndSend(t *testing.T) {
	e := newEnv(t)
	rec := e.schedule(t, "alice", 10*time.Second)

	if len(e.entries(t)) != 1 || len(e.copies(t, "alice")) != 1 {
		t.Fatal("schedule did not write entry and copy")
	}

	w := e.worker(t)

	if wait := w.round(context.Background()); wait != 10*time.Second || e.relay.count() != 0 {
		t.Fatalf("before the release: wait %v, calls %d", wait, e.relay.count())
	}

	e.clock.add(10 * time.Second)
	w.round(context.Background())
	checkSent(t, e)

	s := e.book.get(rec.ID)
	if s == nil || s.State != StateFinal || s.Unknown || s.Delivery["bob@example.net"].Delivered != "queued" {
		t.Fatalf("settled %#v", s)
	}

	e.gone(t, rec)

	e.clock.add(time.Hour)
	w.round(context.Background())

	if e.relay.count() != 1 {
		t.Error("sent twice")
	}
}

// checkSent checks the one relay call of TestScheduleAndSend.
func checkSent(t *testing.T, e *env) {
	t.Helper()

	if e.relay.count() != 1 || e.relay.raw[0] != "Subject: hi\r\n\r\nbody\r\n" {
		t.Fatalf("relay calls %d %q", e.relay.count(), e.relay.raw)
	}

	if as := e.relay.as[0]; as == nil || as.Login != "alice" || as.Addr != "192.0.2.1" {
		t.Errorf("xclient %#v", as)
	}
}

// TestCancel: a pending submission is canceled and never sent; a second
// cancel finds nothing.
func TestCancel(t *testing.T) {
	e := newEnv(t)
	rec := e.schedule(t, "alice", time.Hour)

	got, err := e.svc.Cancel("alice", rec.ID, "jmap")
	if err != nil || got.State != StateCanceled || got.CanceledBy != "jmap" {
		t.Fatalf("cancel: %v %#v", err, got)
	}

	if s := e.book.get(rec.ID); s == nil || s.State != StateCanceled {
		t.Fatalf("settled %#v", s)
	}

	e.gone(t, rec)

	e.clock.add(2 * time.Hour)
	e.worker(t).round(context.Background())

	if e.relay.count() != 0 {
		t.Error("a canceled submission was sent")
	}

	if _, err := e.svc.Cancel("alice", rec.ID, "jmap"); !errors.Is(err, ErrNotFound) {
		t.Errorf("second cancel: %v", err)
	}
}

// TestCancelRacesSend: a cancel and the worker race for the same
// submission many times; exactly one of them wins each time.
func TestCancelRacesSend(t *testing.T) {
	e := newEnv(t, func(o *Options) { o.Concurrency = 8 })
	w := e.worker(t)

	const n = 50

	recs := make([]*Record, n)
	for i := range recs {
		recs[i] = e.schedule(t, fmt.Sprintf("user%d", i%5), 0)
	}

	var (
		wg       sync.WaitGroup
		canceled atomic.Int64
	)

	wg.Add(1)

	go func() {
		defer wg.Done()

		w.round(context.Background())
	}()

	for _, rec := range recs {
		wg.Add(1)

		go func() {
			defer wg.Done()

			_, err := e.svc.Cancel(rec.Account, rec.ID, "jmap")

			var np *NotPendingError

			switch {
			case err == nil:
				canceled.Add(1)
			case errors.As(err, &np), errors.Is(err, ErrNotFound):
			default:
				t.Errorf("cancel: %v", err)
			}
		}()
	}

	wg.Wait()
	w.round(context.Background())

	if sent := int64(e.relay.count()); sent+canceled.Load() != n {
		t.Fatalf("sent %d + canceled %d != %d", sent, canceled.Load(), n)
	}

	for _, rec := range recs {
		e.gone(t, rec)
	}
}

// TestCancelWhileSending: once the worker has begun to send, a cancel is
// refused.
func TestCancelWhileSending(t *testing.T) {
	e := newEnv(t)
	e.relay.hold = make(chan struct{})
	rec := e.schedule(t, "alice", 0)
	w := e.worker(t)

	done := make(chan struct{})

	go func() {
		defer close(done)

		w.round(context.Background())
	}()

	waitFor(t, func() bool {
		r, err := e.svc.Get("alice", rec.ID)
		return err == nil && r.State == StateSending
	})

	var np *NotPendingError
	if _, err := e.svc.Cancel("alice", rec.ID, "jmap"); !errors.As(err, &np) || np.State != StateSending {
		t.Errorf("cancel while sending: %v", err)
	}

	close(e.relay.hold)
	<-done

	if s := e.book.get(rec.ID); s == nil || s.State != StateFinal {
		t.Errorf("settled %#v", s)
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()

	for range 500 {
		if cond() {
			return
		}

		time.Sleep(5 * time.Millisecond)
	}

	t.Fatal("condition not reached")
}

// TestSendingAfterRestartNotResent: a record left in sending (the server
// stopped) is final with an unknown result once the sending timeout has
// passed, and the relay is never asked again.
func TestSendingAfterRestartNotResent(t *testing.T) {
	e := newEnv(t)
	rec := e.schedule(t, "alice", 0)

	// The pending -> sending step of a worker that then died.
	sending, err := update(e.kv, "alice", rec.ID, func(r *Record) error {
		r.State, r.Gen, r.SendingSince = StateSending, r.Gen+1, e.clock.Now()
		r.Attempts++

		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	w := e.worker(t)
	w.round(context.Background())

	if r, err := e.svc.Get("alice", rec.ID); err != nil || r.State != StateSending {
		t.Fatalf("within the timeout: %v %#v", err, r)
	}

	e.clock.add(time.Hour)
	w.round(context.Background())

	if e.relay.count() != 0 {
		t.Fatal("an interrupted submission was sent again")
	}

	s := e.book.get(rec.ID)
	if s == nil || s.State != StateFinal || !s.Unknown || s.Delivery["bob@example.net"].Delivered != "unknown" {
		t.Fatalf("settled %#v", s)
	}

	e.gone(t, sending)
}

// TestLateOutcomeAfterTimeout: a worker whose attempt outlasted the
// sending timeout finds the record taken for interrupted and records
// nothing; the message is not sent again.
func TestLateOutcomeAfterTimeout(t *testing.T) {
	e := newEnv(t)
	e.relay.hold = make(chan struct{})
	rec := e.schedule(t, "alice", 0)

	slow := e.worker(t)
	done := make(chan struct{})

	go func() {
		defer close(done)

		slow.round(context.Background())
	}()

	waitFor(t, func() bool {
		r, err := e.svc.Get("alice", rec.ID)
		return err == nil && r.State == StateSending
	})

	e.clock.add(2 * time.Hour)
	e.svc.handle(context.Background(), model.DueEntry{ID: EntryID("alice", rec.ID), DueKey: model.DueKey{Due: e.clock.Now(), Gen: 2}},
		"alice", rec.ID, e.clock.Now(), func() bool { return false })

	close(e.relay.hold)
	<-done

	if e.relay.count() != 1 {
		t.Fatalf("relay calls %d", e.relay.count())
	}

	if s := e.book.get(rec.ID); s == nil || !s.Unknown {
		t.Fatalf("settled %#v", s)
	}

	e.gone(t, rec)
}

// TestRetryBeforeTransfer: an error before the message was transferred
// is retried after a backoff; an error during the transfer is final with
// an unknown result and never retried.
func TestRetryBeforeTransfer(t *testing.T) {
	e := newEnv(t, func(o *Options) { o.RetryAttempts = 2 })
	e.relay.err = errors.New("relay connect: refused")
	rec := e.schedule(t, "alice", 0)
	w := e.worker(t)

	w.round(context.Background())

	r, err := e.svc.Get("alice", rec.ID)
	if err != nil || r.State != StatePending || r.Attempts != 1 || !r.Due.Equal(e.clock.Now().Add(time.Minute)) {
		t.Fatalf("after a failed attempt: %v %#v", err, r)
	}

	e.clock.add(time.Minute)
	w.round(context.Background())

	if s := e.book.get(rec.ID); s == nil || s.State != StateFailed || e.relay.count() != 2 {
		t.Fatalf("after the last attempt: calls %d, %#v", e.relay.count(), s)
	}

	e.gone(t, rec)

	e.relay.err = fmt.Errorf("relay DATA: %w: EOF", outbound.ErrInData)
	rec = e.schedule(t, "alice", 0)

	w.round(context.Background())
	e.clock.add(time.Hour)
	w.round(context.Background())

	if s := e.book.get(rec.ID); s == nil || !s.Unknown || e.relay.count() != 3 {
		t.Fatalf("interrupted transfer: calls %d, %#v", e.relay.count(), s)
	}
}

// TestRefused: a permanent refusal fails the submission with the
// relay's replies.
func TestRefused(t *testing.T) {
	e := newEnv(t)
	e.relay.report = &outbound.Report{Stage: "RCPT", Refused: map[string]outbound.Reply{
		"bob@example.net": {Code: 550, Message: "5.1.1 no such user"}}}
	rec := e.schedule(t, "alice", 0)
	e.worker(t).round(context.Background())

	s := e.book.get(rec.ID)
	if s == nil || s.State != StateFailed || s.Delivery["bob@example.net"].SMTPReply != "550 5.1.1 no such user" {
		t.Fatalf("settled %#v", s)
	}

	if e.relay.count() != 1 {
		t.Error("a refused submission was retried")
	}
}

// TestPurgeAccount: a purge removes the records and entries of the
// account's submissions (the copies go with the namespace), and leaves
// other accounts alone.
func TestPurgeAccount(t *testing.T) {
	e := newEnv(t)
	a1 := e.schedule(t, "alice", time.Hour)
	e.schedule(t, "alice", 2*time.Hour)
	b := e.schedule(t, "bob", time.Hour)

	u := e.user(t, "alice")

	n, err := e.svc.PurgeAccount(u, "alice")
	if err != nil || n != 2 {
		t.Fatalf("purge: %d %v", n, err)
	}

	if _, err := u.(model.NamespacePurger).PurgeNamespace(func() error { return nil }); err != nil {
		t.Fatal(err)
	}

	if _, err := e.svc.Get("alice", a1.ID); !errors.Is(err, ErrNotFound) {
		t.Error("record left")
	}

	if len(e.copies(t, "alice")) != 0 {
		t.Error("copies left")
	}

	if got := e.entries(t); len(got) != 1 || got[0].ID != EntryID("bob", b.ID) {
		t.Fatalf("entries %#v", got)
	}

	e.clock.add(3 * time.Hour)
	e.worker(t).round(context.Background())

	if e.relay.count() != 1 {
		t.Errorf("relay calls %d (only bob's)", e.relay.count())
	}
}

// TestCopyGone: a submission whose copy is gone fails without a relay
// call.
func TestCopyGone(t *testing.T) {
	e := newEnv(t)
	rec := e.schedule(t, "alice", 0)

	if err := e.user(t, "alice").(model.SubmissionCopies).DropSubmissionCopy(rec.GUID); err != nil {
		t.Fatal(err)
	}

	e.worker(t).round(context.Background())

	if s := e.book.get(rec.ID); s == nil || s.State != StateFailed || e.relay.count() != 0 {
		t.Fatalf("calls %d, %#v", e.relay.count(), s)
	}
}

// TestStaleEntries: an entry without record is dropped after the grace,
// an entry at the wrong position is moved to the record's.
func TestStaleEntries(t *testing.T) {
	e := newEnv(t)
	q := e.svc.q

	orphan := model.DueKey{Due: e.clock.Now(), Gen: 1}
	if err := q.Move(EntryID("alice", newID()), nil, &orphan); err != nil {
		t.Fatal(err)
	}

	rec := e.schedule(t, "alice", time.Hour)
	early := model.DueKey{Due: e.clock.Now(), Gen: 1}

	if err := q.Move(EntryID("alice", rec.ID), nil, &early); err != nil {
		t.Fatal(err)
	}

	w := e.worker(t)
	w.round(context.Background())

	if n := len(e.entries(t)); n != 2 {
		t.Fatalf("after one round %d entries (want the orphan within its grace and the record's)", n)
	}

	e.clock.add(11 * time.Minute)
	w.round(context.Background())

	got := e.entries(t)
	if len(got) != 1 || !got[0].Due.Equal(rec.Release) || e.relay.count() != 0 {
		t.Fatalf("entries %#v, calls %d", got, e.relay.count())
	}
}

// TestPartitions: a worker sends the submissions of the partitions it
// holds only; another worker takes the rest.
func TestPartitions(t *testing.T) {
	e := newEnv(t, func(o *Options) { o.Partitions, o.MaxPartitions = 4, 1 })

	var recs []*Record
	for i := range 20 {
		recs = append(recs, e.schedule(t, fmt.Sprintf("user%d", i), 0))
	}

	w1 := &worker{s: e.svc, held: map[int]model.Lease{}}
	w1.leases(true)
	t.Cleanup(w1.releaseAll)

	if len(w1.held) != 1 {
		t.Fatalf("held %d", len(w1.held))
	}

	var mine int

	for p := range w1.held {
		for _, r := range recs {
			if e.svc.partitionOf(r.Account) == p {
				mine++
			}
		}
	}

	w1.round(context.Background())

	if e.relay.count() != mine {
		t.Fatalf("sent %d, partition holds %d", e.relay.count(), mine)
	}

	// The other three partitions are free for three more workers.
	for range 3 {
		w := &worker{s: e.svc, held: map[int]model.Lease{}}
		w.leases(true)
		t.Cleanup(w.releaseAll)

		if len(w.held) != 1 {
			t.Fatalf("held %d", len(w.held))
		}

		w.round(context.Background())
	}

	if e.relay.count() != len(recs) {
		t.Fatalf("sent %d of %d", e.relay.count(), len(recs))
	}
}

// TestLeaseHandover: a second worker cannot take a held partition and
// takes it once the first lets it go; the queue is then served by it.
func TestLeaseHandover(t *testing.T) {
	e := newEnv(t)

	first := &worker{s: e.svc, held: map[int]model.Lease{}}
	first.leases(true)

	second := &worker{s: e.svc, held: map[int]model.Lease{}}
	second.leases(true)

	if len(first.held) != 1 || len(second.held) != 0 {
		t.Fatalf("held %d and %d", len(first.held), len(second.held))
	}

	rec := e.schedule(t, "alice", 0)

	second.round(context.Background())

	if e.relay.count() != 0 {
		t.Fatal("a worker without the lease sent")
	}

	first.releaseAll()
	second.leases(true)
	t.Cleanup(second.releaseAll)
	second.round(context.Background())

	if e.relay.count() != 1 {
		t.Fatalf("relay calls %d", e.relay.count())
	}

	e.gone(t, rec)
}

// TestLostLease: a worker whose lease was revoked stops and lets the
// next one take over.
func TestLostLease(t *testing.T) {
	e := newEnv(t)
	w := e.worker(t)

	e.kv.RevokeLocks()
	e.schedule(t, "alice", 0)
	w.round(context.Background())

	if e.relay.count() != 0 {
		t.Fatal("sent with a lost lease")
	}

	w.leases(true)
	w.round(context.Background())

	if e.relay.count() != 1 {
		t.Fatalf("relay calls %d", e.relay.count())
	}
}

// TestRunWakesOnNotify: the running worker sends a new submission when
// it becomes due, woken by the schedule's notify and its own timer.
func TestRunWakesOnNotify(t *testing.T) {
	e := newEnv(t, func(o *Options) { o.Poll = time.Hour; o.Now = time.Now })

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})

	go func() {
		defer close(done)

		e.svc.Run(ctx)
	}()

	time.Sleep(50 * time.Millisecond)

	u := e.user(t, "alice")
	if _, err := e.svc.Schedule(u, Spec{Account: "alice", ID: newID(), Release: time.Now().Add(200 * time.Millisecond),
		MailFrom: "alice", RcptTo: []string{"bob@example.net"}, Raw: []byte("x\r\n")}); err != nil {
		t.Fatal(err)
	}

	waitFor(t, func() bool { return e.relay.count() == 1 })

	cancel()
	<-done
}

// TestNoQueueWithoutIndexes: without the admin store indexes there is no
// queue.
func TestNoQueueWithoutIndexes(t *testing.T) {
	if _, err := New(Options{KV: memstore.NewAdminKVMode(model.KVIndexOff)}); !errors.Is(err, ErrNoQueue) {
		t.Fatalf("New: %v", err)
	}
}

// TestShutdownDoesNotCountAttempt: a dial the shutdown canceled leaves
// the submission pending, due as before, without an attempt counted.
func TestShutdownDoesNotCountAttempt(t *testing.T) {
	e := newEnv(t, func(o *Options) { o.RetryAttempts = 1 })
	rec := e.schedule(t, "alice", 0)
	w := e.worker(t)

	ctx, cancel := context.WithCancel(context.Background())
	e.relay.err = errors.New("relay connect: context canceled")
	e.relay.onCall = cancel

	got, err := e.svc.Get("alice", rec.ID)
	if err != nil {
		t.Fatal(err)
	}

	e.svc.send(ctx, got, nil, func() bool { return false })

	r, err := e.svc.Get("alice", rec.ID)
	if err != nil || r.State != StatePending || r.Attempts != 0 || !r.Due.Equal(rec.Due) {
		t.Fatalf("after a canceled dial: %v %#v", err, r)
	}

	// A canceled context starts nothing.
	w.round(ctx)

	if e.relay.count() != 1 {
		t.Fatalf("relay calls %d", e.relay.count())
	}
}

// TestAccountGone: a submission of an account whose directory entry is
// gone is failed, not sent.
func TestAccountGone(t *testing.T) {
	e := newEnv(t)
	e.svc.o.Directory = e.store
	rec := e.schedule(t, "alice", 0)

	ix, release, err := e.store.AccountIndex()
	if err != nil {
		t.Fatal(err)
	}

	if err := ix.Remove("alice"); err != nil {
		t.Fatal(err)
	}

	release()

	e.worker(t).round(context.Background())

	if s := e.book.get(rec.ID); s == nil || s.State != StateFailed || e.relay.count() != 0 {
		t.Fatalf("calls %d, %#v", e.relay.count(), s)
	}
}

// TestAccountLiveSends: with the directory checked, a live account's
// submission is sent.
func TestAccountLiveSends(t *testing.T) {
	e := newEnv(t)
	e.svc.o.Directory = e.store
	e.schedule(t, "alice", 0)
	e.worker(t).round(context.Background())

	if e.relay.count() != 1 {
		t.Fatalf("calls %d", e.relay.count())
	}
}

// TestLeasesTakenOnPollOnly: without take a worker only drops lost
// leases and tries no lock.
func TestLeasesTakenOnPollOnly(t *testing.T) {
	e := newEnv(t, func(o *Options) { o.Partitions = 4 })
	w := &worker{s: e.svc, held: map[int]model.Lease{}}
	t.Cleanup(w.releaseAll)

	w.leases(false)

	if len(w.held) != 0 {
		t.Fatalf("took %d leases without take", len(w.held))
	}

	w.leases(true)

	if len(w.held) != 4 {
		t.Fatalf("took %d of 4", len(w.held))
	}
}

// TestUnownedReported: overdue entries of partitions nobody here holds
// are counted.
func TestUnownedReported(t *testing.T) {
	e := newEnv(t, func(o *Options) { o.Partitions, o.MaxPartitions, o.Poll = 4, 1, time.Second })

	for i := range 20 {
		e.schedule(t, fmt.Sprintf("user%d", i), 0)
	}

	e.clock.add(time.Minute)

	w := &worker{s: e.svc, held: map[int]model.Lease{}}
	w.leases(true)
	t.Cleanup(w.releaseAll)
	w.round(context.Background())

	if w.unowned == 0 || w.unowned+e.relay.count() != 20 {
		t.Fatalf("unowned %d, sent %d", w.unowned, e.relay.count())
	}
}
