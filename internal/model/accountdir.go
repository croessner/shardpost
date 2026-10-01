package model

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// The account directory (scaling plan B4) is a SortedIndex of every
// account the store holds: an account is in it exactly when its registry
// object exists. The store registers an account when it creates the
// registry (once per account lifetime), the purge job marks it purging
// before it deletes the namespace and removes the entry as its last
// phase. Enumerations of accounts read the directory instead of listing
// the pool.

// Namespaces the store uses for itself (ReservedNamespace). They are no
// accounts and are never registered in the directory.
const (
	AdminNamespace    = "~admin"
	LearningNamespace = "~learning"
	ServerNamespace   = "~server"
	AccountsNamespace = "~accounts"
)

// The directory index.
const (
	AccountIndexName   = "accounts"
	AccountIndexShards = 256
)

// Directory states.
const (
	AccountActive  = "active"
	AccountPurging = "purging"
)

// AccountEntry is the directory value of one account.
type AccountEntry struct {
	// Created is when the entry was written (the registry's mtime for
	// accounts a backfill entered). Concurrent first writers may race;
	// the last one wins, it is only a time.
	Created time.Time `json:"created"`
	// State is AccountActive or AccountPurging.
	State string `json:"state"`
	// Session is the maintenance lock session of the purge job while
	// the state is AccountPurging.
	Session string `json:"session,omitempty"`
	// Cluster is the Ceph cluster (fsid) that holds the account.
	Cluster string `json:"cluster"`
	// Shard and Scrub are reserved for later stages (placement, scrub
	// state); empty for now.
	Shard string          `json:"shard"`
	Scrub json.RawMessage `json:"scrub,omitempty"`
}

// ErrNamespace rejects a namespace that is no account and no namespace
// of the store itself (the empty name, an unknown "~" name).
var ErrNamespace = errors.New("invalid account namespace")

// CheckNamespace reports whether a store view may be opened for name:
// accounts, the public namespace and the store's own namespaces. The
// empty name would address the default namespace, where a registry
// object would belong to nobody.
func CheckNamespace(name string) error {
	switch {
	case name == "":
		return fmt.Errorf("%w: empty name", ErrNamespace)
	case !ReservedNamespace(name):
		return nil
	}

	switch name {
	case PublicNamespace, ServerNamespace, AdminNamespace, LearningNamespace, AccountsNamespace:
		return nil
	default:
		return fmt.Errorf("%w: %q is reserved", ErrNamespace, name)
	}
}

// InDirectory reports whether name is registered in the account
// directory once it has a registry: accounts and the public namespace,
// never the store's own namespaces.
func InDirectory(name string) bool {
	return name != "" && (!ReservedNamespace(name) || IsPublic(name))
}

// AccountDirectory is implemented by stores that keep the account
// directory.
type AccountDirectory interface {
	// AccountIndex opens the directory; the caller calls close.
	AccountIndex() (SortedIndex, func(), error)
	// HasRegistry reports whether the account's registry object exists.
	HasRegistry(account string) (bool, error)
}

// DirectoryPurger is implemented by user views of a store with an
// account directory: the purge job marks the account before it deletes
// the namespace and removes the entry at its end.
type DirectoryPurger interface {
	// MarkPurging sets the entry to AccountPurging under the job's lock
	// session. From then on the registry cannot be created again while
	// it is missing, except by an ImportLock of that session.
	MarkPurging(session string) error
	// FinishPurge deletes the registry once more (a restarted job's
	// ImportLock creates an empty one) and then removes the entry.
	FinishPurge() error
}

// DecodeAccountEntry decodes a directory value.
func DecodeAccountEntry(b []byte) (AccountEntry, error) {
	var e AccountEntry

	err := json.Unmarshal(b, &e)

	return e, err
}

// GetAccountEntry reads the entry of an account; false when absent.
func GetAccountEntry(ix SortedIndex, account string) (AccountEntry, bool, error) {
	b, err := ix.Get(account)
	if errors.Is(err, ErrNotFound) {
		return AccountEntry{}, false, nil
	}

	if err != nil {
		return AccountEntry{}, false, err
	}

	e, err := DecodeAccountEntry(b)
	if err != nil {
		return AccountEntry{}, false, fmt.Errorf("directory entry of %q: %w", account, err)
	}

	return e, true, nil
}

