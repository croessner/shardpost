package learnindex

import (
	"slices"
	"sort"
	"sync"
	"sync/atomic"

	"shardpost/internal/imapsieve"
	"shardpost/internal/model"
)

// MemRecords is an in-memory Records for tests: jobs in a map, listings
// counted, a worker lease that tests can revoke (a key rotation).
type MemRecords struct {
	mu    sync.Mutex
	jobs  map[string]imapsieve.Job
	raw   map[string][]byte
	ready map[string]bool
	lease *memLease
	lists atomic.Int64
}

var _ Records = (*MemRecords)(nil)

// NewMemRecords returns an empty record layer.
func NewMemRecords() *MemRecords {
	return &MemRecords{jobs: map[string]imapsieve.Job{}, raw: map[string][]byte{}, ready: map[string]bool{}}
}

// Listings counts the listings (tests: read mode must not list).
func (m *MemRecords) Listings() int64 { return m.lists.Load() }

// SetReady sets what Ready answers for a prepared job.
func (m *MemRecords) SetReady(id string, ready bool) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.ready[id] = ready
}

// Create implements Records.
func (m *MemRecords) Create(j imapsieve.Job, raw []byte) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if _, ok := m.jobs[j.ID]; ok {
		return false, nil
	}

	m.jobs[j.ID], m.raw[j.ID] = j, slices.Clone(raw)

	return true, nil
}

// Get implements Records.
func (m *MemRecords) Get(id string) (imapsieve.Job, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	j, ok := m.jobs[id]
	if !ok {
		return imapsieve.Job{}, imapsieve.ErrNoJob
	}

	return j, nil
}

// Set implements Records.
func (m *MemRecords) Set(j imapsieve.Job) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if _, ok := m.jobs[j.ID]; !ok {
		return imapsieve.ErrNoJob
	}

	m.jobs[j.ID] = j

	return nil
}

// Put stores a job as a server without the indexes would (tests).
func (m *MemRecords) Put(j imapsieve.Job, raw []byte) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.jobs[j.ID], m.raw[j.ID] = j, slices.Clone(raw)
}

// Remove implements Records.
func (m *MemRecords) Remove(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	delete(m.jobs, id)
	delete(m.raw, id)

	return nil
}

// Read implements Records.
func (m *MemRecords) Read(j imapsieve.Job) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	raw, ok := m.raw[j.ID]
	if !ok {
		return nil, imapsieve.ErrNoJob
	}

	return slices.Clone(raw), nil
}

// List implements Records.
func (m *MemRecords) List(string) ([]imapsieve.Job, error) {
	m.lists.Add(1)

	m.mu.Lock()
	defer m.mu.Unlock()

	out := make([]imapsieve.Job, 0, len(m.jobs))
	for _, j := range m.jobs {
		out = append(out, j)
	}

	sort.Slice(out, func(a, b int) bool { return out[a].ID < out[b].ID })

	return out, nil
}

// Ready implements Records: true unless SetReady said otherwise.
func (m *MemRecords) Ready(j imapsieve.Job) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	ready, ok := m.ready[j.ID]

	return !ok || ready, nil
}

// Cleanup implements Records.
func (m *MemRecords) Cleanup(imapsieve.Job) error { return nil }

// Lock implements Records: one holder at a time.
func (m *MemRecords) Lock() (model.Lease, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.lease != nil {
		return nil, model.ErrLocked
	}

	m.lease = &memLease{m: m}

	return m.lease, nil
}

// Revoke marks the held lease lost and frees the lock, as a key rotation
// does in the RADOS store (tests).
func (m *MemRecords) Revoke() {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.lease != nil {
		m.lease.lost.Store(true)
		m.lease = nil
	}
}

// Held reports whether a lease is held (tests).
func (m *MemRecords) Held() bool {
	m.mu.Lock()
	defer m.mu.Unlock()

	return m.lease != nil
}

type memLease struct {
	m    *MemRecords
	lost atomic.Bool
}

func (l *memLease) Lost() bool { return l.lost.Load() }

func (l *memLease) Release() {
	l.m.mu.Lock()
	defer l.m.mu.Unlock()

	if l.m.lease == l {
		l.m.lease = nil
	}
}
