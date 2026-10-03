package jmap

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"shardpost/internal/model"
)

// The tests of the state per account (stage B18a, §4.8 and §8.1): the
// acknowledgements across a hand-over, the epoch, the round budget.
// A hand-over here is one worker ending in order and another starting.

// handOver ends the worker and starts another under a new name.
func (c *pushCluster) handOver(r *pushWorkerRun, name string) *pushWorkerRun {
	c.t.Helper()

	c.stop(r)

	next := c.start(name)
	awaitCond(c.t, testPatience, name+" holding everything", func() bool { return next.held() == defaultPushPartitions })

	return next
}

// TestPushAckAcrossHandover: the three cases of F3. A subscription in
// backoff (503) gets the change from the successor; a send cut off by
// the end of the worker is either acknowledged or left as a backlog
// the successor sends, exactly one of the two; a delivery after a
// backoff without a new change is not repeated by the successor.
func TestPushAckAcrossHandover(t *testing.T) {
	c := newPushCluster(t, model.KVIndexRead)
	u := c.account("alice")

	w := c.start("w1")
	awaitCond(t, testPatience, "w1 holding everything", func() bool { return w.held() == defaultPushPartitions })
	c.watchedAll(1)

	// Case 1: the push service asks to wait; the worker ends; the
	// successor sends the change after the wait.
	c.sink.status.Store(http.StatusServiceUnavailable)

	before := c.sink.stateChanges(path("alice"))
	deliver(t, u, inboxName, plainMessage)

	awaitCond(t, 5*time.Second, "the refused push", func() bool { return c.sink.stateChanges(path("alice")) > before })

	seen := c.seen("alice")
	if len(seen.Ack) != 1 || !seen.behind(firstKey(seen.Ack)) {
		t.Fatalf("no backlog saved after the refusal: %+v", seen)
	}

	w = c.handOver(w, "w2")
	c.watchedAll(1)
	c.sink.status.Store(http.StatusCreated)

	refused := c.sink.stateChanges(path("alice"))

	awaitCond(t, 5*time.Second, "the push by the successor", func() bool {
		return c.sink.stateChanges(path("alice")) > refused && strings.Contains(lastBody(c, "alice"), typeEmailDelivery)
	})

	awaitCond(t, 5*time.Second, "the acknowledgement", func() bool { s := c.seen("alice"); return !s.behind(firstKey(s.Ack)) })

	// Case 3: a delivery after a backoff without a new change is not
	// repeated by the successor.
	n := c.sink.stateChanges(path("alice"))
	w = c.handOver(w, "w3")
	c.watchedAll(1)
	time.Sleep(5 * testScan)

	if c.sink.stateChanges(path("alice")) != n {
		t.Fatalf("the successor repeated a delivered push: %d pushes, had %d", c.sink.stateChanges(path("alice")), n)
	}

	// Case 2: a send hangs beyond the round budget while the worker
	// ends; the send is cut off, the backlog stays, the successor sends
	// once.
	hold := make(chan struct{})
	c.sink.hold.Store(&hold)

	deliver(t, u, inboxName, "Subject: hang\r\n\r\nx\r\n")
	time.Sleep(testScan) // the round is in the hanging send

	stopped := time.Now()
	c.stop(w)

	if took := time.Since(stopped); took > 3*testHandoff+2*testTake+time.Second {
		t.Errorf("the end waited %s on the hanging send", took)
	}

	// The round ended within its budget (inside the hand-off) and saved
	// the backlog before the worker ended.
	if s := c.seen("alice"); !s.behind(firstKey(s.Ack)) {
		t.Fatalf("the cut-off send was acknowledged: %+v", s)
	}

	c.sink.release()

	awaitCond(t, 5*time.Second, "the cut-off body to land", func() bool { return c.sink.stateChanges(path("alice")) == n+1 })

	w4 := c.start("w4")
	awaitCond(t, testPatience, "w4 holding everything", func() bool { return w4.held() == defaultPushPartitions })

	awaitCond(t, 5*time.Second, "the push by the successor", func() bool { return c.sink.stateChanges(path("alice")) == n+2 })

	time.Sleep(5 * testScan)

	if c.sink.stateChanges(path("alice")) != n+2 {
		t.Fatalf("%d pushes after the cut-off send, want %d", c.sink.stateChanges(path("alice")), n+2)
	}
}

