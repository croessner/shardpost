package model

import (
	"errors"
	"testing"
	"time"
)

func TestDueKeyRoundTrip(t *testing.T) {
	due := time.Date(2026, 10, 1, 12, 0, 0, 123456789, time.FixedZone("x", 3600))

	for _, id := range []string{"20261001T120000Z-abcdef", "job/20261001T120000Z-abcdef", "idem/0011", "a"} {
		key := FormatDueKey(id, DueKey{Due: due, Gen: 42})

		e, err := ParseDueKey(key)
		if err != nil {
			t.Fatal(err)
		}

		if e.ID != id || e.Gen != 42 || !e.Due.Equal(due) || e.Key() != key {
			t.Fatalf("%q: parsed %+v", key, e)
		}
	}
}

func TestDueKeysSortByDue(t *testing.T) {
	early := FormatDueKey("zzz", DueKey{Due: time.Unix(9, 0)})
	late := FormatDueKey("aaa", DueKey{Due: time.Unix(10, 0)})

	if early >= late {
		t.Fatalf("%q sorts after %q", early, late)
	}

	if k := FormatDueKey("x", DueKey{Due: time.Unix(-5, 0)}); k[:20] != "00000000000000000000" {
		t.Fatalf("a due before 1970 is %q", k)
	}
}

func TestParseDueKeyRejects(t *testing.T) {
	for _, bad := range []string{"", "123/x/1", "0000000000000000000a/x/1", "00000000000000000001/x/", "00000000000000000001//1",
		"00000000000000000001/x/y"} {
		if _, err := ParseDueKey(bad); !errors.Is(err, ErrDueKey) {
			t.Errorf("%q: %v", bad, err)
		}
	}
}

func TestSameDueKey(t *testing.T) {
	a := time.Date(2026, 1, 1, 0, 0, 0, 5, time.UTC)

	if !SameDueKey(DueKey{Due: a, Gen: 1}, DueKey{Due: a.In(time.Local), Gen: 1}) {
		t.Fatal("same instant in another zone differs")
	}

	if SameDueKey(DueKey{Due: a, Gen: 1}, DueKey{Due: a, Gen: 2}) || SameDueKey(DueKey{Due: a}, DueKey{Due: a.Add(1)}) {
		t.Fatal("different keys compare equal")
	}
}

func TestParseKVIndexMode(t *testing.T) {
	for in, want := range map[string]KVIndexMode{"": KVIndexWrite, "off": KVIndexOff, "write": KVIndexWrite, "read": KVIndexRead} {
		if got, err := ParseKVIndexMode(in); err != nil || got != want {
			t.Errorf("%q: %q %v", in, got, err)
		}
	}

	if _, err := ParseKVIndexMode("on"); err == nil {
		t.Fatal("unknown mode accepted")
	}
}
