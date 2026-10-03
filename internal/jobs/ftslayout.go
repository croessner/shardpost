package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"shardpost/internal/metrics"
	"shardpost/internal/model"
	"shardpost/internal/window"
)

// KindFTSLayout converts the full-text index of an account between the
// layouts or reshards a layout 3 index (stage B14b).
const KindFTSLayout = "fts_layout"

// ftsLayoutParams are the parameters of fts_layout: the target layout (0:
// the account's), forced object counts — shards one count for the
// posting, dictionary and header objects alike, shape "s/d/h" each its
// own (neither: from the key counts) — the identifier filter of a layout
// 3 target (dict_filter "ids", "none"; omitted: as the configuration and
// the cluster mark of the filter say) and due_only, which ends without
// a change while the counts ask for the shape the index has and its
// filter is the one asked for. Forced counts ignore fts.shards_max and
// fts.dict_shards_max (at most 4096).
//
// online (stage B14i) runs it without the account's maintenance lock:
// true needs the cluster mark fts/online and a layout 3 index; omitted,
// the plan decides (online with the mark and fts.online_build "auto")
// and records the answer, so that the job runs as planned. A server
// before 0.51.0 ignores the field and runs the job offline under the
// lock.
type ftsLayoutParams struct {
	Layout     int    `json:"layout,omitempty"`
	Shards     int    `json:"shards,omitempty"`
	Shape      string `json:"shape,omitempty"`
	DictFilter string `json:"dict_filter,omitempty"`
	DueOnly    bool   `json:"due_only,omitempty"`
	Online     *bool  `json:"online,omitempty"`
}

// maxForcedShards bounds forced object counts.
const maxForcedShards = 4096

func parseFTSLayout(params json.RawMessage) (ftsLayoutParams, error) {
	var p ftsLayoutParams
	if len(params) > 0 {
		if err := json.Unmarshal(params, &p); err != nil {
			return p, fmt.Errorf("%w: params: %v", ErrInvalidParams, err)
		}
	}

	if p.Layout != 0 && p.Layout != model.FTSLayout2 && p.Layout != model.FTSLayout3 {
		return p, fmt.Errorf("%w: layout must be 2 or 3", ErrInvalidParams)
	}

	switch {
	case p.DictFilter != "" && p.DictFilter != model.FTSDictFilterNone && p.DictFilter != model.FTSDictFilterIDs:
		return p, fmt.Errorf("%w: dict_filter must be none or ids", ErrInvalidParams)
	case p.DictFilter != "" && p.Layout == model.FTSLayout2:
		return p, fmt.Errorf("%w: dict_filter applies to layout 3 only", ErrInvalidParams)
	}

	return p, p.checkForced()
}

// checkForced checks the forced counts: shards or shape, layout 3 only.
func (p ftsLayoutParams) checkForced() error {
	switch {
	case p.Shards < 0 || p.Shards > maxForcedShards:
		return fmt.Errorf("%w: shards must be 0 to 4096", ErrInvalidParams)
	case p.Shards > 0 && p.Shape != "":
		return fmt.Errorf("%w: shards and shape exclude each other", ErrInvalidParams)
	case (p.Shards > 0 || p.Shape != "") && p.Layout == model.FTSLayout2:
		return fmt.Errorf("%w: shards and shape apply to layout 3 only", ErrInvalidParams)
	}

	if _, err := ParseFTSShape(p.Shape); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidParams, err)
	}

	return nil
}

// ParseFTSShape parses the forced shape "s/d/h" of fts_layout: three
// counts of 1 to 4096 ("" is none: nil).
func ParseFTSShape(shape string) (*model.FTSForcedShape, error) {
	if shape == "" {
		return nil, nil
	}

	parts := strings.Split(shape, "/")
	if len(parts) != 3 {
		return nil, fmt.Errorf("shape %q is not s/d/h", shape)
	}

	var n [3]int

	for i, part := range parts {
		v, err := strconv.Atoi(part)
		if err != nil || v < 1 || v > maxForcedShards || strings.TrimLeft(part, "0123456789") != "" {
			return nil, fmt.Errorf("shape %q: every count must be 1 to 4096", shape)
		}

		n[i] = v
	}

	return &model.FTSForcedShape{S: n[0], D: n[1], H: n[2]}, nil
}

