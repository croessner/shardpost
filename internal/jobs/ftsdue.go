package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"shardpost/internal/metrics"
	"shardpost/internal/model"
	"shardpost/internal/window"
)

// The fts_due worker (stage B14b, §4.6): writers queue an account in
// ix.fts_due ("~accounts") when an object of its layout 3 index outgrew
// fts.shard_target_keys; the worker plans and applies fts_layout
// {"due_only": true} for it. The entries belong to the worker alone: it
// removes exactly the entry that fired, after checking its lease, and
// nothing else removes them (a purged account's entry goes when it
// fires). The record "fts_due/<account>" of the admin store remembers
// the job it planned, so the worker plans one job per generation and
// learns how it ended without listing the jobs.

// FTSDue is the reshard queue and its worker lease (Env.FTSDue).
type FTSDue struct {
	Queue model.DueQueue
	Lock  func(ttl time.Duration) (model.Lease, error)
}

// ftsDuePrincipal is the principal of the jobs the worker plans,
// ftsDueUrgentPrincipal of those it plans outside the reshard window
// because the account is urgent.
const (
	ftsDuePrincipal       = "fts_due"
	ftsDueUrgentPrincipal = "fts_due/urgent"
)

// runFTSDue is the worker: it takes the lease when it is free and works
// off what is due every Config.ExpiryPoll.
func (m *Manager) runFTSDue(ctx context.Context) {
	m.leaseLoop(ctx, m.takeFTSDueLease, m.ftsDueOnce)
}

func (m *Manager) takeFTSDueLease() model.Lease {
	l, err := m.env.FTSDue.Lock(expiryLockTTL(m.cfg.LeaseTTL))

	switch {
	case err == nil:
		return l
	case !errors.Is(err, model.ErrLocked):
		m.env.Log.Warn("fts_due: lease", "err", err)
	}

	return nil
}

// ftsDueOnce handles what is due now, a page at a time, until nothing is
// due, the lease is lost or ctx ends.
func (m *Manager) ftsDueOnce(ctx context.Context, lost func() bool) {
	const page = 100

	after := ""

	for range 100 {
		due, err := m.env.FTSDue.Queue.DueAfter(m.now(), after, page)
		if err != nil {
			m.env.Log.Warn("fts_due: due", "err", err)
			return
		}

		for _, e := range due {
			after = e.Key()

			if lost() || ctx.Err() != nil {
				return
			}

			if err := m.ftsDueEntry(ctx, e, lost); err != nil {
				m.env.Log.Warn("fts_due: entry", "account", e.ID, "err", err)
			}
		}

		if len(due) < page {
			return
		}
	}
}

// ftsDueEntry handles one fired entry (§4.6 worker steps 1-6).
func (m *Manager) ftsDueEntry(ctx context.Context, e model.DueEntry, lost func() bool) error {
	gen, ok, err := m.ftsDueCurrent(e)
	if err != nil {
		return err
	}

	if !ok {
		return m.ftsDueMove(e, nil, lost) // purged account, or the generation was replaced
	}

	switch locked, err := m.accountLocked(e.ID); {
	case err != nil:
		return err
	case locked:
		return m.ftsDueLater(e, gen, lost) // a job, an import or another lock holds it
	}

	return m.ftsDueByJob(ctx, e, gen, lost)
}

