package jmap

import (
	"context"
	"encoding/json"
	"math/rand/v2"
	"time"

	"shardpost/internal/metrics"
	"shardpost/internal/model"
	"shardpost/internal/webpush"
)

// The watcher of one account (stage B18a, §4.8): it follows the
// account's change log and the shared accounts, pushes what changed to
// every verified subscription, and keeps per subscription the
// acknowledgement of what was delivered (pushstate.go). A round of
// pushes runs under a budget of its own that the stop of the partition
// does not cut short, so what was sent is acknowledged and what was
// not stays as a backlog in the record; the successor derives it from
// the log and sends it.

// watchAccount follows one account and pushes its changes: those of its
// own account, and those of the shared accounts it has access to. A
// watcher that cannot start takes itself out, and the next scan starts
// it again.
func (w *worker) watchAccount(ctx context.Context, svc *partitionService, account string, self *watcher) {
	defer svc.wg.Done()
	defer svc.forget(account, self)
	defer svc.firstPassDone(self)

	u, err := w.o.Store.User(account)
	if err != nil {
		w.o.Log.Warn("push worker: open account", "account", account, "err", err)

		return
	}
	defer u.Close()

	f, err := w.startFollow(u, account, svc)
	if err != nil {
		w.o.Log.Warn("push worker: account state", "account", account, "err", err)

		return
	}
	defer f.close()
	defer f.reportPending(w, 0)

	// Counted as watched once it follows: from here on nothing of the
	// account is missed.
	w.watched.Add(1)
	w.updateStatus()

	defer func() {
		w.watched.Add(-1)
		w.updateStatus()
	}()

	if f.dirty && self.saves() {
		w.saveState(u, account, f, svc)
	}

	feed := newAccountFeed(u)
	defer feed.Close()

	// The polls of many accounts are spread over the interval.
	poll := time.NewTicker(w.o.Poll + rand.N(w.o.Poll/4+1)) //nolint:gosec // spreads the polls, no secret
	defer poll.Stop()

	trigger := make(chan struct{}, 1)
	trigger <- struct{}{} // what happened while no worker watched

	for {
		full, ok := awaitChange(ctx, feed, f.shared, poll, trigger)
		if !ok {
			return
		}

		woke := time.Now()

		// Changes that come in a burst — a COPY of many messages, a
		// client moving a thread — become one push.
		select {
		case <-ctx.Done():
			return
		case <-time.After(w.o.Coalesce):
		}

		if !w.round(ctx, u, account, f, full, svc, self, woke) {
			return
		}
	}
}

// awaitChange waits for something that may have changed: the account's
// watch, a shared account, the poll or the first trigger (both full).
// ok is false when the watcher is to end.
func awaitChange(ctx context.Context, feed *accountFeed, shared *sharedPush, poll *time.Ticker, trigger <-chan struct{}) (full, ok bool) {
	select {
	case <-ctx.Done():
		return false, false
	case <-feed.Events():
		return false, true
	case <-shared.Wake():
		return false, true
	case <-poll.C:
		return true, true
	case <-trigger:
		return true, true
	}
}

// round is one round of the watcher: it has a budget of its own, so the
// stop of the partition does not cut a push short, and the loop looks
// at the stop only once the round is saved. It reports whether the
// watcher goes on.
func (w *worker) round(ctx context.Context, u model.User, account string, f *follow, full bool, svc *partitionService,
	self *watcher, woke time.Time,
) bool {
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), w.o.RoundBudget)
	res := w.pushChanges(rctx, u, account, f, full, svc, woke)

	cancel()

	if res.rollback {
		// The account counter jumped back (purged and created again
		// under a running watcher): the watcher ends without saving; the
		// next start raises the epoch.
		w.o.Log.Info("push worker: account counter jumped back; the watcher restarts", "account", account)

		return false
	}

	if (res.moved || res.ackChanged) && self.saves() {
		w.saveState(u, account, f, svc)
	}

	svc.firstPassDone(self)

	return true
}

