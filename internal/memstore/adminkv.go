package memstore

import (
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"shardpost/internal/kvindex"
	"shardpost/internal/model"
	"shardpost/internal/sortedindex"
)

// AdminKV is an in-memory model.IndexedKV for tests: records in a map,
// hint indexes and queues over an in-memory omap backend, in-process
// watch/notify and worker locks.
type AdminKV struct {
	*kvindex.Store
	recs  *records
	mem   *sortedindex.MemBackend
	wake  *memWaker
	locks *memLocks
}

var _ model.IndexedKV = (*AdminKV)(nil)

// NewAdminKV returns an empty store in the default mode (write).
func NewAdminKV() *AdminKV { return NewAdminKVMode(model.KVIndexWrite) }

// NewAdminKVMode returns an empty store with the given index mode.
func NewAdminKVMode(mode model.KVIndexMode) *AdminKV {
	kv := &AdminKV{recs: &records{m: map[string][]byte{}}, mem: sortedindex.NewMemBackend(),
		wake: &memWaker{subs: map[string][]*memSub{}}, locks: newMemLocks()}

	return kv.WithMode(mode)
}

// WithMode returns a view on the same records, indexes, queues and locks
// in another index mode (a server of another configuration, tests).
// Read mode holds without the backfill marker (see WithGatedMode).
func (kv *AdminKV) WithMode(mode model.KVIndexMode) *AdminKV {
	return kv.view(kvindex.Options{Mode: mode})
}

// WithGatedMode is WithMode with read mode gated on the backfill marker,
// as the RADOS store does it (tests of the fallback).
func (kv *AdminKV) WithGatedMode(mode model.KVIndexMode, now func() time.Time) *AdminKV {
	return kv.view(kvindex.Options{Mode: mode, RequireBackfill: true, Now: now})
}

func (kv *AdminKV) view(o kvindex.Options) *AdminKV {
	v := &AdminKV{recs: kv.recs, mem: kv.mem, wake: kv.wake, locks: kv.locks}

	o.Backend, o.Lock, o.LockKeep = &wakingBackend{MemBackend: kv.mem, w: kv.wake}, kv.locks.lock, kv.locks.lockKeep
	v.Store = kvindex.New(kv.recs, o)

	return v
}

// Listings counts the listings of the record layer (tests: read mode
// must not list).
func (kv *AdminKV) Listings() int64 { return kv.recs.lists.Load() }

// Raw is the record layer (List lists the records, tests).
func (kv *AdminKV) Raw() model.AdminKV { return kv.recs }

// Omap is plain omap access to single objects of the namespace, with
// in-process watch and notify (the counterpart of the RADOS store's).
func (kv *AdminKV) Omap() model.AdminOmap { return &wakingBackend{MemBackend: kv.mem, w: kv.wake} }

// Backend is the omap backend itself (tests set its Hook).
func (kv *AdminKV) Backend() *sortedindex.MemBackend { return kv.mem }

// Objects returns the index and queue objects that exist (tests).
func (kv *AdminKV) Objects() []string { return kv.mem.Objects() }

// Keys returns every record key in order (the backfill's listing).
func (kv *AdminKV) Keys() ([]string, error) {
	kv.recs.mu.Lock()
	defer kv.recs.mu.Unlock()

	keys := make([]string, 0, len(kv.recs.m))
	for k := range kv.recs.m {
		keys = append(keys, k)
	}

	sort.Strings(keys)

	return keys, nil
}

// RevokeLocks marks every held worker lock lost and frees it, as a lost
// renewal does in the RADOS store (tests).
func (kv *AdminKV) RevokeLocks() { kv.locks.revoke() }

// Locks are the worker locks (tests set their Hook and count handles).
func (kv *AdminKV) Locks() *MemLocks { return kv.locks }

// Rotate is the counterpart of the RADOS store's key rotation: the
// leases taken with LockQueue are given up and marked lost, the ones
// taken with LockQueueKeep stand, the connection handle changes, and
// the rotation hooks run (OnRotate), in that order, as Store.Rotate
// does it. LockHandles then tells which leases moved to the new handle.
func (kv *AdminKV) Rotate() { kv.locks.rotate() }

// OnRotate registers a hook Rotate calls; the returned function removes
// it.
func (kv *AdminKV) OnRotate(fn func()) func() { return kv.locks.onRotate(fn) }

