package auth

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	authv1 "github.com/croessner/nauthilus/v4/api/auth/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// fakeBackend counts the calls that reach it.
type fakeBackend struct {
	auths, lookups atomic.Int64
	// gate, when set, holds every call until it is closed.
	gate chan struct{}
	// started is signalled when a call reaches the backend.
	started chan struct{}
	// ctxErr records the context error a held call saw.
	ctxErr atomic.Value
	auth   func(Request) (*Result, error)
	lookup func(string) (*Result, error)
}

func (f *fakeBackend) hold(ctx context.Context) error {
	if f.started != nil {
		select {
		case f.started <- struct{}{}:
		default:
		}
	}

	if f.gate == nil {
		return nil
	}

	select {
	case <-f.gate:
		return nil
	case <-ctx.Done():
		f.ctxErr.Store(ctx.Err())
		return ErrTemporary
	}
}

func (f *fakeBackend) Authenticate(ctx context.Context, r Request) (*Result, error) {
	f.auths.Add(1)

	if err := f.hold(ctx); err != nil {
		return nil, err
	}

	if f.auth != nil {
		return f.auth(r)
	}

	if r.Password != "secret" && r.Token != "tok" {
		return nil, ErrInvalid
	}

	return &Result{Account: "alice@example.org", Attributes: map[string][]string{"quota": {"100"}}}, nil
}

func (f *fakeBackend) Lookup(ctx context.Context, address, _ string, _ net.Addr) (*Result, error) {
	f.lookups.Add(1)

	if err := f.hold(ctx); err != nil {
		return nil, err
	}

	if f.lookup != nil {
		return f.lookup(address)
	}

	if address != "alice@example.org" && address != "Alias@Example.org" {
		return nil, ErrInvalid
	}

	return &Result{Account: "alice@example.org", Attributes: map[string][]string{"mail": {address}}}, nil
}

func testCache(f *fakeBackend, mod func(*CacheConfig)) *Cache {
	cfg := CacheConfig{TTL: 30 * time.Second, MaxEntries: 100, KeyClientIP: true, LookupTTL: 30 * time.Second,
		LookupNegativeTTL: 5 * time.Second, LeaderBudget: 5 * time.Second}
	if mod != nil {
		mod(&cfg)
	}

	return NewCache(f, cfg)
}

// clock is a settable time for the cache.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }

func (c *clock) add(d time.Duration) { c.mu.Lock(); c.t = c.t.Add(d); c.mu.Unlock() }

func withClock(c *Cache) *clock {
	cl := &clock{t: time.Unix(1_800_000_000, 0)}
	c.now = cl.now

	return cl
}

var (
	clientA = &net.TCPAddr{IP: net.IPv4(10, 0, 0, 1), Port: 1000}
	clientB = &net.TCPAddr{IP: net.IPv4(10, 0, 0, 2), Port: 1000}
)

func login() Request {
	return Request{Username: "alice", Password: "secret", Mechanism: "PLAIN", Protocol: "imap", Client: clientA, TLS: true}
}

func TestCacheHitAndTTL(t *testing.T) {
	f := &fakeBackend{}
	c := testCache(f, nil)
	cl := withClock(c)
	ctx := context.Background()

	for range 3 {
		if res, err := c.Authenticate(ctx, login()); err != nil || res.Account != "alice@example.org" {
			t.Fatalf("%v %v", res, err)
		}
	}

	if n := f.auths.Load(); n != 1 {
		t.Fatalf("backend calls %d, want 1", n)
	}

	// Another port of the same address and another attempt ordinal hit.
	r := login()
	r.Client, r.Attempt = &net.TCPAddr{IP: net.IPv4(10, 0, 0, 1), Port: 2222}, 3

	if _, err := c.Authenticate(ctx, r); err != nil || f.auths.Load() != 1 {
		t.Fatalf("same tuple missed: %v, calls %d", err, f.auths.Load())
	}

	cl.add(31 * time.Second)

	if _, err := c.Authenticate(ctx, login()); err != nil || f.auths.Load() != 2 {
		t.Fatalf("expired entry answered: %v, calls %d", err, f.auths.Load())
	}
}

