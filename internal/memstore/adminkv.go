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
		wake: &memWaker{subs: map[string][]chan struct{}{}}, locks: &memLocks{held: map[string]*memLease{}}}

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

	o.Backend, o.Lock = &wakingBackend{MemBackend: kv.mem, w: kv.wake}, kv.locks.lock
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

// RevokeLocks marks every held worker lock lost and frees it, as a key
// rotation does in the RADOS store (tests).
func (kv *AdminKV) RevokeLocks() { kv.locks.revoke() }

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
func (b *wakingBackend) Notify(oid string) { b.w.notify(oid) }

// memWaker delivers notifies to the watchers of an object name.
type memWaker struct {
	mu   sync.Mutex
	subs map[string][]chan struct{}
}

func (w *memWaker) watch(oid string) model.Watch {
	ch := make(chan struct{}, 1)

	w.mu.Lock()
	w.subs[oid] = append(w.subs[oid], ch)
	w.mu.Unlock()

	return &memWatch{ch: ch, close: func() {
		w.mu.Lock()
		defer w.mu.Unlock()

		subs := w.subs[oid]
		for i, c := range subs {
			if c == ch {
				w.subs[oid] = append(subs[:i:i], subs[i+1:]...)
				break
			}
		}
	}}
}

func (w *memWaker) notify(oid string) {
	w.mu.Lock()
	defer w.mu.Unlock()

	for _, ch := range w.subs[oid] {
		select {
		case ch <- struct{}{}:
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

// memLocks are worker locks with a TTL that the holder renews.
type memLocks struct {
	mu   sync.Mutex
	held map[string]*memLease
}

type memLease struct {
	l     *memLocks
	name  string
	until time.Time
	ttl   time.Duration
	lost  bool
	done  bool
}

func (m *memLocks) lock(name string, ttl time.Duration) (model.Lease, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	now := time.Now()
	if cur, ok := m.held[name]; ok && !cur.done && now.Before(cur.until) {
		return nil, model.ErrLocked
	}

	l := &memLease{l: m, name: name, until: now.Add(ttl), ttl: ttl}
	m.held[name] = l

	return l, nil
}

func (m *memLocks) revoke() {
	m.mu.Lock()
	defer m.mu.Unlock()

	for name, l := range m.held {
		l.lost, l.done = true, true

		delete(m.held, name)
	}
}

// Lost implements model.Lease; a held lease renews itself on the check.
func (l *memLease) Lost() bool {
	l.l.mu.Lock()
	defer l.l.mu.Unlock()

	if !l.lost && !l.done {
		l.until = time.Now().Add(l.ttl)
	}

	return l.lost
}

// Release implements model.Lease.
func (l *memLease) Release() {
	l.l.mu.Lock()
	defer l.l.mu.Unlock()

	l.done = true
	if l.l.held[l.name] == l {
		delete(l.l.held, l.name)
	}
}
