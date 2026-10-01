package jobs

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"shardpost/internal/model"
)

// Opener opens accounts (the admin view that does not refuse an account
// under the maintenance lock).
type Opener interface {
	User(name string) (model.User, error)
}

// Env are the dependencies of job kinds.
type Env struct {
	Accounts Opener
	Log      *slog.Logger
	// Learning removes the learning jobs of an account (purge); nil skips.
	Learning interface {
		PurgeOwner(account string) (int, error)
	}
	// Directory is the account directory a batch walks (nil: no
	// batches).
	Directory model.AccountDirectory
}

// Kind is one job kind.
type Kind interface {
	// Name is the kind in the API.
	Name() string
	// Role is the admin role that may plan and run it.
	Role() string
	// Planned kinds only run from a plan (Plan → Apply).
	Planned() bool
	// Exclusive kinds hold the account's maintenance lock while they run.
	Exclusive() bool
	// Plan computes the dry run: a summary for the operator and a
	// fingerprint of the state the run relies on ("" = no drift check).
	Plan(ctx context.Context, env *Env, u model.User, account string, params json.RawMessage) (summary any, fingerprint string, err error)
	// Run executes the job. It calls run.Fence before every mutation and
	// may checkpoint to resume after a takeover.
	Run(ctx context.Context, run *Run) (result any, err error)
}

// Config tunes the job system.
type Config struct {
	Poll        time.Duration
	LeaseTTL    time.Duration
	Retention   time.Duration
	PlanTTL     time.Duration
	MaxParallel int
	// Settle is how long an exclusive job waits after taking the
	// maintenance lock, so deliveries and protocol sessions that opened
	// the account before notice it (sessions re-check every
	// model.LockCheckInterval).
	Settle time.Duration
	// ExpiryPoll is how often the expiry worker deletes what is due and
	// a server without the worker lease tries to take it.
	ExpiryPoll time.Duration
	// BatchParallel is how many accounts a batch works on at once,
	// ScrubParallel the same for a batch of scrubs (both 1 when 0).
	BatchParallel int
	ScrubParallel int
	// ScrubInterval is how long a batch scrub with skip_idle may leave an
	// idle account alone: after it, the account is scrubbed again in any
	// case (7 days when 0 or less; scrub.interval_days is at least 1).
	ScrubInterval time.Duration
}

func (c Config) withDefaults() Config {
	c.Poll = cmpDur(c.Poll, 5*time.Second)
	c.ExpiryPoll = cmpDur(c.ExpiryPoll, time.Minute)
	c.LeaseTTL = cmpDur(c.LeaseTTL, time.Minute)
	c.Retention = cmpDur(c.Retention, 14*24*time.Hour)
	c.PlanTTL = cmpDur(c.PlanTTL, time.Hour)

	if c.MaxParallel <= 0 {
		c.MaxParallel = 1
	}

	c.BatchParallel, c.ScrubParallel = max(c.BatchParallel, 1), max(c.ScrubParallel, 1)

	c.ScrubInterval = cmpDur(c.ScrubInterval, 7*24*time.Hour)

	return c
}

func cmpDur(v, d time.Duration) time.Duration {
	if v <= 0 {
		return d
	}

	return v
}

// Manager is the job system of one server: the API side and the runner.
type Manager struct {
	st       store
	env      *Env
	kinds    map[string]Kind
	cfg      Config
	instance string
	now      func() time.Time
	// q are the due queues (nil: the admin store has none or its index
	// mode is off).
	q *queues
	// afterBatchSubmit (tests) runs between the submit of SubmitBatchOnce
	// and its second look.
	afterBatchSubmit func()
}

// NewManager builds the job system; instance names this server in
// leases. An admin store with indexes (model.IndexedKV) gets the due
// queues written in its mode.
func NewManager(kv model.AdminKV, env *Env, cfg Config, instance string, kinds ...Kind) *Manager {
	m := &Manager{st: store{kv}, env: env, kinds: map[string]Kind{}, cfg: cfg.withDefaults(), instance: instance, now: time.Now,
		q: newQueues(kv)}
	for _, k := range kinds {
		m.kinds[k.Name()] = k
	}

	// Every manager can run a batch of the kinds it has.
	if _, ok := m.kinds[KindBatch]; !ok {
		m.kinds[KindBatch] = newBatchKind()
	}

	return m
}

