//go:build tentacle

package cephstore

import (
	"errors"
	"log/slog"
	"maps"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeFence is a fence in memory with the compare-and-swap semantics of
// radosFence.
type fakeFence struct {
	mu   sync.Mutex
	vals map[string]string
	ver  uint64
	// writeErr fails the next write; conflicts lets the next writes lose
	// (another write landed first, the version moves on); onWrite runs
	// during every write (a slow reply).
	writeErr  error
	conflicts int
	onWrite   func()
}

func (f *fakeFence) read() (Fence, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.vals == nil {
		return Fence{}, nil
	}

	return fenceOf(toBytes(f.vals), f.ver), nil
}

func (f *fakeFence) write(ver uint64, vals map[string]string) (bool, error) {
	if f.onWrite != nil {
		f.onWrite()
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	if err := f.writeErr; err != nil {
		f.writeErr = nil
		return false, err
	}

	if f.conflicts > 0 {
		f.conflicts--
		f.ver++

		return false, nil
	}

	switch {
	case ver == 0 && f.vals != nil, ver != 0 && ver != f.ver:
		return false, nil
	case vals == nil:
		f.vals, f.ver = nil, 0
	default:
		f.vals, f.ver = maps.Clone(vals), f.ver+1
	}

	return true, nil
}

// fakeClock is a clock a test moves.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.t
}

func (c *fakeClock) add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.t = c.t.Add(d)
}

// fakeLease is a lease of instance on f with the clock c; the
// background renewal is effectively off (the test renews by hand).
func fakeLease(t *testing.T, f *fakeFence, c *fakeClock, instance string, wait bool) *fenceLease {
	t.Helper()

	l := &fenceLease{io: f, instance: instance, log: slog.New(slog.DiscardHandler), wait: wait,
		now: c.now, sleep: func(d time.Duration) { c.add(d) }, every: time.Hour}

	t.Cleanup(l.close)

	return l
}

// keep is the plan that leases a fence as it is.
func keep(f Fence) (map[string]string, error) { return leasedValues(f, "test", time.Now()), nil }

func TestLeaseWindow(t *testing.T) {
	f, c := &fakeFence{}, &fakeClock{t: time.Unix(1_000_000, 0)}
	l := fakeLease(t, f, c, "me", false)

	if _, err := l.acquire(keep); err != nil || !l.valid() {
		t.Fatalf("acquire: %v", err)
	}

	c.add(toolLeaseValid - time.Second)

	if err := l.check(); err != nil {
		t.Fatalf("within the window: %v", err)
	}

	c.add(2 * time.Second)

	if err := l.check(); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("after the window: %v", err)
	}

	if err := l.update(nil, false); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("a write after the window: %v", err)
	}
}

// TestLeaseFailedRenewal: a failed renewal does not extend the window,
// a successful one does.
func TestLeaseFailedRenewal(t *testing.T) {
	f, c := &fakeFence{}, &fakeClock{t: time.Unix(1_000_000, 0)}
	l := fakeLease(t, f, c, "me", false)

	if _, err := l.acquire(keep); err != nil {
		t.Fatal(err)
	}

	c.add(30 * time.Second)

	if err := l.update(nil, false); err != nil {
		t.Fatal(err)
	}

	c.add(30 * time.Second)

	f.writeErr = errors.New("timed out")

	if err := l.update(nil, false); err == nil {
		t.Fatal("a failed write was taken for a renewal")
	}

	// 90 s after the last successful renewal, 60 s after the failed one.
	c.add(toolLeaseValid - 30*time.Second)

	if err := l.check(); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("the failed renewal extended the window: %v", err)
	}
}

// TestLeaseSendTimeAnchor: the window starts when the write was sent,
// not when its answer came.
func TestLeaseSendTimeAnchor(t *testing.T) {
	f, c := &fakeFence{}, &fakeClock{t: time.Unix(1_000_000, 0)}
	l := fakeLease(t, f, c, "me", false)

	sent := c.now()
	f.onWrite = func() { c.add(50 * time.Second) } // a slow answer

	if _, err := l.acquire(keep); err != nil {
		t.Fatal(err)
	}

	f.onWrite = nil

	c.t = sent.Add(toolLeaseValid - time.Second)

	if !l.valid() {
		t.Fatal("invalid within the window from the send")
	}

	c.t = sent.Add(toolLeaseValid)

	if l.valid() {
		t.Fatal("valid after the window from the send (the answer came 50 s later)")
	}
}

// heldBy is a fence held by another instance, last written seenAhead
// after the clock c.
func heldBy(c *fakeClock, holder string, seenAhead time.Duration) *fakeFence {
	return &fakeFence{ver: 7, vals: map[string]string{pkState: FenceNative, pkRun: "r1", keyToolInstance: holder,
		keyToolEpoch: "4", keyToolSeen: c.now().Add(seenAhead).UTC().Format(time.RFC3339Nano)}}
}

