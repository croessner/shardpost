package jmap

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"shardpost/internal/auth"
)

func TestLimiterFIFO(t *testing.T) {
	l := newLimiter("test", 1)

	hold, ok := l.tryAcquire("a")
	if !ok {
		t.Fatal("first slot refused")
	}

	order := make(chan int, 2)

	var wg sync.WaitGroup

	for i := range 2 {
		wg.Go(func() {
			release, ok := l.acquire(context.Background(), "a", 5*time.Second)
			if !ok {
				t.Errorf("waiter %d got no slot", i)
				return
			}

			order <- i

			time.Sleep(10 * time.Millisecond)
			release()
		})

		// The second waiter queues after the first.
		waitUntil(t, func() bool { return waiters(l, "a") == i+1 })
	}

	hold()
	wg.Wait()
	close(order)

	if first, second := <-order, <-order; first != 0 || second != 1 {
		t.Fatalf("slots went to %d then %d, not first come first served", first, second)
	}

	if l.inFlight("a") != 0 || len(l.keys) != 0 {
		t.Fatalf("state left behind: %d in flight, %d keys", l.inFlight("a"), len(l.keys))
	}
}

func waiters(l *limiter, key string) int {
	l.mu.Lock()
	defer l.mu.Unlock()

	if s := l.keys[key]; s != nil {
		return s.waiters.Len()
	}

	return 0
}

func waitUntil(t *testing.T, cond func() bool) {
	t.Helper()

	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("timed out")
		}

		time.Sleep(time.Millisecond)
	}
}

func TestLimiterCapAndCancel(t *testing.T) {
	l := newLimiter("test", 1) // at most two waiters

	hold, _ := l.tryAcquire("a")

	ctx, cancel := context.WithCancel(context.Background())
	results := make(chan bool, 2)

	for range 2 {
		go func() { _, ok := l.acquire(ctx, "a", 5*time.Second); results <- ok }()
	}

	waitUntil(t, func() bool { return waiters(l, "a") == 2 })

	start := time.Now()
	if _, ok := l.acquire(context.Background(), "a", 5*time.Second); ok || time.Since(start) > time.Second {
		t.Fatal("a third waiter was queued beyond twice the limit")
	}

	// Cancelled waiters leave the queue without a slot.
	cancel()

	for range 2 {
		if <-results {
			t.Fatal("a cancelled waiter got a slot")
		}
	}

	if waiters(l, "a") != 0 || l.inFlight("a") != 1 {
		t.Fatalf("after cancel: %d waiters, %d in flight", waiters(l, "a"), l.inFlight("a"))
	}

	hold()

	if l.inFlight("a") != 0 {
		t.Fatal("slot not given back")
	}

	// A wait that times out gives no slot either.
	hold, _ = l.tryAcquire("a")
	defer hold()

	if _, ok := l.acquire(context.Background(), "a", 20*time.Millisecond); ok {
		t.Fatal("timed-out waiter got a slot")
	}
}

func TestAPIWaitsForASlot(t *testing.T) {
	s := newTestServer(t, func(o *Options) {
		o.Limits.MaxConcurrentRequests = 1
		o.Load.LimitWait = 5 * time.Second
	})

	hold, _ := s.inAPI.tryAcquire("test")

	go func() { time.Sleep(50 * time.Millisecond); hold() }()

	w := do(t, s, http.MethodPost, PathAPI, `{"using":["urn:ietf:params:jmap:core"],"methodCalls":[["Core/echo",{},"c1"]]}`)
	if w.Code != http.StatusOK {
		t.Fatalf("a request that could wait answered %d", w.Code)
	}
}

