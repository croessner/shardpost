package jmap

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"shardpost/internal/kvindex"
	"shardpost/internal/memstore"
	"shardpost/internal/model"
	"shardpost/internal/sortedindex"
	"shardpost/internal/webpush"
)

// The tests of the partitioned push worker (stage B18a, §8.1) run a
// few workers over one in-memory store and one in-memory admin store,
// with short intervals: a lease of 600 ms, a cadence of 100 ms, a
// hand-off of 300 ms. "Soon" below means within a few of those.

// Intervals of the test workers.
const (
	testLease   = 600 * time.Millisecond
	testTake    = 100 * time.Millisecond
	testRetry   = 50 * time.Millisecond
	testHandoff = 300 * time.Millisecond
	testScan    = 100 * time.Millisecond
	testSettle  = 600 * time.Millisecond
	testMargin  = 100 * time.Millisecond
	// testPatience is how long a test waits for a condition that is a
	// matter of a few cadences.
	testPatience = 20 * time.Second
)

// pushSink is a push service that records what it got, by the path of
// the subscription URL (one path per account).
type pushSink struct {
	mu     sync.Mutex
	bodies map[string][]string
	status atomic.Int32
	// hold, when set, is closed by the test to let a send through; the
	// sends wait on it (a push service that hangs).
	hold atomic.Pointer[chan struct{}]
	// delay slows every send.
	delay atomic.Int64
}

func newPushSink() *pushSink {
	s := &pushSink{bodies: map[string][]string{}}
	s.status.Store(http.StatusCreated)

	return s
}

func (s *pushSink) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body)

	if h := s.hold.Load(); h != nil {
		<-*h
	}

	if d := s.delay.Load(); d > 0 {
		time.Sleep(time.Duration(d))
	}

	s.mu.Lock()
	s.bodies[r.URL.Path] = append(s.bodies[r.URL.Path], string(b))
	s.mu.Unlock()

	w.WriteHeader(int(s.status.Load()))
}

// release lets held sends through (the cleanup of a test that failed
// while it held them).
func (s *pushSink) release() {
	if h := s.hold.Swap(nil); h != nil {
		select {
		case <-*h:
		default:
			close(*h)
		}
	}
}

// count is how many pushes a path got (StateChanges and verifications).
func (s *pushSink) count(path string) int {
	s.mu.Lock()
	defer s.mu.Unlock()

	return len(s.bodies[path])
}

// stateChanges is how many StateChanges a path got.
func (s *pushSink) stateChanges(path string) int {
	s.mu.Lock()
	defer s.mu.Unlock()

	n := 0

	for _, b := range s.bodies[path] {
		if strings.Contains(b, typeStateChange) {
			n++
		}
	}

	return n
}

// pushCluster is a set of push workers over one store.
type pushCluster struct {
	t      *testing.T
	store  *memstore.Store
	kv     *memstore.AdminKV
	sender *webpush.Sender
	vapid  string
	sink   *pushSink
	ps     *httptest.Server
	log    *slog.Logger
	// customize changes the options of every worker started.
	customize func(*WorkerOptions)

	mu      sync.Mutex
	workers map[string]*pushWorkerRun
}

// pushWorkerRun is one running worker.
type pushWorkerRun struct {
	name   string
	cancel context.CancelFunc
	done   chan struct{}
	hooks  *pushTestHooks
	status *PushStatus
	opts   WorkerOptions
}

// held is how many partitions the worker holds as it reports it.
func (r *pushWorkerRun) held() int {
	n, _ := r.status.Get()

	return n
}

// watched is how many accounts the worker watches as it reports it.
func (r *pushWorkerRun) watched() int {
	_, n := r.status.Get()

	return n
}

// newPushCluster makes a cluster whose admin store is in the mode given.
func newPushCluster(t *testing.T, mode model.KVIndexMode) *pushCluster {
	t.Helper()

	sink := newPushSink()
	ps := httptest.NewTLSServer(sink)
	t.Cleanup(ps.Close)
	t.Cleanup(sink.release)

	sender, vapid := testSender(t)

	log := slog.New(slog.DiscardHandler)
	if os.Getenv("SHARDPOST_PUSHTEST_LOG") != "" {
		log = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	}

	c := &pushCluster{t: t, store: memstore.New(), kv: memstore.NewAdminKVMode(mode), sender: sender, vapid: vapid,
		sink: sink, ps: ps, log: log, workers: map[string]*pushWorkerRun{}}

	t.Cleanup(c.stopAll)

	return c
}

// options are the options of a worker named name.
func (c *pushCluster) options(name string, status *PushStatus) WorkerOptions {
	o := WorkerOptions{Store: c.store, KV: c.kv, Sender: c.sender, Instance: name, VAPIDPublicKey: c.vapid, Version: "test",
		Log:  c.log.With("worker", name),
		Scan: testScan, Verify: 50 * time.Millisecond, Poll: 300 * time.Millisecond, Coalesce: 10 * time.Millisecond,
		LeaseTTL: testLease, Take: testTake, Retry: testRetry, OrphanHold: 30 * time.Second, FullScan: 0,
		Rebalance: 100 * time.Millisecond, RebalanceSettle: testSettle, Handoff: testHandoff,
		MaxWatchedAccounts: 1000, StartRate: 100000, SelfStopMargin: testMargin, RoundBudget: time.Second,
		Rotate: c.kv, Status: status}

	if c.customize != nil {
		c.customize(&o)
	}

	return o
}

// start starts a worker; it runs until stopped, crashed or the test
// ends.
func (c *pushCluster) start(name string, customize ...func(*WorkerOptions, *pushTestHooks)) *pushWorkerRun {
	c.t.Helper()

	status := &PushStatus{}
	o := c.options(name, status)
	hooks := &pushTestHooks{crash: make(chan struct{})}

	for _, fn := range customize {
		fn(&o, hooks)
	}

	ctx, cancel := context.WithCancel(context.Background())
	r := &pushWorkerRun{name: name, cancel: cancel, done: make(chan struct{}), hooks: hooks, status: status, opts: o}

	go func() {
		defer close(r.done)

		if err := runPushWorker(ctx, o, hooks); err != nil {
			c.t.Errorf("worker %s: %v", name, err)
		}
	}()

	c.mu.Lock()
	c.workers[name] = r
	c.mu.Unlock()

	return r
}

// stop ends a worker in order and waits for it.
func (c *pushCluster) stop(r *pushWorkerRun) {
	c.t.Helper()

	r.cancel()

	select {
	case <-r.done:
	case <-time.After(30 * time.Second):
		c.t.Fatalf("worker %s did not end", r.name)
	}

	c.mu.Lock()
	delete(c.workers, r.name)
	c.mu.Unlock()
}

