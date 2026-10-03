package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"shardpost/internal/metrics"
	"shardpost/internal/model"
	"shardpost/internal/sortedindex"
)

// The batch (scaling plan B8) runs one job kind over every account of the
// account directory: an account-less job (Account "") whose parameters
// name the kind it runs per account. It walks the directory in its order
// (shard, account) a page at a time, runs the accounts of a page with a
// bounded parallelism and checkpoints the directory cursor after each
// page, so a takeover redoes at most one page. An exclusive kind takes
// the maintenance lock per account; an account another job or an import
// holds is put aside and tried again at the end. The role and the
// exclusivity of a batch are those of the kind it runs. A cancel request
// stops it between accounts.
//
// The nightly scrub is a batch of the kind scrub with skip_idle: an
// account whose last batch scrub was complete and found nothing, and
// that has not changed since (account counter and registry version, read
// after the scrub's own writes, kept in the account's directory entry),
// is skipped (scaling plan B7; model.ScrubIdle).

// KindBatch is the kind of a batch job.
const KindBatch = "batch"

// FanOutKinds are the kinds a batch can run. Their Run must not rely on
// a plan of the account: a batch has one plan for all accounts.
var FanOutKinds = []string{KindIndexCheck, KindScrub, KindFTSRebuild, KindAttachments, KindFTSLayout, KindQuotaRecalc}

// ErrDirectoryNotReady refuses a batch while the account directory
// cannot be trusted to name every account.
var ErrDirectoryNotReady = errors.New("the account directory has no completed backfill; run \"shardpost index backfill accounts\" first")

// BatchParams are the parameters of a batch job: the kind it runs per
// account and that kind's parameters.
type BatchParams struct {
	Kind   string          `json:"kind"`
	Params json.RawMessage `json:"params,omitempty"`
	// SkipIdle (scrub only) skips the accounts nothing changed in since
	// their last complete, clean batch scrub.
	SkipIdle bool `json:"skip_idle,omitempty"`
	// Anytime (fts_layout, fts_rebuild) runs the accounts whatever the
	// reshard window says (fts.reshard_window, stage B14i).
	Anytime bool `json:"anytime,omitempty"`
}

// Bounds of a batch's record.
const (
	maxBatchErrors   = 50
	maxBatchFindings = 50
	maxBatchDeferred = 1000
	maxBatchErrText  = 300
	// batchRetryRounds is how often the accounts put aside for a lock are
	// tried again at the end, a lease period apart.
	batchRetryRounds = 3
)

// batchKind is the kind of a batch job; it is planned (Manager.
// CreateBatchPlan) and never exclusive itself.
type batchKind struct{ kindBase }

func newBatchKind() *batchKind {
	return &batchKind{kindBase{KindBatch, RoleDelete, true, false}}
}

func (k *batchKind) Plan(context.Context, *Env, model.User, string, json.RawMessage) (any, string, error) {
	return nil, "", fmt.Errorf("%w: a batch is planned over all accounts, not for one", ErrInvalidParams)
}

// IsBatch reports whether a kind runs over all accounts (an account-less
// job).
func IsBatch(k Kind) bool { return k != nil && k.Name() == KindBatch }

// batchOf decodes the parameters of a batch and returns the kind it runs.
func (m *Manager) batchOf(params json.RawMessage) (Kind, BatchParams, error) {
	var p BatchParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, p, fmt.Errorf("%w: batch params: %v", ErrInvalidParams, err)
	}

	if !slices.Contains(FanOutKinds, p.Kind) {
		return nil, p, fmt.Errorf("%w: %q cannot run over all accounts (possible: %s)", ErrInvalidParams, p.Kind, strings.Join(FanOutKinds, ", "))
	}

	sub, err := m.Kind(p.Kind)
	if err != nil {
		return nil, p, err
	}

	return sub, p, nil
}

// RoleOf returns the role a job of the kind with these parameters needs:
// for a batch the role of the kind it runs (the strictest role when the
// parameters do not name one).
func (m *Manager) RoleOf(kind Kind, params json.RawMessage) string {
	if !IsBatch(kind) {
		return kind.Role()
	}

	sub, _, err := m.batchOf(params)
	if err != nil {
		return RoleDelete
	}

	return sub.Role()
}

