package imapsieve

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math/rand/v2"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"shardpost/internal/config"
	"shardpost/internal/extprograms"
	"shardpost/internal/membudget"
	"shardpost/internal/model"
)

// labelResult is the label of the learning counters.
const labelResult = "result"

var learningResults = promauto.NewCounterVec(prometheus.CounterOpts{Name: "imap_imapsieve_learning_total", Help: "Learning queue and delivery outcomes."}, []string{labelResult})
var scriptResults = promauto.NewCounterVec(prometheus.CounterOpts{Name: "imap_imapsieve_events_total", Help: "IMAPSieve outcomes by bounded phase."}, []string{phaseName})

// learningLockAttempts counts the attempts to take the worker lease by
// result (taken, busy): a server without the lease tries every
// imapsieve.learning.lock_retry_seconds (±50 %), not every second.
var learningLockAttempts = promauto.NewCounterVec(prometheus.CounterOpts{Name: "imap_imapsieve_learning_lock_attempts_total",
	Help: "Attempts to take the learning worker lease, by result (taken, busy)."}, []string{labelResult})

// DefaultLockRetry is the interval of the lease attempts of a worker
// without the lease when the configuration names none.
const DefaultLockRetry = 20 * time.Second

// learningWakeups counts the worker passes a queue notify started (the
// once-a-second poll is not counted).
var learningWakeups = promauto.NewCounter(prometheus.CounterOpts{Name: "imap_imapsieve_learning_wakeups_total",
	Help: "Learning worker passes started by a queue notify."})

// The metrics of the holder (stage B18b): the duration of a job by
// result (delivered, retry, dead, lost, canceled, deferred), the jobs
// due at the last pass, and the age of the oldest of them.
var (
	learningJobSeconds = promauto.NewHistogramVec(prometheus.HistogramOpts{Name: "imap_imapsieve_learning_job_seconds",
		Help:    "Duration of a learning job in the holder, by result.",
		Buckets: []float64{.01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10, 30, 60},
	}, []string{labelResult})
	learningDue = promauto.NewGauge(prometheus.GaugeOpts{Name: "imap_imapsieve_learning_due",
		Help: "Learning jobs due at the holder's last pass (up to the page of 100)."})
	learningLag = promauto.NewGauge(prometheus.GaugeOpts{Name: "imap_imapsieve_learning_lag_seconds",
		Help: "Age of the oldest learning job due at the holder's last pass."})
)

// Results of learningJobSeconds.
const (
	resultDelivered = "delivered"
	resultRetry     = "retry"
	resultDead      = "dead"
	resultLost      = "lost"
	resultCanceled  = "canceled"
	resultDeferred  = "deferred"
)

// Job owns an independent copy of the message, so EXPUNGE cannot invalidate a retry.
type Job struct {
	Delivered        bool
	Owner, MailboxID string
	Prepared         bool
	Created          time.Time
	ID               string
	Action           string // the extprograms entry to run (field name kept for stored jobs)
	Attempts         int
	Next             time.Time
	Dead             bool
	Size             int64
	// Retries counts manual retries of a dead job (admin API).
	Retries int `json:",omitempty"`
	// Gen counts the writes of the job through the queue index (scaling
	// plan B5): it is part of the job's due queue key, so a reader can
	// tell an entry the job was moved away from. Servers without the
	// index drop it when they rewrite a job; the readers repair that.
	Gen uint64 `json:",omitempty"`
}

// Errors of the learning queue administration.
var (
	ErrNoJob   = errors.New("no such learning job")
	ErrNotDead = errors.New("only dead learning jobs can be retried")
	// ErrNoWatch is returned by Watcher.Watch when the queue cannot be
	// watched (the worker then only polls).
	ErrNoWatch = errors.New("the learning queue cannot be watched")
)

// Queue is the persistent learning job queue with a worker lease.
type Queue interface {
	Put(Job, []byte) error
	// List returns up to 100 live jobs that are due, oldest first.
	List() ([]Job, error)
	Read(Job) ([]byte, error)
	Update(Job) error
	Delete(Job) error
	// Lock takes the worker lease, which stays held (renewed) until
	// Release or until it is reported lost.
	Lock() (model.Lease, error)
	Ready(Job) (bool, error)
	Cleanup(Job) error
}

// Watcher is a Queue whose writers notify the worker when a job becomes
// due earlier (enqueued, retried).
type Watcher interface {
	Watch() (model.Watch, error)
}