// crash ends a worker without the orderly end: its leases expire, its
// record stays until it expires.
func (c *pushCluster) crash(r *pushWorkerRun) {
	c.t.Helper()

	close(r.hooks.crash)

	select {
	case <-r.done:
	case <-time.After(30 * time.Second):
		c.t.Fatalf("worker %s did not end", r.name)
	}

	c.mu.Lock()
	delete(c.workers, r.name)
	c.mu.Unlock()
}

// stopAll ends every worker (the cleanup).
func (c *pushCluster) stopAll() {
	c.mu.Lock()
	runs := make([]*pushWorkerRun, 0, len(c.workers))

	for _, r := range c.workers {
		runs = append(runs, r)
	}
	c.mu.Unlock()

	for _, r := range runs {
		r.cancel()
	}

	for _, r := range runs {
		<-r.done
	}
}

// running are the workers running, by name.
func (c *pushCluster) running() []*pushWorkerRun {
	c.mu.Lock()
	defer c.mu.Unlock()

	out := make([]*pushWorkerRun, 0, len(c.workers))
	for _, r := range c.workers {
		out = append(out, r)
	}

	slices.SortFunc(out, func(a, b *pushWorkerRun) int { return strings.Compare(a.name, b.name) })

	return out
}

// account makes an account with a mailbox and a verified subscription,
// and returns its user; the sink records its pushes under /<account>.
func (c *pushCluster) account(name string, types ...string) model.User {
	c.t.Helper()

	u, err := c.store.User(name)
	if err != nil {
		c.t.Fatal(err)
	}

	if err := u.CreateMailbox(inboxName, nil); err != nil {
		c.t.Fatal(err)
	}

	c.subscribe(name, types...)

	return u
}

// path is the sink path of an account.
func path(account string) string { return "/" + url.PathEscape(account) }

// subscribe adds a verified subscription to an account and returns its
// id. types nil subscribes to every type.
func (c *pushCluster) subscribe(account string, types ...string) string {
	c.t.Helper()

	id := "P" + randomHex(6)
	now := time.Now().UTC()
	rec := &pushRecord{ID: id, Account: account, DeviceClientID: "dev", URL: c.ps.URL + path(account), Code: "c",
		Expires: now.Add(time.Hour), Created: now, VAPIDKey: c.vapid, Verified: true}

	if len(types) > 0 {
		rec.Types = types
	}

	err := updatePushAccount(c.kv, account, func(pa *pushAccount) error {
		pa.Subs = append(pa.Subs, rec)

		return nil
	})
	if err != nil {
		c.t.Fatal(err)
	}

	return id
}

// members reads the membership as the workers wrote it.
func (c *pushCluster) members() map[string]memberRecord {
	c.t.Helper()

	m := newMembership(c.kv.Omap(), "reader", testLease, time.Now, c.log)
	defer m.close()

	members, err := m.load()
	if err != nil {
		c.t.Fatal(err)
	}

	return members
}

// seen reads the state kept for an account.
func (c *pushCluster) seen(account string) pushSeen {
	c.t.Helper()

	raw, err := c.kv.Get(pushSeenKey(account))
	if err != nil {
		return pushSeen{}
	}

	s, _ := decodeSeen(raw)

	return s
}

// awaitCond polls cond until it holds or patience is over.
func awaitCond(t *testing.T, patience time.Duration, what string, cond func() bool) {
	t.Helper()

	deadline := time.Now().Add(patience)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}

		time.Sleep(10 * time.Millisecond)
	}
}

// converged reports whether the running workers hold every partition
// between them, each its share.
func (c *pushCluster) converged() bool {
	runs := c.running()
	names := make([]string, 0, len(runs))

	for _, r := range runs {
		names = append(names, r.name)
	}

	targets := pushTargets(defaultPushPartitions, names)

	for _, r := range runs {
		if r.held() != targets[r.name] {
			return false
		}
	}

	return true
}

// watchedAll waits until the running workers watch n accounts between
// them.
func (c *pushCluster) watchedAll(n int) {
	c.t.Helper()

	awaitCond(c.t, testPatience, fmt.Sprintf("%d accounts watched", n), func() bool {
		total := 0
		for _, r := range c.running() {
			total += r.watched()
		}

		return total == n
	})
}

// disjoint checks that the membership lists every partition once.
func (c *pushCluster) disjoint() {
	c.t.Helper()

	seen := map[int]string{}

	for name, rec := range c.members() {
		for _, p := range rec.Held {
			if other, ok := seen[p]; ok {
				c.t.Fatalf("partition %d held by %s and %s", p, other, name)
			}

			seen[p] = name
		}
	}

	if len(seen) != defaultPushPartitions {
		c.t.Fatalf("%d partitions held, want %d", len(seen), defaultPushPartitions)
	}
}

// pushedAfter delivers a message to the account and waits for a push
// to follow it; it returns the time it took.
func (c *pushCluster) pushedAfter(u model.User, account string, patience time.Duration) time.Duration {
	c.t.Helper()

	before := c.sink.stateChanges(path(account))
	start := time.Now()

	deliver(c.t, u, inboxName, fmt.Sprintf("Subject: %d\r\n\r\nx\r\n", before))

	awaitCond(c.t, patience, "the push of "+account, func() bool { return c.sink.stateChanges(path(account)) > before })

	return time.Since(start)
}

// --- the unit tests ----------------------------------------------------------

// TestPushPartitionIsTheIndexShard: the partition of an account is the
// shard of its record in the hint index, for every shard count that
// divides the index's, and for names with slashes, percent signs and
// umlauts.
func TestPushPartitionIsTheIndexShard(t *testing.T) {
	names := []string{"alice", "a/b", "100%", "grüße@example.org", "x y", "日本"}
	for i := range 100000 {
		names = append(names, fmt.Sprintf("user%d@example.org", i))
	}

	for _, name := range names {
		esc := url.PathEscape(name)

		for _, n := range []int{64, 32, 16, 8, 1} {
			shard := model.IndexShard(esc, n)
			if PushPartition(name, n) != shard {
				t.Fatalf("partition of %q among %d: %d, shard %d", name, n, PushPartition(name, n), shard)
			}
		}

		if PushPartition(name, kvindex.PushCollection().Shards) != model.IndexShard(esc, kvindex.PushCollection().Shards) {
			t.Fatalf("partition of %q is not its index shard", name)
		}
	}
}

