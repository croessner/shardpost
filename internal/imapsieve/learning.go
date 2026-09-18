package imapsieve

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
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
}

// Queue is the persistent learning job queue with a worker lease.
type Queue interface {
	Put(Job, []byte) error
	List() ([]Job, error)
	Read(Job) ([]byte, error)
	Update(Job) error
	Delete(Job) error
	Lock() (model.Lease, error)
	Ready(Job) (bool, error)
	Cleanup(Job) error
}

// Programs is what the worker needs from the allowlist runner.
type Programs interface {
	Run(ctx context.Context, kind, name string, scriptArgs []string, input []byte, c extprograms.Context) (extprograms.Result, error)
}

// LearningWorker sends queued learning jobs to their programs, with
// retries and dead-lettering.
type LearningWorker struct {
	Config   config.Learning
	Queue    Queue
	Programs Programs
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

// Run processes the queue once a second until ctx ends.
func (w *LearningWorker) Run(ctx context.Context) {
	t := time.NewTicker(time.Second)
	defer t.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			w.Step(ctx)
		}
	}
}

// Step processes due jobs once while holding the queue lease.
func (w *LearningWorker) Step(ctx context.Context) {
	lease, err := w.Queue.Lock()
	if err != nil {
		return
	}
	defer lease.Release()

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
