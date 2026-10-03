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

// ErrHandleClosed is reported by a long-lived view used after it was
// closed: an admin store after its Close, a queue of the store after
// Store.Drain.
var ErrHandleClosed = errors.New("the store view is closed")

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
	// owned marks a handle of the store's own (the queues it opens once
	// per process), which Store.Drain closes; the others belong to
	// whoever opened them (AdminKV.Close).
	owned bool
}

// openHandle opens a handle on namespace ns and registers it for key
// rotations; owned is set for the store's own handles (see
// userHandle.owned).
func (s *Store) openHandle(ns string, owned bool) (*userHandle, error) {
	u, err := s.User(ns)
	if err != nil {
		return nil, err
	}

	h := &userHandle{ns: ns, u: u, watches: map[*Watch]struct{}{}, owned: owned,
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
// is done; until then neither a rotation nor close closes the view.
// ErrHandleClosed once the handle is closed.
func (h *userHandle) get() (*User, func(), error) {
	h.mu.RLock()
	defer h.mu.RUnlock()

	if h.closed {
		return nil, nil, ErrHandleClosed
	}

	u := h.u
	u.inflight.Add(1)

	return u, u.inflight.Done, nil
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
	return h.watchData(oid, nil)
}

// watchPayload is watch with the data of every notify.
func (h *userHandle) watchPayload(oid string) (model.PayloadWatch, error) {
	data := sortedindex.NewPayloadPump()

	w, err := h.watchData(oid, data)
	if err != nil {
		data.Close()

		return nil, err
	}

	return payloadWatch{Watch: w}, nil
}

// watchData is watch with an optional pump for the data of the notifies.
func (h *userHandle) watchData(oid string, data *sortedindex.PayloadPump) (*Watch, error) {
	u, done, err := h.get()
	if err != nil {
		return nil, err
	}
	defer done()

	op := rados.CreateWriteOp()
	defer op.Release()

	op.Create(rados.CreateIdempotent)

	if err := op.Operate(u.idx, oid, rados.OperationNoFlag); err != nil {
		return nil, fmt.Errorf("create %s: %w", oid, err)
	}

	w, err := u.watchObjData(oid, data)
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

// notify wakes the watchers of oid in the background (best effort),
// with data for the payload watchers (nil: none).
func (h *userHandle) notify(oid string, data []byte) {
	u, done, err := h.get()
	if err != nil {
		return
	}

	go func() {
		defer done()

		if _, _, err := u.idx.NotifyWithTimeout(oid, data, notifyTimeout); err != nil && errCode(err) != syscall.ENOENT {
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

// closeOwnedHandles closes the store's own handles (Store.Drain); their
// views close once the requests that use them are done.
func (s *Store) closeOwnedHandles() {
	s.heldMu.Lock()
	hs := make([]*userHandle, 0, len(s.handles))

	for h := range s.handles {
		if h.owned {
			hs = append(hs, h)
		}
	}
	s.heldMu.Unlock()

	for _, h := range hs {
		h.close()
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

// on returns the omap backend of the current view and the function to
// call when the request is done.
func (b *handleOmap) on() (*radosOmap, func(), error) {
	u, done, err := b.h.get()
	if err != nil {
		return nil, nil, err
	}

	return &radosOmap{ioctx: u.idx, key: b.key, keysVer: func(oid string, keys []string) (map[string][]byte, uint64, error) {
		return u.getKeysVer(u.idx, oid, keys)
	}}, done, nil
}

// CacheKey implements sortedindex.Backend.
func (b *handleOmap) CacheKey() string { return b.key }

// ReadKeys implements sortedindex.Backend.
func (b *handleOmap) ReadKeys(oid string, keys []string) (map[string][]byte, uint64, error) {
	r, done, err := b.on()
	if err != nil {
		return nil, 0, err
	}
	defer done()

	return r.ReadKeys(oid, keys)
}

// ReadPage implements sortedindex.Backend.
func (b *handleOmap) ReadPage(oid, startAfter string, limit int) ([]sortedindex.KV, bool, error) {
	r, done, err := b.on()
	if err != nil {
		return nil, false, err
	}
	defer done()

	return r.ReadPage(oid, startAfter, limit)
}

// Write implements sortedindex.Backend.
func (b *handleOmap) Write(oid string, ver uint64, set map[string][]byte, rm []string) error {
	r, done, err := b.on()
	if err != nil {
		return err
	}
	defer done()

	return r.Write(oid, ver, set, rm)
}

// Watch implements sortedindex.Waker.
func (b *handleOmap) Watch(oid string) (model.Watch, error) { return b.h.watch(oid) }

// Notify implements sortedindex.Waker.
func (b *handleOmap) Notify(oid string) { b.h.notify(oid, nil) }

// NotifyPayload implements sortedindex.Waker.
func (b *handleOmap) NotifyPayload(oid string, data []byte) { b.h.notify(oid, data) }

// WatchPayload implements sortedindex.Waker.
func (b *handleOmap) WatchPayload(oid string) (model.PayloadWatch, error) {
	return b.h.watchPayload(oid)
}
