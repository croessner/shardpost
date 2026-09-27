package jmap

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"

	"shardpost/internal/model"
)

// cursors are the cursor entries a namespace keeps.
func cursors(t *testing.T, u model.User) map[string]string {
	t.Helper()

	md, err := u.GetMetadata("", serverAccessor)
	if err != nil {
		t.Fatal(err)
	}

	out := map[string]string{}

	for k, v := range md {
		if strings.HasPrefix(k, sharedCursorPrefix) {
			out[k] = v
		}
	}

	return out
}

// TestSharedCursorPlace: the cursor is kept where no user can read it —
// in the owner's namespace, under the server's accessor — and users who
// see the same share one, and the same states.
func TestSharedCursorPlace(t *testing.T) {
	f := newSharing(t)

	// carol gets exactly test's rights in alice's namespace.
	for _, b := range fixtureBoxes {
		if b.owner == "alice" && b.rights != "" {
			if err := f.alice.SetACL(b.name, "carol", b.rights); err != nil {
				t.Fatal(err)
			}
		}
	}

	m, e := f.sharedStates(t, "alice")

	_, carol := callAs(t, f.s, "carol", `,"urn:ietf:params:jmap:mail"`, "Email/get", `{"accountId":"alice","ids":[]}`)
	_, carolBoxes := callAs(t, f.s, "carol", `,"urn:ietf:params:jmap:mail"`, "Mailbox/get", `{"accountId":"alice","ids":[]}`)

	if carol["state"] != e || carolBoxes["state"] != m {
		t.Errorf("same view, different states: %v/%v and %s/%s", carolBoxes["state"], carol["state"], m, e)
	}

	if got := cursors(t, f.alice); len(got) != 1 {
		t.Errorf("alice keeps %d cursors, want one for both", len(got))
	}

	for _, u := range []model.User{f.test, f.alice} {
		for _, acc := range []string{"test", "alice"} {
			checkUnreadable(t, u, acc)
		}
	}
}

// checkUnreadable: an account's METADATA of a namespace holds none of
// the server's entries.
func checkUnreadable(t *testing.T, u model.User, acc string) {
	t.Helper()

	md, _ := u.GetMetadata("", acc)
	for k := range md {
		if strings.Contains(k, "sharedcursor") || strings.Contains(k, "statekey") {
			t.Errorf("%s can read %s", acc, k)
		}
	}
}

// TestSharedStateSealed: the token of a shared account does not carry
// the owner's counter, and reads back.
func TestSharedStateSealed(t *testing.T) {
	f := newSharing(t)

	for range 5 {
		deliver(t, f.alice, "Hidden", plainMessage)
	}

	_, e := f.sharedStates(t, "alice")

	seq, _, _ := strings.Cut(e, stateSep)
	if !strings.HasPrefix(seq, sealedPrefix) || len(seq) != len(sealedPrefix)+16 {
		t.Errorf("token %s does not carry a sealed sequence", e)
	}

	key := []byte("0123456789abcdef0123456789abcdef")
	for _, v := range []uint64{0, 1, 41, 1 << 40, ^uint64(0)} {
		if got, ok := openSeq(key, sealSeq(key, v)); !ok || got != v {
			t.Errorf("%d came back as %d %v", v, got, ok)
		}
	}

	if sealSeq(key, 1) == sealSeq([]byte("another key of thirty-two bytes!"), 1) {
		t.Error("two keys, one token")
	}
}

// TestSharedCursorRestarts: a grant is a new state and no delta across
// it; a cursor the log no longer reaches starts again at the newest
// visible entry, so the state stays.
func TestSharedCursorRestarts(t *testing.T) {
	f := newSharing(t)

	_, e0 := f.sharedStates(t, "alice")

	if err := f.alice.SetACL("Hidden", "test", "lr"); err != nil {
		t.Fatal(err)
	}

	deliver(t, f.alice, "Secret", plainMessage) // invisible

	_, e1 := f.sharedStates(t, "alice")
	if e1 == e0 {
		t.Fatal("a grant left the Email state")
	}

	if got := f.mailErr(t, "Email/changes", `{"accountId":"alice","sinceState":"`+e0+`"}`); got != errCannotCalculateChanges {
		t.Errorf("changes across a grant gave %s", got)
	}

	// A stored cursor ahead of the log (a restored namespace).
	for entry, raw := range cursors(t, f.alice) {
		var c sharedCursor

		_ = json.Unmarshal([]byte(raw), &c)
		c.LV, c.Scanned = 999999, 999999
		b, _ := json.Marshal(c)
		bad := string(b)

		if err := f.alice.SetMetadata("", serverAccessor, map[string]*string{entry: &bad}); err != nil {
			t.Fatal(err)
		}
	}

	if _, e := f.sharedStates(t, "alice"); e != e1 {
		t.Errorf("a restart moved the state: %s -> %s", e1, e)
	}

	// Without any cursor: the same.
	for entry := range cursors(t, f.alice) {
		if err := f.alice.SetMetadata("", serverAccessor, map[string]*string{entry: nil}); err != nil {
			t.Fatal(err)
		}
	}

	if _, e := f.sharedStates(t, "alice"); e != e1 {
		t.Errorf("a new cursor moved the state: %s -> %s", e1, e)
	}
}