// follow is where one account's watcher stands: the personal state last
// read, the shared accounts it follows, the digest of their states as
// last saved, the states of theirs a new watcher found changed, the
// acknowledgements and the epoch, and the backlog per subscription in
// memory (an optimization over deriving it from the acknowledgements).
type follow struct {
	last   uint64
	shared *sharedPush
	digest string
	missed map[string]map[string]string
	views  *views
	ack    map[string]pushAck
	epoch  uint64
	// pending is subscription id -> account -> type -> state.
	pending map[string]map[string]map[string]string
	// dirty asks for a save before the first round (a new subscription
	// got its acknowledgement).
	dirty bool
	// reported is the backlog count last added to the gauge.
	reported int
}

// close gives the watches and the views of a follow back.
func (f *follow) close() {
	f.shared.close()
	f.views.close()
}

// reportPending moves the gauge of subscriptions with a backlog from
// what this watcher reported last to n.
func (f *follow) reportPending(w *worker, n int) {
	w.pending.Add(int64(n - f.reported))
	f.reported = n
}

// startFollow reads where a watcher starts: the state last pushed, the
// shared accounts as the session resource of the account lists them,
// read with the views a request would open, and the backlog of every
// subscription from its acknowledgement.
func (w *worker) startFollow(u model.User, account string, svc *partitionService) (*follow, error) {
	seen, raised, err := w.startState(u, account)
	if err != nil {
		return nil, err
	}

	sess := &session{account: account, user: u, own: u, srv: w.srv, views: &views{store: w.o.Store},
		log: w.o.Log.With("account", account)}

	f := &follow{last: seen.Seq, digest: seen.Digest, views: sess.views, epoch: seen.Epoch, dirty: raised,
		shared:  newSharedPush(w.srv, sess, w.watches, w.o.MaxSharedWatches),
		ack:     map[string]pushAck{},
		pending: map[string]map[string]map[string]string{}}

	for id, a := range seen.Ack {
		f.ack[id] = a
	}

	if seen.Shared != nil {
		// What changed in the shared accounts while no worker watched.
		f.missed = diffStates(seen.Shared, f.shared.all())
	}

	f.rebuildPending(u, account, svc.subsOf(account), seen.Shared)

	return f, nil
}

// rebuildPending builds the backlog of every subscription from its
// acknowledgement: a subscription without one starts now (what
// happened before its verification nobody owes it), one behind gets the
// types the log changed since, filtered by what it wants; a backlog
// that is filtered away whole advances the acknowledgement instead.
func (f *follow) rebuildPending(u model.User, account string, subs []*pushRecord, shared map[string]map[string]string) {
	f.pending = map[string]map[string]map[string]string{}

	for _, rec := range subs {
		a, ok := f.ack[rec.ID]
		if !ok {
			f.ack[rec.ID] = pushAck{Seq: f.last, Digest: f.digest}
			f.dirty = true

			continue
		}

		if a.Seq >= f.last && a.Digest == f.digest {
			continue
		}

		changed := pushTypesFrom(u, account, a.Seq, f.last, a.Digest != f.digest, shared)

		sc := filterChange(changed, rec.wants())
		if sc == nil {
			f.ack[rec.ID] = pushAck{Seq: f.last, Digest: f.digest}
			f.dirty = true

			continue
		}

		f.pending[rec.ID] = sc.Changed
	}
}