// LockHandles counts the held leases by the connection handle they were
// taken or retaken through: on the current one and on older ones
// (tests of LockQueueKeep against LockQueue).
func (kv *AdminKV) LockHandles() (current, old int) { return kv.locks.handles() }

// records is the in-memory record layer.
type records struct {
	mu    sync.Mutex
	m     map[string][]byte
	lists atomic.Int64
}

// Get implements model.AdminKV.
func (kv *records) Get(key string) ([]byte, error) {
	kv.mu.Lock()
	defer kv.mu.Unlock()

	v, ok := kv.m[key]
	if !ok {
		return nil, model.ErrNotFound
	}

	return append([]byte(nil), v...), nil
}

// Update implements model.AdminKV.
func (kv *records) Update(key string, fn func(cur []byte) ([]byte, error)) error {
	kv.mu.Lock()
	defer kv.mu.Unlock()

	var cur []byte
	if v, ok := kv.m[key]; ok {
		cur = append([]byte(nil), v...)
	}

	next, err := fn(cur)
	if err != nil {
		return err
	}

	if next == nil {
		delete(kv.m, key)
	} else {
		kv.m[key] = append([]byte(nil), next...)
	}

	return nil
}

// List implements model.AdminKV.
func (kv *records) List(prefix, after string, limit int) ([]model.KVEntry, error) {
	kv.lists.Add(1)

	kv.mu.Lock()
	defer kv.mu.Unlock()

	var keys []string

	for k := range kv.m {
		if strings.HasPrefix(k, prefix) && k > after {
			keys = append(keys, k)
		}
	}

	sort.Strings(keys)

	out := []model.KVEntry{}
	for _, k := range keys[:min(limit, len(keys))] {
		out = append(out, model.KVEntry{Key: k, Value: append([]byte(nil), kv.m[k]...)})
	}

	return out, nil
}

// wakingBackend adds in-process watch/notify to the memory backend.
type wakingBackend struct {
	*sortedindex.MemBackend
	w *memWaker
}

var _ sortedindex.Waker = (*wakingBackend)(nil)

// Watch implements sortedindex.Waker.
func (b *wakingBackend) Watch(oid string) (model.Watch, error) { return b.w.watch(oid), nil }

// Notify implements sortedindex.Waker.
func (b *wakingBackend) Notify(oid string) { b.w.notify(oid, nil) }

// NotifyPayload implements sortedindex.Waker.
func (b *wakingBackend) NotifyPayload(oid string, data []byte) { b.w.notify(oid, data) }

// WatchPayload implements sortedindex.Waker.
func (b *wakingBackend) WatchPayload(oid string) (model.PayloadWatch, error) {
	return b.w.watchPayload(oid), nil
}

// memWaker delivers notifies to the watchers of an object name.
type memWaker struct {
	mu   sync.Mutex
	subs map[string][]*memSub
}

// memSub is one watcher: a plain one (ch) or one that takes the data
// (pump).
type memSub struct {
	ch   chan struct{}
	pump *sortedindex.PayloadPump
}

func (w *memWaker) add(oid string, s *memSub) func() {
	w.mu.Lock()
	w.subs[oid] = append(w.subs[oid], s)
	w.mu.Unlock()

	return func() {
		w.mu.Lock()
		defer w.mu.Unlock()

		subs := w.subs[oid]
		for i, c := range subs {
			if c == s {
				w.subs[oid] = append(subs[:i:i], subs[i+1:]...)
				break
			}
		}
	}
}

func (w *memWaker) watch(oid string) model.Watch {
	s := &memSub{ch: make(chan struct{}, 1)}

	return &memWatch{ch: s.ch, close: w.add(oid, s)}
}

func (w *memWaker) watchPayload(oid string) model.PayloadWatch {
	s := &memSub{pump: sortedindex.NewPayloadPump()}
	remove := w.add(oid, s)

	return &memPayloadWatch{pump: s.pump, close: func() { remove(); s.pump.Close() }}
}

func (w *memWaker) notify(oid string, data []byte) {
	w.mu.Lock()
	defer w.mu.Unlock()

	for _, s := range w.subs[oid] {
		if s.pump != nil {
			s.pump.Push(data)

			continue
		}

		select {
		case s.ch <- struct{}{}:
		default: // coalesce
		}
	}
}

type memWatch struct {
	ch    chan struct{}
	once  sync.Once
	close func()
}

