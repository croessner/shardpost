package jmap

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"shardpost/internal/memstore"
	"shardpost/internal/model"
	"shardpost/internal/sharenotice"
)

// sharedStates are the Mailbox and Email states of an account as the
// method responses of test answer them.
func (f *sharing) sharedStates(t *testing.T, id string) (mailbox, email string) {
	t.Helper()

	m := f.mail(t, "Mailbox/get", `{"accountId":"`+id+`","ids":[]}`)
	e := f.mail(t, "Email/get", `{"accountId":"`+id+`","ids":[]}`)

	return m["state"].(string), e["state"].(string)
}

// TestSharedStateIgnoresInvisibleChanges: what the owner does where the
// user cannot see leaves the states of the shared account alone; a
// change it can see moves them, and /changes from the old state reports
// exactly it and ends at the state a /get answers.
func TestSharedStateIgnoresInvisibleChanges(t *testing.T) {
	f := newSharing(t)

	m0, e0 := f.sharedStates(t, "alice")

	// INBOX and Hidden: no rights; Secret: r without l; Listed: l
	// without r (visible, its messages are not); a new mailbox.
	for _, box := range []string{inboxName, "Hidden", "Secret", "Listed"} {
		deliver(t, f.alice, box, plainMessage)
	}

	mustCreate(t, f.alice, "Elsewhere")

	if m, e := f.sharedStates(t, "alice"); m != m0 || e != e0 {
		t.Fatalf("invisible changes moved the states: %s/%s -> %s/%s", m0, e0, m, e)
	}

	res := f.mail(t, "Email/changes", `{"accountId":"alice","sinceState":"`+e0+`"}`)
	if res["newState"] != e0 || len(res["created"].([]any)) != 0 {
		t.Errorf("changes over invisible entries: %v", res)
	}

	msg := deliver(t, f.alice, "Team", plainMessage)
	deliver(t, f.alice, "Hidden", plainMessage) // after the visible one

	m1, e1 := f.sharedStates(t, "alice")
	if m1 == m0 || e1 == e0 {
		t.Fatalf("a delivery into Team left the states: %s %s", m1, e1)
	}

	res = f.mail(t, "Email/changes", `{"accountId":"alice","sinceState":"`+e0+`"}`)
	if created := res["created"].([]any); len(created) != 1 || created[0] != msg.GUID || res["newState"] != e1 {
		t.Errorf("Email/changes %v, want %s up to %s", res, msg.GUID, e1)
	}

	res = f.mail(t, "Mailbox/changes", `{"accountId":"alice","sinceState":"`+m0+`"}`)
	if res["newState"] != m1 {
		t.Errorf("Mailbox/changes ended at %v, /get answers %s", res["newState"], m1)
	}
}

// TestPublicStateIgnoresInvisibleChanges: the same for the public
// folders.
func TestPublicStateIgnoresInvisibleChanges(t *testing.T) {
	f := newSharing(t)

	pm0, pe0 := f.sharedStates(t, model.PublicNamespace)

	deliver(t, f.public, "Closed", plainMessage)

	if m, e := f.sharedStates(t, model.PublicNamespace); m != pm0 || e != pe0 {
		t.Fatalf("an invisible public change moved the states: %s/%s -> %s/%s", pm0, pe0, m, e)
	}

	deliver(t, f.public, "News", plainMessage)

	if _, e := f.sharedStates(t, model.PublicNamespace); e == pe0 {
		t.Error("a delivery into News left the public state")
	}
}

// sharingPush is the sharing fixture with a fast event source poll.
func sharingPush(t *testing.T, extra ...func(*Options)) (*sharing, *httptest.Server) {
	t.Helper()

	f := newSharing(t, append([]func(*Options){func(o *Options) {
		o.Push.EventPoll = 50 * time.Millisecond
	}}, extra...)...)

	hs := httptest.NewServer(f.s)

	t.Cleanup(func() { hs.Close(); f.s.Close() })

	return f, hs
}

// nextChange reads events until a state event and returns it.
func nextChange(t *testing.T, r *bufio.Reader) (string, *stateChange) {
	t.Helper()

	for {
		event, id, data := readEvent(t, r)
		if event != "state" {
			continue
		}

		var sc stateChange
		if err := json.Unmarshal([]byte(data), &sc); err != nil {
			t.Fatal(err)
		}

		return id, &sc
	}
}

