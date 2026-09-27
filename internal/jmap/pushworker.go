package jmap

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"math/rand/v2"
	"net/url"
	"strconv"
	"sync"
	"time"

	"shardpost/internal/auth"
	"shardpost/internal/model"
	"shardpost/internal/webpush"
)

// The push worker sends Web Push for the push subscriptions the
// front-ends took (RFC 8620 7.2). It is a process of its own
// ("shardpost push"), because a front-end only runs while a client talks
// to it, and a push subscription exists for the time no client does.
//
// One worker of a deployment works at a time: it holds a lease in the
// admin store, renewed on a clock of its own, and another waits for it to
// lapse. The worker sends the verification of a new subscription, and
// once one is verified it watches that account — through the same
// notification the store sends for IMAP IDLE, and a poll in case one is
// lost — and posts a StateChange when the account changes. The state it
// last pushed is kept per account, so a worker that takes over pushes what
// happened in between instead of starting from "now".

// WorkerOptions configure the push worker.
type WorkerOptions struct {
	Store  Store
	KV     model.AdminKV
	Sender *webpush.Sender
	// Auth, when set, is asked about the accounts that have subscriptions;
	// the subscriptions of an account it denies three times in a row are
	// destroyed (see accountGone).
	Auth auth.Authenticator
	Log  *slog.Logger
	// Instance names this worker in the lease; it has to be unique.
	Instance string
	// VAPIDPublicKey is the key the worker signs with; a subscription made
	// under another key is destroyed (RFC 9749 5).
	VAPIDPublicKey string
	// Scan is how often the subscriptions are read again, Verify how often
	// the verification queue is read, Poll how often a watched account is
	// checked without a notification, Coalesce how long changes are
	// gathered before a push, LeaseTTL how long the lease lasts unrenewed,
	// and AccountCheck how often an account is looked up in Auth.
	Scan, Verify, Poll, Coalesce, LeaseTTL, AccountCheck time.Duration
}

// DefaultWorker are the intervals a configuration that says nothing gets.
var DefaultWorker = WorkerOptions{
	Scan: 30 * time.Second, Verify: 2 * time.Second, Poll: time.Minute, Coalesce: time.Second,
	LeaseTTL: 90 * time.Second, AccountCheck: time.Hour,
}

// Bounds of the account check: accounts looked up per scan, denials in a
// row before an account counts as gone, and the share of denials in one
// scan above which the backend is suspected rather than the accounts.
const (
	checksPerScan   = 20
	deniedToDrop    = 3
	suspectDenials  = 3
	verifyAttempts  = 3
	maxPushStateLag = pushPage
)

// workerDefaults fills in the intervals left at zero.
func workerDefaults(o WorkerOptions) WorkerOptions {
	d := DefaultWorker
	for _, p := range []struct{ v, d *time.Duration }{
		{&o.Scan, &d.Scan}, {&o.Verify, &d.Verify}, {&o.Poll, &d.Poll}, {&o.Coalesce, &d.Coalesce},
		{&o.LeaseTTL, &d.LeaseTTL}, {&o.AccountCheck, &d.AccountCheck},
	} {
		if *p.v <= 0 {
			*p.v = *p.d
		}
	}

	if o.Log == nil {
		o.Log = slog.Default()
	}

	return o
}

// check reports what the options lack.
func (o *WorkerOptions) check() error {
	if o.Store == nil || o.KV == nil || o.Sender == nil || o.Instance == "" {
		return errors.New("push worker: store, admin store, sender and instance are required")
	}

	return nil
}

// pushLeaseKey is the admin store key of the worker lease.
const pushLeaseKey = "jmap/push/lease"

// pushLease is the lease record.
type pushLease struct {
	Holder string    `json:"holder"`
	Until  time.Time `json:"until"`
}

// accountCheck is what the worker knows about an account's standing in
// the authentication backend.
type accountCheck struct {
	at     time.Time
	denied int
}

// watcher is a running account watcher.
type watcher struct{ cancel context.CancelFunc }

