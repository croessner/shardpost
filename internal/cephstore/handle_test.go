//go:build tentacle

package cephstore

import (
	"errors"
	"log/slog"
	"sync"
	"testing"
)

// fakeHandle is a registered handle whose views need no cluster.
func fakeHandle(s *Store, old *User) (*userHandle, *[]*User, *sync.Mutex) {
	var (
		mu     sync.Mutex
		closed []*User
	)

	h := &userHandle{ns: "~admin", u: old, watches: map[*Watch]struct{}{},
		reopen: func() (*User, error) { return &User{name: "~admin"}, nil },
		closeUser: func(u *User) {
			u.inflight.Wait() // what User.Close waits for before it destroys the view

			mu.Lock()
			defer mu.Unlock()

			closed = append(closed, u)
		}}

	s.heldMu.Lock()
	if s.handles == nil {
		s.handles = map[*userHandle]struct{}{}
	}

	s.handles[h] = struct{}{}
	s.heldMu.Unlock()

	h.forget = func() {
		s.heldMu.Lock()
		delete(s.handles, h)
		s.heldMu.Unlock()
	}

	return h, &closed, &mu
}

// TestRotateMovesHandles: a key rotation reopens every long-lived view
// on the new connection, fails the watches of the old one and closes it
// only after the requests that still use it are done; a closed handle is
// no longer moved.
func TestRotateMovesHandles(t *testing.T) {
	s := &Store{log: slog.New(slog.DiscardHandler)}
	old := &User{name: "~admin"}
	h, closed, mu := fakeHandle(s, old)

	w := &Watch{errs: make(chan error, 1)}
	h.watches[w] = struct{}{}

	inUse, done, _ := h.get() // a request that runs across the rotation
	if inUse != old {
		t.Fatal("get did not return the current view")
	}

	rotated := make(chan struct{})

	go func() { s.rotateHandles(); close(rotated) }()

	waitRotated(t, w, rotated)

	mu.Lock()
	early := len(*closed)
	mu.Unlock()

	if early != 0 {
		t.Fatal("the old view was closed while a request used it")
	}

	done()
	<-rotated

	cur, done2, _ := h.get()
	done2()

	if len(*closed) != 1 || (*closed)[0] != old || cur == old || len(h.watches) != 0 {
		t.Fatalf("closed views %v, swapped %v, watches %d", *closed, cur != old, len(h.watches))
	}

	// A second error is coalesced, the channel never blocks the sender.
	w.fail(ErrRotated)
	w.fail(ErrRotated)

	h.close()

	if len(s.handles) != 0 || len(*closed) != 2 || (*closed)[1] != cur {
		t.Fatalf("close: handles %d, closed %d", len(s.handles), len(*closed))
	}

	s.rotateHandles() // nothing registered any more

	if len(*closed) != 2 {
		t.Fatal("a closed handle was rotated")
	}
}

// TestWatchCloseUnwatchesBeforeThePumpEnds: Close removes the watch
// while the pump (which pins the view through inflight) still runs, so
// a closing view cannot destroy its IOContext during rados_unwatch2.
func TestWatchCloseUnwatchesBeforeThePumpEnds(t *testing.T) {
	u := &User{name: "~admin"}
	u.inflight.Add(1) // the pump's reference

	w := &Watch{errs: make(chan error, 1), done: make(chan struct{})}

	go func() { // the pump: ends at done
		<-w.done
		u.inflight.Done()
	}()

	pinned, calls := false, 0
	w.unwatch = func() error {
		calls++

		select {
		case <-w.done:
		default:
			pinned = true
		}

		return nil
	}

	closedHook := false
	w.onClose = func() { closedHook = true }

	w.Close()
	w.Close()
	u.inflight.Wait() // what User.Close waits for before Destroy

	if !pinned || calls != 1 || !closedHook {
		t.Fatalf("unwatch while pinned %v, calls %d, onClose %v", pinned, calls, closedHook)
	}
}

// waitRotated waits for the rotation error of w, which must come before
// the rotation ends.
func waitRotated(t *testing.T, w *Watch, rotated <-chan struct{}) {
	t.Helper()

	select {
	case err := <-w.Errors():
		if !errors.Is(err, ErrRotated) {
			t.Fatalf("watch error %v", err)
		}
	case <-rotated:
		t.Fatal("the rotation did not fail the watch before closing the view")
	}
}

// TestSwapAfterCloseClosesTheNewView: a rotation that races Close does
// not leak the view it opened.
func TestSwapAfterCloseClosesTheNewView(t *testing.T) {
	s := &Store{log: slog.New(slog.DiscardHandler)}
	h, closed, _ := fakeHandle(s, &User{name: "~admin"})

	h.close()

	nu := &User{name: "~admin"}
	h.swap(nu)

	if len(*closed) != 2 || (*closed)[1] != nu {
		t.Fatalf("closed %v", *closed)
	}
}

// TestWatchOnSwappedViewFails: a watch made on a view that was swapped
// out meanwhile reports the rotation at once.
func TestWatchOnSwappedViewFails(t *testing.T) {
	s := &Store{log: slog.New(slog.DiscardHandler)}
	old := &User{name: "~admin"}
	h, _, _ := fakeHandle(s, old)

	h.swap(&User{name: "~admin"})

	w := &Watch{errs: make(chan error, 1)}
	h.track(old, w)

	select {
	case err := <-w.Errors():
		if !errors.Is(err, ErrRotated) {
			t.Fatal(err)
		}
	default:
		t.Fatal("watch on the old view not failed")
	}

	cur, done, _ := h.get()
	done()

	w2 := &Watch{errs: make(chan error, 1)}
	h.track(cur, w2)

	if len(h.watches) != 1 {
		t.Fatal("watch on the current view not tracked")
	}

	w2.onClose()

	if len(h.watches) != 0 {
		t.Fatal("closed watch still tracked")
	}
}
