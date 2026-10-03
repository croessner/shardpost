package jmap

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net/url"
	"sync"
	"sync/atomic"
	"time"

	"shardpost/internal/auth"
	"shardpost/internal/kvindex"
	"shardpost/internal/metrics"
	"shardpost/internal/model"
	"shardpost/internal/sortedindex"
	"shardpost/internal/webpush"
)

// The push worker sends Web Push for the push subscriptions the
// front-ends took (RFC 8620 7.2). It is a process of its own
// ("shardpost push"), because a front-end only runs while a client talks
// to it, and a push subscription exists for the time no client does.
//
// The accounts are split into partitions (stage B18a): as many as the
// hint index of the subscriptions has shards (64), the partition of an
// account being the shard of its record (PushPartition). Every
// partition has a lease of its own, a cls_lock in the admin namespace
// that its holder renews, and the push pods share the partitions out
// among themselves through a membership record (pushmembers.go): each
// should hold its share, takes what is free, takes at once what a pod
// that crashed left behind, and sheds one partition at a time when the
// set of pods has stood still for a while (pushpartition.go). The
// holder of a partition reads only its shard of the index, watches the
// accounts of the partition and posts a StateChange when one changes
// (pushscan.go, pushwatch.go). The state last pushed is kept per
// account with an acknowledgement per subscription (pushstate.go), so
// a hand-over loses nothing and repeats at most one push.

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
	// Instance names this worker in the membership and the legacy lease
	// record; it has to be unique among the push pods and stable across
	// restarts of the same pod (the host name with a suffix).
	Instance string
	// Version is recorded in the membership (display).
	Version string
	// VAPIDPublicKey is the key the worker signs with; a subscription made
	// under another key is destroyed (RFC 9749 5).
	VAPIDPublicKey string
	// Sharing are the shared accounts the front-ends offer, with their
	// bounds; the worker pushes the changes of the ones an account has
	// access to. Both prefixes empty: none.
	Sharing SharingOptions
	// MaxSharedWatches bounds the shared accounts one account's watcher
	// watches (the others are checked on its poll), and MaxWatches the
	// namespaces the worker watches for shared accounts in all, however
	// many accounts share them (0: DefaultWorker's).
	MaxSharedWatches, MaxWatches int
	// Scan is how often the subscriptions of a held partition are read
	// again, Verify how often the verification queue is read, Poll how
	// often a watched account is checked without a notification,
	// Coalesce how long changes are gathered before a push, LeaseTTL how
	// long a partition lease lasts unrenewed (it is renewed every third
	// of it), and AccountCheck how often an account is looked up in Auth.
	Scan, Verify, Poll, Coalesce, LeaseTTL, AccountCheck time.Duration
	// The partitions (jmap.push.*, see config.JMAPPush): Take is the
	// cadence of taking and shedding (± 20 %) and the deadline of a lock
	// or membership operation, Retry the fast cadence for orphaned and
	// contested partitions, OrphanHold how long a partition taken as a
	// replacement is kept, FullScan how often every record of a
	// partition is read whatever its checksum (0: never), Rebalance the
	// pause between two sheds, RebalanceSettle how long the set of pods
	// has to stand still before a shed, Handoff how long the watchers of
	// a partition get to end when it is shed and the period of the waves
	// at shutdown, MaxPartitions how many partitions this pod holds at
	// most (0: all), MaxWatchedAccounts how many accounts it watches at
	// most, StartRate how many watchers it starts per second. 0 takes
	// the default.
	Take, Retry, OrphanHold, FullScan, Rebalance, RebalanceSettle, Handoff time.Duration
	MaxPartitions, MaxWatchedAccounts, StartRate                           int
	// SelfStopMargin is how long before its lease would run out a holder
	// stops a partition it could not renew (5 s); RoundBudget how long
	// one round of a watcher (the pushes to every subscription of an
	// account) may take once the partition is being given up (3 s, and
	// never more than four fifths of Handoff).
	SelfStopMargin, RoundBudget time.Duration
	// Rotate, when set, tells the worker about key rotations of the
	// store, after which it moves its leases to the new connection
	// (model.LeaseRetaker); nil for a store without rotations.
	Rotate Rotator
	// Status, when set, is kept up to date with the partitions held and
	// the accounts watched (the registration shows them).
	Status *PushStatus
	// Now is the clock of the membership (time.Now when nil; tests).
	Now func() time.Time
}

