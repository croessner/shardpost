package auth

import (
	"context"
	"errors"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"shardpost/internal/memstore"
	"shardpost/internal/model"
)

// pod is one server: its cache and its flush log.
type pod struct {
	cache *Cache
	log   *FlushLog
}

func startPod(t *testing.T, omap model.AdminOmap, poll time.Duration) *pod {
	t.Helper()

	c := testCache(&fakeBackend{}, nil)
	l := NewFlushLog(omap, c, poll, nil)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})

	go func() { defer close(done); l.Run(ctx) }()

	t.Cleanup(func() { cancel(); <-done })

	waitFor(t, "flush log started", func() bool { l.mu.Lock(); defer l.mu.Unlock(); return l.started })

	return &pod{cache: c, log: l}
}

// fill caches a login and a lookup of alice and a login of bob.
func (p *pod) fill(t *testing.T) {
	t.Helper()

	ctx := context.Background()
	_, _ = p.cache.Authenticate(ctx, login())
	_, _ = p.cache.Lookup(ctx, "alice@example.org", "lmtp", nil)

	p.cache.next.(*fakeBackend).auth = func(Request) (*Result, error) { return &Result{Account: "bob@example.org"}, nil }
	_, _ = p.cache.Authenticate(ctx, Request{Username: "bob", Password: "x", Protocol: "imap"})
	p.cache.next.(*fakeBackend).auth = nil

	if p.cache.Len() != 3 {
		t.Fatalf("filled %d entries", p.cache.Len())
	}
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()

	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}

		time.Sleep(5 * time.Millisecond)
	}
}

func TestFlushLogAcrossPods(t *testing.T) {
	omap := memstore.NewAdminKV().Omap()
	a, b := startPod(t, omap, time.Hour), startPod(t, omap, time.Hour)

	a.fill(t)
	b.fill(t)

	if err := a.log.Flush("Alice@Example.org"); err != nil {
		t.Fatal(err)
	}

	// The flushing pod is flushed before the answer.
	if a.cache.Len() != 1 {
		t.Fatalf("pod a kept %d entries", a.cache.Len())
	}

	waitFor(t, "pod b to flush alice", func() bool { return b.cache.Len() == 1 })

	if err := b.log.Flush(""); err != nil {
		t.Fatal(err)
	}

	waitFor(t, "pod a to flush everything", func() bool { return a.cache.Len() == 0 })

	// Both have applied every generation.
	vals, _, _ := omap.ReadKeys(FlushObject, []string{flushHeadKey})
	head, _ := parseGen(vals[flushHeadKey])

	waitFor(t, "generations applied", func() bool {
		a.log.mu.Lock()
		b.log.mu.Lock()
		defer a.log.mu.Unlock()
		defer b.log.mu.Unlock()

		return head == 2 && a.log.lastGen == 2 && b.log.lastGen == 2
	})
}

// silentOmap drops the notifications, as a lost notify would.
type silentOmap struct{ model.AdminOmap }

func (silentOmap) Notify(string) {}

func TestFlushLogPollFallback(t *testing.T) {
	omap := silentOmap{memstore.NewAdminKV().Omap()}
	a, b := startPod(t, omap, time.Hour), startPod(t, omap, 30*time.Millisecond)

	b.fill(t)

	if err := a.log.Flush("bob@example.org"); err != nil {
		t.Fatal(err)
	}

	waitFor(t, "the poll to flush bob", func() bool { return b.cache.Len() == 2 })
}

func TestFlushLogGap(t *testing.T) {
	omap := memstore.NewAdminKV().Omap()
	b := startPod(t, omap, time.Hour)
	b.fill(t)

	// Generations 1 to 4 are gone (pruned while b was not looking): only
	// generation 5 names an account, but b must flush everything.
	set := map[string][]byte{flushHeadKey: []byte("5"), flushEntryKey(5): []byte(strconv.FormatInt(time.Now().Unix(), 10) + "\tnobody")}
	if err := omap.Write(FlushObject, model.OmapAnyVersion, set, nil); err != nil {
		t.Fatal(err)
	}

	omap.Notify(FlushObject)

	waitFor(t, "a full flush after a gap", func() bool { return b.cache.Len() == 0 })
}

