package sendqueue

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"sync"
	"time"

	"shardpost/internal/metrics"
	"shardpost/internal/model"
	"shardpost/internal/outbound"
)

// The worker (scaling plan B2). The accounts are split into partitions
// by hash; each partition has a lease (a renewed cls_lock), and a server
// holds up to MaxPartitions of them. The holder of a partition sends the
// due submissions of its accounts. A worker wakes on a notify on shard 0
// of the queue (a new submission), when the next entry it saw becomes
// due, and on the poll otherwise.
//
// The leases only spread the work: every write is a compare-and-swap on
// the record, so two workers that both believe they hold a partition (a
// lost lease, servers with different partition counts) cannot send one
// submission twice.

// Run is the worker: it runs until ctx ends.
func (s *Service) Run(ctx context.Context) {
	w := &worker{s: s, held: map[int]model.Lease{}, start: rand.IntN(s.o.Partitions)} //nolint:gosec // spreads the partitions, no secret

	defer w.releaseAll()

	var events <-chan struct{}

	if watch, err := s.q.Watch(); err != nil {
		s.o.Log.Warn("sendqueue: watch the submission queue; polling only", "err", err)
	} else {
		defer watch.Close()

		events = watch.Events()
	}

	timer := time.NewTimer(s.o.Poll)
	defer timer.Stop()

	for {
		w.leases()

		wait := s.o.Poll
		if len(w.held) > 0 {
			wait = w.round(ctx)
		}

		timer.Reset(wait)

		select {
		case <-ctx.Done():
			return
		case <-events:
		case <-timer.C:
		}
	}
}

// worker is the state of Run.
type worker struct {
	s     *Service
	held  map[int]model.Lease
	start int
	// counted is when the queue was last counted for the gauge.
	counted time.Time
}

// lockName is the lease name of partition p.
func lockName(p int) string { return fmt.Sprintf("%s/p%d", model.SubmissionQueue, p) }

// partitionOf is the partition of an account.
func (s *Service) partitionOf(account string) int { return model.IndexShard(account, s.o.Partitions) }

// leases drops the leases that are lost and takes free ones up to the
// bound, starting at a random partition so that servers spread.
func (w *worker) leases() {
	for p, l := range w.held {
		if l.Lost() {
			l.Release()
			delete(w.held, p)
			w.s.o.Log.Info("sendqueue: partition lease lost", "partition", p)
		}
	}

	limit := w.s.o.MaxPartitions
	if limit <= 0 || limit > w.s.o.Partitions {
		limit = w.s.o.Partitions
	}

	for i := 0; i < w.s.o.Partitions && len(w.held) < limit; i++ {
		p := (w.start + i) % w.s.o.Partitions
		if _, ok := w.held[p]; ok {
			continue
		}

		l, err := w.s.o.KV.LockQueue(lockName(p), leaseTTL)

		switch {
		case err == nil:
			w.held[p] = l
			w.s.o.Log.Info("sendqueue: partition lease taken", "partition", p, "event", "submission_lease")
		case !errors.Is(err, model.ErrLocked):
			w.s.o.Log.Warn("sendqueue: partition lease", "partition", p, "err", err)
		}
	}
}

// releaseAll gives the leases back (shutdown).
func (w *worker) releaseAll() {
	for p, l := range w.held {
		l.Release()
		delete(w.held, p)
	}
}

// lostFn reports whether the lease of p is gone.
func (w *worker) lostFn(p int) func() bool {
	l := w.held[p]

	return func() bool { return l == nil || l.Lost() }
}

// page is the number of entries one read of the queue returns; pages
// bounds the reads of one round.
const (
	page  = 100
	pages = 50
)

// round handles what is due in the held partitions and returns how
// long to wait for the next round.
func (w *worker) round(ctx context.Context) time.Duration {
	now := w.s.o.Now()
	after := ""

	for range pages {
		due, err := w.s.q.DueAfter(now, after, page)
		if err != nil {
			w.s.o.Log.Warn("sendqueue: due", "err", err)
			return w.s.o.Poll
		}

		w.handlePage(ctx, due, now)

		if len(due) < page || ctx.Err() != nil {
			break
		}

		after = due[len(due)-1].Key()
	}

	w.gauge(now)

	return w.nextWait(now)
}

