package backfill

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"shardpost/internal/jobs"
	"shardpost/internal/kvindex"
	"shardpost/internal/model"
)

// Markers in the head of the job hint index ("ix.job/head" in "~admin").
const (
	// MarkerKV records the last completed admin store backfill (KVReport
	// as JSON). Readers may switch to admin.kv_index=read only after a
	// backfill that started after the last server without the indexes
	// stopped.
	MarkerKV = kvindex.MarkerKV
	// markerKVProgress records the position of a running backfill.
	markerKVProgress = "backfill.kv.progress"
)

// KVSource is what the admin store backfill needs from the store.
type KVSource interface {
	model.IndexedKV
	// Keys lists the keys of every record (one listing of the
	// namespace), in key order.
	Keys() ([]string, error)
	// Reindex writes the hint index entry of key from its record.
	Reindex(key string) error
	// MarkerIndex carries the backfill markers.
	MarkerIndex() model.SortedIndex
}

// KVReport is the result of an admin store backfill.
type KVReport struct {
	// Started is when the backfill began (for a resumed one, when its
	// first run began).
	Started  time.Time `json:"started"`
	Finished time.Time `json:"finished,omitzero"`
	// Records is the number of records this run handled (a resumed run
	// does not count those before its position), Indexed the hint index
	// entries written, Queued the queue entries written.
	Records int `json:"records"`
	Indexed int `json:"indexed"`
	Queued  int `json:"queued"`
	// Resumed: the run continued an interrupted one.
	Resumed bool `json:"resumed,omitempty"`
}

// KVOptions configure an admin store backfill.
type KVOptions struct {
	// Lost reports that the backfill's lock could not be renewed; the
	// backfill stops before its next write. nil: never.
	Lost func() bool
	// Restart ignores the position of an interrupted run.
	Restart bool
	// Retention is the jobs retention (jobs.retention_days) the expiry
	// entries are computed with.
	Retention time.Duration
	Log       *slog.Logger
	Now       func() time.Time
}

// KV fills the hint indexes and the due queues of the admin store from
// its records (scaling plan B3): one listing of the namespace, then for
// every record its hint index entry (from the record as it is) and the
// queue entries it needs (jobs.QueueEntries). Every write sets what the
// record implies, so the backfill can be repeated; it resumes after an
// interruption. The caller holds the backfill lock.
func KV(src KVSource, o KVOptions) (KVReport, error) {
	if o.Now == nil {
		o.Now = time.Now
	}

	if o.Lost == nil {
		o.Lost = func() bool { return false }
	}

	log := Options{Log: o.Log}.withDefaults().Log
	marks := src.MarkerIndex()
	rep := KVReport{Started: o.Now().UTC()}

	pos, err := kvResume(marks, o.Restart, &rep, log)
	if err != nil {
		return rep, err
	}

	keys, err := src.Keys()
	if err != nil {
		return rep, fmt.Errorf("list admin records: %w", err)
	}

	for i, key := range keys {
		if key <= pos.After {
			continue // done by the interrupted run
		}

		rep.Records++

		if err := kvStep(src, marks, key, i, pos, o, &rep, log); err != nil {
			return rep, err
		}
	}

	if o.Lost() {
		return rep, ErrLockLost
	}

	rep.Finished = o.Now().UTC()
	b, _ := json.Marshal(rep)

	if err := marks.SetMarker(MarkerKV, string(b)); err != nil {
		return rep, err
	}

	return rep, marks.SetMarker(markerKVProgress, "")
}

// kvResume returns the position of an interrupted backfill (unless
// restart) and takes over its start time.
func kvResume(marks model.SortedIndex, restart bool, rep *KVReport, log *slog.Logger) (*progress, error) {
	pos := &progress{}

	h, err := marks.Head()
	if err != nil {
		return pos, err
	}

	if v := h.Markers[markerKVProgress]; v != "" && !restart && json.Unmarshal([]byte(v), pos) == nil {
		rep.Started, rep.Resumed = pos.Started, true
		log.Info("backfill resumes", "after", pos.After, "started", pos.Started)
	}

	pos.Started = rep.Started

	return pos, nil
}

// kvStep handles the record key (the i-th listed) and records the
// position every progressEvery records.
func kvStep(src KVSource, marks model.SortedIndex, key string, i int, pos *progress, o KVOptions, rep *KVReport, log *slog.Logger) error {
	if o.Lost() {
		return ErrLockLost
	}

	if err := kvRecord(src, key, o, rep); err != nil {
		return err
	}

	pos.After = key

	if (i+1)%progressEvery != 0 {
		return nil
	}

	b, _ := json.Marshal(pos)
	if err := marks.SetMarker(markerKVProgress, string(b)); err != nil {
		return err
	}

	log.Info("backfill progress", "after", key, "indexed", rep.Indexed, "queued", rep.Queued)

	return nil
}

// kvRecord writes the index and queue entries of one record.
func kvRecord(src KVSource, key string, o KVOptions, rep *KVReport) error {
	rec, err := src.Get(key)
	if errors.Is(err, model.ErrNotFound) {
		return nil // removed meanwhile
	}

	if err != nil {
		return fmt.Errorf("record %q: %w", key, err)
	}

	if _, _, ok := kvindex.CollectionOf(key); ok {
		if err := src.Reindex(key); err != nil {
			return fmt.Errorf("index %q: %w", key, err)
		}

		rep.Indexed++
	}

	entries, err := jobs.QueueEntries(key, rec, src.Get, o.Retention, o.Now())
	if err != nil {
		return fmt.Errorf("queue entries of %q: %w", key, err)
	}

	for _, e := range entries {
		q, err := src.Queue(e.Queue)
		if err != nil {
			return err
		}

		if err := q.Move(e.ID, nil, &e.Key); err != nil {
			return fmt.Errorf("queue %s entry of %q: %w", e.Queue, key, err)
		}

		rep.Queued++
	}

	return nil
}

// KVBackfilled returns the report of the last completed admin store
// backfill (false: none completed).
func KVBackfilled(src KVSource) (KVReport, bool, error) {
	h, err := src.MarkerIndex().Head()
	if err != nil {
		return KVReport{}, false, err
	}

	var rep KVReport

	v := h.Markers[MarkerKV]
	if v == "" || json.Unmarshal([]byte(v), &rep) != nil {
		return KVReport{}, false, nil
	}

	return rep, true, nil
}
