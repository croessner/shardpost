package cephstore

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/ceph/go-ceph/rados"

	"shardpost/internal/kvindex"
	"shardpost/internal/metrics"
	"shardpost/internal/model"
	"shardpost/internal/sortedindex"
)

// adminNamespace holds admin state (jobs, plans) in the index pool. Its
// name cannot be an account (model.ReservedNamespace).
const adminNamespace = "~admin"

// maxAdminValue bounds one admin record (a job with its report).
const maxAdminValue = model.MaxAdminValue

// AdminKV is the admin store: one RADOS object per record ("kv.<key>"),
// so updates of different keys (heartbeats of different jobs) never
// contend; each update is a compare-and-swap on the object version. The
// hint indexes and due queues of the job system (kvindex) live as omap
// objects "ix.<name>/…" in the same namespace.
//
//	index pool, namespace "~admin":
//	  "kv.<key>"                  the record (job/, plan/, idem/, jmap/…)
//	  "ix.<collection>/head|<nn>" hint index: "<id>" -> what readers need
//	  "ix.jobs/claimable/head|<n>" due queue: "<due>/<id>/<gen>" -> ""
//	  "ix.jobs/expiry/head|<n>"   due queue of expiring records
//	  "ix.jobs/expiry/lock"       cls_lock of the expiry worker
type AdminKV struct {
	*kvindex.Store
	recs *adminRecords
	omap *handleOmap
}

var _ model.IndexedKV = (*AdminKV)(nil)

// adminRecords is the record layer of the admin store (List lists the
// namespace).
// It works on a handle that follows key rotations (handle.go), because
// the job runner, JMAP and the push worker keep it open for the process
// lifetime.
type adminRecords struct{ h *userHandle }

var _ model.AdminKV = (*adminRecords)(nil)

// AdminKV opens the admin store with the configured index mode; Close
// releases it.
func (s *Store) AdminKV() (*AdminKV, error) {
	h, err := s.openHandle(adminNamespace, false)
	if err != nil {
		return nil, err
	}

	s.mu.RLock()
	mode, shards := s.cfg.KVIndex, s.cfg.KVQueueShards
	s.mu.RUnlock()

	recs := &adminRecords{h: h}
	b := &handleOmap{h: h, key: s.indexKey(adminNamespace)}

	st := kvindex.New(recs, kvindex.Options{Mode: mode, QueueShards: shards, Backend: b, Log: s.log.With("component", "kvindex"),
		RequireBackfill: true,
		Lock: func(name string, ttl time.Duration) (model.Lease, error) {
			return s.holdLock(adminNamespace, sortedindex.LockOID(name), "worker", name+" worker", ttl, true)
		},
		// The push worker's partition leases stand through a key rotation
		// (holdLock); their lock operations are measured for its metrics.
		LockKeep: func(name string, ttl time.Duration) (model.Lease, error) {
			start := time.Now()
			l, err := s.holdLock(adminNamespace, sortedindex.LockOID(name), "worker", name+" worker", ttl, false)

			metrics.PushLockOp.WithLabelValues("take").Observe(time.Since(start).Seconds())

			if err != nil {
				return nil, err
			}

			if ll, ok := l.(*lease); ok {
				ll.observer = func(op string, d time.Duration) { metrics.PushLockOp.WithLabelValues(op).Observe(d.Seconds()) }
			}

			return l, nil
		}})

	return &AdminKV{Store: st, recs: recs, omap: b}, nil
}

// Omap is plain omap access to single objects of the admin namespace,
// with watch and notify. It runs on the store's handle, so it follows
// key rotations and stays valid until Close.
func (kv *AdminKV) Omap() model.AdminOmap { return kv.omap }

var _ model.AdminOmap = (*handleOmap)(nil)

// Close releases the store.
func (kv *AdminKV) Close() { kv.recs.h.close() }

// Keys lists the keys of every record (one listing of the namespace, for
// the backfill).
func (kv *AdminKV) Keys() ([]string, error) {
	u, done, err := kv.recs.h.get()
	if err != nil {
		return nil, err
	}
	defer done()

	it, err := listObjects(u.idx, listAdminBackfill)
	if err != nil {
		return nil, err
	}
	defer it.Close()

	var keys []string

	for it.Next() {
		if key, ok := strings.CutPrefix(it.Value(), "kv."); ok {
			keys = append(keys, key)
		}
	}

	if err := it.Err(); err != nil {
		return nil, err
	}

	sort.Strings(keys)

	return keys, nil
}

func adminOID(key string) string { return "kv." + key }