// startState is where a watcher starts: the state last pushed for the
// account. A state ahead of the account (the counter jumped back: the
// account was purged and created again) raises the epoch and starts
// the state and every acknowledgement at the account's counter (raised
// reports it, and the watcher saves at once); a state far behind is
// kept as it is (the first pass then reports every type, as
// changedTypes does for a log that no longer reaches back).
func (w *worker) startState(u model.User, account string) (seen pushSeen, raised bool, err error) {
	now, err := u.AccountState()
	if err != nil {
		return pushSeen{}, false, err
	}

	raw, err := w.o.KV.Get(pushSeenKey(account))
	if err != nil {
		return pushSeen{Seq: now}, false, nil
	}

	seen, ok := decodeSeen(raw)
	if !ok {
		return pushSeen{Seq: now}, false, nil
	}

	ahead := seen.Seq > now

	for _, a := range seen.Ack {
		ahead = ahead || a.Seq > now
	}

	if ahead {
		seen.Epoch++
		seen.Seq = now

		for id, a := range seen.Ack {
			a.Seq = now
			seen.Ack[id] = a
		}

		w.o.Log.Info("push worker: account counter behind the state last pushed; new epoch", "account", account,
			"epoch", seen.Epoch)
	}

	return seen, ahead, nil
}

// saveState records what was last pushed for an account: a
// compare-and-swap that merges with what another holder may have
// written (mergeSeen). A stored record of a higher epoch is adopted
// instead: the watcher takes its state and acknowledgements whole and
// builds its backlog from them.
func (w *worker) saveState(u model.User, account string, f *follow, svc *partitionService) {
	subs := svc.subsOf(account)
	live := map[string]bool{}

	for _, rec := range subs {
		live[rec.ID] = true
	}

	mine := pushSeen{Seq: f.last, Digest: f.digest, Shared: f.shared.all(), Epoch: f.epoch, Ack: map[string]pushAck{}}
	if mine.Shared == nil {
		mine.Shared = map[string]map[string]string{}
	}

	for id, a := range f.ack {
		if live[id] {
			mine.Ack[id] = a
		}
	}

	var adopted *pushSeen

	err := w.o.KV.Update(pushSeenKey(account), func(cur []byte) ([]byte, error) {
		var stored *pushSeen

		if cur != nil {
			if s, ok := decodeSeen(cur); ok {
				stored = &s
			}
		}

		merged, adopt := mergeSeen(stored, mine, live)
		if adopt {
			adopted = &merged

			return cur, nil
		}

		return json.Marshal(merged)
	})
	if err != nil {
		w.o.Log.Warn("push worker: save state", "account", account, "err", err)
	}

	f.dirty = false

	if adopted != nil {
		f.adopt(u, account, subs, adopted)
	}

	f.reportPending(w, f.countBehind(subs))
}

// adopt takes a stored record of a higher epoch over: its state and
// acknowledgements whole, and the backlog built from them.
func (f *follow) adopt(u model.User, account string, subs []*pushRecord, seen *pushSeen) {
	f.last, f.digest, f.epoch = seen.Seq, seen.Digest, seen.Epoch
	f.ack = map[string]pushAck{}

	for id, a := range seen.Ack {
		f.ack[id] = a
	}

	f.rebuildPending(u, account, subs, seen.Shared)
}

// countBehind counts the subscriptions with a backlog.
func (f *follow) countBehind(subs []*pushRecord) int {
	behind := 0

	for _, rec := range subs {
		if a, ok := f.ack[rec.ID]; !ok || a.Seq < f.last || a.Digest != f.digest {
			behind++
		}
	}

	return behind
}

// roundResult is what one round of pushChanges reports: the state
// moved, an acknowledgement changed, or the account counter jumped
// back.
type roundResult struct {
	moved, ackChanged, rollback bool
}

// pushChanges reads what changed — the account's own log from f.last,
// the shared accounts that fired (all of them and the share
// notifications when full) — adds it to what each verified subscription
// of the account has not been sent yet, and pushes that to every
// subscription the push service does not ask to wait for, as long as
// the partition's lease holds. A delivery (or a final refusal) moves
// the subscription's acknowledgement to the state of this round; what
// could not be delivered stays, in memory and as a backlog in the
// record.
func (w *worker) pushChanges(ctx context.Context, u model.User, account string, f *follow, full bool,
	svc *partitionService, woke time.Time,
) roundResult {
	subs := svc.subsOf(account)
	before := f.last

	changed, rollback := w.collect(u, account, f, full)
	if rollback {
		return roundResult{rollback: true}
	}

	queueChanges(subs, changed, f.pending)

	digest := f.shared.digest()
	at := pushAck{Seq: f.last, Digest: digest}
	live := map[string]bool{}

	var res roundResult

	for _, rec := range subs {
		live[rec.ID] = true

		if w.pushOne(ctx, rec, f, at, svc, woke) {
			res.ackChanged = true
		}
	}

	for id := range f.pending {
		if !live[id] {
			delete(f.pending, id)
		}
	}

	res.moved = f.last != before || digest != f.digest
	f.digest = digest

	return res
}

