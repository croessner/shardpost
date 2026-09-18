package imapproto

import (
	"testing"

	"shardpost/internal/model"
)

func TestFormatGaps(t *testing.T) {
	msgs := func(uids ...uint32) []model.Meta {
		out := make([]model.Meta, 0, len(uids))
		for _, u := range uids {
			out = append(out, model.Meta{UID: u})
		}

		return out
	}

	cases := []struct {
		msgs    []model.Meta
		uidNext uint32
		want    string
	}{
		{msgs(), 1, ""},
		{msgs(), 5, "1:4"},
		{msgs(1, 2, 3), 4, ""},
		{msgs(2, 5, 6, 9), 12, "1,3:4,7:8,10:11"},
		{msgs(1), 1, ""},
		{msgs(3), 3, "1:2"},
	}
	for _, c := range cases {
		if got := formatGaps(c.msgs, c.uidNext); got != c.want {
			t.Errorf("gaps(%v, %d) = %q, want %q", c.msgs, c.uidNext, got, c.want)
		}
	}
}
