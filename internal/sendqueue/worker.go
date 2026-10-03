package sendqueue

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"slices"
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

// Run is the worker: it runs until ctx ends. At the end it hands its
// partitions over: it releases them and notifies the queue with their
// numbers, so that another worker takes them at once instead of at its
// next poll (stage B18b, F9).
func (s *Service) Run(ctx context.Context) {
	w := &worker{s: s, held: map[int]model.Lease{}, start: rand.IntN(s.o.Partitions)} //nolint:gosec // spreads the partitions, no secret

	defer w.handOver()

	events, errs, closeWatch := w.watch()
	defer func() { closeWatch() }()

	rewatch := false

	timer := time.NewTimer(s.o.Poll)
	defer timer.Stop()

	for {
		// Free partitions are taken over on the poll only: a notify
		// wakes every worker, and trying every lock then would cost
		// partitions × servers lock operations per submission. The
		// partitions a worker handed over are the exception: they are
		// named in its notify and tried at once.
		take := w.lastTake.IsZero() || time.Since(w.lastTake) >= s.o.Poll
		if take {
			w.lastTake = time.Now()
		}

		w.leases(take)

		wait := s.o.Poll
		if len(w.held) > 0 {
			wait = w.round(ctx)
		}

		timer.Reset(wait)

		select {
		case <-ctx.Done():
			return
		case data := <-events:
			w.takeReleased(releasedPartitions(data))
		case <-errs:
			// A broken watch (an OSD drop, a key rotation) is set up
			// again on the next pass; until then the poll remains.
			closeWatch()
			events, errs, closeWatch = nil, nil, func() {}
			rewatch = true
		case <-timer.C:
		}

		if rewatch {
			events, errs, closeWatch = w.watch()
			rewatch = events == nil
		}
	}
}

// payloadQueue is a queue whose notifies carry data (sortedindex.Queue).
type payloadQueue interface {
	WatchPayload() (model.PayloadWatch, error)
	NotifyPayload(data []byte)
}

// watch watches the queue, with the data of the notifies when the queue
// offers it, and the watch's errors; nil channels without a watch (the
// poll remains).
func (w *worker) watch() (<-chan []byte, <-chan error, func()) {
	if pq, ok := w.s.q.(payloadQueue); ok {
		if watch, err := pq.WatchPayload(); err == nil {
			return watch.Events(), watch.Errors(), watch.Close
		}
	}

	watch, err := w.s.q.Watch()
	if err != nil {
		w.s.o.Log.Warn("sendqueue: watch the submission queue; polling only", "err", err)

		return nil, nil, func() {}
	}

	// A plain watch: its events are forwarded without data.
	out := make(chan []byte)
	stop := make(chan struct{})

	go func() {
		for {
			select {
			case <-stop:
				return
			case <-watch.Events():
				select {
				case out <- nil:
				case <-stop:
					return
				}
			}
		}
	}()

	return out, watch.Errors(), func() { close(stop); watch.Close() }
}

// handoverNotice is the payload of the notify a worker sends when it
// hands its partitions over.
type handoverNotice struct {
	Released []int `json:"released"`
}

// releasedPartitions are the partitions a notify names (none for a
// notify without data: a new submission).
func releasedPartitions(data []byte) []int {
	if len(data) == 0 {
		return nil
	}

	var n handoverNotice
	if json.Unmarshal(data, &n) != nil {
		return nil
	}

	return n.Released
}

// takeReleased tries the partitions another worker handed over, up to
// the bound.
func (w *worker) takeReleased(ps []int) {
	limit := w.s.limit()

	for _, p := range ps {
		if _, ok := w.held[p]; ok || p < 0 || p >= w.s.o.Partitions || len(w.held) >= limit {
			continue
		}

		w.take(p)
	}
}

// handOver gives the leases back at shutdown and tells the other
// workers which partitions are free now.
func (w *worker) handOver() {
	released := make([]int, 0, len(w.held))

	for p := range w.held {
		released = append(released, p)
	}

	slices.Sort(released)
	w.releaseAll()

	if len(released) == 0 {
		return
	}

	if pq, ok := w.s.q.(payloadQueue); ok {
		data, _ := json.Marshal(handoverNotice{Released: released})
		pq.NotifyPayload(data)
	}
}

// worker is the state of Run.
type worker struct {
	s     *Service
	held  map[int]model.Lease
	start int
	// counted is when the queue was last counted for the gauge,
	// lastTake when free partitions were last tried.
	counted, lastTake time.Time
	// unowned counts, per round, the entries of partitions this worker
	// does not hold that are overdue by more than a poll; warned is
	// when that was last logged.
	unowned int
	warned  time.Time
}

// lockName is the lease name of partition p.
func lockName(p int) string { return fmt.Sprintf("%s/p%d", model.SubmissionQueue, p) }

// partitionOf is the partition of an account.
func (s *Service) partitionOf(account string) int { return model.IndexShard(account, s.o.Partitions) }

// leases drops the leases that are lost and, with take, takes free ones
// up to the bound, starting at a random partition so that servers
// spread.
func (w *worker) leases(take bool) {
	for p, l := range w.held {
		if l.Lost() {
			l.Release()
			delete(w.held, p)
			w.s.o.Log.Info("sendqueue: partition lease lost", "partition", p)
		}
	}

	if !take {
		return
	}

	limit := w.s.limit()

	for i := 0; i < w.s.o.Partitions && len(w.held) < limit; i++ {
		p := (w.start + i) % w.s.o.Partitions
		if _, ok := w.held[p]; ok {
			continue
		}

		w.take(p)
	}
}