func TestAPILimitAnswers429WithRetryAfter(t *testing.T) {
	s := newTestServer(t, func(o *Options) {
		o.Limits.MaxConcurrentRequests = 1
		o.Load.LimitWait = 20 * time.Millisecond
		o.Load.RetryAfter = 3 * time.Second
	})

	hold, _ := s.inAPI.tryAcquire("test")
	defer hold()

	w := do(t, s, http.MethodPost, PathAPI, `{"using":["urn:ietf:params:jmap:core"],"methodCalls":[]}`)
	if w.Code != http.StatusTooManyRequests || w.Header().Get("Retry-After") != "3" {
		t.Fatalf("answered %d, Retry-After %q", w.Code, w.Header().Get("Retry-After"))
	}

	var p problem
	if err := json.Unmarshal(w.Body.Bytes(), &p); err != nil || p.Type != errURNLimit || p.Limit != "maxConcurrentRequests" {
		t.Fatalf("problem %+v (%v)", p, err)
	}
}

func TestEventSourceNeverWaits(t *testing.T) {
	s := newTestServer(t, func(o *Options) {
		o.Limits.MaxEventSources = 1
		o.Load.LimitWait = 5 * time.Second
	})

	hold, _ := s.inEvents.tryAcquire("test")
	defer hold()

	start := time.Now()

	w := do(t, s, http.MethodGet, PathEvents+"?types=*&closeafter=no&ping=0", "")
	if w.Code != http.StatusTooManyRequests || time.Since(start) > time.Second {
		t.Fatalf("event source answered %d after %s", w.Code, time.Since(start))
	}

	if strings.Contains(w.Body.String(), "maxConcurrentRequests") || !strings.Contains(w.Body.String(), problemAboutBlank) {
		t.Fatalf("event source problem names a session limit: %s", w.Body.String())
	}

	// The API is counted apart.
	if w := do(t, s, http.MethodPost, PathAPI, `{"using":["urn:ietf:params:jmap:core"],"methodCalls":[]}`); w.Code != http.StatusOK {
		t.Fatalf("api beside a full event source limit: %d", w.Code)
	}
}

func TestDownloadsHaveTheirOwnLimit(t *testing.T) {
	s := newTestServer(t, func(o *Options) { o.Limits.MaxConcurrentDownloads = 3 })

	if s.inDown.limit != 3 || s.inEvents.limit != DefaultLimits.MaxEventSources || s.inAPI.limit != 8 {
		t.Fatalf("limits: downloads %d, event sources %d, requests %d", s.inDown.limit, s.inEvents.limit, s.inAPI.limit)
	}

	if got := sessionAs(t, s, "test").Capabilities[CapCore]; !strings.Contains(string(got), `"maxConcurrentRequests":8`) {
		t.Fatalf("session core capability %s", got)
	}
}

// countingAuth counts the authentications that reach it.
type countingAuth struct {
	calls atomic.Int64
	err   error
}

func (c *countingAuth) Authenticate(context.Context, auth.Request) (*auth.Result, error) {
	c.calls.Add(1)
	time.Sleep(20 * time.Millisecond)

	if c.err != nil {
		return nil, c.err
	}

	return &auth.Result{Account: "test"}, nil
}

func (c *countingAuth) Lookup(context.Context, string, string, net.Addr) (*auth.Result, error) {
	return nil, auth.ErrInvalid
}

func TestPerIPLimitBeforeAuthentication(t *testing.T) {
	backend := &countingAuth{}
	s := newTestServer(t, func(o *Options) {
		o.Auth = backend
		o.Load.MaxConcurrentPerIP = 1
	})

	hold, _ := s.perIP.tryAcquire("192.0.2.1") // httptest's client address
	defer hold()

	w := do(t, s, http.MethodGet, PathSession, "")
	if w.Code != http.StatusTooManyRequests || w.Header().Get("Retry-After") == "" || backend.calls.Load() != 0 {
		t.Fatalf("answered %d, backend calls %d", w.Code, backend.calls.Load())
	}

	// No session limit applies, so no RFC 8620 limit error either.
	var p problem
	if err := json.Unmarshal(w.Body.Bytes(), &p); err != nil || p.Type != problemAboutBlank || p.Limit != "" {
		t.Fatalf("problem %+v (%v)", p, err)
	}
}

