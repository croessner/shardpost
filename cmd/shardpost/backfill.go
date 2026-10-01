package main

import (
	"errors"
	"fmt"
	"log/slog"
	"time"

	"shardpost/internal/backfill"
	"shardpost/internal/cephstore"
	"shardpost/internal/config"
	"shardpost/internal/model"
)

// backfillLockTTL is the lifetime of the backfill's cls_lock without
// renewal (it is renewed every third of it).
const backfillLockTTL = time.Minute

// backfillCommand returns the backfill of an "index backfill" command.
func backfillCommand(command string) (func(*cephstore.Store, *slog.Logger, *config.Config) error, bool) {
	switch command {
	case commandBackfillAccounts, commandBackfillRestart:
		return func(store *cephstore.Store, log *slog.Logger, _ *config.Config) error {
			return backfillAccounts(store, log, command == commandBackfillRestart)
		}, true
	case commandBackfillKV, commandBackfillKVRestart:
		return func(store *cephstore.Store, log *slog.Logger, settings *config.Config) error {
			return backfillKV(store, log, settings, command == commandBackfillKVRestart)
		}, true
	default:
		return nil, false
	}
}

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

// backfillKV runs "shardpost index backfill kv": it fills the hint
// indexes and the due queues of the admin store from its records, under
// the backfill's cls_lock. It is run after a rollout of the version that
// writes them, once no older server is left; its completion marker is
// what admin.kv_index=read relies on.
func backfillKV(store *cephstore.Store, log *slog.Logger, settings *config.Config, restart bool) error {
	if model.KVIndexMode(settings.Admin.KVIndex) == model.KVIndexOff {
		return errors.New("admin.kv_index is off: the indexes are not kept, a backfill would go stale")
	}

	lease, err := store.LockKVBackfill(backfillLockTTL)
	if errors.Is(err, cephstore.ErrLocked) {
		return errors.New("another admin store backfill is running")
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

	rep, err := backfill.KV(kv, backfill.KVOptions{
		Lost: lease.Lost, Restart: restart, Log: log.With("component", "backfill"),
		Retention: time.Duration(settings.Jobs.RetentionDays) * 24 * time.Hour,
	})
	if err != nil {
		return fmt.Errorf("admin store backfill: %w", err)
	}

	log.Info("admin store backfill finished", "started", rep.Started, "records", rep.Records, "indexed", rep.Indexed,
		"queued", rep.Queued, "resumed", rep.Resumed)

	return nil
}