func TestCacheKeyDistinguishes(t *testing.T) {
	f := &fakeBackend{}
	c := testCache(f, nil)
	ctx := context.Background()

	variants := []func(*Request){
		func(*Request) {},
		func(r *Request) { r.Protocol = "pop3" },
		func(r *Request) { r.TLS = false },
		func(r *Request) { r.Client = clientB },
		func(r *Request) { r.Mechanism = "LOGIN" },
		func(r *Request) { r.Username = "Alice" },
		func(r *Request) { r.Password, r.Token = "", "secret" },
		func(r *Request) { r.Local = &net.TCPAddr{IP: net.IPv4(10, 0, 9, 9), Port: 993} },
	}

	for i, v := range variants {
		r := login()
		v(&r)

		_, _ = c.Authenticate(ctx, r)

		if n := f.auths.Load(); n != int64(i+1) {
			t.Fatalf("variant %d hit another variant's entry (calls %d)", i, n)
		}
	}

	// Without the IP in the key, another address hits.
	f2 := &fakeBackend{}
	c2 := testCache(f2, func(cfg *CacheConfig) { cfg.KeyClientIP = false })
	r := login()
	_, _ = c2.Authenticate(ctx, r)
	r.Client = clientB
	_, _ = c2.Authenticate(ctx, r)

	if n := f2.auths.Load(); n != 1 {
		t.Fatalf("key_client_ip off: %d calls", n)
	}
}

func TestCacheBearerExpiry(t *testing.T) {
	f := &fakeBackend{}
	c := testCache(f, nil)
	cl := withClock(c)
	ctx := context.Background()

	f.auth = func(Request) (*Result, error) {
		return &Result{Account: "alice@example.org", Expiry: cl.now().Add(10 * time.Second)}, nil
	}

	r := Request{Token: "tok", Mechanism: "OAUTHBEARER", Protocol: "jmap", Client: clientA, TLS: true}
	_, _ = c.Authenticate(ctx, r)

	cl.add(9 * time.Second)

	_, _ = c.Authenticate(ctx, r)

	if n := f.auths.Load(); n != 1 {
		t.Fatalf("before exp: %d calls", n)
	}

	cl.add(2 * time.Second)

	_, _ = c.Authenticate(ctx, r)

	if n := f.auths.Load(); n != 2 {
		t.Fatalf("after exp the entry must be gone: %d calls", n)
	}

	// An expiry in the past is not cached at all.
	f.auth = func(Request) (*Result, error) {
		return &Result{Account: "alice@example.org", Expiry: cl.now().Add(-time.Second)}, nil
	}

	r.Token = "other"
	_, _ = c.Authenticate(ctx, r)
	_, _ = c.Authenticate(ctx, r)

	if n := f.auths.Load(); n != 4 {
		t.Fatalf("expired token cached: %d calls", n)
	}
}

func TestCacheLRU(t *testing.T) {
	f := &fakeBackend{}
	c := testCache(f, func(cfg *CacheConfig) { cfg.MaxEntries = 2 })
	ctx := context.Background()

	req := func(p string) Request { r := login(); r.Protocol = p; return r }

	_, _ = c.Authenticate(ctx, req("a"))
	_, _ = c.Authenticate(ctx, req("b"))
	_, _ = c.Authenticate(ctx, req("a")) // a is now the most recent
	_, _ = c.Authenticate(ctx, req("c")) // evicts b

	if n := f.auths.Load(); n != 3 {
		t.Fatalf("calls %d", n)
	}

	_, _ = c.Authenticate(ctx, req("a"))

	if n := f.auths.Load(); n != 3 {
		t.Fatalf("a was evicted: %d", n)
	}

	_, _ = c.Authenticate(ctx, req("b"))

	if n := f.auths.Load(); n != 4 {
		t.Fatalf("b was kept: %d", n)
	}
}

func TestCacheClonesResults(t *testing.T) {
	f := &fakeBackend{}
	c := testCache(f, nil)
	ctx := context.Background()

	res, _ := c.Authenticate(ctx, login())
	res.Attributes["quota"][0] = "changed"
	res.Attributes["new"] = []string{"x"}
	res.Account = "mallory"

	again, _ := c.Authenticate(ctx, login())
	if again.Account != "alice@example.org" || again.Attributes["quota"][0] != "100" || again.Attributes["new"] != nil {
		t.Fatalf("a caller changed the cached result: %+v", again)
	}
}