// lossyOmap hands out watches whose error channel the test controls.
type lossyOmap struct {
	model.AdminOmap

	mu      sync.Mutex
	errs    []chan error
	watches int
}

type lossyWatch struct {
	model.Watch
	errs chan error
}

func (w *lossyWatch) Errors() <-chan error { return w.errs }

func (o *lossyOmap) Watch(oid string) (model.Watch, error) {
	w, err := o.AdminOmap.Watch(oid)
	if err != nil {
		return nil, err
	}

	errs := make(chan error, 1)

	o.mu.Lock()
	o.errs = append(o.errs, errs)
	o.watches++
	o.mu.Unlock()

	return &lossyWatch{Watch: w, errs: errs}, nil
}

func (o *lossyOmap) lose() {
	o.mu.Lock()
	defer o.mu.Unlock()

	o.errs[len(o.errs)-1] <- errors.New("watch lost")
}

func (o *lossyOmap) count() int {
	o.mu.Lock()
	defer o.mu.Unlock()

	return o.watches
}

func TestFlushLogWatchLoss(t *testing.T) {
	omap := &lossyOmap{AdminOmap: memstore.NewAdminKV().Omap()}
	b := startPod(t, omap, time.Hour)
	b.fill(t)

	omap.lose()

	waitFor(t, "a full flush after a lost watch", func() bool { return b.cache.Len() == 0 })
	waitFor(t, "the watch to be set up again", func() bool { return omap.count() == 2 })

	// It follows the log again.
	b.fill(t)

	a := NewFlushLog(omap, testCache(&fakeBackend{}, nil), time.Hour, nil)
	if err := a.Flush("bob"); err != nil {
		t.Fatal(err)
	}

	waitFor(t, "a flush after the new watch", func() bool { return b.cache.Len() == 2 })
}

// flakyOmap fails the first reads of the generation.
type flakyOmap struct {
	model.AdminOmap
	failures atomic.Int64
}

func (o *flakyOmap) ReadKeys(oid string, keys []string) (map[string][]byte, uint64, error) {
	if o.failures.Add(-1) >= 0 {
		return nil, 0, errors.New("rados: timed out")
	}

	return o.AdminOmap.ReadKeys(oid, keys)
}

// TestFlushLogStartRetried: a failed read of the generation when the
// watch is set up is retried on the poll; the server is not deaf to the
// flushes after it.
func TestFlushLogStartRetried(t *testing.T) {
	omap := &flakyOmap{AdminOmap: memstore.NewAdminKV().Omap()}
	omap.failures.Store(1)

	c := testCache(&fakeBackend{}, nil)
	l := NewFlushLog(omap, c, 20*time.Millisecond, nil)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})

	go func() { defer close(done); l.Run(ctx) }()

	t.Cleanup(func() { cancel(); <-done })

	waitFor(t, "the start to be retried", func() bool { l.mu.Lock(); defer l.mu.Unlock(); return l.started })

	b := &pod{cache: c, log: l}
	b.fill(t)

	a := NewFlushLog(omap, testCache(&fakeBackend{}, nil), time.Hour, nil)
	if err := a.Flush("bob@example.org"); err != nil {
		t.Fatal(err)
	}

	waitFor(t, "the flush to reach the retried log", func() bool { return b.cache.Len() == 2 })
}

func TestFlushLogPrunes(t *testing.T) {
	omap := memstore.NewAdminKV().Omap()
	l := NewFlushLog(omap, testCache(&fakeBackend{}, nil), time.Hour, nil)
	now := time.Unix(1_800_000_000, 0)
	l.now = func() time.Time { return now }

	for range 3 {
		if err := l.Flush("x"); err != nil {
			t.Fatal(err)
		}
	}

	now = now.Add(FlushRetention + time.Minute)

	if err := l.Flush("y"); err != nil {
		t.Fatal(err)
	}

	page, _, err := omap.ReadPage(FlushObject, flushEntryPrefix, 100)
	if err != nil {
		t.Fatal(err)
	}

	var keys []string

	for _, kv := range page {
		if kv.Key < flushEntryEnd {
			keys = append(keys, kv.Key)
		}
	}

	if len(keys) != 1 || keys[0] != flushEntryKey(4) {
		t.Fatalf("entries after pruning: %v", keys)
	}
}
