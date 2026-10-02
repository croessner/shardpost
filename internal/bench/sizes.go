package bench

import (
	"fmt"
	"math/rand/v2"
	"strconv"
	"strings"
)

// DefaultSizes is the default message size distribution: 60 % small,
// 30 % medium, 10 % large mail.
const DefaultSizes = "60:1KiB-8KiB,30:8KiB-64KiB,10:64KiB-1MiB"

// maxMessageSize bounds a bucket (the store refuses larger messages
// long before; a bench of such sizes measures nothing per account).
const maxMessageSize = 64 << 20

// SizeBucket is one range of a SizeDist: Weight parts of the messages
// are between Min and Max bytes (inclusive), uniformly.
type SizeBucket struct {
	Weight   int `json:"weight"`
	Min, Max int
}

// SizeDist is a distribution of message sizes.
type SizeDist struct {
	Buckets []SizeBucket
	total   int
}

// ParseSizes parses "<weight>:<min>-<max>,..." with sizes in bytes or
// with the suffix KiB, MiB (or K, M); weights are relative.
func ParseSizes(s string) (SizeDist, error) {
	var d SizeDist

	for part := range strings.SplitSeq(s, ",") {
		b, err := parseBucket(strings.TrimSpace(part))
		if err != nil {
			return SizeDist{}, fmt.Errorf("size bucket %q: %w", part, err)
		}

		d.Buckets = append(d.Buckets, b)
		d.total += b.Weight
	}

	if d.total <= 0 {
		return SizeDist{}, fmt.Errorf("size distribution %q has no weight", s)
	}

	return d, nil
}

func parseBucket(s string) (SizeBucket, error) {
	w, rng, ok := strings.Cut(s, ":")
	if !ok {
		return SizeBucket{}, fmt.Errorf("expected <weight>:<min>-<max>")
	}

	weight, err := strconv.Atoi(strings.TrimSpace(w))
	if err != nil || weight < 0 {
		return SizeBucket{}, fmt.Errorf("bad weight %q", w)
	}

	lo, hi, ok := strings.Cut(rng, "-")
	if !ok {
		return SizeBucket{}, fmt.Errorf("expected <min>-<max>")
	}

	minSize, err := parseSize(lo)
	if err != nil {
		return SizeBucket{}, err
	}

	maxSize, err := parseSize(hi)
	if err != nil {
		return SizeBucket{}, err
	}

	if minSize < minMessageSize || maxSize < minSize || maxSize > maxMessageSize {
		return SizeBucket{}, fmt.Errorf("range must satisfy %d <= min <= max <= %d", minMessageSize, maxMessageSize)
	}

	return SizeBucket{Weight: weight, Min: minSize, Max: maxSize}, nil
}

// parseSize parses a byte count with an optional binary suffix.
func parseSize(s string) (int, error) {
	s = strings.TrimSpace(s)
	mult := 1

	for _, suf := range []struct {
		s string
		m int
	}{{"KiB", 1 << 10}, {"MiB", 1 << 20}, {"K", 1 << 10}, {"M", 1 << 20}} {
		if v, ok := strings.CutSuffix(s, suf.s); ok {
			s, mult = v, suf.m

			break
		}
	}

	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil || n < 0 {
		return 0, fmt.Errorf("bad size %q", s)
	}

	return n * mult, nil
}

// Pick draws a size.
func (d SizeDist) Pick(r *rand.Rand) int {
	n := r.IntN(d.total)

	for _, b := range d.Buckets {
		if n < b.Weight {
			return b.Min + r.IntN(b.Max-b.Min+1)
		}

		n -= b.Weight
	}

	last := d.Buckets[len(d.Buckets)-1]

	return last.Max
}

// String formats the distribution the way ParseSizes reads it.
func (d SizeDist) String() string {
	parts := make([]string, 0, len(d.Buckets))
	for _, b := range d.Buckets {
		parts = append(parts, fmt.Sprintf("%d:%d-%d", b.Weight, b.Min, b.Max))
	}

	return strings.Join(parts, ",")
}
