package main

import (
	"errors"
	"fmt"
	"log/slog"
	"time"

	"shardpost/internal/backfill"
	"shardpost/internal/cephstore"
)

// backfillLockTTL is the lifetime of the backfill's cls_lock without
// renewal (it is renewed every third of it).
const backfillLockTTL = time.Minute

// backfillAccounts runs "shardpost index backfill accounts": it enters
// every account with a registry into the account directory and repairs
// entries a failed purge left behind, under the directory's cls_lock.
// It is run after a rollout of the version that writes the directory,
// once no older server is left.
func backfillAccounts(store *cephstore.Store, log *slog.Logger, restart bool) error {
	lease, err := store.LockAccountIndex(backfillLockTTL)
	if errors.Is(err, cephstore.ErrLocked) {
		return errors.New("another account backfill is running")
	}

	if err != nil {
		return fmt.Errorf("backfill lock: %w", err)
	}

	defer lease.Release()

	kv, err := store.AdminKV()
	if err != nil {
		return err
	}
	defer kv.Close()

	rep, err := backfill.Accounts(store, kv, backfill.Options{
		Lost: lease.Lost, Restart: restart, Cluster: store.Cluster(), Log: log.With("component", "backfill"),
	})
	if err != nil {
		return fmt.Errorf("account backfill: %w", err)
	}

	log.Info("account backfill finished", "started", rep.Started, "namespaces", rep.Namespaces, "accounts", rep.Accounts,
		"added", rep.Added, "repaired", rep.Repaired, "resumed", rep.Resumed)

	return nil
}