// TestEventSourceGivesItsAddressSlotBack: an open event source does not
// hold a slot of the bound per client address, so an address with one
// open can still send requests.
func TestEventSourceGivesItsAddressSlotBack(t *testing.T) {
	s := newTestServer(t, func(o *Options) { o.Load.MaxConcurrentPerIP = 1 })
	srv := httptest.NewServer(s)

	t.Cleanup(func() { s.Close(); srv.Close() })

	req, _ := http.NewRequest(http.MethodGet, srv.URL+PathEvents+"?types=*&closeafter=no&ping=0", nil)
	req.SetBasicAuth("test", "secret")

	resp, err := srv.Client().Do(req)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("event source: %v %v", resp, err)
	}
	defer func() { _ = resp.Body.Close() }()

	// The stream is open, holds its event source slot and has given the
	// address slot back.
	waitUntil(t, func() bool { return s.inEvents.inFlight("test") == 1 && s.perIP.inFlight("127.0.0.1") == 0 })

	api, _ := http.NewRequest(http.MethodPost, srv.URL+PathAPI, strings.NewReader(`{"using":["urn:ietf:params:jmap:core"],"methodCalls":[]}`))
	api.SetBasicAuth("test", "secret")

	got, err := srv.Client().Do(api)
	if err != nil {
		t.Fatal(err)
	}

	_ = got.Body.Close()

	if got.StatusCode != http.StatusOK {
		t.Fatalf("a request beside an open event source answered %d", got.StatusCode)
	}
}

func TestTempfailAnswers503WithRetryAfter(t *testing.T) {
	s := newTestServer(t, func(o *Options) { o.Auth = &countingAuth{err: auth.ErrTemporary} })

	w := do(t, s, http.MethodGet, PathSession, "")
	if w.Code != http.StatusServiceUnavailable || w.Header().Get("Retry-After") != "1" {
		t.Fatalf("answered %d, Retry-After %q", w.Code, w.Header().Get("Retry-After"))
	}
}

// TestParallelRequestsShareOneLogin: 50 parallel requests of one account
// make one backend call through the auth cache, and the API requests
// within what the account may have running and waiting all succeed.
func TestParallelRequestsShareOneLogin(t *testing.T) {
	backend := &countingAuth{}
	cache := auth.NewCache(backend, auth.CacheConfig{TTL: time.Minute, MaxEntries: 100, KeyClientIP: true,
		LookupTTL: time.Minute, LeaderBudget: 5 * time.Second})
	s := newTestServer(t, func(o *Options) {
		o.Auth = cache
		o.Load = DefaultLoad
	})

	const n = 50

	codes := make([]int, n)

	var wg sync.WaitGroup

	for i := range n {
		wg.Go(func() {
			r := httptest.NewRequest(http.MethodGet, PathSession, nil)
			r.SetBasicAuth("test", "secret")

			w := httptest.NewRecorder()
			s.ServeHTTP(w, r)
			codes[i] = w.Code
		})
	}

	wg.Wait()

	for i, c := range codes {
		if c != http.StatusOK {
			t.Fatalf("request %d answered %d", i, c)
		}
	}

	if got := backend.calls.Load(); got != 1 {
		t.Fatalf("%d parallel requests made %d backend calls", n, got)
	}

	// As many API requests as may run (8) and wait (16) at once.
	api := 3 * DefaultLimits.MaxConcurrentRequests
	codes = make([]int, api)

	for i := range api {
		wg.Go(func() {
			codes[i] = do(t, s, http.MethodPost, PathAPI,
				`{"using":["urn:ietf:params:jmap:core"],"methodCalls":[["Core/echo",{},"c1"]]}`).Code
		})
	}

	wg.Wait()

	for i, c := range codes {
		if c != http.StatusOK {
			t.Fatalf("api request %d answered %d", i, c)
		}
	}

	if got := backend.calls.Load(); got != 1 {
		t.Fatalf("the api requests asked the backend again: %d calls", got)
	}
}
