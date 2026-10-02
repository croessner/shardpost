// Package kvindex adds hint indexes and due queues to the admin store
// (scaling plan B3), shared by the RADOS store and the in-memory store.
//
// The admin store keeps one object per record ("kv.<key>" in "~admin").
// Listing those objects visits every placement group of the pool, so the
// collections that are listed get a hint index "ix.<collection>" in the
// same namespace (never under "kv.", which older servers list as
// records). A hint index entry carries what its readers need of the
// record, so a reader that filters does not have to read every record.
//
// Hint rule: a record is written before its entry, and an entry is
// removed before its record. A crash in between leaves a record without
// entry (the backfill "shardpost index backfill kv" adds it) or an entry
// without record (readers skip and remove it). The entry is written from
// the record as it is after the write ("refresh": the index update reads
// the record inside its compare-and-swap), so two writers of one record
// cannot leave the older value in the index.
package kvindex

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"shardpost/internal/metrics"
	"shardpost/internal/model"
	"shardpost/internal/sortedindex"
)

// Collection is an indexed group of admin store keys.
type Collection struct {
	// Name is the index name ("ix.<Name>/…"); the keys of the collection
	// start with Name + "/".
	Name string
	// Shards is the number of shard objects of the index.
	Shards int
	// Value is the index value of a record: what readers need of it.
	Value func(rec []byte) []byte
}

// Prefix is the key prefix of the collection.
func (c *Collection) Prefix() string { return c.Name + "/" }

// The indexed collections and their shard counts (plan B3).
var collections = []*Collection{
	{Name: "job", Shards: 1, Value: jobValue},
	{Name: "plan", Shards: 1, Value: planValue},
	{Name: "idem", Shards: 16, Value: idemValue},
	{Name: "jmap/push/acct", Shards: 64, Value: pushValue},
	{Name: "servers", Shards: 1, Value: serverValue},
}

// Collections returns the indexed collections.
func Collections() []*Collection { return collections }

// CollectionOf returns the collection of key and the id of key in it.
func CollectionOf(key string) (*Collection, string, bool) {
	for _, c := range collections {
		if id, ok := strings.CutPrefix(key, c.Prefix()); ok && id != "" {
			return c, id, true
		}
	}

	return nil, "", false
}

// collectionNamed returns the collection whose prefix is exactly prefix.
func collectionNamed(prefix string) *Collection {
	for _, c := range collections {
		if c.Prefix() == prefix {
			return c
		}
	}

	return nil
}

// JobValue is the index value of a job record.
type JobValue struct {
	State    string    `json:"state"`
	Account  string    `json:"account"`
	Kind     string    `json:"kind"`
	Finished time.Time `json:"finished,omitzero"`
}

func jobValue(rec []byte) []byte {
	var v JobValue

	_ = json.Unmarshal(rec, &v)

	return marshal(v)
}

// PlanValue is the index value of a plan record.
type PlanValue struct {
	Kind    string    `json:"kind"`
	Account string    `json:"account"`
	Expires time.Time `json:"expires"`
}

func planValue(rec []byte) []byte {
	var v PlanValue

	_ = json.Unmarshal(rec, &v)

	return marshal(v)
}

// IdemValue is the index value of an idempotency key record.
type IdemValue struct {
	JobID string `json:"job_id"`
}

func idemValue(rec []byte) []byte {
	var v IdemValue

	_ = json.Unmarshal(rec, &v)

	return marshal(v)
}

// PushValue is the index value of an account's push subscriptions:
// whether one is verified and when the last one expires.
type PushValue struct {
	Verified bool      `json:"verified"`
	Expires  time.Time `json:"expires,omitzero"`
}

func pushValue(rec []byte) []byte {
	var pa struct {
		Subs []struct {
			Expires  time.Time `json:"expires"`
			Verified bool      `json:"verified"`
		} `json:"subs"`
	}

	_ = json.Unmarshal(rec, &pa)

	var v PushValue

	for _, s := range pa.Subs {
		v.Verified = v.Verified || s.Verified
		if s.Expires.After(v.Expires) {
			v.Expires = s.Expires.UTC()
		}
	}

	return marshal(v)
}

// ServerValue is the index value of a server registration: version and
// capabilities, without the time it was last seen, so that a heartbeat
// does not write the index (stage B14b).
type ServerValue struct {
	Version string   `json:"version"`
	Caps    []string `json:"caps"`
}

func serverValue(rec []byte) []byte {
	var v ServerValue

	_ = json.Unmarshal(rec, &v)

	return marshal(v)
}

func marshal(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil || len(b) == 0 {
		return []byte("{}")
	}

	return b
}

