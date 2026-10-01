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
	// AccountBackfillMarker is the head marker of the last completed
	// account backfill.
	AccountBackfillMarker = "backfill.accounts"
)

// Directory states.
const (
	AccountActive  = "active"
	AccountPurging = "purging"
)

// AccountEntry is the directory value of one account.
type AccountEntry struct {
	// Created is when the registry was last created (the registry's
	// mtime for accounts a backfill entered). Concurrent creators may
	// race; the last one wins, it is only a time. A stale-entry repair
	// relies on every creation writing it anew (repairStale).
	Created time.Time `json:"created"`
	// State is AccountActive or AccountPurging.
	State string `json:"state"`
	// Session is the maintenance lock session of the purge job while
	// the state is AccountPurging.
	Session string `json:"session,omitempty"`
	// Cluster is the Ceph cluster (fsid) that holds the account.
	Cluster string `json:"cluster"`
	// Shard is reserved for a later stage (placement); empty for now.
	// Scrub is the record of the last batch scrub (AccountScrub, see
	// scrubstate.go). A registration rewrites the whole entry (Cluster
	// of the creating store, Shard and Scrub empty): it only happens
	// while the registry is missing, so the account is new.
	Shard string          `json:"shard"`
	Scrub json.RawMessage `json:"scrub,omitempty"`
}

// ErrAccountPurging refuses the creation of an account's registry, or a
// grant to it, while the account is being purged. It is an
// ErrImportLocked, so every protocol answers "try again later".
var ErrAccountPurging = fmt.Errorf("%w: the account is being purged", ErrImportLocked)

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
	// PurgingSession returns the lock session of the purge that marked
	// the account, when its entry is purging.
	PurgingSession() (string, bool, error)
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
// absent or active entry is written as active with created = now; an
// entry in AccountPurging refuses with ErrAccountPurging unless session
// is the lock session of the purge (the restarted purge job taking its
// lock again). Names outside the directory are ignored.
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

				return nil, ErrAccountPurging
			}

			// An active entry is written again with the time of this
			// creation: a stale-entry repair that read it before compares
			// the time and leaves the entry alone (repairStale). An
			// unreadable entry is replaced: it must not keep the account
			// from being created.
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

// PurgingSession returns the lock session of the purge that marked the
// account, when its entry is purging.
func PurgingSession(ix SortedIndex, account string) (string, bool, error) {
	e, ok, err := GetAccountEntry(ix, account)
	if err != nil || !ok || e.State != AccountPurging {
		return "", false, err
	}

	return e.Session, true, nil
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
	RepairStale     = "stale_removed" // active, old, without registry: entry removed
)

// DirectoryRepair is the outcome of RepairAccountEntry.
type DirectoryRepair struct {
	Account string        `json:"account"`
	Action  string        `json:"action"`
	Found   bool          `json:"found"`
	Before  *AccountEntry `json:"before,omitempty"`
	After   *AccountEntry `json:"after,omitempty"`
}

// DefaultDirectoryGrace is how old an active entry without registry
// must be before a repair removes it: the registry hook writes the entry
// right before it creates the registry, so a young entry may belong to
// an account being created.
const DefaultDirectoryGrace = 10 * time.Minute

// RepairChecks are what RepairAccountEntry asks outside the directory.
type RepairChecks struct {
	// HasRegistry reports whether the account's registry exists.
	HasRegistry func() (bool, error)
	// PurgeRunning reports whether the job of a lock session still runs.
	PurgeRunning func(session string) (bool, error)
	// Now and Grace (DefaultDirectoryGrace when 0) decide whether an
	// active entry without registry is stale.
	Now   time.Time
	Grace time.Duration
}

// RepairAccountEntry applies the repair rules to one entry:
//
//   - purging, and no purge job of its session still runs: without
//     registry the entry is removed, with registry it is active again;
//   - active, older than the grace and without registry: the entry is
//     stale and removed (a purge of an older version, a crash between
//     the hook and the registry creation, a creation that failed).
func RepairAccountEntry(ix SortedIndex, account string, c RepairChecks) (DirectoryRepair, error) {
	out := DirectoryRepair{Account: account, Action: RepairNone}

	e, ok, err := GetAccountEntry(ix, account)
	if err != nil || !ok {
		return out, err
	}

	out.Found, out.Before, out.After = true, &e, &e

	if e.State != AccountPurging {
		return repairStale(ix, account, e, c, out)
	}

	running, err := c.PurgeRunning(e.Session)
	if err != nil {
		return out, err
	}

	if running {
		out.Action = RepairRunning
		return out, nil
	}

	// While the entry is purging and the registry is missing, only the
	// purge's own session can create the registry, and its job has
	// ended: the answer stays valid until the update below.
	reg, err := c.HasRegistry()
	if err != nil {
		return out, err
	}

	err = ix.Update(account, func(cur []byte) ([]byte, error) { return repairUpdate(cur, e.Session, reg, &out) })

	return out, err
}

// repairStale removes an active entry that is older than the grace and
// has no registry, with a compare-and-swap on the entry as it was read.
// A creation of the account writes the entry (RegisterAccount, with a
// new created time) before it creates the registry, so whichever of the
// two writes lands first, the entry exists afterwards: if the hook wrote
// first, the created time differs and the repair leaves the entry; if
// the repair removed it first, the hook finds no entry and writes one.
func repairStale(ix SortedIndex, account string, e AccountEntry, c RepairChecks, out DirectoryRepair) (DirectoryRepair, error) {
	grace := c.Grace
	if grace <= 0 {
		grace = DefaultDirectoryGrace
	}

	if c.Now.Sub(e.Created) < grace {
		return out, nil
	}

	if reg, err := c.HasRegistry(); err != nil || reg {
		return out, err
	}

	err := ix.Update(account, func(cur []byte) ([]byte, error) {
		out.Action, out.After = RepairNone, nil

		cur2, err := DecodeAccountEntry(cur)
		if cur != nil && err == nil {
			out.After = &cur2
		}

		if cur == nil || err != nil || cur2.State != AccountActive || !cur2.Created.Equal(e.Created) {
			return cur, nil // changed meanwhile: leave it
		}

		out.Action, out.After = RepairStale, nil

		return nil, nil
	})

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
