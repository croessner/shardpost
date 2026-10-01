// Package watchhub shares the RADOS watches of a process between its
// sessions (scaling plan B12). Without it every IMAP session in IDLE held
// a watch on its mailbox object and every NOTIFY session one per notified
// mailbox, so the watches of a pod grew with its sessions. The hub holds
// one watch per object however many sessions follow it and fans its
// events out to them.
//
// A hub watch has a store view of its own (the sessions come and go
// independently of it) and keeps itself up: when the watch reports an
// error — the OSD dropped it, or the store rotated its connection
// (cephstore.ErrRotated) — the hub closes the view, wakes every follower
// (they look for what the broken watch may have missed) and watches again
// on a fresh view, retrying with a growing delay while that fails. A
// follower therefore never sees an error of its own; it only stops
// getting events while there is no watch, and its own polling covers
// that time.
package watchhub

import (
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"shardpost/internal/metrics"
	"shardpost/internal/model"
)

// Opener opens a store view of a namespace.
type Opener interface {
	User(name string) (model.User, error)
}

// Objects a hub watches (label "object" of the watch metrics).
const (
	objMailbox = "mailbox"
	objAccount = "account"
)

// Re-watch delays after an error: the first attempt is immediate, each
// failure doubles the wait up to maxRetry.
const (
	minRetry = 100 * time.Millisecond
	maxRetry = 30 * time.Second
)

// ErrClosed refuses a subscription to a closed hub.
var ErrClosed = errors.New("watch hub closed")

// key names a watched object: the mailbox object of a mailbox, or the
// account object (mailbox "") of a namespace.
type key struct {
	ns, mailbox string
}

func (k key) object() string {
	if k.mailbox == "" {
		return objAccount
	}

	return objMailbox
}

// Hub holds the shared watches of a process.
type Hub struct {
	open Opener
	log  *slog.Logger
	// minRetry and maxRetry bound the re-watch delays (tests shorten
	// them).
	minRetry, maxRetry time.Duration

	mu      sync.Mutex
	entries map[key]*entry
	closed  bool
	wg      sync.WaitGroup
}

// New makes a hub over a store.
func New(open Opener, log *slog.Logger) *Hub {
	return &Hub{open: open, log: log, minRetry: minRetry, maxRetry: maxRetry, entries: map[key]*entry{}}
}

// Mailbox follows the mailbox object of a mailbox in namespace ns: every
// change of the mailbox's messages wakes it.
func (h *Hub) Mailbox(ns, id string) (model.Watch, error) {
	return h.subscribe(key{ns: ns, mailbox: id})
}

// Account follows the account object of namespace ns: every change the
// account change log records wakes it.
func (h *Hub) Account(ns string) (model.Watch, error) {
	return h.subscribe(key{ns: ns})
}

// Size is the number of objects the hub watches.
func (h *Hub) Size() int {
	h.mu.Lock()
	defer h.mu.Unlock()

	return len(h.entries)
}

// Close stops every watch and waits for them.
func (h *Hub) Close() {
	h.mu.Lock()

	h.closed = true

	for k, e := range h.entries {
		delete(h.entries, k)
		e.stopOnce()
	}

	h.mu.Unlock()

	h.wg.Wait()
}

// subscribe joins the watch of k, setting it up when nobody follows it
// yet. It returns once the watch is armed, so a change after the return
// wakes the follower; when the first attempt to arm it fails, the error
// is returned (the caller polls instead).
func (h *Hub) subscribe(k key) (model.Watch, error) {
	h.mu.Lock()

	if h.closed {
		h.mu.Unlock()
		return nil, ErrClosed
	}

	e := h.entries[k]
	if e == nil {
		e = &entry{h: h, k: k, subs: map[*Sub]struct{}{}, ready: make(chan struct{}), stop: make(chan struct{})}
		h.entries[k] = e
		h.wg.Add(1)

		go e.run()
	}

	s := &Sub{e: e, events: make(chan struct{}, 1)}
	e.subs[s] = struct{}{}

	h.mu.Unlock()

	metrics.IMAPWatchSubscribers.WithLabelValues(k.object()).Inc()

	<-e.ready

	if e.err != nil {
		s.Close()
		return nil, e.err
	}

	return s, nil
}