// firstKey is the one key of a map of one.
func firstKey(m map[string]pushAck) string {
	for k := range m {
		return k
	}

	return ""
}

// lastBody is the last push an account got.
func lastBody(c *pushCluster, account string) string {
	c.sink.mu.Lock()
	defer c.sink.mu.Unlock()

	b := c.sink.bodies[path(account)]
	if len(b) == 0 {
		return ""
	}

	return b[len(b)-1]
}

// TestPushNewSubscriptionAndWants: a subscription verified between two
// hand-overs gets no "every type" push from the successor, only the
// next change; a subscription whose backlog is only types it does not
// want is acknowledged without a push, and not pushed to after a
// hand-over either.
func TestPushNewSubscriptionAndWants(t *testing.T) {
	c := newPushCluster(t, model.KVIndexRead)
	u := c.account("alice")

	w := c.start("w1")
	awaitCond(t, testPatience, "w1 holding everything", func() bool { return w.held() == defaultPushPartitions })
	c.watchedAll(1)
	c.pushedAfter(u, "alice", 5*time.Second)

	// A second subscription, verified now, with a path of its own.
	id := c.subscribe("alice")
	n := c.sink.stateChanges(path("alice"))

	awaitCond(t, testPatience, "the new subscription acknowledged", func() bool {
		s := c.seen("alice")

		return len(s.Ack) == 2 && !s.behind(id)
	})

	w = c.handOver(w, "w2")
	c.watchedAll(1)
	time.Sleep(5 * testScan)

	if c.sink.stateChanges(path("alice")) != n {
		t.Fatalf("the successor pushed %d times to a subscription that owes nothing", c.sink.stateChanges(path("alice"))-n)
	}

	// Both subscriptions get the next change.
	deliver(t, u, inboxName, "Subject: next\r\n\r\nx\r\n")
	awaitCond(t, 5*time.Second, "both pushes", func() bool { return c.sink.stateChanges(path("alice")) == n+2 })

	// A subscription that wants a type no change of the account produces
	// (a delivery changes the message, its thread and the counts of its
	// mailbox): its acknowledgement moves on without a push, and the
	// successor owes it nothing.
	bob := c.account("bob", "Identity")
	c.watchedAll(2)

	deliver(t, bob, inboxName, plainMessage)

	awaitCond(t, 5*time.Second, "the unwanted change acknowledged", func() bool {
		s := c.seen("bob")

		return len(s.Ack) == 1 && s.Seq >= 2 && !s.behind(firstKey(s.Ack))
	})

	c.handOver(w, "w3")
	c.watchedAll(2)
	time.Sleep(5 * testScan)

	if c.sink.stateChanges(path("bob")) != 0 {
		t.Fatalf("bob got %d pushes for changes it does not want", c.sink.stateChanges(path("bob")))
	}
}

// TestPushStateFarBehind (F4): a state far behind the account gives one
// push naming every type instead of a silent jump to now.
func TestPushStateFarBehind(t *testing.T) {
	c := newPushCluster(t, model.KVIndexRead)
	u := c.account("alice")

	for range 1200 {
		deliver(t, u, inboxName, plainMessage)
	}

	behind, _ := json.Marshal(pushSeen{Seq: 1})
	if err := c.kv.Update(pushSeenKey("alice"), func([]byte) ([]byte, error) { return behind, nil }); err != nil {
		t.Fatal(err)
	}

	w := c.start("w1")
	awaitCond(t, testPatience, "w1 holding everything", func() bool { return w.held() == defaultPushPartitions })

	awaitCond(t, 5*time.Second, "the push", func() bool { return c.sink.stateChanges(path("alice")) == 1 })

	body := lastBody(c, "alice")
	for _, typ := range pushTypes {
		if !strings.Contains(body, typ) {
			t.Fatalf("the push after a long gap lacks %s: %s", typ, body)
		}
	}

	time.Sleep(5 * testScan)

	if c.sink.stateChanges(path("alice")) != 1 {
		t.Fatalf("%d pushes for one gap", c.sink.stateChanges(path("alice")))
	}
}

