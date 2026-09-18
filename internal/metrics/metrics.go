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
)

// Label names.
const (
	labelOp      = "op"
	labelKind    = "kind"
	labelProto   = "proto"
	labelCommand = "command"
	labelOutcome = "outcome"
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
)

// Timer measures one RADOS operation.
func Timer(op string) func() {
	start := time.Now()
	return func() { RadosOps.WithLabelValues(op).Observe(time.Since(start).Seconds()) }
}
