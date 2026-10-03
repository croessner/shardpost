package jmap

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"sort"
	"sync"
	"time"

	"shardpost/internal/auth"
	"shardpost/internal/kvindex"
	"shardpost/internal/metrics"
	"shardpost/internal/model"
)

// Serving one partition (stage B18a, §4.6): the holder reads its shard
// of the hint index of the subscriptions every scan, in pages, and
// reads a subscription record only when the checksum in its index
// value changed, when the value has none (an older front-end wrote
// it), or in the hourly full pass; it watches the lock object of the
// partition, which the front-ends notify with the account as payload
// when a subscription changes, and reads that one record then. It
// starts and stops the account watchers of the partition to match the
// verified subscriptions, at a bounded rate, and keeps the backoffs of
// the subscriptions and the checks of the accounts of this partition
// alone.

// partitionService is the state of serving one partition.
type partitionService struct {
	w *worker
	p *partition

	mu       sync.Mutex
	subs     map[string][]*pushRecord // account -> verified subscriptions
	watching map[string]*watcher
	backoff  map[string]time.Time // subscription id -> no push before
	checked  map[string]*accountCheck
	// sums is the checksum of each account's record as last read (0:
	// read on every scan); known are the accounts the shard listed.
	sums  map[string]uint64
	known map[string]bool
	// fullCursor is where the full pass stands in the sorted accounts;
	// lastFull when it last went round.
	fullCursor string
	paused     bool
	// first is the first pass of the watchers started by the first
	// scan (the hand-over measurement).
	first   sync.WaitGroup
	started bool
	wg      sync.WaitGroup
}

// watcher is a running account watcher.
type watcher struct {
	cancel context.CancelFunc
	// noSave is set when the watcher is to end without saving: the
	// account is gone (its state record goes with it).
	noSave bool
	mu     sync.Mutex
	// firstDone is set once the watcher's first pass is over.
	firstDone bool
	first     bool
}

// stopSilently marks the watcher as one that does not save and stops
// it.
func (wt *watcher) stopSilently() {
	wt.mu.Lock()
	wt.noSave = true
	wt.mu.Unlock()

	wt.cancel()
}

// saves reports whether the watcher may save.
func (wt *watcher) saves() bool {
	wt.mu.Lock()
	defer wt.mu.Unlock()

	return !wt.noSave
}

func newPartitionService(w *worker, p *partition) *partitionService {
	return &partitionService{w: w, p: p, subs: map[string][]*pushRecord{}, watching: map[string]*watcher{},
		backoff: map[string]time.Time{}, checked: map[string]*accountCheck{}, sums: map[string]uint64{}, known: map[string]bool{}}
}

// servePartition serves p until ctx ends: the first scan reads the
// shard and starts the watchers, the payload watch on the lock object
// wakes it for single accounts, and the scan goes round on its
// interval.
func (w *worker) servePartition(ctx context.Context, p *partition) {
	defer close(p.done)

	svc := newPartitionService(w, p)

	w.mu.Lock()
	p.svc = svc
	w.mu.Unlock()

	defer svc.stopAll()

	wake := svc.watchLock(ctx)

	svc.scan(ctx, true)

	go svc.measureHandover()

	scan := time.NewTicker(w.o.Scan)
	defer scan.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-scan.C:
			svc.scan(ctx, false)
		case data := <-wake:
			svc.woken(ctx, data)
		}
	}
}

// measureHandover observes the time from the take to the end of the
// first pass of every watcher the first scan started.
func (svc *partitionService) measureHandover() {
	svc.first.Wait()

	svc.mu.Lock()
	started := svc.started
	svc.mu.Unlock()

	if started {
		metrics.PushHandover.Observe(time.Since(svc.p.takenAt).Seconds())
	}
}

// watchLock watches the lock object of the partition for the payload
// notifies of the front-ends; the watch is set up again when the store
// fails it.
func (svc *partitionService) watchLock(ctx context.Context) <-chan []byte {
	out := make(chan []byte, 16)

	go func() {
		for {
			watch, err := svc.w.omap.WatchPayload(pushLockOID(svc.p.n))
			if err != nil {
				svc.w.o.Log.Warn("push worker: watch the partition", "partition", svc.p.n, "err", err)
			} else {
				svc.followLock(ctx, watch, out)
				watch.Close()

				// What the broken watch may have missed.
				select {
				case out <- nil:
				default:
				}
			}

			select {
			case <-ctx.Done():
				return
			case <-time.After(rewatchWait()):
			}
		}
	}()

	return out
}

