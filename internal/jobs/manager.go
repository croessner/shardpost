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
}

func (c Config) withDefaults() Config {
	c.Poll = cmpDur(c.Poll, 5*time.Second)
	c.LeaseTTL = cmpDur(c.LeaseTTL, time.Minute)
	c.Retention = cmpDur(c.Retention, 14*24*time.Hour)
	c.PlanTTL = cmpDur(c.PlanTTL, time.Hour)

	if c.MaxParallel <= 0 {
		c.MaxParallel = 1
	}

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
}

// NewManager builds the job system; instance names this server in
// leases.
func NewManager(kv model.AdminKV, env *Env, cfg Config, instance string, kinds ...Kind) *Manager {
	m := &Manager{st: store{kv}, env: env, kinds: map[string]Kind{}, cfg: cfg.withDefaults(), instance: instance, now: time.Now}
	for _, k := range kinds {
		m.kinds[k.Name()] = k
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

	now := m.now()
	p := &Plan{ID: newID(now), Kind: kind.Name(), Account: account, Params: params, Summary: b, Fingerprint: fp, Touched: touched,
		Principal: principal, Created: now, Expires: now.Add(m.cfg.PlanTTL)}

	if err := m.st.create(prefixPlan+p.ID, p); err != nil {
		return nil, err
	}

	return p, nil
}

// GetPlan returns a plan.
func (m *Manager) GetPlan(id string) (*Plan, error) { return m.st.getPlan(id) }

// Submit queues a job: from a plan (planID set) or, for an unplanned
// kind, directly. A repeated idempotency key with the same request
// returns the existing job (created false); the key is claimed before
// the job exists, so concurrent repeats cannot both create one.
func (m *Manager) Submit(kind Kind, account string, params json.RawMessage, planID, principal, idemKey string) (*Job, bool, error) {
	switch {
	case kind.Planned() && planID == "":
		return nil, false, ErrNeedsPlan
	case !kind.Planned() && planID != "":
		return nil, false, ErrNoPlan
	}

	now := m.now()
	j := &Job{ID: newID(now), Kind: kind.Name(), Account: account, Params: params, PlanID: planID, State: StateQueued,
		Principal: principal, Created: now}

	if idemKey != "" {
		hash := requestHash(kind.Name(), account, params, planID)

		if err := m.st.create(idemKeyOf(principal, idemKey), idempotency{JobID: j.ID, RequestHash: hash}); err != nil {
			existing, gerr := m.byIdempotency(principal, idemKey, hash)
			if gerr != nil || existing != nil {
				return existing, false, gerr
			}

			return nil, false, err
		}
	}

	if err := m.queue(j, planID); err != nil {
		m.unclaimPlan(planID, j.ID)

		if idemKey != "" {
			_ = m.st.kv.Update(idemKeyOf(principal, idemKey), func([]byte) ([]byte, error) { return nil, nil })
		}

		return nil, false, err
	}

	return j, true, nil
}

// queue claims the plan (if any) and stores the job.
func (m *Manager) queue(j *Job, planID string) error {
	if planID != "" {
		if err := m.claimPlan(planID, j); err != nil {
			return err
		}
	}

	return m.st.create(prefixJob+j.ID, j)
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
	return m.st.updateJob(id, func(j *Job) error {
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
