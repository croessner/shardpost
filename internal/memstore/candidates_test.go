package memstore

import (
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	"shardpost/internal/fts"
)

// candidateCase is a substring query on fields and the GUIDs (by
// message number) its candidates must be; nil: not narrowed.
type candidateCase struct {
	fields []string
	query  string
	want   []int
}

// candidateMessages and candidateCases are the cases cephstore's
// TestSearchCandidates runs over its key layout as well.
var candidateMessages = []string{
	"Subject: hello beautiful world\r\nX-Extra: hello beautiful header\r\n\r\nbody text\r\n",
	"Subject: other\r\n\r\nsome " + strings.Repeat("xyz", 20) + " text\r\n",
	"Subject: third\r\nX-Id: " + strings.Repeat("q", 50) + "\r\n\r\n" + strings.Repeat("abcdefghij", 30) + "\r\n",
}

var candidateCases = []candidateCase{
	{[]string{fts.FieldSubject}, "eautiful worl", []int{0}},
	{[]string{fts.FieldSubject}, "llo bea", []int{0}},
	{[]string{fts.FieldHeader}, "eautiful head", []int{0}},
	{[]string{fts.FieldHeader}, "eautiful", nil},
	{[]string{fts.FieldHeader}, "x-extra", []int{0}},
	{[]string{fts.FieldHeader}, ": " + strings.Repeat("q", 30), []int{2}}, // the start of a long word: window 0
	{[]string{fts.FieldHeader}, ": " + strings.Repeat("q", 45), nil},      // longer than any term
	{[]string{fts.FieldBody}, "zxyzx", []int{1, 2}},                       // a window of 1, HugeWord of 2
	{[]string{fts.FieldBody}, "od", nil},
	{[]string{fts.FieldBody}, "some xyz", []int{1}},
	{[]string{fts.FieldBody}, "ext", []int{0, 1, 2}},
	{[]string{fts.FieldBody}, "hijabcdefghijabcdefghija", []int{1, 2}}, // 24 runes: LongWord
	{[]string{fts.FieldBody}, "jabcd", []int{2}},                       // a window, and HugeWord
	{[]string{fts.FieldHeader, fts.FieldBody}, "text", nil},
	{[]string{fts.FieldHeader, fts.FieldBody}, " text ", []int{0, 1}},
}

// TestSearchCandidates: the candidates of a substring query are the
// messages that can contain it, and nil where the index cannot narrow
// them.
func TestSearchCandidates(t *testing.T) {
	u, err := New().User("alice")
	if err != nil {
		t.Fatal(err)
	}

	if err := u.CreateMailbox("INBOX", nil); err != nil {
		t.Fatal(err)
	}

	var guids []string

	for _, raw := range candidateMessages {
		m, err := u.Deliver("INBOX", []byte(raw), nil, time.Now())
		if err != nil {
			t.Fatal(err)
		}

		guids = append(guids, m.GUID)
	}

	for _, c := range candidateCases {
		hits, err := u.SearchCandidates(c.fields, c.query)
		if err != nil {
			t.Fatal(err)
		}

		if c.want == nil {
			if hits != nil {
				t.Errorf("%v %q: %v, want nil", c.fields, c.query, hits)
			}

			continue
		}

		want := make([]string, 0, len(c.want))
		for _, i := range c.want {
			want = append(want, guids[i])
		}

		if got := slices.Sorted(maps.Keys(hits)); !slices.Equal(got, slices.Sorted(slices.Values(want))) {
			t.Errorf("%v %q: %v, want %v", c.fields, c.query, got, want)
		}
	}
}
