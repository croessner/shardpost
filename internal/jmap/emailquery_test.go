package jmap

import (
	"strconv"
	"strings"
	"testing"
	"time"

	"shardpost/internal/model"
)

// deliverAt stores a message with the given arrival, an hour after a fixed
// start per step.
func deliverAt(t *testing.T, u model.User, mailbox, raw string, hour int, flags ...string) *model.Meta {
	t.Helper()

	m, err := u.Deliver(mailbox, []byte(raw), flags, time.Date(2026, 9, 20, hour, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}

	return m
}

// boxIDs maps mailbox names to their ids.
func boxIDs(t *testing.T, u model.User) map[string]string {
	t.Helper()

	boxes, err := u.ListMailboxes()
	if err != nil {
		t.Fatal(err)
	}

	out := map[string]string{}
	for _, mb := range boxes {
		out[mb.Name] = mb.ID
	}

	return out
}

// queryIDs runs Email/query and returns the ids in order.
func queryIDs(t *testing.T, s *Server, args string) []string {
	t.Helper()

	res := mailCall(t, s, "Email/query", args)

	list, _ := res["ids"].([]any)
	out := make([]string, 0, len(list))

	for _, id := range list {
		out = append(out, id.(string))
	}

	return out
}

// sameIDs fails the test when got is not want, in order.
func sameIDs(t *testing.T, what string, got []string, want ...string) {
	t.Helper()

	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("%s gave %v, want %v", what, got, want)
	}
}

// queryFixture is a small account: three messages in the inbox, one in the
// archive, and one in both.
type queryFixture struct {
	s                           *Server
	u                           model.User
	boxes                       map[string]string
	alice, bob, carol, old, dup *model.Meta
}

func newQueryFixture(t *testing.T) *queryFixture {
	t.Helper()

	s, u := emailServer(t)
	f := &queryFixture{s: s, u: u}

	f.old = deliverAt(t, u, "Archive", "From: Dora <dora@example.org>\r\nSubject: old report\r\n\r\nquarterly numbers\r\n", 1)
	f.alice = deliverAt(t, u, inboxName, "From: Alice <alice@example.org>\r\nTo: bob@example.org\r\n"+
		"Subject: Lunch\r\nX-Priority: 1\r\n\r\nshall we meet\r\n", 2, `\Seen`)
	f.bob = deliverAt(t, u, inboxName, "From: Bob <bob@example.org>\r\nSubject: Re: Lunch\r\n\r\nyes please, a longer body here\r\n", 3)
	f.carol = deliverAt(t, u, inboxName, "From: carol@example.org\r\nSubject: numbers\r\n\r\nsee attached\r\n", 4, `\Flagged`)
	f.dup = deliverAt(t, u, inboxName, "From: Eve <eve@example.org>\r\nSubject: both\r\n\r\ntwice\r\n", 5)

	if _, err := u.Copy([]model.Meta{*f.dup}, "Archive"); err != nil {
		t.Fatal(err)
	}

	f.boxes = boxIDs(t, u)

	return f
}

// TestEmailQueryDefaultOrder: without a filter every message is one
// result, newest first, however many mailboxes it is in.
func TestEmailQueryDefaultOrder(t *testing.T) {
	f := newQueryFixture(t)

	sameIDs(t, "no filter", queryIDs(t, f.s, `{"accountId":"test"}`),
		f.dup.GUID, f.carol.GUID, f.bob.GUID, f.alice.GUID, f.old.GUID)

	res := mailCall(t, f.s, "Email/query", `{"accountId":"test","calculateTotal":true}`)
	if res["total"] != float64(5) || res["canCalculateChanges"] != true || res["queryState"] == "" {
		t.Errorf("the response is %#v", res)
	}
}