func (w *memWatch) Events() <-chan struct{} { return w.ch }
func (w *memWatch) Errors() <-chan error    { return nil }
func (w *memWatch) Close()                  { w.once.Do(w.close) }

type memPayloadWatch struct {
	pump  *sortedindex.PayloadPump
	once  sync.Once
	close func()
}

func (w *memPayloadWatch) Events() <-chan []byte { return w.pump.Events() }
func (w *memPayloadWatch) Errors() <-chan error  { return nil }
func (w *memPayloadWatch) Close()                { w.once.Do(w.close) }

// Lock operations the Hook of MemLocks sees.
const (
	LockOpLock   = "lock"
	LockOpRenew  = "renew"
	LockOpUnlock = "unlock"
)

// MemLocks are worker locks with a TTL that the holder renews, as the
// RADOS store's cls_locks: a lease renews itself every TTL/3 in a
// goroutine of its own, the first failed renewal marks it lost, and a
// lease nobody renews expires, so another holder can take it. Tests
// steer them through Hook.
type MemLocks struct {
	mu   sync.Mutex
	held map[string]*memLease
	// Hook, when set, runs before every lock operation (LockOpLock,
	// LockOpRenew, LockOpUnlock) on the lease named; it may block (a
	// lock object in peering) or return an error, which fails the
	// operation. It is read without the lock, so a test sets it before
	// the locks are used.
	Hook func(op, name string) error
	// handle is the current connection handle (Rotate changes it).
	handle int64
	hooks  map[int]func()
	hookID int
}

type memLocks = MemLocks

func newMemLocks() *MemLocks { return &MemLocks{held: map[string]*memLease{}, handle: 1} }

// memLease is one held lock.
type memLease struct {
	l    *MemLocks
	name string
	ttl  time.Duration
	keep bool

	mu sync.Mutex
	// until is when the lock expires in the store; validUntil the
	// holder's deadline (the time the last renewal was sent plus the
	// TTL); handle the connection it was taken or retaken through.
	until, validUntil time.Time
	handle            int64
	lost, done        bool
	stop              chan struct{}
	renewDone         chan struct{}
}

func (m *MemLocks) hook(op, name string) error {
	if m.Hook != nil {
		return m.Hook(op, name)
	}

	return nil
}

// lock takes a lease that a rotation gives up (LockQueue).
func (m *MemLocks) lock(name string, ttl time.Duration) (model.Lease, error) {
	return m.take(name, ttl, false)
}

// lockKeep takes a lease that outlives a rotation (LockQueueKeep).
func (m *MemLocks) lockKeep(name string, ttl time.Duration) (model.Lease, error) {
	return m.take(name, ttl, true)
}

func (m *MemLocks) take(name string, ttl time.Duration, keep bool) (model.Lease, error) {
	if err := m.hook(LockOpLock, name); err != nil {
		return nil, err
	}

	sent := time.Now()

	m.mu.Lock()
	defer m.mu.Unlock()

	if cur, ok := m.held[name]; ok && cur.holds(sent) {
		return nil, model.ErrLocked
	}

	l := &memLease{l: m, name: name, ttl: ttl, keep: keep, until: sent.Add(ttl), validUntil: sent.Add(ttl),
		handle: m.handle, stop: make(chan struct{}), renewDone: make(chan struct{})}
	m.held[name] = l

	go l.renew(l.stop, l.renewDone)

	return l, nil
}

// holds reports whether the lease still holds its lock at now (the
// caller holds the locks' mutex).
func (l *memLease) holds(now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	return !l.done && !l.lost && now.Before(l.until)
}

// renew is the renewal goroutine: every ttl/3 it takes the lock again
// (a hung Hook holds it, as a hung RADOS operation would); a renewal
// that finds the lock held by another lease, or that the Hook refuses,
// marks the lease lost and ends.
func (l *memLease) renew(stop <-chan struct{}, done chan<- struct{}) {
	defer close(done)

	t := time.NewTicker(l.ttl / 3)
	defer t.Stop()

	for {
		select {
		case <-stop:
			return
		case <-t.C:
			if err := l.l.hook(LockOpRenew, l.name); err != nil {
				l.markLost()

				return
			}

			if !l.renewOnce(time.Now()) {
				return
			}
		}
	}
}

