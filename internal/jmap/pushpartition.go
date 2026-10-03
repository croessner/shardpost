package jmap

import (
	"context"
	"errors"
	"math/rand/v2"
	"slices"
	"strconv"
	"sync"
	"time"

	"shardpost/internal/metrics"
	"shardpost/internal/model"
)

// The partitions of one pod (stage B18a, §4.4): per partition the
// states free → starting → active → shedding → free, driven by the
// cadence (the tick, every take_seconds ± 20 %, and the fast one every
// retry_seconds while a partition is orphaned, contested or refused),
// by the control notifies and by what the partition goroutines report.
// Every lock and membership operation runs in a goroutine of its own
// with a deadline, so the loop never waits on the store.

// pstate is the state of a partition in this pod.
type pstate int

const (
	stateFree     pstate = iota // not held
	stateStarting               // a lock attempt is in flight
	stateActive                 // held and served
	stateShedding               // held, being given up
)

// partition is one partition as this pod sees it.
type partition struct {
	n int

	// What the loop owns (worker.mu guards the fields a goroutine of
	// the partition reads: state, lease).
	state pstate
	lease model.Lease
	// takenAt is when the lock was taken; orphanUntil is set when it
	// was taken as a replacement (no shed until then); busy marks a
	// free partition whose lock this pod was refused (fast cadence);
	// freeSince is the first fresh read in which it was free
	// (contested after two cadences); orphaned marks a partition seen
	// orphaned, tried over the target until a fresh member holds it (a
	// record of the crashed pod's name that comes back empty does not
	// make it an ordinary free one); rotGen is the rotation count a
	// lock attempt started under.
	takenAt     time.Time
	orphanUntil time.Time
	busy        bool
	freeSince   time.Time
	orphaned    bool
	rotGen      int64
	// opStart is when the operation in flight started (a warning when
	// it takes longer than the cadence).
	opStart time.Time
	// releasedAt is when this pod last released the partition in order
	// (for the gap measurement of the next holder); dropped marks a
	// partition being given up that is out of the held list already
	// (the record is written before the unlock).
	releasedAt time.Time
	dropped    bool

	// The serving goroutine (pushscan.go): its context, its end, the
	// guard that watches the lease, and the service state.
	cancel context.CancelFunc
	done   chan struct{}
	guard  chan struct{}
	svc    *partitionService
}

// Partition events (the label of imapceph_jmap_push_partition_events_total).
const (
	eventTaken        = "taken"
	eventTakenOrphan  = "taken_orphan"
	eventReleased     = "released"
	eventShed         = "shed"
	eventLost         = "lost"
	eventExpired      = "expired"
	eventRotated      = "rotated"
	eventRotateFailed = "rotate_failed"
	eventMembersStale = "members_stale"
)

// partEvent is what a partition goroutine reports to the loop.
type partEvent struct {
	p    *partition
	kind eventKind
	// lease is set for a take that succeeded; orphan marks a take over
	// the target (orphaned or contested); err the error of a failed
	// take.
	lease  model.Lease
	orphan bool
	err    error
}

type eventKind int

const (
	evTaken        eventKind = iota // the lock was taken
	evBusy                          // another holder has the lock
	evFailed                        // the lock attempt failed
	evLost                          // the lease was lost (renewal failed)
	evExpired                       // the lease's deadline is up without a renewal
	evRotateFailed                  // the lease could not be moved after a rotation
	evReleased                      // the partition was given up (the release ended)
)

// detectRestart is the restart detection of §4.3: a pod that finds a
// fresh record of its own name with partitions (its predecessor crashed
// and its locks still run) writes nothing and tries nothing until that
// record expired, and for a renewal interval and a cadence beyond: the
// other pods see the partitions as orphaned from the record's expiry on
// and try them every fast cadence, the locks expire within a renewal
// interval after the record (the last renewal came within one of the
// last heartbeat), so by the time the restarted pod starts, the healthy
// pods hold what they had capacity for, and it starts as an ordinary
// new member without a right to its old partitions.
func (w *worker) detectRestart(ctx context.Context) {
	if w.hooks != nil && w.hooks.noRestartDetection {
		return
	}

	select {
	case <-w.members.read():
	case <-time.After(w.o.Take):
	case <-ctx.Done():
		return
	}

	v := w.members.current()

	rec, ok := v.members[w.o.Instance]
	if !ok || len(rec.Held) == 0 || !v.fresh(rec, w.now(), w.o.LeaseTTL) || rec.Leaving {
		return
	}

	wait := w.o.LeaseTTL - w.now().Sub(rec.Seen) + w.o.LeaseTTL/pushHeartbeatsPerLease + w.o.Take
	w.o.Log.Warn("push worker: a fresh record of this instance holds partitions; its predecessor crashed, waiting for the record to expire",
		"event", "push_partition", "action", "restart_wait", "held", rec.Held, "wait", wait)

	select {
	case <-ctx.Done():
	case <-time.After(wait):
	}
}