// TestPushTargets: the shares of 64 partitions among n pods, with no
// pod left empty.
func TestPushTargets(t *testing.T) {
	for _, tc := range []struct {
		n    int
		want []int
	}{
		{3, []int{22, 21, 21}},
		{5, []int{13, 13, 13, 13, 12}},
		{7, []int{10, 9, 9, 9, 9, 9, 9}},
		{1, []int{64}},
		{64, slices.Repeat([]int{1}, 64)},
	} {
		names := make([]string, tc.n)
		for i := range names {
			names[i] = fmt.Sprintf("pod-%02d", i)
		}

		targets := pushTargets(64, names)

		got := make([]int, tc.n)
		for i, name := range names {
			got[i] = targets[name]
		}

		if !slices.Equal(got, tc.want) {
			t.Errorf("%d pods: %v, want %v", tc.n, got, tc.want)
		}
	}
}

// TestMergeSeen: two writers merge to max per subscription; the higher
// epoch wins whole; an acknowledgement present on one side stays; the
// counter-example of the first draft (A delivers and acknowledges, B
// writes the old state at the same time) leaves no backlog.
func TestMergeSeen(t *testing.T) {
	a := pushSeen{Seq: 10, Digest: "d", Ack: map[string]pushAck{"s1": {Seq: 10, Digest: "d"}, "s2": {Seq: 4}}}
	b := pushSeen{Seq: 8, Digest: "c", Ack: map[string]pushAck{"s1": {Seq: 8, Digest: "c"}, "s3": {Seq: 8}}}
	old := pushSeen{Seq: 10, Digest: "d", Ack: map[string]pushAck{"s1": {Seq: 8, Digest: "d"}}}
	newer := pushSeen{Seq: 3, Epoch: 1, Ack: map[string]pushAck{"s1": {Seq: 3}}}
	live := map[string]bool{"s1": true, "s2": true, "s3": true}

	for _, tc := range []struct {
		name  string
		cur   *pushSeen
		mine  pushSeen
		live  map[string]bool
		want  pushSeen
		adopt bool
	}{
		{"max per subscription", &a, b, live,
			pushSeen{Seq: 10, Digest: "d", Ack: map[string]pushAck{"s1": {Seq: 10, Digest: "d"}, "s2": {Seq: 4}, "s3": {Seq: 8}}}, false},
		{"the delivered state is not taken back", &a, old, live,
			pushSeen{Seq: 10, Digest: "d", Ack: map[string]pushAck{"s1": {Seq: 10, Digest: "d"}, "s2": {Seq: 4}}}, false},
		{"dead subscriptions are left out", &a, b, map[string]bool{"s1": true},
			pushSeen{Seq: 10, Digest: "d", Ack: map[string]pushAck{"s1": {Seq: 10, Digest: "d"}}}, false},
		{"the stored higher epoch wins whole", &newer, a, live, newer, true},
		{"the writer's higher epoch wins whole", &a, newer, live, newer, false},
		{"the first write", nil, a, live, a, false},
	} {
		got, adopt := mergeSeen(tc.cur, tc.mine, tc.live)
		if adopt != tc.adopt || !sameSeen(got, tc.want) {
			t.Errorf("%s: %+v adopt %v, want %+v adopt %v", tc.name, got, adopt, tc.want, tc.adopt)
		}
	}

	m, _ := mergeSeen(&a, b, live)
	if m.behind("s1") || !m.behind("s2") || !m.behind("s3") {
		t.Fatalf("backlog after merge %+v", m)
	}
}

// sameSeen compares the fields of two records the merge decides on.
func sameSeen(a, b pushSeen) bool {
	if a.Seq != b.Seq || a.Digest != b.Digest || a.Epoch != b.Epoch || len(a.Ack) != len(b.Ack) {
		return false
	}

	for id, x := range a.Ack {
		if b.Ack[id] != x {
			return false
		}
	}

	return true
}

// TestClassifyPartition: the classes of §4.4 from a membership view.
func TestClassifyPartition(t *testing.T) {
	now := time.Now()
	v := &membersView{readAt: now, members: map[string]memberRecord{
		"fresh":   {Seen: now, Held: []int{1}},
		"leaving": {Seen: now, Held: []int{2}, Leaving: true},
		"stale":   {Seen: now.Add(-2 * testLease), Held: []int{3}},
	}}

	for p, want := range map[int]partitionClass{1: classHeld, 2: classShedding, 3: classOrphan, 4: classFree} {
		if got := classifyPartition(v, p, now, testLease, 0, testTake); got != want {
			t.Errorf("partition %d: class %d, want %d", p, got, want)
		}
	}

	if got := classifyPartition(v, 3, now, testLease, 2*testTake, testTake); got != classUnknown {
		t.Errorf("a stale holder with an old view: class %d, want unknown", got)
	}

	if n := unowned(v, 64, now, testLease); n != 62 {
		t.Errorf("unowned %d, want 62", n)
	}
}

// --- the cluster tests -------------------------------------------------------

// TestPushClusterConverges: three workers settle at 22/21/21 with every
// partition held once and every account watched once; a delivery into
// any account is pushed; when one worker ends in order the other two
// take its partitions, each moving once, and no delivery during the
// hand-over is lost; a crashed worker's partitions are taken over
// within the lease and a cadence.
func TestPushClusterConverges(t *testing.T) {
	c := newPushCluster(t, model.KVIndexRead)

	accounts := map[string]model.User{}

	for i := range 24 {
		name := fmt.Sprintf("acct%02d", i)
		accounts[name] = c.account(name)
	}

	w1, w2, w3 := c.start("w1"), c.start("w2"), c.start("w3")

	awaitCond(t, testPatience, "convergence at 22/21/21", c.converged)
	c.disjoint()
	c.watchedAll(len(accounts))

	for name, u := range accounts {
		c.pushedAfter(u, name, 5*time.Second)
	}

	if n := c.kv.Listings(); n != 0 {
		t.Errorf("the admin store was listed %d times in read mode", n)
	}

	names := slices.Sorted(func(yield func(string) bool) {
		for name := range accounts {
			if !yield(name) {
				return
			}
		}
	})

	c.scaleDown(w1, w2, w3, accounts, names)
	c.crashTakeover(w1, w2, accounts, names)
}

