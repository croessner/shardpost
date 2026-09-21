//go:build tentacle

package cephstore

import (
	"testing"

	"shardpost/internal/model"
)

// TestMarkRoundTrip: a mark survives being written and read back, and a
// value that is not one keeps the message unread rather than failing.
func TestMarkRoundTrip(t *testing.T) {
	for _, m := range []model.PrivateMark{{Seen: true, ModSeq: 42}, {Seen: false, ModSeq: 1}, {}} {
		if got := decodeMark(encodeMark(m)); got != m {
			t.Errorf("mark %+v came back as %+v", m, got)
		}
	}

	if got := decodeMark([]byte("nonsense")); got.Seen || got.ModSeq != 0 {
		t.Errorf("a damaged mark reads as %+v, want unread", got)
	}
}

// TestStaleMarks: marks of messages that are gone and every mark of an
// earlier incarnation of the folder are removed, at most limit of them.
func TestStaleMarks(t *testing.T) {
	vals := map[string][]byte{
		pseenKey(7, 1): encodeMark(model.PrivateMark{Seen: true}),  // still there
		pseenKey(7, 2): encodeMark(model.PrivateMark{Seen: true}),  // expunged
		pseenKey(7, 3): encodeMark(model.PrivateMark{Seen: false}), // expunged
		pseenKey(6, 1): encodeMark(model.PrivateMark{Seen: true}),  // older folder
	}

	keep := map[uint32]bool{1: true}

	rm := staleMarks(vals, 7, keep, 10)
	if len(rm) != 3 {
		t.Fatalf("removed %v, want the two expunged UIDs and the older incarnation", rm)
	}

	for _, k := range rm {
		if k == pseenKey(7, 1) {
			t.Errorf("the mark of a message that exists was removed")
		}
	}

	if rm := staleMarks(vals, 7, keep, 2); len(rm) != 2 {
		t.Errorf("removed %d marks, want the limit of 2", len(rm))
	}

	if rm := staleMarks(vals, 7, map[uint32]bool{1: true, 2: true, 3: true}, 10); len(rm) != 1 {
		t.Errorf("removed %v, want only the older incarnation", rm)
	}
}

// TestClampRange: a requested byte range is cut down to what the blob
// holds; a length below zero means "to the end", and a range beyond the
// end is empty rather than an error.
func TestClampRange(t *testing.T) {
	cases := []struct {
		size, offset, length int64
		lo, hi               int64
	}{
		{10, 0, -1, 0, 10},
		{10, 3, -1, 3, 10},
		{10, 3, 4, 3, 7},
		{10, 3, 100, 3, 10},
		{10, 20, 5, 10, 10},
		{10, -5, 4, 0, 4},
		{0, 0, -1, 0, 0},
	}

	for _, c := range cases {
		lo, hi := clampRange(c.size, c.offset, c.length)
		if lo != c.lo || hi != c.hi {
			t.Errorf("clampRange(%d, %d, %d) = %d..%d, want %d..%d",
				c.size, c.offset, c.length, lo, hi, c.lo, c.hi)
		}
	}
}
