package backfill

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"shardpost/internal/imapsieve"
	"shardpost/internal/learnindex"
	"shardpost/internal/model"
)

// LearningReport is the result of a learning queue backfill.
type LearningReport struct {
	// Started is when the backfill began (for a resumed one, when its
	// first run began).
	Started  time.Time `json:"started"`
	Finished time.Time `json:"finished,omitzero"`
	// Jobs is the number of jobs this run handled (a resumed run does not
	// count those before its position).
	Jobs    int  `json:"jobs"`
	Resumed bool `json:"resumed,omitempty"`
}

// LearningOptions configure a learning queue backfill.
type LearningOptions struct {
	// Lost reports that the backfill's lock could not be renewed; the
	// backfill stops before its next write. nil: never.
	Lost func() bool
	// Restart ignores the position of an interrupted run.
	Restart bool
	Log     *slog.Logger
	Now     func() time.Time
}

// Learning fills the due queue and the owner index of the learning queue
// from its records (scaling plan B5): one listing of the namespace
// "~learning" of the data pool, then for every job, read again, its
// queue entry at learnindex.Position and its owner entry. Every write
// sets what the record implies, so the backfill can be repeated; it
// resumes after an interruption. Entries a job no longer has are left to
// the readers, which move or remove them. The caller holds the
// backfill lock.
func Learning(q *learnindex.Queue, o LearningOptions) (LearningReport, error) {
	if o.Now == nil {
		o.Now = time.Now
	}

	if o.Lost == nil {
		o.Lost = func() bool { return false }
	}

	if q.DueQueue() == nil || q.ConfiguredMode() == model.KVIndexOff {
		return LearningReport{}, errors.New("imapsieve.learning.index is off: the indexes are not kept, a backfill would go stale")
	}

	log := Options{Log: o.Log}.withDefaults().Log
	dq := q.DueQueue()
	rep := LearningReport{Started: o.Now().UTC()}

	pos, err := learningResume(dq, o.Restart, &rep, log)
	if err != nil {
		return rep, err
	}

	jobs, err := q.Records().List(learnindex.SiteBackfill)
	if err != nil {
		return rep, fmt.Errorf("list learning jobs: %w", err)
	}

	slices.SortFunc(jobs, func(a, b imapsieve.Job) int { return strings.Compare(a.ID, b.ID) })

	for i, j := range jobs {
		if j.ID <= pos.After {
			continue // done by the interrupted run
		}

		if err := learningStep(q, j.ID, i, pos, o, &rep, log); err != nil {
			return rep, err
		}
	}

	if o.Lost() {
		return rep, ErrLockLost
	}

	rep.Finished = o.Now().UTC()
	b, _ := json.Marshal(rep)

	if err := dq.SetMarker(learnindex.MarkerBackfill, string(b)); err != nil {
		return rep, err
	}

	return rep, dq.SetMarker(learnindex.MarkerProgress, "")
}

// learningResume returns the position of an interrupted backfill (unless
// restart) and takes over its start time.
func learningResume(dq interface {
	Head() (model.IndexHead, error)
}, restart bool, rep *LearningReport, log *slog.Logger,
) (*progress, error) {
	pos := &progress{}

	h, err := dq.Head()
	if err != nil {
		return pos, err
	}

	if v := h.Markers[learnindex.MarkerProgress]; v != "" && !restart && json.Unmarshal([]byte(v), pos) == nil {
		rep.Started, rep.Resumed = pos.Started, true
		log.Info("backfill resumes", "after", pos.After, "started", pos.Started)
	}

	pos.Started = rep.Started

	return pos, nil
}

// learningStep handles the job id (the i-th listed) and records the
// position every progressEvery jobs.
func learningStep(q *learnindex.Queue, id string, i int, pos *progress, o LearningOptions, rep *LearningReport, log *slog.Logger) error {
	if o.Lost() {
		return ErrLockLost
	}

	if err := learningJob(q, id); err != nil {
		return err
	}

	rep.Jobs++
	pos.After = id

	if (i+1)%progressEvery != 0 {
		return nil
	}

	b, _ := json.Marshal(pos)
	if err := q.DueQueue().SetMarker(learnindex.MarkerProgress, string(b)); err != nil {
		return err
	}

	log.Info("backfill progress", "after", id, "jobs", rep.Jobs)

	return nil
}