// Programs is what the worker needs from the allowlist runner.
type Programs interface {
	Run(ctx context.Context, kind, name string, scriptArgs []string, input []byte, c extprograms.Context) (extprograms.Result, error)
}

// LearningWorker sends queued learning jobs to their programs, with
// retries and dead-lettering. One worker of the cluster holds the queue
// lease at a time and keeps it while it runs.
//
// With Config.Concurrency above 1 (stage B18b) the holder runs jobs in
// goroutines: jobs of different accounts in parallel, jobs of one
// account one after the other in due order (a semaphore per owner,
// tried without waiting: a job whose owner is busy is skipped and
// offered again at the next pass), never the same job twice at once (a
// set of the jobs in flight). The runners live on a context of their
// own: at shutdown they get Config.ShutdownSeconds to end, then they
// are cut off and the next holder runs their jobs again. With a
// concurrency of 1 a pass runs its jobs one after the other, as before.
type LearningWorker struct {
	Config   config.Learning
	Queue    Queue
	Programs Programs
	// Now is the clock and Jitter the factor of a retry interval, in
	// [0.5, 1.5) (time.Now and a random one when nil; tests set them).
	Now    func() time.Time
	Jitter func() float64

	mu    sync.Mutex
	lease model.Lease
	// nextTry is when a worker without the lease tries to take it next:
	// the lease is one object all servers lock, so they try every
	// LockRetrySeconds with jitter, not on every pass.
	nextTry time.Time
	// inflight are the jobs being run, owners the semaphore per owner
	// (an entry is removed at 0), slots the bound of the runners, wg
	// waits for them; jobCtx is their context, cut off by cancelJobs at
	// the end of the shutdown grace.
	inflight   map[string]struct{}
	owners     map[string]int
	slots      chan struct{}
	wg         sync.WaitGroup
	jobCtx     context.Context
	cancelJobs context.CancelFunc
}

// NewLearningWorker builds a worker for queue q.
func NewLearningWorker(c config.Learning, q Queue, programs Programs) *LearningWorker {
	w := &LearningWorker{Config: c, Queue: q, Programs: programs, inflight: map[string]struct{}{}, owners: map[string]int{}}

	if n := w.concurrency(); n > 1 {
		w.slots = make(chan struct{}, n)
	}

	w.jobCtx, w.cancelJobs = context.WithCancel(context.Background())

	return w
}

// concurrency is the number of runners (1: the pass runs its jobs
// itself).
func (w *LearningWorker) concurrency() int {
	return int(max(w.Config.Concurrency, 1))
}

// shutdownGrace is how long running jobs get at the end.
func (w *LearningWorker) shutdownGrace() time.Duration {
	if w.Config.ShutdownSeconds <= 0 {
		return DefaultShutdownGrace
	}

	return time.Duration(w.Config.ShutdownSeconds) * time.Second
}

// DefaultShutdownGrace is how long running learning jobs get at the end
// when the configuration names nothing.
const DefaultShutdownGrace = 10 * time.Second

// Enqueue stores a learning job for the event's message; the job ID is
// derived from the message and action, so a repeated event is one job.
func (w *LearningWorker) Enqueue(ctx context.Context, e Event, action string, raw []byte) error {
	if int64(len(raw)) > w.Config.MaxMessageBytes {
		learningResults.WithLabelValues("oversize").Inc()
		return fmt.Errorf("learning message too large")
	}

	if err := ctx.Err(); err != nil {
		return err
	}

	sum := sha256.Sum256([]byte(fmt.Sprintf("%s\x00%s\x00%d\x00%d\x00%s", e.Owner, e.MailboxID, e.Message.UID, e.Message.ModSeq, action)))

	j := Job{ID: hex.EncodeToString(sum[:]), Action: action, Owner: e.Owner, MailboxID: e.MailboxID, Size: int64(len(raw)), Next: time.Now()}
	if err := w.Queue.Put(j, raw); err != nil {
		learningResults.WithLabelValues("enqueue_error").Inc()
		return err
	}

	learningResults.WithLabelValues("queued").Inc()

	return nil
}

