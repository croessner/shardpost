package metrics

import (
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
)

// exposed are the metric families of the process that have a series,
// with the number of their series.
func exposed(t *testing.T) map[string]int {
	t.Helper()

	families, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatal(err)
	}

	out := map[string]int{}

	for _, f := range families {
		if strings.HasPrefix(f.GetName(), namespace+"_") {
			out[f.GetName()] = len(f.GetMetric())
		}
	}

	return out
}

// series checks the number of series of the named families.
func series(t *testing.T, step string, want map[string]int) {
	t.Helper()

	got := exposed(t)

	for name, n := range want {
		if got[name] != n {
			t.Errorf("%s: %s has %d series, want %d", step, name, got[name], n)
		}
	}
}

// absent is series with zero for every name.
func absent(t *testing.T, step string, names ...string) {
	t.Helper()

	want := map[string]int{}
	for _, name := range names {
		want[name] = 0
	}

	series(t, step, want)
}

// initTested is set once TestInit has run (go test -count).
var initTested bool

// TestInit: a process exposes, from its start, the event metrics of the
// parts it runs and none of the parts it does not. The roles are
// initialized one after the other in one process here, so each step
// checks what it adds.
func TestInit(t *testing.T) {
	const (
		pushSent    = "imapceph_jmap_push_sent_total"
		deliveries  = "imapceph_lmtp_deliveries_total"
		authCache   = "imapceph_auth_cache_total"
		findings    = "imapceph_scrub_findings_total"
		shadow      = "imapceph_scrub_shadow_differences_total"
		listings    = "imapceph_rados_listings_total"
		recounts    = "imapceph_quota_recount_runs_total"
		submissions = "imapceph_jmap_delayed_submissions_total"
		notify      = "imapceph_rados_notify_seconds"
		connections = "imapceph_connections"
		queueOps    = "imapceph_queue_ops_total"
	)

	// The registry is the process's: what a later step created cannot be
	// absent again, so the steps can be checked once per process.
	if initTested {
		t.Skip("already run in this process")
	}

	initTested = true

	// The push worker: its own events, the store's, nothing of a server.
	Init(Features{Push: true})
	series(t, "push worker", map[string]int{pushSent: 4, notify: 1, "imapceph_rados_op_seconds": 4})
	absent(t, "push worker", deliveries, authCache, findings, shadow, listings, recounts, submissions, connections,
		"imapceph_commands_total", "imapceph_job_running", queueOps)

	// A server without the optional parts.
	Init(Features{Server: true})
	series(t, "bare server", map[string]int{connections: 3, notify: 3, queueOps: 24, "imapceph_rados_op_seconds": 8,
		"imapceph_rados_cas_retries_total": 12, "imapceph_commands_total": 15, "imapceph_queue_stale_entries_total": 14})
	absent(t, "bare server", deliveries, authCache, findings, shadow, listings, recounts, submissions)

	// The whole server, scrubbing from the intents as production does.
	// What only an event of its own creates stays absent, and so does
	// what the configuration switches off.
	Init(Features{Server: true, Workers: true, LMTP: true, AuthCache: true, Jobs: true, DelayedOffered: true,
		DelayedSubmission: true})
	series(t, "server", map[string]int{deliveries: 2, authCache: 7, findings: 13, listings: 3, recounts: 12,
		submissions: 7, "imapceph_auth_cache_flushes_total": 5, "imapceph_jobs_mailbox_delete_runs_total": 7,
		"imapceph_fts_build_runs_total": 16, "imapceph_fts_layout_runs_total": 5, "imapceph_job_running": 13})
	absent(t, "server", shadow, "imapceph_fts_shard_keys", "imapceph_jmap_submission_queue_entries")

	// The runner: workers and jobs without the front-ends.
	Init(Features{Workers: true, Jobs: true, ScrubShadow: true, ScrubListing: true, PurgeListing: true})
	series(t, "listing scrub", map[string]int{shadow: 4, findings: 14, listings: 6})

	SetFeatures(map[string]bool{FeatureJobs: true, FeatureScrubShadow: false})
	series(t, "features", map[string]int{"imapceph_feature_enabled": 2})
}