// Rotator tells its hooks about key rotations of the store (the RADOS
// store, and the in-memory store in tests).
type Rotator interface {
	OnRotate(fn func()) func()
}

// PushStatus is what the worker reports about itself: the partitions it
// holds and the accounts it watches.
type PushStatus struct {
	partitions, watched atomic.Int64
}

// Get returns the partitions held and the accounts watched
// (jobs.Registration.Status).
func (s *PushStatus) Get() (partitions, watched int) {
	return int(s.partitions.Load()), int(s.watched.Load())
}

// DefaultWorker are the intervals a configuration that says nothing gets.
var DefaultWorker = WorkerOptions{
	Scan: 30 * time.Second, Verify: 2 * time.Second, Poll: time.Minute, Coalesce: time.Second,
	LeaseTTL: 30 * time.Second, AccountCheck: time.Hour,
	MaxSharedWatches: DefaultPush.MaxSharedWatches, MaxWatches: 1000,
	Take: 5 * time.Second, Retry: 2 * time.Second, OrphanHold: 10 * time.Minute, FullScan: time.Hour,
	Rebalance: 10 * time.Second, RebalanceSettle: time.Minute, Handoff: 5 * time.Second,
	MaxWatchedAccounts: 50000, StartRate: 5000, SelfStopMargin: 5 * time.Second, RoundBudget: 3 * time.Second,
}

// Bounds of the account check: accounts looked up per scan of a
// partition, denials in a row before an account counts as gone, and the
// share of denials in one scan above which the backend is suspected
// rather than the accounts; the attempts of a verification.
const (
	checksPerScan  = 20
	deniedToDrop   = 3
	suspectDenials = 3
	verifyAttempts = 3
	// defaultPushPartitions is the partition count while the hint index
	// of the subscriptions has no head yet (its shard count).
	defaultPushPartitions = 64
	// pushScanPage is the page of a shard read.
	pushScanPage = 256
	// wavesAtShutdown is how many waves a pod sheds its partitions in at
	// shutdown.
	wavesAtShutdown = 3
	// pushLegacyTTL is how long the legacy lease record is held as a
	// blocking marker, and pushLegacyRefresh from which remaining time
	// on it is refreshed.
	pushLegacyTTL     = 90 * time.Second
	pushLegacyRefresh = 60 * time.Second
	// pushLegacyPoll is how often a pod waiting for an old holder looks
	// again.
	pushLegacyPoll = 2 * time.Second
	// pushRecheck is how long after a control notify that showed no
	// change the membership is read once more.
	pushRecheck = time.Second
	// pushTickJitter is the jitter of the cadence (± 20 %).
	pushTickJitter = 0.2
)

