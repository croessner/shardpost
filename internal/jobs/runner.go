package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/otel/attribute"

	"shardpost/internal/metrics"
	"shardpost/internal/model"
	"shardpost/internal/telemetry"
)

// Run is the execution context of one claimed job.
type Run struct {
	Job  *Job // snapshot at claim time (ID, kind, account, params, checkpoint)
	User model.User
	Plan *Plan // the applied plan, nil for unplanned kinds
	Env  *Env

	m      *Manager
	gen    uint64
	mu     sync.Mutex
	errors []string
	// lockDropped tolerates a failing renewal of the maintenance lock
	// (purge removes it with the account's objects, the registry last).
	lockDropped atomic.Bool
	// stopReason explains why the heartbeat stopped the job.
	stopReason atomic.Value
	// sub marks the run of one account inside a batch (subRun): the
	// checkpoint and the progress belong to the batch, the Partial
	// errors stay with the account.
	sub bool
	// window: an online run of this run is admitted only when it fits
	// into the reshard window (a batch without "anytime").
	window bool
}

// subRun is the run of one account inside a batch: the kind and its
// parameters, fenced on the batch's lease.
func (r *Run) subRun(account string, u model.User, kind string, params json.RawMessage) *Run {
	j := *r.Job
	j.Account, j.Kind, j.Params, j.Checkpoint, j.PlanID = account, kind, params, nil, ""

	return &Run{Job: &j, User: u, Env: r.Env, m: r.m, gen: r.gen, sub: true}
}

// partialErrors returns the parts recorded as failed.
func (r *Run) partialErrors() []string {
	r.mu.Lock()
	defer r.mu.Unlock()

	return append([]string(nil), r.errors...)
}

// DropLock stops renewing the maintenance lock: the job removes it itself
// (purge deletes the registry that holds it).
func (r *Run) DropLock() { r.lockDropped.Store(true) }

// Fence checks that this server still holds the job: call it before
// every mutation. It returns ErrLeaseLost after a takeover or expiry and
// context.Canceled when the job was asked to stop.
func (r *Run) Fence() error {
	j, err := r.m.st.getJob(r.Job.ID)
	if err != nil {
		return err
	}

	switch {
	case j.Lease.Owner != r.m.instance || j.Lease.Gen != r.gen || !r.m.now().Before(j.Lease.Until):
		return ErrLeaseLost
	case j.State == StateCancelRequested:
		return context.Canceled
	default:
		return nil
	}
}

// guarded updates the job only while this server holds it.
func (r *Run) guarded(fn func(j *Job)) error {
	_, err := r.m.updateJob(r.Job.ID, nil, func(j *Job) error {
		if j.Lease.Owner != r.m.instance || j.Lease.Gen != r.gen {
			return ErrLeaseLost
		}

		fn(j)

		return nil
	})

	return err
}

// Checkpoint stores the resume state of the job (read it back from
// Job.Checkpoint after a takeover).
func (r *Run) Checkpoint(v any) error {
	if r.sub {
		return nil
	}

	b, err := json.Marshal(v)
	if err != nil {
		return err
	}

	return r.guarded(func(j *Job) { j.Checkpoint = b })
}

// Progress records the visible progress.
func (r *Run) Progress(phase string, done, total int64) error {
	if r.sub {
		return nil
	}

	return r.guarded(func(j *Job) { j.Progress = Progress{Phase: phase, Done: done, Total: total} })
}

// Partial records a failed part; the job ends partially_failed.
func (r *Run) Partial(format string, a ...any) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.errors = append(r.errors, fmt.Sprintf(format, a...))
}

// LoadCheckpoint decodes the stored checkpoint into v; false when none.
func (r *Run) LoadCheckpoint(v any) bool {
	return len(r.Job.Checkpoint) > 0 && json.Unmarshal(r.Job.Checkpoint, v) == nil
}

