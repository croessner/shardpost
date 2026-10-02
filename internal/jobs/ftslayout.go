package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"shardpost/internal/model"
)

// KindFTSLayout converts the full-text index of an account between the
// layouts or reshards a layout 3 index (stage B14b).
const KindFTSLayout = "fts_layout"

// ftsLayoutParams are the parameters of fts_layout: the target layout (0:
// the account's), forced object counts (0: from the key counts) and
// due_only, which ends without a change while the counts ask for the
// shape the index has.
type ftsLayoutParams struct {
	Layout  int  `json:"layout,omitempty"`
	Shards  int  `json:"shards,omitempty"`
	DueOnly bool `json:"due_only,omitempty"`
}

func parseFTSLayout(params json.RawMessage) (ftsLayoutParams, error) {
	var p ftsLayoutParams
	if len(params) > 0 {
		if err := json.Unmarshal(params, &p); err != nil {
			return p, fmt.Errorf("%w: params: %v", ErrInvalidParams, err)
		}
	}

	switch {
	case p.Layout != 0 && p.Layout != model.FTSLayout2 && p.Layout != model.FTSLayout3:
		return p, fmt.Errorf("%w: layout must be 2 or 3", ErrInvalidParams)
	case p.Shards < 0 || p.Shards > 4096:
		return p, fmt.Errorf("%w: shards must be 0 to 4096", ErrInvalidParams)
	case p.Shards > 0 && p.Layout == model.FTSLayout2:
		return p, fmt.Errorf("%w: shards apply to layout 3 only", ErrInvalidParams)
	}

	return p, nil
}

// ftsLayoutKind runs a conversion or reshard under the maintenance lock
// (exclusive; deliveries meet a frozen index for the whole job and
// answer a temporary failure after fts.frozen_wait_seconds).
type ftsLayoutKind struct{ kindBase }

// ftsLayoutSummary is the plan of fts_layout.
type ftsLayoutSummary struct {
	State  model.FTSLayoutState  `json:"state"`
	Target *model.FTSLayoutShape `json:"target,omitempty"`
	Due    bool                  `json:"due"`
}

func (k *ftsLayoutKind) Plan(_ context.Context, env *Env, u model.User, _ string, params json.RawMessage) (any, string, error) {
	p, err := parseFTSLayout(params)
	if err != nil {
		return nil, "", err
	}

	if err := requireLayout3(env, p); err != nil {
		return nil, "", err
	}

	lay, err := capability[model.FTSLayouter](u, "change the full-text layout")
	if err != nil {
		return nil, "", err
	}

	st, err := lay.FTSLayout()
	if err != nil {
		return nil, "", err
	}

	s := ftsLayoutSummary{State: st}

	if st.Current.Layout == model.FTSLayout3 {
		target, due, err := lay.FTSLayoutDue()
		if err != nil {
			return nil, "", err
		}

		s.Target, s.Due = &target, due
	}

	return s, "", nil
}

// requireLayout3 refuses a conversion to layout 3 without the cluster
// mark, read now (N7).
func requireLayout3(env *Env, p ftsLayoutParams) error {
	if p.Layout != model.FTSLayout3 {
		return nil
	}

	if env == nil || env.Layout3 == nil {
		return model.ErrFTSLayoutMark
	}

	ok, err := env.Layout3.FTSLayout3Enabled()
	if err == nil && !ok {
		err = model.ErrFTSLayoutMark
	}

	return err
}

// layoutToken is the conversion token of a run: the lock session of the
// job and the generation of its lease (R4.1).
func (r *Run) layoutToken() string { return model.JobLockToken(lockSession(r.Job.ID), r.gen) }

func (k *ftsLayoutKind) Run(_ context.Context, run *Run) (res any, err error) {
	p, err := parseFTSLayout(run.Job.Params)
	if err != nil {
		return nil, err
	}

	if err := requireLayout3(run.Env, p); err != nil {
		return nil, err
	}

	lay, err := capability[model.FTSLayouter](run.User, "change the full-text layout")
	if err != nil {
		return nil, err
	}

	tok := run.layoutToken()

	// The kind clears what it left itself (F5): after an error or a
	// cancel request, but not after a lost lease, whose successor takes
	// the unfinished conversion for foreign (R4.4).
	defer func() {
		if err == nil || errors.Is(err, ErrLeaseLost) || errors.Is(run.Fence(), ErrLeaseLost) {
			return
		}

		if _, cerr := lay.ClearFTSLayoutNext(tok); cerr != nil {
			run.Env.Log.Warn("fts layout: unfinished conversion not cleared", "account", run.Job.Account, "err", cerr)
		}
	}()

	var last time.Time

	return lay.ConvertFTSLayout(model.FTSLayoutRequest{Layout: p.Layout, Shards: p.Shards, DueOnly: p.DueOnly, Tok: tok, Fence: run.Fence,
		Progress: func(phase string, done, total int) {
			if time.Since(last) > time.Second || done == total {
				last = time.Now()
				_ = run.Progress(phase, int64(done), int64(total))
			}
		}})
}

