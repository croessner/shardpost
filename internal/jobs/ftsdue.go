package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"shardpost/internal/model"
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

// ftsDuePrincipal is the principal of the jobs the worker plans.
const ftsDuePrincipal = "fts_due"

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

	switch {
	case idle:
		return m.ftsDuePlan(ctx, e, gen, noops+1, lost) // the wait after the last no-op is over
	case j != nil && !j.Terminal():
		return m.ftsDueLater(e, gen, lost)
	case j != nil && j.State == StateSucceeded && !ftsLayoutNoop(j):
		if err := m.ftsDueMove(e, nil, lost); err != nil {
			return err
		}

		return m.st.kv.Update(model.FTSDueRecordKey(e.ID), func([]byte) ([]byte, error) { return nil, nil })
	case j != nil && j.State == StateSucceeded:
		return m.ftsDueNoop(e, rec, lost)
	}

	return m.ftsDuePlan(ctx, e, gen, noops, lost) // a failed job keeps the count
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

	params := json.RawMessage(`{"due_only":true}`)

	plan, err := m.CreatePlan(ctx, kind, e.ID, params, ftsDuePrincipal, nil)
	if err != nil {
		return err
	}

	if lost() {
		return nil
	}

	j, _, err := m.Submit(kind, e.ID, nil, plan.ID, ftsDuePrincipal, "")
	if err != nil || lost() {
		return err
	}

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
