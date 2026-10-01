package watchhub

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"shardpost/internal/memstore"
	"shardpost/internal/model"
)

// fakeWatch is a watch the test fires and breaks.
type fakeWatch struct {
	events chan struct{}
	errs   chan error
	closed chan struct{}
	once   sync.Once
}

func (w *fakeWatch) Events() <-chan struct{} { return w.events }
func (w *fakeWatch) Errors() <-chan error    { return w.errs }
func (w *fakeWatch) Close()                  { w.once.Do(func() { close(w.closed) }) }

// fakeStore counts the views and watches it hands out; failing makes the
// next watches fail.
type fakeStore struct {
	mu      sync.Mutex
	opened  int
	open    int // views not closed yet
	watches []*fakeWatch
	objects []string
	failing int
}

type fakeUser struct {
	model.User
	s  *fakeStore
	ns string
}

func (s *fakeStore) User(ns string) (model.User, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.opened++
	s.open++

	return &fakeUser{s: s, ns: ns}, nil
}

func (u *fakeUser) Close() {
	u.s.mu.Lock()
	defer u.s.mu.Unlock()

	u.s.open--
}

func (u *fakeUser) Watch(id string) (model.Watch, error) { return u.s.watch(u.ns + "/mbox/" + id) }
func (u *fakeUser) WatchAccount() (model.Watch, error)   { return u.s.watch(u.ns + "/account") }

func (s *fakeStore) watch(object string) (model.Watch, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.failing > 0 {
		s.failing--
		return nil, errors.New("osd says no")
	}

	w := &fakeWatch{events: make(chan struct{}, 1), errs: make(chan error, 1), closed: make(chan struct{})}
	s.watches = append(s.watches, w)
	s.objects = append(s.objects, object)

	return w, nil
}

func (s *fakeStore) last() *fakeWatch {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.watches[len(s.watches)-1]
}

func (s *fakeStore) counts() (watches, openViews int) {
	s.mu.Lock()
	defer s.mu.Unlock()

	return len(s.watches), s.open
}

func newHub(s Opener) *Hub {
	h := New(s, slog.New(slog.NewTextHandler(io.Discard, nil)))
	h.minRetry, h.maxRetry = time.Millisecond, 5*time.Millisecond

	return h
}

// event waits for an event of w.
func event(t *testing.T, w model.Watch, what string) {
	t.Helper()

	select {
	case <-w.Events():
	case <-time.After(2 * time.Second):
		t.Fatalf("%s: no event", what)
	}
}

// waitUntil polls cond.
func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()

	deadline := time.Now().Add(2 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("%s: timed out", what)
		}

		time.Sleep(time.Millisecond)
	}
}

// TestManySessionsOneWatch: every follower of an object shares one
// watch, and each of them gets its events.
func TestManySessionsOneWatch(t *testing.T) {
	s := &fakeStore{}
	h := newHub(s)

	t.Cleanup(h.Close)

	subs := make([]model.Watch, 50)

	for i := range subs {
		w, err := h.Mailbox("alice", "m1")
		if err != nil {
			t.Fatal(err)
		}

		subs[i] = w
	}

	other, err := h.Mailbox("bob", "m1")
	if err != nil {
		t.Fatal(err)
	}

	if n, _ := s.counts(); n != 2 || h.Size() != 2 {
		t.Fatalf("%d watches for two objects, hub size %d", n, h.Size())
	}

	s.watches[0].events <- struct{}{}

	for i, w := range subs {
		event(t, w, fmt.Sprintf("session %d", i))
	}

	select {
	case <-other.Events():
		t.Fatal("the event of another object woke bob")
	case <-time.After(20 * time.Millisecond):
	}
}

// TestUnsubscribe: the watch goes with its last follower; the next one
// sets it up again.
func TestUnsubscribe(t *testing.T) {
	s := &fakeStore{}
	h := newHub(s)

	t.Cleanup(h.Close)

	a, _ := h.Account("alice")
	b, _ := h.Account("alice")
	first := s.last()

	a.Close()
	a.Close() // twice is harmless

	select {
	case <-first.closed:
		t.Fatal("watch closed while a follower is left")
	case <-time.After(20 * time.Millisecond):
	}

	b.Close()

	<-first.closed
	waitUntil(t, "view closed", func() bool { _, open := s.counts(); return open == 0 })

	if h.Size() != 0 {
		t.Fatalf("hub size %d", h.Size())
	}

	c, err := h.Account("alice")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	if n, _ := s.counts(); n != 2 || s.objects[1] != "alice/account" {
		t.Fatalf("watches %d %v", n, s.objects)
	}
}

// TestRewatchAfterError: a broken watch (the OSD dropped it, or the store
// rotated its connection) is set up again on a fresh view, retried while
// that fails; the followers are woken for what they may have missed and
// get the events of the new watch.
func TestRewatchAfterError(t *testing.T) {
	s := &fakeStore{}
	h := newHub(s)

	t.Cleanup(h.Close)

	w, err := h.Mailbox("alice", "m1")
	if err != nil {
		t.Fatal(err)
	}

	first := s.last()

	s.mu.Lock()
	s.failing = 3
	s.mu.Unlock()

	first.errs <- errors.New("the store rotated its connection, watch again")

	event(t, w, "after the error")
	<-first.closed
	waitUntil(t, "watched again", func() bool { n, _ := s.counts(); return n == 2 })

	second := s.last()
	if second == first || w.Errors() != nil {
		t.Fatal("no new watch, or an error reached the follower")
	}

	// Drain the wake-ups of the re-watch, then a change arrives through
	// the new watch.
	waitUntil(t, "drained", func() bool {
		select {
		case <-w.Events():
			return false
		default:
			return true
		}
	})

	second.events <- struct{}{}

	event(t, w, "through the new watch")

	if _, open := s.counts(); open != 1 {
		t.Fatalf("%d views open", open)
	}

	w.Close()
}

// TestFirstWatchFails: a watch that cannot be set up is reported to the
// subscriber (it polls); the next subscription tries again.
func TestFirstWatchFails(t *testing.T) {
	s := &fakeStore{failing: 1}
	h := newHub(s)

	t.Cleanup(h.Close)

	if _, err := h.Mailbox("alice", "m1"); err == nil {
		t.Fatal("failed watch reported as armed")
	}

	if h.Size() != 0 {
		t.Fatal("failed entry kept")
	}

	w, err := h.Mailbox("alice", "m1")
	if err != nil {
		t.Fatal(err)
	}

	w.Close()

	h.Close()

	if _, err := h.Mailbox("alice", "m1"); !errors.Is(err, ErrClosed) {
		t.Fatalf("closed hub: %v", err)
	}

	if _, open := s.counts(); open != 0 {
		t.Fatalf("%d views open after Close", open)
	}
}

// TestHubOverMemstore: the hub's own view sees the changes made through
// another view of the same account.
func TestHubOverMemstore(t *testing.T) {
	store := memstore.New()
	h := newHub(store)

	t.Cleanup(h.Close)

	u, _ := store.User("alice")
	if err := u.EnsureMailbox("INBOX", nil); err != nil {
		t.Fatal(err)
	}

	mb, _ := u.Lookup("INBOX")

	box, err := h.Mailbox("alice", mb.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer box.Close()

	acct, err := h.Account("alice")
	if err != nil {
		t.Fatal(err)
	}
	defer acct.Close()

	if _, err := u.Deliver("INBOX", []byte("Subject: x\r\n\r\nbody\r\n"), nil, time.Now()); err != nil {
		t.Fatal(err)
	}

	event(t, box, "mailbox")
	event(t, acct, "account")
}
