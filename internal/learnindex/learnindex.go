// Package learnindex is the learning queue with its due queue and owner
// index (scaling plan B5), shared by the RADOS store and the in-memory
// test store.
//
// A learning job is a record of its own: one object "learning/<id>" in
// the namespace "~learning" of the data pool, holding the message with
// the job as an attribute. Finding the jobs that are due used to list
// that namespace every second, and an account purge listed it to find
// the account's jobs. The data pool is erasure coded and has no omap, so
// the indexes live in the namespace "~learning" of the index pool:
//
//   - the due queue "learning" (model.LearningQueue): one entry
//     "<next attempt>/<id>/<gen>" per job, written before its record and
//     removed after it (a completeness index); a dead job sits at
//     DeadDue, after every live entry, so it is never due;
//   - the hint index "learning/owner": one entry "<account>/<id>" per
//     job, written after its record and removed before it.
//
// The worker reads the due entries instead of listing, holds a renewed
// worker lease and is woken by a notify on shard 0 of the queue. Every
// write of a job through the queue counts up its Gen, which is part of
// the queue key: a reader that finds an entry the record does not match
// moves it to the record's position (or removes it, for an entry without
// record, after model.DueQueueGrace).
//
// Modes (imapsieve.learning.index): off writes no index, write keeps the
// indexes while readers still list the namespace, read also reads from
// them. Read mode needs the marker of a completed "shardpost index
// backfill learning"; until it exists the queue behaves as in write mode.
package learnindex

import (
	"errors"
	"fmt"
	"log/slog"
	"math"
	"slices"
	"sort"
	"strings"
	"time"

	"shardpost/internal/imapsieve"
	"shardpost/internal/metrics"
	"shardpost/internal/model"
	"shardpost/internal/sortedindex"
)

// Listing sites of the record layer (label "site" of
// imapceph_rados_listings_total).
const (
	SiteAll      = "learning_all"
	SitePurge    = "learning_purge_owner"
	SiteBackfill = "learning_backfill"
)

// Index names, shard counts and markers.
const (
	// OwnerIndex is the hint index of the jobs by owner.
	OwnerIndex = "learning/owner"
	// OwnerShards is its shard count.
	OwnerShards = 16
	// MarkerBackfill records the last completed backfill in the head of
	// the queue ("ix.learning/head"); read mode waits for it.
	MarkerBackfill = "backfill.learning"
	// MarkerProgress records the position of a running backfill.
	MarkerProgress = "backfill.learning.progress"
	// maxDue bounds the jobs one worker pass handles.
	maxDue = 100
)

// DeadDue is the due time of a dead job: after every live entry, so a
// dead job is never due and the readers of due entries never reach it.
var DeadDue = time.Unix(0, math.MaxInt64).UTC()

// Records is the record layer of the learning queue.
type Records interface {
	// Create stores a new job with its message; false when a job with
	// the ID exists (it is left as it is).
	Create(j imapsieve.Job, raw []byte) (bool, error)
	// Get reads a job; imapsieve.ErrNoJob when it does not exist.
	Get(id string) (imapsieve.Job, error)
	// Set rewrites the job of an existing record; imapsieve.ErrNoJob
	// when it is gone (a deleted job is not recreated).
	Set(j imapsieve.Job) error
	// Remove deletes a job and its message; no error when it is gone.
	Remove(id string) error
	// Read returns the message of a job.
	Read(j imapsieve.Job) ([]byte, error)
	// List returns every job (one listing of the namespace, counted
	// under site).
	List(site string) ([]imapsieve.Job, error)
	// Ready and Cleanup: see imapsieve.Queue.
	Ready(j imapsieve.Job) (bool, error)
	Cleanup(j imapsieve.Job) error
	// Lock takes the worker lease: held and renewed until Release,
	// reported lost when the renewal fails or the store rotates its key.
	Lock() (model.Lease, error)
}

// Options configure a Queue.
type Options struct {
	// Mode is the configured index mode ("" = model.KVIndexWrite).
	Mode model.KVIndexMode
	// QueueShards is the shard count of the due queue when its first
	// writer creates it (model.DefaultQueueShards when 0).
	QueueShards int
	// Backend is the omap access to the namespace "~learning" of the
	// index pool.
	Backend sortedindex.Backend
	// RequireBackfill gates read mode on MarkerBackfill.
	RequireBackfill bool
	// Now is the clock (time.Now when nil).
	Now func() time.Time
	Log *slog.Logger
}

// Queue is the learning queue: imapsieve.Queue for the worker, the
// admin API's view and the purge's.
type Queue struct {
	rec   Records
	o     Options
	q     *sortedindex.Queue
	owner *sortedindex.Index
	gate  *sortedindex.Gate
}

var _ imapsieve.Queue = (*Queue)(nil)