// workerDefaults fills in the intervals left at zero.
func workerDefaults(o WorkerOptions) WorkerOptions {
	d := DefaultWorker
	for _, p := range []struct{ v, d *time.Duration }{
		{&o.Scan, &d.Scan}, {&o.Verify, &d.Verify}, {&o.Poll, &d.Poll}, {&o.Coalesce, &d.Coalesce},
		{&o.LeaseTTL, &d.LeaseTTL}, {&o.AccountCheck, &d.AccountCheck},
		{&o.Take, &d.Take}, {&o.Retry, &d.Retry}, {&o.Rebalance, &d.Rebalance}, {&o.RebalanceSettle, &d.RebalanceSettle},
		{&o.Handoff, &d.Handoff}, {&o.SelfStopMargin, &d.SelfStopMargin}, {&o.RoundBudget, &d.RoundBudget},
	} {
		if *p.v <= 0 {
			*p.v = *p.d
		}
	}

	// The round of a watcher ends within the hand-off, so that a
	// partition given up never waits on a push: the budget is bounded
	// by four fifths of the hand-off.
	if o.RoundBudget > o.Handoff*4/5 {
		o.RoundBudget = o.Handoff * 4 / 5
	}

	// OrphanHold and FullScan may be 0 by choice: a negative value asks
	// for the default.
	if o.OrphanHold < 0 {
		o.OrphanHold = d.OrphanHold
	}

	if o.FullScan < 0 {
		o.FullScan = d.FullScan
	}

	for _, p := range []struct{ v, d *int }{
		{&o.MaxSharedWatches, &d.MaxSharedWatches}, {&o.MaxWatches, &d.MaxWatches},
		{&o.MaxWatchedAccounts, &d.MaxWatchedAccounts}, {&o.StartRate, &d.StartRate},
	} {
		if *p.v <= 0 {
			*p.v = *p.d
		}
	}

	if o.Log == nil {
		o.Log = slog.Default()
	}

	if o.Now == nil {
		o.Now = time.Now
	}

	o.Sharing = sharingDefaults(o.Sharing)

	return o
}

// check reports what the options lack.
func (o *WorkerOptions) check() error {
	if o.Store == nil || o.KV == nil || o.Sender == nil || o.Instance == "" {
		return errors.New("push worker: store, admin store, sender and instance are required")
	}

	if _, ok := o.KV.(model.IndexedKV); !ok {
		return errors.New("push worker: the admin store has no queues and locks (admin.kv_index off)")
	}

	if _, ok := o.KV.(omapper); !ok {
		return errors.New("push worker: the admin store has no omap access")
	}

	return nil
}

// omapper is an admin store with plain omap access (the RADOS store and
// the in-memory store).
type omapper interface {
	Omap() model.AdminOmap
}

// indexer is an admin store that gives out its hint indexes (the
// kvindex store both admin stores embed).
type indexer interface {
	Index(c *kvindex.Collection) *sortedindex.Index
}

// pushIndex is the hint index of the subscriptions of an admin store,
// or nil when the store has none.
func pushIndex(kv model.AdminKV) *sortedindex.Index {
	ix, ok := kv.(indexer)
	if !ok {
		return nil
	}

	return ix.Index(kvindex.PushCollection())
}

// pushPartitionCount is the partition count: the shard count of the
// hint index of the subscriptions (the default while it has no head).
func pushPartitionCount(kv model.AdminKV) (int, error) {
	ix := pushIndex(kv)
	if ix == nil {
		return defaultPushPartitions, nil
	}

	h, err := ix.Head()
	if err != nil {
		return 0, err
	}

	return h.Shards, nil
}

// pushLeaseKey is the admin store key of the lease of the worker before
// 0.52.0, which the partitioned pods hold as a blocking marker
// (pushlegacy.go).
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

// worker is a running push worker.
type worker struct {
	o    WorkerOptions
	kv   model.IndexedKV
	omap model.AdminOmap
	// ix is the hint index of the subscriptions (nil: the store has
	// none, every scan lists).
	ix *sortedindex.Index
	// n is the partition count; parts the partitions.
	n     int
	parts []*partition
	// srv carries the sharing settings the shared accounts are built
	// with, the way a front-end builds them; watches are the watches on
	// the namespaces of shared accounts, one each for all accounts.
	srv     *Server
	watches *watchHub
	members *membership
	legacy  legacyMarker
	// events is where the partition goroutines report (pushpartition.go).
	events chan partEvent
	// leaving is set at shutdown: no partition is tried any more.
	leaving atomic.Bool
	// releasing counts the releases of single partitions in flight
	// (giveUp); the shutdown waits for them before it deletes the
	// record.
	releasing sync.WaitGroup
	// rotGen counts the key rotations seen; a lock taken before one has
	// to be moved.
	rotGen atomic.Int64
	// limiter bounds the watcher starts per second.
	limiter *rateLimiter
	// watched counts the accounts watched, pending the subscriptions
	// with a backlog.
	watched, pending atomic.Int64
	// verifyKick wakes the verification sender (a notify on partition 0,
	// the verification interval); wake asks the loop for a cadence (the
	// tick, the fast cadence, a control notify).
	verifyKick chan struct{}
	wake       chan wakeKind
	// listedWarned is when the scan last warned that it lists.
	listedWarned time.Time

	mu sync.Mutex
	// registered is set once the pod wrote its membership record for
	// the first time (after its first take, or a cadence after its first
	// tick).
	registered bool
	firstTick  time.Time
	// names and namesSince are the set of fresh members last seen and
	// since when it has been that (the settle time for sheds).
	names      []string
	namesSince time.Time
	lastShed   time.Time
	// hooks are the test hooks (nil in production).
	hooks *pushTestHooks
}