// TestSharedCursorsPruned: a namespace keeps at most maxCursors, and the
// one written longest ago goes.
func TestSharedCursorsPruned(t *testing.T) {
	f := newSharing(t)

	fill := map[string]*string{}

	for i := range maxCursors {
		v := `{"k":"old` + strconv.Itoa(i) + `","t":` + strconv.Itoa(i+1) + `}`
		fill[sharedCursorPrefix+"old"+strconv.Itoa(i)] = &v
	}

	if err := f.alice.SetMetadata("", serverAccessor, fill); err != nil {
		t.Fatal(err)
	}

	f.sharedStates(t, "alice")

	got := cursors(t, f.alice)
	if len(got) != maxCursors {
		t.Errorf("%d cursors, want %d", len(got), maxCursors)
	}

	if _, ok := got[sharedCursorPrefix+"old0"]; ok {
		t.Error("the oldest cursor was kept")
	}
}

// TestSharedChangesAcrossHiddenRuns: /changes of a shared account reads
// past invisible entries, more than a page of them, pages by visible
// entries, and ends at the state a /get answers; hasMoreChanges only when
// visible entries were left.
func TestSharedChangesAcrossHiddenRuns(t *testing.T) {
	f := newSharing(t)

	_, e0 := f.sharedStates(t, "alice")

	var want []string

	for range 3 {
		want = append(want, deliver(t, f.alice, "Team", plainMessage).GUID)
	}

	for range pushPage + 200 {
		deliver(t, f.alice, "Hidden", plainMessage)
	}

	for range 2 {
		want = append(want, deliver(t, f.alice, "Team", plainMessage).GUID)
	}

	m1, e1 := f.sharedStates(t, "alice")

	got, state := f.pageChanges(t, e0, 2)

	if strings.Join(got, ",") != strings.Join(want, ",") || state != e1 {
		t.Errorf("created %v, want %v; ended at %s, /get answers %s", got, want, state, e1)
	}

	// Only invisible entries after a state: nothing, and no more.
	for range pushPage + 10 {
		deliver(t, f.alice, "Hidden", plainMessage)
	}

	res := f.mail(t, "Mailbox/changes", `{"accountId":"alice","sinceState":"`+m1+`","maxChanges":1}`)
	if res["hasMoreChanges"] != false || res["newState"] != m1 || len(res["updated"].([]any)) != 0 {
		t.Errorf("Mailbox/changes over invisible entries: %v", res)
	}

	res = f.mail(t, "Thread/changes", `{"accountId":"alice","sinceState":"`+e1+`","maxChanges":1}`)
	if res["hasMoreChanges"] != false || res["newState"] != e1 {
		t.Errorf("Thread/changes over invisible entries: %v", res)
	}
}

// pageChanges follows Email/changes of alice's account from a state in
// pages of max, and returns the created ids and the state it ends at.
func (f *sharing) pageChanges(t *testing.T, state string, maxChanges int) ([]string, string) {
	t.Helper()

	var got []string

	for round := 0; ; round++ {
		res := f.mail(t, "Email/changes", `{"accountId":"alice","sinceState":"`+state+`","maxChanges":`+strconv.Itoa(maxChanges)+`}`)
		for _, id := range res["created"].([]any) {
			got = append(got, id.(string))
		}

		state = res["newState"].(string)

		if res["hasMoreChanges"] != true {
			return got, state
		}

		if round > 10 {
			t.Fatalf("no end: %v", res)
		}
	}
}

// TestHiddenWithoutMailbox: an entry that names no mailbox is invisible
// in a shared account and visible in the personal one.
func TestHiddenWithoutMailbox(t *testing.T) {
	e := &model.AccountChange{Kind: model.KindEmail, Op: model.OpUpdated, Partial: true}

	if !hidden(&account{boxes: map[string]*sharedBox{}}, e) {
		t.Error("a shared account sees an entry without a mailbox")
	}

	if hidden(&account{personal: true}, e) {
		t.Error("the personal account does not see it")
	}
}