// scaleDown ends w3 in order while deliveries go on: each partition of
// w3 moves once, nothing is lost, and a hand-over repeats at most one
// push.
func (c *pushCluster) scaleDown(w1, w2, w3 *pushWorkerRun, accounts map[string]model.User, names []string) {
	c.t.Helper()

	taken := w1.hooks.of(eventTaken) + w2.hooks.of(eventTaken)
	stopped := make(chan struct{})

	go func() {
		defer close(stopped)

		c.stop(w3)
	}()

	for range 4 {
		for _, name := range names {
			c.pushedAfter(accounts[name], name, 10*time.Second)
		}
	}

	<-stopped

	awaitCond(c.t, testPatience, "convergence at 32/32", c.converged)
	c.disjoint()

	if moved := w1.hooks.of(eventTaken) + w2.hooks.of(eventTaken) - taken; moved != 21 {
		c.t.Errorf("%d takes after w3 left, want 21 (each partition once)", moved)
	}

	if orphan := w1.hooks.of(eventTakenOrphan) + w2.hooks.of(eventTakenOrphan); orphan != 0 {
		c.t.Errorf("%d partitions taken as orphans in an orderly hand-over", orphan)
	}

	// Duplicates: a push per round, at most one more per hand-over.
	for _, name := range names {
		if n := c.sink.stateChanges(path(name)); n > 4+1+2 {
			c.t.Errorf("%s got %d pushes for 5 deliveries and at most one hand-over", name, n)
		}
	}
}

// crashTakeover crashes w1: its leases expire, w2 takes them as orphans
// within the lease plus a cadence, and deliveries are pushed again.
func (c *pushCluster) crashTakeover(w1, w2 *pushWorkerRun, accounts map[string]model.User, names []string) {
	c.t.Helper()

	start := time.Now()

	c.crash(w1)

	awaitCond(c.t, testPatience, "w2 holding everything", func() bool { return w2.held() == defaultPushPartitions })

	if took := time.Since(start); took > testLease+2*testTake+2*time.Second {
		c.t.Errorf("the take-over after the crash took %s", took)
	}

	if w2.hooks.of(eventTakenOrphan) == 0 {
		c.t.Error("no partition was taken as an orphan after the crash")
	}

	for _, name := range names {
		c.pushedAfter(accounts[name], name, 10*time.Second)
	}
}

// TestPushLeaseLost: a lost lease (every lock revoked) ends the
// partitions, and they are taken again; pushes go on.
func TestPushLeaseLost(t *testing.T) {
	c := newPushCluster(t, model.KVIndexRead)
	u := c.account("alice")
	w := c.start("w1")

	awaitCond(t, testPatience, "w1 holding everything", func() bool { return w.held() == defaultPushPartitions })
	c.watchedAll(1)
	c.pushedAfter(u, "alice", 5*time.Second)

	c.kv.RevokeLocks()

	awaitCond(t, testPatience, "the leases lost", func() bool { return w.hooks.of(eventLost) == defaultPushPartitions })
	awaitCond(t, testPatience, "w1 holding everything again", func() bool { return w.held() == defaultPushPartitions })
	c.watchedAll(1)
	c.pushedAfter(u, "alice", 5*time.Second)
}

// TestPushSelfStopReleases: a renewal that hangs beyond the deadline
// makes the holder stop the partition (expired) and release it, even
// though the renewal comes back successful later; the partition is
// taken again and served, not left dead.
func TestPushSelfStopReleases(t *testing.T) {
	c := newPushCluster(t, model.KVIndexRead)
	u := c.account("alice")
	p := PushPartition("alice", defaultPushPartitions)

	var (
		hung atomic.Int32
		gate = make(chan struct{})
	)

	c.kv.Locks().SetHook(func(op, name string) error {
		if op == memstore.LockOpRenew && name == pushLockName(p) && hung.CompareAndSwap(0, 1) {
			<-gate // the renewal hangs until the test lets it through
		}

		return nil
	})

	w := c.start("w1")

	awaitCond(t, testPatience, "w1 holding everything", func() bool { return w.held() == defaultPushPartitions })
	c.watchedAll(1)
	awaitCond(t, testPatience, "the renewal to hang", func() bool { return hung.Load() == 1 })

	// The holder stops the partition itself before the lease is up: its
	// record no longer names it, while the release waits for the hung
	// renewal.
	awaitCond(t, testLease, "the self-stop", func() bool { return !c.members()["w1"].holds(p) })

	if w.hooks.of(eventExpired) != 0 {
		t.Fatal("the expiry was reported before the release ended")
	}

	// The other partitions are served meanwhile.
	bob := c.account("bob")
	if PushPartition("bob", defaultPushPartitions) != p {
		c.watchedAll(1) // bob; alice is not watched while her partition is being given up
		c.pushedAfter(bob, "bob", 5*time.Second)
	}

	// The hung renewal comes through: the release waits for it and
	// unlocks; the partition is taken again and alice is pushed to.
	close(gate)

	awaitCond(t, testPatience, "the expiry", func() bool { return w.hooks.of(eventExpired) == 1 })
	awaitCond(t, testPatience, "the partition taken again", func() bool { return w.held() == defaultPushPartitions })
	c.watchedAll(2)
	c.pushedAfter(u, "alice", 5*time.Second)
}

// TestPushMembersHang: a membership object that hangs freezes taking
// and shedding (members_stale), while the held partitions go on being
// renewed and served; once it answers again the distribution settles.
func TestPushMembersHang(t *testing.T) {
	c := newPushCluster(t, model.KVIndexRead)
	u := c.account("alice")
	w1, w2 := c.start("w1"), c.start("w2")

	awaitCond(t, testPatience, "convergence at 32/32", c.converged)
	c.watchedAll(1)

	gate := make(chan struct{})

	c.kv.Backend().SetHook(func(op, oid string) error {
		if oid == pushMembersOID && op == "read_page" {
			<-gate
		}

		return nil
	})

	awaitCond(t, testPatience, "the membership stale", func() bool { return w1.hooks.of(eventMembersStale) > 0 && w2.hooks.of(eventMembersStale) > 0 })

	tried := w1.hooks.lockTried.Load() + w2.hooks.lockTried.Load()

	for range 3 {
		c.pushedAfter(u, "alice", 5*time.Second)
	}

	if w1.held()+w2.held() != defaultPushPartitions || w1.hooks.lockTried.Load()+w2.hooks.lockTried.Load() != tried {
		t.Fatalf("partitions taken or given up while the membership hung: %d/%d held, %d lock attempts",
			w1.held(), w2.held(), w1.hooks.lockTried.Load()+w2.hooks.lockTried.Load()-tried)
	}

	close(gate)
	c.kv.Backend().SetHook(nil)

	awaitCond(t, testPatience, "convergence after the hang", c.converged)
	c.watchedAll(1)
	c.pushedAfter(u, "alice", 5*time.Second)
}

