// Package sortedindex implements model.SortedIndex over omap objects
// (scaling plan B1). The logic is shared by the RADOS store and the
// in-memory store; each supplies a Backend for the objects of one
// namespace.
//
// Layout: the head object "ix.<name>/head" holds the shard count, the
// format, the reshard phase and named markers as omap keys; the entries
// live in "ix.<name>/<n>" (n = model.IndexShard(id, shards), two digits
// at least), one omap key per id.
//
// A write of one entry is one write op on its shard object. Update is a
// compare-and-swap on the shard object's version (go-ceph has no
// omap_cmp), so concurrent updates of entries of the same shard retry.
package sortedindex

import (
	"bytes"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"shardpost/internal/model"
)

// Head keys.
const (
	keyShards = "shards"
	keyFormat = "format"
	keyPhase  = "phase"
	keyMarker = "mark:"
)

// ErrConflict is returned by Backend.Write when the object's version is
// no longer the one the write was guarded with (or an exclusive create
// met an existing object).
var ErrConflict = errors.New("object changed concurrently")

// AnyVersion makes Backend.Write unconditional.
const AnyVersion = ^uint64(0)

// casRetries bounds the compare-and-swap rounds of one Update.
const casRetries = 32

// HeadTTL is how long a process trusts a cached head. A reshard (a later
// stage) announces itself through the head, so writers notice it within
// this time.
const HeadTTL = 10 * time.Second

// KV is one omap key with its value.
type KV struct {
	Key   string
	Value []byte
}

// Backend is the omap access to the objects of one namespace.
type Backend interface {
	// ReadKeys returns the values of keys that exist and the object's
	// version (0: the object does not exist).
	ReadKeys(oid string, keys []string) (map[string][]byte, uint64, error)
	// ReadPage returns up to limit keys after startAfter in key order and
	// whether more follow; an absent object reads as empty.
	ReadPage(oid, startAfter string, limit int) ([]KV, bool, error)
	// Write sets and removes keys. ver 0 creates the object exclusively,
	// AnyVersion writes unconditionally (creating the object when set is
	// not empty; removing from an absent object is no error), any other
	// version asserts it. A failed guard returns ErrConflict.
	Write(oid string, ver uint64, set map[string][]byte, rm []string) error
	// CacheKey identifies the namespace for the process-wide head cache.
	CacheKey() string
}

// Index is a model.SortedIndex over a Backend. It is not safe for
// concurrent use beyond what the Backend allows; open one per user.
type Index struct {
	b        Backend
	name     string
	defShard int
	now      func() time.Time
}

var _ model.SortedIndex = (*Index)(nil)

// New returns the index name in the backend's namespace; defaultShards
// applies while no head exists (and to the head its first writer
// creates).
func New(b Backend, name string, defaultShards int) *Index {
	if defaultShards < 1 {
		defaultShards = 1
	}

	return &Index{b: b, name: name, defShard: defaultShards, now: time.Now}
}

// HeadOID and ShardOID name the objects of an index.
func HeadOID(name string) string { return "ix." + name + "/head" }

// ShardOID names shard n of an index.
func ShardOID(name string, n int) string { return fmt.Sprintf("ix.%s/%02d", name, n) }

// --- head --------------------------------------------------------------------

type cachedHead struct {
	head model.IndexHead
	exp  time.Time
}

var (
	headMu    sync.Mutex
	headCache = map[string]cachedHead{}
)

func (ix *Index) cacheKey() string { return ix.b.CacheKey() + "\x00" + ix.name }

// Head implements model.SortedIndex (uncached: it reads the markers).
func (ix *Index) Head() (model.IndexHead, error) {
	h, exists, err := ix.readHead()
	if err != nil || !exists {
		return h, err
	}

	after := ""

	for {
		page, more, err := ix.b.ReadPage(HeadOID(ix.name), after, 256)
		if err != nil {
			return h, err
		}

		for _, kv := range page {
			after = kv.Key
			if m, ok := strings.CutPrefix(kv.Key, keyMarker); ok {
				h.Markers[m] = string(kv.Value)
			}
		}

		if !more {
			return h, nil
		}
	}
}

// readHead reads the head object without its markers; exists is false
// when no writer has created it yet (the defaults apply).
func (ix *Index) readHead() (model.IndexHead, bool, error) {
	vals, ver, err := ix.b.ReadKeys(HeadOID(ix.name), []string{keyShards, keyFormat, keyPhase})
	if err != nil {
		return model.IndexHead{}, false, err
	}

	h := model.IndexHead{Shards: ix.defShard, Format: model.IndexFormat, Phase: model.IndexPhaseStable, Markers: map[string]string{}}
	if ver == 0 {
		return h, false, nil
	}

	if v, ok := vals[keyShards]; ok {
		if h.Shards, err = strconv.Atoi(string(v)); err != nil || h.Shards < 1 {
			return h, true, fmt.Errorf("index %s: bad shard count %q", ix.name, v)
		}
	}

	if v, ok := vals[keyFormat]; ok {
		if h.Format, err = strconv.Atoi(string(v)); err != nil {
			return h, true, fmt.Errorf("index %s: bad format %q", ix.name, v)
		}
	}

	if v, ok := vals[keyPhase]; ok {
		h.Phase = string(v)
	}

	return h, true, nil
}