// Run claims and executes jobs until ctx ends. It looks for claimable
// jobs every Config.Poll and, in read mode, whenever a writer notifies
// the claimable queue (a job was submitted or handed over); a watch that
// reports an error (dropped by the OSD, or the store rotated its
// connection) is set up again at the next poll. With the queues it also
// runs the expiry worker when this server holds its lease. In write and
// off mode cleanup() (a listing) runs on the poll beside it; in read mode
// the expiry worker alone deletes what is due.
func (m *Manager) Run(ctx context.Context) {
	sem := make(chan struct{}, m.cfg.MaxParallel)

	var wg sync.WaitGroup

	defer wg.Wait()

	if m.q != nil {
		wg.Add(1)

		go func() {
			defer wg.Done()

			m.runExpiry(ctx)
		}()
	}

	if m.env.FTSDue != nil {
		wg.Add(1)

		go func() {
			defer wg.Done()

			m.runFTSDue(ctx)
		}()
	}

	tick := time.NewTicker(m.cfg.Poll)
	defer tick.Stop()

	events, errs, unwatch := m.watchClaimable()
	defer func() { unwatch() }()

	cause := wakePoll

	for {
		metrics.JobWakeups.WithLabelValues(cause).Inc()
		m.pollOnce(ctx, sem, &wg)

		if cause == wakePoll && !m.q.reads() {
			m.cleanup()
		}

		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			cause = wakePoll

			if events == nil && m.q.reads() {
				events, errs, unwatch = m.watchClaimable()
			}
		case <-events:
			cause = wakeNotify
		case err := <-errs:
			// The watch broke (the OSD dropped it): watch again on the
			// next poll, which looks for jobs meanwhile.
			m.env.Log.Warn("jobs: claimable queue watch lost", "err", err)
			unwatch()

			events, errs, unwatch, cause = nil, nil, func() {}, wakePoll
		}
	}
}

// Causes of a look for claimable jobs (label "cause" of
// imapceph_job_wakeups_total).
const (
	wakePoll   = "poll"
	wakeNotify = "notify"
)

// pollOnce claims every claimable job there is room for: from the
// claimable queue in read mode, else from a listing of the jobs.
func (m *Manager) pollOnce(ctx context.Context, sem chan struct{}, wg *sync.WaitGroup) {
	if m.q.reads() {
		m.pollDue(ctx, sem, wg)
		return
	}

	after := ""

	for {
		page, err := m.st.listJobs(after, 100)
		if err != nil {
			m.env.Log.Warn("jobs: list", "err", err)
			return
		}

		for i := range page {
			after = page[i].ID

			if !page[i].claimable(m.now()) {
				continue
			}

			select {
			case sem <- struct{}{}:
			default:
				return // busy; later polls continue
			}

			j, gen, err := m.claim(page[i].ID, nil)
			if err != nil {
				<-sem
				continue // another server was faster
			}

			wg.Add(1)

			go func() {
				defer wg.Done()
				defer func() { <-sem }()

				m.execute(ctx, j, gen)
			}()
		}

		if len(page) < 100 {
			return
		}
	}
}

// claim takes a claimable job over with a new lease generation.
func (m *Manager) claim(id string, extra []model.DueKey) (*Job, uint64, error) {
	var gen uint64

	j, err := m.updateJob(id, extra, func(j *Job) error {
		now := m.now()
		if !j.claimable(now) {
			return errors.New("not claimable")
		}

		metrics.JobClaimWait.Observe(now.Sub(j.claimableSince()).Seconds())

		if j.State == StateQueued {
			j.State, j.Started = StateRunning, now
		}

		j.Attempts++
		j.Lease = Lease{Owner: m.instance, Until: now.Add(m.cfg.LeaseTTL), Gen: j.Lease.Gen + 1}
		gen = j.Lease.Gen

		return nil
	})

	return j, gen, err
}

