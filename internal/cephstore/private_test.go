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