// followLock forwards the events of one watch until it breaks or ctx
// ends.
func (svc *partitionService) followLock(ctx context.Context, watch model.PayloadWatch, out chan<- []byte) {
	for {
		select {
		case <-ctx.Done():
			return
		case data := <-watch.Events():
			select {
			case out <- data:
			case <-ctx.Done():
				return
			}
		case <-watch.Errors():
			return
		}
	}
}

// pushWake is the payload of a front-end's notify: the account whose
// subscriptions changed.
type pushWake struct {
	Account string `json:"a"`
}

// woken acts on a payload notify: the account named is read again; an
// empty payload (an overflow, a watch set up again) means a scan.
func (svc *partitionService) woken(ctx context.Context, data []byte) {
	var wk pushWake
	if len(data) == 0 || json.Unmarshal(data, &wk) != nil || wk.Account == "" {
		svc.scan(ctx, false)

		return
	}

	if PushPartition(wk.Account, svc.w.n) != svc.p.n {
		return
	}

	svc.refreshAccount(ctx, wk.Account)

	// A verification is queued with the notify on partition 0.
	if svc.p.n == 0 {
		select {
		case svc.w.verifyKick <- struct{}{}:
		default:
		}
	}
}

// refreshAccount reads one account's record and brings its watcher in
// line: started when it has verified subscriptions, stopped (without
// saving) when the record is gone.
func (svc *partitionService) refreshAccount(ctx context.Context, account string) {
	raw, err := svc.w.o.KV.Get(pushAcctKey(account))

	switch {
	case errors.Is(err, model.ErrNotFound):
		svc.mu.Lock()
		delete(svc.subs, account)
		delete(svc.sums, account)
		delete(svc.known, account)
		wt := svc.watching[account]
		delete(svc.watching, account)
		svc.mu.Unlock()

		if wt != nil {
			wt.stopSilently()
		}

		return
	case err != nil:
		svc.w.o.Log.Warn("push worker: read subscriptions", "account", account, "err", err)

		return
	}

	pa := &pushAccount{}
	if json.Unmarshal(raw, pa) != nil || pa.Account != account {
		return
	}

	verified := svc.w.sortOut(pa)

	svc.mu.Lock()
	svc.sums[account], svc.known[account] = kvindex.PushSum(raw), true

	if len(verified) > 0 {
		svc.subs[account] = verified
	} else {
		delete(svc.subs, account)
	}

	wt, watching := svc.watching[account]
	svc.mu.Unlock()

	switch {
	case len(verified) == 0 && watching:
		// No verified subscription left: the watcher ends and saves
		// (the account is still there).
		svc.mu.Lock()
		delete(svc.watching, account)
		svc.mu.Unlock()
		wt.cancel()
	case len(verified) > 0 && !watching:
		svc.startWatcher(ctx, account, false)
	}
}

// scan reads the shard of the partition (or lists, without an index in
// read mode), reads the records that changed, drops the subscriptions
// that are over, and starts and stops the watchers to match the
// verified ones. first marks the scan after the take.
func (svc *partitionService) scan(ctx context.Context, first bool) {
	w := svc.w

	var (
		byAccount map[string][]*pushRecord
		ids       map[string]bool
		ok        bool
	)

	if w.ix != nil && w.kv.IndexMode() == model.KVIndexRead {
		byAccount, ids, ok = svc.scanShard(ctx)
	} else {
		byAccount, ids, ok = svc.scanListing()
	}

	if !ok {
		return
	}

	for _, account := range svc.goneAccounts(ctx, byAccount) {
		w.dropAccount(account)
		delete(byAccount, account)

		svc.mu.Lock()
		delete(svc.sums, account)
		delete(svc.known, account)
		svc.mu.Unlock()
	}

	svc.reconcile(ctx, byAccount, first)
	svc.prune(ids, byAccount)
}