// ftsDueByJob decides by the job the worker planned for the generation:
// wait for it, forget the entry after a switch, back off after a no-op,
// or plan one.
func (m *Manager) ftsDueByJob(ctx context.Context, e model.DueEntry, gen string, lost func() bool) error {
	rec, j, err := m.ftsDueJob(e.ID, gen)
	if err != nil {
		return err
	}

	noops, idle := noopsOf(rec, gen)

	// An online run that found the account busy (as a locked one) or
	// outside the window: wait once, then plan anew (R2.10).
	if deferred := ftsLayoutDeferred(j); deferred != "" && !idle {
		return m.ftsDueAfterDeferred(ctx, e, gen, rec, deferred, noops, lost)
	}

	switch {
	case idle:
		return m.ftsDuePlan(ctx, e, gen, noops+1, lost) // the wait after the last no-op is over
	case j != nil && !j.Terminal():
		return m.ftsDueLater(e, gen, lost)
	case j != nil && j.State == StateSucceeded && !ftsLayoutNoop(j):
		return m.ftsDueSwitched(e, lost)
	case j != nil && j.State == StateSucceeded:
		return m.ftsDueNoop(e, rec, lost)
	}

	return m.ftsDuePlan(ctx, e, gen, noops, lost) // a failed job keeps the count
}

// ftsDueSwitched forgets the entry and the record of an account whose
// job switched.
func (m *Manager) ftsDueSwitched(e model.DueEntry, lost func() bool) error {
	if err := m.ftsDueMove(e, nil, lost); err != nil {
		return err
	}

	return m.st.kv.Update(model.FTSDueRecordKey(e.ID), func([]byte) ([]byte, error) { return nil, nil })
}

// noopsOf is the number of no-op jobs the record counts for generation
// gen and whether the wait after the last one is over (idle).
func noopsOf(rec *model.FTSDueRecord, gen string) (int, bool) {
	if rec == nil || rec.Gen != gen {
		return 0, false
	}

	return rec.Noops, rec.Idle
}

// accountLocked reports whether the account's import (maintenance) lock
// is held: one read.
func (m *Manager) accountLocked(account string) (bool, error) {
	u, err := m.env.Accounts.User(account)
	if err != nil {
		return false, err
	}
	defer u.Close()

	err = model.RefuseDuringImport(u)
	if errors.Is(err, model.ErrImportLocked) {
		return true, nil
	}

	return false, err
}

// ftsDueCurrent reports whether the entry's account exists and still has
// the generation the entry was written for, and returns it.
func (m *Manager) ftsDueCurrent(e model.DueEntry) (string, bool, error) {
	if m.env.Directory != nil {
		reg, err := m.env.Directory.HasRegistry(e.ID)
		if err != nil || !reg {
			return "", false, err
		}
	}

	u, err := m.env.Accounts.User(e.ID)
	if err != nil {
		return "", false, err
	}
	defer u.Close()

	lay, ok := u.(model.FTSLayouter)
	if !ok {
		return "", false, nil
	}

	st, err := lay.FTSLayout()
	if err != nil {
		return "", false, err
	}

	if st.Current.Layout != model.FTSLayout3 || model.FTSDueGen(st.Current.Gen) != e.Gen {
		return "", false, nil
	}

	return st.Current.Gen, true, nil
}

// ftsDueJob reads the worker's record of the account and the job it
// names, if the record belongs to gen.
func (m *Manager) ftsDueJob(account, gen string) (*model.FTSDueRecord, *Job, error) {
	var rec model.FTSDueRecord

	err := m.st.get(model.FTSDueRecordKey(account), &rec)

	switch {
	case errors.Is(err, ErrNotFound):
		return nil, nil, nil
	case err != nil:
		return nil, nil, err
	case rec.Gen != gen:
		return &rec, nil, nil
	}

	j, err := m.st.getJob(rec.JobID)
	if errors.Is(err, ErrNotFound) {
		return &rec, nil, nil
	}

	return &rec, j, err
}

// ftsLayoutNoop reports whether a finished fts_layout job changed nothing.
func ftsLayoutNoop(j *Job) bool {
	var rep model.FTSLayoutReport

	return json.Unmarshal(j.Result, &rep) == nil && rep.Noop
}

// ftsLayoutDeferred is why a succeeded fts_layout job ended without a
// change other than a no-op ("" for none): busy, window,
// window_too_small.
func ftsLayoutDeferred(j *Job) string {
	if j == nil || j.State != StateSucceeded {
		return ""
	}

	var rep model.FTSLayoutReport
	if json.Unmarshal(j.Result, &rep) != nil {
		return ""
	}

	return rep.Deferred
}