// TestEventSourceSharedAccounts: a visible change of a shared account is
// a StateChange keyed by its id with the states a /get answers; an
// invisible one is none; a grant, an account that appears and one that
// goes, and a share notification are pushed too.
func TestEventSourceSharedAccounts(t *testing.T) {
	f, hs := sharingPush(t)

	resp, r := openEvents(t, hs.URL, "types=*&closeafter=no&ping=0", "")
	defer func() { _ = resp.Body.Close() }()

	deliver(t, f.alice, "Hidden", plainMessage)
	deliver(t, f.alice, "Listed", plainMessage)
	time.Sleep(200 * time.Millisecond) // several polls: nothing to say

	deliver(t, f.alice, "Team", plainMessage)

	_, sc := nextChange(t, r)

	m, e := f.sharedStates(t, "alice")
	if got := sc.Changed["alice"]; got[typeEmail] != e || got[typeEmailDelivery] != e || got[typeMailbox] != m {
		t.Fatalf("first event %+v, want the Team delivery at %s/%s", sc.Changed, m, e)
	}

	if len(sc.Changed) != 1 {
		t.Errorf("the event names other accounts: %+v", sc.Changed)
	}
}

// TestEventSourceSharedRights: a grant, an account that appears and one
// that goes, and a share notification are pushed.
func TestEventSourceSharedRights(t *testing.T) {
	f, hs := sharingPush(t)

	resp, r := openEvents(t, hs.URL, "types=*&closeafter=no&ping=0", "")
	defer func() { _ = resp.Body.Close() }()

	// A grant changes what test sees in alice's account.
	if err := f.alice.SetACL("Hidden", "test", "lr"); err != nil {
		t.Fatal(err)
	}

	_, sc := nextChange(t, r)
	if m2, _ := f.sharedStates(t, "alice"); sc.Changed["alice"][typeMailbox] != m2 {
		t.Fatalf("after the grant %+v, want Mailbox %s", sc.Changed, m2)
	}

	// bob shares a mailbox with test: a new account.
	if err := f.bob.SetACL("Everyone", "test", "lr"); err != nil {
		t.Fatal(err)
	}

	_, sc = nextChange(t, r)
	if bm, _ := f.sharedStates(t, "bob"); sc.Changed["bob"][typeMailbox] != bm {
		t.Fatalf("the new account: %+v, want %s", sc.Changed, bm)
	}

	// and takes it back: the account is gone.
	if err := f.bob.SetACL("Everyone", "test", ""); err != nil {
		t.Fatal(err)
	}

	_, sc = nextChange(t, r)
	if sc.Changed["bob"][typeEmail] != stateRevoked {
		t.Fatalf("the account that went: %+v", sc.Changed)
	}

	// A share notification is a change of the personal account.
	if err := sharenotice.Record(f.test, "test", sharenotice.Notice{Owner: "alice", MailboxID: "x", Name: "Team", New: "lr"},
		sharenotice.Limits{}, time.Now()); err != nil {
		t.Fatal(err)
	}

	_, sc = nextChange(t, r)
	if sc.Changed["test"][typeShareNotification] == "" {
		t.Fatalf("the notification: %+v", sc.Changed)
	}
}

// TestEventSourceSharedFilterAndReconnect: the types filter holds for
// shared accounts, and a client that reconnects with an event id whose
// digest is not the current one is sent the states of the shared
// accounts at once.
func TestEventSourceSharedFilterAndReconnect(t *testing.T) {
	f, hs := sharingPush(t)

	resp, r := openEvents(t, hs.URL, "types=Mailbox&closeafter=state&ping=0", "")

	deliver(t, f.public, "Staff", plainMessage)

	id, sc := nextChange(t, r)
	_ = resp.Body.Close()

	if ts := sc.Changed[model.PublicNamespace]; ts[typeMailbox] == "" || ts[typeEmail] != "" {
		t.Fatalf("filtered event %+v", sc.Changed)
	}

	seq, digest, ok := parsePushState(id)
	if !ok || digest == "" {
		t.Fatalf("event id %q carries no digest", id)
	}

	// Same id: nothing missed. Another digest: the shared states.
	deliver(t, f.alice, "Team", plainMessage)

	resp, r = openEvents(t, hs.URL, "types=*&closeafter=state&ping=0", pushState(seq, digest))
	_, sc = nextChange(t, r)
	_ = resp.Body.Close()

	if _, e := f.sharedStates(t, "alice"); sc.Changed["alice"][typeEmail] != e {
		t.Errorf("reconnect %+v, want alice at %s", sc.Changed, e)
	}
}

