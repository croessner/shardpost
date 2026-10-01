package cephstore

import (
	"errors"
	"fmt"
	"sync"
	"syscall"

	"github.com/ceph/go-ceph/rados"

	"shardpost/internal/model"
	"shardpost/internal/sortedindex"
)

// Long-lived handles and key rotation
//
// A User keeps the connection it was opened on until it is closed, so a
// view that lives as long as the process (the admin store of the job
// runner, of JMAP and of the push worker) would keep working on the
// connection of the old cephx key after Store.Rotate, until that key is
// withdrawn. A userHandle follows the rotation instead: the store
// reopens the namespace on the new connection and swaps it in; requests
// that already run finish on the old view, which is closed once they
// are done; watches on the old view get an error, so their holders
// watch again on the new one.

// ErrRotated is reported on the watches of a handle when the store
// rotated its connection.
var ErrRotated = errors.New("the store rotated its connection, watch again")

// userHandle is a long-lived view on one namespace that follows
// Store.Rotate.
type userHandle struct {
	ns string

	mu      sync.RWMutex
	u       *User
	closed  bool
	watches map[*Watch]struct{}

	// reopen opens the namespace on the current connection; closeUser
	// closes a replaced view (it waits for its requests and watches
	// itself). Tests replace both.
	reopen    func() (*User, error)
	closeUser func(*User)
	// forget unregisters the handle from its store.
	forget func()
}

// openHandle opens a handle on namespace ns and registers it for key
// rotations.
func (s *Store) openHandle(ns string) (*userHandle, error) {
	u, err := s.User(ns)
	if err != nil {
		return nil, err
	}

	h := &userHandle{ns: ns, u: u, watches: map[*Watch]struct{}{},
		reopen: func() (*User, error) { return s.User(ns) }, closeUser: (*User).Close}

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

	return h, nil
}

// get returns the current view and a function to call when the request
// is done; until then a rotation does not close the view.
func (h *userHandle) get() (*User, func()) {
	h.mu.RLock()
	defer h.mu.RUnlock()

	u := h.u
	u.inflight.Add(1)

	return u, u.inflight.Done
}

// swap replaces the view by nu, fails the watches on the old one and
// closes it once its requests are done.
func (h *userHandle) swap(nu *User) {
	h.mu.Lock()

	if h.closed {
		h.mu.Unlock()
		h.closeUser(nu)

		return
	}

	old, watches := h.u, h.watches
	h.u, h.watches = nu, map[*Watch]struct{}{}
	h.mu.Unlock()

	for w := range watches {
		w.fail(ErrRotated)
	}

	h.closeUser(old)
}

// close closes the handle and its view.
func (h *userHandle) close() {
	h.mu.Lock()

	if h.closed {
		h.mu.Unlock()
		return
	}

	h.closed = true
	u := h.u
	h.mu.Unlock()

	if h.forget != nil {
		h.forget()
	}

	h.closeUser(u)
}

// watch creates oid if needed (a watch needs the object) and watches it
// on the current view; a rotation fails the watch.
func (h *userHandle) watch(oid string) (model.Watch, error) {
	u, done := h.get()
	defer done()

	op := rados.CreateWriteOp()
	defer op.Release()

	op.Create(rados.CreateIdempotent)

	if err := op.Operate(u.idx, oid, rados.OperationNoFlag); err != nil {
		return nil, fmt.Errorf("create %s: %w", oid, err)
	}

	w, err := u.watchObj(oid)
	if err != nil {
		return nil, err
	}

	h.track(u, w)

	return w, nil
}

// track registers a watch made on view u; a watch on a view that was
// swapped out meanwhile fails at once.
func (h *userHandle) track(u *User, w *Watch) {
	h.mu.Lock()
	defer h.mu.Unlock()

	if h.u != u {
		w.fail(ErrRotated)
		return
	}

	h.watches[w] = struct{}{}
	w.onClose = func() {
		h.mu.Lock()
		delete(h.watches, w)
		h.mu.Unlock()
	}
}

// notify wakes the watchers of oid in the background (best effort).
func (h *userHandle) notify(oid string) {
	u, done := h.get()

	go func() {
		defer done()

		if _, _, err := u.idx.NotifyWithTimeout(oid, nil, notifyTimeout); err != nil && errCode(err) != syscall.ENOENT {
			u.log.Debug("queue notify failed", "object", oid, "err", err)
		}
	}()
}

// rotateHandles reopens every registered handle on the current
// connection (Store.Rotate). A handle whose reopen fails keeps its view
// and is retried at the next rotation.
func (s *Store) rotateHandles() {
	s.heldMu.Lock()
	hs := make([]*userHandle, 0, len(s.handles))

	for h := range s.handles {
		hs = append(hs, h)
	}
	s.heldMu.Unlock()

	for _, h := range hs {
		nu, err := h.reopen()
		if err != nil {
			s.log.Error("long-lived store view not moved to the new connection", "namespace", h.ns, "err", err)
			continue
		}

		h.swap(nu)
	}

	if len(hs) > 0 {
		s.log.Info("long-lived store views moved to the new connection", "views", len(hs))
	}
}

// handleOmap is the sortedindex.Backend of a handle's namespace: every
// call runs on the current view.
type handleOmap struct {
	h   *userHandle
	key string
}

var (
	_ sortedindex.Backend = (*handleOmap)(nil)
	_ sortedindex.Waker   = (*handleOmap)(nil)
)

// on runs fn with the omap backend of the current view.
func (b *handleOmap) on() (*radosOmap, func()) {
	u, done := b.h.get()

	return &radosOmap{ioctx: u.idx, key: b.key, keysVer: func(oid string, keys []string) (map[string][]byte, uint64, error) {
		return u.getKeysVer(u.idx, oid, keys)
	}}, done
}

// CacheKey implements sortedindex.Backend.
func (b *handleOmap) CacheKey() string { return b.key }

// ReadKeys implements sortedindex.Backend.
func (b *handleOmap) ReadKeys(oid string, keys []string) (map[string][]byte, uint64, error) {
	r, done := b.on()
	defer done()

	return r.ReadKeys(oid, keys)
}

// ReadPage implements sortedindex.Backend.
func (b *handleOmap) ReadPage(oid, startAfter string, limit int) ([]sortedindex.KV, bool, error) {
	r, done := b.on()
	defer done()

	return r.ReadPage(oid, startAfter, limit)
}

// Write implements sortedindex.Backend.
func (b *handleOmap) Write(oid string, ver uint64, set map[string][]byte, rm []string) error {
	r, done := b.on()
	defer done()

	return r.Write(oid, ver, set, rm)
}

// Watch implements sortedindex.Waker.
func (b *handleOmap) Watch(oid string) (model.Watch, error) { return b.h.watch(oid) }

// Notify implements sortedindex.Waker.
func (b *handleOmap) Notify(oid string) { b.h.notify(oid) }