func TestCacheSingleflight(t *testing.T) {
	f := &fakeBackend{gate: make(chan struct{})}
	c := testCache(f, nil)
	ctx := context.Background()

	const n = 50

	var wg sync.WaitGroup

	results := make([]*Result, n)

	for i := range n {
		wg.Go(func() {
			res, err := c.Authenticate(ctx, login())
			if err != nil {
				t.Error(err)
				return
			}

			results[i] = res
		})
	}

	time.Sleep(50 * time.Millisecond)
	close(f.gate)
	wg.Wait()

	if got := f.auths.Load(); got != 1 {
		t.Fatalf("%d parallel logins made %d backend calls", n, got)
	}

	// Every caller has its own copy.
	results[0].Attributes["quota"][0] = "x"

	for i := 1; i < n; i++ {
		if results[i] == nil || results[i].Attributes["quota"][0] != "100" {
			t.Fatalf("result %d shares the attributes: %+v", i, results[i])
		}
	}
}

func TestCacheSingleflightFailureNotCached(t *testing.T) {
	f := &fakeBackend{gate: make(chan struct{})}
	c := testCache(f, nil)
	ctx := context.Background()

	r := login()
	r.Password = "wrong"

	var wg sync.WaitGroup

	var refused atomic.Int64

	for range 10 {
		wg.Go(func() {
			if _, err := c.Authenticate(ctx, r); errors.Is(err, ErrInvalid) {
				refused.Add(1)
			}
		})
	}

	time.Sleep(30 * time.Millisecond)
	close(f.gate)
	wg.Wait()

	if refused.Load() != 10 || f.auths.Load() != 1 {
		t.Fatalf("refused %d, calls %d", refused.Load(), f.auths.Load())
	}

	// The negative cache is off by default: the next try asks again.
	_, _ = c.Authenticate(ctx, r)

	if f.auths.Load() != 2 {
		t.Fatalf("a refusal was cached without negative_ttl: %d", f.auths.Load())
	}
}

func TestCacheNegative(t *testing.T) {
	f := &fakeBackend{}
	c := testCache(f, func(cfg *CacheConfig) { cfg.NegativeTTL = 5 * time.Second })
	cl := withClock(c)
	ctx := context.Background()

	r := login()
	r.Password = "stale"

	for range 3 {
		if _, err := c.Authenticate(ctx, r); !errors.Is(err, ErrInvalid) {
			t.Fatal(err)
		}
	}

	if f.auths.Load() != 1 {
		t.Fatalf("identical refusals: %d calls", f.auths.Load())
	}

	// Another password is another tuple; the right one is never blocked.
	r.Password = "other"
	_, _ = c.Authenticate(ctx, r)

	if res, err := c.Authenticate(ctx, login()); err != nil || res == nil {
		t.Fatalf("right password after a refusal: %v", err)
	}

	if f.auths.Load() != 3 {
		t.Fatalf("calls %d", f.auths.Load())
	}

	cl.add(6 * time.Second)

	r.Password = "stale"
	_, _ = c.Authenticate(ctx, r)

	if f.auths.Load() != 4 {
		t.Fatalf("negative entry outlived negative_ttl: %d", f.auths.Load())
	}

	// A temporary failure is never cached.
	f.auth = func(Request) (*Result, error) { return nil, ErrTemporary }
	r.Password = "temp"
	_, _ = c.Authenticate(ctx, r)
	_, _ = c.Authenticate(ctx, r)

	if f.auths.Load() != 6 {
		t.Fatalf("tempfail cached: %d", f.auths.Load())
	}
}

func TestCacheWaiterLeavesOnItsContext(t *testing.T) {
	f := &fakeBackend{gate: make(chan struct{}), started: make(chan struct{}, 1)}
	c := testCache(f, nil)

	// The first caller gives up; the backend call goes on for the
	// others and its answer is stored.
	first, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)

	go func() { _, err := c.Authenticate(first, login()); done <- err }()

	<-f.started
	cancel()

	if err := <-done; !errors.Is(err, ErrTemporary) {
		t.Fatalf("the cancelled caller got %v", err)
	}

	second := make(chan error, 1)

	go func() { _, err := c.Authenticate(context.Background(), login()); second <- err }()

	time.Sleep(20 * time.Millisecond)
	close(f.gate)

	if err := <-second; err != nil {
		t.Fatalf("the waiter got %v", err)
	}

	if f.ctxErr.Load() != nil {
		t.Fatalf("the leader's call was cancelled with the first caller: %v", f.ctxErr.Load())
	}

	if _, err := c.Authenticate(context.Background(), login()); err != nil || f.auths.Load() != 1 {
		t.Fatalf("answer not stored: %v, calls %d", err, f.auths.Load())
	}
}

