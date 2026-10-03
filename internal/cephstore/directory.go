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

// indexKey identifies namespace ns of its index pool (indexPoolOf) in
// the process-wide head cache of sorted indexes.
func (s *Store) indexKey(ns string) string {
	return s.indexPoolOf(ns) + "/" + ns
}

// ReadPage implements sortedindex.Backend.
func (b *radosOmap) ReadPage(oid, startAfter string, limit int) ([]sortedindex.KV, bool, error) {
	defer mh.timer("index_page")()

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

	err := timedOp("index_write", func() error { return op.Operate(b.ioctx, oid, rados.OperationNoFlag) })

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

// openIndex opens the sorted index name in namespace ns of its index
// pool (indexPoolOf); release closes it.
func (s *Store) openIndex(ns, name string, shards int) (*sortedindex.Index, func(), error) {
	b, release, err := s.openOmap(ns)
	if err != nil {
		return nil, nil, err
	}

	return sortedindex.New(b, name, shards), release, nil
}

// openOmap opens the omap backend of namespace ns of its index pool
// (indexPoolOf) on a private context; release closes it.
func (s *Store) openOmap(ns string) (*radosOmap, func(), error) {
	r := s.ref()
	pool := s.indexPoolOf(ns)

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

	return &radosOmap{ioctx: ioctx, key: pool + "/" + ns}, release, nil
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
// release closes it. Only accounts and the public namespace have a
// registry; the store's own namespaces are refused (they need not even
// live in the index pool).
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

		if !model.InDirectory(account) {
			return time.Time{}, false, fmt.Errorf("%w: %q has no registry", model.ErrNamespace, account)
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
//
// A lock taken without onRotate stands through a rotation; its holder
// moves it to the new connection with Retake (model.LeaseRetaker) when
// the rotation hook (Store.OnRotate) tells it, and until then it renews
// through the old connection, whose key is still valid for a while. The
// push worker's partition leases are taken that way (stage B18a), so
// that a rotation does not stop its watchers.
func (s *Store) holdLock(ns, oid, name, desc string, ttl time.Duration, onRotate bool) (model.Lease, error) {
	u, err := s.User(ns)
	if err != nil {
		return nil, err
	}

	l, err := s.holdLockOn(u, oid, name, desc, ttl, onRotate)
	if err != nil {
		return nil, err
	}

	l.reopen = func() (*User, error) { return s.User(ns) }

	return l, nil
}

// holdLockIn is holdLock on namespace ns of pool, whatever pool
// indexPoolOf names for ns: the sweep keeps its lock in the index pool
// while the rest of "~accounts" may live in the system pool.
func (s *Store) holdLockIn(pool, ns, oid, name, desc string, ttl time.Duration, onRotate bool) (model.Lease, error) {
	u, err := s.userIn(ns, pool)
	if err != nil {
		return nil, err
	}

	return s.holdLockOn(u, oid, name, desc, ttl, onRotate)
}

// holdLockOn is holdLock on the index context of view u, which the lease
// closes when it is released (or replaces, on Retake).
func (s *Store) holdLockOn(u *User, oid, name, desc string, ttl time.Duration, onRotate bool) (*lease, error) {
	l := &lease{stop: make(chan struct{}), u: u, oid: oid, name: name, desc: desc, ttl: ttl, cookie: newGUID()}

	sent := time.Now()
	if err := l.take(nil); err != nil {
		u.Close()
		return nil, err
	}

	l.setValidUntil(sent)
	u.keepLock(l, ttl/3, l.take)

	l.unlock = func() {
		l.unlockOn(l.view())

		if onRotate {
			s.forgetHeld(l)
		}
	}

	if onRotate {
		s.rememberHeld(l)
	}

	return l, nil
}

// take takes or renews (flags) the lock through the lease's current
// view and cookie.
func (l *lease) take(flags *byte) error {
	u, cookie := l.view()

	ret, err := u.idx.LockExclusive(l.oid, l.name, cookie, l.desc, l.ttl, flags)
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

// view is the view and the cookie the lease holds its lock with.
func (l *lease) view() (*User, string) {
	l.mu.Lock()
	defer l.mu.Unlock()

	return l.u, l.cookie
}

// unlockOn unlocks through u with cookie and closes u.
func (l *lease) unlockOn(u *User, cookie string) {
	start := time.Now()
	_, _ = u.idx.Unlock(l.oid, l.name, cookie)
	l.observe("release", start)
	u.Close()
}

// setValidUntil records a take or renewal that was sent at sent and
// succeeded.
func (l *lease) setValidUntil(sent time.Time) {
	until := sent.Add(l.ttl)
	l.validUntil.Store(&until)
}

// ValidUntil implements model.LeaseDeadline: the time the last
// successful take or renewal was sent plus the TTL (zero for a maildrop
// lock, which has no renewal of that kind).
func (l *lease) ValidUntil() time.Time {
	if t := l.validUntil.Load(); t != nil {
		return *t
	}

	return time.Time{}
}

// observe reports the duration of a lock operation to the lease's
// observer, if it has one.
func (l *lease) observe(op string, start time.Time) {
	if l.observer != nil {
		l.observer(op, time.Since(start))
	}
}

// Retake implements model.LeaseRetaker: the renewal is stopped, the lock
// is unlocked through the view it was taken with (the old connection),
// a new view is opened on the current connection, the lock is taken
// there with a new cookie, and the renewal goes on through it. The old
// view is closed, so the old connection loses its reference and can be
// shut down. An error (another holder was quicker in the gap, or the
// new view cannot be opened) marks the lease lost; what is held through
// the old connection has been unlocked by then.
func (l *lease) Retake() error {
	if l.reopen == nil {
		return errors.New("the lease cannot be retaken")
	}

	l.opMu.Lock()
	defer l.opMu.Unlock()

	l.mu.Lock()
	if l.released {
		l.mu.Unlock()

		return errors.New("the lease is released")
	}

	stop, done := l.stop, l.done
	l.stop = make(chan struct{})
	l.mu.Unlock()

	close(stop)

	if done != nil {
		<-done
	}

	old, oldCookie := l.view()

	nu, err := l.reopen()
	if err != nil {
		l.unlockOn(old, oldCookie)
		l.lost.Store(true)

		return err
	}

	l.mu.Lock()
	l.u, l.cookie = nu, newGUID()
	l.mu.Unlock()

	// The old lock goes before the new one is taken: the same holder
	// may not hold a cls_lock under two cookies.
	l.unlockOn(old, oldCookie)

	sent := time.Now()
	if err := l.take(nil); err != nil {
		l.observe("take", sent)
		l.lost.Store(true)
		nu.Close()

		return err
	}

	l.observe("take", sent)
	l.setValidUntil(sent)
	nu.keepLock(l, l.ttl/3, l.take)

	return nil
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
// lease is released; a failed renewal marks the lease lost. The time a
// renewal was sent is taken before the call, so that the deadline the
// lease reports (ValidUntil) is never later than the lock's expiry in
// the cluster.
func (u *User) keepLock(l *lease, every time.Duration, take func(flags *byte) error) {
	u.inflight.Add(1)

	l.mu.Lock()
	stop := l.stop
	done := make(chan struct{})
	l.done = done
	l.mu.Unlock()

	go func() {
		defer u.inflight.Done()
		defer close(done)

		renew := byte(1) // LIBRADOS_LOCK_FLAG_MAY_RENEW

		t := time.NewTicker(every)
		defer t.Stop()

		for {
			select {
			case <-stop:
				return
			case <-t.C:
				sent := time.Now()
				err := take(&renew)
				l.observe("renew", sent)

				if err != nil {
					u.log.Warn("lock renewal failed", "err", err)
					l.lost.Store(true)

					return
				}

				l.setValidUntil(sent)
			}
		}
	}()
}

// OnRotate registers fn to run after every key rotation (Store.Rotate),
// once the admin store and User() hand out the new connection; the
// returned function removes it. A holder of leases that stand through
// a rotation (LockQueueKeep) retakes them in it.
func (s *Store) OnRotate(fn func()) func() {
	s.heldMu.Lock()
	defer s.heldMu.Unlock()

	if s.rotateHooks == nil {
		s.rotateHooks = map[int]func(){}
	}

	s.rotateHook++
	id := s.rotateHook
	s.rotateHooks[id] = fn

	return func() {
		s.heldMu.Lock()
		defer s.heldMu.Unlock()

		delete(s.rotateHooks, id)
	}
}

// runRotateHooks runs the rotation hooks (Store.Rotate).
func (s *Store) runRotateHooks() {
	s.heldMu.Lock()
	hooks := make([]func(), 0, len(s.rotateHooks))

	for _, fn := range s.rotateHooks {
		hooks = append(hooks, fn)
	}
	s.heldMu.Unlock()

	for _, fn := range hooks {
		fn()
	}
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
