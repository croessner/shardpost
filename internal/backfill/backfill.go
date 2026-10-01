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
	MarkerAccounts = model.AccountBackfillMarker
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
	// RegistryStatter returns a function that reports whether an
	// account's registry exists and its last modification (zero when
	// unknown). It is used from one goroutine only; release closes it.
	RegistryStatter() (stat func(account string) (time.Time, bool, error), release func(), err error)
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
	// Repaired counts the entries the repair rules looked at, by action.
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
	// Grace is the age below which an active entry without registry is
	// left alone (model.DefaultDirectoryGrace when 0).
	Grace time.Duration
	Log   *slog.Logger
	Now   func() time.Time
}

// ErrLockLost stops a backfill whose lock could not be renewed.
var ErrLockLost = errors.New("backfill lock lost")

// run is one account backfill.
type run struct {
	ix   model.SortedIndex
	src  AccountSource
	kv   model.AdminKV
	stat func(account string) (time.Time, bool, error)
	o    Options
	rep  Report
	// known holds the accounts whose registry this run found.
	known map[string]bool
}

// Accounts enters every account with a registry into the account
// directory (state active, created = the registry's last change) and
// applies the repair rules to every entry (kv is the job store that tells
// whether a purge still runs). It lists the pool once, can be repeated
// (existing entries stay as they are) and resumes after an
// interruption; the caller holds the backfill lock.
func Accounts(src AccountSource, kv model.AdminKV, o Options) (Report, error) {
	o = o.withDefaults()

	ix, release, err := src.AccountIndex()
	if err != nil {
		return Report{}, err
	}
	defer release()

	stat, closeStat, err := src.RegistryStatter()
	if err != nil {
		return Report{}, err
	}
	defer closeStat()

	r := &run{ix: ix, src: src, kv: kv, stat: stat, o: o, known: map[string]bool{},
		rep: Report{Started: o.Now().UTC(), Repaired: map[string]int{}}}

	err = r.do()

	return r.rep, err
}

func (r *run) do() error {
	pos, err := r.resumePosition()
	if err != nil {
		return err
	}

	names, err := r.src.ListUsers()
	if err != nil {
		return fmt.Errorf("list namespaces: %w", err)
	}

	if err := r.enter(names, pos); err != nil {
		return err
	}

	if err := r.repairAll(); err != nil {
		return err
	}

	if r.o.Lost() {
		return ErrLockLost
	}

	r.rep.Finished = r.o.Now().UTC()
	b, _ := json.Marshal(r.rep)

	if err := r.ix.SetMarker(MarkerAccounts, string(b)); err != nil {
		return err
	}

	return r.ix.SetMarker(markerProgress, "")
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
func (r *run) resumePosition() (*progress, error) {
	pos := &progress{}

	h, err := r.ix.Head()
	if err != nil {
		return pos, err
	}

	if v := h.Markers[markerProgress]; v != "" && !r.o.Restart && json.Unmarshal([]byte(v), pos) == nil {
		r.rep.Started, r.rep.Resumed = pos.Started, true
		r.o.Log.Info("backfill resumes", "after", pos.After, "started", pos.Started)
	}

	return pos, nil
}

// enter writes the missing entries of the listed namespaces in name
// order, from the resume position on.
func (r *run) enter(names []string, pos *progress) error {
	pos.Started = r.rep.Started

	for i, name := range names {
		if !model.InDirectory(name) || model.CheckNamespace(name) != nil {
			continue
		}

		r.rep.Namespaces++

		if name <= pos.After {
			continue
		}

		if err := r.enterOne(name); err != nil {
			return err
		}

		pos.After = name

		if (i+1)%progressEvery == 0 {
			b, _ := json.Marshal(pos)
			if err := r.ix.SetMarker(markerProgress, string(b)); err != nil {
				return err
			}

			r.o.Log.Info("backfill progress", "after", name, "added", r.rep.Added)
		}
	}

	return nil
}

// enterOne writes the entry of one namespace if it has a registry and no
// entry.
func (r *run) enterOne(name string) error {
	mtime, ok, err := r.stat(name)
	if err != nil {
		return fmt.Errorf("registry of %q: %w", name, err)
	}

	if !ok {
		return nil
	}

	r.rep.Accounts++
	r.known[name] = true

	if r.o.Lost() {
		return ErrLockLost
	}

	added, err := addEntry(r.ix, name, r.o.Cluster, mtime, r.o.Now())
	if err != nil {
		return fmt.Errorf("directory entry of %q: %w", name, err)
	}

	if added {
		r.rep.Added++
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

// repairAll applies the repair rules to every entry: purging ones, and
// active ones whose registry this run did not find (stale entries).
func (r *run) repairAll() error {
	cursor := ""

	for {
		page, next, err := r.ix.List(cursor, 1000)
		if err != nil {
			return err
		}

		for _, e := range page {
			if err := r.repair(e); err != nil {
				return err
			}
		}

		if next == "" {
			return nil
		}

		cursor = next
	}
}

func (r *run) repair(e model.IndexEntry) error {
	ent, err := model.DecodeAccountEntry(e.Value)
	if err != nil || (ent.State == model.AccountActive && r.known[e.ID]) {
		return nil //nolint:nilerr // an unreadable entry is replaced by the next registration
	}

	if r.o.Lost() {
		return ErrLockLost
	}

	res, err := model.RepairAccountEntry(r.ix, e.ID, model.RepairChecks{
		HasRegistry: func() (bool, error) {
			_, ok, err := r.stat(e.ID)
			return ok, err
		},
		PurgeRunning: func(session string) (bool, error) { return jobs.PurgeRunning(r.kv, session) },
		Now:          r.o.Now(),
		Grace:        r.o.Grace,
	})
	if err != nil {
		return fmt.Errorf("repair %q: %w", e.ID, err)
	}

	jobs.ForgetActivated(r.src, res)

	r.rep.Repaired[res.Action]++

	if res.Action != model.RepairNone {
		r.o.Log.Info("backfill repair", "account", e.ID, "action", res.Action)
	}

	return nil
}