// pushTestHooks steer and observe the worker in tests.
type pushTestHooks struct {
	// swapHandoffOrder notifies before the membership write of a release
	// (the counter-check of the write → unlock → notify order).
	swapHandoffOrder bool
	// noRestartDetection switches the restart detection off (the
	// counter-check of the crash loop test).
	noRestartDetection bool
	// lockTried counts the lock attempts, watcherStarts the watchers
	// started.
	lockTried, watcherStarts atomic.Int64
	// events counts the partition events by name.
	mu     sync.Mutex
	events map[string]int
	// crash, when closed, ends the worker at once, without the orderly
	// end: its leases stop being renewed (memstore) and its record stays.
	crash chan struct{}
	// onMembersWrite sees every membership record written.
	onMembersWrite func(rec *memberRecord)
	// afterTake runs in a lock attempt that succeeded, after its check
	// for a rotation and before it reports the take (a rotation in that
	// gap is the case activate catches).
	afterTake func(p int)
}

// count counts an event.
func (h *pushTestHooks) count(event string) {
	if h == nil {
		return
	}

	h.mu.Lock()
	defer h.mu.Unlock()

	if h.events == nil {
		h.events = map[string]int{}
	}

	h.events[event]++
}

// of is the count of an event.
func (h *pushTestHooks) of(event string) int {
	h.mu.Lock()
	defer h.mu.Unlock()

	return h.events[event]
}

// event counts a partition event in the metric and the test hooks.
func (w *worker) event(name string) {
	metrics.PushPartitionEvents.WithLabelValues(name).Inc()
	w.hooks.count(name)
}

// RunPushWorker runs the push worker until ctx ends.
func RunPushWorker(ctx context.Context, o WorkerOptions) error {
	return runPushWorker(ctx, o, nil)
}

func runPushWorker(ctx context.Context, o WorkerOptions, hooks *pushTestHooks) error {
	o = workerDefaults(o)
	if err := o.check(); err != nil {
		return err
	}

	w := &worker{o: o, hooks: hooks,
		srv:        &Server{o: Options{Store: o.Store, Log: o.Log, Sharing: o.Sharing}},
		watches:    newWatchHub(o.Store, o.MaxWatches, o.Log),
		events:     make(chan partEvent, 256),
		verifyKick: make(chan struct{}, 1),
		wake:       make(chan wakeKind, 8),
		limiter:    newRateLimiter(float64(o.StartRate)),
	}
	w.kv, _ = o.KV.(model.IndexedKV)
	w.omap = o.KV.(omapper).Omap()
	w.ix = pushIndex(o.KV)

	n, err := w.partitionCount(ctx)
	if err != nil {
		return err
	}

	w.n = n
	w.parts = make([]*partition, n)

	for i := range w.parts {
		w.parts[i] = &partition{n: i}
	}

	w.members = newMembership(w.omap, o.Instance, o.LeaseTTL, o.Now, o.Log)
	defer w.members.close()

	if hooks != nil {
		w.members.writeHook = hooks.onMembersWrite
	}

	w.legacy = legacyMarker{w: w}

	if o.Rotate != nil {
		defer o.Rotate.OnRotate(w.onRotate)()
	}

	o.Log.Info("push worker: partitioned", "instance", o.Instance, "partitions", n, "event", "push_partition", "action", "start")

	w.detectRestart(ctx)
	w.run(ctx)

	return nil
}