// head returns the cached head for reads (the defaults without a head).
func (ix *Index) head() (model.IndexHead, error) {
	key := ix.cacheKey()
	now := ix.now()

	headMu.Lock()
	c, ok := headCache[key]
	headMu.Unlock()

	if ok && now.Before(c.exp) {
		return c.head, nil
	}

	h, exists, err := ix.readHead()
	if err != nil {
		return h, err
	}

	if h.Format > model.IndexFormat {
		return h, fmt.Errorf("index %s: %w (format %d)", ix.name, model.ErrIndexFormat, h.Format)
	}

	// Without a head the defaults are not cached: the first writer
	// creates the head, and readers must not keep a guess.
	if exists {
		headMu.Lock()
		headCache[key] = cachedHead{head: h, exp: now.Add(HeadTTL)}
		headMu.Unlock()
	}

	return h, nil
}

// writeHead returns the head for a write, creating it when absent, and
// refuses while a reshard runs.
func (ix *Index) writeHead() (model.IndexHead, error) {
	h, err := ix.head()
	if err != nil {
		return h, err
	}

	if h.Phase != model.IndexPhaseStable {
		return h, fmt.Errorf("index %s: %w (%s)", ix.name, model.ErrIndexReshard, h.Phase)
	}

	headMu.Lock()
	_, cached := headCache[ix.cacheKey()]
	headMu.Unlock()

	if cached {
		return h, nil
	}

	err = ix.b.Write(HeadOID(ix.name), 0, map[string][]byte{
		keyShards: []byte(strconv.Itoa(ix.defShard)),
		keyFormat: []byte(strconv.Itoa(model.IndexFormat)),
		keyPhase:  []byte(model.IndexPhaseStable),
	}, nil)
	if err != nil && !errors.Is(err, ErrConflict) {
		return h, fmt.Errorf("index %s: create head: %w", ix.name, err)
	}

	// Created now or by another writer: read what is there.
	return ix.head()
}

// SetMarker implements model.SortedIndex.
func (ix *Index) SetMarker(key, value string) error {
	if _, err := ix.writeHead(); err != nil {
		return err
	}

	return ix.b.Write(HeadOID(ix.name), AnyVersion, map[string][]byte{keyMarker + key: []byte(value)}, nil)
}

// --- entries -----------------------------------------------------------------

func (ix *Index) shardOID(h model.IndexHead, id string) string {
	return ShardOID(ix.name, model.IndexShard(id, h.Shards))
}

// Get implements model.SortedIndex.
func (ix *Index) Get(id string) ([]byte, error) {
	h, err := ix.head()
	if err != nil {
		return nil, err
	}

	vals, _, err := ix.b.ReadKeys(ix.shardOID(h, id), []string{id})
	if err != nil {
		return nil, err
	}

	v, ok := vals[id]
	if !ok {
		return nil, model.ErrNotFound
	}

	return v, nil
}

// Set implements model.SortedIndex.
func (ix *Index) Set(id string, value []byte) error {
	h, err := ix.writeHead()
	if err != nil {
		return err
	}

	return ix.b.Write(ix.shardOID(h, id), AnyVersion, map[string][]byte{id: value}, nil)
}

// Remove implements model.SortedIndex.
func (ix *Index) Remove(id string) error {
	h, err := ix.writeHead()
	if err != nil {
		return err
	}

	return ix.b.Write(ix.shardOID(h, id), AnyVersion, nil, []string{id})
}

// Update implements model.SortedIndex.
func (ix *Index) Update(id string, fn func(cur []byte) ([]byte, error)) error {
	h, err := ix.writeHead()
	if err != nil {
		return err
	}

	oid := ix.shardOID(h, id)

	for attempt := 0; attempt < casRetries; attempt++ {
		vals, ver, err := ix.b.ReadKeys(oid, []string{id})
		if err != nil {
			return err
		}

		cur, had := vals[id]

		next, err := fn(cur)
		if err != nil {
			return err
		}

		var (
			set map[string][]byte
			rm  []string
		)

		switch {
		case next == nil && !had:
			return nil
		case next == nil:
			rm = []string{id}
		case had && bytes.Equal(next, cur):
			return nil
		default:
			set = map[string][]byte{id: next}
		}

		err = ix.b.Write(oid, ver, set, rm)
		if errors.Is(err, ErrConflict) {
			continue
		}

		return err
	}

	return fmt.Errorf("index %s: %q: too many concurrent writers", ix.name, id)
}

// List implements model.SortedIndex: a page reads the shard of the
// cursor from the position after its id and continues with the next
// shards until the page is full (one read per shard).
func (ix *Index) List(cursor string, limit int) ([]model.IndexEntry, string, error) {
	shard, after, err := model.ParseIndexCursor(cursor)
	if err != nil {
		return nil, "", err
	}

	if limit < 1 {
		limit = 1
	}

	h, err := ix.head()
	if err != nil {
		return nil, "", err
	}

	var out []model.IndexEntry

	for shard < h.Shards {
		page, more, err := ix.b.ReadPage(ShardOID(ix.name, shard), after, limit-len(out))
		if err != nil {
			return nil, "", err
		}

		for _, kv := range page {
			out = append(out, model.IndexEntry{ID: kv.Key, Value: kv.Value})
			after = kv.Key
		}

		if len(out) == limit {
			if more {
				return out, model.IndexCursor(shard, after), nil
			}

			if shard+1 < h.Shards {
				return out, model.IndexCursor(shard+1, ""), nil
			}

			return out, "", nil
		}

		shard, after = shard+1, ""
	}

	return out, "", nil
}

// ForgetHeads empties the head cache (tests).
func ForgetHeads() {
	headMu.Lock()
	defer headMu.Unlock()

	clear(headCache)
}
