// Package metrics exposes Prometheus metrics for the store and the
// protocol front-ends.
package metrics

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// Metric namespace and subsystems.
const (
	namespace      = "imapceph"
	subsystemRados = "rados"
	subsystemLMTP  = "lmtp"
	subsystemScrub = "scrub"
	subsystemIMAP  = "imap"
	subsystemAuth  = "auth"
	subsystemJMAP  = "jmap"
)

// Label names.
const (
	labelOp      = "op"
	labelKind    = "kind"
	labelProto   = "proto"
	labelCommand = "command"
	labelOutcome = "outcome"
	labelSite    = "site"
	labelQueue   = "queue"
	labelAction  = "action"
	labelColl    = "collection"
	labelCause   = "cause"
	labelObject  = "object"
	labelResult  = "result"
	labelScope   = "scope"
	labelSource  = "source"
	labelLimit   = "limit"
	labelState   = "state"
)

var (
	// RadosOps is the latency of RADOS operations by kind.
	RadosOps = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: namespace, Subsystem: subsystemRados, Name: "op_seconds",
		Help:    "Latency of RADOS operations by kind.",
		Buckets: []float64{.0005, .001, .0025, .005, .01, .025, .05, .1, .25, .5, 1, 2.5},
	}, []string{labelOp})
	// CASRetries counts compare-and-swap conflicts that led to a retry.
	CASRetries = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace, Subsystem: subsystemRados, Name: "cas_retries_total",
		Help: "Compare-and-swap conflicts that led to a retry, by object kind.",
	}, []string{labelKind})
	// RadosListings counts RADOS object listings by call site. A listing
	// visits every placement group of the pool, so its cost grows with
	// the whole pool; running paths must not list (scaling plan,
	// guideline 1), and this counter shows where they still do.
	RadosListings = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace, Subsystem: subsystemRados, Name: "listings_total",
		Help: "RADOS object listings (each visits every placement group of the pool), by call site.",
	}, []string{labelSite})
	// NotifyLatency is the time until all watchers acknowledged a notify.
	NotifyLatency = promauto.NewHistogram(prometheus.HistogramOpts{
		Namespace: namespace, Subsystem: subsystemRados, Name: "notify_seconds",
		Help:    "Time a mailbox notify took until all watchers acknowledged.",
		Buckets: []float64{.001, .0025, .005, .01, .025, .05, .1, .25, .5, 1},
	})
	// Commands counts protocol commands by protocol, command and outcome.
	Commands = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace, Name: "commands_total",
		Help: "Protocol commands by protocol, command and outcome.",
	}, []string{labelProto, labelCommand, labelOutcome})
	// CommandLatency is the command latency by protocol and command.
	CommandLatency = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: namespace, Name: "command_seconds",
		Help:    "Command latency by protocol and command.",
		Buckets: []float64{.001, .0025, .005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5},
	}, []string{labelProto, labelCommand})
	// Connections is the number of open client connections by protocol.
	Connections = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: namespace, Name: "connections",
		Help: "Open client connections by protocol.",
	}, []string{labelProto})
	// Deliveries counts LMTP deliveries by outcome.
	Deliveries = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace, Subsystem: subsystemLMTP, Name: "deliveries_total",
		Help: "LMTP deliveries by outcome.",
	}, []string{labelOutcome})
	// DeliveryBytes counts the bytes delivered via LMTP.
	DeliveryBytes = promauto.NewCounter(prometheus.CounterOpts{
		Namespace: namespace, Subsystem: subsystemLMTP, Name: "delivered_bytes_total",
		Help: "Bytes delivered via LMTP.",
	})
	// Scrub counts scrubber findings by kind.
	Scrub = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace, Subsystem: subsystemScrub, Name: "findings_total",
		Help: "Scrubber findings by kind.",
	}, []string{labelKind})
	// ScrubShadowDiff counts what a scrub in shadow mode found by listing
	// and the intents did not name (orphan_missed, unknown_missed,
	// marks_missed), and what the intents took for an orphan that the
	// listing did not (orphan_extra). Before the scrub may switch to
	// intents only, the first must stay at what the sweep cleans up and
	// the second at zero.
	ScrubShadowDiff = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace, Subsystem: subsystemScrub, Name: "shadow_differences_total",
		Help: "Differences between the listing and the intents in a scrub in shadow mode, by kind.",
	}, []string{labelKind})
	// PurgeUnnamed counts objects a purge with listing found that the
	// data model did not name (they are deleted all the same).
	PurgeUnnamed = promauto.NewCounter(prometheus.CounterOpts{
		Namespace: namespace, Subsystem: subsystemScrub, Name: "purge_unnamed_objects_total",
		Help: "Objects a purge found by listing that the account's data model did not name.",
	})
	// QueueOps counts the operations on due queues (scaling plan B1) by
	// queue, operation (move, due) and outcome (ok, error).
	QueueOps = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace, Name: "queue_ops_total",
		Help: "Operations on due queues by queue, operation and outcome.",
	}, []string{labelQueue, labelOp, labelOutcome})
	// QueueStale counts queue entries a reader found not matching their
	// record (two writers moved it without compare-and-swap, or a crash
	// fell between the record and the queue write), by queue and what
	// the reader did: rescheduled to the record's position, removed, or
	// skipped (within the grace).
	QueueStale = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace, Name: "queue_stale_entries_total",
		Help: "Queue entries that did not match their record, by queue and action.",
	}, []string{labelQueue, labelAction})
	// Submissions counts what happened to delayed JMAP submissions
	// (scaling plan B10), by outcome: scheduled, sent (the relay took
	// it), canceled, failed (refused, or given up after the retries),
	// unknown (the sending was interrupted and is not repeated: at most
	// once), retried (a temporary failure before the message was
	// transferred), and conflict (a worker's write after sending found
	// the record changed, see the sending timeout).
	Submissions = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace, Subsystem: subsystemJMAP, Name: "delayed_submissions_total",
		Help: "Delayed JMAP submissions by outcome.",
	}, []string{labelOutcome})
	// SubmissionQueue is the number of entries of the submission queue
	// (counted by the holder of partition 0 at most once per poll, up to
	// a bound), by state: all entries and the ones due.
	SubmissionQueue = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: namespace, Subsystem: subsystemJMAP, Name: "submission_queue_entries",
		Help: "Entries of the delayed submission queue (all, due).",
	}, []string{labelState})
	// KVIndexErrors counts failed writes of an admin store hint index
	// (the record was written; the backfill repairs the entry).
	KVIndexErrors = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace, Name: "kv_index_errors_total",
		Help: "Failed hint index writes of the admin store, by collection.",
	}, []string{labelColl})
	// JobWakeups counts the times a job runner looked for claimable jobs,
	// by cause: poll (the interval) or notify (a writer woke it).
	JobWakeups = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace, Name: "job_wakeups_total",
		Help: "Job runner looks for claimable jobs by cause (poll, notify).",
	}, []string{labelCause})
	// BatchAccounts counts the accounts a batch job (a kind over every
	// account of the directory) handled, by kind and outcome: done,
	// skipped (idle, scrub), gone (purged meanwhile), deferred (locked,
	// tried again at the end) and failed.
	BatchAccounts = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace, Name: "batch_accounts_total",
		Help: "Accounts handled by batch jobs, by kind and outcome.",
	}, []string{labelKind, labelOutcome})
	// IMAPWatches is the number of RADOS watches the IMAP watch hub of
	// this process holds, by object (mailbox, account): one per object
	// however many sessions follow it.
	IMAPWatches = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: namespace, Subsystem: subsystemIMAP, Name: "watches",
		Help: "RADOS watches held by the IMAP watch hub of this process, by object.",
	}, []string{labelObject})
	// IMAPWatchSubscribers is the number of sessions following a watch
	// of the hub, by object.
	IMAPWatchSubscribers = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: namespace, Subsystem: subsystemIMAP, Name: "watch_subscribers",
		Help: "Session subscriptions to the watches of the IMAP watch hub, by object.",
	}, []string{labelObject})
	// IMAPWatchRearms counts the watches the hub set up again after an
	// error (the OSD dropped it, the store rotated its connection).
	IMAPWatchRearms = promauto.NewCounter(prometheus.CounterOpts{
		Namespace: namespace, Subsystem: subsystemIMAP, Name: "watch_rearms_total",
		Help: "Watches the IMAP watch hub set up again after an error.",
	})
	// IMAPWatchRefused counts subscriptions a connection was refused for
	// being at its watch limit (it polls instead).
	IMAPWatchRefused = promauto.NewCounter(prometheus.CounterOpts{
		Namespace: namespace, Subsystem: subsystemIMAP, Name: "watch_refused_total",
		Help: "Watch subscriptions refused because the connection was at its limit.",
	})
	// AuthCache counts the auth cache's answers by operation
	// (authenticate, lookup) and result: hit (a cached entry, positive
	// or negative), miss (the caller asked the backend), shared (the
	// caller waited for another caller's identical backend call) and
	// bypass (an admin lookup that went past a negative entry).
	AuthCache = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace, Subsystem: subsystemAuth, Name: "cache_total",
		Help: "Auth cache answers by operation and result (hit, miss, shared, bypass).",
	}, []string{labelOp, labelResult})
	// AuthCacheEntries is the number of entries the auth cache holds,
	// by operation.
	AuthCacheEntries = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: namespace, Subsystem: subsystemAuth, Name: "cache_entries",
		Help: "Entries held by the auth cache, by operation.",
	}, []string{labelOp})
	// AuthCacheFlushes counts the flushes of the auth cache by scope
	// (account, all) and source (local: this server's admin API; remote:
	// the flush log of another server; resync: a full flush after a
	// lost watch, a failed first read of the flush log or a gap in it).
	AuthCacheFlushes = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace, Subsystem: subsystemAuth, Name: "cache_flushes_total",
		Help: "Auth cache flushes by scope and source.",
	}, []string{labelScope, labelSource})
	// AuthBackend is the latency of the backend calls behind the auth
	// cache, by operation.
	AuthBackend = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: namespace, Subsystem: subsystemAuth, Name: "backend_seconds",
		Help:    "Latency of the authentication backend calls behind the auth cache, by operation.",
		Buckets: []float64{.005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10, 20},
	}, []string{labelOp})
	// JMAPLimit counts the JMAP requests that met a full per-account or
	// per-IP limit, by limit and outcome: waited (got a slot after
	// waiting) and rejected (answered 429).
	JMAPLimit = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace, Subsystem: subsystemJMAP, Name: "limit_total",
		Help: "JMAP requests that met a full limit, by limit and outcome (waited, rejected).",
	}, []string{labelLimit, labelOutcome})
)

// Timer measures one RADOS operation.
func Timer(op string) func() {
	start := time.Now()
	return func() { RadosOps.WithLabelValues(op).Observe(time.Since(start).Seconds()) }
}
