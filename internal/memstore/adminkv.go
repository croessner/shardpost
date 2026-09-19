package memstore

import (
	"sort"
	"strings"
	"sync"

	"shardpost/internal/model"
)

// AdminKV is an in-memory model.AdminKV for tests.
type AdminKV struct {
	mu sync.Mutex
	m  map[string][]byte
}

var _ model.AdminKV = (*AdminKV)(nil)

// NewAdminKV returns an empty store.
func NewAdminKV() *AdminKV { return &AdminKV{m: map[string][]byte{}} }

// Get implements model.AdminKV.
func (kv *AdminKV) Get(key string) ([]byte, error) {
	kv.mu.Lock()
	defer kv.mu.Unlock()

	v, ok := kv.m[key]
	if !ok {
		return nil, model.ErrNotFound
	}

	return append([]byte(nil), v...), nil
}

// Update implements model.AdminKV.
func (kv *AdminKV) Update(key string, fn func(cur []byte) ([]byte, error)) error {
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
func (kv *AdminKV) List(prefix, after string, limit int) ([]model.KVEntry, error) {
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