func TestCacheLeaderBudget(t *testing.T) {
	f := &fakeBackend{gate: make(chan struct{})}
	defer close(f.gate)

	c := testCache(f, func(cfg *CacheConfig) { cfg.LeaderBudget = 50 * time.Millisecond })

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	start := time.Now()

	_, err := c.Authenticate(ctx, login())
	if err == nil || time.Since(start) > time.Second {
		t.Fatalf("budget not applied: %v after %s", err, time.Since(start))
	}

	if !errors.Is(f.ctxErr.Load().(error), context.DeadlineExceeded) {
		t.Fatalf("backend context: %v", f.ctxErr.Load())
	}
}

func TestCacheLookupKey(t *testing.T) {
	f := &fakeBackend{}
	c := testCache(f, nil)
	ctx := context.Background()

	for _, addr := range []string{"alice@example.org", "<alice@EXAMPLE.org>", " alice@example.ORG "} {
		if res, err := c.Lookup(ctx, addr, "lmtp", nil); err != nil || res.Account != "alice@example.org" {
			t.Fatalf("%s: %v %v", addr, res, err)
		}
	}

	if f.lookups.Load() != 1 {
		t.Fatalf("normalised names missed: %d", f.lookups.Load())
	}

	// The protocol is part of the key: Nauthilus picks its search per
	// protocol (the jmap one returns more attributes).
	_, _ = c.Lookup(ctx, "alice@example.org", "jmap", nil)

	if f.lookups.Load() != 2 {
		t.Fatalf("protocols share an entry: %d", f.lookups.Load())
	}
}

func TestCacheLookupNegative(t *testing.T) {
	f := &fakeBackend{}
	c := testCache(f, nil)
	cl := withClock(c)
	ctx := context.Background()

	_, _ = c.Lookup(ctx, "alice@example.org", "lmtp", nil)
	_, _ = c.Lookup(ctx, "alice@example.org", "jmap", nil)

	// Unknown: negative for lookup_negative_ttl.
	for range 2 {
		if _, err := c.Lookup(ctx, "new@example.org", "lmtp", nil); !errors.Is(err, ErrInvalid) {
			t.Fatal(err)
		}
	}

	if f.lookups.Load() != 3 {
		t.Fatalf("negative lookup not cached: %d", f.lookups.Load())
	}

	// The account appears; an admin lookup goes past the negative entry
	// and stores the positive answer for everyone.
	f.lookup = func(string) (*Result, error) { return &Result{Account: "new@example.org"}, nil }

	if res, err := c.Lookup(AdminLookup(ctx), "new@example.org", "lmtp", nil); err != nil || res.Account != "new@example.org" {
		t.Fatalf("admin lookup: %v %v", res, err)
	}

	if res, err := c.Lookup(ctx, "new@example.org", "lmtp", nil); err != nil || res.Account != "new@example.org" || f.lookups.Load() != 4 {
		t.Fatalf("admin result not stored: %v %v (%d)", res, err, f.lookups.Load())
	}

	// An admin lookup that finds nothing stores nothing.
	f.lookup = nil

	_, _ = c.Lookup(AdminLookup(ctx), "gone@example.org", "lmtp", nil)
	_, _ = c.Lookup(AdminLookup(ctx), "gone@example.org", "lmtp", nil)

	if f.lookups.Load() != 6 {
		t.Fatalf("admin refusals cached: %d", f.lookups.Load())
	}

	cl.add(6 * time.Second)

	_, _ = c.Lookup(ctx, "unknown@example.org", "lmtp", nil)

	cl.add(6 * time.Second)

	_, _ = c.Lookup(ctx, "unknown@example.org", "lmtp", nil)

	if f.lookups.Load() != 8 {
		t.Fatalf("negative lookup outlived its ttl: %d", f.lookups.Load())
	}
}