// execute runs one claimed job with heartbeat, maintenance lock and the
// final state.
func (m *Manager) execute(parent context.Context, j *Job, gen uint64) {
	log := m.env.Log.With("job", j.ID, "kind", j.Kind, "account", j.Account)

	metrics.JobRunning.WithLabelValues(j.Kind).Inc()
	defer metrics.JobRunning.WithLabelValues(j.Kind).Dec()

	ctx, cancel := context.WithCancel(parent)
	defer cancel()

	run, err := m.prepare(j, gen)
	if err != nil {
		m.finish(run, j, gen, nil, err, log)
		return
	}

	if run.User != nil {
		defer run.User.Close()
	}

	kind := m.kinds[j.Kind]

	// One trace per job run: the store records its operations below it,
	// instead of producing a root span per object it touches.
	ctx, span := telemetry.Span(ctx, "job."+j.Kind,
		append(telemetry.Personal(attribute.String("job.account", j.Account)), attribute.String("job.id", j.ID))...)
	defer span.End()

	model.UseRequest(ctx, run.User)
	defer model.UseRequest(context.Background(), run.User)

	excl := exclusive(kind, j.Params)

	if excl && !m.lock(run, j, gen, log) {
		return
	}

	stop := m.heartbeat(ctx, cancel, run, excl, log)

	log.Info("job started", "event", "job_start", "attempt", j.Attempts, "exclusive", excl)

	result, err := m.settleAndRun(ctx, kind, run, excl)

	stop()

	if reason, ok := run.stopReason.Load().(string); ok && err != nil && parent.Err() == nil {
		err = fmt.Errorf("%s: %w", reason, err)
	}

	m.conclude(parent, run, excl, result, err, log)
}

// conclude ends a run: hands the job over at shutdown, gives a waiting
// batch back, or stores the final state.
func (m *Manager) conclude(parent context.Context, run *Run, excl bool, result any, err error, log *slog.Logger) {
	// Shutdown interrupted the job: hand it over (another server resumes
	// it from its checkpoint at once) instead of failing it. The
	// maintenance lock stays; the successor takes it over (same session).
	if err != nil && parent.Err() != nil {
		m.release(run, log)
		return
	}

	// The batch waits for the reshard window: it is given back until then.
	var later *PostponeError
	if errors.As(err, &later) {
		m.postponeUntil(run, later, log)
		return
	}

	// The lock is released only by the server that stored the final
	// state: after a takeover it belongs to the successor. Before, a
	// conversion the run left unfinished goes (the fallback of the kind's
	// own clean-up, also after a panic and for an online run without the
	// lock, R2.5; single-account jobs only, a batch clears per account).
	if m.finish(run, run.Job, run.gen, result, err, log) {
		m.clearFinished(run, log)

		if excl {
			unlockAccount(run.User, run.Job.ID)
		}
	}
}

// PostponeError gives a job back until Until without counting the
// attempt (a batch outside the reshard window); its checkpoint is
// stored.
type PostponeError struct {
	Until  time.Time
	Reason string
}

func (e *PostponeError) Error() string {
	return fmt.Sprintf("postponed until %s: %s", e.Until.UTC().Format(time.RFC3339), e.Reason)
}

// postponeUntil gives the job back until p.Until; the attempt does not
// count.
func (m *Manager) postponeUntil(run *Run, p *PostponeError, log interface{ Info(string, ...any) }) {
	err := run.guarded(func(j *Job) {
		j.Attempts--
		j.Lease.Until = p.Until
		j.Progress.Phase = p.Reason
	})
	if err == nil {
		log.Info("job waits", "event", "job_postpone", "until", p.Until, "reason", p.Reason)
	}
}

// buildsLayout reports whether a job of the kind may leave a full-text
// conversion behind ("next"): fts_layout, and fts_rebuild since its
// online runs (stage B14i).
func buildsLayout(kind string) bool { return kind == KindFTSLayout || kind == KindFTSRebuild }

// clearFinished removes the unfinished full-text conversion of a run
// whose final state is stored (exactly its token).
func (m *Manager) clearFinished(run *Run, log interface{ Warn(string, ...any) }) {
	if !buildsLayout(run.Job.Kind) || run.User == nil {
		return
	}

	if lay, ok := run.User.(model.FTSLayouter); ok {
		if _, err := lay.ClearFTSLayoutNext(run.layoutToken()); err != nil {
			log.Warn("fts layout: unfinished conversion not cleared", "err", err)
		}
	}
}

// settleAndRun lets writers that opened the account before an exclusive
// job's lock finish (Config.Settle), then runs the kind.
func (m *Manager) settleAndRun(ctx context.Context, kind Kind, run *Run, excl bool) (any, error) {
	if excl && m.cfg.Settle > 0 {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(m.cfg.Settle):
		}
	}

	return runKind(ctx, kind, run)
}