// Options configure a Store.
type Options struct {
	// Mode is the configured index mode ("" = model.KVIndexWrite).
	Mode model.KVIndexMode
	// QueueShards is the shard count of a queue the first writer creates
	// (model.DefaultQueueShards when 0; at most model.MaxQueueShards).
	QueueShards int
	// Backend is the omap access to the admin namespace.
	Backend sortedindex.Backend
	// Lock takes a worker lease (model.IndexedKV.LockQueue).
	Lock func(name string, ttl time.Duration) (model.Lease, error)
	Log  *slog.Logger
	// RequireBackfill gates read mode on the completion marker of
	// "shardpost index backfill kv" (MarkerKV): while it is missing the
	// store behaves as in write mode (readers list the namespace) and
	// looks for the marker again every sortedindex.BackfillRecheck.
	RequireBackfill bool
	// Now is the clock of the marker checks (time.Now when nil).
	Now func() time.Time
}

// MarkerKV is the marker of the last completed admin store backfill in
// the head of the job hint index ("ix.job/head"): a backfill report as
// JSON whose "finished" is set.
const MarkerKV = "backfill.kv"

// Store is a model.IndexedKV over a record store (the plain admin
// store, whose List lists the namespace).
type Store struct {
	raw model.AdminKV
	o   Options

	mu     sync.Mutex
	ix     map[string]*sortedindex.Index
	queues map[string]*sortedindex.Queue

	// gate holds read mode back until the backfill marker exists
	// (RequireBackfill).
	gate *sortedindex.Gate
}

var _ model.IndexedKV = (*Store)(nil)

// New wraps the record store raw.
func New(raw model.AdminKV, o Options) *Store {
	if o.Mode == "" {
		o.Mode = model.KVIndexWrite
	}

	if o.QueueShards <= 0 {
		o.QueueShards = model.DefaultQueueShards
	}

	if o.Log == nil {
		o.Log = slog.New(slog.DiscardHandler)
	}

	s := &Store{raw: raw, o: o, ix: map[string]*sortedindex.Index{}, queues: map[string]*sortedindex.Queue{}}

	if o.Mode == model.KVIndexRead && o.RequireBackfill {
		s.gate = &sortedindex.Gate{Head: s.MarkerIndex(), Marker: MarkerKV, Now: o.Now,
			Closed: func(err error) {
				o.Log.Warn("admin.kv_index=read without a completed \"shardpost index backfill kv\": "+
					"behaving as admin.kv_index=write (listing) until the marker exists", "err", err)
			},
			Opened: func() { o.Log.Info("admin.kv_index=read: backfill marker found, readers use the indexes now") },
		}
	}

	return s
}

// IndexMode implements model.IndexedKV: the mode the store behaves in.
// A store configured for read mode that requires the backfill marker
// behaves as in write mode until the marker exists.
func (s *Store) IndexMode() model.KVIndexMode {
	if s.gate != nil && !s.gate.Open() {
		return model.KVIndexWrite
	}

	return s.o.Mode
}

// ConfiguredMode is the configured mode (IndexMode is the effective one).
func (s *Store) ConfiguredMode() model.KVIndexMode { return s.o.Mode }

// writes reports whether the indexes are kept.
func (s *Store) writes() bool { return s.o.Mode != model.KVIndexOff }

// Index returns the hint index of a collection.
func (s *Store) Index(c *Collection) *sortedindex.Index {
	s.mu.Lock()
	defer s.mu.Unlock()

	ix, ok := s.ix[c.Name]
	if !ok {
		ix = sortedindex.New(s.o.Backend, c.Name, c.Shards)
		s.ix[c.Name] = ix
	}

	return ix
}

// Queue implements model.IndexedKV.
func (s *Store) Queue(name string) (model.DueQueue, error) { return s.queue(name), nil }

// QueueOf returns the queue name with its sortedindex type (tests,
// backfill markers).
func (s *Store) QueueOf(name string) *sortedindex.Queue { return s.queue(name) }

func (s *Store) queue(name string) *sortedindex.Queue {
	s.mu.Lock()
	defer s.mu.Unlock()

	q, ok := s.queues[name]
	if !ok {
		q = sortedindex.NewQueue(s.o.Backend, name, s.o.QueueShards)
		s.queues[name] = q
	}

	return q
}

// LockQueue implements model.IndexedKV.
func (s *Store) LockQueue(name string, ttl time.Duration) (model.Lease, error) {
	if s.o.Lock == nil {
		return nil, errors.New("the store has no queue lock")
	}

	return s.o.Lock(name, ttl)
}

// Get implements model.AdminKV.
func (s *Store) Get(key string) ([]byte, error) { return s.raw.Get(key) }