// worker is a running push worker.
type worker struct {
	o          WorkerOptions
	mu         sync.Mutex
	subs       map[string][]*pushRecord // account -> verified subscriptions
	watching   map[string]*watcher
	backoff    map[string]time.Time // subscription id -> no push before
	checked    map[string]*accountCheck
	leaseUntil time.Time
	wg         sync.WaitGroup
}

// RunPushWorker runs the push worker until ctx ends.
func RunPushWorker(ctx context.Context, o WorkerOptions) error {
	o = workerDefaults(o)
	if err := o.check(); err != nil {
		return err
	}

	w := &worker{o: o, subs: map[string][]*pushRecord{}, watching: map[string]*watcher{},
		backoff: map[string]time.Time{}, checked: map[string]*accountCheck{}}

	renew := time.NewTicker(o.LeaseTTL / 3)
	defer renew.Stop()

	scan := time.NewTicker(o.Scan)
	defer scan.Stop()

	verify := time.NewTicker(o.Verify)
	defer verify.Stop()

	leader := w.renew(false)
	if leader {
		w.scan(ctx)
		w.verify(ctx)
	}

	for {
		var job func(context.Context)

		select {
		case <-ctx.Done():
			w.stopAll()
			w.release()

			return nil
		case <-renew.C:
			leader = w.renew(leader)
		case <-scan.C:
			job = w.scan
		case <-verify.C:
			job = w.verify
		}

		if leader && job != nil {
			job(ctx)
		}
	}
}

// renew takes or renews the lease and reports whether this worker may
// work. A store that cannot be reached does not end the work at once:
// the worker goes on while the lease it last wrote is valid.
func (w *worker) renew(held bool) bool {
	now := time.Now()

	err := w.o.KV.Update(pushLeaseKey, func(cur []byte) ([]byte, error) {
		var l pushLease
		if cur != nil && json.Unmarshal(cur, &l) == nil && l.Holder != w.o.Instance && l.Until.After(now) {
			return nil, errLeaseHeld
		}

		return json.Marshal(pushLease{Holder: w.o.Instance, Until: now.Add(w.o.LeaseTTL)})
	})

	switch {
	case err == nil:
		if !held {
			w.o.Log.Info("push worker: took the lease", "instance", w.o.Instance)
		}

		w.leaseUntil = now.Add(w.o.LeaseTTL)

		return true
	case errors.Is(err, errLeaseHeld):
		if held {
			w.o.Log.Warn("push worker: lost the lease", "instance", w.o.Instance)
			w.stopAll()
		}

		return false
	}

	w.o.Log.Warn("push worker: lease", "err", err)

	if held && now.Before(w.leaseUntil) {
		return true
	}

	if held {
		w.stopAll()
	}

	return false
}

// errLeaseHeld aborts a lease update another worker holds.
var errLeaseHeld = errors.New("the lease is held by another worker")

// release gives the lease up on shutdown, so a successor need not wait.
func (w *worker) release() {
	_ = w.o.KV.Update(pushLeaseKey, func(cur []byte) ([]byte, error) {
		var l pushLease
		if cur == nil || json.Unmarshal(cur, &l) != nil || l.Holder != w.o.Instance {
			return nil, errLeaseHeld
		}

		return nil, nil
	})
}

// listAll reads every record under a prefix.
func listAll(kv model.AdminKV, prefix string) ([]model.KVEntry, error) {
	var out []model.KVEntry

	after := ""

	for {
		page, err := kv.List(prefix, after, 256)
		if err != nil {
			return nil, err
		}

		out = append(out, page...)

		if len(page) < 256 {
			return out, nil
		}

		after = page[len(page)-1].Key
	}
}