// TestEmailQueryFilters covers the conditions one by one.
func TestEmailQueryFilters(t *testing.T) {
	f := newQueryFixture(t)
	inbox, archive := f.boxes[inboxName], f.boxes["Archive"]

	cases := []struct {
		name, filter string
		want         []string
	}{
		{"inMailbox", `{"inMailbox":"` + archive + `"}`, []string{f.dup.GUID, f.old.GUID}},
		{"inMailboxOtherThan", `{"inMailboxOtherThan":["` + inbox + `"]}`, []string{f.dup.GUID, f.old.GUID}},
		{"hasKeyword", `{"hasKeyword":"$seen"}`, []string{f.alice.GUID}},
		{"notKeyword", `{"inMailbox":"` + inbox + `","notKeyword":"$flagged"}`, []string{f.dup.GUID, f.bob.GUID, f.alice.GUID}},
		{"before", `{"before":"2026-09-20T03:00:00Z"}`, []string{f.alice.GUID, f.old.GUID}},
		{"after", `{"after":"2026-09-20T04:00:00Z"}`, []string{f.dup.GUID, f.carol.GUID}},
		{"minSize", `{"minSize":` + strconv.FormatInt(f.bob.Size, 10) + `}`, bigger(f, f.bob.Size)},
		{"text", `{"text":"numbers"}`, []string{f.carol.GUID, f.old.GUID}},
		{"subject", `{"subject":"lunch"}`, []string{f.bob.GUID, f.alice.GUID}},
		{"body", `{"body":"quarterly"}`, []string{f.old.GUID}},
		{"from", `{"from":"alice@example"}`, []string{f.alice.GUID}},
		{"to", `{"to":"bob"}`, []string{f.alice.GUID}},
		{"header present", `{"header":["X-Priority"]}`, []string{f.alice.GUID}},
		{"header value", `{"header":["X-Priority","1"]}`, []string{f.alice.GUID}},
		{"from, two words", `{"from":"alice example"}`, []string{f.alice.GUID}},
		{"OR", `{"operator":"OR","conditions":[{"from":"bob"},{"from":"carol"}]}`, []string{f.carol.GUID, f.bob.GUID}},
		{"NOT", `{"operator":"NOT","conditions":[{"inMailbox":"` + inbox + `"}]}`, []string{f.old.GUID}},
		{"AND in mailbox with text", `{"operator":"AND","conditions":[{"inMailbox":"` + inbox + `"},{"subject":"lunch"}]}`,
			[]string{f.bob.GUID, f.alice.GUID}},
	}

	for _, c := range cases {
		sameIDs(t, c.name, queryIDs(t, f.s, `{"accountId":"test","filter":`+c.filter+`}`), c.want...)
	}
}

// bigger are the fixture's messages of at least the size, newest first.
func bigger(f *queryFixture, size int64) []string {
	var out []string

	for _, m := range []*model.Meta{f.dup, f.carol, f.bob, f.alice, f.old} {
		if m.Size >= size {
			out = append(out, m.GUID)
		}
	}

	return out
}

// TestEmailQuerySortAndCollapse: the sort keys, the tie-break and a
// collapsed thread.
func TestEmailQuerySortAndCollapse(t *testing.T) {
	f := newQueryFixture(t)

	sameIDs(t, "receivedAt ascending", queryIDs(t, f.s, `{"accountId":"test","sort":[{"property":"receivedAt"}]}`),
		f.old.GUID, f.alice.GUID, f.bob.GUID, f.carol.GUID, f.dup.GUID)

	// Base subjects: both, lunch, lunch, numbers, old report; the two
	// lunches fall back to arrival, newest first.
	sameIDs(t, "subject", queryIDs(t, f.s, `{"accountId":"test","sort":[{"property":"subject"}]}`),
		f.dup.GUID, f.bob.GUID, f.alice.GUID, f.carol.GUID, f.old.GUID)

	// From: alice, bob, carol@example.org, dora, eve.
	sameIDs(t, "from", queryIDs(t, f.s, `{"accountId":"test","sort":[{"property":"from"}]}`),
		f.alice.GUID, f.bob.GUID, f.carol.GUID, f.old.GUID, f.dup.GUID)

	sameIDs(t, "hasKeyword descending", queryIDs(t, f.s,
		`{"accountId":"test","sort":[{"property":"hasKeyword","keyword":"$flagged","isAscending":false}]}`),
		f.carol.GUID, f.dup.GUID, f.bob.GUID, f.alice.GUID, f.old.GUID)

	res := mailCall(t, f.s, "Email/query", `{"accountId":"test","sort":[{"property":"subject"}]}`)
	if res["canCalculateChanges"] != false {
		t.Error("a query in another order claims it can calculate changes")
	}

	// A reply and its root, which arrived earlier than everything else,
	// are one thread; collapsed, the reply stands for it.
	reply := deliverAt(t, f.u, inboxName, "Message-ID: <r@example.org>\r\nIn-Reply-To: <l@example.org>\r\nSubject: Re: x\r\n\r\nb\r\n", 6)
	deliverAt(t, f.u, inboxName, "Message-ID: <l@example.org>\r\nSubject: x\r\n\r\nb\r\n", 0)

	sameIDs(t, "collapsed", queryIDs(t, f.s,
		`{"accountId":"test","collapseThreads":true,"filter":{"inMailbox":"`+f.boxes[inboxName]+`"}}`),
		reply.GUID, f.dup.GUID, f.carol.GUID, f.bob.GUID, f.alice.GUID)
}

