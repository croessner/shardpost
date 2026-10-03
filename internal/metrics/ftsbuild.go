package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// Metrics of the online runs of the full-text index (stage B14i):
// fts_layout and fts_rebuild without the account's maintenance lock.
const (
	labelPhase = "phase"
	labelVia   = "via"
)

var (
	// FTSBuildRuns counts online runs by mode ("copy", "reindex") and
	// result ("ok", "busy", "lost", "error", "noop", "deferred",
	// "unsupported", "mark_off": the mark of online runs went after the
	// plan).
	FTSBuildRuns = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace, Subsystem: subsystemFTS, Name: "build_runs_total",
		Help: "Online runs of the full-text index (fts_layout, fts_rebuild without lock), by mode and result.",
	}, []string{labelMode, labelResult})
	// FTSBuildPhase is the duration of the phases of an online run (O3
	// freeze, copy, reindex, catchup, switch, cleanup).
	FTSBuildPhase = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: namespace, Subsystem: subsystemFTS, Name: "build_phase_seconds",
		Help:    "Duration of the phases of an online run of the full-text index.",
		Buckets: prometheus.ExponentialBuckets(0.01, 4, 10),
	}, []string{labelPhase})
	// FTSBuildRegistered counts the documents writers recorded for an
	// online run: in the id map's compare-and-swap ("alloc") or after
	// they met the run's freeze ("crossing").
	FTSBuildRegistered = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace, Subsystem: subsystemFTS, Name: "build_registered_total",
		Help: "Documents recorded for an online run of the full-text index, by where (alloc, crossing).",
	}, []string{labelVia})
	// FTSBuildCatchupRounds is the number of catch-up rounds of an
	// online run until it switched.
	FTSBuildCatchupRounds = promauto.NewHistogram(prometheus.HistogramOpts{
		Namespace: namespace, Subsystem: subsystemFTS, Name: "build_catchup_rounds",
		Help:    "Catch-up rounds of an online run of the full-text index before its switch.",
		Buckets: []float64{1, 2, 3, 5, 8, 13, 21, 34},
	})
	// FTSBuildHold is how long an online run held new id allocations
	// before its switch (only runs that did not converge otherwise).
	FTSBuildHold = promauto.NewHistogram(prometheus.HistogramOpts{
		Namespace: namespace, Subsystem: subsystemFTS, Name: "build_hold_seconds",
		Help:    "Holds of new id allocations before the switch of an online run of the full-text index.",
		Buckets: []float64{0.1, 0.25, 0.5, 1, 2, 3, 5},
	})
	// FTSSoftFreezePass counts operations that wrote through the freeze
	// of an online run ("index", "remove").
	FTSSoftFreezePass = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace, Subsystem: subsystemFTS, Name: "soft_freeze_pass_total",
		Help: "Full-text operations that wrote through the freeze of an online run, by operation (index, remove).",
	}, []string{labelOp})
	// FTSBuildWindowDeferred counts runs the fts_due worker, a batch or
	// the run itself put off to the next reshard window.
	FTSBuildWindowDeferred = promauto.NewCounter(prometheus.CounterOpts{
		Namespace: namespace, Subsystem: subsystemFTS, Name: "build_window_deferred_total",
		Help: "Full-text runs put off to the next reshard window (fts.reshard_window).",
	})
	// FTSBuildWindowTooSmall counts runs not started because their
	// estimate is longer than the whole reshard window.
	FTSBuildWindowTooSmall = promauto.NewCounter(prometheus.CounterOpts{
		Namespace: namespace, Subsystem: subsystemFTS, Name: "build_window_too_small_total",
		Help: "Full-text runs not started because their estimate is longer than the reshard window.",
	})
)
