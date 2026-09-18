// Package membudget is a process-wide budget for message-sized buffers.
// Messages are handled as whole byte slices (delivery, APPEND literals,
// FETCH body sections). With a 256 MiB message limit a handful of
// concurrent operations could exceed the pod's memory limit, so all such
// buffers are accounted here; operations beyond the budget wait, and a
// single buffer larger than the whole budget is refused. The package is
// cgo-free so the protocol layers can use it directly.
package membudget

import (
	"errors"
	"sync"
	"time"
)

// ErrTooLarge is returned when one buffer alone exceeds the budget.
var ErrTooLarge = errors.New("buffer exceeds the memory budget")

// ErrBusy is returned when the budget stayed exhausted for WaitTimeout.
// Waiting is bounded because callers may already hold part of the budget
// (all literals of one command, the steps of one LMTP message): two such
// callers waiting for each other would otherwise never make progress.
var ErrBusy = errors.New("memory budget exhausted")

// WaitTimeout bounds how long Acquire waits for the budget.
var WaitTimeout = 10 * time.Second

type budget struct {
	mu    sync.Mutex
	cond  *sync.Cond
	limit int64
	used  int64
}

var global = newBudget(256 << 20)

func newBudget(limit int64) *budget {
	b := &budget{limit: limit}
	b.cond = sync.NewCond(&b.mu)

	return b
}

// SetLimit sets the process-wide budget in bytes (0 = unlimited).
func SetLimit(limit int64) {
	global.mu.Lock()
	global.limit = limit
	global.mu.Unlock()
	global.cond.Broadcast()
}

// Limit returns the process-wide budget in bytes (0 = unlimited).
func Limit() int64 {
	global.mu.Lock()
	defer global.mu.Unlock()

	return global.limit
}

// Acquire reserves n bytes, waiting at most WaitTimeout while the budget
// is exhausted (ErrBusy afterwards). The returned function releases the
// reservation (idempotent).
func Acquire(n int64) (func(), error) {
	return global.acquire(n, WaitTimeout)
}

// TryAcquire reserves n bytes without blocking; ok is false when the
// budget is currently exhausted.
func TryAcquire(n int64) (release func(), ok bool, err error) {
	global.mu.Lock()
	defer global.mu.Unlock()

	if global.limit > 0 && n > global.limit {
		return nil, false, ErrTooLarge
	}

	if global.limit > 0 && global.used+n > global.limit {
		return nil, false, nil
	}

	global.used += n

	return global.releaser(n), true, nil
}

func (b *budget) acquire(n int64, wait time.Duration) (func(), error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.limit > 0 && n > b.limit {
		return nil, ErrTooLarge
	}

	if b.limit > 0 && b.used+n > b.limit {
		// sync.Cond has no timed wait; a timer wakes the waiters up so
		// that the deadline is noticed. The broadcast is sent while holding
		// b.mu: a waiter that has checked the deadline but not yet entered
		// Wait holds the lock, so the wake-up cannot slip in between and
		// get lost; it is delivered once the waiter is parked in Wait.
		deadline := time.Now().Add(wait)

		timer := time.AfterFunc(wait, func() {
			b.mu.Lock()
			defer b.mu.Unlock()

			b.cond.Broadcast()
		})
		defer timer.Stop()

		for b.limit > 0 && b.used+n > b.limit {
			if !time.Now().Before(deadline) {
				return nil, ErrBusy
			}

			b.cond.Wait()
		}
	}

	b.used += n

	return b.releaser(n), nil
}

func (b *budget) releaser(n int64) func() {
	var once sync.Once

	return func() {
		once.Do(func() {
			b.mu.Lock()
			b.used -= n
			b.mu.Unlock()
			b.cond.Broadcast()
		})
	}
}

// Used returns the current reservation (for metrics/tests).
func Used() int64 {
	global.mu.Lock()
	defer global.mu.Unlock()

	return global.used
}
