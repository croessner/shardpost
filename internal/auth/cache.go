package auth

import (
	"container/list"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"hash"
	"log/slog"
	"net"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"time"

	"shardpost/internal/metrics"
)

// The operations of the cache, as the metrics label them.
const (
	opAuthenticate = "authenticate"
	opLookup       = "lookup"
)

// The results of a cache question, as the metrics label them.
const (
	resultHit    = "hit"
	resultMiss   = "miss"
	resultShared = "shared"
	resultBypass = "bypass"
)

// CacheConfig configures the auth cache (auth.cache).
type CacheConfig struct {
	// TTL is how long a successful authentication is answered from the
	// cache, counted from when it was stored. A bearer token's entry
	// ends at the token's expiry when that comes first.
	TTL time.Duration
	// NegativeTTL is how long a refused authentication is answered from
	// the cache, for exactly the same credentials; 0 caches no refusal.
	NegativeTTL time.Duration
	// MaxEntries bounds the entries of each operation (least recently
	// used go first).
	MaxEntries int
	// KeyClientIP puts the client's IP address into the authentication
	// key, so that IP-based rules in the backend still see every
	// address once.
	KeyClientIP bool
	// LookupTTL and LookupNegativeTTL are how long a lookup that found
	// an account, and one that found none, are answered from the cache;
	// 0 caches none of that kind.
	LookupTTL         time.Duration
	LookupNegativeTTL time.Duration
	// LeaderBudget bounds one backend call made on behalf of every
	// caller waiting for it (an Authenticate with a bearer token can be
	// up to three requests).
	LeaderBudget time.Duration
}

// Cache answers repeated authentications and lookups from memory and
// joins identical concurrent ones into one backend call. It wraps the
// Nauthilus authenticator only: a master-user login is decided before
// it (MasterUser{Next: Cache{Next: Nauthilus}}), so the master password
// is never cached, only the lookup of the target account.
//
// Secrets are never kept: an authentication is found by an HMAC over
// the mechanism, the user name, the secret, the protocol, the TLS state
// and (configurable) the client's IP address, under a key that lives in
// this process only. Results are cloned on the way in and out, so no
// caller sees another's attribute map.
type Cache struct {
	next Authenticator
	cfg  CacheConfig
	key  []byte
	now  func() time.Time

	mu    sync.Mutex
	ops   map[string]*lru
	index map[string]map[*cacheEntry]struct{}
	calls map[string]*flight
	// gen counts the flushes; a backend answer that started before a
	// flush is not stored after it.
	gen uint64
}

var _ Authenticator = (*Cache)(nil)

// NewCache wraps next.
func NewCache(next Authenticator, cfg CacheConfig) *Cache {
	key := make([]byte, 32)
	_, _ = rand.Read(key)

	if cfg.MaxEntries <= 0 {
		cfg.MaxEntries = 10000
	}

	if cfg.LeaderBudget <= 0 {
		cfg.LeaderBudget = 2 * DefaultTimeout
	}

	c := &Cache{next: next, cfg: cfg, key: key, now: time.Now,
		ops:   map[string]*lru{opAuthenticate: newLRU(), opLookup: newLRU()},
		index: map[string]map[*cacheEntry]struct{}{}, calls: map[string]*flight{}}

	return c
}

// cacheEntry is one cached answer; res nil is a refusal.
type cacheEntry struct {
	op      string
	key     string
	res     *Result
	expires time.Time
	// names are the lower-cased names a flush of an account finds the
	// entry under.
	names []string
	elem  *list.Element
}

// lru is the entries of one operation, most recently used in front.
type lru struct {
	byKey map[string]*cacheEntry
	order *list.List
}

func newLRU() *lru { return &lru{byKey: map[string]*cacheEntry{}, order: list.New()} }

// Authenticate implements Authenticator.
func (c *Cache) Authenticate(ctx context.Context, r Request) (*Result, error) {
	if c.cfg.TTL <= 0 {
		return c.next.Authenticate(ctx, r)
	}

	key := c.authKey(r)

	if e, ok := c.get(opAuthenticate, key); ok {
		metrics.AuthCache.WithLabelValues(opAuthenticate, resultHit).Inc()

		if e == nil {
			return nil, ErrInvalid
		}

		return e, nil
	}

	return c.do(ctx, opAuthenticate, key, func(lctx context.Context) (*Result, error) {
		return c.next.Authenticate(lctx, r)
	}, func(gen uint64, res *Result, err error) {
		c.storeAuth(gen, key, r, res, err)
	})
}

