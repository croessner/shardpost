package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// A labelled metric has no series until its first event, so a counter
// nobody incremented is not a zero in Prometheus but nothing at all:
// a dashboard cannot tell "no failure yet" from "not measured". The
// processes with a scraped listener therefore create, when they start,
// the series of the parts they really run (Init), with every label
// value that part can produce. A part that is switched off creates
// none, and FeatureEnabled says so: zero then means "nothing happened",
// an absent series with the feature off means "does not apply", and an
// absent series with the feature on means the collection is broken.
//
// Label values that are not a small fixed set (the IMAP command of
// imapceph_commands_total, the operation and code of the admin API)
// are created only for the events an alert or an overview reads.

// Features of FeatureEnabled.
const (
	FeatureAuthCache         = "auth_cache"
	FeatureLMTP              = "lmtp"
	FeatureJMAP              = "jmap"
	FeatureJMAPPerIPLimit    = "jmap_per_ip_limit"
	FeatureJobs              = "jobs"
	FeatureLearning          = "learning"
	FeatureDelayedSubmission = "delayed_submission"
	FeatureScrubShadow       = "scrub_shadow"
	FeatureScrubListing      = "scrub_listing"
	FeaturePurgeListing      = "purge_listing"
	FeatureQuotaEvents       = "quota_events"
	FeatureExtPrograms       = "ext_programs"
)

// FeatureEnabled is 1 for a part of the server that its configuration
// switches on and 0 for one it switches off; the event metrics of a
// part that is off have no series.
var FeatureEnabled = promauto.NewGaugeVec(prometheus.GaugeOpts{
	Namespace: namespace, Name: "feature_enabled",
	Help: "Parts of this process by configuration: 1 enabled, 0 disabled (its event metrics then have no series).",
}, []string{"feature"})

// Features are the parts a process runs, as its configuration says.
type Features struct {
	// Server is a process with the protocol front-ends (serve); the
	// others are the push worker and the job runner.
	Server bool
	// Push is the JMAP push worker.
	Push bool
	// LMTP: the LMTP listener is open.
	LMTP bool
	// AuthCache: the auth cache is in front of the backend.
	AuthCache bool
	// Jobs: the process runs maintenance jobs (the nightly scrub batch
	// among them).
	Jobs bool
	// Workers: the process runs the background workers (a server with
	// workers.run, the runner): the due queues are read here.
	Workers bool
	// DelayedOffered: clients can schedule and cancel delayed JMAP
	// submissions here. DelayedSubmission: the process runs the worker
	// that sends them.
	DelayedOffered    bool
	DelayedSubmission bool
	// ScrubShadow: scrubs compare the listing with the intents
	// (scrub.orphan_source shadow). ScrubListing: scrubs list at all
	// (shadow or listing). PurgeListing: purges list (purge.listing).
	ScrubShadow  bool
	ScrubListing bool
	PurgeListing bool
}

// each creates the series of every value of a one-label vector.
func each[T any](with func(...string) T, values ...string) {
	for _, v := range values {
		with(v)
	}
}

// under creates the series of one value of the first label with every
// given value of the second.
func under[T any](with func(...string) T, first string, second ...string) {
	for _, v := range second {
		with(first, v)
	}
}

// Init creates the series of the event metrics of the parts f names.
// It is called once, by a process that serves /metrics.
func Init(f Features) {
	initStore(f)

	if f.Push {
		initPush()
	}

	if f.Server {
		initFrontEnds(f)
	}

	// The servers write mail and its indexes, the workers on their
	// behalf (jobs, the upload expiry, the learning and submission
	// queues).
	if f.Server || f.Workers {
		initWriters()
		initQueues()
	}

	if f.Jobs {
		initJobs(f)
		initRuns()
	}

	if f.DelayedOffered {
		each(Submissions.WithLabelValues, "scheduled", "canceled")
	}

	if f.DelayedSubmission {
		each(Submissions.WithLabelValues, "sent", "failed", "unknown", "retried", "conflict")
	}
}

// SetFeatures publishes which parts are on.
func SetFeatures(enabled map[string]bool) {
	for name, on := range enabled {
		v := 0.0
		if on {
			v = 1
		}

		FeatureEnabled.WithLabelValues(name).Set(v)
	}
}

