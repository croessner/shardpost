// Package backfill fills store-wide indexes from the data they index,
// after a rollout that started writing them (scaling plan, guideline 4:
// write first, backfill after the rollout, then switch the readers).
//
// A backfill is the one place where listing a pool is allowed: it runs
// rarely, by hand, under a lock, and can be repeated and resumed.
package backfill

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"shardpost/internal/jobs"
	"shardpost/internal/model"
)

// Markers in the head of the account directory.
const (
	// MarkerAccounts records the last completed backfill (Report as
	// JSON). Only a backfill that started after the last server without
	// the directory hook stopped makes the directory complete.
	MarkerAccounts = "backfill.accounts"
	// markerProgress records the position of a running backfill ("" when
	// none runs), so that an interrupted one resumes.
	markerProgress = "backfill.accounts.progress"
)

// progressEvery is how many accounts a backfill handles between two
// progress markers.
const progressEvery = 500

// AccountSource is what the account backfill needs from the store.
type AccountSource interface {
	model.AccountDirectory
	// ListUsers lists every namespace of the index pool (one pool
	// listing).
	ListUsers() ([]string, error)
	// RegistryTime reports whether the account's registry exists and its
	// last modification (zero when unknown).
	RegistryTime(account string) (time.Time, bool, error)
}

// Report is the result of an account backfill.
type Report struct {
	// Started is when the backfill began (for a resumed one, when its
	// first run began).
	Started  time.Time `json:"started"`
	Finished time.Time `json:"finished,omitzero"`
	// Namespaces is the number of namespaces listed, Accounts those with
	// a registry, Added the entries written.
	Namespaces int `json:"namespaces"`
	Accounts   int `json:"accounts"`
	Added      int `json:"added"`
	// Repaired counts the entries the repair rules changed, by action.
	Repaired map[string]int `json:"repaired"`
	// Resumed: the run continued an interrupted one.
	Resumed bool `json:"resumed,omitempty"`
}

type progress struct {
	Started time.Time `json:"started"`
	After   string    `json:"after"`
}

// Options of an account backfill.
type Options struct {
	// Lost reports that the backfill's lock could not be renewed; the
	// backfill stops before its next write. nil: never.
	Lost func() bool
	// Restart ignores the position of an interrupted run.
	Restart bool
	// Cluster is recorded in the entries the backfill writes.
	Cluster string
	Log     *slog.Logger
	Now     func() time.Time
}

// ErrLockLost stops a backfill whose lock could not be renewed.
var ErrLockLost = errors.New("backfill lock lost")

// Accounts enters every account with a registry into the account
// directory (state active, created = the registry's last change) and
// applies the repair rules to every entry left purging (kv is the job
// store that tells whether a purge still runs). It lists the pool once,
// can be repeated (existing entries stay as they are) and resumes after
// an interruption; the caller holds the backfill lock.
func Accounts(src AccountSource, kv model.AdminKV, o Options) (Report, error) {
	o = o.withDefaults()

	ix, release, err := src.AccountIndex()
	if err != nil {
		return Report{}, err
	}
	defer release()

	rep := Report{Started: o.Now().UTC(), Repaired: map[string]int{}}

	pos, err := resumePosition(ix, &rep, o)
	if err != nil {
		return rep, err
	}

	names, err := src.ListUsers()
	if err != nil {
		return rep, fmt.Errorf("list namespaces: %w", err)
	}

	if err := enter(ix, src, names, pos, &rep, o); err != nil {
		return rep, err
	}

	if err := repairAll(ix, src, kv, &rep, o); err != nil {
		return rep, err
	}

	if o.Lost() {
		return rep, ErrLockLost
	}

	rep.Finished = o.Now().UTC()
	b, _ := json.Marshal(rep)

	if err := ix.SetMarker(MarkerAccounts, string(b)); err != nil {
		return rep, err
	}

	return rep, ix.SetMarker(markerProgress, "")
}

func (o Options) withDefaults() Options {
	if o.Now == nil {
		o.Now = time.Now
	}

	if o.Log == nil {
		o.Log = slog.New(slog.DiscardHandler)
	}

	if o.Lost == nil {
		o.Lost = func() bool { return false }
	}

	return o
}

// resumePosition returns the position of an interrupted run (unless
// o.Restart) and takes over its start time.
func resumePosition(ix model.SortedIndex, rep *Report, o Options) (*progress, error) {
	pos := &progress{}

	h, err := ix.Head()
	if err != nil {
		return pos, err
	}

	if v := h.Markers[markerProgress]; v != "" && !o.Restart && json.Unmarshal([]byte(v), pos) == nil {
		rep.Started, rep.Resumed = pos.Started, true
		o.Log.Info("backfill resumes", "after", pos.After, "started", pos.Started)
	}

	return pos, nil
}

// enter writes the missing entries of the listed namespaces in name
// order, from the resume position on.
func enter(ix model.SortedIndex, src AccountSource, names []string, pos *progress, rep *Report, o Options) error {
	pos.Started = rep.Started

	for i, name := range names {
		if !model.InDirectory(name) || model.CheckNamespace(name) != nil {
			continue
		}

		rep.Namespaces++

		if name <= pos.After {
			continue
		}

		mtime, ok, err := src.RegistryTime(name)
		if err != nil {
			return fmt.Errorf("registry of %q: %w", name, err)
		}

		if ok {
			rep.Accounts++

			if o.Lost() {
				return ErrLockLost
			}

			added, err := addEntry(ix, name, o.Cluster, mtime, o.Now())
			if err != nil {
				return fmt.Errorf("directory entry of %q: %w", name, err)
			}

			if added {
				rep.Added++
			}
		}

		pos.After = name

		if (i+1)%progressEvery == 0 {
			b, _ := json.Marshal(pos)
			if err := ix.SetMarker(markerProgress, string(b)); err != nil {
				return err
			}

			o.Log.Info("backfill progress", "after", name, "added", rep.Added)
		}
	}

	return nil
}

// addEntry writes an active entry unless there is one.
func addEntry(ix model.SortedIndex, name, cluster string, created, now time.Time) (bool, error) {
	if created.IsZero() {
		created = now
	}

	added := false

	err := ix.Update(name, func(cur []byte) ([]byte, error) {
		if cur != nil {
			added = false
			return cur, nil
		}

		added = true

		return json.Marshal(model.AccountEntry{Created: created.UTC(), State: model.AccountActive, Cluster: cluster})
	})

	return added, err
}

// repairAll applies the repair rules to every entry that is purging.
func repairAll(ix model.SortedIndex, src AccountSource, kv model.AdminKV, rep *Report, o Options) error {
	cursor := ""

	for {
		page, next, err := ix.List(cursor, 1000)
		if err != nil {
			return err
		}

		for _, e := range page {
			if ent, err := model.DecodeAccountEntry(e.Value); err != nil || ent.State != model.AccountPurging {
				continue
			}

			if o.Lost() {
				return ErrLockLost
			}

			r, err := jobs.RepairAccountEntry(kv, src, e.ID)
			if err != nil {
				return fmt.Errorf("repair %q: %w", e.ID, err)
			}

			rep.Repaired[r.Action]++
			o.Log.Info("backfill repair", "account", e.ID, "action", r.Action)
		}

		if next == "" {
			return nil
		}

		cursor = next
	}
}
