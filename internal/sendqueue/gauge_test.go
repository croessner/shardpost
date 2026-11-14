package sendqueue

import (
	"context"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"

	"shardpost/internal/metrics"
	"shardpost/internal/model"
)

// queueStates are the states imapceph_jmap_submission_queue_entries has
// a series for, with their values.
func queueStates(t *testing.T) map[string]float64 {
	t.Helper()

	ch := make(chan prometheus.Metric, 8)

	go func() {
		metrics.SubmissionQueue.Collect(ch)
		close(ch)
	}()

	out := map[string]float64{}

	for m := range ch {
		var d dto.Metric
		if err := m.Write(&d); err != nil {
			t.Fatal(err)
		}

		out[d.GetLabel()[0].GetValue()] = d.GetGauge().GetValue()
	}

	return out
}

// TestQueueGaugeFollowsTheHolder: the holder of partition 0 counts the
// queue; a worker that no longer holds it withdraws its count, which
// would otherwise go stale beside the new holder's.
func TestQueueGaugeFollowsTheHolder(t *testing.T) {
	e := newEnv(t)

	w := &worker{s: e.svc, held: map[int]model.Lease{}}
	w.leases(true)

	e.schedule(t, "alice", 3600)
	w.round(context.Background())

	if got := queueStates(t); got["all"] != 1 || got["due"] != 0 {
		t.Fatalf("the holder counts %v", got)
	}

	// A worker that still holds another partition withdraws the count in
	// its round; one without any partition runs no round (Run), and
	// withdraws it there.
	w.releaseAll()
	w.withdrawGauge()

	got := queueStates(t)
	if _, ok := got["all"]; ok {
		t.Errorf("a worker without partition 0 still exports the queue: %v", got)
	}

	if v, ok := got["unowned"]; !ok || v != 0 {
		t.Errorf("a worker without partitions reports overdue entries: %v", got)
	}

	w.leases(true)
	t.Cleanup(w.releaseAll)
	w.round(context.Background())

	if got := queueStates(t); got["all"] != 1 {
		t.Errorf("the holder counts again: %v", got)
	}
}