// partitionCount reads the partition count, trying again while the
// store does not answer.
func (w *worker) partitionCount(ctx context.Context) (int, error) {
	for {
		n, err := pushPartitionCount(w.o.KV)
		if err == nil {
			return n, nil
		}

		w.o.Log.Warn("push worker: read the subscription index head", "err", err)

		select {
		case <-ctx.Done():
			return 0, fmt.Errorf("push worker: partition count: %w", err)
		case <-time.After(w.o.Retry):
		}
	}
}

// now is the clock of the membership.
func (w *worker) now() time.Time { return w.o.Now() }

// jittered is d ± 20 %.
func jittered(d time.Duration) time.Duration {
	f := 1 + pushTickJitter*(2*rand.Float64()-1) //nolint:gosec // jitter, no secret

	return time.Duration(float64(d) * f)
}

// wakeKind is what asks the loop for a cadence.
type wakeKind int

const (
	wakeTick   wakeKind = iota // the cadence itself
	wakeFast                   // the fast cadence, or a result that lets the pod take now
	wakeNotify                 // a control notify, or the recheck after one
)

// wakeup asks the loop for a cadence out of turn (never blocks).
func (w *worker) wakeup(k wakeKind) {
	select {
	case w.wake <- k:
	default:
	}
}

// run is the loop of the worker: the cadence, the fast cadence while a
// partition needs it, the control notifies, the heartbeat of the
// membership, the verifications and what the partition goroutines
// report.
func (w *worker) run(ctx context.Context) {
	c := &cadence{w: w}

	heartbeat := time.NewTicker(w.o.LeaseTTL / pushHeartbeatsPerLease)
	defer heartbeat.Stop()

	go w.ticks(ctx)
	go w.watchCtl(ctx)

	var crash chan struct{}
	if w.hooks != nil {
		crash = w.hooks.crash
	}

	for {
		select {
		case <-ctx.Done():
			w.shutdown()

			return
		case <-crash:
			w.abandon()

			return
		case k := <-w.wake:
			c.woken(k)
		case <-c.readDone:
			c.finish(ctx)
		case <-c.readDeadline:
			c.finish(ctx)
		case ev := <-w.events:
			w.onEvent(ev)
		case <-heartbeat.C:
			w.heartbeat()
		case <-w.verifyKick:
			w.verifyIfOwner(ctx)
		}
	}
}

// ticks sends the cadence (take_seconds ± 20 %, the first at once) and
// the verification interval to the loop until ctx ends.
func (w *worker) ticks(ctx context.Context) {
	verify := time.NewTicker(w.o.Verify)
	defer verify.Stop()

	tick := time.NewTimer(0)
	defer tick.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			w.wakeup(wakeTick)
			tick.Reset(jittered(w.o.Take))
		case <-verify.C:
			select {
			case w.verifyKick <- struct{}{}:
			default:
			}
		}
	}
}

// cadence is the membership read of one tick and what follows it: the
// loop starts the read and goes on; when it ends, or took longer than a
// cadence, the rebalance runs on the view there is.
type cadence struct {
	w *worker
	// readDone is closed when the membership read ends; readDeadline
	// fires when it took too long (both nil while none runs).
	readDone     <-chan struct{}
	readDeadline <-chan time.Time
	// fromNotify tells the pending tick apart from the cadence.
	fromNotify bool
	// fastArmed and recheckArmed are set while a fast cadence or a
	// recheck is scheduled, so that they do not pile up.
	fastArmed, recheckArmed atomic.Bool
}

// woken starts the read of a tick (a tick, the fast cadence or a
// control notify); a read in flight is shared.
func (c *cadence) woken(k wakeKind) {
	if k != wakeNotify {
		c.w.beforeTick()
	}

	if c.readDone != nil {
		c.fromNotify = c.fromNotify || k == wakeNotify

		return
	}

	c.fromNotify = k == wakeNotify
	c.readDone = c.w.members.read()
	c.readDeadline = time.After(c.w.o.Take)
}

