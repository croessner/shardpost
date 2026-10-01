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
var FanOutKinds = []string{KindIndexCheck, KindScrub, KindFTSRebuild, KindAttachments}

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
		return kind.Exclusive()
	}

	sub, _, err := m.batchOf(params)

	return err == nil && sub.Exclusive()
}

// checkBatchParams vets the per-account parameters of the kind.
func checkBatchParams(p BatchParams) error {
	if p.SkipIdle && p.Kind != KindScrub {
		return fmt.Errorf("%w: skip_idle is for scrub only", ErrInvalidParams)
	}

	if p.Kind == KindScrub {
		_, err := parseScrub(p.Params)
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
	Parallel  int             `json:"parallel"`
	Shards    int             `json:"directory_shards"`
	Backfill  string          `json:"directory_backfill"`
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

	head, err := m.directoryHead()
	if err != nil {
		return nil, err
	}

	b, err := json.Marshal(batchSummary{Kind: sub.Name(), Params: p.Params, Role: sub.Role(), Exclusive: sub.Exclusive(),
		SkipIdle: p.SkipIdle, Parallel: m.batchParallel(sub.Name()), Shards: head.Shards, Backfill: head.Markers[model.AccountBackfillMarker]})
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
// not started twice.
func (m *Manager) SubmitBatchOnce(p BatchParams, principal string) (*Job, bool, error) {
	if j, err := m.unfinishedBatch(p.Kind); err != nil || j != nil {
		return j, false, err
	}

	plan, err := m.CreateBatchPlan(p, principal)
	if err != nil {
		return nil, false, err
	}

	k, err := m.Kind(KindBatch)
	if err != nil {
		return nil, false, err
	}

	return m.Submit(k, "", nil, plan.ID, principal, "")
}

// unfinishedBatch returns a batch of the kind that has not ended (nil for
// none).
func (m *Manager) unfinishedBatch(kind string) (*Job, error) {
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
					return &page[i], nil
				}
			}

			if next == "" {
				break
			}

			after = next
		}
	}

	return nil, nil
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
	// import held them; Rounds counts the retries at the end.
	Deferred []string `json:"deferred,omitempty"`
	Rounds   int      `json:"rounds,omitempty"`
}

// Outcomes of one account.
const (
	outDone     = "done"
	outSkipped  = "skipped"
	outGone     = "gone"
	outDeferred = "deferred"
	outFailed   = "failed"
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
		parallel: max(run.m.batchParallel(sub.Name()), 1)}

	if sub.Name() == KindScrub {
		classes, err := parseScrub(p.Params)
		if err != nil {
			return nil, err
		}

		b.records = len(classes) == len(model.RepairClasses)
	}

	st := batchState{Kind: sub.Name()}

	run.LoadCheckpoint(&st)

	if err := b.walk(ctx, &st); err != nil {
		return st.result(), err
	}

	if err := b.retryDeferred(ctx, &st); err != nil {
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

// retryDeferred tries the accounts put aside for a lock again, a lease
// period apart, and fails those still locked after the last round.
func (b *batch) retryDeferred(ctx context.Context, st *batchState) error {
	for len(st.Deferred) > 0 && st.Rounds < batchRetryRounds {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(b.run.m.cfg.LeaseTTL):
		}

		entries := make([]model.IndexEntry, 0, len(st.Deferred))

		b.ixMu.Lock()

		for _, a := range st.Deferred {
			v, err := b.ix.Get(a)

			switch {
			case errors.Is(err, model.ErrNotFound):
				entries = append(entries, model.IndexEntry{ID: a}) // gone: account() finds out
			case err != nil:
				b.ixMu.Unlock()
				return fmt.Errorf("account directory: %w", err)
			default:
				entries = append(entries, model.IndexEntry{ID: a, Value: v})
			}
		}

		b.ixMu.Unlock()

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
	prober, pre, preErr := b.preState(u)

	res, err := runKind(actx, b.sub, sr)
	if err != nil {
		return failed(ctx, actx, err, r)
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
	if !b.sub.Exclusive() {
		return ctx, func() {}, nil
	}

	m, jobID := b.run.m, b.run.Job.ID

	if err := lockAccount(u, jobID, 2*m.cfg.LeaseTTL); err != nil {
		if errors.Is(err, model.ErrAccountPurging) && m.leftPurging(u, jobID) {
			return nil, nil, ErrLeftPurging
		}

		return nil, nil, err
	}

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