// scanShard reads the shard of the partition from the hint index. It
// returns the verified subscriptions per account and the ids of every
// subscription seen; ok is false when nothing could be read (the
// watchers stay as they are).
func (svc *partitionService) scanShard(ctx context.Context) (map[string][]*pushRecord, map[string]bool, bool) {
	w := svc.w

	head, err := w.ix.Head()
	if err != nil {
		w.o.Log.Warn("push worker: read the subscription index head", "partition", svc.p.n, "err", err)

		return nil, nil, false
	}

	if head.Phase != model.IndexPhaseStable || head.Shards != w.n {
		if !svc.paused {
			w.o.Log.Warn("push worker: the subscription index is being resharded; the scan of the partition pauses",
				"partition", svc.p.n, "phase", head.Phase, "shards", head.Shards)
		}

		svc.paused = true

		return nil, nil, false
	}

	svc.paused = false

	type entry struct {
		account string
		sum     uint64
	}

	var entries []entry

	after := ""

	for {
		page, more, err := w.ix.ListShard(svc.p.n, after, pushScanPage)
		if err != nil {
			w.o.Log.Warn("push worker: read the subscription index", "partition", svc.p.n, "err", err)

			return nil, nil, false
		}

		for _, e := range page {
			after = e.ID

			account, err := url.PathUnescape(e.ID)
			if err != nil {
				continue
			}

			var v kvindex.PushValue

			_ = json.Unmarshal(e.Value, &v)

			entries = append(entries, entry{account: account, sum: v.Sum})
		}

		if !more {
			break
		}
	}

	accounts := make([]string, 0, len(entries))
	for _, e := range entries {
		accounts = append(accounts, e.account)
	}

	full := svc.fullPass(accounts)

	byAccount := map[string][]*pushRecord{}
	ids := map[string]bool{}
	known := map[string]bool{}

	for _, e := range entries {
		if ctx.Err() != nil {
			return nil, nil, false
		}

		known[e.account] = true

		svc.mu.Lock()
		prev, had := svc.sums[e.account]
		cur := svc.subs[e.account]
		svc.mu.Unlock()

		cause := ""

		switch {
		case !had, e.sum != 0 && e.sum != prev:
			cause = "changed"
		case e.sum == 0:
			cause = "nosum"
		case full[e.account]:
			cause = "full"
		}

		if cause == "" {
			// Unchanged: what the last read gave still holds.
			if len(cur) > 0 {
				byAccount[e.account] = cur
			}

			for _, rec := range cur {
				ids[rec.ID] = true
			}

			continue
		}

		metrics.PushScanGets.WithLabelValues(cause).Inc()

		raw, err := w.o.KV.Get(pushAcctKey(e.account))

		switch {
		case errors.Is(err, model.ErrNotFound):
			// An entry without record: the index is refreshed, which
			// removes it (kvindex).
			if r, ok := w.o.KV.(interface{ Refresh(string) }); ok {
				r.Refresh(pushAcctKey(e.account))
			}

			delete(known, e.account)

			continue
		case err != nil:
			w.o.Log.Warn("push worker: read subscriptions", "account", e.account, "err", err)

			if len(cur) > 0 {
				byAccount[e.account] = cur
			}

			continue
		}

		svc.mu.Lock()
		svc.sums[e.account] = kvindex.PushSum(raw)
		svc.mu.Unlock()

		pa := &pushAccount{}
		if json.Unmarshal(raw, pa) != nil || pa.Account == "" {
			continue
		}

		for _, rec := range pa.Subs {
			ids[rec.ID] = true
		}

		if verified := w.sortOut(pa); len(verified) > 0 {
			byAccount[e.account] = verified
		}
	}

	svc.mu.Lock()
	for account := range svc.sums {
		if !known[account] {
			delete(svc.sums, account)
		}
	}

	svc.known = known
	svc.mu.Unlock()

	return byAccount, ids, true
}

// fullPass picks the accounts of the full pass that are due in this
// scan: a share of them each round, in order, so that the pass goes
// round once per full scan interval (none without one). It is the
// safety net for index values whose checksum does not tell: an older
// front-end's, or a wrong one.
func (svc *partitionService) fullPass(accounts []string) map[string]bool {
	o := svc.w.o
	if o.FullScan <= 0 || len(accounts) == 0 {
		return nil
	}

	share := int(float64(len(accounts))*float64(o.Scan)/float64(o.FullScan) + 1)

	sorted := append([]string(nil), accounts...)
	sort.Strings(sorted)

	out := map[string]bool{}

	svc.mu.Lock()
	cursor := svc.fullCursor
	svc.mu.Unlock()

	for _, account := range sorted {
		if len(out) == share {
			break
		}

		if account > cursor {
			out[account] = true
			cursor = account
		}
	}

	if len(out) < share {
		cursor = "" // the end: the next pass starts over
	}

	svc.mu.Lock()
	svc.fullCursor = cursor
	svc.mu.Unlock()

	return out
}