// storeAuth keeps the answer of an authentication.
func (c *Cache) storeAuth(gen uint64, key string, r Request, res *Result, err error) {
	now := c.now()

	switch {
	case err == nil:
		expires := now.Add(c.cfg.TTL)
		if !res.Expiry.IsZero() && res.Expiry.Before(expires) {
			expires = res.Expiry
		}

		if !expires.After(now) {
			return
		}

		c.put(gen, opAuthenticate, key, res.Clone(), expires, r.Username, res.Account)
	case c.cfg.NegativeTTL > 0 && errors.Is(err, ErrInvalid) && !errors.Is(err, ErrTemporary):
		c.put(gen, opAuthenticate, key, nil, now.Add(c.cfg.NegativeTTL), r.Username)
	}
}

// Lookup implements Authenticator. A lookup on a context marked with
// AdminLookup goes past a cached refusal and does not store one: an
// operator who just created an account sees it at once.
func (c *Cache) Lookup(ctx context.Context, address, protocol string, client net.Addr) (*Result, error) {
	if c.cfg.LookupTTL <= 0 {
		return c.next.Lookup(ctx, address, protocol, client)
	}

	name := NormalizeAddress(address)

	if protocol == "" {
		protocol = defaultLookupProtocol
	}

	key := lengthPrefixed(protocol, name)
	admin := isAdminLookup(ctx)

	if e, ok := c.get(opLookup, key); ok {
		switch {
		case e != nil:
			metrics.AuthCache.WithLabelValues(opLookup, resultHit).Inc()
			return e, nil
		case !admin:
			metrics.AuthCache.WithLabelValues(opLookup, resultHit).Inc()
			return nil, ErrInvalid
		default:
			metrics.AuthCache.WithLabelValues(opLookup, resultBypass).Inc()
		}
	}

	flightKey := key
	if admin {
		// An admin lookup does not join a flight that might store a
		// refusal for it, nor hand one to callers who would.
		flightKey = "admin" + key
	}

	return c.do(ctx, opLookup, flightKey, func(lctx context.Context) (*Result, error) {
		return c.next.Lookup(lctx, address, protocol, client)
	}, func(gen uint64, res *Result, err error) {
		now := c.now()

		switch {
		case err == nil:
			c.put(gen, opLookup, key, res.Clone(), now.Add(c.cfg.LookupTTL), name, res.Account)
		case !admin && c.cfg.LookupNegativeTTL > 0 && errors.Is(err, ErrInvalid) && !errors.Is(err, ErrTemporary):
			c.put(gen, opLookup, key, nil, now.Add(c.cfg.LookupNegativeTTL), name)
		}
	})
}

// defaultLookupProtocol is the protocol of a lookup that names none, as
// Nauthilus.Lookup sends it.
const defaultLookupProtocol = "lmtp"

// NormalizeAddress is the form of an address Nauthilus is asked with:
// without angle brackets and surrounding space, with the domain in
// lower case (the local part keeps its case).
func NormalizeAddress(address string) string {
	addr := strings.TrimSpace(strings.Trim(address, "<>"))
	if i := strings.LastIndex(addr, "@"); i >= 0 {
		addr = addr[:i] + strings.ToLower(addr[i:])
	}

	return addr
}

// adminLookupKey marks the context of an admin API lookup.
type adminLookupKey struct{}

// AdminLookup marks ctx as an admin API lookup (diagnosis, import,
// delivery): the cache goes past a cached refusal and stores what it
// finds.
func AdminLookup(ctx context.Context) context.Context {
	return context.WithValue(ctx, adminLookupKey{}, true)
}

func isAdminLookup(ctx context.Context) bool {
	v, _ := ctx.Value(adminLookupKey{}).(bool)
	return v
}