// ftsLayoutKind runs a conversion or reshard under the maintenance lock
// (exclusive; deliveries meet a frozen index for the whole job and
// answer a temporary failure after fts.frozen_wait_seconds), or online
// without it (stage B14i: deliveries, logins and sessions go on).
type ftsLayoutKind struct{ kindBase }

// ExclusiveFor implements exclusiveFor: an online run takes no lock. A
// job whose parameters do not parse is exclusive (its run fails).
func (k *ftsLayoutKind) ExclusiveFor(params json.RawMessage) bool {
	p, err := parseFTSLayout(params)

	return err != nil || !isOnline(p.Online)
}

// isOnline reports whether a normalized "online" asks for an online run.
func isOnline(b *bool) bool { return b != nil && *b }

// normalizeParams implements paramsNormalizer: "online" is decided.
// Conversions to layout 2 run offline; online true without the cluster
// mark is refused (409), on a layout 2 index (u, a single account) with
// a 400; omitted, it is true with the mark, fts.online_build "auto" and
// (for a single account) a layout 3 index.
func (k *ftsLayoutKind) normalizeParams(m *Manager, u model.User, params json.RawMessage) (json.RawMessage, error) {
	p, err := parseFTSLayout(params)
	if err != nil {
		return nil, err
	}

	if p.Layout == model.FTSLayout2 {
		if isOnline(p.Online) {
			return nil, fmt.Errorf("%w: a conversion to layout 2 runs offline", ErrInvalidParams)
		}

		p.Online = new(bool)
	} else if p.Online, err = m.decideOnline(u, p.Online); err != nil {
		return nil, err
	}

	return json.Marshal(p)
}

// decideOnline decides "online" of fts_layout or fts_rebuild for the
// account u (nil: a batch, whose runs report a layout 2 account as
// unsupported).
func (m *Manager) decideOnline(u model.User, asked *bool) (*bool, error) {
	marked, err := m.onlineMarked()
	if err != nil {
		return nil, err
	}

	switch {
	case asked != nil && *asked && !marked:
		return nil, model.ErrFTSOnlineMark
	case asked != nil && !*asked:
		return asked, nil
	case asked == nil && (!marked || !m.cfg.FTSBuild.OnlineAuto):
		return new(bool), nil
	}

	// online, explicitly or by fts.online_build auto: a single account
	// needs a layout 3 index
	return onlineFor(u, asked != nil)
}

// onlineMarked reads the cluster mark of online runs (false without a
// store that has one).
func (m *Manager) onlineMarked() (bool, error) {
	if m.env.FTSOnline == nil {
		return false, nil
	}

	return m.env.FTSOnline.FTSOnlineEnabled()
}

// onlineFor is "online" for the account u (nil: a batch) when it may
// run online: true with a layout 3 index; else offline, or a refusal
// when the request asked for online explicitly.
func onlineFor(u model.User, explicit bool) (*bool, error) {
	if u != nil {
		layout3, err := hasLayout3(u)

		switch {
		case err != nil:
			return nil, err
		case !layout3 && explicit:
			return nil, fmt.Errorf("%w: online runs need a layout 3 index; the account has layout 2 (run it offline)", ErrInvalidParams)
		case !layout3:
			return new(bool), nil
		}
	}

	on := true

	return &on, nil
}

// hasLayout3 reports whether the account's index has layout 3.
func hasLayout3(u model.User) (bool, error) {
	lay, ok := u.(model.FTSLayouter)
	if !ok {
		return false, nil
	}

	st, err := lay.FTSLayout()

	return st.Current.Layout == model.FTSLayout3, err
}

// ftsLayoutSummary is the plan of fts_layout.
type ftsLayoutSummary struct {
	State  model.FTSLayoutState  `json:"state"`
	Target *model.FTSLayoutShape `json:"target,omitempty"`
	Due    bool                  `json:"due"`
	// Online is set when the job runs without the account lock (0.51.0).
	Online bool `json:"online,omitempty"`
}

