package jobs

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"shardpost/internal/model"
)

// quota_recalc (stage B16): offline it replaces the usage counters by a
// recount under the maintenance lock (no delivery holds a reservation
// then); online it recounts beside the writers, which capture their
// changes for it, and one compare-and-swap switches the counters
// (temp/16 §4). Online runs need the cluster mark model.QuotaOnlineMarkKey.

// quotaRecalc is the kind.
type quotaRecalc struct{ kindBase }

// quotaRecalcParams are the parameters of quota_recalc.
type quotaRecalcParams struct {
	Online *bool `json:"online,omitempty"`
}

func parseQuotaRecalc(params json.RawMessage) (quotaRecalcParams, error) {
	var p quotaRecalcParams

	if len(bytes.TrimSpace(params)) > 0 {
		if err := json.Unmarshal(params, &p); err != nil {
			return p, fmt.Errorf("%w: params: %v", ErrInvalidParams, err)
		}
	}

	return p, nil
}

// ExclusiveFor implements exclusiveFor: an online run takes no lock.
func (k *quotaRecalc) ExclusiveFor(params json.RawMessage) bool {
	p, err := parseQuotaRecalc(params)

	return err != nil || !isOnline(p.Online)
}

// normalizeParams implements paramsNormalizer: "online" is decided when
// the plan is made (online true without the mark: 409; omitted: online
// with the mark and quota.online_recount auto).
func (k *quotaRecalc) normalizeParams(m *Manager, _ model.User, params json.RawMessage) (json.RawMessage, error) {
	return decideOnlineParams(m, params, parseQuotaRecalc, func(p *quotaRecalcParams) **bool { return &p.Online },
		m.quotaOnlineMark(), m.cfg.QuotaOnlineAuto, model.ErrQuotaOnlineMark)
}

// decideOnlineParams parses the parameters of a kind, decides their
// "online" against a cluster mark (decideMarked) and returns them.
func decideOnlineParams[P any](m *Manager, params json.RawMessage, parse func(json.RawMessage) (P, error), online func(*P) **bool,
	mark markReader, auto bool, refusal error,
) (json.RawMessage, error) {
	p, err := parse(params)
	if err != nil {
		return nil, err
	}

	on := online(&p)

	if *on, err = m.decideMarked(*on, mark, auto, refusal); err != nil {
		return nil, err
	}

	return json.Marshal(p)
}

// markReader reads a cluster mark.
type markReader interface{ Enabled() (bool, error) }

// quotaOnlineMark is the mark of online recounts (nil without a store
// that reads it); mailboxLockMark the mark of the mailbox lock.
func (m *Manager) quotaOnlineMark() markReader {
	if m.env.QuotaOnline == nil {
		return nil
	}

	return quotaOnlineMark{m.env.QuotaOnline}
}

func (m *Manager) mailboxLockMark() markReader {
	if m.env.MailboxLock == nil {
		return nil
	}

	return mailboxLockMark{m.env.MailboxLock}
}

// resultError is the result of a run that failed.
const resultError = "error"

// decideMarked decides an "online" parameter against a cluster mark:
// asked true without the mark is refused with refusal; asked false
// stays; omitted is true with the mark and the auto setting.
func (m *Manager) decideMarked(asked *bool, mark markReader, auto bool, refusal error) (*bool, error) {
	marked := false

	if mark != nil {
		var err error
		if marked, err = mark.Enabled(); err != nil {
			return nil, err
		}
	}

	switch {
	case asked != nil && *asked && !marked:
		return nil, refusal
	case asked != nil:
		return asked, nil
	}

	on := marked && auto

	return &on, nil
}

// quotaOnlineMark adapts model.QuotaOnlineMarked to decideMarked.
type quotaOnlineMark struct{ m model.QuotaOnlineMarked }

func (q quotaOnlineMark) Enabled() (bool, error) { return q.m.QuotaOnlineEnabled() }

// mailboxLockMark adapts model.MailboxLockMarked to decideMarked.
type mailboxLockMark struct{ m model.MailboxLockMarked }

func (q mailboxLockMark) Enabled() (bool, error) { return q.m.MailboxLockEnabled() }