// scanListing is the scan without a usable index: the whole prefix is
// listed and filtered to the partition (the fallback for a cluster
// without the admin store backfill, warned about once per scan
// interval).
func (svc *partitionService) scanListing() (map[string][]*pushRecord, map[string]bool, bool) {
	w := svc.w

	w.mu.Lock()
	if time.Since(w.listedWarned) >= w.o.Scan {
		w.listedWarned = time.Now()

		w.o.Log.Warn("push worker: the admin store has no usable subscription index (admin.kv_index read with a completed backfill); "+
			"every scan lists the admin store", "event", "push_scan_listing")
	}
	w.mu.Unlock()

	entries, err := listAll(w.o.KV, pushAcctPrefix)
	if err != nil {
		w.o.Log.Warn("push worker: list subscriptions", "err", err)

		return nil, nil, false
	}

	byAccount := map[string][]*pushRecord{}
	ids := map[string]bool{}

	for _, e := range entries {
		pa := &pushAccount{}
		if json.Unmarshal(e.Value, pa) != nil || pa.Account == "" || PushPartition(pa.Account, w.n) != svc.p.n {
			continue
		}

		for _, rec := range pa.Subs {
			ids[rec.ID] = true
		}

		if verified := w.sortOut(pa); len(verified) > 0 {
			byAccount[pa.Account] = verified
		}
	}

	return byAccount, ids, true
}

// listAll reads every record under a prefix.
func listAll(kv model.AdminKV, prefix string) ([]model.KVEntry, error) {
	var out []model.KVEntry

	after := ""

	for {
		page, err := kv.List(prefix, after, pushScanPage)
		if err != nil {
			return nil, err
		}

		out = append(out, page...)

		if len(page) < pushScanPage {
			return out, nil
		}

		after = page[len(page)-1].Key
	}
}

// sortOut drops the expired subscriptions of an account and those made
// under another VAPID key, and returns the verified ones.
func (w *worker) sortOut(pa *pushAccount) []*pushRecord {
	now := time.Now()

	var verified []*pushRecord

	for _, rec := range pa.Subs {
		switch {
		case !rec.Expires.After(now):
			w.drop(rec, "expired")
		case w.o.VAPIDPublicKey != "" && rec.VAPIDKey != w.o.VAPIDPublicKey:
			w.o.Log.Warn("push worker: subscription made under another VAPID key", "account", rec.Account,
				"id", rec.ID, "its key", keyPrefix(rec.VAPIDKey), "worker key", keyPrefix(w.o.VAPIDPublicKey))
			w.drop(rec, "made under another VAPID key")
		case rec.Verified:
			verified = append(verified, rec)
		}
	}

	return verified
}

// keyPrefix is the start of a key, enough to tell two apart in a log.
func keyPrefix(k string) string {
	if len(k) > 12 {
		return k[:12]
	}

	return k
}

// goneAccounts asks Auth about a bounded number of accounts per scan and
// returns those it has denied deniedToDrop times in a row. When most of
// the accounts asked in one scan are denied, the backend is suspected —
// a worker pointed at the wrong one would deny every account — and no
// account is counted.
func (svc *partitionService) goneAccounts(ctx context.Context, byAccount map[string][]*pushRecord) []string {
	if svc.w.o.Auth == nil {
		return nil
	}

	asked, denied := svc.askAccounts(ctx, byAccount)

	if len(denied) >= suspectDenials && 2*len(denied) > len(asked) {
		svc.w.o.Log.Error("push worker: the authentication backend denies most accounts; no subscription is dropped",
			"asked", len(asked), "denied", len(denied))

		return nil
	}

	var gone []string

	svc.mu.Lock()
	defer svc.mu.Unlock()

	for _, account := range denied {
		if c := svc.checked[account]; c != nil {
			if c.denied++; c.denied >= deniedToDrop {
				gone = append(gone, account)
			}
		}
	}

	return gone
}

// askAccounts looks up the accounts that are due, at most checksPerScan
// of them, and returns those asked and those denied.
func (svc *partitionService) askAccounts(ctx context.Context, byAccount map[string][]*pushRecord) (asked, denied []string) {
	svc.mu.Lock()

	var due []string

	for account := range byAccount {
		c := svc.checked[account]
		if c == nil {
			c = &accountCheck{}
			svc.checked[account] = c
		}

		if len(due) >= checksPerScan || time.Since(c.at) < svc.w.o.AccountCheck {
			continue
		}

		c.at = time.Now()

		due = append(due, account)
	}
	svc.mu.Unlock()

	for _, account := range due {
		asked = append(asked, account)

		switch _, err := svc.w.o.Auth.Lookup(ctx, account, "jmap", nil); {
		case errors.Is(err, auth.ErrInvalid):
			denied = append(denied, account)
		case err == nil:
			svc.mu.Lock()
			if c := svc.checked[account]; c != nil {
				c.denied = 0
			}
			svc.mu.Unlock()
		}
	}

	return asked, denied
}

