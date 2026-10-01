package backfill

import (
	"encoding/json"
	"errors"
	"slices"
	"testing"
	"time"

	"shardpost/internal/jobs"
	"shardpost/internal/kvindex"
	"shardpost/internal/memstore"
	"shardpost/internal/model"
)

const retention = 14 * 24 * time.Hour

// legacyKV has admin records written by servers without indexes.
func legacyKV(t *testing.T, now time.Time) *memstore.AdminKV {
	t.Helper()

	old := memstore.NewAdminKVMode(model.KVIndexOff)

	put := func(key string, v any) {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}

		if err := old.Update(key, func([]byte) ([]byte, error) { return b, nil }); err != nil {
			t.Fatal(err)
		}
	}

	put("job/q1", jobs.Job{ID: "q1", Kind: "count", Account: "alice", State: jobs.StateQueued, Created: now.Add(-time.Hour)})
	put("job/r1", jobs.Job{ID: "r1", Kind: "count", Account: "bob", State: jobs.StateRunning, Created: now.Add(-time.Hour),
		Lease: jobs.Lease{Owner: "pod-x", Until: now.Add(time.Minute), Gen: 3}})
	put("job/d1", jobs.Job{ID: "d1", Kind: "count", Account: "alice", State: jobs.StateSucceeded, Created: now.Add(-2 * time.Hour),
		Finished: now.Add(-time.Hour), IdemKey: "idem/k1"})
	put("idem/k1", map[string]string{"job_id": "d1", "request_hash": "h"})
	put("idem/orphan", map[string]string{"job_id": "never", "request_hash": "h"})
	put("plan/p1", jobs.Plan{ID: "p1", Kind: "wipe", Account: "alice", Expires: now.Add(time.Hour)})
	put("jmap/push/acct/alice", map[string]any{"account": "alice", "subs": []map[string]any{{"id": "s", "verified": true}}})
	put("jmap/push/lease", map[string]any{"holder": "x"})

	if objs := old.Objects(); len(objs) != 0 {
		t.Fatalf("the legacy store has index objects: %v", objs)
	}

	return old.WithMode(model.KVIndexWrite)
}

// snapshot is every index and queue entry of the store.
func snapshot(t *testing.T, kv *memstore.AdminKV) []string {
	t.Helper()

	var out []string

	for _, c := range kvindex.Collections() {
		page, err := kv.ListIndex(c.Prefix(), "", 1000)
		if err != nil {
			t.Fatal(err)
		}

		for _, e := range page {
			out = append(out, "ix "+e.Key+" "+string(e.Value))
		}
	}

	for _, q := range []string{model.JobsClaimableQueue, model.JobsExpiryQueue} {
		all, err := kv.QueueOf(q).All()
		if err != nil {
			t.Fatal(err)
		}

		for _, e := range all {
			out = append(out, q+" "+e.Key())
		}
	}

	slices.Sort(out)

	return out
}

func TestKVBackfillFillsIndexesAndQueues(t *testing.T) {
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	kv := legacyKV(t, now)
	clock := func() time.Time { return now }

	rep, err := KV(kv, KVOptions{Retention: retention, Now: clock})
	if err != nil {
		t.Fatal(err)
	}

	if rep.Records != 8 || rep.Indexed != 7 || rep.Queued != 8 || rep.Finished.IsZero() {
		t.Fatalf("report %+v", rep)
	}

	want := []string{
		"ix idem/k1 " + `{"job_id":"d1"}`,
		"ix idem/orphan " + `{"job_id":"never"}`,
		"ix jmap/push/acct/alice " + `{"verified":true}`,
		"ix job/d1 " + `{"state":"succeeded","account":"alice","kind":"count","finished":"2026-10-01T11:00:00Z"}`,
		"ix job/q1 " + `{"state":"queued","account":"alice","kind":"count"}`,
		"ix job/r1 " + `{"state":"running","account":"bob","kind":"count"}`,
		"ix plan/p1 " + `{"kind":"wipe","account":"alice","expires":"2026-10-01T13:00:00Z"}`,
		model.JobsClaimableQueue + " " + model.FormatDueKey("q1", model.DueKey{Due: now.Add(-time.Hour)}),
		model.JobsClaimableQueue + " " + model.FormatDueKey("r1", model.DueKey{Due: now.Add(time.Minute), Gen: 3}),
		model.JobsExpiryQueue + " " + model.FormatDueKey("idem/k1", model.DueKey{Due: now.Add(-time.Hour + retention), Gen: 1}),
		model.JobsExpiryQueue + " " + model.FormatDueKey("idem/orphan", model.DueKey{Due: now.Add(model.DueQueueGrace)}),
		model.JobsExpiryQueue + " " + model.FormatDueKey("job/d1", model.DueKey{Due: now.Add(-time.Hour + retention), Gen: 1}),
		model.JobsExpiryQueue + " " + model.FormatDueKey("job/q1", model.DueKey{Due: now.Add(-time.Hour + retention)}),
		model.JobsExpiryQueue + " " + model.FormatDueKey("job/r1", model.DueKey{Due: now.Add(-time.Hour + retention)}),
		model.JobsExpiryQueue + " " + model.FormatDueKey("plan/p1", model.DueKey{Due: now.Add(time.Hour + retention)}),
	}
	slices.Sort(want)

	got := snapshot(t, kv)
	if !slices.Equal(got, want) {
		t.Fatalf("entries:\n%v\nwant:\n%v", got, want)
	}

	done, ok, err := KVBackfilled(kv)
	if err != nil || !ok || !done.Started.Equal(rep.Started) {
		t.Fatalf("marker %+v %v %v", done, ok, err)
	}

	// Idempotent: a second run writes the same entries.
	if _, err := KV(kv, KVOptions{Retention: retention, Now: clock}); err != nil {
		t.Fatal(err)
	}

	if again := snapshot(t, kv); !slices.Equal(again, want) {
		t.Fatalf("second run changed the entries:\n%v", again)
	}
}