// ExclusiveOf reports whether a job of the kind takes the maintenance
// lock: for a batch, per account when the kind it runs does.
func (m *Manager) ExclusiveOf(kind Kind, params json.RawMessage) bool {
	if !IsBatch(kind) {
		return exclusive(kind, params)
	}

	sub, p, err := m.batchOf(params)

	return err == nil && exclusive(sub, p.Params)
}

// windowKind reports whether a batch of the kind keeps the reshard
// window.
func windowKind(kind string) bool { return kind == KindFTSLayout || kind == KindFTSRebuild }

// checkBatchParams vets the per-account parameters of the kind.
func checkBatchParams(p BatchParams) error {
	if p.SkipIdle && p.Kind != KindScrub {
		return fmt.Errorf("%w: skip_idle is for scrub only", ErrInvalidParams)
	}

	if p.Anytime && !windowKind(p.Kind) {
		return fmt.Errorf("%w: anytime is for fts_layout and fts_rebuild only", ErrInvalidParams)
	}

	switch p.Kind {
	case KindScrub:
		_, err := parseScrub(p.Params)
		return err
	case KindFTSLayout:
		_, err := parseFTSLayout(p.Params)
		return err
	case KindFTSRebuild:
		_, err := parseFTSRebuild(p.Params)
		return err
	case KindQuotaRecalc:
		_, err := parseQuotaRecalc(p.Params)
		return err
	}

	if s := strings.TrimSpace(string(p.Params)); s != "" && s != "{}" && s != "null" {
		return fmt.Errorf("%w: %s takes no parameters", ErrInvalidParams, p.Kind)
	}

	return nil
}

// batchSummary is the plan of a batch.
type batchSummary struct {
	Kind      string          `json:"kind"`
	Params    json.RawMessage `json:"params,omitempty"`
	Role      string          `json:"role"`
	Exclusive bool            `json:"exclusive"`
	SkipIdle  bool            `json:"skip_idle,omitempty"`
	Anytime   bool            `json:"anytime,omitempty"`
	// Window is the reshard window the batch keeps ("": none).
	Window   string `json:"window,omitempty"`
	Parallel int    `json:"parallel"`
	Shards   int    `json:"directory_shards"`
	Backfill string `json:"directory_backfill"`
}

// CreateBatchPlan plans a batch: the kind p.Kind over every account of
// the account directory. The directory must have completed a backfill,
// or accounts from before it would be missed.
func (m *Manager) CreateBatchPlan(p BatchParams, principal string) (*Plan, error) {
	sub, err := m.Kind(p.Kind)
	if err != nil {
		return nil, err
	}

	params, _ := json.Marshal(p)
	if _, _, err := m.batchOf(params); err != nil {
		return nil, err
	}

	if err := checkBatchParams(p); err != nil {
		return nil, err
	}

	// "online" is decided for the whole batch (accounts without a layout
	// 3 index count as unsupported then).
	if p.Params, err = m.normalize(sub, nil, p.Params); err != nil {
		return nil, err
	}

	params, _ = json.Marshal(p)

	head, err := m.directoryHead()
	if err != nil {
		return nil, err
	}

	b, err := json.Marshal(batchSummary{Kind: sub.Name(), Params: p.Params, Role: sub.Role(), Exclusive: exclusive(sub, p.Params),
		SkipIdle: p.SkipIdle, Anytime: p.Anytime, Window: m.batchWindow(sub.Name(), p.Anytime), Parallel: m.batchParallel(sub.Name()),
		Shards: head.Shards, Backfill: head.Markers[model.AccountBackfillMarker]})
	if err != nil {
		return nil, err
	}

	now := m.now()
	plan := &Plan{ID: newID(now), Kind: KindBatch, Params: params, Summary: b, Principal: principal, Created: now,
		Expires: now.Add(m.cfg.PlanTTL)}

	if err := m.storePlan(plan); err != nil {
		return nil, err
	}

	return plan, nil
}

