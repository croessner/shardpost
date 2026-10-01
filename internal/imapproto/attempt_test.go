package imapproto

import (
	"context"
	"net"
	"sync"
	"testing"

	"shardpost/internal/auth"
	"shardpost/internal/memstore"
)

// ordinalAuth records the attempt ordinals it is asked with.
type ordinalAuth struct {
	auth.Static

	mu   sync.Mutex
	seen []uint
}

func (a *ordinalAuth) Authenticate(ctx context.Context, r auth.Request) (*auth.Result, error) {
	a.mu.Lock()
	a.seen = append(a.seen, r.Attempt)
	a.mu.Unlock()

	return a.Static.Authenticate(ctx, r)
}

func (a *ordinalAuth) Lookup(ctx context.Context, address, protocol string, client net.Addr) (*auth.Result, error) {
	return a.Static.Lookup(ctx, address, protocol, client)
}

// TestLoginAttemptOrdinal: a login carries 1 + the refused logins of
// its connection, which is what Nauthilus reads as auth_login_attempt.
func TestLoginAttemptOrdinal(t *testing.T) {
	oa := &ordinalAuth{Static: auth.Static{"test": "test"}}
	c, _ := newTestServer(t, func(o *Options, _ *memstore.Store) { o.Auth = oa })

	c.cmd("LOGIN test wrong")
	c.cmd("AUTHENTICATE PLAIN AHRlc3QAbm9wZQ==") // test / nope
	c.ok("LOGIN test test")

	oa.mu.Lock()
	defer oa.mu.Unlock()

	if len(oa.seen) != 3 || oa.seen[0] != 1 || oa.seen[1] != 2 || oa.seen[2] != 3 {
		t.Fatalf("ordinals %v, want [1 2 3]", oa.seen)
	}
}