// New returns the queue over the records rec.
func New(rec Records, o Options) *Queue {
	if o.Mode == "" {
		o.Mode = model.KVIndexWrite
	}

	if o.QueueShards <= 0 {
		o.QueueShards = model.DefaultQueueShards
	}

	if o.Now == nil {
		o.Now = time.Now
	}

	if o.Log == nil {
		o.Log = slog.New(slog.DiscardHandler)
	}

	q := &Queue{rec: rec, o: o}

	if o.Backend != nil {
		q.q = sortedindex.NewQueue(o.Backend, model.LearningQueue, o.QueueShards)
		q.owner = sortedindex.New(o.Backend, OwnerIndex, OwnerShards)
	}

	if o.Mode == model.KVIndexRead && o.RequireBackfill && q.q != nil {
		q.gate = &sortedindex.Gate{Head: q.q, Marker: MarkerBackfill, Now: o.Now,
			Closed: func(err error) {
				o.Log.Error("imapsieve.learning.index=read without a completed \"shardpost index backfill learning\": "+
					"behaving as write (listing) until the marker exists", "err", err)
			},
			Opened: func() {
				o.Log.Info("imapsieve.learning.index=read: backfill marker found, readers use the indexes now")
			},
		}
	}

	return q
}

// Mode is the mode the queue behaves in (read only with the backfill
// marker when it is required).
func (q *Queue) Mode() model.KVIndexMode {
	switch {
	case q.q == nil:
		return model.KVIndexOff
	case q.gate != nil && !q.gate.Open():
		return model.KVIndexWrite
	default:
		return q.o.Mode
	}
}

// ConfiguredMode is the configured mode.
func (q *Queue) ConfiguredMode() model.KVIndexMode { return q.o.Mode }

func (q *Queue) writes() bool { return q.q != nil && q.o.Mode != model.KVIndexOff }

func (q *Queue) reads() bool { return q.Mode() == model.KVIndexRead }

// DueQueue is the due queue (backfill, inspection).
func (q *Queue) DueQueue() *sortedindex.Queue { return q.q }

// Owners is the owner index (backfill, inspection).
func (q *Queue) Owners() *sortedindex.Index { return q.owner }

// Records is the record layer (backfill, inspection).
func (q *Queue) Records() Records { return q.rec }

// Position is the due queue position of a job: its next attempt (a job
// without one is due at once) or DeadDue, at its generation.
func Position(j imapsieve.Job) model.DueKey {
	switch {
	case j.Dead:
		return model.DueKey{Due: DeadDue, Gen: j.Gen}
	case j.Next.IsZero() || j.Next.Before(time.Unix(0, 0)):
		return model.DueKey{Due: time.Unix(0, 0).UTC(), Gen: j.Gen}
	default:
		return model.DueKey{Due: j.Next, Gen: j.Gen}
	}
}

// OwnerID is the owner index id of a job.
func OwnerID(j imapsieve.Job) string { return j.Owner + "/" + j.ID }

// Stale queue entries (label "action" of
// imapceph_queue_stale_entries_total).
const (
	staleMoved   = "rescheduled"
	staleRemoved = "removed"
)

func stale(action string) { metrics.QueueStale.WithLabelValues(model.LearningQueue, action).Inc() }

// indexError logs and counts an index write that failed: the record
// stands, readers and the backfill repair the entry.
func (q *Queue) indexError(what, id string, err error) {
	metrics.KVIndexErrors.WithLabelValues(what).Inc()
	q.o.Log.Warn("learning queue: index not written", "index", what, "job", id, "err", err)
}

// move moves the queue entry of id and logs a failure.
func (q *Queue) move(id string, from []model.DueKey, to *model.DueKey) {
	if err := q.q.Move(id, from, to); err != nil {
		q.indexError(model.LearningQueue, id, err)
	}
}

// Put implements imapsieve.Queue: the queue entry first, then the record,
// then the owner entry; a job that is already queued (same ID) is left
// as it is.
func (q *Queue) Put(j imapsieve.Job, raw []byte) error {
	if !q.writes() {
		_, err := q.rec.Create(j, raw)
		return err
	}

	j.Gen = 0
	pos := Position(j)

	if err := q.q.Move(j.ID, nil, &pos); err != nil {
		return fmt.Errorf("learning queue: %w", err)
	}

	created, err := q.rec.Create(j, raw)
	if err != nil {
		q.move(j.ID, []model.DueKey{pos}, nil)
		return err
	}

	if !created {
		// The existing job has its own entry: ours goes unless it is
		// the same.
		cur, err := q.rec.Get(j.ID)

		switch {
		case errors.Is(err, imapsieve.ErrNoJob):
			q.move(j.ID, []model.DueKey{pos}, nil)
		case err == nil && !model.SameDueKey(Position(cur), pos):
			at := Position(cur)
			q.move(j.ID, []model.DueKey{pos}, &at)
		}

		return nil
	}

	if err := q.owner.Set(OwnerID(j), []byte{}); err != nil {
		q.indexError(OwnerIndex, j.ID, err)
	}

	q.q.Notify()

	return nil
}