// read returns the value and object version (0 and nil when absent).
// The version is only meaningful on a context nobody else uses between
// the read and the query, so the read takes a private one (verpool.go).
func (kv *adminRecords) read(key string) ([]byte, uint64, error) {
	u, done, err := kv.h.get()
	if err != nil {
		return nil, 0, err
	}
	defer done()

	pool := u.poolFor(u.idx)

	c, err := pool.get()
	if err != nil {
		return nil, 0, err
	}
	defer pool.put(c)

	for attempt := 0; attempt < casRetries; attempt++ {
		st, err := c.Stat(adminOID(key))
		if errCode(err) == syscall.ENOENT {
			return nil, 0, nil
		}

		if err != nil {
			return nil, 0, err
		}

		if st.Size > maxAdminValue {
			return nil, 0, fmt.Errorf("admin record %q too large", key)
		}

		// One byte more than the size shows whether the object grew
		// between the stat and the read.
		buf, ver, err := readVersioned(c, adminOID(key), int(st.Size)+1)
		if errCode(err) == syscall.ENOENT {
			return nil, 0, nil
		}

		if err != nil {
			return nil, 0, err
		}

		if len(buf) <= int(st.Size) {
			return buf, ver, nil
		}
	}

	return nil, 0, fmt.Errorf("admin record %q changes too fast", key)
}

// readVersioned reads up to n bytes and the object version on c.
func readVersioned(c *rados.IOContext, oid string, n int) ([]byte, uint64, error) {
	op := rados.CreateReadOp()
	defer op.Release()

	buf := make([]byte, n)
	step := op.Read(0, buf)

	if err := op.Operate(c, oid, rados.OperationNoFlag); err != nil {
		return nil, 0, err
	}

	ver, err := c.GetLastVersion()
	if err != nil {
		return nil, 0, err
	}

	return buf[:step.BytesRead], ver, nil
}

// Get implements model.AdminKV.
func (kv *adminRecords) Get(key string) ([]byte, error) {
	v, _, err := kv.read(key)
	if err == nil && v == nil {
		return nil, model.ErrNotFound
	}

	return v, err
}

// Update implements model.AdminKV.
func (kv *adminRecords) Update(key string, fn func(cur []byte) ([]byte, error)) error {
	for attempt := 0; attempt < casRetries; attempt++ {
		cur, ver, err := kv.read(key)
		if err != nil {
			return err
		}

		next, err := fn(cur)
		if err != nil {
			return err
		}

		if len(next) > maxAdminValue {
			return fmt.Errorf("admin record %q too large", key)
		}

		err = kv.write(key, ver, cur != nil, next)
		if err == nil {
			return nil
		}

		switch errCode(err) {
		case syscall.ERANGE, syscall.EOVERFLOW, syscall.EEXIST, syscall.ENOENT:
			continue // changed meanwhile
		}

		return err
	}

	return fmt.Errorf("admin record %q: too many concurrent writers", key)
}

// write stores (or with nil removes) the value if the object is still at
// version ver.
func (kv *adminRecords) write(key string, ver uint64, exists bool, next []byte) error {
	op := rados.CreateWriteOp()
	defer op.Release()

	switch {
	case next == nil && !exists:
		return nil
	case next == nil:
		op.AssertVersion(ver)
		op.Remove()
	case !exists:
		op.Create(rados.CreateExclusive)
		op.WriteFull(next)
	default:
		op.AssertVersion(ver)
		op.WriteFull(next)
	}

	u, done, err := kv.h.get()
	if err != nil {
		return err
	}
	defer done()

	return op.Operate(u.idx, adminOID(key), rados.OperationNoFlag)
}

// List implements model.AdminKV. It lists the namespace (admin state is
// small) and reads the selected records.
func (kv *adminRecords) List(prefix, after string, limit int) ([]model.KVEntry, error) {
	keys, err := kv.listKeys(prefix, after)
	if err != nil {
		return nil, err
	}

	out := make([]model.KVEntry, 0, min(limit, len(keys)))

	for _, key := range keys {
		if len(out) == limit {
			break
		}

		v, err := kv.Get(key)
		if errors.Is(err, model.ErrNotFound) {
			continue // removed meanwhile
		}

		if err != nil {
			return nil, err
		}

		out = append(out, model.KVEntry{Key: key, Value: v})
	}

	return out, nil
}

// listKeys lists the record keys with prefix after after, sorted.
func (kv *adminRecords) listKeys(prefix, after string) ([]string, error) {
	u, done, err := kv.h.get()
	if err != nil {
		return nil, err
	}
	defer done()

	it, err := listObjects(u.idx, listAdminKV)
	if err != nil {
		return nil, err
	}
	defer it.Close()

	var keys []string

	for it.Next() {
		key, ok := strings.CutPrefix(it.Value(), "kv.")
		if ok && strings.HasPrefix(key, prefix) && key > after {
			keys = append(keys, key)
		}
	}

	if err := it.Err(); err != nil {
		return nil, err
	}

	sort.Strings(keys)

	return keys, nil
}