// finish is the rebalance after the read; it schedules the fast
// cadence when a partition needs it, and a recheck when a control
// notify showed no change yet (the releaser writes before it notifies,
// but the write may have been slow).
func (c *cadence) finish(ctx context.Context) {
	c.readDone, c.readDeadline = nil, nil

	if c.w.rebalance(ctx) && c.fastArmed.CompareAndSwap(false, true) {
		time.AfterFunc(c.w.o.Retry, func() {
			c.fastArmed.Store(false)
			c.w.wakeup(wakeFast)
		})
	}

	if c.fromNotify && !c.w.members.changed() && c.recheckArmed.CompareAndSwap(false, true) {
		time.AfterFunc(pushRecheck, func() {
			c.recheckArmed.Store(false)
			c.w.wakeup(wakeNotify)
		})
	}
}

// watchCtl watches the control object until ctx ends and wakes the loop
// on every notify; the watch is set up again when the store fails it.
func (w *worker) watchCtl(ctx context.Context) {
	for {
		watch, err := w.omap.Watch(pushCtlOID)
		if err != nil {
			w.o.Log.Warn("push worker: watch the control object", "err", err)
		} else {
			w.followCtl(ctx, watch)
			watch.Close()
		}

		select {
		case <-ctx.Done():
			return
		case <-time.After(rewatchWait()):
		}
	}
}

// followCtl forwards the events of one watch until it breaks or ctx
// ends.
func (w *worker) followCtl(ctx context.Context, watch model.Watch) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-watch.Events():
			w.wakeup(wakeNotify)
		case <-watch.Errors():
			return
		}
	}
}

// beforeTick is what a tick does before it reads the membership: it
// notes the first tick (for the first membership write) and refreshes
// the legacy marker.
func (w *worker) beforeTick() {
	now := w.now()

	w.mu.Lock()
	if w.firstTick.IsZero() {
		w.firstTick = now
	}
	w.mu.Unlock()

	w.legacy.refresh(now)
}

// heartbeat writes the membership record on its interval, once the pod
// is registered.
func (w *worker) heartbeat() {
	w.mu.Lock()
	registered := w.registered
	first := w.firstTick
	w.mu.Unlock()

	if !registered && !first.IsZero() && w.now().Sub(first) >= w.o.Take {
		w.setRegistered()

		registered = true
	}

	if registered {
		w.writeMembers()
	}
}

// setRegistered marks the pod as writing its record from now on.
func (w *worker) setRegistered() {
	w.mu.Lock()
	defer w.mu.Unlock()

	w.registered = true
}

// record is the pod's membership record as it stands.
func (w *worker) record() *memberRecord {
	now := w.now()
	rec := &memberRecord{Seen: now, Held: w.heldList(), Watched: int(w.watched.Load()), Version: w.o.Version,
		MaxHeld: w.o.MaxPartitions, MaxWatched: w.o.MaxWatchedAccounts, Leaving: w.leaving.Load()}

	w.mu.Lock()
	for _, p := range w.parts {
		if !p.releasedAt.IsZero() && now.Sub(p.releasedAt) <= w.o.LeaseTTL {
			if rec.Released == nil {
				rec.Released = map[string]time.Time{}
			}

			rec.Released[fmt.Sprint(p.n)] = p.releasedAt
		}
	}
	w.mu.Unlock()

	return rec
}

// writeMembers queues a write of the record (event-driven writes and
// the heartbeat) and returns the channel of its result.
func (w *worker) writeMembers() <-chan error {
	w.setRegistered()

	return w.members.write(w.record())
}

// writeMembersWait is writeMembers that waits for the result up to the
// cadence (and goes on without it).
func (w *worker) writeMembersWait() {
	select {
	case <-w.writeMembers():
	case <-time.After(w.o.Take):
		w.o.Log.Warn("push worker: the membership write did not come back in time", "event", "push_partition", "action", "members_slow")
	}
}