// handlePage handles the entries of the held partitions, up to
// Concurrency at once.
func (w *worker) handlePage(ctx context.Context, due []model.DueEntry, now time.Time) {
	sem := make(chan struct{}, w.s.o.Concurrency)

	var wg sync.WaitGroup

	for _, e := range due {
		account, id, ok := SplitEntryID(e.ID)
		if !ok {
			w.s.dropAfterGrace(e, now)
			continue
		}

		p := w.s.partitionOf(account)
		if _, held := w.held[p]; !held {
			continue
		}

		lost := w.lostFn(p)
		if lost() || ctx.Err() != nil {
			continue
		}

		sem <- struct{}{}

		wg.Add(1)

		go func() {
			defer wg.Done()
			defer func() { <-sem }()

			w.s.handle(ctx, e, account, id, now, lost)
		}()
	}

	wg.Wait()
}

// nextWait is the time until the first entry due after now (bounded by
// the poll).
func (w *worker) nextWait(now time.Time) time.Duration {
	after := model.FormatDueKey("", model.DueKey{Due: now})[:dueDigits]

	next, err := w.s.q.DueAfter(now.Add(w.s.o.Poll), after, 1)
	if err != nil || len(next) == 0 {
		return w.s.o.Poll
	}

	return min(max(next[0].Due.Sub(now), 10*time.Millisecond), w.s.o.Poll)
}

// dueDigits is the width of the due time in a queue key.
const dueDigits = 20

// gauge counts the queue for the metric, by the holder of partition 0,
// at most once per poll.
func (w *worker) gauge(now time.Time) {
	if _, ok := w.held[0]; !ok || now.Sub(w.counted) < w.s.o.Poll {
		return
	}

	w.counted = now

	far := now.Add(100 * 365 * 24 * time.Hour)
	all, due := 0, 0

	for after := ""; all < countLimit; {
		entries, err := w.s.q.DueAfter(far, after, 1000)
		if err != nil {
			return
		}

		for _, e := range entries {
			all++

			if !e.Due.After(now) {
				due++
			}
		}

		if len(entries) < 1000 {
			break
		}

		after = entries[len(entries)-1].Key()
	}

	metrics.SubmissionQueue.WithLabelValues("all").Set(float64(all))
	metrics.SubmissionQueue.WithLabelValues("due").Set(float64(due))
}

// dropAfterGrace removes an entry that has no usable record once it is
// older than the grace (its writer may still be between entry and
// record).
func (s *Service) dropAfterGrace(e model.DueEntry, now time.Time) {
	if now.Sub(e.Due) < s.o.Grace {
		stale(staleSkipped)
		return
	}

	if err := s.q.Move(e.ID, []model.DueKey{e.DueKey}, nil); err != nil {
		s.o.Log.Warn("sendqueue: drop queue entry", "entry", e.ID, "err", err)
		return
	}

	stale(staleRemoved)
}

// handle handles one due entry: it compares it with its record and
// sends, finishes or repairs.
func (s *Service) handle(ctx context.Context, e model.DueEntry, account, id string, now time.Time, lost func() bool) {
	rec, err := get(s.o.KV, account, id)

	switch {
	case errors.Is(err, ErrNotFound):
		s.dropAfterGrace(e, now)
		return
	case err != nil:
		s.o.Log.Warn("sendqueue: read record", "account", account, "id", id, "err", err)
		return
	case e.Gen > rec.Gen:
		// Newer than the record: a writer that writes the entry first
		// is still on its way.
		s.dropAfterGrace(e, now)
		return
	}

	if !lost() {
		s.act(ctx, e, rec, now, lost)
	}
}

// act does what a record whose entry came due needs.
func (s *Service) act(ctx context.Context, e model.DueEntry, rec *Record, now time.Time, lost func() bool) {
	cur := rec.position(s.o.SendingTimeout)

	var extra []model.DueKey
	if !model.SameDueKey(cur, e.DueKey) {
		extra = []model.DueKey{e.DueKey}
	}

	switch {
	case rec.State.Terminal():
		if err := s.settle(rec, extra); err != nil {
			s.o.Log.Warn("sendqueue: clean-up", "account", rec.Account, "id", rec.ID, "err", err)
		}
	case rec.State == StatePending && !rec.Due.After(now):
		s.send(ctx, rec, extra, lost)
	case rec.State == StateSending && !now.Before(rec.SendingSince.Add(s.o.SendingTimeout)):
		s.interrupted(rec, extra)
	case extra != nil:
		if err := s.q.Move(e.ID, extra, &cur); err != nil {
			s.o.Log.Warn("sendqueue: reschedule", "entry", e.ID, "err", err)
			return
		}

		stale(staleRescheduled)
	}
}

