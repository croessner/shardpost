//go:build tentacle

package cephstore

import (
	"encoding/json"
	"testing"

	"shardpost/internal/model"
)

// TestAccountKeysSortBySequence: the omap keys of the log have to sort
// in the order of the sequences they carry, because that is the only
// order a range read over them returns.
func TestAccountKeysSortBySequence(t *testing.T) {
	seqs := []uint64{1, 2, 9, 10, 99, 100, 1 << 20, 1<<64 - 1}

	for i := 1; i < len(seqs); i++ {
		if acctEntryKey(seqs[i-1]) >= acctEntryKey(seqs[i]) {
			t.Errorf("entry key of %d does not sort before %d", seqs[i-1], seqs[i])
		}

		if acctPendingKey(seqs[i-1]) >= acctPendingKey(seqs[i]) {
			t.Errorf("announcement key of %d does not sort before %d", seqs[i-1], seqs[i])
		}
	}

	for _, s := range seqs {
		if got := acctSeqOf(acctEntryKey(s)); got != s {
			t.Errorf("entry key of %d reads back as %d", s, got)
		}

		if got := acctSeqOf(acctPendingKey(s)); got != s {
			t.Errorf("announcement key of %d reads back as %d", s, got)
		}
	}

	// The two prefixes must not collide in a prefix-filtered read.
	if acctEntryPrefix == acctPendingPrefix {
		t.Error("entries and announcements share a prefix")
	}
}

// TestSortedSeqsIsNumeric: the floor of the log is the lowest announced
// sequence, so the order must be numeric and not the map's.
func TestSortedSeqsIsNumeric(t *testing.T) {
	vals := map[string][]byte{
		acctPendingKey(100): nil,
		acctPendingKey(2):   nil,
		acctPendingKey(17):  nil,
	}

	got := sortedSeqs(vals)
	if len(got) != 3 || got[0] != 2 || got[1] != 17 || got[2] != 100 {
		t.Errorf("sortedSeqs returned %v, want 2, 17, 100", got)
	}
}

// TestEmailChangeTakesTheHighestModSeq: an entry carries the modseq of
// the change, and a batch whose records carry their own takes the
// highest, because that is the one a reader has to have passed.
func TestEmailChangeTakesTheHighestModSeq(t *testing.T) {
	metas := []Meta{
		{UID: 1, GUID: "aa", ModSeq: 7},
		{UID: 2, GUID: "bb", ModSeq: 9},
	}

	c := emailChange(model.OpUpdated, 0, metas)
	if c.ModSeq != 9 {
		t.Errorf("the entry carries modseq %d, want 9", c.ModSeq)
	}

	if len(c.Msgs) != 2 || c.Msgs[0].GUID != "aa" || c.Msgs[1].UID != 2 {
		t.Errorf("the entry names %+v, want both messages by UID and GUID", c.Msgs)
	}

	if c := emailChange(model.OpDestroyed, 42, nil); c.ModSeq != 42 || len(c.Msgs) != 0 {
		t.Errorf("an empty change came out as %+v", c)
	}
}

// TestAccountEntryRoundTrip: an entry survives the omap encoding, and a
// Partial entry keeps saying that it is one — a reader that lost that
// flag would apply a delta it was never given.
func TestAccountEntryRoundTrip(t *testing.T) {
	in := model.AccountChange{
		Kind: model.KindEmail, Op: model.OpDestroyed, Mailbox: "box1", ModSeq: 12,
		Msgs: []model.EmailRef{{UID: 3, GUID: "cc"}}, Partial: true,
	}

	b, err := json.Marshal(&in)
	if err != nil {
		t.Fatal(err)
	}

	var out model.AccountChange
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}

	in.Seq = 0 // the sequence lives in the key, not in the value
	if out.Kind != in.Kind || out.Op != in.Op || out.Mailbox != in.Mailbox ||
		out.ModSeq != in.ModSeq || !out.Partial || len(out.Msgs) != 1 || out.Msgs[0].GUID != "cc" {
		t.Errorf("the entry came back as %+v, want %+v", out, in)
	}
}

// TestCeiling: a reader may return an entry only when it was already
// allocated at the moment it snapshotted the counter, and never at or
// above an announcement that has no record yet.
func TestCeiling(t *testing.T) {
	cases := []struct{ floor, snapshot, want uint64 }{
		{0, 11, 11},  // nothing in flight: everything up to the snapshot
		{5, 11, 5},   // an announcement below the snapshot bounds the read
		{12, 11, 11}, // announced after the snapshot: the snapshot bounds it
		{1, 11, 1},   // the very first sequence is in flight: nothing to return
		{0, 1, 1},    // an empty account
	}

	for _, c := range cases {
		if got := ceiling(c.floor, c.snapshot); got != c.want {
			t.Errorf("ceiling(%d, %d) = %d, want %d", c.floor, c.snapshot, got, c.want)
		}
	}
}