// runKind runs the kind and turns a panic into an error, so a broken
// job fails instead of taking every server down in turn.
func runKind(ctx context.Context, kind Kind, run *Run) (result any, err error) {
	defer func() {
		if p := recover(); p != nil {
			result, err = nil, fmt.Errorf("job panicked: %v", p)
		}
	}()

	return kind.Run(ctx, run)
}

// release ends this server's lease so another server can take over now.
// lock takes the maintenance lock of an exclusive job; false means the
// job was postponed or finished.
func (m *Manager) lock(run *Run, j *Job, gen uint64, log *slog.Logger) bool {
	err := lockAccount(run.User, j.ID, 2*m.cfg.LeaseTTL)

	switch {
	case err == nil:
		clearForeignLayout(run.User, run.layoutToken(), log)
		return true
	case j.State == StateCancelRequested:
		err = context.Canceled
	case errors.Is(err, model.ErrAccountPurging) && m.leftPurging(run.User, j.ID):
		err = ErrLeftPurging
	case errors.Is(err, model.ErrImportLocked) && m.now().Sub(j.Created) < m.cfg.PlanTTL:
		m.postpone(run, log)
		return false
	default:
		err = fmt.Errorf("maintenance lock: %w", err)
	}

	m.finish(run, j, gen, nil, err, log)

	return false
}

// postpone gives a job back whose account is locked by an import or
// another job: it is tried again after a lease period, the attempt does
// not count (up to jobs.plan_ttl_seconds after the job was created).
func (m *Manager) postpone(run *Run, log interface{ Info(string, ...any) }) {
	err := run.guarded(func(j *Job) {
		j.Attempts--
		j.Lease.Until = m.now().Add(m.cfg.LeaseTTL)
		j.Progress.Phase = "waiting for the account lock"
	})
	if err == nil {
		log.Info("job waits for the account lock", "event", "job_postpone")
	}
}

func (m *Manager) release(run *Run, log interface{ Info(string, ...any) }) {
	if err := run.guarded(func(j *Job) { j.Lease.Until = m.now() }); err == nil {
		log.Info("job handed over at shutdown", "event", "job_release")
	}
}

// prepare opens the account and loads the plan.
func (m *Manager) prepare(j *Job, gen uint64) (*Run, error) {
	run := &Run{Job: j, Env: m.env, m: m, gen: gen}

	if _, ok := m.kinds[j.Kind]; !ok {
		return run, ErrUnknownKind
	}

	if j.PlanID != "" {
		p, err := m.st.getPlan(j.PlanID)
		if err != nil {
			return run, fmt.Errorf("plan %s: %w", j.PlanID, err)
		}

		run.Plan = p
	}

	// A batch opens its accounts one by one; there is no account to
	// open for it (and the empty name is no account).
	if j.Account == "" {
		if !IsBatch(m.kinds[j.Kind]) {
			return run, fmt.Errorf("%w: %s needs an account", ErrInvalidParams, j.Kind)
		}

		return run, nil
	}

	u, err := m.env.Accounts.User(j.Account)
	if err != nil {
		return run, err
	}

	run.User = u

	return run, nil
}

// heartbeat renews the lease (and the maintenance lock) every third of
// its TTL; a lost lease or a cancel request cancels the job's context.
func (m *Manager) heartbeat(ctx context.Context, cancel context.CancelFunc, run *Run, exclusive bool, log interface {
	Warn(string, ...any)
},
) func() {
	done := make(chan struct{})
	tick := time.NewTicker(m.cfg.LeaseTTL / 3)

	go func() {
		defer tick.Stop()

		for {
			select {
			case <-done:
				return
			case <-ctx.Done():
				return
			case <-tick.C:
			}

			if !m.renew(run, exclusive, log) {
				cancel()
				return
			}
		}
	}()

	return func() { close(done) }
}

// renew extends the lease; false when the job is lost or asked to stop.
func (m *Manager) renew(run *Run, exclusive bool, log interface{ Warn(string, ...any) }) bool {
	var stop bool

	err := run.guarded(func(j *Job) {
		j.Lease.Until = m.now().Add(m.cfg.LeaseTTL)
		stop = j.State == StateCancelRequested
	})
	if err != nil {
		log.Warn("jobs: lease lost", "err", err)
		return false
	}

	// Purge keeps the lock fresh until it deletes the registry; a failing
	// renewal after that is expected.
	if exclusive {
		if err := renewAccount(run.User, run.Job.ID, 2*m.cfg.LeaseTTL); err != nil && !run.lockDropped.Load() {
			log.Warn("jobs: maintenance lock lost", "err", err)
			run.stopReason.Store("maintenance lock lost")

			return false
		}
	}

	return !stop
}