// Run processes the queue until ctx ends: once a second and, when the
// queue can be watched, whenever a writer notifies it. Only the holder
// of the lease watches (stage B18b, F5): an enqueue wakes one process,
// not every server. A watch that breaks (the OSD dropped it, the store
// rotated its key) is set up again at the next poll. At the end the
// running jobs get the shutdown grace, then they are cut off; the lease
// is released when Run returns.
func (w *LearningWorker) Run(ctx context.Context) {
	defer w.Release()
	defer w.drain()

	t := time.NewTicker(time.Second)
	defer t.Stop()

	var (
		events  <-chan struct{}
		errs    <-chan error
		unwatch = func() {}
	)

	defer func() { unwatch() }()

	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-events:
			learningWakeups.Inc()
		case <-errs:
			unwatch()

			events, errs, unwatch = nil, nil, func() {}
		}

		lease := w.holdLease()

		switch {
		case lease != nil && events == nil:
			events, errs, unwatch = w.watch()
		case lease == nil && events != nil:
			unwatch()

			events, errs, unwatch = nil, nil, func() {}
		}

		if lease != nil {
			w.step(ctx, lease)
		}
	}
}

// drain is the end of Run: the running jobs get the shutdown grace,
// then they are cut off (the next holder runs them again).
func (w *LearningWorker) drain() {
	done := make(chan struct{})

	go func() {
		w.wg.Wait()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(w.shutdownGrace()):
		w.cancelJobs()
		<-done
	}
}

// watch watches the queue; nil channels without a watch.
func (w *LearningWorker) watch() (<-chan struct{}, <-chan error, func()) {
	wq, ok := w.Queue.(Watcher)
	if !ok {
		return nil, nil, func() {}
	}

	wt, err := wq.Watch()
	if err != nil {
		return nil, nil, func() {}
	}

	return wt.Events(), wt.Errors(), wt.Close
}

// holdLease returns the worker lease, taking it when this worker does
// not hold it (nil: another worker holds it). A lease reported lost is
// released and taken again at once; a worker that did not get the lease
// tries again after the retry interval (lockRetry), not on every pass.
func (w *LearningWorker) holdLease() model.Lease {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.lease != nil && w.lease.Lost() {
		w.lease.Release()
		w.lease = nil
		w.nextTry = time.Time{}
	}

	if w.lease == nil {
		now := w.now()
		if now.Before(w.nextTry) {
			return nil
		}

		l, err := w.Queue.Lock()
		if err != nil {
			learningLockAttempts.WithLabelValues("busy").Inc()

			w.nextTry = now.Add(w.lockRetry())

			return nil
		}

		learningLockAttempts.WithLabelValues("taken").Inc()

		w.lease = l
	}

	return w.lease
}

func (w *LearningWorker) now() time.Time {
	if w.Now != nil {
		return w.Now()
	}

	return time.Now()
}

// lockRetry is the wait before the next attempt to take the lease:
// LockRetrySeconds (DefaultLockRetry when 0) times a jitter of ±50 %.
func (w *LearningWorker) lockRetry() time.Duration {
	base := time.Duration(w.Config.LockRetrySeconds) * time.Second
	if base <= 0 {
		base = DefaultLockRetry
	}

	j := 0.5 + rand.Float64() //nolint:gosec // jitter, not a secret
	if w.Jitter != nil {
		j = w.Jitter()
	}

	return time.Duration(float64(base) * j)
}

// Release gives the worker lease up (shutdown).
func (w *LearningWorker) Release() {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.lease != nil {
		w.lease.Release()
		w.lease = nil
	}
}

// Step processes due jobs once while holding the queue lease; it takes
// the lease when this worker does not hold it and keeps it. With a
// concurrency above 1 it returns once the jobs are handed to runners.
func (w *LearningWorker) Step(ctx context.Context) {
	if lease := w.holdLease(); lease != nil {
		w.step(ctx, lease)
	}
}

// step is one pass under the lease.
func (w *LearningWorker) step(ctx context.Context, lease model.Lease) {
	jobs, err := w.Queue.List()
	if err != nil {
		learningResults.WithLabelValues("queue_error").Inc()
		return
	}

	w.gauges(jobs)

	for _, j := range jobs {
		if ctx.Err() != nil || lease.Lost() {
			return
		}

		if j.Dead || time.Now().Before(j.Next) {
			continue
		}

		if w.slots == nil {
			if !w.stepJob(ctx, lease, j) {
				return
			}

			continue
		}

		if !w.dispatch(ctx, lease, j) {
			return
		}
	}
}

// gauges publishes what a pass found due: the count and the age of the
// oldest.
func (w *LearningWorker) gauges(jobs []Job) {
	now := time.Now()
	due, lag := 0, time.Duration(0)

	for _, j := range jobs {
		if j.Dead || now.Before(j.Next) {
			continue
		}

		due++

		if age := now.Sub(j.Next); age > lag {
			lag = age
		}
	}

	learningDue.Set(float64(due))
	learningLag.Set(lag.Seconds())
}

