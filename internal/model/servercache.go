package model

import (
	"maps"
	"sync"
	"time"
)

// ServerMetadataTTL is how long a process keeps the shared server
// metadata (the METADATA entries of the reserved namespace "~server"):
// every IMAPSieve event without a mailbox script and every GETMETADATA
// of a /shared server entry reads them, so without a cache one object
// takes a read per event of the whole cluster. A change made through
// this process is seen at once; one made elsewhere within this time.
const ServerMetadataTTL = 30 * time.Second

// ServerMetadataCache keeps the server metadata of a process for TTL.
// The zero value is ready to use (ServerMetadataTTL, time.Now).
type ServerMetadataCache struct {
	// TTL is how long a read is kept (ServerMetadataTTL when 0).
	TTL time.Duration
	// Now is the clock (time.Now when nil).
	Now func() time.Time

	mu  sync.Mutex
	val map[string]string
	exp time.Time
	// gen counts the invalidations: a read that started before one is
	// not kept.
	gen uint64
}

// Get returns the cached entries, or what load returns when the cache is
// empty or expired. The map is the caller's.
func (c *ServerMetadataCache) Get(load func() (map[string]string, error)) (map[string]string, error) {
	now := c.now()

	c.mu.Lock()
	if c.val != nil && now.Before(c.exp) {
		v := maps.Clone(c.val)
		c.mu.Unlock()

		return v, nil
	}

	gen := c.gen
	c.mu.Unlock()

	v, err := load()
	if err != nil {
		return nil, err
	}

	if v == nil {
		v = map[string]string{}
	}

	ttl := c.TTL
	if ttl <= 0 {
		ttl = ServerMetadataTTL
	}

	c.mu.Lock()
	if c.gen == gen {
		c.val, c.exp = maps.Clone(v), now.Add(ttl)
	}
	c.mu.Unlock()

	return v, nil
}

// Invalidate drops the cached entries (a change through this process).
func (c *ServerMetadataCache) Invalidate() {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.val = nil
	c.gen++
}

func (c *ServerMetadataCache) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}

	return time.Now()
}