// finish stores the final state unless the job was taken over; it
// reports whether it did.
func (m *Manager) finish(run *Run, j *Job, gen uint64, result any, runErr error, log interface {
	Info(string, ...any)
	Warn(string, ...any)
},
) bool {
	if errors.Is(runErr, ErrLeaseLost) {
		log.Warn("job lease lost, another server continues", "err", runErr)
		return false
	}

	var resB json.RawMessage
	if result != nil {
		resB, _ = json.Marshal(result)
	}

	var partial []string

	if run != nil {
		run.mu.Lock()
		partial = append(partial, run.errors...)
		run.mu.Unlock()
	}

	_, err := m.updateJob(j.ID, nil, func(cur *Job) error {
		if cur.Lease.Owner != m.instance || cur.Lease.Gen != gen {
			return ErrLeaseLost
		}

		cur.State, cur.Finished, cur.Result = finalState(runErr, partial, cur.State), m.now(), resB
		cur.Errors = append(partial, errorText(runErr)...)
		cur.Lease.Until = time.Time{}

		return nil
	})
	if err != nil {
		log.Warn("jobs: final state not stored", "err", err)
		return false
	}

	log.Info("job finished", "event", "job_finish", "error", runErr != nil, "partial", len(partial))

	return true
}

func finalState(runErr error, partial []string, cur string) string {
	switch {
	case errors.Is(runErr, context.Canceled) && cur == StateCancelRequested:
		return StateCancelled
	case runErr != nil:
		return StateFailed
	case len(partial) > 0:
		return StatePartiallyFailed
	default:
		return StateSucceeded
	}
}

func errorText(err error) []string {
	if err == nil {
		return nil
	}

	return []string{err.Error()}
}

// cleanup removes finished jobs and expired plans after the retention.
func (m *Manager) cleanup() {
	cutoff := m.now().Add(-m.cfg.Retention)

	jobs, err := m.st.listJobs("", 100)
	if err != nil {
		return
	}

	for i := range jobs {
		if jobs[i].Terminal() && jobs[i].Finished.Before(cutoff) {
			_ = m.st.kv.Update(prefixJob+jobs[i].ID, func([]byte) ([]byte, error) { return nil, nil })
		}
	}

	plans, err := m.st.kv.List(prefixPlan, "", 100)
	if err != nil {
		return
	}

	for _, rec := range plans {
		var p Plan
		if json.Unmarshal(rec.Value, &p) == nil && p.Expires.Before(cutoff) {
			_ = m.st.kv.Update(rec.Key, func([]byte) ([]byte, error) { return nil, nil })
		}
	}
}

// Maintenance lock: the account import lock under the job's session, so
// protocols refuse the account temporarily and imports, other exclusive
// jobs and the scrubber keep out.
func lockAccount(u model.User, jobID string, ttl time.Duration) error {
	imp, ok := u.(model.Importer)
	if !ok {
		return errors.New("store has no account lock")
	}

	_, err := imp.ImportLock(lockSession(jobID), ttl)

	return err
}

func renewAccount(u model.User, jobID string, ttl time.Duration) error {
	imp, ok := u.(model.Importer)
	if !ok {
		return errors.New("store has no account lock")
	}

	_, err := imp.ImportRenew(lockSession(jobID), ttl)

	return err
}

func unlockAccount(u model.User, jobID string) {
	if imp, ok := u.(model.Importer); ok {
		_ = imp.ImportUnlock(lockSession(jobID))
	}
}

// lockSession is the lock session of a job (the import session pattern
// allows [A-Za-z0-9_-]).
func lockSession(jobID string) string {
	b := []byte("job-" + jobID)
	for i, c := range b {
		if (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && (c < '0' || c > '9') && c != '-' && c != '_' {
			b[i] = '_'
		}
	}

	return string(b)
}
