package cephstore

import (
	"errors"
	"fmt"
	"syscall"
	"time"

	"github.com/ceph/go-ceph/rados"

	"shardpost/internal/model"
	"shardpost/internal/sortedindex"
)

// Sorted indexes and the account directory (scaling plan B1, B4).
//
//	index pool, namespace "~accounts":
//	  "ix.accounts/head"   omap: "shards", "format", "phase", "mark:<name>"
//	  "ix.accounts/<nn>"   omap: "<account>" -> model.AccountEntry JSON
//
// An account is registered when its registry object is created (the
// hook in casOmapOpAs), so the directory costs one read and one write
// per account lifetime.

// radosOmap is the sortedindex.Backend of one namespace. Without keysVer
// its context is private (opened for the index), so the version a read
// reports is the one of that read; a shared context (the admin store's)
// reads versions through keysVer, which takes a private context of the
// pool (verpool.go).
type radosOmap struct {
	ioctx   *rados.IOContext
	key     string
	keysVer func(oid string, keys []string) (map[string][]byte, uint64, error)
}

var (
	_ sortedindex.Backend = (*radosOmap)(nil)
)

// CacheKey implements sortedindex.Backend.
func (b *radosOmap) CacheKey() string { return b.key }

// ReadKeys implements sortedindex.Backend.
func (b *radosOmap) ReadKeys(oid string, keys []string) (map[string][]byte, uint64, error) {
	if b.keysVer != nil {
		return b.keysVer(oid, keys)
	}

	return getOmapKeysVer(b.ioctx, oid, keys)
}

// indexKey identifies namespace ns of the index pool in the process-wide
// head cache of sorted indexes.
func (s *Store) indexKey(ns string) string {
	s.mu.RLock()
	defer s.mu.RUnlock()

	return s.cfg.IndexPool + "/" + ns
}

// ReadPage implements sortedindex.Backend.
func (b *radosOmap) ReadPage(oid, startAfter string, limit int) ([]sortedindex.KV, bool, error) {
	op := rados.CreateReadOp()
	defer op.Release()

	step := op.GetOmapValues(startAfter, "", uint64(limit))
	if err := op.Operate(b.ioctx, oid, rados.OperationNoFlag); err != nil {
		if errCode(err) == syscall.ENOENT {
			return nil, false, nil
		}

		return nil, false, err
	}

	var out []sortedindex.KV

	for {
		kv, err := step.Next()
		if err != nil {
			return nil, false, err
		}

		if kv == nil {
			return out, step.More(), nil
		}

		out = append(out, sortedindex.KV{Key: kv.Key, Value: append([]byte(nil), kv.Value...)})
	}
}

// Write implements sortedindex.Backend.
func (b *radosOmap) Write(oid string, ver uint64, set map[string][]byte, rm []string) error {
	op := rados.CreateWriteOp()
	defer op.Release()

	switch {
	case ver == 0:
		op.Create(rados.CreateExclusive)
	case ver != sortedindex.AnyVersion:
		op.AssertVersion(ver)
	case len(set) > 0:
		op.Create(rados.CreateIdempotent)
	}

	if len(set) > 0 {
		op.SetOmap(set)
	}

	if len(rm) > 0 {
		op.RmOmapKeys(rm)
	}

	err := op.Operate(b.ioctx, oid, rados.OperationNoFlag)

	switch errCode(err) {
	case 0:
		return err
	case syscall.ERANGE, syscall.EOVERFLOW, syscall.EEXIST:
		mh.casRetry("index", casClassIndex)
		return sortedindex.ErrConflict
	case syscall.ENOENT:
		if ver == sortedindex.AnyVersion && len(set) == 0 {
			return nil // nothing to remove from an absent shard
		}

		return sortedindex.ErrConflict
	default:
		return err
	}
}

// openIndex opens the sorted index name in namespace ns of the index
// pool; release closes it.
func (s *Store) openIndex(ns, name string, shards int) (*sortedindex.Index, func(), error) {
	r := s.ref()

	s.mu.RLock()
	pool := s.cfg.IndexPool
	s.mu.RUnlock()

	ioctx, err := r.conn.OpenIOContext(pool)
	if err != nil {
		r.refs.Done()
		return nil, nil, fmt.Errorf("open index pool %q: %w", pool, err)
	}

	ioctx.SetNamespace(ns)

	release := func() {
		ioctx.Destroy()
		r.refs.Done()
	}

	return sortedindex.New(&radosOmap{ioctx: ioctx, key: pool + "/" + ns}, name, shards), release, nil
}