// Update implements imapsieve.Queue: the record, then its queue entry
// from where the record was to where it is now.
func (q *Queue) Update(j imapsieve.Job) error {
	if !q.writes() {
		return q.rec.Set(j)
	}

	pre, err := q.rec.Get(j.ID)
	if err != nil {
		return err
	}

	j.Gen = pre.Gen + 1

	if err := q.rec.Set(j); err != nil {
		return err
	}

	from, to := Position(pre), Position(j)
	q.move(j.ID, []model.DueKey{from}, &to)

	if to.Due.Before(from.Due) {
		q.q.Notify()
	}

	return nil
}

// Delete implements imapsieve.Queue: the owner entry, the record, then
// the queue entries of the record as it was and as the caller saw it.
func (q *Queue) Delete(j imapsieve.Job) error {
	if !q.writes() {
		return q.rec.Remove(j.ID)
	}

	from := []model.DueKey{Position(j)}

	cur, err := q.rec.Get(j.ID)

	switch {
	case err == nil:
		j.Owner = cur.Owner
		from = append(from, Position(cur))
	case !errors.Is(err, imapsieve.ErrNoJob):
		return err
	}

	if j.Owner != "" {
		if err := q.owner.Remove(OwnerID(j)); err != nil {
			return fmt.Errorf("learning owner index: %w", err)
		}
	}

	if err := q.rec.Remove(j.ID); err != nil {
		return err
	}

	q.move(j.ID, from, nil)

	return nil
}

// Read implements imapsieve.Queue.
func (q *Queue) Read(j imapsieve.Job) ([]byte, error) { return q.rec.Read(j) }

// Ready implements imapsieve.Queue.
func (q *Queue) Ready(j imapsieve.Job) (bool, error) { return q.rec.Ready(j) }

// Cleanup implements imapsieve.Queue.
func (q *Queue) Cleanup(j imapsieve.Job) error { return q.rec.Cleanup(j) }

// Lock implements imapsieve.Queue.
func (q *Queue) Lock() (model.Lease, error) { return q.rec.Lock() }

// Watch implements imapsieve.Watcher: in read mode the worker watches
// shard 0 of the due queue, which Put, Revive and earlier moves notify.
// In the other modes a wake-up would cost a listing, so there is no
// watch (the poll remains).
func (q *Queue) Watch() (model.Watch, error) {
	if !q.reads() {
		return nil, imapsieve.ErrNoWatch
	}

	return q.q.Watch()
}

// List implements imapsieve.Queue: up to 100 live jobs that are due,
// oldest due time first; from the due queue in read mode, else from a
// listing.
func (q *Queue) List() ([]imapsieve.Job, error) {
	if !q.reads() {
		return q.listDue()
	}

	now := q.o.Now()

	due, err := q.q.Due(now, maxDue)
	if err != nil {
		return nil, err
	}

	jobs := make([]imapsieve.Job, 0, len(due))

	for _, e := range due {
		j, ok, err := q.checkEntry(e, now)
		if err != nil {
			return nil, err
		}

		if ok {
			jobs = append(jobs, j)
		}
	}

	return jobs, nil
}

// checkEntry compares a due entry with its record: ok when the record is
// a live job that is due at the entry's position. An entry the record
// does not match is moved to the record's position; an entry without
// record is removed once it is older than model.DueQueueGrace (the
// entry is written before the record).
func (q *Queue) checkEntry(e model.DueEntry, now time.Time) (imapsieve.Job, bool, error) {
	j, err := q.rec.Get(e.ID)

	switch {
	case errors.Is(err, imapsieve.ErrNoJob):
		if now.Sub(e.Due) >= model.DueQueueGrace {
			q.move(e.ID, []model.DueKey{e.DueKey}, nil)
			stale(staleRemoved)
		}

		return j, false, nil
	case err != nil:
		return j, false, err
	}

	pos := Position(j)
	if !model.SameDueKey(pos, e.DueKey) {
		q.move(e.ID, []model.DueKey{e.DueKey}, &pos)
		stale(staleMoved)
	}

	return j, !j.Dead && !now.Before(j.Next), nil
}

