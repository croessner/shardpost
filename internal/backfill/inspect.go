package backfill

import (
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"time"

	"shardpost/internal/kvindex"
	"shardpost/internal/model"
	"shardpost/internal/sortedindex"
)

// KVInspection is what "shardpost index show kv" prints: the mode, the
// backfill marker, the size of the queues and, for the keys asked for,
// record, hint index entry and queue entries.
type KVInspection struct {
	// Mode is the mode the store behaves in, Configured the configured
	// one (read waits for the backfill marker).
	Mode       model.KVIndexMode       `json:"mode"`
	Configured model.KVIndexMode       `json:"configured_mode,omitempty"`
	Backfill   *KVReport               `json:"backfill"`
	Queues     map[string]QueueSummary `json:"queues"`
	Keys       []KeyInspection         `json:"keys,omitempty"`
}

// QueueSummary counts the entries of a queue.
type QueueSummary struct {
	Shards  int `json:"shards"`
	Entries int `json:"entries"`
	Due     int `json:"due"`
}

// KeyInspection is one admin store key.
type KeyInspection struct {
	Key     string          `json:"key"`
	Record  bool            `json:"record"`
	Index   json.RawMessage `json:"index,omitempty"`
	Entries []QueueEntryOut `json:"entries"`
}

// QueueEntryOut is a queue entry of a key.
type QueueEntryOut struct {
	Queue string    `json:"queue"`
	Due   time.Time `json:"due"`
	Gen   uint64    `json:"gen"`
}

// InspectSource is what the inspection reads.
type InspectSource interface {
	KVSource
	// QueueOf returns a queue with its full listing.
	QueueOf(name string) *sortedindex.Queue
	// Index returns the hint index of a collection.
	Index(c *kvindex.Collection) *sortedindex.Index
}

// InspectKV reads the state of the admin store indexes (read-only; it
// reads every queue entry, so it is a diagnosis tool, not a routine).
// A job key brings the idempotency key its record names along.
func InspectKV(src InspectSource, keys []string, now time.Time) (KVInspection, error) {
	out := KVInspection{Mode: src.IndexMode(), Configured: configuredMode(src), Queues: map[string]QueueSummary{}}

	if rep, ok, err := KVBackfilled(src); err != nil {
		return out, err
	} else if ok {
		out.Backfill = &rep
	}

	all := map[string][]model.DueEntry{}

	for _, name := range []string{model.JobsClaimableQueue, model.JobsExpiryQueue} {
		q := src.QueueOf(name)

		h, err := q.Head()
		if err != nil {
			return out, err
		}

		entries, err := q.All()
		if err != nil {
			return out, err
		}

		s := QueueSummary{Shards: h.Shards, Entries: len(entries)}

		for _, e := range entries {
			if !e.Due.After(now) {
				s.Due++
			}
		}

		out.Queues[name], all[name] = s, entries
	}

	for i := 0; i < len(keys); i++ {
		ki, idem, err := inspectKey(src, keys[i], all)
		if err != nil {
			return out, err
		}

		out.Keys = append(out.Keys, ki)

		if idem != "" && !slices.Contains(keys, idem) {
			keys = append(keys, idem)
		}
	}

	return out, nil
}

// configuredMode is the configured mode of a store that tells it ("":
// it does not).
func configuredMode(src InspectSource) model.KVIndexMode {
	if c, ok := src.(interface{ ConfiguredMode() model.KVIndexMode }); ok {
		return c.ConfiguredMode()
	}

	return ""
}

// entriesOf returns the queue entries of key: in jobs/expiry under the
// key, in jobs/claimable (a job) under the job id.
func entriesOf(key string, all map[string][]model.DueEntry) []QueueEntryOut {
	out := []QueueEntryOut{}
	claimID, isJob := strings.CutPrefix(key, "job/")

	for _, name := range []string{model.JobsClaimableQueue, model.JobsExpiryQueue} {
		id := key

		if name == model.JobsClaimableQueue {
			if !isJob {
				continue
			}

			id = claimID
		}

		for _, e := range all[name] {
			if e.ID == id {
				out = append(out, QueueEntryOut{Queue: name, Due: e.Due, Gen: e.Gen})
			}
		}
	}

	return out
}

// inspectKey reads one key; idem is the idempotency key a job names.
func inspectKey(src InspectSource, key string, all map[string][]model.DueEntry) (KeyInspection, string, error) {
	ki := KeyInspection{Key: key, Entries: []QueueEntryOut{}}

	rec, err := src.Get(key)

	switch {
	case err == nil:
		ki.Record = true
	case !errors.Is(err, model.ErrNotFound):
		return ki, "", err
	}

	if c, id, ok := kvindex.CollectionOf(key); ok {
		if v, err := src.Index(c).Get(id); err == nil {
			ki.Index = v
		}
	}

	_, isJob := strings.CutPrefix(key, "job/")
	ki.Entries = entriesOf(key, all)

	var j struct {
		IdemKey string `json:"idem_key"`
	}

	if isJob && rec != nil && json.Unmarshal(rec, &j) == nil {
		return ki, j.IdemKey, nil
	}

	return ki, "", nil
}