// reconcile starts a watcher for every account with verified
// subscriptions and stops the others; the accounts that are gone from
// the shard stop without saving. The starts are rate-limited; first
// marks the scan after the take, whose watchers' first pass is the
// hand-over.
func (svc *partitionService) reconcile(ctx context.Context, byAccount map[string][]*pushRecord, first bool) {
	svc.mu.Lock()
	svc.subs = byAccount

	var stop []*watcher

	for account, wt := range svc.watching {
		if _, ok := byAccount[account]; !ok {
			stop = append(stop, wt)
			delete(svc.watching, account)
		}
	}

	var start []string

	for account := range byAccount {
		if _, ok := svc.watching[account]; !ok {
			start = append(start, account)
		}
	}
	svc.mu.Unlock()

	for _, wt := range stop {
		wt.stopSilently()
	}

	sort.Strings(start)

	for _, account := range start {
		if !svc.w.limiter.wait(ctx) {
			return
		}

		svc.startWatcher(ctx, account, first)
	}
}

// startWatcher starts the watcher of an account (unless one runs).
func (svc *partitionService) startWatcher(ctx context.Context, account string, first bool) {
	svc.mu.Lock()
	defer svc.mu.Unlock()

	if _, ok := svc.watching[account]; ok || ctx.Err() != nil {
		return
	}

	actx, cancel := context.WithCancel(ctx)
	wt := &watcher{cancel: cancel, first: first}
	svc.watching[account] = wt
	svc.wg.Add(1)

	if svc.w.hooks != nil {
		svc.w.hooks.watcherStarts.Add(1)
	}

	if first {
		svc.started = true
		svc.first.Add(1)
	}

	go svc.w.watchAccount(actx, svc, account, wt)
}

// firstPassDone is told by a watcher that its first pass is over.
func (svc *partitionService) firstPassDone(wt *watcher) {
	wt.mu.Lock()
	defer wt.mu.Unlock()

	if wt.first && !wt.firstDone {
		wt.firstDone = true
		svc.first.Done()
	}
}

// forget takes an account watcher out of the table, if it is still the
// one there.
func (svc *partitionService) forget(account string, self *watcher) {
	svc.mu.Lock()
	defer svc.mu.Unlock()

	if svc.watching[account] == self {
		delete(svc.watching, account)
	}
}

// prune forgets the backoff of subscriptions and the checks of accounts
// a scan of this partition no longer finds, so the long-running worker
// does not keep them after the subscription or the account is gone.
// Only this partition's are touched: the other partitions of the pod
// keep theirs.
func (svc *partitionService) prune(ids map[string]bool, byAccount map[string][]*pushRecord) {
	svc.mu.Lock()
	defer svc.mu.Unlock()

	for id := range svc.backoff {
		if !ids[id] {
			delete(svc.backoff, id)
		}
	}

	for account := range svc.checked {
		if _, ok := byAccount[account]; !ok {
			delete(svc.checked, account)
		}
	}
}

// stopAll stops every account watcher of the partition and waits for
// them (they end their rounds and save).
func (svc *partitionService) stopAll() {
	svc.mu.Lock()

	for account, wt := range svc.watching {
		wt.cancel()
		delete(svc.watching, account)
	}

	svc.mu.Unlock()
	svc.wg.Wait()
}

// subsOf are the verified subscriptions of an account.
func (svc *partitionService) subsOf(account string) []*pushRecord {
	svc.mu.Lock()
	defer svc.mu.Unlock()

	return append([]*pushRecord(nil), svc.subs[account]...)
}

// backoffOf is when a subscription may be pushed to again.
func (svc *partitionService) backoffOf(id string) time.Time {
	svc.mu.Lock()
	defer svc.mu.Unlock()

	return svc.backoff[id]
}

// hold records that a subscription is to be left alone until a time.
func (svc *partitionService) hold(id string, until time.Time) {
	svc.mu.Lock()
	defer svc.mu.Unlock()

	svc.backoff[id] = until
}

// leaseValid reports whether the partition's lease may still be worked
// under (the send path checks it before every push).
func (svc *partitionService) leaseValid() bool {
	svc.w.mu.Lock()
	l := svc.p.lease
	svc.w.mu.Unlock()

	return l != nil && svc.w.leaseValid(l)
}
