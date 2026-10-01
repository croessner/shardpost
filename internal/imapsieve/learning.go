package imapsieve

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"shardpost/internal/config"
	"shardpost/internal/extprograms"
	"shardpost/internal/membudget"
	"shardpost/internal/model"
)

var learningResults = promauto.NewCounterVec(prometheus.CounterOpts{Name: "imap_imapsieve_learning_total", Help: "Learning queue and delivery outcomes."}, []string{"result"})
var scriptResults = promauto.NewCounterVec(prometheus.CounterOpts{Name: "imap_imapsieve_events_total", Help: "IMAPSieve outcomes by bounded phase."}, []string{phaseName})

// learningWakeups counts the worker passes a queue notify started (the
// once-a-second poll is not counted).
var learningWakeups = promauto.NewCounter(prometheus.CounterOpts{Name: "imap_imapsieve_learning_wakeups_total",
	Help: "Learning worker passes started by a queue notify."})

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
type LearningWorker struct {
	Config   config.Learning
	Queue    Queue
	Programs Programs

	mu    sync.Mutex
	lease model.Lease
}

// NewLearningWorker builds a worker for queue q.
func NewLearningWorker(c config.Learning, q Queue, programs Programs) *LearningWorker {
	return &LearningWorker{Config: c, Queue: q, Programs: programs}
}

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
// queue can be watched, whenever a writer notifies it. A watch that
// breaks (the OSD dropped it, the store rotated its key) is set up again
// at the next poll. The lease is released when Run returns.
func (w *LearningWorker) Run(ctx context.Context) {
	defer w.Release()

	t := time.NewTicker(time.Second)
	defer t.Stop()

	events, errs, unwatch := w.watch()
	defer func() { unwatch() }()

	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if events == nil {
				events, errs, unwatch = w.watch()
			}
		case <-events:
			learningWakeups.Inc()
		case <-errs:
			unwatch()

			events, errs, unwatch = nil, nil, func() {}
		}

		w.Step(ctx)
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
// released and taken again.
func (w *LearningWorker) holdLease() model.Lease {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.lease != nil && w.lease.Lost() {
		w.lease.Release()
		w.lease = nil
	}

	if w.lease == nil {
		l, err := w.Queue.Lock()
		if err != nil {
			return nil
		}

		w.lease = l
	}

	return w.lease
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
// the lease when this worker does not hold it and keeps it.
func (w *LearningWorker) Step(ctx context.Context) {
	lease := w.holdLease()
	if lease == nil {
		return
	}

	jobs, err := w.Queue.List()
	if err != nil {
		learningResults.WithLabelValues("queue_error").Inc()
		return
	}

	for _, j := range jobs {
		if ctx.Err() != nil || lease.Lost() {
			return
		}

		if !w.stepJob(ctx, lease, j) {
			return
		}
	}
}

// stepJob processes one job; it returns false when the pass must stop
// (lease lost or ctx ended during delivery).
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

	tried, err := w.attempt(ctx, j)
	if !tried {
		return true
	}

	if lease.Lost() {
		return false
	}

	if err == nil {
		w.completeJob(j)
		return true
	}

	if ctx.Err() != nil {
		return false
	}

	w.failJob(j, err)

	return true
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
// dead (attempts exhausted or a permanent error).
func (w *LearningWorker) failJob(j Job, err error) {
	j.Attempts++

	j.Dead = j.Attempts >= int(w.Config.MaxAttempts)
	if _, permanent := err.(errPermanent); permanent {
		j.Dead = true
	}

	j.Next = time.Now().Add(time.Duration(1<<min(j.Attempts, 10)) * time.Second)
	if err = w.Queue.Update(j); err != nil {
		learningResults.WithLabelValues("queue_error").Inc()
	} else if j.Dead {
		learningResults.WithLabelValues("dead").Inc()
	} else {
		learningResults.WithLabelValues("retry").Inc()
	}
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
