package sortedindex

import (
	"errors"
	"log/slog"
	"slices"
	"strings"
	"time"

	"shardpost/internal/metrics"
	"shardpost/internal/model"
)

// Waker is implemented by a Backend that can watch and notify objects
// (RADOS watch/notify, or an in-process stand-in).
type Waker interface {
	// Watch watches oid; it creates the object when it does not exist.
	Watch(oid string) (model.Watch, error)
	// Notify wakes the watchers of oid (best effort, may return before
	// they are woken).
	Notify(oid string)
}

// ErrNoWatch is returned by Queue.Watch on a backend without Waker.
var ErrNoWatch = errors.New("the store cannot watch a queue")

// Queue is a model.DueQueue over a Backend: a sorted index whose ids are
// the keys "<due>/<id>/<gen>", sharded by the id alone so that every
// entry of an id lives in one shard object.
type Queue struct {
	ix *Index
}

var _ model.DueQueue = (*Queue)(nil)

// NewQueue returns the due queue name in the backend's namespace with
// shards shards (1 to model.MaxQueueShards) while no head exists.
func NewQueue(b Backend, name string, shards int) *Queue {
	shards = min(max(shards, 1), model.MaxQueueShards)

	return &Queue{ix: New(b, name, shards)}
}

// Queue operations (label "op" of imapceph_queue_ops_total).
const (
	opMove = "move"
	opDue  = "due"
)

// staleUnparsable is the action of imapceph_queue_stale_entries_total
// for a key Due cannot parse.
const staleUnparsable = "unparsable"

func (q *Queue) count(op string, err error) {
	outcome := "ok"
	if err != nil {
		outcome = "error"
	}

	metrics.QueueOps.WithLabelValues(q.ix.name, op, outcome).Inc()
}

// Move implements model.DueQueue: one write on the shard of id.
func (q *Queue) Move(id string, from []model.DueKey, to *model.DueKey) (err error) {
	defer func() { q.count(opMove, err) }()

	h, err := q.ix.writeHead()
	if err != nil {
		return err
	}

	var (
		set  map[string][]byte
		keep string
		rm   []string
	)

	if to != nil {
		keep = model.FormatDueKey(id, *to)
		set = map[string][]byte{keep: {}}
	}

	for _, k := range from {
		if key := model.FormatDueKey(id, k); key != keep && !slices.Contains(rm, key) {
			rm = append(rm, key)
		}
	}

	if set == nil && rm == nil {
		return nil
	}

	return q.ix.b.Write(ShardOID(q.ix.name, model.IndexShard(id, h.Shards)), AnyVersion, set, rm)
}

// Due implements model.DueQueue: it reads the first limit keys of every
// shard (one read each) and merges them. An unreadable key is skipped.
func (q *Queue) Due(now time.Time, limit int) (out []model.DueEntry, err error) {
	defer func() { q.count(opDue, err) }()

	limit = max(limit, 1)

	h, err := q.ix.head()
	if err != nil {
		return nil, err
	}

	bound := model.FormatDueKey("", model.DueKey{Due: now})[:dueWidth+1] + "\xff"

	var keys []string

	for shard := range h.Shards {
		page, _, err := q.ix.b.ReadPage(ShardOID(q.ix.name, shard), "", limit)
		if err != nil {
			return nil, err
		}

		for _, kv := range page {
			if kv.Key > bound {
				break
			}

			keys = append(keys, kv.Key)
		}
	}

	slices.Sort(keys)

	for _, k := range keys {
		if len(out) == limit {
			break
		}

		e, err := model.ParseDueKey(k)
		if err != nil {
			// Not written by this layout: counted, never handed out.
			metrics.QueueStale.WithLabelValues(q.ix.name, staleUnparsable).Inc()
			slog.Debug("queue: unparsable key", "queue", q.ix.name, "key", k, "err", err)

			continue
		}

		if e.Due.After(now) {
			continue
		}

		out = append(out, e)
	}

	return out, nil
}

// dueWidth is the width of the due time in a key.
const dueWidth = 20

// Head implements model.DueQueue.
func (q *Queue) Head() (model.IndexHead, error) { return q.ix.Head() }

// SetMarker stores a named marker in the head.
func (q *Queue) SetMarker(key, value string) error { return q.ix.SetMarker(key, value) }

// Watch implements model.DueQueue: it watches shard 0.
func (q *Queue) Watch() (model.Watch, error) {
	w, ok := q.ix.b.(Waker)
	if !ok {
		return nil, ErrNoWatch
	}

	return w.Watch(ShardOID(q.ix.name, 0))
}

// Notify implements model.DueQueue.
func (q *Queue) Notify() {
	if w, ok := q.ix.b.(Waker); ok {
		w.Notify(ShardOID(q.ix.name, 0))
	}
}

// All returns every entry of the queue in shard order (tests and
// diagnosis).
func (q *Queue) All() ([]model.DueEntry, error) {
	var out []model.DueEntry

	cursor := ""

	for {
		page, next, err := q.ix.List(cursor, 1000)
		if err != nil {
			return nil, err
		}

		for _, e := range page {
			if d, err := model.ParseDueKey(e.ID); err == nil {
				out = append(out, d)
			}
		}

		if next == "" {
			return out, nil
		}

		cursor = next
	}
}

// ListOrdered returns up to limit entries whose ids sort after after, in
// id order across all shards (a k-way merge): every shard is read from
// after on, and only the entries up to the smallest last id of a shard
// that has more are certain to be complete. A page reads every shard
// once and returns at least one entry while any is left.
func (ix *Index) ListOrdered(after string, limit int) ([]model.IndexEntry, error) {
	limit = max(limit, 1)

	h, err := ix.head()
	if err != nil {
		return nil, err
	}

	per := limit
	if h.Shards > 1 {
		per = max(8, 2*((limit+h.Shards-1)/h.Shards))
	}

	all, bound, cut, err := ix.readHeads(h.Shards, after, per)
	if err != nil {
		return nil, err
	}

	slices.SortFunc(all, func(a, b model.IndexEntry) int { return strings.Compare(a.ID, b.ID) })

	out := make([]model.IndexEntry, 0, min(limit, len(all)))

	for _, e := range all {
		if len(out) == limit || (cut && e.ID > bound) {
			break
		}

		out = append(out, e)
	}

	return out, nil
}

// readHeads reads up to per entries after after from every shard; bound
// is the smallest last id of a shard that has more (cut false: none has).
func (ix *Index) readHeads(shards int, after string, per int) (all []model.IndexEntry, bound string, cut bool, err error) {
	for shard := range shards {
		page, more, err := ix.b.ReadPage(ShardOID(ix.name, shard), after, per)
		if err != nil {
			return nil, "", false, err
		}

		for _, kv := range page {
			all = append(all, model.IndexEntry{ID: kv.Key, Value: kv.Value})
		}

		if !more || len(page) == 0 {
			continue
		}

		if last := page[len(page)-1].Key; !cut || last < bound {
			bound, cut = last, true
		}
	}

	return all, bound, cut, nil
}