// TestPushEpoch: an account purged and created again under a running
// watcher gets a new epoch; the state and the acknowledgements start
// at the new counter, deliveries in the new account are pushed, and
// hand-overs without changes push nothing.
func TestPushEpoch(t *testing.T) {
	c := newPushCluster(t, model.KVIndexRead)
	u := c.account("alice")

	w := c.start("w1")
	awaitCond(t, testPatience, "w1 holding everything", func() bool { return w.held() == defaultPushPartitions })
	c.watchedAll(1)

	for range 5 {
		c.pushedAfter(u, "alice", 5*time.Second)
	}

	if s := c.seen("alice"); s.Epoch != 0 || s.Seq < 5 {
		t.Fatalf("state before the purge: %+v", s)
	}

	// The purge and the new account: the counter starts over.
	if _, err := u.(model.NamespacePurger).PurgeNamespace(func() error { return nil }); err != nil {
		t.Fatal(err)
	}

	if err := u.CreateMailbox(inboxName, nil); err != nil {
		t.Fatal(err)
	}

	n := c.sink.stateChanges(path("alice"))

	awaitCond(t, testPatience, "the new epoch", func() bool { s := c.seen("alice"); return s.Epoch == 1 })

	now, _ := u.AccountState()
	if s := c.seen("alice"); s.Seq > now || s.Ack[firstKey(s.Ack)].Seq > now {
		t.Fatalf("state ahead of the new account: %+v, counter %d", s, now)
	}

	c.watchedAll(1)
	c.pushedAfter(u, "alice", 5*time.Second)

	if c.sink.stateChanges(path("alice")) < n+1 {
		t.Fatal("no push in the new account")
	}

	m := c.sink.stateChanges(path("alice"))

	for i := range 3 {
		w = c.handOver(w, fmt.Sprintf("w%d", i+2))
		c.watchedAll(1)
	}

	time.Sleep(5 * testScan)

	if c.sink.stateChanges(path("alice")) != m {
		t.Fatalf("%d pushes over hand-overs without a change", c.sink.stateChanges(path("alice"))-m)
	}

	if s := c.seen("alice"); s.Epoch != 1 {
		t.Fatalf("epoch after the hand-overs: %+v", s)
	}
}

// TestPushRoundBudget: a round of pushes to many subscriptions ends
// within its budget when the worker ends; what was not sent is a
// backlog the successor sends, and the end of the partition takes no
// longer than the hand-off.
func TestPushRoundBudget(t *testing.T) {
	c := newPushCluster(t, model.KVIndexRead)
	u := c.account("alice")

	for range 4 {
		c.subscribe("alice")
	}

	c.sink.delay.Store(int64(150 * time.Millisecond))

	// A hand-off of a second with a round budget of half of it: five
	// sends of 150 ms do not fit. The scan interval sets the backoff
	// after a cut-off send, long enough that the watcher does not send
	// again before its partition is given up (the waves take three
	// hand-offs).
	c.customize = func(o *WorkerOptions) {
		o.LeaseTTL, o.RebalanceSettle, o.Handoff, o.RoundBudget = 2*time.Second, 2*time.Second, time.Second, 500*time.Millisecond
		o.Scan = 10 * time.Second
	}

	w := c.start("w1")
	awaitCond(t, testPatience, "w1 holding everything", func() bool { return w.held() == defaultPushPartitions })
	c.watchedAll(1)

	deliver(t, u, inboxName, plainMessage)
	time.Sleep(100 * time.Millisecond) // the round is sending

	start := time.Now()
	c.stop(w)

	if took := time.Since(start); took > 3*time.Second+2*testTake+time.Second {
		t.Errorf("the end took %s", took)
	}

	s := c.seen("alice")
	owed := 0

	for id := range s.Ack {
		if s.behind(id) {
			owed++
		}
	}

	if len(s.Ack) != 5 || owed == 0 || owed == 5 {
		t.Fatalf("after the cut-off round: %d of %d subscriptions owed, want some of them (%+v)", owed, len(s.Ack), s)
	}

	c.sink.delay.Store(0)

	sent := c.sink.stateChanges(path("alice"))

	w2 := c.start("w2")
	awaitCond(t, testPatience, "w2 holding everything", func() bool { return w2.held() == defaultPushPartitions })

	awaitCond(t, 5*time.Second, "the successor's pushes", func() bool { return c.sink.stateChanges(path("alice")) == sent+owed })

	time.Sleep(5 * testScan)

	if c.sink.stateChanges(path("alice")) != sent+owed {
		t.Fatalf("%d pushes, want %d", c.sink.stateChanges(path("alice")), sent+owed)
	}
}