// TestSharedPushBounds: a follower holds at most its bound of watches and
// polls the rest; a hub watches at most its bound of namespaces and
// shares one watch between followers; closing gives the watches back.
func TestSharedPushBounds(t *testing.T) {
	f := newSharing(t)

	if err := f.bob.SetACL("Everyone", "test", "lr"); err != nil {
		t.Fatal(err)
	}

	hub := newWatchHub(f.store, 2, f.s.o.Log)

	sess := &session{account: "test", user: f.test, own: f.test, srv: f.s, views: &views{store: f.store}, log: f.s.o.Log}
	defer sess.views.close()

	sp := newSharedPush(f.s, sess, hub, 1)

	polled := ""
	watched := 0

	for id, tr := range sp.accts {
		switch {
		case tr.release != nil:
			watched++
		case id != model.PublicNamespace:
			polled = id
		}
	}

	if len(sp.accts) != 3 || watched != 1 {
		t.Fatalf("%d accounts, %d watched; want 3 and 1", len(sp.accts), watched)
	}

	// A polled account's change is found on the poll, not before.
	owner := map[string]model.User{"alice": f.alice, "bob": f.bob}[polled]
	box := map[string]string{"alice": "Team", "bob": "Everyone"}[polled]
	deliver(t, owner, box, plainMessage)

	if got := sp.check(false); len(got) != 0 {
		t.Errorf("a check without the poll found %+v", got)
	}

	if got := sp.check(true); got[polled][typeEmail] == "" {
		t.Errorf("the poll found %+v, want %s", got, polled)
	}

	other := newSharedPush(f.s, sess, hub, 3)
	if hub.size() != 2 {
		t.Errorf("the hub watches %d namespaces, want its bound 2", hub.size())
	}

	sp.close()
	other.close()

	if hub.size() != 0 {
		t.Errorf("%d watches left after closing", hub.size())
	}
}

// TestPushWorkerSharedAccounts: the worker pushes a visible change of a
// shared account, keyed by its id and within the subscription's types,
// and nothing for an invisible one.
func TestPushWorkerSharedAccounts(t *testing.T) {
	fake := &fakePushService{status: http.StatusCreated}
	ps := httptest.NewTLSServer(fake)

	defer ps.Close()

	sender, vapid := testSender(t)
	kv := memstore.NewAdminKV()

	f := newSharing(t, func(o *Options) {
		o.Push = PushOptions{KV: kv, VAPIDPublicKey: vapid, AllowInternalURLs: true}
	})

	res := mailCall(t, f.s, "PushSubscription/set",
		`{"create":{"p":{"deviceClientId":"dev","url":"`+ps.URL+`/x","types":["Email"]}}}`)
	id, _ := res["created"].(map[string]any)["p"].(map[string]any)[propID].(string)
	mailCall(t, f.s, "PushSubscription/set", `{"update":{"`+id+`":{"verificationCode":"`+storedCode(t, kv, id)+`"}}}`)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})

	go func() {
		defer close(done)

		_ = RunPushWorker(ctx, WorkerOptions{Store: f.store, KV: kv, Sender: sender, Instance: "w1", VAPIDPublicKey: vapid,
			Sharing: f.s.o.Sharing, Scan: 50 * time.Millisecond, Verify: 20 * time.Millisecond,
			Poll: 50 * time.Millisecond, Coalesce: 10 * time.Millisecond})
	}()

	defer func() { cancel(); <-done }()

	time.Sleep(200 * time.Millisecond) // the worker follows the account

	n := len(fake.got())

	deliver(t, f.alice, "Hidden", plainMessage)
	time.Sleep(300 * time.Millisecond)

	if got := fake.got(); len(got) != n {
		t.Fatalf("an invisible change was pushed: %v", got[n:])
	}

	deliver(t, f.alice, "Team", plainMessage)

	waitFor(t, "the shared push", func() bool { return len(fake.got()) > n })

	var sc stateChange
	if err := json.Unmarshal([]byte(fake.got()[n]), &sc); err != nil {
		t.Fatal(err)
	}

	if _, e := f.sharedStates(t, "alice"); sc.Changed["alice"][typeEmail] != e || sc.Changed["alice"][typeMailbox] != "" {
		t.Errorf("push %+v, want alice Email %s only", sc.Changed, e)
	}

	// The state saved for the account carries the shared states, so a
	// worker that takes over pushes what it missed.
	raw, err := kv.Get(pushSeenKey("test"))
	if err != nil || !strings.Contains(string(raw), `"alice"`) {
		t.Errorf("saved state %s %v", raw, err)
	}
}

// TestDiffStates: what a new worker pushes from a saved state.
func TestDiffStates(t *testing.T) {
	was := map[string]map[string]string{
		"alice": {typeEmail: "1-a", typeMailbox: "1-b"},
		"gone":  {typeEmail: "4-x"},
		"test":  {typeShareNotification: "3"},
	}
	now := map[string]map[string]string{
		"alice": {typeEmail: "2-a", typeMailbox: "1-b"},
		"new":   {typeEmail: "5-y"},
	}

	got := diffStates(was, now)
	if got["alice"][typeEmail] != "2-a" || got["alice"][typeMailbox] != "" || got["new"][typeEmail] != "5-y" ||
		got["gone"][typeEmail] != stateRevoked || got["test"] != nil {
		t.Errorf("diff %+v", got)
	}
}