// clearForeignLayout removes an unfinished conversion of another run
// from an account whose maintenance lock the run tok just took (F2): no
// other run writes legitimately any more. Errors are logged; the run
// meets the freeze then (fts_layout replaces it in its P2).
func clearForeignLayout(u model.User, tok string, log interface{ Warn(string, ...any) }) {
	lay, ok := u.(model.FTSLayouter)
	if !ok {
		return
	}

	if _, err := lay.ClearForeignFTSLayoutNext(tok); err != nil {
		log.Warn("fts layout: foreign unfinished conversion not cleared", "err", err)
	}
}

// clearJobLayout removes the unfinished conversion of a job that has
// ended (the runner's fallback after the final state, the admin path of
// a cancel): every token of the job's lock session.
func clearJobLayout(u model.User, jobID string) error {
	lay, ok := u.(model.FTSLayouter)
	if !ok {
		return nil
	}

	st, err := lay.FTSLayout()
	if err != nil || st.NextTok == "" || model.TokenSession(st.NextTok) != lockSession(jobID) {
		return err
	}

	_, err = lay.ClearFTSLayoutNext(st.NextTok)

	return err
}

// ImportReshardFresh is how recent the heartbeat of an import reshard
// run must be for the run to count as alive (3 × its 30 s beat).
const ImportReshardFresh = 90 * time.Second

// ClearStaleLayout removes what a conversion whose run has ended left
// (the fallback of the jobs scrub and of "shardpostctl fts layout
// clear"): its unfinished conversion ("next") and the sources it left
// after its switch ("old"). A run has ended when its job is not running
// any more (pattern of PurgeRunning), an import reshard when its
// heartbeat is older than ImportReshardFresh. It returns the tokens it
// cleared, comma-separated ("" for none).
func ClearStaleLayout(kv model.AdminKV, u model.User, now time.Time) (string, error) {
	lay, ok := u.(model.FTSLayouter)
	if !ok {
		return "", nil
	}

	st, err := lay.FTSLayout()
	if err != nil {
		return "", err
	}

	var cleared []string

	if st.NextTok != "" {
		ok, err := clearIfEnded(kv, st.NextTok, st.NextAt, now, lay.ClearFTSLayoutNext)
		if err != nil {
			return "", err
		}

		if ok {
			cleared = append(cleared, st.NextTok)
		}
	}

	for i, tok := range st.OldToks {
		var at time.Time
		if i < len(st.OldAt) {
			at = st.OldAt[i]
		}

		ok, err := clearIfEnded(kv, tok, at, now, lay.ClearFTSLayoutOld)
		if err != nil {
			return strings.Join(cleared, ","), err
		}

		if ok {
			cleared = append(cleared, tok)
		}
	}

	return strings.Join(cleared, ","), nil
}

// clearIfEnded runs drop for tok when its run has ended.
func clearIfEnded(kv model.AdminKV, tok string, at, now time.Time, drop func(string) (bool, error)) (bool, error) {
	running, err := layoutRunAlive(kv, tok, at, now)
	if err != nil || running {
		return false, err
	}

	return drop(tok)
}

// layoutRunAlive reports whether the run tok (its heartbeat at, for an
// import reshard) may still be working.
func layoutRunAlive(kv model.AdminKV, tok string, at, now time.Time) (bool, error) {
	session := model.TokenSession(tok)

	id, ok := strings.CutPrefix(session, "job-")
	if !ok || lockSession(id) != session {
		return !at.IsZero() && now.Sub(at) < ImportReshardFresh, nil
	}

	j, err := store{kv}.getJob(id)

	switch {
	case errors.Is(err, ErrNotFound):
		return false, nil
	case err != nil:
		return false, err
	}

	return !j.Terminal() && !j.claimable(now), nil
}