// SubmitBatchOnce plans and submits a batch unless a batch of the same
// kind has not ended yet; then it returns that one and created false.
// The nightly scrub uses it, so a scrub that runs longer than a day is
// not started twice. Two submitters racing both look again after their
// submit: the older unfinished batch stays, a newer one is cancelled.
func (m *Manager) SubmitBatchOnce(p BatchParams, principal string) (*Job, bool, error) {
	if js, err := m.unfinishedBatches(p.Kind); err != nil || len(js) > 0 {
		if err != nil {
			return nil, false, err
		}

		return &js[0], false, nil
	}

	plan, err := m.CreateBatchPlan(p, principal)
	if err != nil {
		return nil, false, err
	}

	k, err := m.Kind(KindBatch)
	if err != nil {
		return nil, false, err
	}

	j, _, err := m.Submit(k, "", nil, plan.ID, principal, "")
	if err != nil {
		return nil, false, err
	}

	if m.afterBatchSubmit != nil {
		m.afterBatchSubmit()
	}

	js, err := m.unfinishedBatches(p.Kind)
	if err != nil || len(js) == 0 || js[0].ID == j.ID {
		return j, true, nil
	}

	_, _ = m.Cancel(j.ID)

	return &js[0], false, nil
}

// unfinishedBatches returns the batches of the kind that have not ended,
// oldest first.
func (m *Manager) unfinishedBatches(kind string) ([]Job, error) {
	var out []Job

	for _, state := range []string{StateQueued, StateRunning, StateCancelRequested} {
		after := ""

		for {
			page, next, err := m.List("", state, after, 100)
			if err != nil {
				return nil, fmt.Errorf("list jobs: %w", err)
			}

			for i := range page {
				var p BatchParams
				if page[i].Kind == KindBatch && json.Unmarshal(page[i].Params, &p) == nil && p.Kind == kind {
					out = append(out, page[i])
				}
			}

			if next == "" {
				break
			}

			after = next
		}
	}

	// The id orders by the second only; the creation time decides, the id
	// breaks a tie, so every submitter picks the same oldest one.
	slices.SortFunc(out, func(a, b Job) int {
		if c := a.Created.Compare(b.Created); c != 0 {
			return c
		}

		return strings.Compare(a.ID, b.ID)
	})

	return out, nil
}

// directoryHead returns the head of the account directory, refusing a
// directory without a completed backfill.
func (m *Manager) directoryHead() (model.IndexHead, error) {
	if m.env.Directory == nil {
		return model.IndexHead{}, fmt.Errorf("%w (this store keeps none)", ErrDirectoryNotReady)
	}

	ix, release, err := m.env.Directory.AccountIndex()
	if err != nil {
		return model.IndexHead{}, err
	}
	defer release()

	done, err := sortedindex.Backfilled(ix, model.AccountBackfillMarker)
	if err != nil {
		return model.IndexHead{}, fmt.Errorf("account directory: %w", err)
	}

	if !done {
		return model.IndexHead{}, ErrDirectoryNotReady
	}

	return ix.Head()
}

// batchWindow is the reshard window a batch of the kind keeps ("" for
// none).
func (m *Manager) batchWindow(kind string, anytime bool) string {
	if anytime || !windowKind(kind) {
		return ""
	}

	return m.cfg.FTSBuild.Window.String()
}

// batchParallel is how many accounts a batch of the kind works on at
// once.
func (m *Manager) batchParallel(kind string) int {
	if kind == KindScrub {
		return m.cfg.ScrubParallel
	}

	return m.cfg.BatchParallel
}

// batchError names an account that failed.
type batchError struct {
	Account string `json:"account"`
	Error   string `json:"error"`
}

