package mcpserver

import (
	"context"
	"sync"
	"time"
)

// gate bounds concurrent operations globally and per principal identity. A service owns its gate, so a second
// transport cannot bypass it; a transport may add its own, stricter gate in front.
type gate struct {
	slots     chan struct{}
	perCaller map[string]int
	mu        sync.Mutex
	wait      time.Duration
	limit     int
}

// newGate creates a gate with global and per-identity bounds and the longest wait for a free global slot.
// Non-positive bounds are raised to one.
func newGate(global, perIdentity int, wait time.Duration) *gate {
	return &gate{slots: make(chan struct{}, max(global, 1)), perCaller: map[string]int{}, limit: max(perIdentity, 1),
		wait: max(wait, 0)}
}

// acquire takes a slot for identity and returns its release function. It reports false, without waiting, when the
// identity already holds its share, and after the wait or cancellation when the global bound stays full.
func (g *gate) acquire(ctx context.Context, identity string) (func(), bool) {
	g.mu.Lock()
	if g.perCaller[identity] >= g.limit {
		g.mu.Unlock()

		return nil, false
	}

	g.perCaller[identity]++
	g.mu.Unlock()

	timer := time.NewTimer(g.wait)
	defer timer.Stop()

	select {
	case g.slots <- struct{}{}:
		var once sync.Once

		return func() { once.Do(func() { g.release(identity) }) }, true
	case <-timer.C:
	case <-ctx.Done():
	}

	g.leave(identity)

	return nil, false
}

// release frees the global slot and the identity share.
func (g *gate) release(identity string) {
	<-g.slots
	g.leave(identity)
}

// leave frees the identity share.
func (g *gate) leave(identity string) {
	g.mu.Lock()
	defer g.mu.Unlock()

	if g.perCaller[identity]--; g.perCaller[identity] <= 0 {
		delete(g.perCaller, identity)
	}
}