// TestPushRotationKeepsWatchers: a key rotation moves the partition
// leases to the new handle without restarting a watcher; a lease taken
// with LockQueue in the same store is lost, as before.
func TestPushRotationKeepsWatchers(t *testing.T) {
	c := newPushCluster(t, model.KVIndexRead)
	u := c.account("alice")
	w := c.start("w1")

	awaitCond(t, testPatience, "w1 holding everything", func() bool { return w.held() == defaultPushPartitions })
	c.watchedAll(1)
	c.pushedAfter(u, "alice", 5*time.Second)

	other, err := c.kv.LockQueue("other", time.Minute)
	if err != nil {
		t.Fatal(err)
	}

	defer other.Release()

	starts := w.hooks.watcherStarts.Load()

	c.kv.Rotate()

	awaitCond(t, testPatience, "every lease moved", func() bool { return w.hooks.of(eventRotated) == defaultPushPartitions })

	if cur, old := c.kv.LockHandles(); cur != defaultPushPartitions || old != 0 {
		t.Fatalf("leases on the current handle %d, on old ones %d", cur, old)
	}

	if !other.Lost() {
		t.Error("the LockQueue lease survived the rotation")
	}

	if w.hooks.of(eventRotateFailed) != 0 || w.hooks.watcherStarts.Load() != starts {
		t.Errorf("rotation: %d failed, %d watchers started", w.hooks.of(eventRotateFailed), w.hooks.watcherStarts.Load()-starts)
	}

	c.pushedAfter(u, "alice", 5*time.Second)

	// A rotation whose retake is refused for one partition ends that
	// partition's watchers alone.
	p := PushPartition("alice", defaultPushPartitions)

	c.kv.Locks().SetHook(func(op, name string) error {
		if op == memstore.LockOpLock && name == pushLockName(p) {
			return model.ErrLocked
		}

		return nil
	})

	c.kv.Rotate()

	awaitCond(t, testPatience, "the refused retake", func() bool { return w.hooks.of(eventRotateFailed) == 1 })

	c.kv.Locks().SetHook(nil)

	awaitCond(t, testPatience, "the partition taken again", func() bool { return w.held() == defaultPushPartitions })
	c.watchedAll(1)
	c.pushedAfter(u, "alice", 5*time.Second)
}

// TestPushRestartDetection: a worker that starts under the name of one
// that crashed waits for the crashed record to expire; the other worker
// takes the orphaned partitions and keeps them, and the restarted one
// holds nothing while the orphan hold lasts. Without the detection
// (the counter-check) the contested rule still gives them to the
// healthy worker.
func TestPushRestartDetection(t *testing.T) {
	c := newPushCluster(t, model.KVIndexRead)
	u := c.account("alice")
	w1, w2 := c.start("w1"), c.start("w2")

	awaitCond(t, testPatience, "convergence at 32/32", c.converged)
	c.watchedAll(1)

	oldHeld := c.members()["w1"].Held

	crashed := time.Now()

	c.crash(w1)

	again := c.start("w1")

	// While its crashed record is fresh the restarted worker tries
	// nothing; the healthy worker takes the partitions as orphans once
	// the record and the locks expired, and the restarted one gets what
	// the healthy one did not reach first, never its old partitions back
	// by right.
	time.Sleep(testLease / 2)

	if n := again.hooks.lockTried.Load(); n != 0 {
		t.Fatalf("the restarted worker tried %d locks under its fresh crashed record", n)
	}

	awaitCond(t, testPatience, "w2 holding everything", func() bool { return w2.held() == defaultPushPartitions })

	if took := time.Since(crashed); took > 2*testLease+2*testTake+2*time.Second {
		t.Errorf("the take-over took %s", took)
	}

	if w2.hooks.of(eventTakenOrphan) != 32 {
		t.Errorf("w2 took %d partitions as orphans, want 32", w2.hooks.of(eventTakenOrphan))
	}

	if again.held() != 0 || again.hooks.of(eventTaken)+again.hooks.of(eventTakenOrphan) != 0 {
		t.Errorf("the restarted worker took %d partitions", again.held())
	}

	c.watchedAll(1)
	c.pushedAfter(u, "alice", 5*time.Second)

	// What w2 took as a replacement it keeps while the orphan hold
	// lasts: it may shed its own partitions to even the load once the
	// set of pods has settled, never the crashed pod's old ones.
	time.Sleep(testSettle + 3*testTake)

	for _, p := range c.members()["w1"].Held {
		if slices.Contains(oldHeld, p) {
			t.Errorf("the restarted worker got its old partition %d back within the orphan hold", p)
		}
	}

	if w2.held() < 32 {
		t.Errorf("w2 holds %d, below its share", w2.held())
	}

	// The counter-check: without the detection the restarted worker
	// writes an empty record and lies under its share, but the contested
	// rule lets w2 take the partitions as well; nobody holds a partition
	// twice, and alice is served.
	c.stop(again)
	c.crash(w2)

	w3 := c.start("w3")
	w2b := c.start("w2", func(_ *WorkerOptions, h *pushTestHooks) { h.noRestartDetection = true })

	awaitCond(t, testPatience, "every partition held", func() bool { return w3.held()+w2b.held() == defaultPushPartitions })
	c.disjoint()
	c.watchedAll(1)
	c.pushedAfter(u, "alice", 5*time.Second)
}

// TestPushShutdownOrder: a worker with eight partitions ends in order:
// first its record with leaving and the eight partitions, then three
// waves (3/3/2), each the record without the wave's partitions, then
// the unlocks, then one control notify, at the hand-off period; last
// the deletion of the record. The other worker takes every partition
// without an orphan take, and a delivery during the end is pushed.
func TestPushShutdownOrder(t *testing.T) {
	c, u, trail, leaver, other := shutdownSetup(t)

	taken := other.hooks.of(eventTaken)
	start := time.Now()
	stopped := make(chan struct{})

	go func() {
		defer close(stopped)

		c.stop(leaver)
	}()

	c.pushedAfter(u, "alice", 10*time.Second)

	<-stopped

	took := time.Since(start)

	awaitCond(t, testPatience, "w2 holding everything", func() bool { return other.held() == defaultPushPartitions })

	got, recs := trail.snapshot()

	if helds := heldsOf(t, recs); !slices.Equal(helds, []int{8, 5, 2, 0}) {
		t.Errorf("held per record during the end: %v, want 8 5 2 0 (trail %q)", helds, got)
	}

	if waves := countWaves(t, got); waves != 3 {
		t.Errorf("%d waves, want 3: %q", waves, got)
	}

	if took > 3*testHandoff+2*testTake+time.Second {
		t.Errorf("the end took %s", took)
	}

	if other.hooks.of(eventTaken)-taken != 8 || other.hooks.of(eventTakenOrphan) != 0 {
		t.Errorf("w2 took %d partitions (%d as orphans), want 8 and 0", other.hooks.of(eventTaken)-taken, other.hooks.of(eventTakenOrphan))
	}

	if _, ok := c.members()["w1"]; ok {
		t.Error("the record of the worker that left is still there")
	}
}

