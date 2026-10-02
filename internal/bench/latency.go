package bench

import (
	"math"
	"sync"
	"time"
)

// Latency is a histogram of durations with logarithmic buckets (2 %
// apart, from 1 µs to beyond an hour), so a bench of millions of
// deliveries keeps a few kilobytes and its quantiles are within 2 %.
type Latency struct {
	mu      sync.Mutex
	buckets []uint64
	count   uint64
	sum     time.Duration
	maxD    time.Duration
}

const (
	latencyBase    = 1.02
	latencyBuckets = 1200 // 1.02^1200 µs is about 7 hours
)

var logBase = math.Log(latencyBase)

func bucketOf(d time.Duration) int {
	us := float64(d) / float64(time.Microsecond)
	if us <= 1 {
		return 0
	}

	return min(int(math.Log(us)/logBase)+1, latencyBuckets-1)
}

// bucketUpper is the upper bound of bucket i.
func bucketUpper(i int) time.Duration {
	return time.Duration(math.Pow(latencyBase, float64(i)) * float64(time.Microsecond))
}

// Add records one duration.
func (l *Latency) Add(d time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()

	if l.buckets == nil {
		l.buckets = make([]uint64, latencyBuckets)
	}

	l.buckets[bucketOf(d)]++
	l.count++
	l.sum += d
	l.maxD = max(l.maxD, d)
}

// Quantile returns the duration below which the fraction q of the
// recorded durations lies (the upper bound of its bucket, at most the
// maximum).
func (l *Latency) Quantile(q float64) time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()

	if l.count == 0 {
		return 0
	}

	rank := uint64(math.Ceil(q * float64(l.count)))
	seen := uint64(0)

	for i, n := range l.buckets {
		if seen += n; seen >= rank && n > 0 {
			return min(bucketUpper(i), l.maxD)
		}
	}

	return l.maxD
}

// LatencySummary is what a report shows of a Latency, in milliseconds.
type LatencySummary struct {
	Count  uint64  `json:"count"`
	MeanMS float64 `json:"mean_ms"`
	P50MS  float64 `json:"p50_ms"`
	P90MS  float64 `json:"p90_ms"`
	P99MS  float64 `json:"p99_ms"`
	MaxMS  float64 `json:"max_ms"`
}

func ms(d time.Duration) float64 { return math.Round(float64(d)/float64(time.Microsecond)) / 1000 }

// Summary returns count, mean, p50, p90, p99 and maximum.
func (l *Latency) Summary() LatencySummary {
	s := LatencySummary{P50MS: ms(l.Quantile(0.5)), P90MS: ms(l.Quantile(0.9)), P99MS: ms(l.Quantile(0.99))}

	l.mu.Lock()
	defer l.mu.Unlock()

	s.Count, s.MaxMS = l.count, ms(l.maxD)
	if l.count > 0 {
		s.MeanMS = ms(l.sum / time.Duration(l.count))
	}

	return s
}
