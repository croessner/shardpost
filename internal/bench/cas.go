package bench

import (
	"github.com/prometheus/client_golang/prometheus"
)

// casMetric is the store's counter of compare-and-swap retries
// (metrics.CASRetries), read from the default registry so that this
// package needs no store.
const casMetric = "imapceph_rados_cas_retries_total"

// CASRetries returns the store's compare-and-swap retries so far, by
// object kind.
func CASRetries() map[string]float64 {
	out := map[string]float64{}

	families, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		return out
	}

	for _, f := range families {
		if f.GetName() != casMetric {
			continue
		}

		for _, m := range f.GetMetric() {
			kind := ""

			for _, l := range m.GetLabel() {
				if l.GetName() == "kind" {
					kind = l.GetValue()
				}
			}

			out[kind] += m.GetCounter().GetValue()
		}
	}

	return out
}

// CASDelta is the retries between two readings, per delivery of n.
func CASDelta(before, after map[string]float64, n int64) CASSummary {
	s := CASSummary{ByKind: map[string]float64{}}

	for kind, v := range after {
		if d := v - before[kind]; d > 0 {
			s.ByKind[kind] = d
			s.Total += d
		}
	}

	if n > 0 {
		s.PerDelivery = round3(s.Total / float64(n))
	}

	return s
}
