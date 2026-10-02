package bench

import (
	"github.com/prometheus/client_golang/prometheus"
)

// casMetric is the store's counter of compare-and-swap retries
// (metrics.CASRetries), read from the default registry so that this
// package needs no store.
const casMetric = "imapceph_rados_cas_retries_total"

// CASKey is one series of the retry counter: the kind of
// compare-and-swap and the class of the object.
type CASKey struct{ Kind, Class string }

// CASReading is the retry counter at one moment, by series.
type CASReading map[CASKey]float64

// CASRetries returns the store's compare-and-swap retries so far.
func CASRetries() CASReading {
	out := CASReading{}

	families, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		return out
	}

	for _, f := range families {
		if f.GetName() != casMetric {
			continue
		}

		for _, m := range f.GetMetric() {
			var k CASKey

			for _, l := range m.GetLabel() {
				switch l.GetName() {
				case "kind":
					k.Kind = l.GetValue()
				case "class":
					k.Class = l.GetValue()
				}
			}

			out[k] += m.GetCounter().GetValue()
		}
	}

	return out
}

// CASDelta is the retries between two readings, per delivery of n.
func CASDelta(before, after CASReading, n int64) CASSummary {
	s := CASSummary{ByKind: map[string]float64{}, ByClass: map[string]float64{}}

	for k, v := range after {
		if d := v - before[k]; d > 0 {
			s.ByKind[k.Kind] += d
			s.ByClass[k.Class] += d
			s.Total += d
		}
	}

	if n > 0 {
		s.PerDelivery = round3(s.Total / float64(n))
	}

	return s
}