// Kind returns a registered kind.
func (m *Manager) Kind(name string) (Kind, error) {
	k, ok := m.kinds[name]
	if !ok {
		return nil, ErrUnknownKind
	}

	return k, nil
}

// maxPlanSummary bounds the summary of a plan so the plan record fits
// into one admin record.
const maxPlanSummary = model.MaxAdminValue * 3 / 4

// ErrNeedsPlan and ErrNoPlan refuse submitting a kind the wrong way.
var (
	ErrNeedsPlan = errors.New("this kind runs from a plan")
	ErrNoPlan    = errors.New("this kind runs without a plan")
)

// CreatePlan computes and stores the plan of a planned kind. allow (if
// set) vets the other accounts the plan would change before it is stored.
func (m *Manager) CreatePlan(ctx context.Context, kind Kind, account string, params json.RawMessage, principal string,
	allow func(touched []string) error,
) (*Plan, error) {
	if !kind.Planned() {
		return nil, ErrNoPlan
	}

	if IsBatch(kind) {
		return nil, fmt.Errorf("%w: plan a batch over all accounts instead", ErrInvalidParams)
	}

	u, err := m.env.Accounts.User(account)
	if err != nil {
		return nil, err
	}
	defer u.Close()

	summary, fp, err := kind.Plan(ctx, m.env, u, account, params)
	if err != nil {
		return nil, err
	}

	var touched []string
	if t, ok := summary.(Touching); ok {
		touched = t.Touched()
	}

	if allow != nil {
		if err := allow(touched); err != nil {
			return nil, err
		}
	}

	b, err := json.Marshal(summary)
	if err != nil {
		return nil, err
	}

	if len(b) > maxPlanSummary {
		return nil, fmt.Errorf("%w: the plan is too large (%d bytes), narrow it", ErrInvalidParams, len(b))
	}

	now := m.now()
	p := &Plan{ID: newID(now), Kind: kind.Name(), Account: account, Params: params, Summary: b, Fingerprint: fp, Touched: touched,
		Principal: principal, Created: now, Expires: now.Add(m.cfg.PlanTTL)}

	if err := m.storePlan(p); err != nil {
		return nil, err
	}

	return p, nil
}

// storePlan stores a new plan: its expiry entry before the record
// (completeness), removed again when the record could not be stored.
func (m *Manager) storePlan(p *Plan) error {
	if m.q == nil {
		return m.st.create(prefixPlan+p.ID, p)
	}

	expiry := planExpiryKey(p, m.cfg.Retention)

	if err := m.q.expiry.Move(prefixPlan+p.ID, nil, &expiry); err != nil {
		return fmt.Errorf("expiry queue: %w", err)
	}

	if err := m.st.create(prefixPlan+p.ID, p); err != nil {
		_ = m.q.expiry.Move(prefixPlan+p.ID, []model.DueKey{expiry}, nil)
		return err
	}

	return nil
}

// GetPlan returns a plan.
func (m *Manager) GetPlan(id string) (*Plan, error) { return m.st.getPlan(id) }

// Submit queues a job: from a plan (planID set) or, for an unplanned
// kind, directly. A repeated idempotency key with the same request
// returns the existing job (created false); the key is claimed before
// the job exists, so concurrent repeats cannot both create one.
func (m *Manager) Submit(kind Kind, account string, params json.RawMessage, planID, principal, idemKey string) (*Job, bool, error) {
	if err := checkSubmit(kind, account, planID); err != nil {
		return nil, false, err
	}

	now := m.now()
	j := &Job{ID: newID(now), Kind: kind.Name(), Account: account, Params: params, PlanID: planID, State: StateQueued,
		Principal: principal, Created: now}

	if idemKey != "" {
		if existing, err := m.claimIdem(j, principal, idemKey); existing != nil || err != nil {
			return existing, false, err
		}
	}

	if err := m.queue(j, planID); err != nil {
		m.unclaimPlan(planID, j.ID)

		if idemKey != "" {
			_ = m.st.kv.Update(j.IdemKey, func([]byte) ([]byte, error) { return nil, nil })
		}

		return nil, false, err
	}

	return j, true, nil
}