// ftsDueAfterDeferred handles the entry of a job whose online run ended
// without a change: the first time it waits (ftsDueDeferred), then a new
// job is planned.
func (m *Manager) ftsDueAfterDeferred(ctx context.Context, e model.DueEntry, gen string, rec *model.FTSDueRecord, deferred string,
	noops int, lost func() bool,
) error {
	if !rec.Deferred {
		return m.ftsDueDeferred(e, rec, deferred, lost)
	}

	return m.ftsDuePlan(ctx, e, gen, noops, lost)
}

// ftsDueDeferred handles a job whose online run ended without a change:
// busy waits as for a locked account (the backoff), window until the
// next window starts, window_too_small a day. The record notes it, so
// that the entry that fires next plans a new job.
func (m *Manager) ftsDueDeferred(e model.DueEntry, rec *model.FTSDueRecord, deferred string, lost func() bool) error {
	if lost() {
		return nil
	}

	waits := 0

	err := m.st.kv.Update(model.FTSDueRecordKey(e.ID), func(cur []byte) ([]byte, error) {
		var c model.FTSDueRecord
		if cur == nil || json.Unmarshal(cur, &c) != nil || c.JobID != rec.JobID {
			return cur, nil
		}

		c.Deferred = true
		c.Waits++
		waits = c.Waits

		return json.Marshal(c)
	})
	if err != nil {
		return err
	}

	now := m.now()
	due := now.Add(model.FTSDueBackoffAfter(waits))

	switch deferred {
	case model.FTSDeferredWindow:
		due = m.windowStart(e.ID, now)
	case model.FTSDeferredTooSmall:
		due = now.Add(ftsDueTooSmallWait)
	}

	return m.ftsDueMove(e, &model.DueKey{Due: due, Gen: e.Gen}, lost)
}

// windowStart is when the worker looks at an account again that waits
// for the reshard window: the next window's start plus the account's
// spread.
func (m *Manager) windowStart(account string, now time.Time) time.Time {
	w := m.cfg.FTSBuild.Window

	from := w.NextStart(now)
	_, to := w.Bounds(from)

	return from.Add(w.Spread(account, to.Sub(from)))
}

// ftsDueGate is the window step before the worker plans (temp/14
// §4.12): without a window nothing. Outside it only an urgent account
// is planned (its largest object at ftsUrgentLoad times its target, near
// the 2·T at which Ceph reports large omap objects); the others wait for
// the next window. Inside it at most fts.build_max_running jobs the
// worker planned run at once, and a run starts only when its estimate
// fits into what is left of the window; one longer than the whole
// window is not planned (window_too_small, a day later again). It
// returns where to move the entry instead of planning (nil: plan), and
// whether the account is urgent.
func (m *Manager) ftsDueGate(e model.DueEntry) (*model.DueKey, bool, error) {
	b := m.cfg.FTSBuild
	if !b.Window.Set() {
		return nil, false, nil
	}

	shape, err := m.ftsDueShape(e.ID)
	if err != nil {
		return nil, false, err
	}

	now := m.now()

	if !b.Window.In(now) {
		if shape.Load >= ftsUrgentLoad {
			return nil, true, nil
		}

		metrics.FTSBuildWindowDeferred.Inc()

		return &model.DueKey{Due: m.windowStart(e.ID, now), Gen: e.Gen}, false, nil
	}

	if n, err := m.ftsDueRunningCount(); err != nil || n >= b.MaxRunning {
		return &model.DueKey{Due: now.Add(ftsDueBusyWait), Gen: e.Gen}, false, err
	}

	switch err := b.Window.Fit(now, b.estimate(model.FTSModeCopy, shape.Docs)); {
	case errors.Is(err, window.ErrTooSmall):
		metrics.FTSBuildWindowTooSmall.Inc()
		m.env.Log.Warn("fts_due: window_too_small: the reshard of the account takes longer than the reshard window; "+
			"plan it with fts_layout for the account, or widen fts.reshard_window", "account", e.ID, "docs", shape.Docs,
			"window", b.Window.String())

		return &model.DueKey{Due: now.Add(ftsDueTooSmallWait), Gen: e.Gen}, false, nil
	case err != nil:
		metrics.FTSBuildWindowDeferred.Inc()
		return &model.DueKey{Due: m.windowStart(e.ID, now), Gen: e.Gen}, false, nil
	}

	return nil, false, nil
}

