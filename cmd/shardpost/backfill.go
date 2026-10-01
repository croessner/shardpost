package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"time"

	"shardpost/internal/backfill"
	"shardpost/internal/cephstore"
	"shardpost/internal/config"
	"shardpost/internal/model"
)

// backfillLockTTL is the lifetime of the backfill's cls_lock without
// renewal. It is renewed every third of it; a renewal that waits out the
// OSD op timeout (60 s) still lands before the lock runs out, because
// TTL/3 + 60 s < TTL.
const backfillLockTTL = 3 * time.Minute

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
	case commandBackfillLearning, commandBackfillLearningRestart:
		return func(store *cephstore.Store, log *slog.Logger, _ *config.Config) error {
			return backfillLearning(store, log, command == commandBackfillLearningRestart)
		}, true
	}

	if rest, ok := strings.CutPrefix(command, commandShowKV); ok {
		return func(store *cephstore.Store, _ *slog.Logger, _ *config.Config) error {
			return showKV(store, os.Stdout, strings.Fields(rest))
		}, true
	}

	if rest, ok := strings.CutPrefix(command, commandShowLearning); ok {
		return func(store *cephstore.Store, _ *slog.Logger, _ *config.Config) error {
			return showLearning(store, os.Stdout, strings.Fields(rest))
		}, true
	}

	return nil, false
}

// showLearning runs "shardpost index show learning [<id>...]": it prints
// the index mode, the backfill marker, the size of the due queue and the
// owner index and, for each job id, the record and its index entries, as
// JSON. It only reads.
func showLearning(store *cephstore.Store, w io.Writer, ids []string) error {
	q, err := store.LearningQueue()
	if err != nil {
		return err
	}

	out, err := backfill.InspectLearning(q, ids, time.Now())
	if err != nil {
		return fmt.Errorf("learning queue: %w", err)
	}

	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")

	return enc.Encode(out)
}

// backfillLearning runs "shardpost index backfill learning": it fills
// the learning queue's due queue and owner index from its records (one
// listing of the namespace "~learning" of the data pool), under the
// backfill's cls_lock. It is run after a rollout of the version that
// writes them, once no older server is left; its completion marker is
// what imapsieve.learning.index=read relies on.
func backfillLearning(store *cephstore.Store, log *slog.Logger, restart bool) error {
	q, err := store.LearningQueue()
	if err != nil {
		return err
	}

	lease, err := store.LockLearningBackfill(backfillLockTTL)
	if errors.Is(err, cephstore.ErrLocked) {
		return errors.New("another learning queue backfill is running")
	}

	if err != nil {
		return fmt.Errorf("backfill lock: %w", err)
	}

	defer lease.Release()

	rep, err := backfill.Learning(q, backfill.LearningOptions{Lost: lease.Lost, Restart: restart, Log: log.With("component", "backfill")})
	if err != nil {
		return fmt.Errorf("learning queue backfill: %w", err)
	}

	log.Info("learning queue backfill finished", "started", rep.Started, "jobs", rep.Jobs, "resumed", rep.Resumed)

	return nil
}

// showKV runs "shardpost index show kv [<key>...]": it prints the index
// mode, the backfill marker, the size of the due queues and, for each
// key (a job brings its idempotency key along), the record, its hint
// index entry and its queue entries, as JSON. It only reads.
func showKV(store *cephstore.Store, w io.Writer, keys []string) error {
	kv, err := store.AdminKV()
	if err != nil {
		return err
	}
	defer kv.Close()

	out, err := backfill.InspectKV(kv, keys, time.Now())
	if err != nil {
		return fmt.Errorf("admin store: %w", err)
	}

	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")

	return enc.Encode(out)
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