// shutdownSetup starts a worker with eight partitions and one with the
// rest, with a trail of the writes, unlocks and notifies.
func shutdownSetup(t *testing.T) (*pushCluster, model.User, *shutdownTrail, *pushWorkerRun, *pushWorkerRun) {
	t.Helper()

	c := newPushCluster(t, model.KVIndexRead)
	u := c.account("alice")
	trail := &shutdownTrail{}

	c.kv.Locks().SetHook(func(op, name string) error {
		if op == memstore.LockOpUnlock {
			trail.note("unlock " + name)
		}

		return nil
	})

	ctl, err := c.kv.Omap().Watch(pushCtlOID)
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(ctl.Close)

	go func() {
		for range ctl.Events() {
			trail.note("notify")
		}
	}()

	leaver := c.start("w1", func(o *WorkerOptions, h *pushTestHooks) {
		o.MaxPartitions = 8
		h.onMembersWrite = trail.record
	})
	other := c.start("w2")

	awaitCond(t, testPatience, "w1 at 8 and w2 at 56", func() bool { return leaver.held() == 8 && other.held() == 56 })
	trail.reset()

	return c, u, trail, leaver, other
}

// shutdownTrail records the writes, unlocks and notifies of an end.
type shutdownTrail struct {
	mu      sync.Mutex
	trail   []string
	records []memberRecord
}

func (s *shutdownTrail) note(line string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.trail = append(s.trail, line)
}

func (s *shutdownTrail) record(rec *memberRecord) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.records = append(s.records, *rec)
	s.trail = append(s.trail, fmt.Sprintf("write leaving=%v held=%d", rec.Leaving, len(rec.Held)))
}

func (s *shutdownTrail) reset() {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.trail, s.records = nil, nil
}

func (s *shutdownTrail) snapshot() ([]string, []memberRecord) {
	s.mu.Lock()
	defer s.mu.Unlock()

	return append([]string(nil), s.trail...), append([]memberRecord(nil), s.records...)
}

// heldsOf are the held counts of the records of an end, runs collapsed;
// every record has to be a leaving one.
func heldsOf(t *testing.T, recs []memberRecord) []int {
	t.Helper()

	var helds []int

	for _, rec := range recs {
		if !rec.Leaving {
			t.Fatalf("a record without leaving during the end: %+v", rec)
		}

		if len(helds) == 0 || helds[len(helds)-1] != len(rec.Held) {
			helds = append(helds, len(rec.Held))
		}
	}

	return helds
}

// countWaves counts the waves of a trail: a write (not the leaving one
// with every partition), then the unlocks, then one notify.
func countWaves(t *testing.T, got []string) int {
	t.Helper()

	waves := 0

	for i := 0; i < len(got); {
		if !strings.HasPrefix(got[i], "write") || strings.HasSuffix(got[i], "held=8") {
			i++

			continue
		}

		j := i + 1
		for j < len(got) && strings.HasPrefix(got[j], "unlock") {
			j++
		}

		if j == i+1 || j >= len(got) || got[j] != "notify" {
			t.Fatalf("wave %d out of order at %d: %q", waves+1, i, got)
		}

		waves++
		i = j + 1
	}

	return waves
}

// TestPushHandoffOrderCounterCheck: with the notify before the
// membership write, the taker sees the partition still held and has
// to read again; it still takes it, within the recheck.
func TestPushHandoffOrderCounterCheck(t *testing.T) {
	c := newPushCluster(t, model.KVIndexRead)
	c.account("alice")

	w1 := c.start("w1", func(_ *WorkerOptions, h *pushTestHooks) { h.swapHandoffOrder = true })
	w2 := c.start("w2")

	awaitCond(t, testPatience, "convergence at 32/32", c.converged)

	// w2 does not shed: only w1 does, with the swapped order, when a
	// third worker arrives.
	w3 := c.start("w3")

	awaitCond(t, testPatience, "convergence at 22/21/21", c.converged)

	if w1.held() != 22 || w2.held() != 21 || w3.held() != 21 {
		t.Fatalf("held %d/%d/%d", w1.held(), w2.held(), w3.held())
	}
}

// TestPushCrashLoopSelfStop: a worker whose release hangs (the renewal
// of its lease hangs) does not keep the others from taking the
// partition: after the lease expires in the store, the contested rule
// gives it to a healthy worker.
func TestPushCrashLoopSelfStop(t *testing.T) {
	c := newPushCluster(t, model.KVIndexRead)
	u := c.account("alice")
	p := PushPartition("alice", defaultPushPartitions)

	var hung atomic.Int32

	gate := make(chan struct{})

	c.kv.Locks().SetHook(func(op, name string) error {
		if op == memstore.LockOpRenew && name == pushLockName(p) && hung.CompareAndSwap(0, 1) {
			<-gate
		}

		return nil
	})

	w1 := c.start("w1")

	awaitCond(t, testPatience, "w1 holding everything", func() bool { return w1.held() == defaultPushPartitions })
	c.watchedAll(1)
	awaitCond(t, testPatience, "the renewal to hang", func() bool { return hung.Load() == 1 })
	awaitCond(t, testLease, "the self-stop", func() bool { return !c.members()["w1"].holds(p) })

	c.start("w2")

	awaitCond(t, testPatience, "w2 holding the partition", func() bool {
		rec := c.members()["w2"]

		return rec.holds(p)
	})

	c.watchedAll(1)
	c.pushedAfter(u, "alice", 5*time.Second)
	close(gate)
}

// TestPushMaxWatchedAccounts: a worker takes no partition that would
// put it over its bound; with every worker full, partitions stay
// unowned.
func TestPushMaxWatchedAccounts(t *testing.T) {
	c := newPushCluster(t, model.KVIndexRead)

	for i := range 64 {
		c.account(fmt.Sprintf("acct%02d", i))
	}

	// 64 accounts over 64 partitions: about one per partition; a bound
	// of 100 accounts for the first worker (per the estimate it takes
	// its share), of 100 for the second too.
	c.customize = func(o *WorkerOptions) { o.MaxWatchedAccounts = 100 }
	w1 := c.start("w1")

	awaitCond(t, testPatience, "w1 holding everything", func() bool { return w1.held() == defaultPushPartitions })

	c.customize = func(o *WorkerOptions) { o.MaxWatchedAccounts = 100; o.MaxPartitions = 10 }
	w2 := c.start("w2")

	awaitCond(t, testPatience, "w2 at 10", func() bool { return w2.held() == 10 })

	time.Sleep(testSettle + 5*testTake)

	if w1.held() != 54 || w2.held() != 10 {
		t.Fatalf("held %d/%d, want 54/10 (no shed to a full worker)", w1.held(), w2.held())
	}
}