// batchState is the checkpoint and the result of a batch. Its lists are
// bounded, so the record stays far below model.MaxAdminValue however many
// accounts there are.
type batchState struct {
	Kind string `json:"kind"`
	// Cursor is the directory position after the last page done; Walked
	// is set once the directory is through.
	Cursor string `json:"cursor,omitempty"`
	Walked bool   `json:"walked,omitempty"`
	// Accounts counts the accounts looked at: Done ran the kind, Skipped
	// were idle (scrub with skip_idle), Gone were purged meanwhile,
	// Failed failed (the first maxBatchErrors in Errors, the rest in
	// ErrorsDropped).
	Accounts      int64        `json:"accounts"`
	Done          int64        `json:"done"`
	Skipped       int64        `json:"skipped"`
	Gone          int64        `json:"gone"`
	Failed        int64        `json:"failed"`
	Errors        []batchError `json:"errors,omitempty"`
	ErrorsDropped int64        `json:"errors_dropped,omitempty"`
	// Findings counts the accounts whose report had findings (scrub,
	// index_check); the first maxBatchFindings are named.
	Findings        int64    `json:"findings"`
	FindingAccounts []string `json:"finding_accounts,omitempty"`
	// Deferred are the accounts put aside because another job or an
	// import held them (or, online, another run was alive); Rounds counts
	// the retries at the end.
	Deferred []string `json:"deferred,omitempty"`
	Rounds   int      `json:"rounds,omitempty"`
	// Later are the accounts put off to the next reshard window (outside
	// it, or their run did not fit into what was left of it); they come
	// first when the batch goes on (since 0.51.0).
	Later []string `json:"later,omitempty"`
	// Unsupported counts the accounts an online run cannot handle (no
	// layout 3 index); they are not failures.
	Unsupported int64 `json:"unsupported,omitempty"`
}

// Outcomes of one account.
const (
	outDone        = "done"
	outSkipped     = "skipped"
	outGone        = "gone"
	outDeferred    = "deferred"
	outFailed      = "failed"
	outLater       = "later"
	outUnsupported = "unsupported"
)

// acctResult is what happened to one account.
type acctResult struct {
	account  string
	out      string
	err      string
	findings int
	// fatal stops the batch (lease lost, cancel requested, shutdown).
	fatal error
}

// batch is one run of a batch job.
type batch struct {
	run *Run
	sub Kind
	p   BatchParams
	dir model.AccountDirectory
	// ix is the directory; ixMu serialises its use (an index is not safe
	// for concurrent use).
	ix       model.SortedIndex
	ixMu     sync.Mutex
	shards   int
	parallel int
	// records: the run writes the scrub record of the accounts it
	// scrubbed (a repairing scrub of every class).
	records bool
	// window: the batch keeps the reshard window (fts_layout,
	// fts_rebuild without anytime, with fts.reshard_window set).
	window bool
}

func (k *batchKind) Run(ctx context.Context, run *Run) (any, error) {
	sub, p, err := run.m.batchOf(run.Job.Params)
	if err != nil {
		return nil, err
	}

	if run.Env.Directory == nil {
		return nil, fmt.Errorf("%w (this store keeps none)", ErrDirectoryNotReady)
	}

	ix, release, err := run.Env.Directory.AccountIndex()
	if err != nil {
		return nil, fmt.Errorf("account directory: %w", err)
	}
	defer release()

	head, err := ix.Head()
	if err != nil {
		return nil, fmt.Errorf("account directory: %w", err)
	}

	b := &batch{run: run, sub: sub, p: p, dir: run.Env.Directory, ix: ix, shards: head.Shards,
		parallel: max(run.m.batchParallel(sub.Name()), 1), window: run.m.batchWindow(sub.Name(), p.Anytime) != ""}

	if sub.Name() == KindScrub {
		classes, err := parseScrub(p.Params)
		if err != nil {
			return nil, err
		}

		b.records = len(classes) == len(model.RepairClasses)
	}

	st := batchState{Kind: sub.Name()}

	run.LoadCheckpoint(&st)

	// the accounts put off to this window first
	if err := b.later(ctx, &st); err != nil {
		return st.result(), err
	}

	if err := b.walk(ctx, &st); err != nil {
		return st.result(), err
	}

	if err := b.retryDeferred(ctx, &st); err != nil {
		return st.result(), err
	}

	if err := b.later(ctx, &st); err != nil {
		return st.result(), err
	}

	if st.Failed > 0 {
		run.Partial("%d of %d accounts failed (the first %d are named in the result)", st.Failed, st.Accounts, len(st.Errors))
	}

	return st.result(), nil
}

// result is the state without the resume position.
func (st batchState) result() batchState {
	st.Cursor, st.Walked = "", false

	return st
}