// rebalance is the cadence (§4.4) on the membership as last read: it
// takes what it should, takes at once what is orphaned or contested,
// and sheds when the pod holds more than its share and the set of pods
// has stood still. It reports whether the fast cadence is to run.
func (w *worker) rebalance(ctx context.Context) bool {
	defer w.updateStatus()

	if w.leaving.Load() || ctx.Err() != nil {
		return false
	}

	now := w.now()
	v := w.members.current()
	age := w.members.age(now)

	w.warnSlowOps(now)

	if age > 2*w.o.Take {
		w.event(eventMembersStale)
		w.o.Log.Warn("push worker: the membership is stale; taking and shedding nothing", "event", "push_partition",
			"action", eventMembersStale, "age", age.Round(time.Millisecond))

		return true
	}

	metrics.PushPartitionsUnowned.Set(float64(unowned(v, w.n, now, w.o.LeaseTTL)))

	if w.legacy.waiting(now) {
		return true // an old pod holds the lease: look again soon
	}

	names := v.names(now, w.o.LeaseTTL)
	if !slices.Contains(names, w.o.Instance) {
		names = append(names, w.o.Instance)
		slices.Sort(names)
	}

	w.settle(names, now)

	target := pushTargets(w.n, names)[w.o.Instance]
	if w.o.MaxPartitions > 0 {
		target = min(target, w.o.MaxPartitions)
	}

	r := &takeRound{w: w, v: v, now: now, age: age, target: target, est: w.estimate(v, now), taking: w.counted()}
	start := rand.IntN(w.n) //nolint:gosec // spreads the partitions, no secret

	for i := range w.n {
		r.consider(w.parts[(start+i)%w.n])
	}

	w.maybeShed(v, names, target, r.est, now)

	return r.fast
}

// takeRound is the taking of one cadence: the view it works on, the
// share and the capacity estimate, what is held or being taken so far,
// and whether a partition asked for the fast cadence.
type takeRound struct {
	w           *worker
	v           *membersView
	now         time.Time
	age         time.Duration
	target, est int
	taking      int
	fast        bool
}

// capacity reports whether the pod can take one more partition within
// its bounds.
func (r *takeRound) capacity() bool {
	limit := r.w.o.MaxPartitions
	if limit <= 0 || limit > r.w.n {
		limit = r.w.n
	}

	return r.taking < limit && int(r.w.watched.Load())+r.est <= r.w.o.MaxWatchedAccounts
}

// try starts a lock attempt when there is capacity.
func (r *takeRound) try(p *partition, orphan bool) {
	if r.capacity() {
		r.w.tryTake(p, orphan)
		r.taking++
	}
}

// consider classifies a partition this pod does not hold and tries it
// as the class allows (§4.4, second table).
func (r *takeRound) consider(p *partition) {
	w := r.w

	w.mu.Lock()
	state := p.state
	w.mu.Unlock()

	if state != stateFree {
		p.freeSince = time.Time{}

		return
	}

	switch classifyPartition(r.v, p.n, r.now, w.o.LeaseTTL, r.age, w.o.Take) {
	case classFree:
		r.considerFree(p)
	case classOrphan:
		r.fast = true
		p.freeSince, p.orphaned = time.Time{}, true

		r.try(p, true)
	case classUnknown:
		r.fast = true
		p.freeSince = time.Time{}
	default:
		p.freeSince, p.busy, p.orphaned = time.Time{}, false, false
	}
}