// entry is the watch on one object and its followers (subs, guarded by
// the hub's mutex).
type entry struct {
	h    *Hub
	k    key
	subs map[*Sub]struct{}
	// ready is closed after the first attempt to arm the watch; err is
	// its error.
	ready chan struct{}
	err   error
	stop  chan struct{}
	once  sync.Once
}

func (e *entry) stopOnce() { e.once.Do(func() { close(e.stop) }) }

// run keeps the watch up until the last follower leaves.
func (e *entry) run() {
	defer e.h.wg.Done()

	u, w, err := e.arm()

	e.err = err
	close(e.ready)

	if err != nil {
		e.h.log.Warn("watch hub: watch failed, the sessions poll", "object", e.k.object(), "err", err)
		e.h.remove(e)

		return
	}

	for {
		broken := e.follow(w)

		w.Close()
		u.Close()
		metrics.IMAPWatches.WithLabelValues(e.k.object()).Dec()

		if !broken {
			return
		}

		metrics.IMAPWatchRearms.Inc()
		e.fire() // what the broken watch may have missed

		if u, w = e.rearm(); w == nil {
			return
		}
	}
}

// rearm watches again until it succeeds or the entry is stopped (nil).
func (e *entry) rearm() (model.User, model.Watch) {
	delay := time.Duration(0)

	for {
		select {
		case <-e.stop:
			return nil, nil
		case <-time.After(delay):
		}

		u, w, err := e.arm()
		if err == nil {
			// A change between the error and the new watch wakes nobody
			// otherwise.
			e.fire()

			return u, w
		}

		e.h.log.Warn("watch hub: watching again failed", "object", e.k.object(), "err", err)

		delay = min(max(2*delay, e.h.minRetry), e.h.maxRetry)
	}
}

// arm opens a view of the namespace and watches the object.
func (e *entry) arm() (model.User, model.Watch, error) {
	u, err := e.h.open.User(e.k.ns)
	if err != nil {
		return nil, nil, err
	}

	var w model.Watch

	if e.k.mailbox != "" {
		w, err = u.Watch(e.k.mailbox)
	} else if aw, ok := u.(model.AccountWatcher); ok {
		w, err = aw.WatchAccount()
	} else {
		err = errors.New("the store has no account watch")
	}

	if err != nil {
		u.Close()
		return nil, nil, fmt.Errorf("watch: %w", err)
	}

	metrics.IMAPWatches.WithLabelValues(e.k.object()).Inc()

	return u, w, nil
}

// follow fans the events of w out until it breaks (true) or the entry is
// stopped (false).
func (e *entry) follow(w model.Watch) bool {
	for {
		select {
		case <-e.stop:
			return false
		case <-w.Events():
			e.fire()
		case err := <-w.Errors():
			e.h.log.Info("watch hub: watch lost, watching again", "object", e.k.object(), "err", err)
			return true
		}
	}
}

// fire wakes every follower, coalescing with an event not yet taken.
func (e *entry) fire() {
	e.h.mu.Lock()
	defer e.h.mu.Unlock()

	for s := range e.subs {
		select {
		case s.events <- struct{}{}:
		default:
		}
	}
}

// remove drops an entry whose first watch failed, so that the next
// subscription tries again.
func (h *Hub) remove(e *entry) {
	h.mu.Lock()
	defer h.mu.Unlock()

	if h.entries[e.k] == e {
		delete(h.entries, e.k)
	}
}

// leave drops a follower, and the watch with the last one.
func (h *Hub) leave(s *Sub) {
	h.mu.Lock()
	defer h.mu.Unlock()

	e := s.e

	if _, ok := e.subs[s]; !ok {
		return
	}

	delete(e.subs, s)
	metrics.IMAPWatchSubscribers.WithLabelValues(e.k.object()).Dec()

	if len(e.subs) > 0 {
		return
	}

	if h.entries[e.k] == e {
		delete(h.entries, e.k)
	}

	e.stopOnce()
}

// Sub is one follower of a hub watch; it implements model.Watch.
type Sub struct {
	e      *entry
	events chan struct{}
}

// Events fires when the object may have changed (coalesced).
func (s *Sub) Events() <-chan struct{} { return s.events }

// Errors never fires: the hub sets up a broken watch again itself.
func (s *Sub) Errors() <-chan error { return nil }

// Close leaves the watch.
func (s *Sub) Close() { s.e.h.leave(s) }