// walk runs the kind over the directory from the checkpoint on.
func (b *batch) walk(ctx context.Context, st *batchState) error {
	page := max(b.parallel*4, 16)

	for !st.Walked {
		if err := b.run.Fence(); err != nil {
			return err
		}

		if !b.inWindow() {
			return b.postpone() // the checkpoint of the last page is stored
		}

		b.ixMu.Lock()
		entries, next, err := b.ix.List(st.Cursor, page)
		b.ixMu.Unlock()

		if err != nil {
			return fmt.Errorf("account directory: %w", err)
		}

		results, fatal := b.runAccounts(ctx, entries)
		if fatal != nil {
			// The page is done again by whoever resumes: its results are
			// not counted.
			return fatal
		}

		st.add(results)
		st.Cursor, st.Walked = next, next == ""

		if err := b.run.Checkpoint(*st); err != nil {
			return err
		}

		b.progress(st)
	}

	return nil
}

// progress shows how far the walk is: the directory shard of the cursor
// (the number of accounts is not known up front) and the accounts done.
func (b *batch) progress(st *batchState) {
	phase := "retrying locked accounts"

	if !st.Walked {
		shard, _, _ := model.ParseIndexCursor(st.Cursor)
		phase = fmt.Sprintf("directory shard %d of %d", shard+1, b.shards)
	}

	_ = b.run.Progress(phase, st.Accounts, 0)
}

// inWindow reports whether the batch may start an account now.
func (b *batch) inWindow() bool {
	return !b.window || b.run.m.cfg.FTSBuild.Window.In(b.run.m.now())
}

// postpone gives the batch back until the next reshard window.
func (b *batch) postpone() error {
	metrics.FTSBuildWindowDeferred.Inc()

	return &PostponeError{Until: b.run.m.cfg.FTSBuild.Window.NextStart(b.run.m.now()), Reason: "waiting for the reshard window"}
}

// later runs the accounts put off to the reshard window when the batch
// is inside it; it postpones the batch while any remain.
func (b *batch) later(ctx context.Context, st *batchState) error {
	if len(st.Later) == 0 {
		return nil
	}

	if !b.inWindow() {
		return b.postpone()
	}

	entries, err := b.entriesOf(st.Later)
	if err != nil {
		return err
	}

	results, fatal := b.runAccounts(ctx, entries)
	if fatal != nil {
		return fatal
	}

	st.Later, st.Accounts = nil, st.Accounts-int64(len(results))
	st.add(results)

	if err := b.run.Checkpoint(*st); err != nil {
		return err
	}

	b.progress(st)

	if len(st.Later) > 0 {
		return b.postpone() // they did not fit into what is left of it
	}

	return nil
}

// entriesOf reads the directory entries of accounts (an account without
// one is handed on as gone for account() to find out).
func (b *batch) entriesOf(accounts []string) ([]model.IndexEntry, error) {
	entries := make([]model.IndexEntry, 0, len(accounts))

	b.ixMu.Lock()
	defer b.ixMu.Unlock()

	for _, a := range accounts {
		v, err := b.ix.Get(a)

		switch {
		case errors.Is(err, model.ErrNotFound):
			entries = append(entries, model.IndexEntry{ID: a})
		case err != nil:
			return nil, fmt.Errorf("account directory: %w", err)
		default:
			entries = append(entries, model.IndexEntry{ID: a, Value: v})
		}
	}

	return entries, nil
}

// retryDeferred tries the accounts put aside for a lock again, a lease
// period apart, and fails those still locked after the last round.
func (b *batch) retryDeferred(ctx context.Context, st *batchState) error {
	for len(st.Deferred) > 0 && st.Rounds < batchRetryRounds {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(b.run.m.cfg.LeaseTTL):
		}

		entries, err := b.entriesOf(st.Deferred)
		if err != nil {
			return err
		}

		results, fatal := b.runAccounts(ctx, entries)
		if fatal != nil {
			return fatal
		}

		st.Deferred, st.Accounts = nil, st.Accounts-int64(len(results))
		st.Rounds++

		st.add(results)

		if err := b.run.Checkpoint(*st); err != nil {
			return err
		}

		b.progress(st)
	}

	for _, a := range st.Deferred {
		st.fail(a, "the account stayed locked by an import or another job; run the job for it again later")
	}

	st.Deferred = nil

	return nil
}