// considerFree is consider for a partition no fresh member holds: taken
// within the share, at once and over it when it was orphaned before or
// has been free for two cadences (contested), on the fast cadence when
// this pod's last attempt was refused.
func (r *takeRound) considerFree(p *partition) {
	if p.freeSince.IsZero() {
		p.freeSince = r.now
	}

	switch {
	case p.orphaned || r.now.Sub(p.freeSince) > 2*r.w.o.Take:
		r.fast = true

		r.try(p, true)
	case r.taking < r.target:
		r.try(p, false)
	case p.busy:
		r.fast = true
	}
}

// counted are the partitions held, being given up or being taken: what
// the target is compared with.
func (w *worker) counted() int {
	w.mu.Lock()
	defer w.mu.Unlock()

	n := 0

	for _, p := range w.parts {
		if p.state != stateFree {
			n++
		}
	}

	return n
}

// settle notes the set of fresh members and since when it is unchanged.
func (w *worker) settle(names []string, now time.Time) {
	w.mu.Lock()
	defer w.mu.Unlock()

	if !slices.Equal(names, w.names) {
		w.names, w.namesSince = names, now
	}
}

// estimate is how many accounts one more partition would bring: this
// pod's accounts per partition, else the cluster's mean over the fresh
// members, and 0 when nobody holds anything (then anyone may take).
func (w *worker) estimate(v *membersView, now time.Time) int {
	if held := len(w.heldList()); held > 0 {
		return int(w.watched.Load()) / held
	}

	watched, held := 0, 0

	for _, rec := range v.members {
		if v.fresh(rec, now, w.o.LeaseTTL) {
			watched += rec.Watched
			held += len(rec.Held)
		}
	}

	if held == 0 {
		return 0
	}

	return watched / held
}

// warnSlowOps warns about lock attempts that run longer than a cadence.
func (w *worker) warnSlowOps(now time.Time) {
	w.mu.Lock()
	defer w.mu.Unlock()

	for _, p := range w.parts {
		if p.state == stateStarting && now.Sub(p.opStart) > w.o.Take {
			w.o.Log.Warn("push worker: a lock attempt takes longer than the cadence", "event", "push_partition",
				"action", "lock_slow", "partition", p.n, "since", now.Sub(p.opStart).Round(time.Millisecond))
		}
	}
}

// tryTake starts a lock attempt for p in a goroutine of its own; the
// result comes back as an event. The marker against an old push pod is
// written first, when the pod does not hold it yet.
func (w *worker) tryTake(p *partition, orphan bool) {
	w.legacy.ensure(w.now())

	w.mu.Lock()
	p.state, p.opStart, p.rotGen = stateStarting, w.now(), w.rotGen.Load()
	w.mu.Unlock()

	if w.hooks != nil {
		w.hooks.lockTried.Add(1)
	}

	go func() {
		l, err := w.kv.LockQueueKeep(pushLockName(p.n), w.o.LeaseTTL)

		switch {
		case errors.Is(err, model.ErrLocked):
			w.events <- partEvent{p: p, kind: evBusy}

			return
		case err != nil:
			w.events <- partEvent{p: p, kind: evFailed, err: err}

			return
		}

		// A lock taken through the connection of before a rotation is
		// moved to the new one at once; a lock the loop no longer wants
		// (the pod is leaving) is given back.
		if p.rotGen != w.rotGen.Load() {
			if rt, ok := l.(model.LeaseRetaker); ok {
				if err := rt.Retake(); err != nil {
					w.events <- partEvent{p: p, kind: evFailed, err: err}

					return
				}
			}
		}

		w.events <- partEvent{p: p, kind: evTaken, lease: l, orphan: orphan}
	}()
}

// onEvent applies what a partition goroutine reported.
func (w *worker) onEvent(ev partEvent) {
	p := ev.p

	defer w.updateStatus()

	switch ev.kind {
	case evTaken:
		if w.leaving.Load() {
			w.mu.Lock()
			p.state = stateFree
			w.mu.Unlock()

			go ev.lease.Release()

			return
		}

		w.activate(p, ev.lease, ev.orphan)
	case evBusy:
		w.mu.Lock()
		p.state, p.busy = stateFree, true
		w.mu.Unlock()
	case evFailed:
		w.mu.Lock()
		p.state = stateFree
		w.mu.Unlock()

		w.o.Log.Warn("push worker: lock attempt failed", "event", "push_partition", "action", "lock_failed",
			"partition", p.n, "err", ev.err)
	case evLost:
		w.giveUp(p, eventLost, false)
	case evExpired:
		w.giveUp(p, eventExpired, false)
	case evRotateFailed:
		w.giveUp(p, eventRotateFailed, false)
	case evReleased:
		w.mu.Lock()
		p.state, p.lease, p.svc, p.dropped = stateFree, nil, nil, false
		w.mu.Unlock()
	}
}