// scan reads the subscriptions, drops the ones that are over, and starts
// and stops the account watchers to match the verified ones.
func (w *worker) scan(ctx context.Context) {
	entries, err := listAll(w.o.KV, pushAcctPrefix)
	if err != nil {
		w.o.Log.Warn("push worker: list subscriptions", "err", err)

		return
	}

	byAccount := map[string][]*pushRecord{}
	ids := map[string]bool{}

	for _, e := range entries {
		pa := &pushAccount{}
		if json.Unmarshal(e.Value, pa) != nil || pa.Account == "" {
			continue
		}

		for _, rec := range pa.Subs {
			ids[rec.ID] = true
		}

		if verified := w.sortOut(pa); len(verified) > 0 {
			byAccount[pa.Account] = verified
		}
	}

	for _, account := range w.goneAccounts(ctx, byAccount) {
		w.dropAccount(account)
		delete(byAccount, account)
	}

	w.reconcileWatchers(ctx, byAccount)
	w.prune(ids, byAccount)
}

// prune forgets the backoff of subscriptions and the checks of accounts a
// scan no longer finds, so the long-running worker does not keep them
// after the subscription or the account is gone.
func (w *worker) prune(ids map[string]bool, byAccount map[string][]*pushRecord) {
	w.mu.Lock()
	for id := range w.backoff {
		if !ids[id] {
			delete(w.backoff, id)
		}
	}
	w.mu.Unlock()

	for account := range w.checked {
		if _, ok := byAccount[account]; !ok {
			delete(w.checked, account)
		}
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
func (w *worker) goneAccounts(ctx context.Context, byAccount map[string][]*pushRecord) []string {
	if w.o.Auth == nil {
		return nil
	}

	asked, denied := w.askAccounts(ctx, byAccount)

	if len(denied) >= suspectDenials && 2*len(denied) > len(asked) {
		w.o.Log.Error("push worker: the authentication backend denies most accounts; no subscription is dropped",
			"asked", len(asked), "denied", len(denied))

		return nil
	}

	var gone []string

	for _, account := range denied {
		if c := w.checked[account]; c != nil {
			if c.denied++; c.denied >= deniedToDrop {
				gone = append(gone, account)
			}
		}
	}

	return gone
}

// askAccounts looks up the accounts that are due, at most checksPerScan
// of them, and returns those asked and those denied.
func (w *worker) askAccounts(ctx context.Context, byAccount map[string][]*pushRecord) (asked, denied []string) {
	for account := range byAccount {
		c := w.checked[account]
		if c == nil {
			c = &accountCheck{}
			w.checked[account] = c
		}

		if len(asked) >= checksPerScan || time.Since(c.at) < w.o.AccountCheck {
			continue
		}

		c.at = time.Now()

		asked = append(asked, account)

		switch _, err := w.o.Auth.Lookup(ctx, account, "jmap", nil); {
		case errors.Is(err, auth.ErrInvalid):
			denied = append(denied, account)
		case err == nil:
			c.denied = 0
		}
	}

	return asked, denied
}

// reconcileWatchers starts a watcher for every account with verified
// subscriptions and stops the others.
func (w *worker) reconcileWatchers(ctx context.Context, byAccount map[string][]*pushRecord) {
	w.mu.Lock()
	defer w.mu.Unlock()

	w.subs = byAccount

	for account, wt := range w.watching {
		if _, ok := byAccount[account]; !ok {
			wt.cancel()
			delete(w.watching, account)
		}
	}

	for account := range byAccount {
		if _, ok := w.watching[account]; ok {
			continue
		}

		actx, cancel := context.WithCancel(ctx)
		wt := &watcher{cancel: cancel}
		w.watching[account] = wt
		w.wg.Add(1)

		go w.watchAccount(actx, account, wt)
	}
}

// stopAll stops every account watcher.
func (w *worker) stopAll() {
	w.mu.Lock()

	for account, wt := range w.watching {
		wt.cancel()
		delete(w.watching, account)
	}

	w.mu.Unlock()
	w.wg.Wait()
}

// drop destroys a subscription.
func (w *worker) drop(rec *pushRecord, why string) {
	w.o.Log.Info("push worker: subscription destroyed", "account", rec.Account, "id", rec.ID, "why", why)

	_ = updatePushAccount(w.o.KV, rec.Account, func(pa *pushAccount) error {
		for i, r := range pa.Subs {
			if r.ID == rec.ID {
				pa.Subs = append(pa.Subs[:i], pa.Subs[i+1:]...)

				return nil
			}
		}

		return errNoSuchPush
	})
}

// dropAccount destroys every subscription of an account.
func (w *worker) dropAccount(account string) {
	w.o.Log.Warn("push worker: the authentication backend no longer knows the account; its subscriptions are destroyed",
		"account", account)

	_ = w.o.KV.Update(pushAcctKey(account), func([]byte) ([]byte, error) { return nil, nil })
	_ = w.o.KV.Update(pushSeenKey(account), func([]byte) ([]byte, error) { return nil, nil })
}

// verify sends the verifications that are waiting. A push service that
// refuses one is not asked again (RFC 9749 6); one that could not be
// reached is tried again, a few times.
func (w *worker) verify(ctx context.Context) {
	var items []verifyItem

	err := w.o.KV.Update(pushVerifyQueue, func(cur []byte) ([]byte, error) {
		if cur == nil {
			return nil, errNoSuchPush
		}

		_ = json.Unmarshal(cur, &items)

		return nil, nil
	})
	if err != nil {
		if !errors.Is(err, errNoSuchPush) {
			w.o.Log.Warn("push worker: read verifications", "err", err)
		}

		return
	}

	var again []verifyItem

	for _, it := range items {
		pa, err := loadPushAccount(w.o.KV, it.Account)
		if err != nil {
			again = append(again, it)
			continue
		}

		rec := pa.find(it.ID)
		if rec == nil || rec.Verified {
			continue
		}

		payload, _ := json.Marshal(map[string]string{
			"@type": "PushVerification", "pushSubscriptionId": rec.ID, "verificationCode": rec.Code,
		})

		if w.send(ctx, rec, payload) == webpush.Slow && it.Attempts+1 < verifyAttempts {
			it.Attempts++
			again = append(again, it)
		}
	}

	for _, it := range again {
		_ = w.o.KV.Update(pushVerifyQueue, func(cur []byte) ([]byte, error) {
			var q []verifyItem
			if cur != nil {
				_ = json.Unmarshal(cur, &q)
			}

			return json.Marshal(append(q, it))
		})
	}
}

// send posts one payload and acts on the answer.
func (w *worker) send(ctx context.Context, rec *pushRecord, payload []byte) webpush.Outcome {
	res, err := w.o.Sender.Send(ctx, rec.URL, rec.Keys, payload)

	switch res.Outcome {
	case webpush.Delivered:
	case webpush.Gone:
		w.drop(rec, "the push service says it is gone")
	case webpush.Slow:
		wait := max(res.RetryAfter, w.o.Scan)

		w.mu.Lock()
		w.backoff[rec.ID] = time.Now().Add(wait)
		w.mu.Unlock()

		w.o.Log.Info("push worker: push service asks to wait", "account", rec.Account, "id", rec.ID,
			"status", res.Status, "wait", wait, "err", sanitize(err))
	case webpush.Refused:
		w.o.Log.Warn("push worker: push refused", "account", rec.Account, "id", rec.ID,
			"status", res.Status, "err", sanitize(err))
	}

	return res.Outcome
}

// sanitize drops the URL from an HTTP client error: a push URL is a
// capability, and a log is not the place for it.
func sanitize(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return ue.Err
	}

	return err
}

// watchAccount follows one account and pushes its changes. A watcher that
// cannot start takes itself out, and the next scan starts it again.
func (w *worker) watchAccount(ctx context.Context, account string, self *watcher) {
	defer w.wg.Done()
	defer w.forget(account, self)

	u, err := w.o.Store.User(account)
	if err != nil {
		w.o.Log.Warn("push worker: open account", "account", account, "err", err)

		return
	}
	defer u.Close()

	last, err := w.startState(u, account)
	if err != nil {
		w.o.Log.Warn("push worker: account state", "account", account, "err", err)

		return
	}

	feed := newAccountFeed(u)
	defer feed.Close()

	// The polls of many accounts are spread over the interval.
	poll := time.NewTicker(w.o.Poll + rand.N(w.o.Poll/4+1))
	defer poll.Stop()

	pending := map[string]map[string]bool{} // subscription id -> types not yet pushed

	trigger := make(chan struct{}, 1)
	trigger <- struct{}{} // what happened while no worker watched

	for {
		select {
		case <-ctx.Done():
			return
		case <-feed.Events():
		case <-poll.C:
		case <-trigger:
		}

		// Changes that come in a burst — a COPY of many messages, a
		// client moving a thread — become one push.
		select {
		case <-ctx.Done():
			return
		case <-time.After(w.o.Coalesce):
		}

		if next := w.pushChanges(ctx, u, account, last, pending); next != last {
			last = next
			w.saveState(account, last)
		}
	}
}

// forget takes an account watcher out of the table, if it is still the
// one there.
func (w *worker) forget(account string, self *watcher) {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.watching[account] == self {
		delete(w.watching, account)
	}
}

// startState is where a watcher starts: the state last pushed for the
// account, unless it is gone from the log or ahead of the account.
func (w *worker) startState(u model.User, account string) (uint64, error) {
	now, err := u.AccountState()
	if err != nil {
		return 0, err
	}

	raw, err := w.o.KV.Get(pushSeenKey(account))
	if err != nil {
		return now, nil
	}

	seen, err := strconv.ParseUint(string(raw), 10, 64)
	if err != nil || seen > now || now-seen > maxPushStateLag {
		return now, nil
	}

	return seen, nil
}

// saveState records the state last pushed for an account.
func (w *worker) saveState(account string, seq uint64) {
	_ = w.o.KV.Update(pushSeenKey(account), func([]byte) ([]byte, error) {
		return []byte(strconv.FormatUint(seq, 10)), nil
	})
}

// pushChanges reads what changed since last, adds it to what each
// verified subscription of the account has not been sent yet, and pushes
// that to every subscription the push service does not ask to wait for.
// What could not be delivered stays for the next round. It returns the
// new state.
func (w *worker) pushChanges(ctx context.Context, u model.User, account string, last uint64,
	pending map[string]map[string]bool,
) uint64 {
	subs := w.subsOf(account)
	seq := w.collect(u, account, last, subs, pending)

	live := map[string]bool{}
	now := time.Now()

	for _, rec := range subs {
		live[rec.ID] = true

		if len(pending[rec.ID]) == 0 || now.Before(w.backoffOf(rec.ID)) {
			continue
		}

		payload, _ := json.Marshal(newStateChange(account, pending[rec.ID], seq))
		if out := w.send(ctx, rec, payload); out == webpush.Delivered || out == webpush.Gone || out == webpush.Refused {
			delete(pending, rec.ID)
		}
	}

	for id := range pending {
		if !live[id] {
			delete(pending, id)
		}
	}

	return seq
}

// collect reads the log from last and adds what changed to what each
// subscription has not been sent yet; it returns the state reached.
func (w *worker) collect(u model.User, account string, last uint64, subs []*pushRecord,
	pending map[string]map[string]bool,
) uint64 {
	seq := last

	for {
		types, next, err := changedTypes(u, seq, nil)
		if err != nil {
			w.o.Log.Warn("push worker: read changes", "account", account, "err", err)

			return seq
		}

		if next == seq {
			return seq
		}

		seq = next

		for _, rec := range subs {
			if pending[rec.ID] == nil {
				pending[rec.ID] = map[string]bool{}
			}

			for t := range filterTypes(types, rec.wants()) {
				pending[rec.ID][t] = true
			}
		}
	}
}

// subsOf are the verified subscriptions of an account.
func (w *worker) subsOf(account string) []*pushRecord {
	w.mu.Lock()
	defer w.mu.Unlock()

	return append([]*pushRecord(nil), w.subs[account]...)
}

// backoffOf is when a subscription may be pushed to again.
func (w *worker) backoffOf(id string) time.Time {
	w.mu.Lock()
	defer w.mu.Unlock()

	return w.backoff[id]
}