// TestLeaseHeld: without wait a held lease refuses at once naming the
// holder; with wait it is taken over once the holder was idle, and a
// tool_seen 31 s ahead of the clock is refused while 29 s is not.
func TestLeaseHeld(t *testing.T) {
	c := &fakeClock{t: time.Unix(1_000_000, 0)}

	l := fakeLease(t, heldBy(c, "other-1", 0), c, "me", false)
	if _, err := l.acquire(keep); !errors.Is(err, ErrFenceHeld) || !strings.Contains(err.Error(), "other-1") {
		t.Fatalf("held lease without wait: %v", err)
	}

	f := heldBy(c, "other-1", 29*time.Second)
	l = fakeLease(t, f, c, "me", true)

	nf, err := l.acquire(keep)
	if err != nil || nf.Values[keyToolInstance] != "me" || nf.State != FenceNative || nf.Values[keyToolEpoch] != "5" {
		t.Fatalf("takeover after the holder was idle: %+v %v", nf, err)
	}

	l = fakeLease(t, heldBy(c, "other-1", 31*time.Second), c, "me", true)
	if _, err := l.acquire(keep); !errors.Is(err, ErrFenceHeld) || !strings.Contains(err.Error(), "future") {
		t.Fatalf("a tool_seen ahead of the clock: %v", err)
	}
}

// TestLeaseHolderAlive: a holder that writes while the taker waits
// refuses the taker.
func TestLeaseHolderAlive(t *testing.T) {
	c := &fakeClock{t: time.Unix(1_000_000, 0)}
	f := heldBy(c, "other-1", 0)
	l := fakeLease(t, f, c, "me", true)

	l.sleep = func(d time.Duration) {
		c.add(d)

		f.mu.Lock()
		f.vals[keyToolEpoch] = strconv.Itoa(len(f.vals[keyToolEpoch]) + 10) // the holder renews
		f.mu.Unlock()
	}

	if _, err := l.acquire(keep); !errors.Is(err, ErrFenceHeld) || !strings.Contains(err.Error(), "working on it") {
		t.Fatalf("a live holder: %v", err)
	}
}

// TestLeaseKeepsState: leasing an existing fence changes only the lease
// fields, and giving it up restores it; a fence the lease created is
// removed again.
func TestLeaseKeepsState(t *testing.T) {
	c := &fakeClock{t: time.Unix(1_000_000, 0)}

	for _, state := range []string{FenceReverted, FenceNative, FenceDone} {
		leaseExisting(t, c, state)
	}

	f := &fakeFence{}
	l := fakeLease(t, f, c, "me", false)

	if nf, err := l.acquire(keep); err != nil || nf.State != FenceLeased {
		t.Fatalf("absent fence: %+v %v", nf, err)
	}

	if err := l.release(); err != nil {
		t.Fatal(err)
	}

	if got, _ := f.read(); got.Exists {
		t.Fatalf("the created fence was left: %+v", got)
	}
}

// leaseExisting leases and releases a fence in state.
func leaseExisting(t *testing.T, c *fakeClock, state string) {
	t.Helper()

	f := &fakeFence{ver: 3, vals: map[string]string{pkState: state, pkRun: "r1", pkToID: "9"}}
	l := fakeLease(t, f, c, "me", false)

	nf, err := l.acquire(keep)
	if err != nil || nf.State != state || nf.Run != "r1" || nf.ToID != 9 {
		t.Fatalf("%s: leased %+v %v", state, nf, err)
	}

	if err := l.release(); err != nil {
		t.Fatal(err)
	}

	got, _ := f.read()
	if got.State != state || got.Values[keyToolInstance] != "" || got.Values[keyToolSeen] != "" {
		t.Fatalf("%s: after the release %+v", state, got)
	}
}

// TestLeaseConcurrentWrites: renewals and a state change at the same
// time all land (one writer per process), also after lost
// compare-and-swaps; each counts the epoch up.
func TestLeaseConcurrentWrites(t *testing.T) {
	f, c := &fakeFence{}, &fakeClock{t: time.Unix(1_000_000, 0)}
	l := fakeLease(t, f, c, "me", false)

	if _, err := l.acquire(keep); err != nil {
		t.Fatal(err)
	}

	f.mu.Lock()
	f.conflicts = 3
	f.mu.Unlock()

	var wg sync.WaitGroup

	errs := make(chan error, 21)

	for range 20 {
		wg.Go(func() { errs <- l.update(nil, false) })
	}

	wg.Go(func() {
		errs <- l.update(func(v map[string]string) error { v[pkState] = FenceInitializing; return nil }, false)
	})

	wg.Wait()
	close(errs)

	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}

	got, _ := f.read()
	if got.State != FenceInitializing || got.Values[keyToolEpoch] != "22" || got.Values[keyToolInstance] != "me" {
		t.Fatalf("fence %+v", got)
	}
}
