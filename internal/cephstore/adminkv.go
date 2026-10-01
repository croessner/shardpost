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
}

var _ model.IndexedKV = (*AdminKV)(nil)

// adminRecords is the record layer of the admin store (List lists the
// namespace).
type adminRecords struct{ u *User }

var _ model.AdminKV = (*adminRecords)(nil)

// AdminKV opens the admin store with the configured index mode; Close
// releases it.
func (s *Store) AdminKV() (*AdminKV, error) {
	u, err := s.User(adminNamespace)
	if err != nil {
		return nil, err
	}

	s.mu.RLock()
	mode, shards := s.cfg.KVIndex, s.cfg.KVQueueShards
	s.mu.RUnlock()

	recs := &adminRecords{u: u}
	b := &radosOmap{ioctx: u.idx, key: u.store.indexKey(adminNamespace), keysVer: func(oid string, keys []string) (map[string][]byte, uint64, error) {
		return u.getKeysVer(u.idx, oid, keys)
	}, waker: queueWaker{u}}

	st := kvindex.New(recs, kvindex.Options{Mode: mode, QueueShards: shards, Backend: b, Log: s.log.With("component", "kvindex"),
		Lock: func(name string, ttl time.Duration) (model.Lease, error) {
			return s.holdLock(adminNamespace, sortedindex.LockOID(name), "worker", name+" worker", ttl, true)
		}})

	return &AdminKV{Store: st, recs: recs}, nil
}

// Close releases the store.
func (kv *AdminKV) Close() { kv.recs.u.Close() }

// Keys lists the keys of every record (one listing of the namespace, for
// the backfill).
func (kv *AdminKV) Keys() ([]string, error) {
	it, err := listObjects(kv.recs.u.idx, listAdminBackfill)
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
	pool := kv.u.poolFor(kv.u.idx)

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

	return op.Operate(kv.u.idx, adminOID(key), rados.OperationNoFlag)
}

// List implements model.AdminKV. It lists the namespace (admin state is
// small) and reads the selected records.
func (kv *adminRecords) List(prefix, after string, limit int) ([]model.KVEntry, error) {
	it, err := listObjects(kv.u.idx, listAdminKV)
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
