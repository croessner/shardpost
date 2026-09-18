package cephstore

import "shardpost/internal/metrics"

// metricsHooks keeps the store's dependency on the metrics package in one
// place.
type metricsHooks struct{}

var mh metricsHooks

func (metricsHooks) casRetry(kind string)   { metrics.CASRetries.WithLabelValues(kind).Inc() }
func (metricsHooks) timer(op string) func() { return metrics.Timer(op) }