// authKey is the HMAC of everything an authentication's answer depends
// on, including the local address and port Nauthilus is told (rules may
// depend on the listener). Attempt is left out: it is the connection's
// count, not part of the credentials. The mechanism is kept as it is, so
// PLAIN and LOGIN with the same password are two entries; Nauthilus may
// treat them differently.
func (c *Cache) authKey(r Request) string {
	m := hmac.New(sha256.New, c.key)

	secretKind := "p"
	secret := r.Password

	if r.Token != "" {
		secretKind, secret = "t", r.Token
	}

	tls := "0"
	if r.TLS {
		tls = "1"
	}

	ip := ""
	if c.cfg.KeyClientIP {
		ip, _ = hostPort(r.Client)
	}

	localIP, localPort := hostPort(r.Local)

	for _, part := range []string{strings.ToLower(r.Mechanism), r.Username, secretKind, secret, r.Protocol, tls, ip, localIP, localPort} {
		writePart(m, part)
	}

	return string(m.Sum(nil))
}

// writePart writes a length-prefixed part, so that no two different
// tuples hash the same bytes.
func writePart(h hash.Hash, s string) {
	var n [4]byte

	binary.BigEndian.PutUint32(n[:], uint32(len(s)))
	_, _ = h.Write(n[:])
	_, _ = h.Write([]byte(s))
}

// lengthPrefixed joins parts unambiguously (the lookup key).
func lengthPrefixed(parts ...string) string {
	var b strings.Builder

	for _, p := range parts {
		_, _ = fmt.Fprintf(&b, "%d:%s", len(p), p)
	}

	return b.String()
}

// get returns a live entry's result (cloned; nil for a refusal). An
// entry's result is never changed after put, so it is cloned outside
// the lock.
func (c *Cache) get(op, key string) (*Result, bool) {
	res, ok := c.lookupEntry(op, key)
	if !ok {
		return nil, false
	}

	return res.Clone(), true
}

// lookupEntry returns the stored result of a live entry and marks it
// used.
func (c *Cache) lookupEntry(op, key string) (*Result, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	l := c.ops[op]

	e, ok := l.byKey[key]
	if !ok {
		return nil, false
	}

	if !c.now().Before(e.expires) {
		c.remove(e)
		c.gauge(op)

		return nil, false
	}

	l.order.MoveToFront(e.elem)

	return e.res, true
}

// put stores an entry unless a flush happened since gen.
func (c *Cache) put(gen uint64, op, key string, res *Result, expires time.Time, names ...string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if gen != c.gen {
		return
	}

	l := c.ops[op]
	if old, ok := l.byKey[key]; ok {
		c.remove(old)
	}

	e := &cacheEntry{op: op, key: key, res: res, expires: expires}

	for _, n := range names {
		if n = strings.ToLower(NormalizeAddress(n)); n != "" && !containsString(e.names, n) {
			e.names = append(e.names, n)
		}
	}

	e.elem = l.order.PushFront(e)
	l.byKey[key] = e

	for _, n := range e.names {
		set := c.index[n]
		if set == nil {
			set = map[*cacheEntry]struct{}{}
			c.index[n] = set
		}

		set[e] = struct{}{}
	}

	for l.order.Len() > c.cfg.MaxEntries {
		c.remove(l.order.Back().Value.(*cacheEntry))
	}

	c.gauge(op)
}

func containsString(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}

	return false
}

// remove drops an entry from its LRU and the index; c.mu is held.
func (c *Cache) remove(e *cacheEntry) {
	l := c.ops[e.op]
	if l.byKey[e.key] != e {
		return
	}

	delete(l.byKey, e.key)
	l.order.Remove(e.elem)

	for _, n := range e.names {
		if set := c.index[n]; set != nil {
			delete(set, e)

			if len(set) == 0 {
				delete(c.index, n)
			}
		}
	}
}

// gauge publishes the size of an operation's entries; c.mu is held.
func (c *Cache) gauge(op string) {
	metrics.AuthCacheEntries.WithLabelValues(op).Set(float64(c.ops[op].order.Len()))
}

// Flush drops the entries of an account — every entry whose login,
// lookup name or account is the given name, compared in lower case — or
// all entries when account is empty or "*". Answers of backend calls
// that are still running are not stored.
func (c *Cache) Flush(account string) {
	c.flush(account, "local")
}