// TestEmailQueryPaging: position, anchor and limit work as in
// Mailbox/query.
func TestEmailQueryPaging(t *testing.T) {
	f := newQueryFixture(t)

	sameIDs(t, "position", queryIDs(t, f.s, `{"accountId":"test","position":1,"limit":2}`), f.carol.GUID, f.bob.GUID)
	sameIDs(t, "anchor", queryIDs(t, f.s, `{"accountId":"test","anchor":"`+f.bob.GUID+`","anchorOffset":1,"limit":1}`),
		f.alice.GUID)

	if got := mailError(t, f.s, "Email/query", `{"accountId":"test","anchor":"nope"}`); got != errAnchorNotFound {
		t.Errorf("an unknown anchor gave %s", got)
	}
}

// TestEmailQueryRefuses: what the server does not do is said, not
// quietly ignored.
func TestEmailQueryRefuses(t *testing.T) {
	f := newQueryFixture(t)

	cases := []struct{ args, want string }{
		{`{"accountId":"test","filter":{"nonsense":1}}`, errUnsupportedFilter},
		{`{"accountId":"test","filter":{"operator":"XOR","conditions":[]}}`, errUnsupportedFilter},
		{`{"accountId":"test","filter":{"before":"yesterday"}}`, errUnsupportedFilter},
		{`{"accountId":"test","filter":{"operator":"AND","conditions":[],"hasKeyword":"$seen"}}`, errUnsupportedFilter},
		{`{"accountId":"test","filter":{"conditions":[{"hasKeyword":"$seen"}]}}`, errUnsupportedFilter},
		{`{"accountId":"test","sort":[{"property":"id"}]}`, errUnsupportedSort},
		{`{"accountId":"test","sort":[{"property":"subject","collation":"i;unicode-casemap"}]}`, errUnsupportedSort},
		{`{"accountId":"test","sort":[{"property":"hasKeyword"}]}`, errUnsupportedSort},
	}

	for _, c := range cases {
		if got := mailError(t, f.s, "Email/query", c.args); got != c.want {
			t.Errorf("%s gave %s, want %s", c.args, got, c.want)
		}
	}
}

// TestEmailQueryChanges: a delta against the default order puts a new
// message at its place and takes an expunged one out.
func TestEmailQueryChanges(t *testing.T) {
	f := newQueryFixture(t)
	inbox := f.boxes[inboxName]
	args := `{"accountId":"test","filter":{"inMailbox":"` + inbox + `"}}`

	before := mailCall(t, f.s, "Email/query", args)
	state := before["queryState"].(string)

	fresh := deliverAt(t, f.u, inboxName, "Subject: in between\r\n\r\nb\r\n", 3)
	if err := f.u.Expunge(inboxName, []model.Meta{*f.carol}); err != nil {
		t.Fatal(err)
	}

	res := mailCall(t, f.s, "Email/queryChanges", `{"accountId":"test","filter":{"inMailbox":"`+inbox+
		`"},"sinceQueryState":"`+state+`","calculateTotal":true}`)

	removed, _ := res["removed"].([]any)
	if len(removed) != 2 {
		t.Errorf("removed is %#v, want the new and the expunged message", removed)
	}

	added, _ := res["added"].([]any)
	if len(added) != 1 {
		t.Fatalf("added is %#v, want the new message", added)
	}

	a, _ := added[0].(map[string]any)
	// The new message arrived at the same time as Bob's; the id decides
	// between the two, and the index has to agree with the query now.
	now := queryIDs(t, f.s, args)
	if a["id"] != fresh.GUID || now[int(a["index"].(float64))] != fresh.GUID {
		t.Errorf("added %#v does not match the query now: %v", a, now)
	}

	if res["total"] != float64(len(now)) || res["oldQueryState"] != state {
		t.Errorf("the response is %#v", res)
	}
}

// TestEmailQueryChangesRefuses: other orders, collapsed threads, thread
// conditions and unknown states are cannotCalculateChanges.
func TestEmailQueryChangesRefuses(t *testing.T) {
	f := newQueryFixture(t)
	state := currentState(t, f.s)

	cases := []string{
		`"sort":[{"property":"subject"}]`,
		`"collapseThreads":true`,
		`"filter":{"someInThreadHaveKeyword":"$seen"}`,
	}

	for _, c := range cases {
		args := `{"accountId":"test","sinceQueryState":"` + state + `",` + c + `}`
		if got := mailError(t, f.s, "Email/queryChanges", args); got != errCannotCalculateChanges {
			t.Errorf("%s gave %s", c, got)
		}
	}

	if got := mailError(t, f.s, "Email/queryChanges", `{"accountId":"test","sinceQueryState":"999999"}`); got != errCannotCalculateChanges {
		t.Errorf("a state from the future gave %s", got)
	}

	deliverAt(t, f.u, inboxName, "Subject: more\r\n\r\nb\r\n", 7)

	if got := mailError(t, f.s, "Email/queryChanges",
		`{"accountId":"test","sinceQueryState":"`+state+`","maxChanges":1}`); got != errTooManyChanges {
		t.Errorf("maxChanges gave %s", got)
	}
}
