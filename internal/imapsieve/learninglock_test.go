package imapsieve

import (
	"context"
	"errors"
	"testing"
	"time"

	"shardpost/internal/config"
	"shardpost/internal/model"
)

// busyQueue is a queue whose worker lease another server holds until
// free is set; it counts the attempts to take it.
type busyQueue struct {
	testQueue
	locks int
	free  bool
	lost  bool
}

var errBusy = errors.New("held by another server")

type lostLease struct{ q *busyQueue }

func (l lostLease) Lost() bool { return l.q.lost }
func (lostLease) Release()     {}

func (q *busyQueue) Lock() (model.Lease, error) {
	q.locks++

	if !q.free {
		return nil, errBusy
	}

	return lostLease{q}, nil
}

// TestLearningLockBackoff: a worker without the lease tries to take it
// every lock_retry_seconds with jitter, not on every pass of its
// one-second loop; one that holds it keeps it; one that lost it tries
// again at once.
func TestLearningLockBackoff(t *testing.T) {
	q := &busyQueue{testQueue: testQueue{map[string]Job{}, map[string][]byte{}}}
	now := time.Unix(1_000_000, 0)
	jitter := 1.0

	w := NewLearningWorker(config.Learning{MaxMessageBytes: 100, MaxAttempts: 3, LockRetrySeconds: 20}, q, &fakePrograms{})
	w.Now = func() time.Time { return now }
	w.Jitter = func() float64 { return jitter }

	// 60 passes of the one-second loop: attempts at 0, 20 and 40 s.
	for range 60 {
		w.Step(context.Background())

		now = now.Add(time.Second)
	}

	if q.locks != 3 {
		t.Fatalf("a worker without the lease tried %d times in 60 s, want 3 (every 20 s)", q.locks)
	}

	// The jitter stretches and shrinks the interval (±50 %).
	jitter = 0.5
	q.locks = 0

	for range 60 {
		w.Step(context.Background())

		now = now.Add(time.Second)
	}

	if q.locks < 3 || q.locks > 6 {
		t.Fatalf("%d attempts in 60 s with a 10 s interval", q.locks)
	}

	// Free: the next attempt takes it, and the holder keeps it without
	// locking again.
	q.free, q.locks = true, 0

	for range 60 {
		w.Step(context.Background())

		now = now.Add(time.Second)
	}

	if q.locks != 1 || w.lease == nil {
		t.Fatalf("lease taken with %d attempts, held %v", q.locks, w.lease != nil)
	}

	// Lost (a key rotation): taken again at the next pass.
	q.lost, q.locks = true, 0

	w.Step(context.Background())

	if q.locks != 1 {
		t.Fatalf("a lost lease was not taken again at once: %d attempts", q.locks)
	}
}

func TestLearningLockRetryDefault(t *testing.T) {
	w := NewLearningWorker(config.Learning{}, nil, nil)
	w.Jitter = func() float64 { return 1 }

	if d := w.lockRetry(); d != DefaultLockRetry {
		t.Fatalf("default retry %s", d)
	}

	w.Jitter = nil

	for range 100 {
		if d := w.lockRetry(); d < DefaultLockRetry/2 || d >= DefaultLockRetry*3/2 {
			t.Fatalf("retry %s outside ±50 %%", d)
		}
	}
}