// activate takes a partition into service: the record is written at
// once, the serving goroutine and the guard of the lease start.
func (w *worker) activate(p *partition, l model.Lease, orphan bool) {
	now := w.now()

	w.mu.Lock()
	p.state, p.lease, p.takenAt, p.busy, p.freeSince, p.orphaned = stateActive, l, now, false, time.Time{}, false
	p.orphanUntil = time.Time{}

	if orphan {
		p.orphanUntil = now.Add(w.o.OrphanHold)
	}

	ctx, cancel := context.WithCancel(context.Background())
	p.cancel, p.done, p.guard = cancel, make(chan struct{}), make(chan struct{})
	w.mu.Unlock()

	event := eventTaken
	if orphan {
		event = eventTakenOrphan
	}

	w.event(event)
	w.observeGap(p, now)
	w.o.Log.Info("push worker: partition taken", "event", "push_partition", "action", event, "partition", p.n)

	w.writeMembers()

	go w.servePartition(ctx, p)
	go w.guardLease(p, l, p.guard)
}

// observeGap measures the time from the orderly release of p by its
// previous holder, as its record says, to now.
func (w *worker) observeGap(p *partition, now time.Time) {
	v := w.members.current()

	for name, rec := range v.members {
		if name == w.o.Instance {
			continue
		}

		if at, ok := rec.Released[strconv.Itoa(p.n)]; ok && now.Sub(at) <= w.o.LeaseTTL {
			metrics.PushGap.Observe(now.Sub(at).Seconds())
		}
	}
}

// guardLease watches the lease of an active partition: a lost renewal
// or a deadline that is up without one ends the partition.
func (w *worker) guardLease(p *partition, l model.Lease, stop <-chan struct{}) {
	every := min(w.o.Take/10, 250*time.Millisecond)
	if every <= 0 {
		every = 50 * time.Millisecond
	}

	t := time.NewTicker(every)
	defer t.Stop()

	for {
		select {
		case <-stop:
			return
		case <-t.C:
			switch {
			case l.Lost():
				w.events <- partEvent{p: p, kind: evLost}

				return
			case !w.leaseValid(l):
				w.events <- partEvent{p: p, kind: evExpired}

				return
			}
		}
	}
}

// leaseValid reports whether a lease may still be worked under: its
// deadline, less the margin, has not passed (a lease without deadline
// is valid until lost).
func (w *worker) leaseValid(l model.Lease) bool {
	dl, ok := l.(model.LeaseDeadline)
	if !ok {
		return true
	}

	until := dl.ValidUntil()

	return until.IsZero() || time.Now().Before(until.Add(-w.o.SelfStopMargin))
}

// giveUp gives an active partition up for a reason other than a shed:
// the watchers stop and save, the record is written without it, the
// lease is released (which waits for a renewal in flight, so that a
// renewal that comes through late does not leave the lock held
// unserved), and the partition is tried again like a foreign one.
func (w *worker) giveUp(p *partition, event string, notify bool) {
	w.mu.Lock()
	if p.state != stateActive {
		w.mu.Unlock()

		return
	}

	p.state = stateShedding
	w.mu.Unlock()

	go w.releasePartition(p, event, notify)
}