// interrupted ends a submission that stayed in sending past the sending
// timeout: final, with an unknown result. It is not sent again.
func (s *Service) interrupted(rec *Record, extra []model.DueKey) {
	now := s.o.Now().UTC()

	fin, err := update(s.o.KV, rec.Account, rec.ID, func(r *Record) error {
		if r.Gen != rec.Gen || r.State != StateSending {
			return errChanged
		}

		r.State, r.Gen, r.Finished, r.Unknown = StateFinal, r.Gen+1, now, true
		r.Reason = "the sending was interrupted; the message may or may not have been relayed, and is not sent again"
		r.Delivery = deliveries(r.RcptTo, Delivery{Delivered: DeliveredUnknown})

		return nil
	})
	if err != nil {
		if !errors.Is(err, errChanged) {
			s.o.Log.Warn("sendqueue: interrupted submission", "account", rec.Account, "id", rec.ID, "err", err)
		}

		return
	}

	count(outcomeUnknown)
	s.o.Log.Warn("sendqueue: submission interrupted while sending; its result is unknown and it is not sent again",
		"account", rec.Account, "id", rec.ID, "sending_since", rec.SendingSince)

	if err := s.settle(fin, append(extra, rec.position(s.o.SendingTimeout))); err != nil {
		s.o.Log.Warn("sendqueue: clean-up", "account", rec.Account, "id", rec.ID, "err", err)
	}
}

// send makes one attempt: pending -> sending by compare-and-swap, then
// the relay, then the outcome by compare-and-swap on the generation it
// wrote.
func (s *Service) send(ctx context.Context, rec *Record, extra []model.DueKey, lost func() bool) {
	if lost() {
		return
	}

	now := s.o.Now().UTC()

	sending, err := update(s.o.KV, rec.Account, rec.ID, func(r *Record) error {
		if r.Gen != rec.Gen || r.State != StatePending || r.Due.After(now) {
			return errChanged
		}

		r.State, r.Gen, r.SendingSince = StateSending, r.Gen+1, now
		r.Attempts++

		return nil
	})
	if err != nil {
		if !errors.Is(err, errChanged) {
			s.o.Log.Warn("sendqueue: start sending", "account", rec.Account, "id", rec.ID, "err", err)
		}

		return
	}

	s.move(sending, append(extra, rec.position(s.o.SendingTimeout)))

	out := s.attempt(ctx, sending)

	s.finish(sending, out)
}

// outcome is the result of one attempt.
type outcome struct {
	state    State // final, failed, or pending (retry)
	unknown  bool
	delivery map[string]Delivery
	reason   string
}

// attempt reads the copy and hands it to the relay.
func (s *Service) attempt(ctx context.Context, rec *Record) outcome {
	raw, err := s.readCopy(rec)

	switch {
	case errors.Is(err, model.ErrNoBlob):
		return outcome{state: StateFailed, reason: "the copy of the message is gone",
			delivery: deliveries(rec.RcptTo, Delivery{Delivered: DeliveredNo})}
	case err != nil:
		return s.retry(rec, "the copy of the message could not be read: "+err.Error())
	}

	var as *outbound.XClient
	if rec.XClient {
		as = &outbound.XClient{Login: rec.Login, Addr: rec.ClientAddr}
	}

	rep, err := s.o.Relay.Submit(ctx, as, rec.MailFrom, rec.RcptTo, raw)

	switch {
	case errors.Is(err, outbound.ErrInData):
		return outcome{state: StateFinal, unknown: true, reason: err.Error(),
			delivery: deliveries(rec.RcptTo, Delivery{Delivered: DeliveredUnknown})}
	case err != nil:
		// Nothing was transferred.
		return s.retry(rec, err.Error())
	case rep.Accepted():
		return outcome{state: StateFinal, delivery: deliveries(rec.RcptTo, Delivery{SMTPReply: rep.Final.String(), Delivered: DeliveredQueued})}
	case rep.Temporary():
		return s.retry(rec, refusal(rep))
	}

	out := outcome{state: StateFailed, reason: refusal(rep), delivery: deliveries(rec.RcptTo, Delivery{SMTPReply: rep.Final.String(), Delivered: DeliveredNo})}
	for rcpt, rp := range rep.Refused {
		out.delivery[rcpt] = Delivery{SMTPReply: rp.String(), Delivered: DeliveredNo}
	}

	return out
}

