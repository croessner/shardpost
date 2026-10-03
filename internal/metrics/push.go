package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// The metrics of the JMAP push worker (stage B18a): its partitions, the
// hand-overs between pods, what it sent and how its scans and lock
// operations fare. Prefix imapceph_jmap_push_ (docs/observability.md).

const labelEvent = "event"

var (
	// PushPartitionsHeld is the number of partitions this process holds.
	PushPartitionsHeld = promauto.NewGauge(prometheus.GaugeOpts{
		Namespace: namespace, Subsystem: subsystemJMAP, Name: "push_partitions_held",
		Help: "Push partitions this process holds.",
	})
	// PushPartitionsUnowned is the number of partitions no fresh member
	// holds, as this process read the membership last (every process
	// reports it; alert on the maximum).
	PushPartitionsUnowned = promauto.NewGauge(prometheus.GaugeOpts{
		Namespace: namespace, Subsystem: subsystemJMAP, Name: "push_partitions_unowned",
		Help: "Push partitions no fresh member holds, as this process last read the membership.",
	})
	// PushPartitionEvents counts what happened to the partitions of this
	// process, by event: taken, taken_orphan, released, shed, lost,
	// expired, rotated, rotate_failed, members_stale.
	PushPartitionEvents = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace, Subsystem: subsystemJMAP, Name: "push_partition_events_total",
		Help: "Push partition events of this process (taken, taken_orphan, released, shed, lost, expired, rotated, rotate_failed, members_stale).",
	}, []string{labelEvent})
	// PushHandover is the time from taking a partition's lock to the end
	// of the first pass of every watcher of the partition.
	PushHandover = promauto.NewHistogram(prometheus.HistogramOpts{
		Namespace: namespace, Subsystem: subsystemJMAP, Name: "push_handover_seconds",
		Help:    "Time from taking a push partition to the end of the first pass of its watchers.",
		Buckets: []float64{.1, .25, .5, 1, 2, 3, 5, 8, 10, 15, 30, 60},
	})
	// PushGap is the time from the moment the previous holder released a
	// partition in order (its membership timestamp) to the take by this
	// process (orderly hand-overs only).
	PushGap = promauto.NewHistogram(prometheus.HistogramOpts{
		Namespace: namespace, Subsystem: subsystemJMAP, Name: "push_gap_seconds",
		Help:    "Time from the orderly release of a push partition to its take by this process.",
		Buckets: []float64{.05, .1, .25, .5, 1, 2, 3, 5, 8, 10, 15, 30, 60},
	})
	// PushWatchedAccounts is the number of accounts this process watches.
	PushWatchedAccounts = promauto.NewGauge(prometheus.GaugeOpts{
		Namespace: namespace, Subsystem: subsystemJMAP, Name: "push_watched_accounts",
		Help: "Accounts with push subscriptions this process watches.",
	})
	// PushSent counts the pushes by outcome (delivered, gone, slow,
	// refused).
	PushSent = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace, Subsystem: subsystemJMAP, Name: "push_sent_total",
		Help: "Web Push requests of the push worker by outcome (delivered, gone, slow, refused).",
	}, []string{labelOutcome})
	// PushDelay is the time from the event that woke a watcher to the
	// push it sent (the coalescing plus the load).
	PushDelay = promauto.NewHistogram(prometheus.HistogramOpts{
		Namespace: namespace, Subsystem: subsystemJMAP, Name: "push_delay_seconds",
		Help:    "Time from a watch event to the push it led to.",
		Buckets: []float64{.1, .25, .5, 1, 1.5, 2, 3, 5, 10, 30},
	})
	// PushPending is the number of subscriptions with a backlog (an
	// acknowledgement behind the account state), as counted at the last
	// save of each watched account.
	PushPending = promauto.NewGauge(prometheus.GaugeOpts{
		Namespace: namespace, Subsystem: subsystemJMAP, Name: "push_pending",
		Help: "Push subscriptions with a backlog (acknowledgement behind the account state).",
	})
	// PushScanGets counts the subscription records a scan read, by
	// cause: changed (the checksum in the index moved), nosum (an index
	// value without checksum, written by an older server), full (the
	// hourly full pass).
	PushScanGets = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace, Subsystem: subsystemJMAP, Name: "push_scan_gets_total",
		Help: "Subscription records the push scan read, by cause (changed, nosum, full).",
	}, []string{labelCause})
	// PushVerifyQueueAge is the age of the oldest verification waiting,
	// as the process that sends them sees it.
	PushVerifyQueueAge = promauto.NewGauge(prometheus.GaugeOpts{
		Namespace: namespace, Subsystem: subsystemJMAP, Name: "push_verifyq_age_seconds",
		Help: "Age of the oldest push verification waiting, set by the process that sends them.",
	})
	// PushLockOp is the duration of the lock operations of the partition
	// leases, by operation (take, renew, release): a hung lock object
	// shows here.
	PushLockOp = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: namespace, Subsystem: subsystemJMAP, Name: "push_lock_op_seconds",
		Help:    "Duration of the lock operations of the push partition leases, by operation (take, renew, release).",
		Buckets: []float64{.001, .0025, .005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10, 30, 60},
	}, []string{labelOp})
)