// TestPushScanReadsOnlyChanges: in read mode the scan reads a record
// when its checksum changed, when the value has none, or in the full
// pass; an index entry without record is removed; the scan reads one
// shard object per partition.
func TestPushScanReadsOnlyChanges(t *testing.T) {
	c := newPushCluster(t, model.KVIndexRead)
	u := c.account("alice")

	gets := &countingKV{AdminKV: c.kv}
	c.customize = func(o *WorkerOptions) { o.KV = gets; o.FullScan = 0 }

	w := c.start("w1")

	awaitCond(t, testPatience, "w1 holding everything", func() bool { return w.held() == defaultPushPartitions })
	c.watchedAll(1)
	c.pushedAfter(u, "alice", 5*time.Second)

	n := gets.of(pushAcctKey("alice"))

	time.Sleep(5 * testScan)

	if gets.of(pushAcctKey("alice")) != n {
		t.Fatalf("the record was read %d more times with an unchanged checksum", gets.of(pushAcctKey("alice"))-n)
	}

	// A change: one more read.
	c.subscribe("alice")

	awaitCond(t, testPatience, "the read after the change", func() bool { return gets.of(pushAcctKey("alice")) == n+1 })

	time.Sleep(5 * testScan)

	if gets.of(pushAcctKey("alice")) != n+1 {
		t.Fatalf("the record was read %d more times after one change", gets.of(pushAcctKey("alice"))-n-1)
	}

	// A value without checksum (an older front-end): read every scan.
	ix := pushIndex(c.kv)
	if err := ix.Set(url.PathEscape("alice"), []byte(`{"verified":true}`)); err != nil {
		t.Fatal(err)
	}

	m := gets.of(pushAcctKey("alice"))

	awaitCond(t, testPatience, "reads without a checksum", func() bool { return gets.of(pushAcctKey("alice")) >= m+3 })

	// An entry without record is removed.
	if err := ix.Set(url.PathEscape("ghost"), []byte(`{"verified":true,"sum":1}`)); err != nil {
		t.Fatal(err)
	}

	awaitCond(t, testPatience, "the ghost entry removed", func() bool {
		_, err := ix.Get(url.PathEscape("ghost"))

		return err != nil
	})

	if n := c.kv.Listings(); n != 0 {
		t.Errorf("the admin store was listed %d times", n)
	}
}

// TestPushFullPass: a record whose checksum in the index does not tell
// (an older front-end left the value unchanged) is read by the full
// pass.
func TestPushFullPass(t *testing.T) {
	c := newPushCluster(t, model.KVIndexRead)
	c.account("alice")

	c.customize = func(o *WorkerOptions) { o.FullScan = 3 * testScan }

	w := c.start("w1")

	awaitCond(t, testPatience, "w1 holding everything", func() bool { return w.held() == defaultPushPartitions })
	awaitCond(t, testPatience, "alice watched", func() bool { return w.watched() == 1 })

	// The front-end of before writes the record without touching the
	// checksum: a second account's record appears under an index value
	// whose checksum says "unchanged".
	raw, _ := c.kv.Get(pushAcctKey("alice"))

	var pa pushAccount

	_ = json.Unmarshal(raw, &pa)

	pa.Subs = nil

	b, _ := json.Marshal(pa)

	if err := c.kv.Raw().Update(pushAcctKey("alice"), func([]byte) ([]byte, error) { return b, nil }); err != nil {
		t.Fatal(err)
	}

	awaitCond(t, testPatience, "the full pass to notice", func() bool { return w.watched() == 0 })
}

// TestPushPayloadWake: a subscription verified through the front-end
// is watched within a second without a scan; a notify for an account
// of another partition is ignored; an overflow of notifies leads to a
// scan and loses nothing.
func TestPushPayloadWake(t *testing.T) {
	c := newPushCluster(t, model.KVIndexRead)

	s := newTestServer(t, func(o *Options) {
		o.Store = c.store
		o.Push = PushOptions{KV: c.kv, VAPIDPublicKey: c.vapid, AllowInternalURLs: true}
	})

	u, err := c.store.User("test")
	if err != nil {
		t.Fatal(err)
	}

	if err := u.CreateMailbox(inboxName, nil); err != nil {
		t.Fatal(err)
	}

	c.customize = func(o *WorkerOptions) { o.Scan = time.Hour }

	w := c.start("w1")

	awaitCond(t, testPatience, "w1 holding everything", func() bool { return w.held() == defaultPushPartitions })

	res := mailCall(t, s, "PushSubscription/set", `{"create":{"p":{"deviceClientId":"dev","url":"`+c.ps.URL+`/test"}}}`)
	id, _ := res["created"].(map[string]any)["p"].(map[string]any)[propID].(string)

	awaitCond(t, 2*time.Second, "the verification", func() bool { return c.sink.count("/test") == 1 })

	start := time.Now()

	mailCall(t, s, "PushSubscription/set", `{"update":{"`+id+`":{"verificationCode":"`+storedCode(t, c.kv, id)+`"}}}`)

	awaitCond(t, 2*time.Second, "the watcher", func() bool { return w.watched() == 1 })

	if time.Since(start) > time.Second {
		t.Errorf("the watcher took %s to start", time.Since(start))
	}

	c.pushedAfter(u, "test", 5*time.Second)

	// An overflow: many notifies at once become a scan, which finds a
	// subscription written without a notify (an older front-end).
	c.subscribe("alice")

	alice, _ := c.store.User("alice")
	_ = alice.CreateMailbox(inboxName, nil)

	data, _ := json.Marshal(pushWake{Account: "nobody"})

	for range 3 * sortedindex.PayloadBuffer {
		c.kv.Omap().NotifyPayload(pushLockOID(PushPartition("alice", defaultPushPartitions)), data)
	}

	awaitCond(t, testPatience, "alice watched after the overflow", func() bool { return w.watched() == 2 })
}