var (
	_ model.AccountDirectory = (*Store)(nil)
	_ model.DirectoryPurger  = (*User)(nil)
)

// AccountIndex implements model.AccountDirectory.
func (s *Store) AccountIndex() (model.SortedIndex, func(), error) {
	return s.openIndex(model.AccountsNamespace, model.AccountIndexName, model.AccountIndexShards)
}

// HasRegistry implements model.AccountDirectory.
func (s *Store) HasRegistry(account string) (bool, error) {
	stat, release, err := s.RegistryStatter()
	if err != nil {
		return false, err
	}
	defer release()

	_, ok, err := stat(account)

	return ok, err
}

// RegistryStatter returns a function that reports whether an account's
// registry exists and when it was last modified (the backfill takes it
// as the creation time). It uses one index-pool context, switching its
// namespace per call, so it must not be shared between goroutines;
// release closes it.
func (s *Store) RegistryStatter() (func(account string) (time.Time, bool, error), func(), error) {
	r := s.ref()

	s.mu.RLock()
	pool := s.cfg.IndexPool
	s.mu.RUnlock()

	ioctx, err := r.conn.OpenIOContext(pool)
	if err != nil {
		r.refs.Done()
		return nil, nil, fmt.Errorf("open index pool %q: %w", pool, err)
	}

	release := func() {
		ioctx.Destroy()
		r.refs.Done()
	}

	stat := func(account string) (time.Time, bool, error) {
		if err := model.CheckNamespace(account); err != nil {
			return time.Time{}, false, err
		}

		ioctx.SetNamespace(account)

		st, err := ioctx.Stat(oidRegistry)
		if errCode(err) == syscall.ENOENT {
			return time.Time{}, false, nil
		}

		if err != nil {
			return time.Time{}, false, err
		}

		return st.ModTime, true, nil
	}

	return stat, release, nil
}

// LockAccountIndex takes the exclusive cls_lock of the account
// directory's maintenance object ("ix.accounts/lock" in "~accounts") and
// renews it every ttl/3 until Release. ErrLocked when another holder
// has it. The lock is advisory: the backfill checks Lost before each
// write, and every write it does is safe to repeat.
func (s *Store) LockAccountIndex(ttl time.Duration) (model.Lease, error) {
	return s.holdLock(model.AccountsNamespace, sortedindex.LockOID(model.AccountIndexName), "backfill", "account directory backfill", ttl, false)
}

// LockKVBackfill takes the exclusive cls_lock of the admin store backfill
// ("ix.kv/lock" in "~admin") like LockAccountIndex.
func (s *Store) LockKVBackfill(ttl time.Duration) (model.Lease, error) {
	return s.holdLock(adminNamespace, sortedindex.LockOID("kv"), "backfill", "admin store backfill", ttl, false)
}

// holdLock takes the exclusive cls_lock name on oid in namespace ns and
// renews it (LIBRADOS_LOCK_FLAG_MAY_RENEW) every ttl/3 until Release;
// ErrLocked when another holder has it. The lock is advisory: its holder
// fences every mutation by data and checks Lost before it.
//
// A lock taken with onRotate is given up when the store rotates its key
// (Store.Rotate): it is unlocked through the handle it was taken with,
// before that connection's key is withdrawn, and reports Lost, so its
// holder takes it again on the new connection. Without that the lock
// would stay held until its TTL ran out.
func (s *Store) holdLock(ns, oid, name, desc string, ttl time.Duration, onRotate bool) (model.Lease, error) {
	u, err := s.User(ns)
	if err != nil {
		return nil, err
	}

	cookie := newGUID()

	take := func(flags *byte) error {
		ret, err := u.idx.LockExclusive(oid, name, cookie, desc, ttl, flags)
		if err != nil {
			return err
		}

		if ret != 0 {
			if syscall.Errno(-ret) == syscall.EBUSY || syscall.Errno(-ret) == syscall.EEXIST {
				return ErrLocked
			}

			return fmt.Errorf("lock: ret=%d", ret)
		}

		return nil
	}

	if err := take(nil); err != nil {
		u.Close()
		return nil, err
	}

	l := &lease{stop: make(chan struct{})}
	u.keepLock(l, ttl/3, take)

	l.unlock = func() {
		_, _ = u.idx.Unlock(oid, name, cookie)
		u.Close()

		if onRotate {
			s.forgetHeld(l)
		}
	}

	if onRotate {
		s.rememberHeld(l)
	}

	return l, nil
}