// listDue is List from a listing of the namespace.
func (q *Queue) listDue() ([]imapsieve.Job, error) {
	all, err := q.rec.List(SiteAll)
	if err != nil {
		return nil, err
	}

	now := q.o.Now()
	jobs := all[:0]

	for _, j := range all {
		if !j.Dead && !now.Before(j.Next) {
			jobs = append(jobs, j)
		}
	}

	sort.Slice(jobs, func(i, j int) bool {
		if jobs[i].Next.Equal(jobs[j].Next) {
			return jobs[i].ID < jobs[j].ID
		}

		return jobs[i].Next.Before(jobs[j].Next)
	})

	if len(jobs) > maxDue {
		jobs = jobs[:maxDue]
	}

	return jobs, nil
}

// Job returns one job (one record read in every mode).
func (q *Queue) Job(id string) (imapsieve.Job, error) { return q.rec.Get(id) }

// Jobs returns every job of account ("" = all accounts), dead ones
// included, by id: from the owner index in read mode, else from a
// listing.
func (q *Queue) Jobs(account string) ([]imapsieve.Job, error) {
	var (
		jobs []imapsieve.Job
		err  error
	)

	if q.reads() {
		jobs, err = q.ownedJobs(account)
	} else {
		jobs, err = q.listOwned(account, SiteAll)
	}

	if err != nil {
		return nil, err
	}

	slices.SortFunc(jobs, func(a, b imapsieve.Job) int { return strings.Compare(a.ID, b.ID) })

	return jobs, nil
}

// listOwned lists the namespace for the jobs of account ("" = all).
func (q *Queue) listOwned(account, site string) ([]imapsieve.Job, error) {
	all, err := q.rec.List(site)
	if err != nil {
		return nil, err
	}

	out := all[:0]

	for _, j := range all {
		if account == "" || j.Owner == account {
			out = append(out, j)
		}
	}

	return out, nil
}

// ownedJobs reads the jobs of account ("" = all) through the owner
// index; an entry whose record is gone is removed (the entry is written
// after the record and removed before it, so it is stale).
func (q *Queue) ownedJobs(account string) ([]imapsieve.Job, error) {
	ids, err := q.ownerIDs(account)
	if err != nil {
		return nil, err
	}

	jobs := make([]imapsieve.Job, 0, len(ids))

	for _, oid := range ids {
		_, id, _ := strings.Cut(oid, "/")

		j, err := q.rec.Get(id)

		switch {
		case errors.Is(err, imapsieve.ErrNoJob):
			if err := q.owner.Remove(oid); err != nil {
				q.indexError(OwnerIndex, id, err)
			}

			continue
		case err != nil:
			return nil, err
		}

		jobs = append(jobs, j)
	}

	return jobs, nil
}

// ownerIDs returns the owner index ids of account ("" = all), in order.
func (q *Queue) ownerIDs(account string) ([]string, error) {
	prefix := ""
	if account != "" {
		prefix = account + "/"
	}

	var ids []string

	after := prefix

	for {
		page, err := q.owner.ListOrdered(after, 256)
		if err != nil {
			return nil, err
		}

		for _, e := range page {
			if !strings.HasPrefix(e.ID, prefix) {
				return ids, nil
			}

			ids = append(ids, e.ID)
			after = e.ID
		}

		if len(page) == 0 {
			return ids, nil
		}
	}
}

// Revive makes a dead job due again (manual retry): attempts start over,
// Retries counts the retry. The job must still exist (a purged job is
// not recreated) and be dead; the worker leaves dead jobs alone, so no
// worker write races this one.
func (q *Queue) Revive(id string) (imapsieve.Job, error) {
	pre, err := q.rec.Get(id)
	if err != nil {
		return imapsieve.Job{}, err
	}

	if !pre.Dead {
		return pre, imapsieve.ErrNotDead
	}

	j := pre
	j.Dead, j.Attempts, j.Next = false, 0, q.o.Now()
	j.Retries++

	if q.writes() {
		j.Gen = pre.Gen + 1
	}

	if err := q.rec.Set(j); err != nil {
		return imapsieve.Job{}, err
	}

	if q.writes() {
		from, to := Position(pre), Position(j)
		q.move(id, []model.DueKey{from}, &to)
		q.q.Notify()
	}

	return j, nil
}

// PurgeOwner deletes every learning job of an account (account purge):
// through the owner index in read mode, else from a listing.
func (q *Queue) PurgeOwner(account string) (int, error) {
	var (
		jobs []imapsieve.Job
		err  error
	)

	if q.reads() {
		var ids []string

		ids, err = q.ownerIDs(account)

		for _, oid := range ids {
			_, id, _ := strings.Cut(oid, "/")
			jobs = append(jobs, imapsieve.Job{ID: id, Owner: account})
		}
	} else {
		jobs, err = q.listOwned(account, SitePurge)
	}

	if err != nil {
		return 0, err
	}

	n := 0

	for _, j := range jobs {
		if err := q.Delete(j); err != nil {
			return n, err
		}

		n++
	}

	return n, nil
}
