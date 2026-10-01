package model

import (
	"errors"
	"fmt"
	"math/rand"
	"slices"
	"sort"
	"testing"
)

func trimKey(seq uint64) string { return fmt.Sprintf("c:%020d", seq) }

// sliceReader serves a sorted key list page by page and counts the calls.
func sliceReader(keys []string, calls *int) LogPageReader {
	return func(after string, limit int) ([]string, error) {
		*calls++

		i := sort.SearchStrings(keys, after)
		if i < len(keys) && keys[i] == after {
			i++
		}

		return slices.Clone(keys[i:min(i+limit, len(keys))]), nil
	}
}

// oldTrim is the count-based rule PlanLogTrim replaces, as trimLog and
// trimAccountLog ran it: read the whole log, keep the newest retention
// records, and drop the rest unless h:logstart (have, as stored) is
// already at or past the new start.
func oldTrim(keys []string, have uint64, retention int) (uint64, []string) {
	if len(keys) <= retention {
		return 0, nil
	}

	var start uint64

	_, _ = fmt.Sscanf(keys[len(keys)-retention][2:], "%d", &start)
	if have >= start {
		return 0, nil
	}

	return start, slices.Clone(keys[:len(keys)-retention])
}

func newTrim(t *testing.T, keys []string, head, have uint64, retention, page int) (uint64, []string) {
	t.Helper()

	calls := 0

	start, drop, err := PlanLogTrim(head, have, retention, page, trimKey, sliceReader(keys, &calls))
	if err != nil {
		t.Fatal(err)
	}

	// The CAS of the callers refuses to move h:logstart backwards.
	if start != 0 && have >= start {
		return 0, nil
	}

	return start, drop
}

func keysOf(seqs []uint64) []string {
	out := make([]string, 0, len(seqs))
	for _, s := range seqs {
		out = append(out, trimKey(s))
	}

	sort.Strings(out)

	return out
}

// TestPlanLogTrimMailboxEquivalence checks that the new rule removes
// exactly what the old one removed on logs that follow the invariants of
// the mailbox change log: contiguous records above h:logstart (from 2 on
// a mailbox that was never trimmed, which has no c:1), plus records an
// import left below h:logstart.
func TestPlanLogTrimMailboxEquivalence(t *testing.T) {
	rng := rand.New(rand.NewSource(1))

	for i := 0; i < 20000; i++ {
		retention := 1 + rng.Intn(20)
		page := 1 + rng.Intn(8)

		var (
			seqs     []uint64
			logStart uint64
			head     uint64
		)

		switch rng.Intn(3) {
		case 0: // fresh mailbox: h:modseq=1, h:logstart=1, no c:1
			logStart = 1
			head = 1 + uint64(rng.Intn(3*retention+2))

			for s := uint64(2); s <= head; s++ {
				seqs = append(seqs, s)
			}
		case 1: // trimmed before: contiguous from h:logstart
			logStart = 2 + uint64(rng.Intn(50))
			head = logStart - 1 + uint64(rng.Intn(3*retention+2))

			for s := logStart; s <= head; s++ {
				seqs = append(seqs, s)
			}
		default: // after an import: leftovers below h:logstart
			logStart = 3 + uint64(rng.Intn(60))
			head = logStart - 1 + uint64(rng.Intn(3*retention+2))

			for s := uint64(2); s < logStart; s++ {
				if rng.Intn(2) == 0 {
					seqs = append(seqs, s)
				}
			}

			for s := logStart; s <= head; s++ {
				seqs = append(seqs, s)
			}
		}

		keys := keysOf(seqs)
		oStart, oDrop := oldTrim(keys, logStart, retention)
		nStart, nDrop := newTrim(t, keys, head, logStart, retention, page)

		if oStart != nStart || !slices.Equal(oDrop, nDrop) {
			t.Fatalf("logstart=%d head=%d retention=%d keys=%v: old (%d, %v), new (%d, %v)",
				logStart, head, retention, keys, oStart, oDrop, nStart, nDrop)
		}
	}
}

// gapLog is a random account log: a start as stored (0: never
// trimmed), a head, the records between them with gaps (more of them
// near the head, as announcements in flight leave them) and the number
// of gaps.
type gapLog struct {
	have, from, head uint64
	keys             []string
	gaps             int
}