// ftsDueShape is what FTSLayoutDue reports of the account (documents,
// load).
func (m *Manager) ftsDueShape(account string) (model.FTSLayoutShape, error) {
	u, err := m.env.Accounts.User(account)
	if err != nil {
		return model.FTSLayoutShape{}, err
	}
	defer u.Close()

	lay, ok := u.(model.FTSLayouter)
	if !ok {
		return model.FTSLayoutShape{}, nil
	}

	shape, _, err := lay.FTSLayoutDue()

	return shape, err
}

// ftsDueRunningCount is the number of jobs the worker planned that have
// not ended (one read per remembered job).
func (m *Manager) ftsDueRunningCount() (int, error) {
	m.ftsDueMu.Lock()
	defer m.ftsDueMu.Unlock()

	for id := range m.ftsDueRunning {
		j, err := m.st.getJob(id)

		switch {
		case errors.Is(err, ErrNotFound):
			delete(m.ftsDueRunning, id)
		case err != nil:
			return 0, err
		case j.Terminal():
			delete(m.ftsDueRunning, id)
		}
	}

	return len(m.ftsDueRunning), nil
}

// ftsDueStarted remembers a job the worker planned.
func (m *Manager) ftsDueStarted(id string) {
	m.ftsDueMu.Lock()
	defer m.ftsDueMu.Unlock()

	if m.ftsDueRunning == nil {
		m.ftsDueRunning = map[string]bool{}
	}

	if len(m.ftsDueRunning) < ftsDueRunningMemoryJobs {
		m.ftsDueRunning[id] = true
	}
}

// ftsDueNoop handles an account whose job found nothing to do (R4.6):
// the per-object trigger and the target from the sum can disagree, so
// the entry waits FTSDueNoopWait(rec.Noops) — six hours, doubling with
// every further no-op of the generation up to seven days — instead of
// planning a job every time the writers' memory of it expires. An entry
// that fires when the wait is over goes, and the record turns idle
// (temp/13 R2.1): a writer whose trigger still asks for a reshard queues
// the account afresh, and the worker plans a new job then. Before 0.49.0
// the entry was moved to the same past time on every look, once a
// minute, and never planned again.
func (m *Manager) ftsDueNoop(e model.DueEntry, rec *model.FTSDueRecord, lost func() bool) error {
	now := m.now()
	wait := model.FTSDueNoopWait(rec.Noops)

	if !rec.NoopAt.IsZero() && !now.Before(rec.NoopAt.Add(wait)) {
		return m.ftsDueForget(e, rec, lost)
	}

	if rec.NoopAt.IsZero() {
		if lost() {
			return nil
		}

		err := m.st.kv.Update(model.FTSDueRecordKey(e.ID), func(cur []byte) ([]byte, error) {
			var c model.FTSDueRecord
			if cur == nil || json.Unmarshal(cur, &c) != nil || c.JobID != rec.JobID {
				return cur, nil
			}

			c.NoopAt = now.UTC()

			return json.Marshal(c)
		})
		if err != nil {
			return err
		}

		rec.NoopAt = now
	}

	return m.ftsDueMove(e, &model.DueKey{Due: rec.NoopAt.Add(wait), Gen: e.Gen}, lost)
}