// rememberHeld and forgetHeld track the locks Rotate gives up.
func (s *Store) rememberHeld(l *lease) {
	s.heldMu.Lock()
	defer s.heldMu.Unlock()

	if s.held == nil {
		s.held = map[*lease]struct{}{}
	}

	s.held[l] = struct{}{}
}

func (s *Store) forgetHeld(l *lease) {
	s.heldMu.Lock()
	defer s.heldMu.Unlock()

	delete(s.held, l)
}

// releaseHeld gives up the tracked locks through their (old) handles and
// marks them lost.
func (s *Store) releaseHeld() {
	s.heldMu.Lock()
	held := make([]*lease, 0, len(s.held))

	for l := range s.held {
		held = append(held, l)
	}
	s.heldMu.Unlock()

	for _, l := range held {
		l.lost.Store(true)
		l.Release()
	}

	if len(held) > 0 {
		s.log.Info("worker locks released after key rotation", "locks", len(held))
	}
}

// keepLock renews a cls_lock taken with take every interval until the
// lease is released; a failed renewal marks the lease lost.
func (u *User) keepLock(l *lease, every time.Duration, take func(flags *byte) error) {
	u.inflight.Add(1)

	l.done = make(chan struct{})

	go func() {
		defer u.inflight.Done()
		defer close(l.done)

		renew := byte(1) // LIBRADOS_LOCK_FLAG_MAY_RENEW

		t := time.NewTicker(every)
		defer t.Stop()

		for {
			select {
			case <-l.stop:
				return
			case <-t.C:
				if err := take(&renew); err != nil {
					u.log.Warn("lock renewal failed", "err", err)
					l.lost.Store(true)

					return
				}
			}
		}
	}()
}

// Cluster is the fsid the directory records for new entries.
func (s *Store) Cluster() string {
	s.mu.RLock()
	defer s.mu.RUnlock()

	return s.fsid
}

// registerAccount is the directory hook of a registry creation (see
// casOmapOpAs). session is the import lock session of the CAS, if it is
// one.
func (u *User) registerAccount(session string) error {
	if u.store == nil || !model.InDirectory(u.name) {
		return nil
	}

	ix, release, err := u.store.AccountIndex()
	if err != nil {
		return fmt.Errorf("account directory: %w", err)
	}
	defer release()

	err = model.RegisterAccount(ix, u.name, session, u.store.Cluster(), time.Now())
	if err != nil && !errors.Is(err, model.ErrImportLocked) {
		return fmt.Errorf("account directory: %w", err)
	}

	if err == nil {
		// An account created again is no purged one any more.
		if ferr := u.store.ForgetPurged(u.name); ferr != nil {
			u.log.Warn("purged account record not removed", "err", ferr)
		}
	}

	return err
}

// grantBlocked reports whether a grant to identifier must be refused
// because the grantee is being purged (model.GrantBlocked). Only grants
// that write a pointer into the grantee's registry are checked.
func (u *User) grantBlocked(identifier string) (bool, error) {
	if u.store == nil || identifier == identAnyone || model.IsPublic(u.name) || model.CheckNamespace(identifier) != nil {
		return false, nil
	}

	ix, release, err := u.store.AccountIndex()
	if err != nil {
		return false, fmt.Errorf("account directory: %w", err)
	}
	defer release()

	return model.GrantBlocked(ix, identifier, func() (bool, error) { return u.store.HasRegistry(identifier) })
}

// MarkPurging implements model.DirectoryPurger.
func (u *User) MarkPurging(session string) error {
	if u.store == nil {
		return nil
	}

	ix, release, err := u.store.AccountIndex()
	if err != nil {
		return fmt.Errorf("account directory: %w", err)
	}
	defer release()

	return model.MarkAccountPurging(ix, u.name, session, u.store.Cluster(), time.Now())
}

// PurgingSession implements model.DirectoryPurger.
func (u *User) PurgingSession() (string, bool, error) {
	if u.store == nil {
		return "", false, nil
	}

	ix, release, err := u.store.AccountIndex()
	if err != nil {
		return "", false, fmt.Errorf("account directory: %w", err)
	}
	defer release()

	return model.PurgingSession(ix, u.name)
}

// FinishPurge implements model.DirectoryPurger.
func (u *User) FinishPurge() error {
	if err := u.idx.Delete(oidRegistry); err != nil && errCode(err) != syscall.ENOENT {
		return fmt.Errorf("delete registry: %w", err)
	}

	u.invalidate()

	if u.store == nil || !model.InDirectory(u.name) {
		return nil
	}

	ix, release, err := u.store.AccountIndex()
	if err != nil {
		return fmt.Errorf("account directory: %w", err)
	}
	defer release()

	return ix.Remove(u.name)
}