// TestKVBackfillThenReadMode: after the backfill a server in read mode
// claims a job an older server queued.
func TestKVBackfillThenReadMode(t *testing.T) {
	now := time.Now()
	kv := legacyKV(t, now)

	if _, err := KV(kv, KVOptions{Retention: retention}); err != nil {
		t.Fatal(err)
	}

	read := kv.WithMode(model.KVIndexRead)

	list, _, err := jobs.NewManager(read, &jobs.Env{Accounts: memstore.New()}, jobs.Config{}, "pod").List("alice", jobs.StateQueued, "", 10)
	if err != nil || len(list) != 1 || list[0].ID != "q1" {
		t.Fatalf("read mode list: %v %v", list, err)
	}

	due, err := read.QueueOf(model.JobsClaimableQueue).Due(time.Now(), 10)
	if err != nil || len(due) != 1 || due[0].ID != "q1" {
		t.Fatalf("due: %+v %v", due, err)
	}
}

// TestInspectKV: the inspection shows the marker, the queue sizes and,
// for a job, its record, index entry and queue entries and those of the
// idempotency key it names.
func TestInspectKV(t *testing.T) {
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	kv := legacyKV(t, now)

	if _, err := KV(kv, KVOptions{Retention: retention, Now: func() time.Time { return now }}); err != nil {
		t.Fatal(err)
	}

	out, err := InspectKV(kv, []string{"job/d1", "job/q1", "job/none"}, now)
	if err != nil {
		t.Fatal(err)
	}

	if out.Mode != model.KVIndexWrite || out.Backfill == nil || out.Queues[model.JobsClaimableQueue].Entries != 2 ||
		out.Queues[model.JobsClaimableQueue].Due != 1 || out.Queues[model.JobsExpiryQueue].Entries != 6 {
		t.Fatalf("summary %+v", out)
	}

	checkInspected(t, out.Keys)
}

// checkInspected checks the keys TestInspectKV asked for.
func checkInspected(t *testing.T, inspected []KeyInspection) {
	t.Helper()

	keys := map[string]KeyInspection{}
	for _, k := range inspected {
		keys[k.Key] = k
	}

	// record, index entry, queues of the entries
	for key, want := range map[string]struct {
		record, index bool
		queues        []string
	}{
		"job/d1":   {true, true, []string{model.JobsExpiryQueue}},
		"idem/k1":  {true, true, []string{model.JobsExpiryQueue}},
		"job/q1":   {true, true, []string{model.JobsClaimableQueue, model.JobsExpiryQueue}},
		"job/none": {false, false, nil},
	} {
		got := keys[key]

		var queues []string
		for _, e := range got.Entries {
			queues = append(queues, e.Queue)
		}

		if got.Record != want.record || (len(got.Index) > 0) != want.index || !slices.Equal(queues, want.queues) {
			t.Errorf("%s: %+v", key, got)
		}
	}

	if d1 := keys["job/d1"]; len(d1.Entries) != 1 || d1.Entries[0].Gen != 1 {
		t.Fatalf("job/d1 expiry not final: %+v", d1)
	}
}

func TestKVBackfillResumes(t *testing.T) {
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	kv := legacyKV(t, now)
	started := now.Add(-time.Hour)

	b, _ := json.Marshal(progress{Started: started, After: "job/q1"})
	if err := kv.MarkerIndex().SetMarker(markerKVProgress, string(b)); err != nil {
		t.Fatal(err)
	}

	rep, err := KV(kv, KVOptions{Retention: retention, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}

	// Only job/r1 and plan/p1 follow job/q1.
	if !rep.Resumed || !rep.Started.Equal(started) || rep.Records != 2 || rep.Indexed != 2 || rep.Queued != 3 {
		t.Fatalf("resumed report %+v", rep)
	}

	h, _ := kv.MarkerIndex().Head()
	if h.Markers[markerKVProgress] != "" || h.Markers[MarkerKV] == "" {
		t.Fatalf("markers %v", h.Markers)
	}
}

func TestKVBackfillStopsWhenLockLost(t *testing.T) {
	kv := legacyKV(t, time.Now())

	if _, err := KV(kv, KVOptions{Lost: func() bool { return true }}); !errors.Is(err, ErrLockLost) {
		t.Fatalf("lost lock: %v", err)
	}

	if _, ok, _ := KVBackfilled(kv); ok {
		t.Fatal("a stopped backfill set the completion marker")
	}
}