// releasePartition is the orderly release of one partition (§4.4): the
// watchers end their rounds and save, the record is written without
// the partition and confirmed, the lock is unlocked, and the control
// object is notified when the release is one others should take up at
// once. It runs in a goroutine of its own and reports evReleased.
func (w *worker) releasePartition(p *partition, event string, notify bool) {
	w.stopServing(p)

	w.mu.Lock()
	p.dropped = true // out of the held list for the write
	l := p.lease

	if notify {
		p.releasedAt = w.now()
	}
	w.mu.Unlock()

	if w.hooks != nil && w.hooks.swapHandoffOrder && notify {
		w.omap.Notify(pushCtlOID)
		w.writeMembersWait()
	} else {
		w.writeMembersWait()
	}

	l.Release()

	if notify && (w.hooks == nil || !w.hooks.swapHandoffOrder) {
		w.omap.Notify(pushCtlOID)
	}

	w.event(event)
	w.o.Log.Info("push worker: partition given up", "event", "push_partition", "action", event, "partition", p.n)

	w.events <- partEvent{p: p, kind: evReleased}
}

// stopServing ends the serving goroutine of p and waits for it up to
// the hand-off time (the watchers end their rounds within it).
func (w *worker) stopServing(p *partition) {
	w.mu.Lock()
	cancel, done, guard := p.cancel, p.done, p.guard
	w.mu.Unlock()

	if guard != nil {
		select {
		case <-guard:
		default:
			close(guard)
		}
	}

	if cancel == nil {
		return
	}

	cancel()

	select {
	case <-done:
	case <-time.After(w.o.Handoff):
		w.o.Log.Warn("push worker: the watchers of a partition did not end within the hand-off time",
			"event", "push_partition", "action", "handoff_slow", "partition", p.n)
	}
}

// maybeShed sheds one partition when the pod holds more than its share,
// the set of pods has stood still for the settle time, a fresh member
// is under its share with capacity (else nobody would take it and the
// pod would take it back: flapping), the last shed is a rebalance
// interval ago, and the partition was not taken as a replacement within
// the orphan hold. The one shed is the one taken last.
func (w *worker) maybeShed(v *membersView, names []string, target, est int, now time.Time) {
	held := w.heldList()

	w.mu.Lock()
	settled := now.Sub(w.namesSince) >= w.o.RebalanceSettle
	paced := w.lastShed.IsZero() || now.Sub(w.lastShed) >= w.o.Rebalance
	w.mu.Unlock()

	if len(held) <= target || !settled || !paced || !someoneUnder(v, names, w.n, w.o, est, w.o.Instance, now) {
		return
	}

	if pick := w.shedCandidate(now); pick != nil {
		w.giveUp(pick, eventShed, true)
	}
}

// shedCandidate is the partition to shed: the one taken last among
// those not under the orphan hold (nil: none), and notes the shed.
func (w *worker) shedCandidate(now time.Time) *partition {
	w.mu.Lock()
	defer w.mu.Unlock()

	var pick *partition

	for _, p := range w.parts {
		if p.state != stateActive || (!p.orphanUntil.IsZero() && now.Before(p.orphanUntil)) {
			continue
		}

		if pick == nil || p.takenAt.After(pick.takenAt) {
			pick = p
		}
	}

	if pick != nil {
		w.lastShed = now
	}

	return pick
}

// someoneUnder reports whether a fresh member other than self holds
// less than its share and has the capacity for one more partition.
func someoneUnder(v *membersView, names []string, n int, o WorkerOptions, est int, self string, now time.Time) bool {
	targets := pushTargets(n, names)

	for name, rec := range v.members {
		if name == self || !v.fresh(rec, now, o.LeaseTTL) || rec.Leaving {
			continue
		}

		if len(rec.Held) < targets[name] && rec.capacity(est) {
			return true
		}
	}

	return false
}

// onRotate is the rotation hook: every active partition moves its lease
// to the new connection, each in a goroutine of its own; one that
// cannot is given up.
func (w *worker) onRotate() {
	w.rotGen.Add(1)

	w.mu.Lock()
	defer w.mu.Unlock()

	for _, p := range w.parts {
		if p.state != stateActive {
			continue
		}

		rt, ok := p.lease.(model.LeaseRetaker)
		if !ok {
			continue
		}

		go func(p *partition) {
			if err := rt.Retake(); err != nil {
				w.o.Log.Warn("push worker: lease not moved after the key rotation", "event", "push_partition",
					"action", eventRotateFailed, "partition", p.n, "err", err)

				w.events <- partEvent{p: p, kind: evRotateFailed}

				return
			}

			w.event(eventRotated)
		}(p)
	}
}