// limit is how many partitions this worker holds at most.
func (s *Service) limit() int {
	limit := s.o.MaxPartitions
	if limit <= 0 || limit > s.o.Partitions {
		limit = s.o.Partitions
	}

	return limit
}

// take tries the lease of one partition.
func (w *worker) take(p int) {
	l, err := w.s.o.KV.LockQueue(lockName(p), leaseTTL)

	switch {
	case err == nil:
		w.held[p] = l
		w.s.o.Log.Info("sendqueue: partition lease taken", "partition", p, "event", "submission_lease")
	case !errors.Is(err, model.ErrLocked):
		w.s.o.Log.Warn("sendqueue: partition lease", "partition", p, "err", err)
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
	w.unowned = 0

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
	w.reportUnowned(now)

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
			if now.Sub(e.Due) > w.s.o.Poll {
				w.unowned++
			}

			continue
		}

		lost := w.lostFn(p)
		if lost() || ctx.Err() != nil {
			continue
		}

		sem <- struct{}{}

		// A shutdown that came while this waited for a slot starts
		// nothing more.
		if ctx.Err() != nil {
			<-sem
			break
		}

		wg.Add(1)

		go func() {
			defer wg.Done()
			defer func() { <-sem }()

			w.s.handle(ctx, e, account, id, now, lost)
		}()
	}

	wg.Wait()
}

// reportUnowned publishes the overdue entries of partitions this worker
// does not hold, and warns (at most once per poll) when there are any:
// a partition nobody holds is never sent (servers × max_partitions must
// reach partitions).
func (w *worker) reportUnowned(now time.Time) {
	metrics.SubmissionQueue.WithLabelValues("unowned").Set(float64(w.unowned))

	if w.unowned == 0 || now.Sub(w.warned) < w.s.o.Poll {
		return
	}

	w.warned = now
	w.s.o.Log.Warn("sendqueue: due submissions of partitions this server does not hold are overdue; "+
		"is every partition held (servers x max_partitions >= partitions)?", "overdue", w.unowned,
		"held", len(w.held), "partitions", w.s.o.Partitions)
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
	if lost() || ctx.Err() != nil {
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
	state   State // final, failed, or pending (retry)
	unknown bool
	// uncounted is a retry that does not count as an attempt: the server
	// is shutting down and the relay was never reached.
	uncounted bool
	// gone marks a failure because the account or the copy is gone.
	gone     bool
	delivery map[string]Delivery
	reason   string
}

// attempt reads the copy and hands it to the relay.
func (s *Service) attempt(ctx context.Context, rec *Record) outcome {
	switch reason, err := s.accountGone(rec.Account); {
	case err != nil:
		return s.retry(rec, "the account directory could not be read: "+err.Error())
	case reason != "":
		return outcome{state: StateFailed, gone: true, reason: reason, delivery: deliveries(rec.RcptTo, Delivery{Delivered: DeliveredNo})}
	}

	raw, err := s.readCopy(rec)

	switch {
	case errors.Is(err, model.ErrNoBlob):
		return outcome{state: StateFailed, gone: true, reason: "the copy of the message is gone",
			delivery: deliveries(rec.RcptTo, Delivery{Delivered: DeliveredNo})}
	case err != nil:
		return s.retry(rec, "the copy of the message could not be read: "+err.Error())
	}

	var as *outbound.XClient
	if rec.XClient {
		as = &outbound.XClient{Login: rec.Login, Addr: rec.ClientAddr}
	}

	rep, err := s.o.Relay.Submit(ctx, as, rec.MailFrom, rec.RcptTo, raw)

	return s.classify(ctx, rec, rep, err)
}

// classify is the outcome of the relay's answer (or error).
func (s *Service) classify(ctx context.Context, rec *Record, rep *outbound.Report, err error) outcome {
	switch {
	case err != nil && ctx.Err() != nil && !errors.Is(err, outbound.ErrInData):
		// The shutdown canceled the dial (the only use of ctx): nothing
		// was transferred, and the attempt does not count.
		return outcome{state: StatePending, uncounted: true, reason: "the server stopped before sending: " + err.Error()}
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

// accountGone says why an account may not send any more: it is being
// purged (the purge marks its directory entry before it removes the
// queue records), or it has neither a directory entry nor a registry
// (purged). An entry alone is no proof of life or death: an account
// created before the directory has an entry only once the backfill ran,
// so an account without entry but with a registry is live. A directory
// or registry that cannot be read is an error (the caller retries)
// rather than a send on a guess.
func (s *Service) accountGone(account string) (string, error) {
	if s.o.Directory == nil {
		return "", nil
	}

	ix, release, err := s.o.Directory.AccountIndex()
	if err != nil {
		return "", err
	}
	defer release()

	e, ok, err := model.GetAccountEntry(ix, account)

	switch {
	case err != nil:
		return "", err
	case ok && e.State == model.AccountPurging:
		return "the account is being purged", nil
	case ok:
		return "", nil
	}

	live, err := s.o.Directory.HasRegistry(account)

	switch {
	case err != nil:
		return "", err
	case !live:
		return "the account no longer exists", nil
	}

	return "", nil
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
		r.State, r.Reason, r.Unknown, r.Gone = out.state, out.reason, out.unknown, out.gone

		if out.uncounted {
			r.Attempts-- // Due stays: the next worker sends at once
			return nil
		}

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
