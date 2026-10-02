package admin

import (
	"slices"
	"testing"
	"time"

	"shardpost/internal/model"
)

// TestAPILayoutStateOld (review round 3): the layout record of the API
// carries the runs of the old entries and their heartbeats (empty for a
// job), in the order of the entries.
func TestAPILayoutStateOld(t *testing.T) {
	beat := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	st := model.FTSLayoutState{
		Current: model.FTSLayoutShape{Layout: 3, Gen: "g2", S: 2, D: 1, H: 1},
		Old:     []model.FTSLayoutShape{{Layout: 2}, {Layout: 3, Gen: "g1", S: 1, D: 1, H: 1}},
		OldToks: []string{"job-x/1", "importsession0123456/n1"},
		OldAt:   []time.Time{{}, beat},
	}

	api := apiLayoutState(st)

	if api.Old == nil || len(*api.Old) != 2 || api.OldToks == nil || api.OldAt == nil {
		t.Fatalf("old entries %+v", api)
	}

	if !slices.Equal(*api.OldToks, st.OldToks) || !slices.Equal(*api.OldAt, []string{"", "2026-10-02T12:00:00Z"}) {
		t.Errorf("old_toks %v old_at %v", *api.OldToks, *api.OldAt)
	}

	if none := apiLayoutState(model.FTSLayoutState{}); none.Old != nil || none.OldToks != nil || none.OldAt != nil {
		t.Errorf("old fields without old entries: %+v", none)
	}
}