// checkSubmit refuses submitting a kind the wrong way.
func checkSubmit(kind Kind, account, planID string) error {
	switch {
	case kind.Planned() && planID == "":
		return ErrNeedsPlan
	case !kind.Planned() && planID != "":
		return ErrNoPlan
	case IsBatch(kind) && account != "":
		return fmt.Errorf("%w: a batch runs over all accounts, not for one", ErrInvalidParams)
	case !IsBatch(kind) && account == "":
		// Only a batch is without an account; the empty name is none.
		return fmt.Errorf("%w: %s needs an account", ErrInvalidParams, kind.Name())
	default:
		return nil
	}
}

// claimIdem claims the idempotency key for j; when the key exists it
// returns the job of an identical earlier request (or ErrIdempotency).
func (m *Manager) claimIdem(j *Job, principal, idemKey string) (*Job, error) {
	hash := requestHash(j.Kind, j.Account, j.Params, j.PlanID)
	j.IdemKey = idemKeyOf(principal, idemKey)

	// A retry of an existing key writes nothing (a key that vanished in
	// between is claimed anew below).
	switch _, err := m.st.kv.Get(j.IdemKey); {
	case err == nil:
		if existing, gerr := m.byIdempotency(principal, idemKey, hash); gerr != nil || existing != nil {
			return existing, gerr
		}
	case !errors.Is(err, model.ErrNotFound):
		return nil, err
	}

	// The key expires with its job; its placeholder comes before the
	// record, so a crash before the job is stored cannot block the key
	// for good.
	if m.q != nil {
		if err := m.q.expiry.Move(j.IdemKey, nil, new(expiryKey(j, m.cfg.Retention))); err != nil {
			return nil, fmt.Errorf("expiry queue: %w", err)
		}
	}

	err := m.st.create(j.IdemKey, idempotency{JobID: j.ID, RequestHash: hash})
	if err == nil {
		return nil, nil
	}

	// A concurrent submit created the key meanwhile; the placeholder
	// left behind is cleaned by the expiry worker.
	existing, gerr := m.byIdempotency(principal, idemKey, hash)
	if gerr != nil || existing != nil {
		return existing, gerr
	}

	return nil, err
}

// queue claims the plan (if any) and stores the job: its queue entries
// first (claimable at its creation time, the expiry placeholder), then
// the record; the runners are woken.
func (m *Manager) queue(j *Job, planID string) error {
	if planID != "" {
		if err := m.claimPlan(planID, j); err != nil {
			return err
		}
	}

	if m.q == nil {
		return m.st.create(prefixJob+j.ID, j)
	}

	claim, _ := claimKey(j)
	expiry := expiryKey(j, m.cfg.Retention)

	if err := m.q.expiry.Move(prefixJob+j.ID, nil, &expiry); err != nil {
		return fmt.Errorf("expiry queue: %w", err)
	}

	if err := m.q.claimable.Move(j.ID, nil, &claim); err != nil {
		return fmt.Errorf("claimable queue: %w", err)
	}

	if err := m.st.create(prefixJob+j.ID, j); err != nil {
		_ = m.q.claimable.Move(j.ID, []model.DueKey{claim}, nil)
		_ = m.q.expiry.Move(prefixJob+j.ID, []model.DueKey{expiry}, nil)

		return err
	}

	m.q.claimable.Notify()

	return nil
}

