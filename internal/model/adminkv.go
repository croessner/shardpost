package model

import (
	"errors"
	"net/url"
)

// ErrNotFound is returned by AdminKV.Get for an absent key.
var ErrNotFound = errors.New("not found")

// AccountRecordKeys are the keys of the admin store records that belong
// to one account ("<prefix><escaped account>"): its JMAP push
// subscriptions, the state last pushed, its JMAP submissions and the
// record of the fts_due worker (FTSDueRecordKey). A purge of the account
// deletes them (scaling plan B9, stage B14b); jmap checks that its keys
// are among the ones named here.
func AccountRecordKeys(account string) []string {
	esc := url.PathEscape(account)

	return []string{"jmap/push/acct/" + esc, "jmap/push/seen/" + esc, "jmap/submission/" + esc, FTSDueRecordKey(account)}
}

// KVEntry is one record of the admin store.
type KVEntry struct {
	Key   string
	Value []byte
}

// AdminKV is a small store for admin state (jobs, plans) that every
// server of the cluster shares. Each key is updated atomically on its
// own; keys do not contend with each other.
type AdminKV interface {
	// Get returns the value of key or ErrNotFound.
	Get(key string) ([]byte, error)
	// Update replaces the value of key with fn(current) atomically
	// (current is nil when absent; fn returning nil deletes the key). An
	// error from fn aborts without a write and is returned.
	Update(key string, fn func(cur []byte) ([]byte, error)) error
	// List returns up to limit records whose keys start with prefix and
	// sort after the given key, in key order.
	List(prefix, after string, limit int) ([]KVEntry, error)
}

// ReservedNamespace reports whether a name belongs to the store itself
// ("~admin", "~learning", PublicNamespace) rather than to an account.
func ReservedNamespace(name string) bool { return len(name) > 0 && name[0] == '~' }

// PublicNamespace holds the public folders: mailboxes that belong to no
// account. It has no login, no user database entry and no INBOX; access
// is granted by ACL entries alone, and the store counts its usage
// separately from any account.
const PublicNamespace = "~public"

// IsPublic reports whether name addresses the public namespace.
func IsPublic(name string) bool { return name == PublicNamespace }

// MaxAdminValue bounds one admin record (a job with its report, a plan).
const MaxAdminValue = 4 << 20

// ErrOmapConflict is returned by AdminOmap.Write when the object's
// version is no longer the one the write was guarded with, or when an
// exclusive create met an existing object.
var ErrOmapConflict = errors.New("object changed concurrently")

// OmapAnyVersion makes AdminOmap.Write unconditional.
const OmapAnyVersion = ^uint64(0)

// AdminOmap is plain omap access to single objects of the admin
// namespace, with watch and notify, for small shared state that is not
// a record of the admin store: the auth cache flush log, for one. The
// RADOS implementation follows key rotations, so a holder may keep it
// for the process lifetime.
type AdminOmap interface {
	// ReadKeys returns the values of the keys that exist and the
	// object's version (0: the object does not exist).
	ReadKeys(oid string, keys []string) (map[string][]byte, uint64, error)
	// ReadPage returns up to limit keys after startAfter in key order
	// and whether more follow; an absent object reads as empty.
	ReadPage(oid, startAfter string, limit int) ([]KVEntry, bool, error)
	// Write sets and removes keys. ver 0 creates the object exclusively,
	// OmapAnyVersion writes unconditionally, any other version asserts
	// it. A failed guard returns ErrOmapConflict.
	Write(oid string, ver uint64, set map[string][]byte, rm []string) error
	// Watch watches oid; it creates the object when it does not exist.
	Watch(oid string) (Watch, error)
	// Notify wakes the watchers of oid (best effort).
	Notify(oid string)
}