// initStore: what every process with a store does.
func initStore(f Features) {
	each(RadosOps.WithLabelValues, "omap_read", "omap_cas", "index_page", "index_write")

	// A notify goes to the object a process writes: every process writes
	// queues and control objects, the mailbox and account objects are
	// written where mail is.
	each(NotifyLatency.WithLabelValues, "queue")

	under(CASRetries.WithLabelValues, "omap", "other", "index")
	under(CASRetries.WithLabelValues, "index", "index")
	each(KVIndexErrors.WithLabelValues, "jmap/push/acct", "servers")

	if f.Push {
		return
	}

	each(RadosOps.WithLabelValues, "header_read", "header_cas", "list_entries", "get_entries")
	each(NotifyLatency.WithLabelValues, "mailbox", "account")
	under(CASRetries.WithLabelValues, "omap", "fts", "ftsmap", "registry", "mbox", "account", "msg")
	under(CASRetries.WithLabelValues, "header", "mbox")
	under(CASRetries.WithLabelValues, "modseq_restamp", "mbox")
	under(CASRetries.WithLabelValues, "refs", "msg")
	each(KVIndexErrors.WithLabelValues, "job", "plan", "idem", "learning", "learning/owner")
}

// initPush: the push worker.
func initPush() {
	each(PushPartitionEvents.WithLabelValues, "taken", "taken_orphan", "released", "shed", "lost", "expired",
		"rotated", "rotate_failed", "members_stale")
	each(PushSent.WithLabelValues, "delivered", "gone", "slow", "refused")
	each(PushScanGets.WithLabelValues, "changed", "nosum", "full")
	each(PushLockOp.WithLabelValues, "take", "renew", "release")
}

// initFrontEnds: IMAP, POP3, ManageSieve, LMTP and the auth cache. Of
// the commands only the events that are no command of a client's
// choosing are created: logins, the connection limit and the polls.
func initFrontEnds(f Features) {
	each(Connections.WithLabelValues, "imap", "pop3", "managesieve")

	each(func(outcome ...string) prometheus.Counter {
		return Commands.WithLabelValues("imap", "LOGIN", outcome[0])
	},
		"ok", "no", "bad", "tempfail", "failed")

	Commands.WithLabelValues("imap", "connect", "limit_ip")
	Commands.WithLabelValues("imap", "poll", "incremental")
	Commands.WithLabelValues("imap", "poll", "reload")
	Commands.WithLabelValues("pop3", "LOGIN", "ok")
	Commands.WithLabelValues("pop3", "LOGIN", "failed")
	Commands.WithLabelValues("pop3", "RETR", "ok")
	Commands.WithLabelValues("pop3", "DELE", "ok")
	Commands.WithLabelValues("managesieve", "AUTHENTICATE", "ok")
	Commands.WithLabelValues("managesieve", "AUTHENTICATE", "failed")
	Commands.WithLabelValues("managesieve", "AUTHENTICATE", "tempfail")

	CommandLatency.WithLabelValues("imap", "LOGIN")
	CommandLatency.WithLabelValues("pop3", "PASS")
	CommandLatency.WithLabelValues("managesieve", "AUTHENTICATE")

	each(IMAPWatches.WithLabelValues, "mailbox", "account")
	each(IMAPWatchSubscribers.WithLabelValues, "mailbox", "account")
	each(IMAPSessionsClosed.WithLabelValues, "mailbox_locked", "mailbox_deleted")
	under(MailboxLockRefusals.WithLabelValues, "jmap", "email_set", "email_create", "mailbox_set")

	if f.LMTP {
		each(Deliveries.WithLabelValues, "ok", "failed")
		each(DeliveryRedirected.WithLabelValues, "locked")
	}

	if f.AuthCache {
		under(AuthCache.WithLabelValues, "authenticate", "hit", "miss", "shared")
		under(AuthCache.WithLabelValues, "lookup", "hit", "miss", "shared", "bypass")
		each(AuthCacheEntries.WithLabelValues, "authenticate", "lookup")
		each(AuthBackend.WithLabelValues, "authenticate", "lookup")
		under(AuthCacheFlushes.WithLabelValues, "account", "local", "remote")
		under(AuthCacheFlushes.WithLabelValues, "all", "local", "remote", "resync")
	}
}

// initWriters: what the writers of a server count while they index and
// account for mail.
func initWriters() {
	each(FTSFrozenWaits.WithLabelValues, "moved", "thawed", "timeout")
	each(FTSLayoutRetries.WithLabelValues, "generation", "frozen")
	each(FTSShardsCapped.WithLabelValues, "postings", "dictionary", "headers")
	each(FTSDictKeysWritten.WithLabelValues, "trigram", "prefix")
	each(FTSBuildRegistered.WithLabelValues, "alloc", "crossing")
	each(FTSSoftFreezePass.WithLabelValues, "index", "remove")
	each(QuotaRecountCaptures.WithLabelValues, "reserve", "rollback", "release", "mailbox", "move", "abort")
}