func newGapLog(rng *rand.Rand, retention int) gapLog {
	var l gapLog
	if rng.Intn(2) == 1 {
		l.have = 2 + uint64(rng.Intn(40))
	}

	l.from = max(l.have, 1)
	l.head = l.from - 1 + uint64(rng.Intn(3*retention+2))
	rate := rng.Intn(4)

	var seqs []uint64

	for s := l.from; s <= l.head; s++ {
		if rate > 0 && rng.Intn(4) < rate && (s+5 > l.head || rng.Intn(3) == 0) {
			l.gaps++

			continue
		}

		seqs = append(seqs, s)
	}

	l.keys = keysOf(seqs)

	return l
}

// expected is what the new rule removes from l: the start
// head-retention+1 and the keys below it, or nothing.
func (l gapLog) expected(retention int) (uint64, []string) {
	if l.head-l.from+1 <= uint64(retention) {
		return 0, nil
	}

	want := l.head + 1 - uint64(retention)

	var below []string

	for _, k := range l.keys {
		if k < trimKey(want) {
			below = append(below, k)
		}
	}

	if len(below) == 0 || l.have >= want {
		return 0, nil
	}

	return want, below
}

// TestPlanLogTrimAccountGaps checks the documented difference on logs
// with gaps (the account log: announcements not yet recorded near the
// head, withdrawn announcements anywhere): the new rule keeps exactly the
// records from head-retention+1 on, the old one the newest retention
// records, so the new start is later by at most the number of missing
// sequences, and without gaps the two agree.
func TestPlanLogTrimAccountGaps(t *testing.T) {
	rng := rand.New(rand.NewSource(2))

	for i := 0; i < 20000; i++ {
		retention := 1 + rng.Intn(20)
		l := newGapLog(rng, retention)

		oStart, oDrop := oldTrim(l.keys, l.have, retention)
		nStart, nDrop := newTrim(t, l.keys, l.head, l.have, retention, 1+rng.Intn(8))
		want, below := l.expected(retention)

		switch {
		case l.gaps == 0 && (oStart != nStart || !slices.Equal(oDrop, nDrop)):
			t.Fatalf("contiguous log differs: %+v: old (%d, %v), new (%d, %v)", l, oStart, oDrop, nStart, nDrop)
		case nStart != want || !slices.Equal(nDrop, below):
			t.Fatalf("%+v: new (%d, %v), want (%d, %v)", l, nStart, nDrop, want, below)
		case oStart != 0 && (nStart < oStart || nStart-oStart > uint64(l.gaps)):
			// The old rule trims only where the new one does, and the new
			// start is later by no more than the missing sequences.
			t.Fatalf("%+v: new start %d vs old %d", l, nStart, oStart)
		case len(nDrop) < len(oDrop):
			t.Fatalf("new rule keeps more than the old one: new %v, old %v", nDrop, oDrop)
		}
	}
}

// TestPlanLogTrimPaging checks that the plan reads only the beginning of
// the log, sized to the expected drop, and continues over pages when
// records below the new start do not fit one.
func TestPlanLogTrimPaging(t *testing.T) {
	var seqs []uint64
	for s := uint64(101); s <= 300; s++ {
		seqs = append(seqs, s)
	}

	keys := keysOf(seqs) // contiguous 101..300, h:logstart 101

	for _, c := range []struct {
		name            string
		head            uint64
		retention       int
		start           uint64
		drop, reads     int
		first, lastDrop uint64
	}{
		// Drop 101..250 with pages of 64: 64, 64, then 64 of which 22
		// are dropped and the key at the start ends the read.
		{"several pages", 300, 50, 251, 150, 3, 101, 250},
		// The common case: just over the retention, one page.
		{"one over", 300, 199, 102, 1, 1, 101, 101},
		// Within the retention nothing is read at all.
		{"within", 300, 200, 0, 0, 0, 0, 0},
		// A log that ends inside a page stops there.
		{"log ends", 1000, 10, 991, 200, 4, 101, 300},
	} {
		calls := 0

		start, drop, err := PlanLogTrim(c.head, 101, c.retention, 64, trimKey, sliceReader(keys, &calls))
		if err != nil || start != c.start || len(drop) != c.drop || calls != c.reads {
			t.Fatalf("%s: start %d, %d keys, %d reads, %v", c.name, start, len(drop), calls, err)
		}

		if c.drop > 0 && (drop[0] != trimKey(c.first) || drop[len(drop)-1] != trimKey(c.lastDrop)) {
			t.Fatalf("%s: drop %v..%v", c.name, drop[0], drop[len(drop)-1])
		}
	}

	// Read errors are passed on.
	boom := errors.New("boom")

	_, _, err := PlanLogTrim(300, 101, 50, 64, trimKey, func(string, int) ([]string, error) { return nil, boom })
	if !errors.Is(err, boom) {
		t.Fatalf("err %v", err)
	}
}