// heldList are the partitions this pod holds (active or being given
// up), in order.
func (w *worker) heldList() []int {
	w.mu.Lock()
	defer w.mu.Unlock()

	out := []int{}

	for _, p := range w.parts {
		if (p.state == stateActive || p.state == stateShedding) && !p.dropped {
			out = append(out, p.n)
		}
	}

	return out
}

// updateStatus publishes the counts.
func (w *worker) updateStatus() {
	held := len(w.heldList())

	metrics.PushPartitionsHeld.Set(float64(held))
	metrics.PushWatchedAccounts.Set(float64(w.watched.Load()))
	metrics.PushPending.Set(float64(w.pending.Load()))

	if w.o.Status != nil {
		w.o.Status.partitions.Store(int64(held))
		w.o.Status.watched.Store(w.watched.Load())
	}
}

// verifyIfOwner sends the verifications when this pod is the one to do
// it: the holder of the smallest held partition, as the membership
// shows it (the pod itself counted). Two pods that both believe it for
// a moment cost at most a compare-and-swap conflict on the queue.
func (w *worker) verifyIfOwner(ctx context.Context) {
	if w.ownsVerification() {
		w.verify(ctx)
	}
}

// ownsVerification reports whether this pod holds the smallest held
// partition.
func (w *worker) ownsVerification() bool {
	mine := w.heldList()
	if len(mine) == 0 {
		return false
	}

	v := w.members.current()
	now := w.now()

	for name, rec := range v.members {
		if name == w.o.Instance || !v.fresh(rec, now, w.o.LeaseTTL) {
			continue
		}

		for _, p := range rec.Held {
			if p < mine[0] {
				return false
			}
		}
	}

	return true
}

// verify sends the verifications that are waiting. A push service that
// refuses one is not asked again (RFC 9749 6); one that could not be
// reached is tried again, a few times.
//
// The queue is read, worked through, and only then are the items that
// were handled taken out of it, in one update that keeps what the
// front-ends queued in between: a worker that dies on the way leaves
// the queue as it was, and its successor sends those verifications
// (again, for the ones already sent — a second verification of the same
// subscription carries the same code and does no harm).
func (w *worker) verify(ctx context.Context) {
	raw, err := w.o.KV.Get(pushVerifyQueue)
	if err != nil {
		if !errors.Is(err, model.ErrNotFound) {
			w.o.Log.Warn("push worker: read verifications", "err", err)
		}

		metrics.PushVerifyQueueAge.Set(0)

		return
	}

	var items []verifyItem
	if json.Unmarshal(raw, &items) != nil || len(items) == 0 {
		metrics.PushVerifyQueueAge.Set(0)

		return
	}

	metrics.PushVerifyQueueAge.Set(verifyQueueAge(items, time.Now()))

	var again []verifyItem

	for _, it := range items {
		if w.verifyOne(ctx, it) {
			it.Attempts++
			again = append(again, it)
		}
	}

	err = w.o.KV.Update(pushVerifyQueue, func(cur []byte) ([]byte, error) {
		var q []verifyItem
		if cur != nil {
			_ = json.Unmarshal(cur, &q)
		}

		q = append(withoutItems(q, items), again...)
		if len(q) == 0 {
			return nil, nil
		}

		return json.Marshal(q)
	})
	if err != nil {
		w.o.Log.Warn("push worker: update verifications", "err", err)
	}
}

// verifyQueueAge is the age of the oldest item that carries a time, in
// seconds.
func verifyQueueAge(items []verifyItem, now time.Time) float64 {
	var oldest time.Time

	for _, it := range items {
		if !it.Queued.IsZero() && (oldest.IsZero() || it.Queued.Before(oldest)) {
			oldest = it.Queued
		}
	}

	if oldest.IsZero() {
		return 0
	}

	return now.Sub(oldest).Seconds()
}

