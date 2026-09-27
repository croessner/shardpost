package jmap

import (
	"sync"
	"time"
)

// The lookups of addresses that turn out to be no principal of this
// server are an oracle: whoever can log in could ask, request after
// request, which addresses exist. jmap.max_principal_lookups bounds one
// request; missBudget bounds an account across requests. Every account
// has a bucket of misses that refills at jmap.principal_miss_per_minute;
// a lookup is only made while the bucket holds a miss, and only a miss
// takes one out of it. A lookup that finds a principal costs nothing, so
// sharing with the colleagues one knows is never held back.
//
// The buckets live in the memory of one process: each front-end of a
// deployment grants its own rate, and a restart refills them. That is a
// bound on the oracle, not a quota; Nauthilus sees every lookup with the
// client's address and can rate-limit on top.

// defaultPrincipalMissPerMinute is the refill rate a configuration that
// says nothing gets.
const defaultPrincipalMissPerMinute = 30

// maxMissBuckets bounds the accounts the process keeps a bucket for; a
// full bucket is dropped first, the least recently used after that.
const maxMissBuckets = 10000

// missBucket is one account's misses.
type missBucket struct {
	tokens float64
	// last is when tokens was computed, used when the account last
	// missed.
	last, used time.Time
}

// missBudget is the buckets of one process.
type missBudget struct {
	mu      sync.Mutex
	rate    float64 // misses per minute, and the size of a bucket
	buckets map[string]*missBucket
	now     func() time.Time
}

// newMissBudget builds the budget; perMinute <= 0 takes the default.
func newMissBudget(perMinute int) *missBudget {
	if perMinute <= 0 {
		perMinute = defaultPrincipalMissPerMinute
	}

	return &missBudget{rate: float64(perMinute), buckets: map[string]*missBucket{}, now: time.Now}
}

// refill tops a bucket up to now.
func (m *missBudget) refill(b *missBucket, now time.Time) {
	b.tokens = min(m.rate, b.tokens+now.Sub(b.last).Minutes()*m.rate)
	b.last = now
}

// allow reports whether the account may make a lookup that may miss.
func (m *missBudget) allow(account string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()

	b := m.buckets[account]
	if b == nil {
		return true
	}

	m.refill(b, m.now())

	return b.tokens >= 1
}

// miss takes one miss out of the account's bucket.
func (m *missBudget) miss(account string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	now := m.now()

	b := m.buckets[account]
	if b == nil {
		m.makeRoom(now)

		b = &missBucket{tokens: m.rate, last: now}
		m.buckets[account] = b
	}

	m.refill(b, now)
	b.tokens = max(b.tokens-1, 0)
	b.used = now
}

// makeRoom keeps the map below maxMissBuckets before one more is added:
// the buckets that are full again carry no state and go first, then the
// least recently used.
func (m *missBudget) makeRoom(now time.Time) {
	if len(m.buckets) < maxMissBuckets {
		return
	}

	var (
		oldest    string
		oldestAt  time.Time
		foundFull bool
	)

	for account, b := range m.buckets {
		if b.tokens+now.Sub(b.last).Minutes()*m.rate >= m.rate {
			delete(m.buckets, account)

			foundFull = true

			continue
		}

		if oldest == "" || b.used.Before(oldestAt) {
			oldest, oldestAt = account, b.used
		}
	}

	if !foundFull && oldest != "" {
		delete(m.buckets, oldest)
	}
}