// dispatch hands a job to a runner: not when it is in flight already,
// not when another job of its owner runs (tried without waiting; the
// next pass offers it again, in due order), and only once a slot is
// free (waiting for one bounds the pass, which has nothing to gain by
// listing more). It reports whether the pass goes on.
func (w *LearningWorker) dispatch(ctx context.Context, lease model.Lease, j Job) bool {
	if !w.admit(j) {
		return true
	}

	select {
	case w.slots <- struct{}{}:
	case <-ctx.Done():
		w.done(j)

		return false
	}

	w.wg.Add(1)

	go func() {
		defer w.wg.Done()
		defer func() { <-w.slots }()
		defer w.done(j)

		w.stepJob(w.jobCtx, lease, j)
	}()

	return true
}

// admit takes the job into the in-flight set and its owner's semaphore
// (count 1), or reports that it cannot run now.
func (w *LearningWorker) admit(j Job) bool {
	w.mu.Lock()
	defer w.mu.Unlock()

	if _, running := w.inflight[j.ID]; running || w.owners[j.Owner] > 0 {
		return false
	}

	w.inflight[j.ID] = struct{}{}
	w.owners[j.Owner]++

	return true
}

// done takes the job out of the in-flight set and its owner's semaphore
// (removed at 0, so the map does not grow with the accounts).
func (w *LearningWorker) done(j Job) {
	w.mu.Lock()
	defer w.mu.Unlock()

	delete(w.inflight, j.ID)

	if w.owners[j.Owner]--; w.owners[j.Owner] <= 0 {
		delete(w.owners, j.Owner)
	}
}

// Running counts the jobs in flight and the owners with one (tests).
func (w *LearningWorker) Running() (jobs, owners int) {
	w.mu.Lock()
	defer w.mu.Unlock()

	return len(w.inflight), len(w.owners)
}

// stepJob processes one job; it returns false when the pass must stop
// (lease lost or ctx ended during delivery). A job that ran under a
// cut-off context is left in the queue, as after a lost lease: the next
// holder runs it again (at-least-once, as before).
func (w *LearningWorker) stepJob(ctx context.Context, lease model.Lease, j Job) bool {
	if j.Dead || time.Now().Before(j.Next) {
		return true
	}

	if j.Delivered {
		w.removeDelivered(j)
		return true
	}

	if j.Prepared && !w.preparedReady(j) {
		return true
	}

	if w.markOversizeDead(j) {
		return true
	}

	start := time.Now()

	tried, err := w.attempt(ctx, j)
	if !tried {
		w.observe(start, resultDeferred)

		return true
	}

	switch {
	case lease.Lost():
		w.observe(start, resultLost)

		return false
	case err == nil:
		w.completeJob(j)
		w.observe(start, resultDelivered)

		return true
	case ctx.Err() != nil:
		w.observe(start, resultCanceled)

		return false
	}

	w.observe(start, w.failJob(j, err))

	return true
}

// observe records the duration of a job by result.
func (w *LearningWorker) observe(start time.Time, result string) {
	learningJobSeconds.WithLabelValues(result).Observe(time.Since(start).Seconds())
}

// removeDelivered finishes a job whose program already succeeded.
func (w *LearningWorker) removeDelivered(j Job) {
	if err := w.Queue.Cleanup(j); err == nil {
		if err = w.Queue.Delete(j); err != nil {
			w.deferJob(j)
		}
	} else {
		w.deferJob(j)
	}
}

// preparedReady reports whether a prepared job has its commit receipt.
// Otherwise it reschedules the job and marks it dead after a day.
func (w *LearningWorker) preparedReady(j Job) bool {
	ready, rerr := w.Queue.Ready(j)
	if rerr != nil {
		learningResults.WithLabelValues("queue_error").Inc()
		w.deferJob(j)

		return false
	}

	if ready {
		return true
	}

	j.Next = time.Now().Add(5 * time.Second)
	if time.Since(j.Created) > 24*time.Hour {
		j.Dead = true
		_ = w.Queue.Update(j)

		learningResults.WithLabelValues("unconfirmed").Inc()
	}

	if err := w.Queue.Update(j); err != nil {
		learningResults.WithLabelValues("queue_error").Inc()
	}

	return false
}

