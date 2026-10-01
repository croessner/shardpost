package jmap

import (
	"container/list"
	"context"
	"sync"
	"time"

	"shardpost/internal/metrics"
)

// limiter bounds what one key (an account, a client address) has in
// flight. A caller that finds the key at its limit may wait for a slot
// in a FIFO queue of its own: a freed slot goes to the oldest waiter,
// never to whoever asks next, and the waiting happens on the waiter's
// own channel, not under a lock. The queue per key is bounded; past it
// the caller is turned away at once.
type limiter struct {
	// name labels the metrics.
	name  string
	limit int
	// maxWaiters bounds the queue of one key.
	maxWaiters int

	mu   sync.Mutex
	keys map[string]*slots
}

// slots is the state of one key.
type slots struct {
	inUse   int
	waiters *list.List // of chan struct{}, oldest in front
}

func newLimiter(name string, limit int) *limiter {
	limit = max(limit, 1)

	return &limiter{name: name, limit: limit, maxWaiters: 2 * limit, keys: map[string]*slots{}}
}

// tryAcquire takes a slot without waiting; ok false when the key is at
// its limit.
func (l *limiter) tryAcquire(key string) (release func(), ok bool) {
	return l.acquire(context.Background(), key, 0)
}

// acquire takes a slot of key, waiting up to wait (and while ctx lasts)
// when the key is at its limit. It reports false when no slot came.
func (l *limiter) acquire(ctx context.Context, key string, wait time.Duration) (release func(), ok bool) {
	l.mu.Lock()

	s := l.keys[key]
	if s == nil {
		s = &slots{waiters: list.New()}
		l.keys[key] = s
	}

	if s.inUse < l.limit && s.waiters.Len() == 0 {
		s.inUse++
		l.mu.Unlock()

		return l.releaser(key), true
	}

	if wait <= 0 || s.waiters.Len() >= l.maxWaiters {
		l.mu.Unlock()
		metrics.JMAPLimit.WithLabelValues(l.name, "rejected").Inc()

		return nil, false
	}

	ticket := make(chan struct{}, 1)
	el := s.waiters.PushBack(ticket)
	l.mu.Unlock()

	timer := time.NewTimer(wait)
	defer timer.Stop()

	select {
	case <-ticket:
		metrics.JMAPLimit.WithLabelValues(l.name, "waited").Inc()

		return l.releaser(key), true
	case <-timer.C:
	case <-ctx.Done():
	}

	l.mu.Lock()

	select {
	case <-ticket:
		// The slot came while the wait ended: it is ours, so pass it on.
		l.mu.Unlock()
		l.release(key)
	default:
		s.waiters.Remove(el)
		l.drop(key, s)
		l.mu.Unlock()
	}

	metrics.JMAPLimit.WithLabelValues(l.name, "rejected").Inc()

	return nil, false
}

// releaser is the release function of one slot (safe to call twice).
func (l *limiter) releaser(key string) func() {
	var once sync.Once

	return func() { once.Do(func() { l.release(key) }) }
}

// release gives a slot back: to the oldest waiter if there is one.
func (l *limiter) release(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()

	s := l.keys[key]
	if s == nil {
		return
	}

	if front := s.waiters.Front(); front != nil {
		ticket := s.waiters.Remove(front).(chan struct{})
		ticket <- struct{}{} // buffered: never blocks

		return
	}

	s.inUse--
	l.drop(key, s)
}

// drop forgets a key that holds nothing; l.mu is held.
func (l *limiter) drop(key string, s *slots) {
	if s.inUse <= 0 && s.waiters.Len() == 0 {
		delete(l.keys, key)
	}
}

// inFlight is how many slots key holds (tests).
func (l *limiter) inFlight(key string) int {
	l.mu.Lock()
	defer l.mu.Unlock()

	if s := l.keys[key]; s != nil {
		return s.inUse
	}

	return 0
}
