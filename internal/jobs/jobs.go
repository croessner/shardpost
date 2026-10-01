// Package jobs runs durable maintenance jobs (roadmap stage 8c): every
// server of the cluster runs a Runner that claims queued jobs from the
// shared admin store (model.AdminKV), holds them with a lease that it
// renews, and fences every mutation on that lease. A job whose server
// dies is taken over after its lease expired and resumes from its last
// checkpoint. Dangerous jobs run from a plan (Plan → Apply): the plan
// names the target set and effects, expires, and the apply refuses when
// the account drifted from it.
package jobs

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"shardpost/internal/model"
)

// States of a job.
const (
	StateQueued          = "queued"
	StateRunning         = "running"
	StateSucceeded       = "succeeded"
	StatePartiallyFailed = "partially_failed"
	StateFailed          = "failed"
	StateCancelRequested = "cancel_requested"
	StateCancelled       = "cancelled"
)

// Key prefixes in the admin store.
const (
	prefixJob  = "job/"
	prefixPlan = "plan/"
	prefixIdem = "idem/"
)

// Errors of the job API.
var (
	ErrNotFound     = errors.New("no such job or plan")
	ErrPlanExpired  = errors.New("plan expired, create a new one")
	ErrPlanUsed     = errors.New("plan already applied")
	ErrIdempotency  = errors.New("idempotency key reused with another request")
	ErrUnknownKind  = errors.New("unknown job kind")
	ErrLeaseLost    = errors.New("job lease lost")
	ErrDrift        = errors.New("the account changed since the plan, create a new one")
	ErrNotCancelled = errors.New("job already finished")
	// ErrInvalidParams wraps every refusal of a kind's parameters or target.
	ErrInvalidParams = errors.New("invalid request")
)

// Progress is the visible progress of a running job.
type Progress struct {
	Phase string `json:"phase,omitempty"`
	Done  int64  `json:"done"`
	Total int64  `json:"total"`
}

// Lease is the claim of a server on a running job.
type Lease struct {
	Owner string    `json:"owner,omitempty"`
	Until time.Time `json:"until"`
	Gen   uint64    `json:"gen"`
}

// Job is one durable job.
type Job struct {
	ID         string          `json:"id"`
	Kind       string          `json:"kind"`
	Account    string          `json:"account"`
	Params     json.RawMessage `json:"params,omitempty"`
	PlanID     string          `json:"plan_id,omitempty"`
	State      string          `json:"state"`
	Principal  string          `json:"principal"`
	Created    time.Time       `json:"created"`
	Started    time.Time       `json:"started,omitzero"`
	Finished   time.Time       `json:"finished,omitzero"`
	Progress   Progress        `json:"progress"`
	Checkpoint json.RawMessage `json:"checkpoint,omitempty"`
	Result     json.RawMessage `json:"result,omitempty"`
	Errors     []string        `json:"errors,omitempty"`
	Lease      Lease           `json:"lease"`
	Attempts   int             `json:"attempts"`
	// IdemKey is the admin store key of the idempotency key that created
	// the job ("" for none); it expires with the job.
	IdemKey string `json:"idem_key,omitempty"`
}

// Terminal reports whether the job has ended.
func (j *Job) Terminal() bool {
	switch j.State {
	case StateSucceeded, StatePartiallyFailed, StateFailed, StateCancelled:
		return true
	default:
		return false
	}
}

// claimable reports whether a runner may take the job over now.
func (j *Job) claimable(now time.Time) bool {
	switch j.State {
	case StateQueued:
		return true
	case StateRunning, StateCancelRequested:
		return !now.Before(j.Lease.Until)
	default:
		return false
	}
}

// Plan is the dry run of a dangerous job: what it would do, valid until
// Expires, applied at most once.
type Plan struct {
	ID          string          `json:"id"`
	Kind        string          `json:"kind"`
	Account     string          `json:"account"`
	Params      json.RawMessage `json:"params,omitempty"`
	Summary     json.RawMessage `json:"summary"`
	Fingerprint string          `json:"fingerprint,omitempty"`
	Touched     []string        `json:"touched,omitempty"`
	Principal   string          `json:"principal"`
	Created     time.Time       `json:"created"`
	Expires     time.Time       `json:"expires"`
	AppliedJob  string          `json:"applied_job,omitempty"`
}

// idempotency binds a key to the job it created and the request.
type idempotency struct {
	JobID       string `json:"job_id"`
	RequestHash string `json:"request_hash"`
}

// newID returns a time-ordered id (listing in key order is creation
// order).
func newID(now time.Time) string {
	var b [6]byte

	_, _ = rand.Read(b[:])

	return now.UTC().Format("20060102T150405Z") + "-" + hex.EncodeToString(b[:])
}

// store wraps the admin store with typed access.
type store struct{ kv model.AdminKV }

func (s store) getJob(id string) (*Job, error) {
	var j Job
	if err := s.get(prefixJob+id, &j); err != nil {
		return nil, err
	}

	return &j, nil
}

func (s store) getPlan(id string) (*Plan, error) {
	var p Plan
	if err := s.get(prefixPlan+id, &p); err != nil {
		return nil, err
	}

	return &p, nil
}

func (s store) get(key string, v any) error {
	b, err := s.kv.Get(key)
	if errors.Is(err, model.ErrNotFound) {
		return ErrNotFound
	}

	if err != nil {
		return err
	}

	return json.Unmarshal(b, v)
}

// updateJob applies fn to the stored job atomically; fn returning an
// error aborts without a write.
func (s store) updateJob(id string, fn func(j *Job) error) (*Job, error) {
	var out Job

	err := s.kv.Update(prefixJob+id, func(cur []byte) ([]byte, error) {
		if cur == nil {
			return nil, ErrNotFound
		}

		var j Job
		if err := json.Unmarshal(cur, &j); err != nil {
			return nil, fmt.Errorf("job %s unreadable: %w", id, err)
		}

		if err := fn(&j); err != nil {
			return nil, err
		}

		out = j

		return json.Marshal(&j)
	})
	if err != nil {
		return nil, err
	}

	return &out, nil
}

// create stores a new record under key; it fails if the key exists.
func (s store) create(key string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}

	return s.kv.Update(key, func(cur []byte) ([]byte, error) {
		if cur != nil {
			return nil, fmt.Errorf("%s exists", key)
		}

		return b, nil
	})
}

// listJobs returns up to limit jobs after the given id, oldest first.
func (s store) listJobs(after string, limit int) ([]Job, error) {
	start := ""
	if after != "" {
		start = prefixJob + after
	}

	recs, err := s.kv.List(prefixJob, start, limit)
	if err != nil {
		return nil, err
	}

	out := make([]Job, 0, len(recs))

	for _, r := range recs {
		var j Job
		if json.Unmarshal(r.Value, &j) == nil {
			out = append(out, j)
		}
	}

	return out, nil
}