// Update implements model.AdminKV and keeps the hint index of an indexed
// collection: an entry is removed before its record is deleted, and
// refreshed after a write that created the record or changed its index
// value.
func (s *Store) Update(key string, fn func(cur []byte) ([]byte, error)) error {
	c, id, ok := CollectionOf(key)
	if !ok || !s.writes() {
		return s.raw.Update(key, fn)
	}

	w := &hintWrite{s: s, c: c, id: id, key: key}

	err := s.raw.Update(key, func(cur []byte) ([]byte, error) {
		next, err := fn(cur)
		if err != nil {
			return nil, err
		}

		w.seen(cur, next)

		return next, nil
	})
	if err != nil {
		if w.removed {
			s.Refresh(key)
		}

		return err
	}

	if w.after != nil && (w.before == nil || w.removed || !bytes.Equal(c.Value(w.before), c.Value(w.after))) {
		s.Refresh(key)
	}

	return nil
}

// hintWrite follows one Update of an indexed record.
type hintWrite struct {
	s             *Store
	c             *Collection
	id, key       string
	before, after []byte
	removed       bool
}

// seen is called with each compare-and-swap round's values; before a
// delete it removes the entry (entry before record: a crash leaves a
// record without entry, which the backfill adds again).
func (w *hintWrite) seen(cur, next []byte) {
	if next == nil && cur != nil && !w.removed {
		if err := w.s.Index(w.c).Remove(w.id); err != nil {
			w.s.indexError(w.c, w.key, err)
		}

		w.removed = true
	}

	w.before, w.after = cur, next
}

// Refresh writes the hint index entry of key from the record as it is
// now (removing it when the record is gone). The record is read inside
// the index's compare-and-swap, so the last refresh reflects the last
// record write. Errors are logged and counted: the record stands, and
// the backfill repairs the entry.
func (s *Store) Refresh(key string) {
	if err := s.refresh(key); err != nil {
		c, _, _ := CollectionOf(key)
		s.indexError(c, key, err)
	}
}

// Reindex is Refresh that returns the error (backfill).
func (s *Store) Reindex(key string) error { return s.refresh(key) }

// MarkerIndex is the index whose head carries the backfill markers of
// the admin store (the job hint index).
func (s *Store) MarkerIndex() model.SortedIndex { return s.Index(collections[0]) }

func (s *Store) refresh(key string) error {
	c, id, ok := CollectionOf(key)
	if !ok {
		return nil
	}

	return s.Index(c).Update(id, func([]byte) ([]byte, error) {
		rec, err := s.raw.Get(key)

		switch {
		case errors.Is(err, model.ErrNotFound):
			return nil, nil
		case err != nil:
			return nil, err
		}

		return c.Value(rec), nil
	})
}

func (s *Store) indexError(c *Collection, key string, err error) {
	name := "unknown"
	if c != nil {
		name = c.Name
	}

	metrics.KVIndexErrors.WithLabelValues(name).Inc()
	s.o.Log.Warn("admin store: hint index not written", "key", key, "err", err)
}

// List implements model.AdminKV. In read mode a listing of exactly an
// indexed collection comes from its hint index (entries without record
// are skipped and removed); otherwise it lists the namespace.
func (s *Store) List(prefix, after string, limit int) ([]model.KVEntry, error) {
	if s.IndexMode() != model.KVIndexRead || collectionNamed(prefix) == nil {
		return s.raw.List(prefix, after, limit)
	}

	out := make([]model.KVEntry, 0, min(limit, 256))

	for len(out) < limit {
		page, err := s.ListIndex(prefix, after, limit-len(out))
		if err != nil {
			return nil, err
		}

		for _, e := range page {
			after = e.Key

			rec, err := s.raw.Get(e.Key)
			if errors.Is(err, model.ErrNotFound) {
				s.Refresh(e.Key) // removes it unless the record appeared
				continue
			}

			if err != nil {
				return nil, err
			}

			out = append(out, model.KVEntry{Key: e.Key, Value: rec})
		}

		if len(page) == 0 {
			break
		}
	}

	return out, nil
}

// ListIndex implements model.IndexedKV.
func (s *Store) ListIndex(prefix, after string, limit int) ([]model.KVEntry, error) {
	c := collectionNamed(prefix)
	if c == nil {
		return nil, fmt.Errorf("%w: %q", model.ErrNotIndexed, prefix)
	}

	afterID := ""
	if a, ok := strings.CutPrefix(after, prefix); ok {
		afterID = a
	} else if after > prefix {
		return nil, nil // after the whole collection
	}

	page, err := s.Index(c).ListOrdered(afterID, limit)
	if err != nil {
		return nil, err
	}

	out := make([]model.KVEntry, 0, len(page))
	for _, e := range page {
		out = append(out, model.KVEntry{Key: prefix + e.ID, Value: e.Value})
	}

	return out, nil
}