// markOversizeDead marks a job with an invalid or too large size dead
// and reports whether it did.
func (w *LearningWorker) markOversizeDead(j Job) bool {
	if j.Size >= 0 && j.Size <= w.Config.MaxMessageBytes {
		return false
	}

	j.Dead = true
	if err := w.Queue.Update(j); err != nil {
		learningResults.WithLabelValues("queue_error").Inc()
	}

	return true
}

// attempt reads the job's message within the memory budget and sends
// it. tried is false when no memory was available (the job is deferred).
func (w *LearningWorker) attempt(ctx context.Context, j Job) (bool, error) {
	release, ok, err := membudget.TryAcquire(j.Size)
	if err != nil || !ok {
		w.deferJob(j)
		return false, nil
	}

	raw, err := w.Queue.Read(j)
	if err == nil {
		err = w.send(ctx, j, raw)
	}

	release()

	return true, err
}

// completeJob records a successful delivery and removes the job.
func (w *LearningWorker) completeJob(j Job) {
	j.Delivered = true

	err := w.Queue.Update(j)
	if err == nil {
		err = w.Queue.Cleanup(j)
	}

	if err == nil {
		err = w.Queue.Delete(j)
	}

	if err != nil {
		learningResults.WithLabelValues("queue_error").Inc()
	} else {
		learningResults.WithLabelValues("delivered").Inc()
	}
}

// failJob schedules a retry with exponential backoff or marks the job
// dead (attempts exhausted or a permanent error); it reports which.
func (w *LearningWorker) failJob(j Job, err error) string {
	j.Attempts++

	j.Dead = j.Attempts >= int(w.Config.MaxAttempts)
	if _, permanent := err.(errPermanent); permanent {
		j.Dead = true
	}

	j.Next = time.Now().Add(time.Duration(1<<min(j.Attempts, 10)) * time.Second)

	switch err = w.Queue.Update(j); {
	case err != nil:
		learningResults.WithLabelValues("queue_error").Inc()
	case j.Dead:
		learningResults.WithLabelValues(resultDead).Inc()

		return resultDead
	default:
		learningResults.WithLabelValues(resultRetry).Inc()
	}

	return resultRetry
}

// send hands the message to the rule's program. Exit 0 is done, 75
// (EX_TEMPFAIL) asks for a retry, everything else fails the job.
func (w *LearningWorker) send(ctx context.Context, j Job, raw []byte) error {
	if j.Action == "" {
		return errPermanent{fmt.Errorf("learning job without program")}
	}

	if w.Programs == nil {
		return fmt.Errorf("no learning programs")
	}

	res, err := w.Programs.Run(ctx, "learning", j.Action, nil, raw, extprograms.Context{Account: j.Owner, Mailbox: j.MailboxID, Cause: "LEARNING"})
	if err != nil {
		if errors.Is(err, extprograms.ErrNotAllowed) {
			return errPermanent{err}
		}

		return err
	}

	switch res.ExitCode {
	case 0:
		return nil
	case extprograms.ExitTempFail:
		return fmt.Errorf("learning program asks for a retry")
	default:
		return errPermanent{fmt.Errorf("learning program exit %d", res.ExitCode)}
	}
}

// errPermanent marks a failure that a retry cannot fix.
type errPermanent struct{ error }

// Prepare persists the payload before the destination entry is committed. The
// Ceph link writes a receipt atomically with that entry; workers only send jobs
// with such a receipt. A crash after commit therefore cannot lose the event.
func (w *LearningWorker) Prepare(e Event, raw []byte) (string, error) {
	if int64(len(raw)) > w.Config.MaxMessageBytes {
		return "", fmt.Errorf("learning message too large")
	}

	id := model.NewGUID()

	j := Job{ID: id, Action: wAction(e), Size: int64(len(raw)), Next: time.Now(), Owner: e.Owner, MailboxID: e.MailboxID, Prepared: true, Created: time.Now()}
	if err := w.Queue.Put(j, raw); err != nil {
		return "", err
	}

	learningResults.WithLabelValues("prepared").Inc()

	return id, nil
}

// The action was selected by the trusted engine, never by message headers.
func wAction(e Event) string { return e.LearningAction }

// LearningOversize counts a learning message refused for its size.
func LearningOversize() { learningResults.WithLabelValues("oversize").Inc() }

func (w *LearningWorker) deferJob(j Job) {
	j.Next = time.Now().Add(5 * time.Second)
	if err := w.Queue.Update(j); err != nil {
		learningResults.WithLabelValues("queue_error").Inc()
	}
}