func TestCacheFlush(t *testing.T) {
	f := &fakeBackend{}
	c := testCache(f, nil)
	ctx := context.Background()

	_, _ = c.Authenticate(ctx, login())                                                        // login "alice", account alice@example.org
	_, _ = c.Lookup(ctx, "Alias@Example.org", "lmtp", nil)                                     // lookup name alias@example.org
	_, _ = c.Lookup(ctx, "alice@example.org", "jmap", nil)                                     // both names alice@example.org
	_, _ = c.Authenticate(ctx, Request{Username: "bob", Password: "secret", Protocol: "imap"}) // bob, alice@example.org

	if c.Len() != 4 {
		t.Fatalf("entries %d", c.Len())
	}

	// By account name, case-insensitive: every entry of that account.
	c.Flush("ALICE@example.org")

	if c.Len() != 0 {
		t.Fatalf("account flush left %d entries", c.Len())
	}

	_, _ = c.Authenticate(ctx, login())
	_, _ = c.Authenticate(ctx, Request{Username: "carol", Password: "x", Protocol: "imap"}) // not cached (refused)

	// By login name.
	c.Flush("Alice")

	if c.Len() != 0 {
		t.Fatalf("login flush left %d entries", c.Len())
	}

	_, _ = c.Authenticate(ctx, login())
	c.Flush("")

	if c.Len() != 0 {
		t.Fatalf("flush all left %d entries", c.Len())
	}
}

func TestCacheFlushDuringCall(t *testing.T) {
	f := &fakeBackend{gate: make(chan struct{}), started: make(chan struct{}, 1)}
	c := testCache(f, nil)
	done := make(chan struct{})

	go func() { _, _ = c.Authenticate(context.Background(), login()); close(done) }()

	<-f.started
	c.Flush("alice@example.org")
	close(f.gate)
	<-done

	if c.Len() != 0 {
		t.Fatal("an answer from before the flush was stored")
	}
}

func TestCacheSweep(t *testing.T) {
	f := &fakeBackend{}
	c := testCache(f, nil)
	cl := withClock(c)

	_, _ = c.Authenticate(context.Background(), login())

	cl.add(time.Minute)
	c.Sweep()

	if c.Len() != 0 {
		t.Fatalf("expired entries kept: %d", c.Len())
	}
}

func TestCacheOff(t *testing.T) {
	f := &fakeBackend{}
	c := testCache(f, func(cfg *CacheConfig) { cfg.TTL, cfg.LookupTTL = 0, 0 })
	ctx := context.Background()

	_, _ = c.Authenticate(ctx, login())
	_, _ = c.Authenticate(ctx, login())
	_, _ = c.Lookup(ctx, "alice@example.org", "", nil)
	_, _ = c.Lookup(ctx, "alice@example.org", "", nil)

	if f.auths.Load() != 2 || f.lookups.Load() != 2 || c.Len() != 0 {
		t.Fatal("a cache with ttl 0 cached")
	}
}

// TestCacheMasterUserLayering: the master password is checked before
// the cache and never cached; the target's lookup is.
func TestCacheMasterUserLayering(t *testing.T) {
	f := &fakeBackend{}
	c := testCache(f, nil)
	m := &MasterUser{Next: c, Name: "director", Password: "master"}
	ctx := context.Background()

	for range 3 {
		res, err := m.Authenticate(ctx, Request{Username: "alice@example.org*director", Password: "master", Protocol: "imap"})
		if err != nil || res.Account != "alice@example.org" {
			t.Fatalf("%v %v", res, err)
		}
	}

	if f.lookups.Load() != 1 || f.auths.Load() != 0 {
		t.Fatalf("lookups %d auths %d", f.lookups.Load(), f.auths.Load())
	}

	if _, err := m.Authenticate(ctx, Request{Username: "alice@example.org*director", Password: "wrong", Protocol: "imap"}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("a wrong master password passed: %v", err)
	}
}

// TestCacheAuthWaitShared: the Auth-Wait of a refusal over HTTP is slept
// once, by the backend call, and every identical caller waits it out.
func TestCacheAuthWaitShared(t *testing.T) {
	var calls atomic.Int64

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		time.Sleep(30 * time.Millisecond) // keep the flight open for the others
		w.Header().Set("Auth-Wait", "0.2")
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()

	n := &Nauthilus{URL: srv.URL, MaxWait: time.Second}
	c := NewCache(n, CacheConfig{TTL: time.Minute, MaxEntries: 10, LookupTTL: time.Minute, LeaderBudget: 5 * time.Second})

	var wg sync.WaitGroup

	start := time.Now()

	for range 5 {
		wg.Go(func() {
			if _, err := c.Authenticate(context.Background(), Request{Username: "u", Password: "bad", Protocol: "imap"}); !errors.Is(err, ErrInvalid) {
				t.Errorf("got %v", err)
			}

			if time.Since(start) < 200*time.Millisecond {
				t.Errorf("a caller did not wait for Auth-Wait: %s", time.Since(start))
			}
		})
	}

	wg.Wait()

	if calls.Load() != 1 {
		t.Fatalf("%d backend calls", calls.Load())
	}
}

