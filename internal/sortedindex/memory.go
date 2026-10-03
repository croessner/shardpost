package sortedindex

import (
	"maps"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
)

// MemBackend is an in-memory Backend with object versions, for the
// in-memory store and tests. It is safe for concurrent use.
type MemBackend struct {
	mu   sync.Mutex
	key  string
	objs map[string]*memObject
	// Writes counts the successful writes (tests).
	Writes atomic.Int64
	// hookFn, when set (SetHook), runs before every operation
	// ("read_keys", "read_page", "write") on the object named; it may
	// block (an object in peering) or return an error, which the
	// operation then returns (tests).
	hookMu sync.RWMutex
	hookFn func(op, oid string) error
}

// SetHook sets the hook of the operations (nil: none); tests set it
// while the backend is in use.
func (m *MemBackend) SetHook(fn func(op, oid string) error) {
	m.hookMu.Lock()
	defer m.hookMu.Unlock()

	m.hookFn = fn
}

// hook runs the hook of an operation, if any (outside the lock: it may
// block).
func (m *MemBackend) hook(op, oid string) error {
	m.hookMu.RLock()
	fn := m.hookFn
	m.hookMu.RUnlock()

	if fn != nil {
		return fn(op, oid)
	}

	return nil
}

type memObject struct {
	ver  uint64
	omap map[string][]byte
}

var memSeq atomic.Int64

// NewMemBackend returns an empty namespace.
func NewMemBackend() *MemBackend {
	return &MemBackend{objs: map[string]*memObject{}, key: "mem/" + strconv.FormatInt(memSeq.Add(1), 10)}
}

// CacheKey implements Backend.
func (m *MemBackend) CacheKey() string { return m.key }

// ReadKeys implements Backend.
func (m *MemBackend) ReadKeys(oid string, keys []string) (map[string][]byte, uint64, error) {
	if err := m.hook("read_keys", oid); err != nil {
		return nil, 0, err
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	out := map[string][]byte{}

	o, ok := m.objs[oid]
	if !ok {
		return out, 0, nil
	}

	for _, k := range keys {
		if v, ok := o.omap[k]; ok {
			out[k] = slices.Clone(v)
		}
	}

	return out, o.ver, nil
}

// ReadPage implements Backend.
func (m *MemBackend) ReadPage(oid, startAfter string, limit int) ([]KV, bool, error) {
	if err := m.hook("read_page", oid); err != nil {
		return nil, false, err
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	o, ok := m.objs[oid]
	if !ok {
		return nil, false, nil
	}

	keys := slices.Sorted(maps.Keys(o.omap))

	var out []KV

	for i, k := range keys {
		if k <= startAfter {
			continue
		}

		if len(out) == limit {
			return out, i < len(keys), nil
		}

		out = append(out, KV{Key: k, Value: slices.Clone(o.omap[k])})
	}

	return out, false, nil
}

// Write implements Backend.
func (m *MemBackend) Write(oid string, ver uint64, set map[string][]byte, rm []string) error {
	if err := m.hook("write", oid); err != nil {
		return err
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	o, ok := m.objs[oid]
	if err := guard(o, ok, ver); err != nil {
		return err
	}

	if !ok {
		if len(set) == 0 {
			return nil // only removals from an absent object
		}

		o = &memObject{omap: map[string][]byte{}}
		m.objs[oid] = o
	}

	for k, v := range set {
		o.omap[k] = slices.Clone(v)
	}

	for _, k := range rm {
		delete(o.omap, k)
	}

	o.ver++

	m.Writes.Add(1)

	return nil
}

// guard checks the version a write is guarded with.
func guard(o *memObject, exists bool, ver uint64) error {
	switch {
	case ver == AnyVersion:
		return nil
	case ver == 0 && exists, ver != 0 && (!exists || o.ver != ver):
		return ErrConflict
	default:
		return nil
	}
}

// Objects returns the names of the objects that exist (tests).
func (m *MemBackend) Objects() []string {
	m.mu.Lock()
	defer m.mu.Unlock()

	return slices.Sorted(maps.Keys(m.objs))
}
