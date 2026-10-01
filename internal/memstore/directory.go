package memstore

import (
	"slices"
	"sync"
	"time"

	"shardpost/internal/model"
	"shardpost/internal/sortedindex"
)

// The account directory and sorted indexes, mirroring cephstore (see
// cephstore/directory.go). This store has no registry object; a user's
// "registry" exists from the first mutation that creates it in the RADOS
// store (mailbox creation, delivery, quota limits, subscriptions, ACL
// changes and shared pointers, the import lock, the trace switch) until
// the purge deletes it. Opening a user registers nothing.

var (
	_ model.AccountDirectory = (*Store)(nil)
	_ model.DirectoryPurger  = (*User)(nil)
)

// indexes holds the index backends of the store, one per namespace.
type indexes struct {
	mu sync.Mutex
	ns map[string]*sortedindex.MemBackend
}

// IndexBackend returns the index objects of a namespace (tests).
func (s *Store) IndexBackend(ns string) *sortedindex.MemBackend {
	s.ix.mu.Lock()
	defer s.ix.mu.Unlock()

	if s.ix.ns == nil {
		s.ix.ns = map[string]*sortedindex.MemBackend{}
	}

	b, ok := s.ix.ns[ns]
	if !ok {
		b = sortedindex.NewMemBackend()
		s.ix.ns[ns] = b
	}

	return b
}

// AccountIndex implements model.AccountDirectory.
func (s *Store) AccountIndex() (model.SortedIndex, func(), error) {
	return sortedindex.New(s.IndexBackend(model.AccountsNamespace), model.AccountIndexName, model.AccountIndexShards), func() {}, nil
}

// HasRegistry implements model.AccountDirectory.
func (s *Store) HasRegistry(account string) (bool, error) {
	s.mu.Lock()
	u, ok := s.users[account]
	s.mu.Unlock()

	return ok && u.registered.Load(), nil
}

// ListUsers lists the namespaces of the store like cephstore: accounts
// and the public namespace, sorted.
func (s *Store) ListUsers() ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	var out []string

	for name := range s.users {
		if model.InDirectory(name) {
			out = append(out, name)
		}
	}

	slices.Sort(out)

	return out, nil
}

// RegistryTime reports whether the user has a registry; this store
// keeps no modification time.
func (s *Store) RegistryTime(account string) (time.Time, bool, error) {
	ok, err := s.HasRegistry(account)
	return time.Time{}, ok, err
}

// register is the directory hook of a registry creation: it runs on the
// mutations that create the registry in the RADOS store, as long as the
// user has none. session is the import lock session of an ImportLock.
func (u *User) register(session string) error {
	if u.registered.Load() {
		return nil
	}

	if model.InDirectory(u.name) {
		ix, release, _ := u.store.AccountIndex()
		defer release()

		if err := model.RegisterAccount(ix, u.name, session, "", time.Now()); err != nil {
			return err
		}
	}

	u.registered.Store(true)

	return nil
}

// grantBlocked mirrors cephstore: a grant to an account under purge is
// refused.
func (u *User) grantBlocked(identifier string) (bool, error) {
	if identifier == identAnyone || model.IsPublic(u.name) || model.CheckNamespace(identifier) != nil {
		return false, nil
	}

	ix, release, _ := u.store.AccountIndex()
	defer release()

	return model.GrantBlocked(ix, identifier, func() (bool, error) { return u.store.HasRegistry(identifier) })
}

// MarkPurging implements model.DirectoryPurger.
func (u *User) MarkPurging(session string) error {
	ix, release, _ := u.store.AccountIndex()
	defer release()

	return model.MarkAccountPurging(ix, u.name, session, "", time.Now())
}

// FinishPurge implements model.DirectoryPurger.
func (u *User) FinishPurge() error {
	u.mu.Lock()
	u.imp = nil
	u.mu.Unlock()

	u.registered.Store(false)

	if !model.InDirectory(u.name) {
		return nil
	}

	ix, release, _ := u.store.AccountIndex()
	defer release()

	return ix.Remove(u.name)
}
