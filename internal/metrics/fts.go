package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// Metrics of the full-text index layouts (stage B14b, layout 3).
const (
	subsystemFTS = "fts"
	labelReason  = "reason"
	labelMode    = "mode"
)

var (
	// FTSFrozenWaits counts the waits of an indexing for a conversion
	// that froze the account's posting objects, by how they ended:
	// "moved" (the conversion switched layouts, the indexing went on in
	// the new one), "thawed" (the freeze went away without a switch) and
	// "timeout" (temporary failure after fts.frozen_wait_seconds; with a
	// wait of 0 at once).
	FTSFrozenWaits = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace, Subsystem: subsystemFTS, Name: "frozen_waits_total",
		Help: "Waits of an indexing for a frozen full-text index, by result (moved, thawed, timeout).",
	}, []string{labelResult})
	// FTSStaleFreezeLifted counts freezes a writer found without a
	// conversion that names them and lifted.
	FTSStaleFreezeLifted = promauto.NewCounter(prometheus.CounterOpts{
		Namespace: namespace, Subsystem: subsystemFTS, Name: "stale_freeze_lifted_total",
		Help: "Freezes of full-text objects lifted by a writer because no conversion named them any more.",
	})
	// FTSLayoutRetries counts the restarts of an indexing, a removal or
	// a search in another index generation, by reason ("generation":
	// the layout record names another generation; "frozen": after a
	// wait for a frozen object).
	FTSLayoutRetries = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace, Subsystem: subsystemFTS, Name: "layout_retries_total",
		Help: "Restarts of a full-text index operation in another index generation, by reason.",
	}, []string{labelReason})
	// FTSShardKeys is the key count of the last layout 3 object a
	// conversion wrote, by kind (postings, dictionary, headers): what
	// the growth limit of fts.shards_max is watched with.
	FTSShardKeys = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: namespace, Subsystem: subsystemFTS, Name: "shard_keys",
		Help: "Keys of the largest object of the last full-text conversion, by kind (postings, dictionary, headers).",
	}, []string{labelKind})
	// FTSReshardDue counts accounts a writer queued for a reshard
	// (ix.fts_due).
	FTSReshardDue = promauto.NewCounter(prometheus.CounterOpts{
		Namespace: namespace, Subsystem: subsystemFTS, Name: "reshard_due_total",
		Help: "Accounts queued for a full-text reshard because a shard outgrew fts.shard_target_keys.",
	})
	// FTSLayoutRuns counts conversions and reshards of the full-text
	// index by mode ("convert", "reshard", "noop", "import") and result
	// ("ok", "error").
	FTSLayoutRuns = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace, Subsystem: subsystemFTS, Name: "layout_runs_total",
		Help: "Full-text layout conversions and reshards, by mode and result.",
	}, []string{labelMode, labelResult})
	// FTSShardMissing counts layout 3 objects an operation found missing
	// in the generation the layout record names (repair: fts_rebuild).
	FTSShardMissing = promauto.NewCounter(prometheus.CounterOpts{
		Namespace: namespace, Subsystem: subsystemFTS, Name: "shard_missing_total",
		Help: "Full-text objects missing in the generation the layout record names (repair with fts_rebuild).",
	})
	// FTSLayoutConfigured is fts.layout of this server (2 or 3).
	FTSLayoutConfigured = promauto.NewGauge(prometheus.GaugeOpts{
		Namespace: namespace, Subsystem: subsystemFTS, Name: "layout_configured",
		Help: "Configured layout of new full-text indexes (fts.layout).",
	})
	// FTSLayoutEffective is the layout new indexes of this server get as
	// of the last read of the cluster mark (at startup and whenever an
	// index is created): 3 only with fts.layout 3 and the mark set; 0
	// while a server with fts.layout 3 has not read the mark yet.
	FTSLayoutEffective = promauto.NewGauge(prometheus.GaugeOpts{
		Namespace: namespace, Subsystem: subsystemFTS, Name: "layout_effective",
		Help: "Layout new full-text indexes get: fts.layout combined with the layout 3 cluster mark at its last read.",
	})
	// FTSShardsCapped counts reshards whose target was capped at
	// fts.shards_max: the objects grow beyond fts.shard_target_keys.
	FTSShardsCapped = promauto.NewCounter(prometheus.CounterOpts{
		Namespace: namespace, Subsystem: subsystemFTS, Name: "shards_capped_total",
		Help: "Full-text reshards whose shard count was capped at fts.shards_max.",
	})
)
