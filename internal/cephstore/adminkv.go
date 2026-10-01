package cephstore

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"syscall"

	"github.com/ceph/go-ceph/rados"

	"shardpost/internal/model"
)

// adminNamespace holds admin state (jobs, plans) in the index pool. Its
// name cannot be an account (model.ReservedNamespace).
const adminNamespace = "~admin"

// maxAdminValue bounds one admin record (a job with its report).
const maxAdminValue = model.MaxAdminValue

// AdminKV implements model.AdminKV with one RADOS object per key, so
// updates of different keys (heartbeats of different jobs) never
// contend; each update is a compare-and-swap on the object version.
type AdminKV struct{ u *User }

var _ model.AdminKV = (*AdminKV)(nil)

// AdminKV opens the admin store; Close releases it.
func (s *Store) AdminKV() (*AdminKV, error) {
	u, err := s.User(adminNamespace)
	if err != nil {
		return nil, err
	}

	return &AdminKV{u: u}, nil
}

// Close releases the store.
func (kv *AdminKV) Close() { kv.u.Close() }

func adminOID(key string) string { return "kv." + key }

// read returns the value and object version (0 and nil when absent).
// The version is only meaningful on a context nobody else uses between
// the read and the query, so the read takes a private one (verpool.go).
func (kv *AdminKV) read(key string) ([]byte, uint64, error) {
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
func (kv *AdminKV) Get(key string) ([]byte, error) {
	v, _, err := kv.read(key)
	if err == nil && v == nil {
		return nil, model.ErrNotFound
	}

	return v, err
}

// Update implements model.AdminKV.
func (kv *AdminKV) Update(key string, fn func(cur []byte) ([]byte, error)) error {
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
func (kv *AdminKV) write(key string, ver uint64, exists bool, next []byte) error {
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
func (kv *AdminKV) List(prefix, after string, limit int) ([]model.KVEntry, error) {
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