// readCopy reads the copy of a submission's message.
func (s *Service) readCopy(rec *Record) ([]byte, error) {
	u, err := s.o.Accounts.User(rec.Account)
	if err != nil {
		return nil, err
	}
	defer u.Close()

	copies, ok := u.(model.SubmissionCopies)
	if !ok {
		return nil, ErrNoCopies
	}

	return copies.ReadSubmissionCopy(rec.GUID)
}

// retry is the outcome of a failure before the message was transferred:
// pending again after a backoff while attempts are left, else failed.
func (s *Service) retry(rec *Record, reason string) outcome {
	if rec.Attempts >= s.o.RetryAttempts {
		return outcome{state: StateFailed, reason: "given up after " + fmt.Sprint(rec.Attempts) + " attempts: " + reason,
			delivery: deliveries(rec.RcptTo, Delivery{Delivered: DeliveredNo})}
	}

	return outcome{state: StatePending, reason: reason}
}

// backoff is the delay before attempt n+1.
func (s *Service) backoff(attempts int) time.Duration {
	d := s.o.RetryBase
	for i := 1; i < attempts && d < maxRetryDelay; i++ {
		d *= 2
	}

	return min(d, maxRetryDelay)
}

// finish records the outcome of an attempt by compare-and-swap on the
// generation the attempt wrote, moves the entry and cleans up a
// terminal submission. A record that changed meanwhile (the sending
// timeout passed and another worker took the attempt for interrupted,
// or the account was purged) keeps what it says.
func (s *Service) finish(sending *Record, out outcome) {
	now := s.o.Now().UTC()

	rec, err := update(s.o.KV, sending.Account, sending.ID, func(r *Record) error {
		if r.Gen != sending.Gen || r.State != StateSending {
			return errChanged
		}

		r.Gen++
		r.State, r.Reason, r.Unknown = out.state, out.reason, out.unknown

		if out.state == StatePending {
			r.Due = now.Add(s.backoff(r.Attempts))
			return nil
		}

		r.Finished, r.Delivery = now, out.delivery

		return nil
	})

	switch {
	case errors.Is(err, errChanged), errors.Is(err, ErrNotFound):
		count(outcomeConflict)
		s.o.Log.Warn("sendqueue: the submission changed while it was sent (sending timeout too short, or purged); "+
			"its outcome is not recorded", "account", sending.Account, "id", sending.ID, "outcome", out.state,
			"unknown", out.unknown, "reason", out.reason)

		return
	case err != nil:
		// The record stays in sending and counts as interrupted after
		// the sending timeout: never sent again.
		s.o.Log.Error("sendqueue: outcome of a submission not recorded", "account", sending.Account, "id", sending.ID,
			"outcome", out.state, "err", err)

		return
	}

	s.countOutcome(rec)
	s.move(rec, []model.DueKey{sending.position(s.o.SendingTimeout)})

	if !rec.State.Terminal() {
		return
	}

	if err := s.settle(rec, nil); err != nil {
		s.o.Log.Warn("sendqueue: clean-up", "account", rec.Account, "id", rec.ID, "err", err)
	}
}

// countOutcome counts and logs the outcome of an attempt.
func (s *Service) countOutcome(rec *Record) {
	switch {
	case rec.State == StatePending:
		count(outcomeRetried)
		s.o.Log.Info("sendqueue: submission retried", "account", rec.Account, "id", rec.ID, "attempts", rec.Attempts,
			"due", rec.Due, "reason", rec.Reason)
	case rec.Unknown:
		count(outcomeUnknown)
		s.o.Log.Warn("sendqueue: submission result unknown; not sent again", "account", rec.Account, "id", rec.ID,
			"reason", rec.Reason)
	case rec.State == StateFinal:
		count(outcomeSent)
		s.o.Log.Info("sendqueue: submission sent", "account", rec.Account, "id", rec.ID)
	default:
		count(outcomeFailed)
		s.o.Log.Warn("sendqueue: submission failed", "account", rec.Account, "id", rec.ID, "reason", rec.Reason)
	}
}

// deliveries is d for every recipient.
func deliveries(rcpts []string, d Delivery) map[string]Delivery {
	out := make(map[string]Delivery, len(rcpts))
	for _, r := range rcpts {
		out[r] = d
	}

	return out
}

// refusal describes a refused transaction.
func refusal(rep *outbound.Report) string {
	if rep.Final.Code != 0 {
		return rep.Stage + ": " + rep.Final.String()
	}

	for rcpt, rp := range rep.Refused {
		return rep.Stage + " " + rcpt + ": " + rp.String()
	}

	return rep.Stage
}