// flush is Flush with the source of the flush for the metrics.
func (c *Cache) flush(account, source string) {
	account = strings.ToLower(NormalizeAddress(account))

	c.mu.Lock()
	defer c.mu.Unlock()

	c.gen++

	if account == "" || account == FlushAll {
		for op := range c.ops {
			c.ops[op] = newLRU()
			c.gauge(op)
		}

		c.index = map[string]map[*cacheEntry]struct{}{}

		metrics.AuthCacheFlushes.WithLabelValues("all", source).Inc()

		return
	}

	for e := range c.index[account] {
		c.remove(e)
	}

	for op := range c.ops {
		c.gauge(op)
	}

	metrics.AuthCacheFlushes.WithLabelValues("account", source).Inc()
}

// FlushAll is the account name that flushes every entry.
const FlushAll = "*"

// Sweep drops the expired entries (the flush log calls it on its poll).
func (c *Cache) Sweep() {
	c.mu.Lock()
	defer c.mu.Unlock()

	now := c.now()

	for op, l := range c.ops {
		for el := l.order.Back(); el != nil; {
			prev := el.Prev()

			if e := el.Value.(*cacheEntry); !now.Before(e.expires) {
				c.remove(e)
			}

			el = prev
		}

		c.gauge(op)
	}
}

// Len returns the number of entries of both operations (tests).
func (c *Cache) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.ops[opAuthenticate].order.Len() + c.ops[opLookup].order.Len()
}

// flight is one backend call that every caller with the same key waits
// for.
type flight struct {
	done chan struct{}
	res  *Result
	err  error
}

// do runs fn once for all concurrent callers with the same key. The call
// runs on a context detached from the first caller's (it keeps its
// values, so the trace span goes along) and bounded by the leader
// budget; every caller, the first included, stops waiting when its own
// context ends. store sees the answer before anyone is woken.
func (c *Cache) do(ctx context.Context, op, key string, fn func(context.Context) (*Result, error),
	store func(gen uint64, res *Result, err error),
) (*Result, error) {
	c.mu.Lock()

	// The flush generation is part of the flight's key: a caller that
	// comes after a flush does not join a call that started before it,
	// whose answer may be what the flush was for.
	gen := c.gen
	fkey := op + "\x00" + strconv.FormatUint(gen, 10) + "\x00" + key
	f, joined := c.calls[fkey]

	if !joined {
		f = &flight{done: make(chan struct{})}
		c.calls[fkey] = f

		go c.lead(ctx, op, fkey, f, gen, fn, store)
	}
	c.mu.Unlock()

	result := resultMiss
	if joined {
		result = resultShared
	}

	metrics.AuthCache.WithLabelValues(op, result).Inc()

	select {
	case <-f.done:
		if f.err != nil {
			return nil, f.err
		}

		return f.res.Clone(), nil
	case <-ctx.Done():
		return nil, fmt.Errorf("%w: %v", ErrTemporary, ctx.Err())
	}
}

// callRecovered runs fn and turns a panic into a temporary failure: the
// call runs in a goroutine of its own, where a panic would end the
// process instead of the one connection or request it came from.
func callRecovered(ctx context.Context, fn func(context.Context) (*Result, error)) (res *Result, err error) {
	defer func() {
		if p := recover(); p != nil {
			slog.Error("auth: backend call panicked", "panic", p, "stack", string(debug.Stack()))

			res, err = nil, fmt.Errorf("%w: backend call failed", ErrTemporary)
		}
	}()

	return fn(ctx)
}

// lead performs the backend call of a flight.
func (c *Cache) lead(ctx context.Context, op, fkey string, f *flight, gen uint64, fn func(context.Context) (*Result, error),
	store func(gen uint64, res *Result, err error),
) {
	lctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), c.cfg.LeaderBudget)
	defer cancel()

	start := time.Now()
	res, err := callRecovered(lctx, fn)

	metrics.AuthBackend.WithLabelValues(op).Observe(time.Since(start).Seconds())

	if err == nil && res == nil {
		err = fmt.Errorf("%w: backend returned no result", ErrTemporary)
	}

	store(gen, res, err)

	c.mu.Lock()
	delete(c.calls, fkey)
	c.mu.Unlock()

	f.res, f.err = res, err
	close(f.done)
}
