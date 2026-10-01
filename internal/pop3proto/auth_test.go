package pop3proto

import (
	"context"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"shardpost/internal/auth"
	"shardpost/internal/memstore"
)

// recordingAuth records the attempt ordinals and the deadlines it sees.
type recordingAuth struct {
	mu        sync.Mutex
	attempts  []uint
	deadlines []time.Duration
}

func (a *recordingAuth) Authenticate(ctx context.Context, r auth.Request) (*auth.Result, error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	a.attempts = append(a.attempts, r.Attempt)

	if d, ok := ctx.Deadline(); ok {
		a.deadlines = append(a.deadlines, time.Until(d))
	} else {
		a.deadlines = append(a.deadlines, 0)
	}

	if r.Password != "test" {
		return nil, auth.ErrInvalid
	}

	return &auth.Result{Account: "test"}, nil
}

func (a *recordingAuth) Lookup(context.Context, string, string, net.Addr) (*auth.Result, error) {
	return nil, auth.ErrInvalid
}

// TestLoginTimeoutAndAttempts: every login of a connection is bounded
// like IMAP's (15 s) and carries the connection's attempt ordinal; a new
// connection starts at 1 again.
func TestLoginTimeoutAndAttempts(t *testing.T) {
	ra := &recordingAuth{}
	srv := New(Options{Store: memstore.New(), Auth: ra, InsecureAuth: true})

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	go func() { _ = srv.Serve(ln) }()

	t.Cleanup(func() { _ = srv.Close() })

	c := dial(t, ln.Addr().String())

	for _, pw := range []string{"bad", "worse", "test"} {
		c.cmd("USER test")
		c.cmd("PASS " + pw)
	}

	c2 := dial(t, ln.Addr().String())
	c2.cmd("USER test")

	if got := c2.cmd("PASS bad"); !strings.HasPrefix(got, "-ERR") {
		t.Fatal(got)
	}

	ra.mu.Lock()
	defer ra.mu.Unlock()

	want := []uint{1, 2, 3, 1}
	for i, w := range want {
		if ra.attempts[i] != w {
			t.Fatalf("ordinals %v, want %v", ra.attempts, want)
		}
	}

	for _, d := range ra.deadlines {
		if d <= 0 || d > authTimeout {
			t.Fatalf("login without the outer timeout: %v", ra.deadlines)
		}
	}
}
