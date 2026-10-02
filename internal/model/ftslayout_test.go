package model

import "testing"

// TestEffectiveFTSLayout: layout 3 needs both fts.layout 3 and the
// cluster mark; everything else creates layout 2 indexes.
func TestEffectiveFTSLayout(t *testing.T) {
	cases := []struct {
		configured int
		marked     bool
		want       int
	}{
		{FTSLayout3, false, FTSLayout2},
		{FTSLayout3, true, FTSLayout3},
		{FTSLayout2, true, FTSLayout2},
		{FTSLayout2, false, FTSLayout2},
		{0, true, FTSLayout2},
	}

	for _, c := range cases {
		if got := EffectiveFTSLayout(c.configured, c.marked); got != c.want {
			t.Errorf("EffectiveFTSLayout(%d, %v) = %d, want %d", c.configured, c.marked, got, c.want)
		}
	}
}
