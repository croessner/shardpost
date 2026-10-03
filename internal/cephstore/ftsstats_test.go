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

	st := statsOf(t, u, model.FTSStatsRequest{})
	if got := summaryOf(st); got != (statsSummary{docs: 30, s: 4, objects: 4, postings: 4, terms: st.Terms, dict: st.DictKeys}) || st.Terms == 0 {
		t.Fatalf("counters %+v", st)
	}

	ex := statsOf(t, u, model.FTSStatsRequest{Exact: true, Parallel: 3, Classes: true, Context: context.Background()})
	if got := summaryOf(ex); got != (statsSummary{exact: true, docs: 30, s: 4, objects: 14, postings: 4, terms: st.Terms, dict: st.DictKeys, headers: 30}) {
		t.Fatalf("exact %+v, counters %+v", got, summaryOf(st))
	}

	if c := ex.Classes; c == nil || (statsSummary{terms: c.Terms, dict: c.DictKeys, objects: len(c.ByIdent)}) != (statsSummary{terms: st.Terms, dict: st.DictKeys, objects: 28}) {
		t.Fatalf("classes %+v", c)
	}

	checkProjection(t, u, ex.Classes)
	noTerms(t, ex)

	// A cancelled count ends.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := u.FTSStats(model.FTSStatsRequest{Exact: true, Context: ctx}); err == nil {
		t.Error("a cancelled exact count went on")
	}
}

// statsSummary are the numbers of statistics a test compares.
type statsSummary struct {
	exact                       bool
	docs, terms, dict, headers  int64
	s, objects, postings        int
}

func summaryOf(st model.FTSStats) statsSummary {
	out := statsSummary{exact: st.Exact, docs: st.Docs, terms: st.Terms, dict: st.DictKeys, headers: st.HeaderKeys,
		s: st.Shape.S, objects: len(st.Objects)}

	for _, o := range st.Objects {
		if o.Kind == model.FTSObjectPostings {
			out.postings++
		}
	}

	return out
}

func statsOf(t *testing.T, u *User, req model.FTSStatsRequest) model.FTSStats {
	t.Helper()

	st, err := u.FTSStats(req)
	if err != nil {
		t.Fatal(err)
	}

	return st
}

// checkProjection: the projection per bound is fts.DictKeys with the
// bound over the terms of the posting objects.
func checkProjection(t *testing.T, u *User, c *model.FTSTermClasses) {
	t.Helper()

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
}

// noTerms: the statistics name none of the words of layoutDocs.
func noTerms(t *testing.T, st model.FTSStats) {
	t.Helper()

	js, _ := json.Marshal(st)
	for _, w := range []string{"photosynthesis", "rechnung", "unique1word"} {
		if strings.Contains(string(js), w) {
			t.Errorf("the statistics name the term %q", w)
		}
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
