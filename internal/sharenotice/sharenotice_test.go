package sharenotice

import (
	"testing"
	"time"

	"shardpost/internal/memstore"
	"shardpost/internal/model"
)

// open opens a namespace of the in-memory store.
func open(t *testing.T, st *memstore.Store, name string) model.User {
	t.Helper()

	u, err := st.User(name)
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(u.Close)

	return u
}

// load reads the record with the bounds applied.
func load(t *testing.T, u model.User, account string, lim Limits, now time.Time) *Log {
	t.Helper()

	l, err := Load(u, account, lim, now)
	if err != nil {
		t.Fatal(err)
	}

	return l
}

// TestRecordCoalesces: a second change of the same mailbox replaces the
// first, keeping its old rights; a change back to where it started
// leaves nothing (RFC 9670 6.3).
func TestRecordCoalesces(t *testing.T) {
	st := memstore.New()
	u := open(t, st, "bob")
	now := time.Unix(1_800_000_000, 0)

	for _, n := range []Notice{
		{Owner: "alice", MailboxID: "m1", Name: "Team", Old: "", New: "lr"},
		{Owner: "alice", MailboxID: "m1", Name: "Team", Old: "lr", New: "lrs", By: "carol"},
		{Owner: "alice", MailboxID: "m2", Name: "Other", Old: "", New: "l"},
	} {
		if err := Record(u, "bob", n, Limits{}, now); err != nil {
			t.Fatal(err)
		}
	}

	l := load(t, u, "bob", Limits{}, now)
	if len(l.Notices) != 2 {
		t.Fatalf("%d notices, want 2 (coalesced): %+v", len(l.Notices), l.Notices)
	}

	if n := l.Notices[0]; n.MailboxID != "m1" || n.Old != "" || n.New != "lrs" || n.By != "carol" {
		t.Errorf("the coalesced notice is %+v", n)
	}

	// Back to no rights: the notice about m2 disappears altogether.
	if err := Record(u, "bob", Notice{Owner: "alice", MailboxID: "m2", Old: "l", New: ""}, Limits{}, now); err != nil {
		t.Fatal(err)
	}

	l = load(t, u, "bob", Limits{}, now)
	if len(l.Notices) != 1 || l.Notices[0].MailboxID != "m1" {
		t.Fatalf("after the change back: %+v", l.Notices)
	}
}

// TestBounds: the count and the age bound the record; the oldest go
// first, and every notice that goes is a destruction /changes reports.
func TestBounds(t *testing.T) {
	st := memstore.New()
	u := open(t, st, "bob")
	start := time.Unix(1_800_000_000, 0)
	lim := Limits{Max: 3, MaxAge: time.Hour}

	for i, box := range []string{"a", "b", "c", "d", "e"} {
		n := Notice{Owner: "alice", MailboxID: box, Name: box, New: "lr"}
		if err := Record(u, "bob", n, lim, start.Add(time.Duration(i)*time.Minute)); err != nil {
			t.Fatal(err)
		}
	}

	l := load(t, u, "bob", lim, start.Add(5*time.Minute))
	if got := boxes(l); got != "cde" {
		t.Fatalf("kept %q, want the newest three", got)
	}

	created, destroyed, _, _, ok := l.Changes(0, 100)
	if !ok || len(created) != 3 || len(destroyed) != 0 {
		t.Errorf("changes since 0: created %v destroyed %v ok %v; a notice that came and went is news to nobody",
			created, destroyed, ok)
	}

	// After an hour and a half c, d and e have expired as well; the read
	// removes them from the stored record too.
	l = load(t, u, "bob", lim, start.Add(90*time.Minute))
	if len(l.Notices) != 0 {
		t.Fatalf("expired notices kept: %+v", l.Notices)
	}

	if again := load(t, u, "bob", lim, start.Add(90*time.Minute)); again.Seq != l.Seq {
		t.Errorf("the pruned record was not stored: seq %d then %d", l.Seq, again.Seq)
	}

	if HardMax < (Limits{Max: 10_000}).WithDefaults().Max {
		t.Error("Max is not capped at HardMax")
	}
}

// boxes lists the mailbox ids of the notices in order.
func boxes(l *Log) string {
	out := ""
	for _, n := range l.Notices {
		out += n.MailboxID
	}

	return out
}