// add counts the results of a page.
func (st *batchState) add(results []acctResult) {
	for _, r := range results {
		st.Accounts++

		switch r.out {
		case outDone:
			st.Done++
		case outSkipped:
			st.Skipped++
		case outGone:
			st.Gone++
		case outDeferred:
			if len(st.Deferred) < maxBatchDeferred {
				st.Deferred = append(st.Deferred, r.account)
			} else {
				st.fail(r.account, "locked by an import or another job (too many accounts put aside)")
			}
		case outLater:
			if len(st.Later) < maxBatchDeferred {
				st.Later = append(st.Later, r.account)
			} else {
				st.fail(r.account, "outside the reshard window (too many accounts put off)")
			}
		case outUnsupported:
			st.Unsupported++
		default:
			st.fail(r.account, r.err)
		}

		if r.findings > 0 {
			st.Findings++

			if len(st.FindingAccounts) < maxBatchFindings {
				st.FindingAccounts = append(st.FindingAccounts, r.account)
			}
		}
	}
}

// fail counts a failed account.
func (st *batchState) fail(account, msg string) {
	st.Failed++

	if len(st.Errors) >= maxBatchErrors {
		st.ErrorsDropped++
		return
	}

	if len(msg) > maxBatchErrText {
		msg = msg[:maxBatchErrText] + "..."
	}

	st.Errors = append(st.Errors, batchError{Account: account, Error: msg})
}

// runAccounts runs the kind on the accounts, b.parallel at a time. A
// fatal result stops starting new ones and is returned once the running
// ones ended.
func (b *batch) runAccounts(ctx context.Context, entries []model.IndexEntry) ([]acctResult, error) {
	results := make([]acctResult, len(entries))

	var fatal firstError

	b.forEach(ctx, len(entries), &fatal, func(i int) {
		results[i] = b.account(ctx, entries[i])
		fatal.set(results[i].fatal)
	})

	if err := fatal.get(); err != nil {
		return nil, err
	}

	for _, r := range results {
		metrics.BatchAccounts.WithLabelValues(b.sub.Name(), r.out).Inc()
	}

	return results, nil
}

// forEach calls fn for 0..n-1, b.parallel at a time, and stops starting
// new calls once fatal is set or ctx ended.
func (b *batch) forEach(ctx context.Context, n int, fatal *firstError, fn func(i int)) {
	sem := make(chan struct{}, b.parallel)

	var wg sync.WaitGroup

	for i := range n {
		if fatal.get() != nil {
			break
		}

		if err := ctx.Err(); err != nil {
			fatal.set(err)
			break
		}

		sem <- struct{}{}

		wg.Add(1)

		go func() {
			defer wg.Done()
			defer func() { <-sem }()

			fn(i)
		}()
	}

	wg.Wait()
}

// firstError keeps the first error set.
type firstError struct {
	mu  sync.Mutex
	err error
}

func (f *firstError) set(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.err == nil {
		f.err = err
	}
}

func (f *firstError) get() error {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.err
}

// isFatal tells the errors that stop the whole batch (the lease is lost,
// a cancel was requested, the job's context ended) from those of one
// account.
func isFatal(ctx context.Context, err error) bool {
	return errors.Is(err, ErrLeaseLost) || errors.Is(err, context.Canceled) || ctx.Err() != nil
}

// account runs the kind on one account.
func (b *batch) account(ctx context.Context, e model.IndexEntry) acctResult {
	r := acctResult{account: e.ID, out: outFailed}

	entry, _ := model.DecodeAccountEntry(e.Value)

	switch gone, err := b.gone(e, entry); {
	case err != nil:
		r.err = err.Error()
		return r
	case gone:
		r.out = outGone
		return r
	}

	if err := b.run.Fence(); err != nil {
		r.fatal = err
		return r
	}

	u, err := b.run.Env.Accounts.User(e.ID)
	if err != nil {
		r.err = err.Error()
		return r
	}
	defer u.Close()

	if b.idle(u, entry) {
		r.out = outSkipped
		return r
	}

	if !b.inWindow() {
		r.out = outLater
		return r
	}

	actx, unlock, err := b.lock(ctx, u)

	switch {
	case err == nil:
		defer unlock()

		return b.runOne(ctx, actx, u, r)
	case errors.Is(err, model.ErrImportLocked):
		r.out = outDeferred
	case isFatal(ctx, err):
		r.fatal = err
	default:
		r.err = err.Error()
	}

	return r
}

