package cephstore

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"shardpost/internal/model"
)

// Purged accounts (scaling plan B9). A purge without listing deletes what
// the account's data model names; what servers before 0.42.0 left without
// an intent stays in a namespace that then has neither a registry nor a
// directory entry, which the sweep cannot tell from an account that only
// ever kept Sieve scripts. Every purge therefore records the account in a
// hint index before it deletes anything:
//
//	index pool, namespace "~accounts":
//	  "ix.purged/head|<nn>"   "<account>" -> {"at": unix seconds}
//
// The sweep deletes the objects of a namespace without registry that is
// recorded here, and when it ends it removes the records older than its
// start minus its grace period: whatever such a namespace held, it has
// looked at it. An account created again under the name has a registry
// and is swept like any other.

// purgedRecord is the value of an entry.
type purgedRecord struct {
	At int64 `json:"at"`
}

// PurgedIndex opens the index of purged accounts; release closes it.
func (s *Store) PurgedIndex() (model.SortedIndex, func(), error) {
	return s.openIndex(model.AccountsNamespace, model.PurgedIndexName, model.PurgedIndexShards)
}

// markPurged records the account as purged (purges of names outside the
// directory are not recorded).
func (u *User) markPurged() error {
	if u.store == nil || !model.InDirectory(u.name) {
		return nil
	}

	ix, release, err := u.store.PurgedIndex()
	if err != nil {
		return fmt.Errorf("purged accounts: %w", err)
	}
	defer release()

	if err := ix.Set(u.name, encode(purgedRecord{At: time.Now().Unix()})); err != nil {
		return fmt.Errorf("purged accounts: %w", err)
	}

	return nil
}

// purged reports whether the account is recorded as purged.
func (s *Store) purged(account string) (bool, error) {
	if !model.InDirectory(account) {
		return false, nil
	}

	ix, release, err := s.PurgedIndex()
	if err != nil {
		return false, err
	}
	defer release()

	switch _, err := ix.Get(account); {
	case errors.Is(err, model.ErrNotFound):
		return false, nil
	case err != nil:
		return false, err
	}

	return true, nil
}

// forgetPurged removes the records older than before and returns how many.
func (s *Store) forgetPurged(before time.Time) (int, error) {
	ix, release, err := s.PurgedIndex()
	if err != nil {
		return 0, err
	}
	defer release()

	n, cursor := 0, ""

	for {
		entries, next, err := ix.List(cursor, 500)
		if err != nil {
			return n, err
		}

		for _, e := range entries {
			var r purgedRecord
			if json.Unmarshal(e.Value, &r) == nil && r.At >= before.Unix() {
				continue
			}

			if err := ix.Remove(e.ID); err != nil {
				return n, err
			}

			n++
		}

		if next == "" {
			return n, nil
		}

		cursor = next
	}
}