// verifyOne sends one verification and reports whether it is to be tried
// again.
func (w *worker) verifyOne(ctx context.Context, it verifyItem) bool {
	pa, err := loadPushAccount(w.o.KV, it.Account)
	if err != nil {
		return true
	}

	rec := pa.find(it.ID)
	if rec == nil || rec.Verified {
		return false
	}

	payload, _ := json.Marshal(map[string]string{
		"@type": "PushVerification", "pushSubscriptionId": rec.ID, "verificationCode": rec.Code,
	})

	return w.send(ctx, rec, payload, nil) == webpush.Slow && it.Attempts+1 < verifyAttempts
}

// withoutItems is the queue without the items a round handled (one
// occurrence each, as they were read).
func withoutItems(q, handled []verifyItem) []verifyItem {
	left := map[verifyItem]int{}
	for _, it := range handled {
		left[it]++
	}

	out := q[:0:0]

	for _, it := range q {
		if left[it] > 0 {
			left[it]--

			continue
		}

		out = append(out, it)
	}

	return out
}

// send posts one payload and acts on the answer; hold, when set, is
// told how long the subscription is to be left alone when the push
// service asks for that.
func (w *worker) send(ctx context.Context, rec *pushRecord, payload []byte, hold func(id string, until time.Time)) webpush.Outcome {
	res, err := w.o.Sender.Send(ctx, rec.URL, rec.Keys, payload)

	switch res.Outcome {
	case webpush.Delivered:
		metrics.PushSent.WithLabelValues("delivered").Inc()
	case webpush.Gone:
		metrics.PushSent.WithLabelValues("gone").Inc()
		w.drop(rec, "the push service says it is gone")
	case webpush.Slow:
		metrics.PushSent.WithLabelValues("slow").Inc()

		wait := max(res.RetryAfter, w.o.Scan)

		if hold != nil {
			hold(rec.ID, time.Now().Add(wait))
		}

		w.o.Log.Info("push worker: push service asks to wait", "account", rec.Account, "id", rec.ID,
			"status", res.Status, "wait", wait, "err", sanitize(err))
	case webpush.Refused:
		metrics.PushSent.WithLabelValues("refused").Inc()
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

// dropAccount destroys every subscription of an account and the state
// kept for it.
func (w *worker) dropAccount(account string) {
	w.o.Log.Warn("push worker: the authentication backend no longer knows the account; its subscriptions are destroyed",
		"account", account)

	_ = w.o.KV.Update(pushAcctKey(account), func([]byte) ([]byte, error) { return nil, nil })
	_ = w.o.KV.Update(pushSeenKey(account), func([]byte) ([]byte, error) { return nil, nil })
}

// rateLimiter is a token bucket: rate tokens per second, at most one
// second's worth saved up.
type rateLimiter struct {
	mu     sync.Mutex
	rate   float64
	tokens float64
	last   time.Time
}

func newRateLimiter(rate float64) *rateLimiter {
	return &rateLimiter{rate: rate, tokens: rate, last: time.Now()}
}

// wait takes one token, waiting for it; false when ctx ended first.
func (r *rateLimiter) wait(ctx context.Context) bool {
	for {
		r.mu.Lock()
		now := time.Now()
		r.tokens = min(r.rate, r.tokens+r.rate*now.Sub(r.last).Seconds())
		r.last = now

		if r.tokens >= 1 {
			r.tokens--
			r.mu.Unlock()

			return true
		}

		wait := time.Duration((1 - r.tokens) / r.rate * float64(time.Second))
		r.mu.Unlock()

		select {
		case <-ctx.Done():
			return false
		case <-time.After(wait):
		}
	}
}

// rewatchWait is how long a feed waits before it sets a broken watch up
// again: half a second to five seconds at random, so that the watches
// of a process do not all come back at once after a key rotation or an
// OSD restart.
func rewatchWait() time.Duration {
	return rewatchMin + rand.N(rewatchMax-rewatchMin) //nolint:gosec // jitter, no secret
}

// The bounds of rewatchWait.
const (
	rewatchMin = 500 * time.Millisecond
	rewatchMax = 5 * time.Second
)