// gone reports whether the account was purged: an entry (or a deferred
// account without one) that is purging and has no registry any more.
// Purging with a registry counts as active (the purge failed, or has not
// reached its delete phase).
func (b *batch) gone(e model.IndexEntry, entry model.AccountEntry) (bool, error) {
	if e.Value != nil && entry.State != model.AccountPurging {
		return false, nil
	}

	reg, err := b.dir.HasRegistry(e.ID)

	return !reg && err == nil, err
}

// runOne runs the kind on an opened (and, for an exclusive kind,
// locked) account.
func (b *batch) runOne(ctx, actx context.Context, u model.User, r acctResult) acctResult {
	sr := b.run.subRun(r.account, u, b.sub.Name(), b.p.Params)
	sr.window = b.window
	prober, pre, preErr := b.preState(u)

	res, err := runKind(actx, b.sub, sr)
	if err != nil {
		b.clearAfterError(ctx, u, sr, err)
		return failed(ctx, actx, err, r)
	}

	if rep, ok := res.(model.FTSLayoutReport); ok && (rep.Unsupported || rep.Deferred != "") {
		return layoutOutcome(rep, r)
	}

	out, _ := res.(model.ScrubOutcome)
	if out != nil {
		r.findings = out.Findings()
	}

	if partial := sr.partialErrors(); len(partial) > 0 {
		r.err = strings.Join(partial, "; ")
		return r
	}

	r.out = outDone

	if prober != nil && out != nil {
		b.record(r.account, prober, pre, preErr, out)
	}

	return r
}

// clearAfterError removes the unfinished conversion of a full-text run
// of the account that failed: one that panicked cannot have cleared it
// (its own clean-up sees a nil error then, R2.5). Not after an error
// that stops the batch (a lost lease: the successor clears it).
func (b *batch) clearAfterError(ctx context.Context, u model.User, sr *Run, err error) {
	if !buildsLayout(b.sub.Name()) || isFatal(ctx, err) {
		return
	}

	if lay, ok := u.(model.FTSLayouter); ok {
		if _, cerr := lay.ClearFTSLayoutNext(sr.layoutToken()); cerr != nil {
			b.run.Env.Log.Warn("batch: unfinished conversion not cleared", "account", sr.Job.Account, "err", cerr)
		}
	}
}

// layoutOutcome is the outcome of an online run that ended without a
// change: unsupported (no layout 3 index), deferred (busy: tried again
// at the end), later (outside the window), or failed (window_too_small).
func layoutOutcome(rep model.FTSLayoutReport, r acctResult) acctResult {
	switch {
	case rep.Unsupported:
		r.out = outUnsupported
	case rep.Deferred == model.FTSDeferredBusy:
		r.out = outDeferred
	case rep.Deferred == model.FTSDeferredWindow:
		r.out = outLater
	case rep.Deferred == model.FTSDeferredMarkOff:
		r.err = "the cluster mark fts/online was removed after the plan; plan the batch again (it runs offline then)"
	default:
		r.err = "window_too_small: the run's estimate is longer than the reshard window; run the account with anytime, or widen the window"
	}

	return r
}

// preState reads the scrub state of the account before a recorded scrub
// (prober nil: nothing is recorded).
func (b *batch) preState(u model.User) (model.ScrubProber, model.ScrubState, error) {
	prober, ok := u.(model.ScrubProber)
	if !b.records || !ok {
		return nil, model.ScrubState{}, nil
	}

	pre, err := prober.ScrubState()

	return prober, pre, err
}

// failed classifies the error of a run: the account's lock was lost, the
// whole batch has to stop, or the account failed.
func failed(ctx, actx context.Context, err error, r acctResult) acctResult {
	switch {
	case actx.Err() != nil && ctx.Err() == nil:
		r.err = "maintenance lock lost: " + err.Error()
	case isFatal(ctx, err):
		r.fatal = err
	default:
		r.err = err.Error()
	}

	return r
}

