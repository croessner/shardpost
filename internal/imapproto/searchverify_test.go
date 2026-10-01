package imapproto

import (
	"fmt"
	"maps"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"shardpost/internal/membudget"
	"shardpost/internal/memstore"
	"shardpost/internal/model"
)

// countingStore hands out users that count whole-message reads and can
// make one message gone.
type countingStore struct {
	inner *memstore.Store
	reads *atomic.Int64
	gone  *sync.Map // GUID -> true: Read answers ErrNoBlob
}

func (s countingStore) User(name string) (model.User, error) {
	u, err := s.inner.User(name)
	if err != nil {
		return nil, err
	}

	return &countingUser{User: u, reads: s.reads, gone: s.gone}, nil
}

type countingUser struct {
	model.User
	reads *atomic.Int64
	gone  *sync.Map
}

// readDelay slows every whole-message read of a countingUser down.
var readDelay atomic.Int64

func (u *countingUser) Read(m *model.Meta) ([]byte, error) {
	u.reads.Add(1)
	time.Sleep(time.Duration(readDelay.Load()))

	if _, ok := u.gone.Load(m.GUID); ok {
		return nil, fmt.Errorf("read %s: %w", m.GUID, model.ErrNoBlob)
	}

	return u.User.Read(m)
}

func newCountingServer(t *testing.T, parallel int) (*client, *atomic.Int64, *sync.Map) {
	t.Helper()

	reads, gone := &atomic.Int64{}, &sync.Map{}

	c, _ := newTestServer(t, func(o *Options, store *memstore.Store) {
		o.Store = countingStore{inner: store, reads: reads, gone: gone}
		o.VerifyParallel = parallel
	})
	c.ok("LOGIN test test")

	return c, reads, gone
}

// TestSearchHeadersWithoutReading: header keys are checked against the
// header text the store keeps, so a search over the headers of every
// message reads none; TEXT reads only the messages whose body the index
// cannot rule out.
func TestSearchHeadersWithoutReading(t *testing.T) {
	c, reads, _ := newCountingServer(t, 4)

	var msgs []string
	for i := 0; i < 20; i++ {
		msgs = append(msgs, fmt.Sprintf("Subject: s%d\r\nX-Extra: hello beautiful header %d\r\n\r\nbody %d\r\n", i, i, i))
	}

	msgs = append(msgs, "Subject: x\r\n\r\nthe word wertyuiop is in this body\r\n")

	runSearches(t, c, "hw", msgs, nil)

	if lines := c.ok(`SEARCH HEADER x-extra "eautiful head"`); !has(lines, "* SEARCH 1 2 3 4 5 6 7 8 9 10 11 12 13 14 15 16 17 18 19 20") {
		t.Fatalf("header search: %v", lines)
	}

	if n := reads.Load(); n != 0 {
		t.Errorf("a header search read %d messages", n)
	}

	mustHave(t, c.ok("SEARCH TEXT ertyuio"), "* SEARCH 21")

	if n := reads.Load(); n != 1 {
		t.Errorf("TEXT read %d messages, want the one whose body may match", n)
	}
}

// TestSearchGoneMessage: a candidate that went away while the search
// ran is no match and does not fail the command.
func TestSearchGoneMessage(t *testing.T) {
	c, _, gone := newCountingServer(t, 2)

	runSearches(t, c, "gm", []string{"Subject: a\r\n\r\nneedle one\r\n", "Subject: b\r\n\r\nneedle two\r\n"}, nil)

	lines := c.ok("FETCH 1 EMAILID")
	for _, l := range lines {
		var id string
		if _, err := fmt.Sscanf(l, "* 1 FETCH (EMAILID (%32s", &id); err == nil {
			gone.Store(id, true)
		}
	}

	mustHave(t, c.ok("SEARCH BODY eedl"), "* SEARCH 2")
}

// TestSearchBusyBudget: a read the memory budget refuses at first is
// retried and the search completes.
func TestSearchBusyBudget(t *testing.T) {
	c, _, _ := newCountingServer(t, 1)
	runSearches(t, c, "bb", []string{"Subject: a\r\n\r\nneedle\r\n"}, nil)

	oldWait, oldLimit := membudget.WaitTimeout, membudget.Limit()
	membudget.WaitTimeout = 10 * time.Millisecond

	membudget.SetLimit(1 << 20)

	t.Cleanup(func() {
		membudget.WaitTimeout = oldWait

		membudget.SetLimit(oldLimit)
	})

	hold, err := membudget.Acquire(1 << 20)
	if err != nil {
		t.Fatal(err)
	}

	go func() {
		time.Sleep(120 * time.Millisecond)
		hold()
	}()

	mustHave(t, c.ok("SEARCH BODY eedl"), "* SEARCH 1")
}

// TestSearchParallelBatches: more messages than one batch, checked with
// one worker and with many, give the same answer.
func TestSearchParallelBatches(t *testing.T) {
	var msgs []string

	for i := 0; i < verifyBatch+40; i++ {
		body := "plain"
		if i%7 == 0 {
			body = "the needle"
		}

		msgs = append(msgs, fmt.Sprintf("Subject: m%d\r\n\r\n%s\r\n", i, body))
	}

	var answers [][]string

	for _, p := range []int{1, 8} {
		c, _, _ := newCountingServer(t, p)
		runSearches(t, c, "pb", msgs, nil)

		answers = append(answers, c.ok("SEARCH OR BODY eedl SUBJECT m1"))
	}

	if !slices.Equal(answers[0], answers[1]) || len(answers[0][0]) < 20 {
		t.Errorf("answers differ: %v", answers)
	}
}

// TestSearchCandidatesTree: AND intersects, OR unites and is not
// narrowed when one side is not, NOT never narrows.
func TestSearchCandidatesTree(t *testing.T) {
	set := func(gs ...string) map[string]bool {
		m := map[string]bool{}
		for _, g := range gs {
			m[g] = true
		}

		return m
	}

	body := func(s map[string]bool) textCrit {
		return textCrit{fields: []string{"b"}, value: "x", bodyCand: s}
	}

	cr := &criteria{
		text: []textCrit{body(set("a", "b", "c"))},
		or:   [][2]*criteria{{{text: []textCrit{body(set("a"))}}, {text: []textCrit{body(set("b", "d"))}}}},
		not:  []*criteria{{text: []textCrit{body(set("a"))}}},
	}

	if got := slices.Sorted(maps.Keys(searchCandidates(cr))); !slices.Equal(got, []string{"a", "b"}) {
		t.Errorf("AND/OR: %v", got)
	}

	cr.or[0][1].text[0].bodyCand = nil
	if got := slices.Sorted(maps.Keys(searchCandidates(cr))); !slices.Equal(got, []string{"a", "b", "c"}) {
		t.Errorf("OR with a side not narrowed: %v", got)
	}

	if searchCandidates(&criteria{not: cr.not}) != nil {
		t.Error("NOT narrowed")
	}

	text := textCrit{fields: []string{"h", "b"}, value: "x", hdrCand: set("a"), bodyCand: set("b")}
	if got := slices.Sorted(maps.Keys(critCandidates(text))); !slices.Equal(got, []string{"a", "b"}) {
		t.Errorf("TEXT: %v", got)
	}
}