// renewOnce is one renewal sent at sent; it reports whether the lease
// goes on.
func (l *memLease) renewOnce(sent time.Time) bool {
	l.l.mu.Lock()
	defer l.l.mu.Unlock()

	l.mu.Lock()
	defer l.mu.Unlock()

	if l.done {
		return false
	}

	if cur := l.l.held[l.name]; cur != l && cur != nil && cur.holds(sent) {
		l.lost = true

		return false
	}

	l.l.held[l.name] = l
	l.until, l.validUntil = sent.Add(l.ttl), sent.Add(l.ttl)

	return true
}

func (l *memLease) markLost() {
	l.mu.Lock()
	defer l.mu.Unlock()

	l.lost = true
}

// Lost implements model.Lease.
func (l *memLease) Lost() bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	return l.lost
}

// ValidUntil implements model.LeaseDeadline.
func (l *memLease) ValidUntil() time.Time {
	l.mu.Lock()
	defer l.mu.Unlock()

	return l.validUntil
}

// Release implements model.Lease: it stops the renewal, waits for one in
// flight (which may hang in the Hook) and unlocks.
func (l *memLease) Release() {
	l.mu.Lock()
	stop, renewDone := l.stop, l.renewDone
	l.stop, l.renewDone = nil, nil
	l.mu.Unlock()

	if stop != nil {
		close(stop)
		<-renewDone
	}

	_ = l.l.hook(LockOpUnlock, l.name)

	l.l.mu.Lock()
	defer l.l.mu.Unlock()

	l.mu.Lock()
	defer l.mu.Unlock()

	l.done = true
	if l.l.held[l.name] == l {
		delete(l.l.held, l.name)
	}
}

// Retake implements model.LeaseRetaker: the lease moves to the current
// handle. It unlocks through the old one and locks again through the
// new one; a Hook that refuses the new lock loses the lease.
func (l *memLease) Retake() error {
	l.mu.Lock()
	stop, renewDone := l.stop, l.renewDone
	l.stop, l.renewDone = nil, nil
	l.mu.Unlock()

	if stop != nil {
		close(stop)
		<-renewDone
	}

	_ = l.l.hook(LockOpUnlock, l.name)

	if err := l.l.hook(LockOpLock, l.name); err != nil {
		l.markLost()

		return err
	}

	sent := time.Now()

	l.l.mu.Lock()
	defer l.l.mu.Unlock()

	l.mu.Lock()
	defer l.mu.Unlock()

	if l.done || l.lost {
		return model.ErrLocked
	}

	l.until, l.validUntil, l.handle = sent.Add(l.ttl), sent.Add(l.ttl), l.l.handle
	l.l.held[l.name] = l
	l.stop, l.renewDone = make(chan struct{}), make(chan struct{})

	go l.renew(l.stop, l.renewDone)

	return nil
}

// revoke marks every held lease lost and frees it.
func (m *MemLocks) revoke() {
	m.mu.Lock()
	leases := make([]*memLease, 0, len(m.held))

	for name, l := range m.held {
		leases = append(leases, l)

		delete(m.held, name)
	}
	m.mu.Unlock()

	for _, l := range leases {
		l.markLost()
	}
}

// rotate is the key rotation: the leases that do not outlive it are
// revoked, the handle changes, the hooks run.
func (m *MemLocks) rotate() {
	m.mu.Lock()

	var gone []*memLease

	for name, l := range m.held {
		if !l.keep {
			gone = append(gone, l)

			delete(m.held, name)
		}
	}

	m.handle++
	hooks := make([]func(), 0, len(m.hooks))

	for _, fn := range m.hooks {
		hooks = append(hooks, fn)
	}
	m.mu.Unlock()

	for _, l := range gone {
		l.markLost()
	}

	for _, fn := range hooks {
		fn()
	}
}

func (m *MemLocks) onRotate(fn func()) func() {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.hooks == nil {
		m.hooks = map[int]func(){}
	}

	m.hookID++
	id := m.hookID
	m.hooks[id] = fn

	return func() {
		m.mu.Lock()
		defer m.mu.Unlock()

		delete(m.hooks, id)
	}
}

func (m *MemLocks) handles() (current, old int) {
	m.mu.Lock()
	defer m.mu.Unlock()

	for _, l := range m.held {
		l.mu.Lock()
		if l.handle == m.handle {
			current++
		} else {
			old++
		}
		l.mu.Unlock()
	}

	return current, old
}

// Held reports whether a lease with the name is held (tests).
func (m *MemLocks) Held(name string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()

	l, ok := m.held[name]

	return ok && l.holds(time.Now())
}