func (k *ftsLayoutKind) Plan(_ context.Context, env *Env, u model.User, _ string, params json.RawMessage) (any, string, error) {
	p, err := parseFTSLayout(params)
	if err != nil {
		return nil, "", err
	}

	if err := requireLayout3(env, p); err != nil {
		return nil, "", err
	}

	if err := requireDictFilter(env, p); err != nil {
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

	// dict_filter ids on a layout 2 index without a layout 3 target
	// would do nothing at all
	if p.DictFilter == model.FTSDictFilterIDs && p.Layout == 0 && st.Current.Layout != model.FTSLayout3 {
		return nil, "", fmt.Errorf("%w: dict_filter ids needs a layout 3 target: the account has layout 2 (add layout 3)", ErrInvalidParams)
	}

	s := ftsLayoutSummary{State: st, Online: isOnline(p.Online)}

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

// requireDictFilter refuses a target with the identifier filter without
// the cluster mark of the filter, read now (temp/13 §5.3).
func requireDictFilter(env *Env, p ftsLayoutParams) error {
	if p.DictFilter != model.FTSDictFilterIDs {
		return nil
	}

	var marked model.FTSDictFilterMarked
	if env != nil {
		marked, _ = env.Layout3.(model.FTSDictFilterMarked)
	}

	if marked == nil {
		return model.ErrFTSDictFilterMark
	}

	ok, err := marked.FTSDictFilterEnabled()
	if err == nil && !ok {
		err = model.ErrFTSDictFilterMark
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

	if err := requireDictFilter(run.Env, p); err != nil {
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
	defer run.clearOwnLayout(lay, tok, &err)

	shape, _ := ParseFTSShape(p.Shape) // checked by parseFTSLayout

	req := model.FTSLayoutRequest{Layout: p.Layout, Shards: p.Shards, Shape: shape, DictFilter: p.DictFilter,
		DueOnly: p.DueOnly, Tok: tok, Fence: run.Fence, Progress: run.throttledProgress()}

	if isOnline(p.Online) {
		run.onlineRequest(&req)
	}

	return lay.ConvertFTSLayout(req)
}

// clearOwnLayout is the kind's clean-up of its own unfinished conversion
// tok (deferred with the run's error): after an error or a cancel
// request, not after a lost lease.
func (r *Run) clearOwnLayout(lay model.FTSLayouter, tok string, err *error) {
	if *err == nil || errors.Is(*err, ErrLeaseLost) || errors.Is(r.Fence(), ErrLeaseLost) {
		return
	}

	if _, cerr := lay.ClearFTSLayoutNext(tok); cerr != nil {
		r.Env.Log.Warn("fts layout: unfinished conversion not cleared", "account", r.Job.Account, "err", cerr)
	}
}

// throttledProgress reports the progress at most once a second (and at
// the end of each phase).
func (r *Run) throttledProgress() func(phase string, done, total int) {
	var last time.Time

	return func(phase string, done, total int) {
		if time.Since(last) > time.Second || done == total {
			last = time.Now()
			_ = r.Progress(phase, int64(done), int64(total))
		}
	}
}

// onlineRequest makes req an online run of this job: a foreign run is
// judged by its job (another lease generation of this job counts as
// ended, R4.1), and a run the fts_due worker or a batch planned is
// admitted only when it fits into the reshard window.
func (r *Run) onlineRequest(req *model.FTSLayoutRequest) {
	req.Online = true

	req.ForeignAlive = func(tok string, at time.Time) (bool, error) {
		if model.TokenSession(tok) == lockSession(r.Job.ID) {
			return false, nil
		}

		return layoutRunAlive(r.m.st.kv, tok, at, r.m.now())
	}

	if r.window || (!r.sub && r.Job.Principal == ftsDuePrincipal) {
		req.Admit = r.m.admitBuild
	}
}

// admitBuild is FTSLayoutRequest.Admit with the reshard window.
func (m *Manager) admitBuild(mode string, docs int64) error {
	b := m.cfg.FTSBuild

	switch err := b.Window.Fit(m.now(), b.estimate(mode, docs)); {
	case errors.Is(err, window.ErrTooSmall):
		metrics.FTSBuildWindowTooSmall.Inc()
		return model.ErrFTSWindowTooSmall
	case err != nil:
		metrics.FTSBuildWindowDeferred.Inc()
		return model.ErrFTSBuildWindow
	}

	return nil
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