// RegisterAccount is the directory hook of a registry creation: an
// absent entry is written as active, an entry in AccountPurging refuses
// with ErrImportLocked unless session is the lock session of the purge
// (the restarted purge job taking its lock again). Names outside the
// directory are ignored.
func RegisterAccount(ix SortedIndex, account, session, cluster string, now time.Time) error {
	if !InDirectory(account) {
		return nil
	}

	return ix.Update(account, func(cur []byte) ([]byte, error) {
		if cur != nil {
			e, err := DecodeAccountEntry(cur)
			if err == nil && e.State == AccountPurging {
				if session != "" && session == e.Session {
					return cur, nil
				}

				return nil, ErrImportLocked
			}

			if err == nil {
				return cur, nil
			}
			// An unreadable entry is replaced: it must not keep the
			// account from being created.
		}

		return json.Marshal(AccountEntry{Created: now.UTC(), State: AccountActive, Cluster: cluster})
	})
}

// MarkAccountPurging sets the entry of an account to AccountPurging with
// the purge job's lock session (an absent entry is created).
func MarkAccountPurging(ix SortedIndex, account, session, cluster string, now time.Time) error {
	if !InDirectory(account) {
		return nil
	}

	return ix.Update(account, func(cur []byte) ([]byte, error) {
		e, err := DecodeAccountEntry(cur)
		if cur == nil || err != nil {
			e = AccountEntry{Created: now.UTC(), Cluster: cluster}
		}

		e.State, e.Session = AccountPurging, session

		return json.Marshal(e)
	})
}

// GrantBlocked reports whether a grant to account must be refused: the
// account is being purged and its registry is gone, so a pointer write
// would fail and the ACL entry would wait for whoever gets the name
// next. hasRegistry is only asked for a purging account.
func GrantBlocked(ix SortedIndex, account string, hasRegistry func() (bool, error)) (bool, error) {
	if !InDirectory(account) {
		return false, nil
	}

	e, ok, err := GetAccountEntry(ix, account)
	if err != nil || !ok || e.State != AccountPurging {
		return false, err
	}

	reg, err := hasRegistry()

	return !reg, err
}

// Directory repair actions.
const (
	RepairNone      = "none"          // nothing to repair
	RepairRunning   = "purge_running" // a purge job still works on the account
	RepairRemoved   = "removed"       // purging without registry: entry removed
	RepairActivated = "activated"     // purging with registry: back to active
)

// DirectoryRepair is the outcome of RepairAccountEntry.
type DirectoryRepair struct {
	Account string        `json:"account"`
	Action  string        `json:"action"`
	Found   bool          `json:"found"`
	Before  *AccountEntry `json:"before,omitempty"`
	After   *AccountEntry `json:"after,omitempty"`
}

// RepairAccountEntry applies the repair rules to one entry: an account
// left purging without a purge job that still runs loses its entry when
// its registry is gone and becomes active again when the registry
// exists. purgeRunning tells whether the job of a lock session is still
// running.
func RepairAccountEntry(ix SortedIndex, account string, hasRegistry func() (bool, error),
	purgeRunning func(session string) (bool, error),
) (DirectoryRepair, error) {
	out := DirectoryRepair{Account: account, Action: RepairNone}

	e, ok, err := GetAccountEntry(ix, account)
	if err != nil || !ok {
		return out, err
	}

	out.Found, out.Before = true, &e
	if e.State != AccountPurging {
		out.After = &e
		return out, nil
	}

	running, err := purgeRunning(e.Session)
	if err != nil {
		return out, err
	}

	if running {
		out.Action, out.After = RepairRunning, &e
		return out, nil
	}

	// While the entry is purging and the registry is missing, only the
	// purge's own session can create the registry, and its job has
	// ended: the answer stays valid until the update below.
	reg, err := hasRegistry()
	if err != nil {
		return out, err
	}

	err = ix.Update(account, func(cur []byte) ([]byte, error) { return repairUpdate(cur, e.Session, reg, &out) })

	return out, err
}

// repairUpdate is the directory update of a repair: the entry is still
// purging under session, reg tells whether the registry exists.
func repairUpdate(cur []byte, session string, reg bool, out *DirectoryRepair) ([]byte, error) {
	c, err := DecodeAccountEntry(cur)
	if cur == nil || err != nil || c.State != AccountPurging || c.Session != session {
		out.Action, out.After = RepairNone, nil
		if cur != nil && err == nil {
			out.After = &c
		}

		return cur, nil // changed meanwhile: leave it
	}

	if !reg {
		out.Action, out.After = RepairRemoved, nil
		return nil, nil
	}

	c.State, c.Session = AccountActive, ""
	out.Action, out.After = RepairActivated, &c

	return json.Marshal(c)
}