// learningJob writes the entries of one job from its record as it is now.
func learningJob(q *learnindex.Queue, id string) error {
	j, err := q.Records().Get(id)
	if errors.Is(err, imapsieve.ErrNoJob) {
		return nil // removed meanwhile
	}

	if err != nil {
		return fmt.Errorf("learning job %s: %w", id, err)
	}

	at := learnindex.Position(j)
	if err := q.DueQueue().Move(j.ID, nil, &at); err != nil {
		return fmt.Errorf("queue entry of %s: %w", id, err)
	}

	if err := q.Owners().Set(learnindex.OwnerID(j), []byte{}); err != nil {
		return fmt.Errorf("owner entry of %s: %w", id, err)
	}

	return nil
}

// LearningBackfilled returns the report of the last completed learning
// queue backfill (false: none completed).
func LearningBackfilled(q *learnindex.Queue) (LearningReport, bool, error) {
	if q.DueQueue() == nil {
		return LearningReport{}, false, nil
	}

	h, err := q.DueQueue().Head()
	if err != nil {
		return LearningReport{}, false, err
	}

	var rep LearningReport

	v := h.Markers[learnindex.MarkerBackfill]
	if v == "" || json.Unmarshal([]byte(v), &rep) != nil || rep.Finished.IsZero() {
		return LearningReport{}, false, nil
	}

	return rep, true, nil
}

// LearningInspection is what "shardpost index show learning" prints.
type LearningInspection struct {
	Mode       model.KVIndexMode `json:"mode"`
	Configured model.KVIndexMode `json:"configured_mode"`
	Backfill   *LearningReport   `json:"backfill"`
	Queue      QueueSummary      `json:"queue"`
	// Dead counts the entries of dead jobs, Owners the owner entries.
	Dead   int                  `json:"dead"`
	Owners int                  `json:"owners"`
	Jobs   []LearningJobInspect `json:"jobs,omitempty"`
}

// LearningJobInspect is one job: record, queue entries, owner entry.
type LearningJobInspect struct {
	ID      string          `json:"id"`
	Record  *imapsieve.Job  `json:"record"`
	Entries []QueueEntryOut `json:"entries"`
	Owner   bool            `json:"owner_entry"`
}

// InspectLearning reads the state of the learning queue indexes
// (read-only; it reads every index entry, so it is a diagnosis tool).
func InspectLearning(q *learnindex.Queue, ids []string, now time.Time) (LearningInspection, error) {
	out := LearningInspection{Mode: q.Mode(), Configured: q.ConfiguredMode()}

	if q.DueQueue() == nil {
		return out, nil
	}

	if rep, ok, err := LearningBackfilled(q); err != nil {
		return out, err
	} else if ok {
		out.Backfill = &rep
	}

	h, err := q.DueQueue().Head()
	if err != nil {
		return out, err
	}

	entries, err := q.DueQueue().All()
	if err != nil {
		return out, err
	}

	out.Queue = QueueSummary{Shards: h.Shards, Entries: len(entries)}

	for _, e := range entries {
		switch {
		case !e.Due.Before(learnindex.DeadDue):
			out.Dead++
		case !e.Due.After(now):
			out.Queue.Due++
		}
	}

	owners, err := allOwners(q)
	if err != nil {
		return out, err
	}

	out.Owners = len(owners)

	for _, id := range ids {
		ji, err := inspectLearningJob(q, id, entries, owners)
		if err != nil {
			return out, err
		}

		out.Jobs = append(out.Jobs, ji)
	}

	return out, nil
}

// inspectLearningJob reads one job and finds its index entries.
func inspectLearningJob(q *learnindex.Queue, id string, entries []model.DueEntry, owners []string) (LearningJobInspect, error) {
	ji := LearningJobInspect{ID: id, Entries: []QueueEntryOut{}}

	j, err := q.Records().Get(id)

	switch {
	case err == nil:
		ji.Record = &j
	case !errors.Is(err, imapsieve.ErrNoJob):
		return ji, err
	}

	for _, e := range entries {
		if e.ID == id {
			ji.Entries = append(ji.Entries, QueueEntryOut{Queue: model.LearningQueue, Due: e.Due, Gen: e.Gen})
		}
	}

	ji.Owner = slices.ContainsFunc(owners, func(o string) bool { return strings.HasSuffix(o, "/"+id) })

	return ji, nil
}

// allOwners reads every owner index id.
func allOwners(q *learnindex.Queue) ([]string, error) {
	var ids []string

	cursor := ""

	for {
		page, next, err := q.Owners().List(cursor, 1000)
		if err != nil {
			return nil, err
		}

		for _, e := range page {
			ids = append(ids, e.ID)
		}

		if next == "" {
			return ids, nil
		}

		cursor = next
	}
}
