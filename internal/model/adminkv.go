package model

import "errors"

// ErrNotFound is returned by AdminKV.Get for an absent key.
var ErrNotFound = errors.New("not found")

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