func (k *quotaRecalc) Plan(_ context.Context, _ *Env, u model.User, _ string, params json.RawMessage) (any, string, error) {
	if _, err := parseQuotaRecalc(params); err != nil {
		return nil, "", err
	}

	rc, err := capability[model.QuotaRecounter](u, "recount quota")
	if err != nil {
		return nil, "", err
	}

	// A dry count: while the account is written to it is approximate
	// (the run counts exactly).
	counted, stored, err := rc.RecountQuota(false)

	return model.QuotaRecountReport{Counted: counted, Stored: stored, Differs: counted != stored, Mode: model.QuotaRecountOffline}, "", err
}

func (k *quotaRecalc) Run(_ context.Context, run *Run) (any, error) {
	p, err := parseQuotaRecalc(run.Job.Params)
	if err != nil {
		return nil, err
	}

	if isOnline(p.Online) {
		return k.runOnline(run)
	}

	rc, err := capability[model.QuotaRecounter](run.User, "recount quota")
	if err != nil {
		return nil, err
	}

	if err := run.Fence(); err != nil {
		return nil, err
	}

	counted, stored, err := rc.RecountQuota(true)
	rep := model.QuotaRecountReport{Counted: counted, Stored: stored, Differs: counted != stored, Mode: model.QuotaRecountOffline, Result: model.QuotaRecountOK}

	if err != nil {
		rep.Result = resultError
	}

	run.m.recountRun(rep)

	return rep, err
}

// runOnline is the online recount: the run key is renewed with the
// lease, a busy or lost run is given back (planned again later, the
// attempt does not count) until the plan's TTL.
func (k *quotaRecalc) runOnline(run *Run) (any, error) {
	rc, err := capability[model.QuotaOnlineRecounter](run.User, "recount quota online")
	if err != nil {
		return nil, err
	}

	tok := run.token()

	run.KeepAlive(func() error { return rc.RenewQuotaRecount(tok, 2*run.m.cfg.LeaseTTL) }, "quota run lost")
	defer run.KeepAlive(nil, "")

	rep, err := rc.RecountQuotaOnline(model.QuotaRecountRequest{Tok: tok, Fence: run.Fence, Progress: run.throttledProgress()})

	run.m.recountRun(rep)

	var busy *model.QuotaRecountBusyError

	switch {
	case errors.As(err, &busy), errors.Is(err, model.ErrQuotaRecountLost):
		if run.m.now().Sub(run.Job.Created) < run.m.cfg.PlanTTL {
			return rep, &PostponeError{Until: run.m.now().Add(run.m.cfg.LeaseTTL), Reason: err.Error()}
		}

		return rep, err
	}

	return rep, err
}

// recountRun counts a run in the metrics.
func (m *Manager) recountRun(rep model.QuotaRecountReport) {
	if m.hooks.recountRun != nil {
		m.hooks.recountRun(rep)
	}
}

// token is the token of a run: the lock session of the job and the
// generation of its lease (the conversion token of the full-text runs,
// the run key of the online recount, the mailbox lock session).
func (r *Run) token() string { return r.layoutToken() }

// KeepAlive registers fn to be called with every renewal of the job's
// lease (nil removes it); when fn fails the job is stopped with reason.
func (r *Run) KeepAlive(fn func() error, reason string) {
	r.keepMu.Lock()
	defer r.keepMu.Unlock()

	r.keepAlive, r.keepReason = fn, reason
}

// keep runs the registered KeepAlive function; false when it failed.
func (r *Run) keep(log interface{ Warn(string, ...any) }) bool {
	r.keepMu.Lock()
	fn, reason := r.keepAlive, r.keepReason
	r.keepMu.Unlock()

	if fn == nil {
		return true
	}

	if err := fn(); err != nil {
		log.Warn("jobs: "+reason, "err", err)
		r.stopReason.Store(reason)

		return false
	}

	return true
}

// clearForeignRecount clears the run key and the captures of an online
// recount from an account whose maintenance lock the caller just took:
// the recount cannot switch any more (it would book onto counters the
// lock holder may rewrite), it ends lost and is planned again.
func clearForeignRecount(u model.User, log interface{ Warn(string, ...any) }) {
	rc, ok := u.(model.QuotaOnlineRecounter)
	if !ok {
		return
	}

	if err := rc.ClearQuotaRecount(""); err != nil {
		log.Warn("online recount of the account not cleared", "err", err)
	}
}

// managerHooks are test hooks of the manager.
type managerHooks struct {
	recountRun func(model.QuotaRecountReport)
}