// pushOne pushes the backlog of one subscription, unless it is in
// backoff or the lease is no longer valid, and reports whether its
// acknowledgement changed. A subscription without backlog — behind only
// in what it does not want, or new — is acknowledged up to the state of
// the round.
func (w *worker) pushOne(ctx context.Context, rec *pushRecord, f *follow, at pushAck, svc *partitionService, woke time.Time) bool {
	if len(f.pending[rec.ID]) == 0 {
		if a, ok := f.ack[rec.ID]; !ok || a != at {
			f.ack[rec.ID] = at

			return true
		}

		return false
	}

	if time.Now().Before(svc.backoffOf(rec.ID)) || !svc.leaseValid() {
		return false
	}

	payload, _ := json.Marshal(&stateChange{Type: typeStateChange, Changed: f.pending[rec.ID]})
	out := w.send(ctx, rec, payload, svc.hold)

	metrics.PushDelay.Observe(time.Since(woke).Seconds())

	if out == webpush.Delivered || out == webpush.Gone || out == webpush.Refused {
		delete(f.pending, rec.ID)
		f.ack[rec.ID] = at

		return true
	}

	return false
}

// queueChanges adds what changed to what each subscription has not been
// sent yet, as far as its types ask for it.
func queueChanges(subs []*pushRecord, changed map[string]map[string]string, pending map[string]map[string]map[string]string) {
	for _, rec := range subs {
		sc := filterChange(changed, rec.wants())
		if sc == nil {
			continue
		}

		if pending[rec.ID] == nil {
			pending[rec.ID] = map[string]map[string]string{}
		}

		mergeChanges(pending[rec.ID], sc.Changed)
	}
}

// collect reads what changed since the watcher last looked and returns
// the StateChange entries of it; rollback reports an account counter
// below where the watcher stands.
func (w *worker) collect(u model.User, account string, f *follow, full bool) (map[string]map[string]string, bool) {
	if now, err := u.AccountState(); err == nil && now < f.last {
		return nil, true
	}

	changed := map[string]map[string]string{}
	own := accountID(account)

	for {
		types, next, err := changedTypes(u, f.last, nil)
		if err != nil {
			w.o.Log.Warn("push worker: read changes", "account", account, "err", err)

			break
		}

		if next == f.last {
			break
		}

		f.last = next

		for t := range types {
			if changed[own] == nil {
				changed[own] = map[string]string{}
			}

			changed[own][t] = ""
		}
	}

	for t := range changed[own] {
		changed[own][t] = stateString(f.last)
	}

	mergeChanges(changed, f.missed)
	f.missed = nil

	mergeChanges(changed, f.shared.check(full))

	return changed, false
}

// diffStates are the entries of a StateChange from the states saved to
// the states now: the types whose state differs, every type of an
// account that is new, and an account that is gone as revoked.
func diffStates(was, now map[string]map[string]string) map[string]map[string]string {
	out := map[string]map[string]string{}

	for id, ts := range now {
		for t, s := range ts {
			if was[id][t] != s {
				if out[id] == nil {
					out[id] = map[string]string{}
				}

				out[id][t] = s
			}
		}
	}

	for id := range was {
		if _, ok := now[id]; !ok && id != "" {
			if _, own := was[id][typeShareNotification]; !own {
				out[id] = revokedStates()
			}
		}
	}

	return out
}
