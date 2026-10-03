//go:build tentacle

package cephstore

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"shardpost/internal/fts"
	"shardpost/internal/model"
)

// TestFTSStats (temp/13 §7.1, §10): the default reads only the counters
// of the posting objects; the exact count agrees with them and counts
// the dictionary and header objects; the classes agree with the
// counters and project fts.DictKeys per bound; no term in the answer.
func TestFTSStats(t *testing.T) {
	s := layout3Store(t, nil)
	u := testUser(t, s)
	mustMailbox(t, u, "INBOX")
	deliverDocs(t, u, layoutDocs(30))
	convert(t, u, model.FTSLayoutRequest{Layout: 3, Shape: &model.FTSForcedShape{S: 4, D: 8, H: 2}})

	st, err := u.FTSStats(model.FTSStatsRequest{})
	if err != nil {
		t.Fatal(err)
	}

	if st.Exact || st.Docs != 30 || st.Shape.S != 4 || len(st.Objects) != 4 || st.Terms == 0 || st.DictKeys == 0 {
		t.Fatalf("counters %+v", st)
	}

	for _, o := range st.Objects {
		if o.Kind != model.FTSObjectPostings {
			t.Errorf("the default read %s", o.Object)
		}
	}

	ex, err := u.FTSStats(model.FTSStatsRequest{Exact: true, Parallel: 3, Classes: true, Context: context.Background()})
	if err != nil {
		t.Fatal(err)
	}

	if !ex.Exact || ex.Terms != st.Terms || ex.DictKeys != st.DictKeys || ex.HeaderKeys != 30 || len(ex.Objects) != 4+8+2 {
		t.Fatalf("exact %+v, counters %+v", ex, st)
	}

	c := ex.Classes
	if c == nil || c.Terms != st.Terms || c.DictKeys != st.DictKeys || len(c.ByIdent) != 28 {
		t.Fatalf("classes %+v", c)
	}

	// The projection is fts.DictKeys with the bound over the terms.
	loc := curLoc(layoutOf(t, u), u.ftsShards)

	for _, b := range c.ByIdent {
		var want int64

		for _, oid := range loc.postingOIDs() {
			if err := u.eachKey(context.Background(), oid, func(term string) { want += int64(fts.DictKeys(term, b.MinRunes)) }); err != nil {
				t.Fatal(err)
			}
		}

		if b.DictKeys != want {
			t.Errorf("bound %d: %d, want %d", b.MinRunes, b.DictKeys, want)
		}
	}

	js, _ := json.Marshal(ex)
	for _, w := range []string{"photosynthesis", "rechnung", "unique1word"} {
		if strings.Contains(string(js), w) {
			t.Errorf("the statistics name the term %q", w)
		}
	}

	// A cancelled count ends.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := u.FTSStats(model.FTSStatsRequest{Exact: true, Context: ctx}); err == nil {
		t.Error("a cancelled exact count went on")
	}
}

// TestFTSStatsLayout2: a layout 2 index has no counters; the exact count
// counts its objects and the documents of its id map.
func TestFTSStatsLayout2(t *testing.T) {
	u := testUser(t, testStore(t))
	mustMailbox(t, u, "INBOX")
	deliverDocs(t, u, layoutDocs(5))

	st, err := u.FTSStats(model.FTSStatsRequest{})
	if err != nil || st.Shape.Layout != 2 || len(st.Objects) != 0 || st.Docs != 0 {
		t.Fatalf("layout 2 counters %+v %v", st, err)
	}

	ex, err := u.FTSStats(model.FTSStatsRequest{Exact: true})
	if err != nil || ex.Docs != 5 || ex.Terms == 0 || ex.DictKeys == 0 || ex.HeaderKeys != 5 {
		t.Fatalf("layout 2 exact %+v %v", ex, err)
	}
}