// ftsDueForget marks the record of a no-op job idle, exactly that one,
// and then removes the entry that fired; nothing after a lost lease. The
// record changes first: an entry left behind by a failed removal fires
// again and plans a job, while a record left behind without the mark
// would answer every later entry of the generation with this branch
// again. The idle record keeps the count of no-ops for the next wait.
func (m *Manager) ftsDueForget(e model.DueEntry, rec *model.FTSDueRecord, lost func() bool) error {
	if lost() {
		return nil
	}

	err := m.st.kv.Update(model.FTSDueRecordKey(e.ID), func(cur []byte) ([]byte, error) {
		var c model.FTSDueRecord
		if cur == nil || json.Unmarshal(cur, &c) != nil || c.JobID != rec.JobID || c.Gen != rec.Gen {
			return cur, nil
		}

		c.Idle = true

		return json.Marshal(c)
	})
	if err != nil {
		return err
	}

	return m.ftsDueMove(e, nil, lost)
}

// ftsDueLater postpones the entry by the record's backoff.
func (m *Manager) ftsDueLater(e model.DueEntry, gen string, lost func() bool) error {
	waits := 0

	if lost() {
		return nil
	}

	err := m.st.kv.Update(model.FTSDueRecordKey(e.ID), func(cur []byte) ([]byte, error) {
		var c model.FTSDueRecord
		if cur == nil || json.Unmarshal(cur, &c) != nil || c.Gen != gen {
			return cur, nil
		}

		c.Waits++
		waits = c.Waits

		return json.Marshal(c)
	})
	if err != nil {
		return err
	}

	return m.ftsDueMove(e, &model.DueKey{Due: m.now().Add(model.FTSDueBackoffAfter(waits)), Gen: e.Gen}, lost)
}

// ftsDuePlan plans and applies fts_layout {"due_only": true} without an
// idempotency key, records the job (with the no-ops of the generation
// before it) and postpones the entry.
func (m *Manager) ftsDuePlan(ctx context.Context, e model.DueEntry, gen string, noops int, lost func() bool) error {
	kind, ok := m.kinds[KindFTSLayout]
	if !ok || lost() {
		return nil
	}

	to, urgent, err := m.ftsDueGate(e)
	if err != nil {
		return err
	}

	if to != nil {
		return m.ftsDueMove(e, to, lost)
	}

	// An urgent job runs outside the window: its online run is not
	// admitted by the window again (onlineRequest).
	principal := ftsDuePrincipal
	if urgent {
		principal = ftsDueUrgentPrincipal
	}

	params := json.RawMessage(`{"due_only":true}`)

	plan, err := m.CreatePlan(ctx, kind, e.ID, params, principal, nil)
	if err != nil {
		return err
	}

	if lost() {
		return nil
	}

	j, _, err := m.Submit(kind, e.ID, nil, plan.ID, principal, "")
	if err != nil || lost() {
		return err
	}

	m.ftsDueStarted(j.ID)

	b, _ := json.Marshal(model.FTSDueRecord{JobID: j.ID, Gen: gen, Noops: noops})

	if err := m.st.kv.Update(model.FTSDueRecordKey(e.ID), func([]byte) ([]byte, error) { return b, nil }); err != nil {
		return err
	}

	m.env.Log.Info("fts_due: reshard planned", "account", e.ID, "job", j.ID)

	return m.ftsDueMove(e, &model.DueKey{Due: m.now().Add(model.FTSDueBackoff), Gen: e.Gen}, lost)
}

// ftsDueMove moves (nil: removes) exactly the fired entry, unless the
// lease was lost meanwhile (R4.7).
func (m *Manager) ftsDueMove(e model.DueEntry, to *model.DueKey, lost func() bool) error {
	if lost() {
		return nil
	}

	return m.env.FTSDue.Queue.Move(e.ID, []model.DueKey{e.DueKey}, to)
}