// TestCachePanickingBackend: a panic in the shared backend call is a
// temporary failure for every caller, not the end of the process, and
// leaves no flight behind.
func TestCachePanickingBackend(t *testing.T) {
	f := &fakeBackend{auth: func(Request) (*Result, error) { panic("boom") }}
	c := testCache(f, nil)

	for range 2 {
		if _, err := c.Authenticate(context.Background(), login()); !errors.Is(err, ErrTemporary) {
			t.Fatalf("got %v", err)
		}
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	if len(c.calls) != 0 || f.auths.Load() != 2 {
		t.Fatalf("flights left %d, calls %d", len(c.calls), f.auths.Load())
	}
}

func TestClaimTime(t *testing.T) {
	want := time.Unix(1_800_000_000, 0)

	for _, v := range []any{float64(1_800_000_000), json.Number("1800000000"), "1800000000"} {
		if got := claimTime(v); !got.Equal(want) {
			t.Errorf("%T %v: %v", v, v, got)
		}
	}

	for _, v := range []any{nil, "soon", float64(0), "-5", true} {
		if got := claimTime(v); !got.IsZero() {
			t.Errorf("%v gave %v", v, got)
		}
	}
}

func TestSSLFlag(t *testing.T) {
	if sslFlag(true) != "on" || sslFlag(false) != "" {
		t.Fatalf("ssl: %q %q", sslFlag(true), sslFlag(false))
	}
}

func TestLoginAttempts(t *testing.T) {
	var seen []uint

	backend := &fakeBackend{auth: func(r Request) (*Result, error) {
		seen = append(seen, r.Attempt)

		switch r.Password {
		case "secret":
			return &Result{Account: "alice"}, nil
		case "temp":
			return nil, ErrTemporary
		default:
			return nil, ErrInvalid
		}
	}}

	var a LoginAttempts

	ctx := context.Background()

	for _, pw := range []string{"bad", "temp", "bad", "secret"} {
		_, _ = a.Authenticate(ctx, backend, Request{Username: "alice", Password: pw})
	}

	// Refusals count, a temporary failure does not.
	want := []uint{1, 2, 2, 3}
	for i := range want {
		if seen[i] != want[i] {
			t.Fatalf("ordinals %v, want %v", seen, want)
		}
	}
}

// refusingClient refuses the backchannel credentials, after a delay.
type refusingClient struct {
	authv1.AuthServiceClient
	calls atomic.Int64
	delay time.Duration
}

func (r *refusingClient) Authenticate(ctx context.Context, _ *authv1.AuthRequest, _ ...grpc.CallOption) (*authv1.AuthResponse, error) {
	r.calls.Add(1)

	select {
	case <-time.After(r.delay):
	case <-ctx.Done():
	}

	return nil, status.Error(codes.Unauthenticated, "token expired")
}

// countingTokens is a TokenSource that counts invalidations.
type countingTokens struct{ invalidated atomic.Int64 }

func (c *countingTokens) Token(context.Context) (string, error) { return "t", nil }
func (c *countingTokens) Invalidate()                           { c.invalidated.Add(1) }

func TestGRPCRetryNeedsBudget(t *testing.T) {
	cl := &refusingClient{delay: 40 * time.Millisecond}
	ts := &countingTokens{}
	tr := (&GRPCTransport{Timeout: time.Second, TokenSource: ts}).WithClient(cl)

	// Plenty of time: the refused token is dropped and the call retried.
	if _, err := tr.call(context.Background(), "", nauthilusRequest{}); err != nil || cl.calls.Load() != 2 {
		t.Fatalf("with budget: %v, %d calls", err, cl.calls.Load())
	}

	// Less time left than the first attempt took: no retry.
	cl.calls.Store(0)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
	defer cancel()

	if _, err := tr.call(ctx, "", nauthilusRequest{}); err != nil || cl.calls.Load() != 1 {
		t.Fatalf("without budget: %v, %d calls", err, cl.calls.Load())
	}

	if ts.invalidated.Load() != 2 {
		t.Fatalf("token invalidated %d times", ts.invalidated.Load())
	}
}

func TestNewHTTPClient(t *testing.T) {
	c := NewHTTPClient(3*time.Second, 7)

	tr, ok := c.Transport.(*http.Transport)
	if !ok || tr.MaxIdleConnsPerHost != 7 || c.Timeout != 3*time.Second {
		t.Fatalf("client %+v", c)
	}
}