// TestChangesAndDestroy: /changes reports creations and destructions
// after a state, pages by maxChanges, and refuses a state the record no
// longer reaches back to.
func TestChangesAndDestroy(t *testing.T) {
	st := memstore.New()
	u := open(t, st, "bob")
	now := time.Unix(1_800_000_000, 0)
	lim := Limits{Max: 2}

	record := func(box string) {
		t.Helper()

		if err := Record(u, "bob", Notice{Owner: "alice", MailboxID: box, New: "l"}, lim, now); err != nil {
			t.Fatal(err)
		}
	}

	record("a")
	record("b")

	l := load(t, u, "bob", lim, now)
	state := l.Seq
	first := l.Notices[0].ID

	if c, _, s, more, _ := l.Changes(0, 1); !more || s != l.Notices[0].Seq || len(c) != 1 {
		t.Errorf("a page of one: created %v state %d more %v", c, s, more)
	}

	checkDestroy(t, u, lim, now, state, first)


	// Enough churn to push the oldest destructions out of the record.
	for _, b := range []string{"c", "d", "e", "f", "g", "h", "i", "j", "k", "l"} {
		record(b)
	}

	l = load(t, u, "bob", lim, now)
	if _, _, _, _, ok := l.Changes(state, 100); ok {
		t.Error("a state older than the remembered destructions is answered with a delta")
	}

	if _, _, _, _, ok := l.Changes(l.Seq+1, 100); ok {
		t.Error("a state from the future is answered with a delta")
	}
}

// checkDestroy destroys a notice and checks that /changes reports it.
func checkDestroy(t *testing.T, u model.User, lim Limits, now time.Time, state uint64, id string) {
	t.Helper()

	after, destroyed, err := Destroy(u, "bob", []string{id, "nope"}, lim, now)
	if err != nil || len(destroyed) != 1 || destroyed[0] != id {
		t.Fatalf("destroy: %v %v", destroyed, err)
	}

	created, gone, newState, more, ok := after.Changes(state, 100)
	if !ok || len(created) != 0 || len(gone) != 1 || gone[0] != id || more || newState != after.Seq {
		t.Errorf("changes after the destruction: %v %v %d %v %v", created, gone, newState, more, ok)
	}
}

// fakeStore opens namespaces of the in-memory store and counts them.
type fakeStore struct {
	st     *memstore.Store
	opened int
}

func (f *fakeStore) User(name string) (model.User, error) {
	f.opened++

	return f.st.User(name)
}

// TestRecorderSkips: no notice for "anyone", a negative identifier, the
// owner, a change the grantee made itself, or rights that stay the same;
// one for a real change.
func TestRecorderSkips(t *testing.T) {
	fs := &fakeStore{st: memstore.New()}
	r := &Recorder{Store: fs}

	for _, c := range []Change{
		{Owner: "alice", MailboxID: "m", Grantee: "anyone", Old: "", New: "lr"},
		{Owner: "alice", MailboxID: "m", Grantee: "-bob", Old: "", New: "lr"},
		{Owner: "alice", MailboxID: "m", Grantee: "alice", Old: "", New: "lr"},
		{Owner: "alice", MailboxID: "m", Grantee: "bob", By: "bob", Old: "", New: "lr"},
		{Owner: "alice", MailboxID: "m", Grantee: "bob", Old: "rl", New: "lr"},
	} {
		r.Record(c)
	}

	if fs.opened != 0 {
		t.Fatalf("%d namespaces opened for changes that need no notice", fs.opened)
	}

	r.Record(Change{Owner: "alice", MailboxID: "m", Mailbox: "Team", Grantee: "bob", By: "alice", Old: "", New: "lr"})

	l := load(t, open(t, fs.st, "bob"), "bob", Limits{}, time.Now())
	if len(l.Notices) != 1 || l.Notices[0].Name != "Team" || l.Notices[0].New != "lr" {
		t.Fatalf("the notice is %+v", l.Notices)
	}

	var nilRecorder *Recorder

	nilRecorder.Record(Change{Owner: "alice", Grantee: "bob", New: "l"}) // must not panic
}

// TestEffective: the rights through the account's own entry and
// "anyone", in canonical order.
func TestEffective(t *testing.T) {
	if got := Effective("rl", "s"); got != "lrs" {
		t.Errorf("Effective = %q", got)
	}
}