// TestPushVerifyBySmallestPartition: the verifications are sent by the
// holder of the smallest held partition; when partition 0 is held by
// nobody, by the holder of the next one.
func TestPushVerifyBySmallestPartition(t *testing.T) {
	c := newPushCluster(t, model.KVIndexRead)

	w1 := c.start("w1", func(o *WorkerOptions, _ *pushTestHooks) { o.MaxPartitions = 32 })
	w2 := c.start("w2", func(o *WorkerOptions, _ *pushTestHooks) { o.MaxPartitions = 32 })

	awaitCond(t, testPatience, "convergence", func() bool { return w1.held() == 32 && w2.held() == 32 })

	members := c.members()

	owner := "w2"
	if members["w1"].holds(0) {
		owner = "w1"
	}

	// A new subscription of an account of the other worker's partition:
	// its verification is sent by the owner of partition 0.
	name := "acct"
	for i := 0; ; i++ {
		name = fmt.Sprintf("acct%d", i)
		p := PushPartition(name, defaultPushPartitions)

		if other := members[otherOf(owner)]; other.holds(p) {
			break
		}
	}

	if err := updatePushAccount(c.kv, name, func(pa *pushAccount) error {
		pa.Subs = append(pa.Subs, &pushRecord{ID: "v1", Account: name, URL: c.ps.URL + path(name), Code: "c",
			Expires: time.Now().Add(time.Hour), VAPIDKey: c.vapid})

		return nil
	}); err != nil {
		t.Fatal(err)
	}

	if err := queueVerify(c.kv, name, "v1"); err != nil {
		t.Fatal(err)
	}

	awaitCond(t, testPatience, "the verification", func() bool { return c.sink.count(path(name)) == 1 })

	// The owner of partition 0 ends; its successor sends the next one.
	c.stop(c.workerNamed(owner))

	rest := c.workerNamed(otherOf(owner))

	awaitCond(t, testPatience, "the rest holding 32 only", func() bool { return rest.held() == 32 })

	if err := queueVerify(c.kv, name, "v1"); err != nil {
		t.Fatal(err)
	}

	awaitCond(t, testPatience, "the verification by the successor", func() bool { return c.sink.count(path(name)) == 2 })
}

// otherOf is the other of w1 and w2.
func otherOf(name string) string {
	if name == "w1" {
		return "w2"
	}

	return "w1"
}

// workerNamed is the running worker with the name.
func (c *pushCluster) workerNamed(name string) *pushWorkerRun {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.workers[name]
}

// TestPushLegacyMarker: new workers wait while an old push worker holds
// the lease record, and take the partitions once it releases; a new
// worker that holds partitions keeps the record occupied as a marker,
// which an old worker respects; the last new worker deletes it when it
// ends.
func TestPushLegacyMarker(t *testing.T) {
	c := newPushCluster(t, model.KVIndexRead)
	c.account("alice")

	old, _ := json.Marshal(pushLease{Holder: "old-pod", Until: time.Now().Add(time.Minute)})
	c.writeRecord(pushLeaseKey, old)

	w := c.start("w1")

	time.Sleep(5 * testTake)

	if w.held() != 0 {
		t.Fatalf("the new worker took %d partitions under an old holder", w.held())
	}

	// The old worker releases.
	c.writeRecord(pushLeaseKey, nil)

	awaitCond(t, testPatience, "w1 holding everything", func() bool { return w.held() == defaultPushPartitions })

	if l := c.marker(); l.Holder != pushLegacyPrefix+"w1" || !l.Until.After(time.Now().Add(pushLegacyRefresh-time.Second)) {
		t.Fatalf("marker %+v", l)
	}

	// An old worker (its lease logic) finds the record held.
	err := c.kv.Update(pushLeaseKey, func(cur []byte) ([]byte, error) {
		var l pushLease
		if cur != nil && json.Unmarshal(cur, &l) == nil && l.Holder != "old-pod" && l.Until.After(time.Now()) {
			return nil, errOldHolder
		}

		return old, nil
	})
	if err == nil {
		t.Fatal("an old worker took the lease beside the marker")
	}

	// The last new worker deletes the marker at its end.
	c.stop(w)

	if _, err := c.kv.Get(pushLeaseKey); err == nil {
		t.Fatal("the marker outlived the last new worker")
	}
}

// writeRecord writes (nil: deletes) an admin record.
func (c *pushCluster) writeRecord(key string, value []byte) {
	c.t.Helper()

	if err := c.kv.Update(key, func([]byte) ([]byte, error) { return value, nil }); err != nil {
		c.t.Fatal(err)
	}
}

// marker reads the legacy lease record.
func (c *pushCluster) marker() pushLease {
	c.t.Helper()

	raw, err := c.kv.Get(pushLeaseKey)
	if err != nil {
		c.t.Fatal(err)
	}

	var l pushLease
	if err := json.Unmarshal(raw, &l); err != nil {
		c.t.Fatal(err)
	}

	return l
}

// TestPushWithoutReadMode: without an effective read mode the
// partitions are still shared by leases, every holder lists and
// filters, and every account is watched once; when the gate opens at
// run time the scans read the shard instead, without a restart.
func TestPushWithoutReadMode(t *testing.T) {
	c := newPushCluster(t, model.KVIndexWrite)

	accounts := map[string]model.User{}

	for i := range 8 {
		name := fmt.Sprintf("acct%d", i)
		accounts[name] = c.account(name)
	}

	var clock atomic.Pointer[time.Time]

	now := time.Now()
	clock.Store(&now)

	gated := c.kv.WithGatedMode(model.KVIndexRead, func() time.Time { return *clock.Load() })

	c.customize = func(o *WorkerOptions) { o.KV = gated }

	c.start("w1")
	c.start("w2")

	awaitCond(t, testPatience, "convergence at 32/32", c.converged)
	c.watchedAll(len(accounts))

	if c.kv.Listings() == 0 {
		t.Fatal("no listing without read mode")
	}

	for name, u := range accounts {
		c.pushedAfter(u, name, 5*time.Second)
	}

	// The gate opens: the marker appears, the next scan reads the shard.
	if err := gated.MarkerIndex().SetMarker(kvindex.MarkerKV, `{"finished":"2026-10-03T00:00:00Z"}`); err != nil {
		t.Fatal(err)
	}

	later := now.Add(2 * sortedindex.BackfillRecheck)
	clock.Store(&later)

	awaitCond(t, testPatience, "the gate to open", func() bool { return gated.IndexMode() == model.KVIndexRead })

	time.Sleep(3 * testScan)

	n := c.kv.Listings()

	time.Sleep(5 * testScan)

	if c.kv.Listings() != n {
		t.Fatalf("still listing in read mode: %d more", c.kv.Listings()-n)
	}

	for name, u := range accounts {
		c.pushedAfter(u, name, 5*time.Second)
	}
}

// countingKV counts the Gets of an admin store by key.
type countingKV struct {
	*memstore.AdminKV
	mu   sync.Mutex
	gets map[string]int
}

func (k *countingKV) Get(key string) ([]byte, error) {
	k.mu.Lock()
	if k.gets == nil {
		k.gets = map[string]int{}
	}

	k.gets[key]++
	k.mu.Unlock()

	return k.AdminKV.Get(key)
}

func (k *countingKV) of(key string) int {
	k.mu.Lock()
	defer k.mu.Unlock()

	return k.gets[key]
}