// unclaimPlan frees a plan claimed by a job that could not be stored.
func (m *Manager) unclaimPlan(planID, jobID string) {
	if planID == "" {
		return
	}

	_ = m.st.kv.Update(prefixPlan+planID, func(cur []byte) ([]byte, error) {
		var p Plan
		if cur == nil || json.Unmarshal(cur, &p) != nil || p.AppliedJob != jobID {
			return cur, nil
		}

		p.AppliedJob = ""

		return json.Marshal(&p)
	})
}

// claimPlan marks the plan applied by j (once, before it expires) and
// copies kind, account and params into j.
func (m *Manager) claimPlan(planID string, j *Job) error {
	return m.st.kv.Update(prefixPlan+planID, func(cur []byte) ([]byte, error) {
		if cur == nil {
			return nil, ErrNotFound
		}

		var p Plan
		if err := json.Unmarshal(cur, &p); err != nil {
			return nil, err
		}

		switch {
		case p.AppliedJob != "":
			return nil, ErrPlanUsed
		case !m.now().Before(p.Expires):
			return nil, ErrPlanExpired
		case p.Kind != j.Kind || p.Account != j.Account:
			return nil, fmt.Errorf("plan %s is for %s on %s", planID, p.Kind, p.Account)
		}

		p.AppliedJob, j.Params = j.ID, p.Params

		return json.Marshal(&p)
	})
}

func (m *Manager) byIdempotency(principal, key, hash string) (*Job, error) {
	var idem idempotency

	err := m.st.get(idemKeyOf(principal, key), &idem)
	if errors.Is(err, ErrNotFound) {
		return nil, nil
	}

	if err != nil {
		return nil, err
	}

	if idem.RequestHash != hash {
		return nil, ErrIdempotency
	}

	// A concurrent submit claimed the key and is still storing its job.
	for range idemRetries {
		j, err := m.st.getJob(idem.JobID)
		if !errors.Is(err, ErrNotFound) {
			return j, err
		}

		time.Sleep(idemRetryWait)
	}

	return nil, fmt.Errorf("%w: the job of this key is not stored (yet), retry", ErrIdempotency)
}

// How long a repeated idempotency key waits for the first submit's job.
const (
	idemRetries   = 10
	idemRetryWait = 50 * time.Millisecond
)

func idemKeyOf(principal, key string) string {
	sum := sha256.Sum256([]byte(principal + "\x00" + key))
	return prefixIdem + hex.EncodeToString(sum[:])
}

func requestHash(kind, account string, params json.RawMessage, planID string) string {
	sum := sha256.Sum256([]byte(kind + "\x00" + account + "\x00" + string(params) + "\x00" + planID))
	return hex.EncodeToString(sum[:])
}

// Get returns a job.
func (m *Manager) Get(id string) (*Job, error) { return m.st.getJob(id) }

// List returns up to limit jobs after the given id matching account and
// state ("" = any), oldest first; next continues the listing and is
// empty on the last page. One match beyond limit is looked up, so an
// exactly full last page has no next.
func (m *Manager) List(account, state, after string, limit int) (out []Job, next string, err error) {
	if m.q.reads() {
		return m.listIndexed(account, state, after, limit)
	}

	for len(out) <= limit {
		page, err := m.st.listJobs(after, limit)
		if err != nil {
			return nil, "", err
		}

		for i := range page {
			after = page[i].ID

			if (account == "" || page[i].Account == account) && (state == "" || page[i].State == state) {
				out = append(out, page[i])
			}
		}

		if len(page) < limit {
			break
		}
	}

	if len(out) > limit {
		return out[:limit], out[limit-1].ID, nil
	}

	return out, "", nil
}

// Cancel asks a job to stop: a queued job is cancelled at once, a
// running one stops at its next safe point.
func (m *Manager) Cancel(id string) (*Job, error) {
	return m.updateJob(id, nil, func(j *Job) error {
		switch j.State {
		case StateQueued:
			j.State, j.Finished = StateCancelled, m.now()
		case StateRunning:
			j.State = StateCancelRequested
		case StateCancelRequested:
		default:
			return ErrNotCancelled
		}

		return nil
	})
}