// initQueues: the due queues of the servers and the job runner.
func initQueues() {
	for _, q := range []string{"jobs/claimable", "jobs/expiry", "learning", "uploads", "submission", "fts_due"} {
		for _, op := range []string{"move", "due"} {
			QueueOps.WithLabelValues(q, op, "ok")
			QueueOps.WithLabelValues(q, op, "error")
		}

		QueueStale.WithLabelValues(q, "unparsable")
	}

	under(QueueStale.WithLabelValues, "jobs/claimable", "rescheduled", "removed", "skipped")
	under(QueueStale.WithLabelValues, "submission", "rescheduled", "removed", "skipped")
	under(QueueStale.WithLabelValues, "learning", "rescheduled", "removed")
}

// initJobs: the job runner and the scrub.
func initJobs(f Features) {
	each(JobWakeups.WithLabelValues, "poll", "notify")
	each(JobRunning.WithLabelValues, "index_check", "scrub", "quota_recalc", "acl_reconcile", "fts_rebuild",
		"fts_dict_convert", "fts_layout", "mailbox_name_convert", "attachment_backfill", "mailbox_delete", "expunge",
		"purge", "batch")
	under(BatchAccounts.WithLabelValues, "scrub", "done", "skipped", "gone", "deferred", "failed", "later", "unsupported")

	each(Scrub.WithLabelValues, "delivery_intent", "move_intent", "retired_mailbox", "uncovered_entry", "missing_blob",
		"refs_mismatch", "orphan_blob", "stale_marks", "quota_drift", "deletion_finished", "creation_undone",
		"stale_metadata", "quota_capture_leftover")
	each(ScrubQuotaSkipped.WithLabelValues, "qr", "mblock", "sr")

	if f.ScrubListing {
		each(Scrub.WithLabelValues, "unknown_shard")
		each(RadosListings.WithLabelValues, "scrub_orphans", "scrub_unknown_index")
	}

	if f.ScrubShadow {
		each(ScrubShadowDiff.WithLabelValues, "orphan_missed", "unknown_missed", "marks_missed", "orphan_extra")
	}

	if f.PurgeListing {
		each(RadosListings.WithLabelValues, "purge_objects")
	}

	// The listings a running process may still make whatever the
	// configuration: the ones that should stay at zero.
	each(RadosListings.WithLabelValues, "admin_kv", "learning_all", "learning_purge_owner")
}

// initRuns: the runs of the jobs that convert, rebuild, recount and
// delete. The runs that need a mode of their own (an online recount, a
// mailbox deletion under the mailbox lock, an online build) are created
// as well: they are started on request, whatever the configuration
// says about the default.
func initRuns() {
	under(FTSLayoutRuns.WithLabelValues, "convert", "ok", "error")
	under(FTSLayoutRuns.WithLabelValues, "reshard", "ok", "error")
	under(FTSLayoutRuns.WithLabelValues, "noop", "ok")
	under(FTSBuildRuns.WithLabelValues, "copy", "ok", "lost", "error", "busy", "mark_off", "deferred", "unsupported", "noop")
	under(FTSBuildRuns.WithLabelValues, "reindex", "ok", "lost", "error", "busy", "mark_off", "deferred", "unsupported", "noop")
	each(FTSBuildPhase.WithLabelValues, "O2", "O3", "O5", "O6", "O8")

	recount := func(v ...string) prometheus.Counter { return QuotaRecountRuns.WithLabelValues(v[0], v[1], "") }
	busy := func(reason ...string) prometheus.Counter {
		return QuotaRecountRuns.WithLabelValues("online", "busy", reason[0])
	}

	under(recount, "offline", "ok", "error")
	under(recount, "online", "ok", "lost", "error")
	each(busy, "intent", "move_intent", "import_lock", "mailbox_lock", "scrub", "foreign_run", "switch")

	each(QuotaRecountPhase.WithLabelValues, "barrier", "list", "fold", "switch")
	each(QuotaRecountDrift.WithLabelValues, "bytes", "messages")

	under(MailboxDeleteRuns.WithLabelValues, "offline", "ok", "drift", "error")
	under(MailboxDeleteRuns.WithLabelValues, "online", "ok", "busy", "drift", "error")
}
