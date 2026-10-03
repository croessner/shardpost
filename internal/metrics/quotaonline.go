package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// Metrics of the online quota recount (stage B16) and of the mailbox
// lock of mailbox_delete (stage B17).
const (
	subsystemQuota   = "quota"
	subsystemMailbox = "mailbox"
	subsystemJobs    = "jobs"
	labelResource    = "resource"
	labelProtocol    = "protocol"
)

var (
	// QuotaRecountRuns counts quota_recalc runs by mode ("offline",
	// "online"), result ("ok", "busy", "lost", "error", "noop") and, for
	// busy, the reason (intent, move_intent, import_lock, mailbox_lock,
	// scrub, foreign_run, switch; "" otherwise). A busy run is planned
	// again; on a distribution account with dozens of registry writes a
	// second it is an expected result, not an alarm.
	QuotaRecountRuns = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace, Subsystem: subsystemQuota, Name: "recount_runs_total",
		Help: "quota_recalc runs by mode, result and (busy) reason.",
	}, []string{labelMode, labelResult, labelReason})
	// QuotaRecountPhase is the duration of the phases of an online
	// recount (barrier, list, fold, switch).
	QuotaRecountPhase = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: namespace, Subsystem: subsystemQuota, Name: "recount_phase_seconds",
		Help:    "Duration of the phases of an online quota recount.",
		Buckets: prometheus.ExponentialBuckets(0.01, 4, 10),
	}, []string{labelPhase})
	// QuotaRecountCaptures counts what writers captured for online
	// recounts by kind (reserve, rollback, release, mailbox, move,
	// abort).
	QuotaRecountCaptures = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace, Subsystem: subsystemQuota, Name: "recount_captures_total",
		Help: "Usage changes captured for online quota recounts, by kind.",
	}, []string{labelKind})
	// QuotaRecountDrift is the difference between the counted and the
	// stored usage a recount found, by resource (bytes, messages).
	QuotaRecountDrift = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: namespace, Subsystem: subsystemQuota, Name: "recount_drift",
		Help:    "Absolute difference between the counted and the stored usage of a quota recount, by resource.",
		Buckets: prometheus.ExponentialBuckets(1, 8, 12),
	}, []string{labelResource})
	// QuotaRecountHold is how long an online recount held the usage
	// writers while it switched.
	QuotaRecountHold = promauto.NewHistogram(prometheus.HistogramOpts{
		Namespace: namespace, Subsystem: subsystemQuota, Name: "recount_hold_seconds",
		Help:    "How long an online quota recount held the usage writers while it switched.",
		Buckets: []float64{.1, .25, .5, 1, 2, 3, 5},
	})
	// QuotaRecountDeadIntents counts intents an online recount gave up
	// waiting for (older than quota.recount_dead_intent_seconds): a
	// crash or an operation hanging for that long. Alarm when above 0.
	QuotaRecountDeadIntents = promauto.NewCounter(prometheus.CounterOpts{
		Namespace: namespace, Subsystem: subsystemQuota, Name: "recount_dead_intents_total",
		Help: "Intents an online quota recount took for abandoned (a crash or an operation hanging longer than the dead-intent time).",
	})
	// ScrubQuotaSkipped counts scrubs that skipped an account or stopped
	// their repairs by reason ("qr": an online recount is under way,
	// "sr": the repair mark could not be renewed, "mblock": a
	// mailbox_delete locks mailboxes).
	ScrubQuotaSkipped = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace, Subsystem: subsystemScrub, Name: "quota_skipped_total",
		Help: "Scrubs that skipped an account or stopped repairing because of an online recount, a lost repair mark or a mailbox lock.",
	}, []string{labelReason})
	// MailboxLockSeconds is how long a mailbox_delete held its mailbox
	// lock (lock to unlock, stage B17).
	MailboxLockSeconds = promauto.NewHistogram(prometheus.HistogramOpts{
		Namespace: namespace, Subsystem: subsystemMailbox, Name: "lock_seconds",
		Help:    "How long a mailbox_delete job held its mailbox lock.",
		Buckets: prometheus.ExponentialBuckets(1, 2, 10),
	})
	// MailboxLockRefusals counts operations refused on a locked mailbox
	// by protocol and operation.
	MailboxLockRefusals = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace, Subsystem: subsystemMailbox, Name: "lock_refusals_total",
		Help: "Operations refused because a mailbox_delete locks the mailbox, by protocol and operation.",
	}, []string{labelProtocol, labelOp})
	// DeliveryRedirected counts Sieve fileinto deliveries that went to
	// INBOX because the target could not take them, by reason.
	DeliveryRedirected = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace, Subsystem: subsystemLMTP, Name: "fileinto_redirected_total",
		Help: "Sieve fileinto deliveries redirected to INBOX, by reason.",
	}, []string{labelReason})
	// IMAPSessionsClosed counts IMAP sessions the server closed with
	// BYE because their selected mailbox is being deleted or was
	// deleted, by reason.
	IMAPSessionsClosed = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace, Subsystem: subsystemIMAP, Name: "sessions_closed_total",
		Help: "IMAP sessions closed with BYE because of their selected mailbox, by reason.",
	}, []string{labelReason})
	// MailboxDeleteRuns counts mailbox_delete runs by mode ("offline",
	// "online") and result ("ok", "busy", "drift", "error").
	MailboxDeleteRuns = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace, Subsystem: subsystemJobs, Name: "mailbox_delete_runs_total",
		Help: "mailbox_delete runs by mode and result.",
	}, []string{labelMode, labelResult})
)

// Reasons of IMAPSessionsClosed and DeliveryRedirected.
const (
	ReasonMailboxLocked  = "mailbox_locked"
	ReasonMailboxDeleted = "mailbox_deleted"
	ReasonLocked         = "locked"
)
