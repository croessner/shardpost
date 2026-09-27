//go:build tentacle

package cephstore

import (
	"encoding/json"
	"fmt"
	"sort"
	"testing"
	"time"
)

// fakeRegistry serves pointer keys in key order, as GetOmapValues does.
type fakeRegistry map[string][]byte

func (r fakeRegistry) read(start string, n int) (map[string][]byte, error) {
	keys := make([]string, 0, len(r))
	for k := range r {
		if k > start {
			keys = append(keys, k)
		}
	}

	sort.Strings(keys)

	out := map[string][]byte{}
	for _, k := range keys[:min(n, len(keys))] {
		out[k] = r[k]
	}

	return out, nil
}

// put writes one record.
func (r fakeRegistry) put(owner, mailbox string, rec sharedRec) {
	b, _ := json.Marshal(&rec)
	r[sharedKey(owner, mailbox)] = b
}

// TestTombstonesDoNotFillTheWindow: an account that grants and revokes
// thousands of mailboxes leaves tombstones in the victim's registry. They
// do not count against a listing's limit, a listing reads past them to
// the next owner's pointer, and once past their time they are collected,
// a bounded number per listing, until the registry is clean.
func TestTombstonesDoNotFillTheWindow(t *testing.T) {
	now := time.Now()
	reg := fakeRegistry{}

	for i := range 8000 {
		reg.put("aaaa", fmt.Sprintf("m%05d", i), sharedRec{Gen: 2, Deleted: true, At: now.Unix()})
	}

	reg.put("bob", "Team", sharedRec{Gen: 7})

	// Fresh tombstones are read past, not collected.
	sc, err := scanSharedPages(reg.read, 8000, now)
	if err != nil {
		t.Fatal(err)
	}

	if len(sc.refs) != 1 || sc.refs[0].Owner != "bob" || sc.more || len(sc.collect) != 0 {
		t.Fatalf("fresh tombstones: refs %v, more %v, collect %d", sc.refs, sc.more, len(sc.collect))
	}

	// A small window cannot read past all of them: it says there is more,
	// which moves the fingerprint, until they are collected.
	if sc, _ = scanSharedPages(reg.read, 10, now); !sc.more || len(sc.refs) != 0 {
		t.Fatalf("a small window: refs %v, more %v", sc.refs, sc.more)
	}

	// Past their time they are collected, sharedCollectMax per listing.
	if rounds := collectUntilClean(t, reg, now.Add(2*sharedTombstoneTTL)); rounds > 8000/sharedCollectMax+1 {
		t.Errorf("the window cleared after %d listings", rounds)
	}
}

// collectUntilClean lists with a small window and removes what each
// listing collects until the live pointer is found without more; it
// returns how many listings that took.
func collectUntilClean(t *testing.T, reg fakeRegistry, now time.Time) int {
	t.Helper()

	for round := 1; round <= 100; round++ {
		sc, err := scanSharedPages(reg.read, 10, now)
		if err != nil || len(sc.collect) > sharedCollectMax {
			t.Fatalf("listing %d: err %v, collects %d", round, err, len(sc.collect))
		}

		for _, k := range sc.collect {
			delete(reg, k)
		}

		if len(sc.refs) == 1 && !sc.more {
			return round
		}
	}

	t.Fatalf("the window never cleared: %d keys left", len(reg))

	return 0
}

// TestTombstoneAge: a tombstone is collectable after the bound, one
// without a time at once, a live pointer never.
func TestTombstoneAge(t *testing.T) {
	now := time.Now()

	for name, tc := range map[string]struct {
		rec  sharedRec
		want bool
	}{
		"fresh":  {sharedRec{Deleted: true, At: now.Unix()}, false},
		"old":    {sharedRec{Deleted: true, At: now.Add(-2 * sharedTombstoneTTL).Unix()}, true},
		"legacy": {sharedRec{Deleted: true}, true},
		"live":   {sharedRec{Gen: 3}, false},
	} {
		if got := tc.rec.collectable(now); got != tc.want {
			t.Errorf("%s: %v, want %v", name, got, tc.want)
		}
	}
}
