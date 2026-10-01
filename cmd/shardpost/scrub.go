package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"shardpost/internal/cephstore"
	"shardpost/internal/config"
	"shardpost/internal/jobs"
	"shardpost/internal/model"
	"shardpost/internal/sortedindex"
)

// scrub walks all users (or SCRUB_USERS) and repairs what a crash may
// have left behind. SCRUB_DRY_RUN=true only reports. Without SCRUB_USERS
// the accounts come from the account directory once its backfill has
// completed; before that from a listing of the index pool.
func scrub(store *cephstore.Store, log *slog.Logger, settings *config.Config) error {
	opts := cephstore.ScrubOptions{
		Grace:  time.Duration(settings.Scrub.GraceSeconds) * time.Second,
		DryRun: settings.Scrub.DryRun,
		Log:    log,
	}
	users, failed := 0, 0

	err := scrubTargets(store, log, settings, func(name string) {
		users++

		u, err := store.User(name)
		if err != nil {
			log.Error("scrub: open user", "user", name, "err", err)

			failed++

			return
		}

		if _, err := u.Scrub(opts); err != nil {
			log.Error("scrub failed", "user", name, "err", err)

			failed++
		}

		u.Close()
	})
	if err != nil {
		return err
	}

	log.Info("scrub finished", "users", users, "failed", failed, "dryRun", opts.DryRun)

	if failed > 0 {
		return fmt.Errorf("%d users failed", failed)
	}

	return nil
}

// scrubTargets calls fn for every account to scrub: SCRUB_USERS, else the
// account directory, else (no completed directory backfill) every
// namespace of a listing.
func scrubTargets(store *cephstore.Store, log *slog.Logger, settings *config.Config, fn func(string)) error {
	if users := strings.TrimSpace(settings.Scrub.Users); users != "" {
		for _, name := range strings.Split(users, ",") {
			if name = strings.TrimSpace(name); name != "" {
				fn(name)
			}
		}

		return nil
	}

	ix, release, err := store.AccountIndex()
	if err != nil {
		return fmt.Errorf("account directory: %w", err)
	}
	defer release()

	if done, err := sortedindex.Backfilled(ix, model.AccountBackfillMarker); err != nil || !done {
		log.Warn("scrub: the account directory has no completed backfill, listing the index pool instead", "err", err)

		users, err := store.ListUsers()
		if err != nil {
			return fmt.Errorf("list users: %w", err)
		}

		for _, name := range users {
			fn(name)
		}

		return nil
	}

	return walkDirectory(ix, store, fn)
}

// walkDirectory calls fn for every account of the directory. An entry
// left purging without a registry is no account any more.
func walkDirectory(ix model.SortedIndex, dir model.AccountDirectory, fn func(string)) error {
	cursor := ""

	for {
		entries, next, err := ix.List(cursor, 500)
		if err != nil {
			return fmt.Errorf("account directory: %w", err)
		}

		for _, e := range entries {
			if entry, err := model.DecodeAccountEntry(e.Value); err == nil && entry.State == model.AccountPurging {
				if reg, err := dir.HasRegistry(e.ID); err != nil || !reg {
					continue
				}
			}

			fn(e.ID)
		}

		if next == "" {
			return nil
		}

		cursor = next
	}
}

// scrubBatch runs "shardpost scrub --batch": it plans and submits the
// scrub of every account as a batch job (with skip_idle; SCRUB_DRY_RUN
// makes it a batch of index_check) and exits; the servers run it. A batch
// scrub that has not ended yet is not submitted twice. Without a
// completed backfill of the account directory it falls back to the scrub
// run in this process over a listing of the index pool.
func scrubBatch(store *cephstore.Store, log *slog.Logger, settings *config.Config) error {
	if strings.TrimSpace(settings.Scrub.Users) != "" {
		return errors.New("scrub --batch covers every account; SCRUB_USERS is for the scrub run in this process")
	}

	ix, release, err := store.AccountIndex()
	if err != nil {
		return fmt.Errorf("account directory: %w", err)
	}

	done, err := sortedindex.Backfilled(ix, model.AccountBackfillMarker)

	release()

	if err != nil || !done {
		log.Warn("scrub --batch: the account directory has no completed backfill, scrubbing here instead", "err", err)
		return scrub(store, log, settings)
	}

	kv, err := store.AdminKV()
	if err != nil {
		return fmt.Errorf("admin store: %w", err)
	}
	defer kv.Close()

	instance, _ := os.Hostname()
	m := jobs.NewManager(kv, &jobs.Env{Accounts: importStore{store}, Directory: store, Log: log},
		jobs.Config{PlanTTL: time.Duration(settings.Jobs.PlanTTLSeconds) * time.Second, ScrubParallel: settings.Scrub.Parallel,
			BatchParallel: settings.Jobs.BatchParallel},
		instance, jobs.Kinds(time.Duration(settings.Scrub.GraceSeconds)*time.Second)...)

	p := jobs.BatchParams{Kind: jobs.KindScrub, SkipIdle: true}
	if settings.Scrub.DryRun {
		p = jobs.BatchParams{Kind: jobs.KindIndexCheck}
	}

	return submitScrubBatch(m, log, p, "shardpost scrub --batch@"+instance)
}

// scrubSweep runs "shardpost scrub --sweep": it lists both pools in every
// namespace and reports what no account's data model names; with
// --repair (and without scrub.dry_run) it deletes what is older than
// scrub.grace_seconds. A sweep that stopped goes on from its checkpoint
// in the cluster unless --restart is given. The report goes to standard
// output as one line of JSON, after the log lines.
func scrubSweep(ctx context.Context, store *cephstore.Store, log *slog.Logger, settings *config.Config, repair, restart bool) error {
	rep, err := store.Sweep(ctx, cephstore.SweepOptions{
		Repair: repair && !settings.Scrub.DryRun, Restart: restart, Log: log,
		Grace:       time.Duration(settings.Scrub.GraceSeconds) * time.Second,
		SpillShards: settings.Scrub.SweepSpillShards,
	})
	if rep != nil {
		b, _ := json.Marshal(rep)
		fmt.Println(string(b))
	}

	return err
}

// submitScrubBatch submits the batch unless one of the same kind has not
// ended yet.
func submitScrubBatch(m *jobs.Manager, log *slog.Logger, p jobs.BatchParams, principal string) error {
	j, created, err := m.SubmitBatchOnce(p, principal)
	if err != nil {
		return err
	}

	if !created {
		log.Info("scrub --batch: the previous batch has not ended, nothing submitted", "job", j.ID)
		return nil
	}

	log.Info("scrub --batch: batch submitted", "event", "scrub_batch_submitted", "job", j.ID, "kind", p.Kind, "skip_idle", p.SkipIdle)

	return nil
}