// shutdown is the orderly end (§4.4): the pod is leaving, lock attempts
// in flight are discarded, the record is written with leaving set, the
// partitions are shed in waves of a third each, one control notify per
// wave, and the record is deleted at the end (with the legacy marker
// when no other pod is left).
func (w *worker) shutdown() {
	w.leaving.Store(true)

	w.o.Log.Info("push worker: leaving", "event", "push_partition", "action", "leaving", "held", w.heldList())

	w.drainStarting()

	w.writeMembersWait()

	// The waves are sized from what is held after the attempts in
	// flight were discarded; a partition that comes in later goes with
	// the next wave.
	held := w.activePartitions()
	size := max((len(held)+wavesAtShutdown-1)/wavesAtShutdown, 1)

	for len(held) > 0 {
		start := time.Now()

		w.releaseWave(held[:min(size, len(held))])

		held = w.activePartitions()

		if len(held) == 0 {
			break
		}

		// The hand-off time is the period of the waves, not the pause.
		if rest := w.o.Handoff - time.Since(start); rest > 0 {
			time.Sleep(rest)
		}
	}

	w.legacy.clear()

	select {
	case <-w.members.write(nil):
	case <-time.After(w.o.Take):
	}

	w.updateStatus()
	w.o.Log.Info("push worker: left", "event", "push_partition", "action", "left")
}

// drainStarting waits up to a cadence for the lock attempts in flight;
// a lock that comes back is given back at once (onEvent, leaving).
func (w *worker) drainStarting() {
	deadline := time.After(w.o.Take)

	for w.starting() > 0 {
		select {
		case ev := <-w.events:
			w.onEvent(ev)
		case <-deadline:
			return
		}
	}
}

// starting counts the lock attempts in flight.
func (w *worker) starting() int {
	w.mu.Lock()
	defer w.mu.Unlock()

	n := 0

	for _, p := range w.parts {
		if p.state == stateStarting {
			n++
		}
	}

	return n
}

// activePartitions are the partitions held and served, in order.
func (w *worker) activePartitions() []*partition {
	w.mu.Lock()
	defer w.mu.Unlock()

	var out []*partition

	for _, p := range w.parts {
		if p.state == stateActive {
			out = append(out, p)
		}
	}

	return out
}

// releaseWave gives a set of partitions up together: the watchers of
// all of them end, one record without them is written and confirmed,
// the locks are unlocked, and one control notify follows.
func (w *worker) releaseWave(wave []*partition) {
	var wg sync.WaitGroup

	for _, p := range wave {
		w.mu.Lock()
		p.state = stateShedding
		w.mu.Unlock()

		wg.Add(1)

		go func(p *partition) {
			defer wg.Done()

			w.stopServing(p)
		}(p)
	}

	wg.Wait()

	now := w.now()

	w.mu.Lock()
	for _, p := range wave {
		p.dropped, p.releasedAt = true, now
	}
	w.mu.Unlock()

	w.writeMembersWait()

	for _, p := range wave {
		wg.Add(1)

		go func(p *partition) {
			defer wg.Done()

			w.mu.Lock()
			l := p.lease
			w.mu.Unlock()

			l.Release()

			w.mu.Lock()
			p.state, p.lease, p.svc, p.dropped = stateFree, nil, nil, false
			w.mu.Unlock()

			w.event(eventReleased)
		}(p)
	}

	wg.Wait()

	w.omap.Notify(pushCtlOID)
	w.o.Log.Info("push worker: wave released", "event", "push_partition", "action", eventReleased, "partitions", partitionNumbers(wave))
}

// abandon is the crash of tests: the leases stop being renewed without
// an unlock (as the leases of a process that died), the watchers stop
// without saving, and nothing is written.
func (w *worker) abandon() {
	w.mu.Lock()
	defer w.mu.Unlock()

	for _, p := range w.parts {
		if p.lease != nil {
			if a, ok := p.lease.(interface{ Abandon() }); ok {
				a.Abandon()
			}
		}

		if p.cancel != nil {
			p.cancel()
		}
	}
}

// partitionNumbers are the numbers of a set of partitions.
func partitionNumbers(ps []*partition) []int {
	out := make([]int, 0, len(ps))
	for _, p := range ps {
		out = append(out, p.n)
	}

	return out
}
