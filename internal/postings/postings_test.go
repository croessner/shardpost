package postings

import (
	"bytes"
	"slices"
	"testing"
)

func TestRoundTrip(t *testing.T) {
	ids := []uint64{5, 1, 1000000, 5, 7}

	p := Encode(ids)
	if IsLegacy(p) {
		t.Fatalf("encoded list must not look legacy (len %d)", len(p))
	}

	got, ok := Decode(p)
	if !ok || !slices.Equal(got, []uint64{1, 5, 7, 1000000}) {
		t.Fatalf("decode: %v %v", got, ok)
	}

	p = Add(p, 3)
	p = Add(p, 3)

	got, _ = Decode(p)
	if len(got) != 5 || got[1] != 3 {
		t.Fatalf("add: %v", got)
	}

	p = Remove(p, 1000000)

	got, _ = Decode(p)
	if len(got) != 4 || got[3] != 7 {
		t.Fatalf("remove: %v", got)
	}

	for _, id := range []uint64{1, 3, 5, 7} {
		p = Remove(p, id)
	}

	if p != nil {
		t.Fatalf("empty list should be nil, got %x", p)
	}
}

func TestNeverMultipleOf16(t *testing.T) {
	for n := 0; n < 200; n++ {
		ids := make([]uint64, n)
		for i := range ids {
			ids[i] = uint64(i * 3)
		}

		p := Encode(ids)
		if n > 0 && len(p)%16 == 0 {
			t.Fatalf("n=%d len=%d", n, len(p))
		}
	}
	// A large id set whose natural length hits a multiple of 16 gets a pad.
	ids := []uint64{1 << 40, 1<<40 + 1, 1<<40 + 2, 1<<40 + 3, 1<<40 + 4, 1<<40 + 5, 1<<40 + 6, 1<<40 + 7}
	p := Encode(ids)

	got, ok := Decode(p)
	if !ok || len(got) != 8 || got[7] != 1<<40+7 {
		t.Fatalf("padded decode: %v %v", got, ok)
	}
}

func TestLegacy(t *testing.T) {
	g1 := bytes.Repeat([]byte{1}, 16)
	g2 := bytes.Repeat([]byte{2}, 16)

	p := append(append([]byte{}, g1...), g2...)
	if !IsLegacy(p) {
		t.Fatal("legacy not recognised")
	}

	if _, ok := Decode(p); ok {
		t.Fatal("legacy decoded as v2")
	}

	if gs := LegacyGUIDs(p); len(gs) != 2 || !bytes.Equal(gs[1], g2) {
		t.Fatalf("legacy guids: %d", len(gs))
	}
}

func TestAddAll(t *testing.T) {
	p := Encode([]uint64{3, 9})

	got, ok := Decode(AddAll(p, []uint64{9, 1, 12, 1}))
	if !ok || len(got) != 4 || got[0] != 1 || got[1] != 3 || got[2] != 9 || got[3] != 12 {
		t.Fatalf("got %v", got)
	}

	if same := AddAll(p, []uint64{3, 9}); string(same) != string(p) {
		t.Fatal("a list that holds every id changed")
	}

	if got, _ := Decode(AddAll(nil, []uint64{5})); len(got) != 1 || got[0] != 5 {
		t.Fatalf("single id into an empty list: %v", got)
	}

	if got, _ := Decode(AddAll(nil, []uint64{7, 5})); len(got) != 2 || got[0] != 5 {
		t.Fatalf("two ids into an empty list: %v", got)
	}
}