// record stores the scrub record of an account after its scrub. The
// state is read after the scrub's own writes; a run that found nothing
// but saw the state move (a concurrent change) does not count as
// complete, so the next batch looks again.
func (b *batch) record(account string, prober model.ScrubProber, pre model.ScrubState, preErr error, out model.ScrubOutcome) {
	post, err := prober.ScrubState()
	if err != nil {
		return
	}

	complete := out.Complete() && preErr == nil && (out.Findings() > 0 || pre == post)
	rec := model.AccountScrub{At: b.run.m.now().UTC(), State: post, Complete: complete, Findings: out.Findings()}

	b.ixMu.Lock()
	err = model.SetAccountScrub(b.ix, account, rec)
	b.ixMu.Unlock()

	if err != nil {
		b.run.Env.Log.Warn("batch: scrub record not stored", "account", account, "err", err)
	}
}

// idle reports whether a batch scrub with skip_idle may leave the
// account alone (model.ScrubIdle). The checks that need no read come
// first.
func (b *batch) idle(u model.User, entry model.AccountEntry) bool {
	if !b.p.SkipIdle || b.sub.Name() != KindScrub {
		return false
	}

	last, ok := entry.ScrubOf()
	now := b.run.m.now()
	interval := b.run.m.cfg.ScrubInterval

	if !ok || !last.Complete || last.Findings > 0 || interval > 0 && now.Sub(last.At) >= interval {
		return false
	}

	prober, ok := u.(model.ScrubProber)
	if !ok {
		return false
	}

	state, err := prober.ScrubState()
	if err != nil || state != last.State {
		return false
	}

	pending, err := prober.ScrubPending()
	if err != nil {
		return false
	}

	return model.ScrubIdle(last, true, state, pending, now, interval)
}

// lock takes the maintenance lock of an exclusive kind on the account
// and keeps it renewed; the returned context ends when the lock is lost.
// A non-exclusive kind gets ctx back. An account another job or an
// import holds is refused with model.ErrImportLocked; one an ended purge
// left purging with ErrLeftPurging.
func (b *batch) lock(ctx context.Context, u model.User) (context.Context, func(), error) {
	if !exclusive(b.sub, b.p.Params) {
		return ctx, func() {}, nil
	}

	m, jobID := b.run.m, b.run.Job.ID

	if err := lockAccount(u, jobID, 2*m.cfg.LeaseTTL); err != nil {
		if errors.Is(err, model.ErrAccountPurging) && m.leftPurging(u, jobID) {
			return nil, nil, ErrLeftPurging
		}

		return nil, nil, err
	}

	clearForeignLayout(u, b.run.layoutToken(), b.run.Env.Log)
	clearForeignRecount(u, b.run.Env.Log)

	actx, cancel := context.WithCancel(ctx)
	stop := b.keepLock(u, cancel)

	unlock := func() {
		stop()
		unlockAccount(u, jobID)
		cancel()
	}

	// Writers that opened the account before the lock notice it within
	// the settle time (see Config.Settle).
	if m.cfg.Settle > 0 {
		select {
		case <-actx.Done():
			unlock()

			if err := ctx.Err(); err != nil {
				return nil, nil, err
			}

			return nil, nil, errors.New("maintenance lock lost")
		case <-time.After(m.cfg.Settle):
		}
	}

	return actx, unlock, nil
}

// keepLock renews the maintenance lock of the account every third of
// the lease and calls lost when a renewal fails; the returned function
// stops it.
func (b *batch) keepLock(u model.User, lost func()) func() {
	m, jobID := b.run.m, b.run.Job.ID
	stop, done := make(chan struct{}), make(chan struct{})

	go func() {
		defer close(done)

		tick := time.NewTicker(m.cfg.LeaseTTL / 3)
		defer tick.Stop()

		for {
			select {
			case <-stop:
				return
			case <-tick.C:
			}

			if err := renewAccount(u, jobID, 2*m.cfg.LeaseTTL); err != nil {
				b.run.Env.Log.Warn("batch: maintenance lock lost", "err", err)
				lost()

				return
			}
		}
	}()

	return func() {
		close(stop)
		<-done
	}
}
