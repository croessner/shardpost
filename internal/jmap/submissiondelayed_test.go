package jmap

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"shardpost/internal/auth"
	"shardpost/internal/memstore"
	"shardpost/internal/model"
	"shardpost/internal/sendqueue"
)

// delayedServer is a front-end with delayed sending over a fake relay;
// the queue's worker is driven by the test.
func delayedServer(t *testing.T, maxDelay time.Duration) (*Server, model.User, *fakeRelay, *sendqueue.Service) {
	t.Helper()

	store := memstore.New()

	u, err := store.User("test")
	if err != nil {
		t.Fatal(err)
	}

	for _, n := range []string{inboxName, "Drafts", "Sent"} {
		if err := u.CreateMailbox(n, nil); err != nil {
			t.Fatal(err)
		}
	}

	relay := &fakeRelay{}
	kv := memstore.NewAdminKV()

	q, err := sendqueue.New(sendqueue.Options{KV: kv, Accounts: store, Relay: relay, Book: SubmissionBook(kv, 0, 0)})
	if err != nil {
		t.Fatal(err)
	}

	s := newTestServer(t, func(o *Options) {
		o.Store = store
		o.Auth = identityAuth{Static: auth.Static{"test": "secret"},
			attrs: map[string][]string{"mail": {"test@example.org"}}}
		o.Submission = &SubmissionOptions{Relay: relay, KV: kv, XClient: true, XClientAddr: true, CheckSender: true,
			MaxRecipients: 4, Delayed: &DelayedOptions{Queue: q, MaxDelay: maxDelay}}
	})

	return s, u, relay, q
}

// runWorker sends what is due by running the worker until it has.
func runWorker(t *testing.T, q *sendqueue.Service, done func() bool) {
	t.Helper()

	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})

	go func() {
		defer close(stopped)

		q.Run(ctx)
	}()

	defer func() { cancel(); <-stopped }()

	for range 400 {
		if done() {
			return
		}

		time.Sleep(10 * time.Millisecond)
	}

	t.Fatal("the worker did not get there")
}

func (f *fakeRelay) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.calls
}

// TestDelayedCapability: maxDelayedSend and FUTURERELEASE with the
// longest interval and a latest time no later than now + the delay.
func TestDelayedCapability(t *testing.T) {
	s, _, _, _ := delayedServer(t, 72*time.Hour)

	w := do(t, s, http.MethodGet, PathSession, "")

	var sess struct {
		Accounts map[string]struct {
			AccountCapabilities map[string]json.RawMessage `json:"accountCapabilities"`
		} `json:"accounts"`
	}

	if err := json.Unmarshal(w.Body.Bytes(), &sess); err != nil {
		t.Fatal(err)
	}

	var c struct {
		MaxDelayedSend int64               `json:"maxDelayedSend"`
		Extensions     map[string][]string `json:"submissionExtensions"`
	}

	if err := json.Unmarshal(sess.Accounts["test"].AccountCapabilities[CapSubmission], &c); err != nil {
		t.Fatal(err)
	}

	fr := c.Extensions[extFutureRelease]
	if c.MaxDelayedSend != 72*3600 || len(fr) != 2 || fr[0] != "259200" {
		t.Fatalf("capability %#v", c)
	}

	latest, err := time.Parse(time.RFC3339, fr[1])
	if err != nil || latest.After(time.Now().Add(72*time.Hour)) || latest.Before(time.Now().Add(47*time.Hour)) {
		t.Errorf("latest release %q", fr[1])
	}
}

// submitHold creates a submission of the draft with a hold parameter.
func submitHold(t *testing.T, s *Server, ident, emailID, param, value, extra string) map[string]any {
	t.Helper()

	return submissionCall(t, s, "EmailSubmission/set", `{"accountId":"test","create":{"k":{"identityId":"`+ident+`","emailId":"`+emailID+
		`","envelope":{"mailFrom":{"email":"test@example.org","parameters":{"`+param+`":"`+value+`"}},"rcptTo":[{"email":"bob@example.net"}]}}}`+extra+`}`)
}

// TestDelayedSubmission: HOLDFOR schedules the submission (pending,
// sendAt ahead), onSuccessUpdateEmail applies at once, destroying the
// draft does not change what is sent, and the worker sends the copy and
// turns the submission final.
func TestDelayedSubmission(t *testing.T) {
	s, u, relay, q := delayedServer(t, time.Hour)
	m := deliver(t, u, "Drafts", draftMessage, `\Draft`)
	boxes := boxIDs(t, u)
	ident := identityID(t, s)

	before := time.Now()

	out := call(t, s, `{"using":["urn:ietf:params:jmap:core","urn:ietf:params:jmap:mail","urn:ietf:params:jmap:submission"],
		"methodCalls":[["EmailSubmission/set",{"accountId":"test","create":{"k":{"identityId":"`+ident+`","emailId":"`+m.GUID+`",
		"envelope":{"mailFrom":{"email":"test@example.org","parameters":{"holdfor":"1"}},"rcptTo":[{"email":"bob@example.net"}]}}},
		"onSuccessUpdateEmail":{"#k":{"mailboxIds/`+boxes["Drafts"]+`":null,"mailboxIds/`+boxes["Sent"]+`":true}}},"c1"]]}`)

	rs := responses(t, out)
	if len(rs) != 2 {
		t.Fatalf("responses %#v", rs)
	}

	created, _ := rs[0].([]any)[1].(map[string]any)["created"].(map[string]any)["k"].(map[string]any)
	if created == nil || created[propUndoStatus] != undoPending {
		t.Fatalf("created %#v", rs[0])
	}

	sendAt, _ := time.Parse(time.RFC3339, created[propSendAt].(string))
	if sendAt.Before(before.Add(time.Second).Truncate(time.Second)) {
		t.Errorf("sendAt %v is not a second ahead", sendAt)
	}

	id := created[propID].(string)

	if relay.count() != 0 {
		t.Fatal("sent before its release")
	}

	state := checkPending(t, s, m.GUID, boxes["Sent"], id)

	runWorker(t, q, func() bool { return relay.count() == 1 && submissionStatus(t, s, id) == undoFinal })

	checkDelayedSent(t, s, relay, id, state)
}

// checkDelayedSent checks what TestDelayedSubmission sent and what the
// client sees of it.
func checkDelayedSent(t *testing.T, s *Server, relay *fakeRelay, id, state string) {
	t.Helper()

	if !strings.Contains(relay.raw, "Subject: hello") || strings.Contains(relay.raw, "Bcc") || relay.from != "test@example.org" {
		t.Errorf("relay got from %q:\n%s", relay.from, relay.raw)
	}

	ch := submissionCall(t, s, "EmailSubmission/changes", `{"accountId":"test","sinceState":"`+state+`"}`)
	if upd, _ := ch["updated"].([]any); len(upd) != 1 || upd[0] != id {
		t.Errorf("changes %#v", ch)
	}

	res := submissionCall(t, s, "EmailSubmission/get", `{"accountId":"test","ids":["`+id+`"]}`)
	ds := res["list"].([]any)[0].(map[string]any)[propDeliveryStatus].(map[string]any)

	if d := ds["bob@example.net"].(map[string]any); d["delivered"] != "queued" {
		t.Errorf("deliveryStatus %#v", ds)
	}
}

// checkPending checks a pending submission: the draft was moved at once,
// destroying it does not cancel, get and query show it pending. It
// returns the state.
func checkPending(t *testing.T, s *Server, emailID, sent, id string) string {
	t.Helper()

	if mb := getEmail(t, s, emailID, `["mailboxIds"]`)[propMailboxIDs].(map[string]any); mb[sent] != true {
		t.Errorf("onSuccessUpdateEmail not applied at creation: %#v", mb)
	}

	mailCall(t, s, "Email/set", `{"accountId":"test","destroy":["`+emailID+`"]}`)

	res := submissionCall(t, s, "EmailSubmission/get", `{"accountId":"test","ids":["`+id+`"]}`)
	if obj := res["list"].([]any)[0].(map[string]any); obj[propUndoStatus] != undoPending || obj[propDeliveryStatus] != nil {
		t.Errorf("pending get %#v", obj)
	}

	if q := submissionCall(t, s, "EmailSubmission/query", `{"accountId":"test","filter":{"undoStatus":"pending"}}`); len(q["ids"].([]any)) != 1 {
		t.Errorf("query pending %#v", q)
	}

	return res["state"].(string)
}

func submissionStatus(t *testing.T, s *Server, id string) string {
	t.Helper()

	res := submissionCall(t, s, "EmailSubmission/get", `{"accountId":"test","ids":["`+id+`"]}`)
	if l := res["list"].([]any); len(l) == 1 {
		st, _ := l[0].(map[string]any)[propUndoStatus].(string)
		return st
	}

	return ""
}

// TestDelayedCancel: a pending submission is canceled with undoStatus,
// is never sent, and onSuccessUpdateEmail of the update applies; a
// destroyed pending submission is still sent (RFC 8621 7.5).
func TestDelayedCancel(t *testing.T) {
	s, u, relay, q := delayedServer(t, time.Hour)
	ident := identityID(t, s)
	m := deliver(t, u, "Drafts", draftMessage)

	until := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	res := submitHold(t, s, ident, m.GUID, "HOLDUNTIL", until, "")
	id := res["created"].(map[string]any)["k"].(map[string]any)[propID].(string)

	upd := submissionCall(t, s, "EmailSubmission/set", `{"accountId":"test","update":{"`+id+`":{"undoStatus":"canceled"}}}`)
	if _, ok := upd["updated"].(map[string]any)[id]; !ok {
		t.Fatalf("cancel %#v", upd)
	}

	if st := submissionStatus(t, s, id); st != undoCanceled {
		t.Fatalf("after the cancel %q", st)
	}

	if list, _ := q.List("test"); len(list) != 0 {
		t.Fatalf("queue holds %d after the cancel", len(list))
	}

	// A destroyed pending submission is sent all the same.
	res = submitHold(t, s, ident, m.GUID, "HOLDFOR", "1", "")
	id2 := res["created"].(map[string]any)["k"].(map[string]any)[propID].(string)

	submissionCall(t, s, "EmailSubmission/set", `{"accountId":"test","destroy":["`+id2+`"]}`)

	runWorker(t, q, func() bool { return relay.count() == 1 })

	if st := submissionStatus(t, s, id2); st != "" {
		t.Errorf("the destroyed submission came back as %q", st)
	}

	// Canceling what was sent is cannotUnsend.
	upd = submissionCall(t, s, "EmailSubmission/set", `{"accountId":"test","update":{"`+id+`":{"undoStatus":"canceled"}}}`)
	if _, ok := upd["updated"].(map[string]any)[id]; !ok {
		t.Errorf("canceling a canceled one again %#v", upd)
	}
}

// TestDelayedRefuses: hold parameters beyond maxDelayedSend, two of
// them, bad values and other parameters are invalidProperties; nothing
// is scheduled. A HOLDUNTIL in the past sends at once.
func TestDelayedRefuses(t *testing.T) {
	s, u, relay, q := delayedServer(t, time.Hour)
	ident := identityID(t, s)
	m := deliver(t, u, "Drafts", draftMessage)

	for _, c := range []struct{ param, value string }{
		{"HOLDFOR", "3601"}, {"HOLDFOR", "-1"}, {"HOLDFOR", "1.5"}, {"HOLDFOR", ""}, {"HOLDFOR", "0"},
		{"HOLDFOR", "0000000060"}, {"HOLDUNTIL", time.Now().Add(time.Minute).Format("2006-01-02T15:04:05+00:00")},
		{"HOLDUNTIL", time.Now().Add(2 * time.Hour).UTC().Format(time.RFC3339)}, {"HOLDUNTIL", "tomorrow"},
		{"SIZE", "100"},
	} {
		if got := setErrorType(submitHold(t, s, ident, m.GUID, c.param, c.value, ""), "notCreated", "k"); got != setInvalidProperties {
			t.Errorf("%s=%s gave %q", c.param, c.value, got)
		}
	}

	two := submissionCall(t, s, "EmailSubmission/set", `{"accountId":"test","create":{"k":{"identityId":"`+ident+`","emailId":"`+m.GUID+
		`","envelope":{"mailFrom":{"email":"test@example.org","parameters":{"HOLDFOR":"10","HOLDUNTIL":"`+
		time.Now().Add(time.Minute).UTC().Format(time.RFC3339)+`"}},"rcptTo":[{"email":"bob@example.net"}]}}}}`)
	if got := setErrorType(two, "notCreated", "k"); got != setInvalidProperties {
		t.Errorf("two hold parameters gave %q", got)
	}

	if list, _ := q.List("test"); len(list) != 0 {
		t.Fatalf("refused submissions were scheduled: %d", len(list))
	}

	for _, c := range []struct{ param, value string }{
		{"HOLDUNTIL", time.Now().Add(-time.Minute).UTC().Format(time.RFC3339)},
	} {
		res := submitHold(t, s, ident, m.GUID, c.param, c.value, "")
		if k, _ := res["created"].(map[string]any)["k"].(map[string]any); k == nil || k[propUndoStatus] != undoFinal {
			t.Errorf("%s=%s gave %#v", c.param, c.value, res)
		}
	}

	if relay.count() != 1 {
		t.Errorf("relay calls %d", relay.count())
	}
}

// TestHoldWithoutDelayedSending: a server without delayed sending (and a
// 0.42 server, which has the same code path) refuses a hold parameter
// with invalidProperties and sends nothing, as before.
func TestHoldWithoutDelayedSending(t *testing.T) {
	s, u, relay := submissionServer(t)
	ident := identityID(t, s)
	m := deliver(t, u, "Drafts", draftMessage)

	for _, p := range []string{"HOLDFOR", "HOLDUNTIL"} {
		v := "60"
		if p == "HOLDUNTIL" {
			v = time.Now().Add(time.Minute).UTC().Format(time.RFC3339)
		}

		if got := setErrorType(submitHold(t, s, ident, m.GUID, p, v, ""), "notCreated", "k"); got != setInvalidProperties {
			t.Errorf("%s gave %q", p, got)
		}
	}

	if relay.calls != 0 {
		t.Errorf("relay calls %d", relay.calls)
	}

	w := do(t, s, http.MethodGet, PathSession, "")
	if !strings.Contains(w.Body.String(), `"maxDelayedSend":0`) {
		t.Errorf("session %s", w.Body.String())
	}
}

// TestHoldUntilSlack: a HOLDUNTIL just past now + maxDelayedSend (a
// client clock a little ahead) is accepted.
func TestHoldUntilSlack(t *testing.T) {
	now := time.Now()

	if _, serr := holdUntil(now.Add(time.Hour+30*time.Second).UTC().Format(time.RFC3339), time.Hour, now); serr != nil {
		t.Errorf("within the slack: %#v", serr)
	}

	if got, serr := holdFor(strconv.Itoa(3600), time.Hour, now); serr != nil || !got.Equal(now.Add(time.Hour).UTC()) {
		t.Errorf("HOLDFOR=3600: %v %#v", got, serr)
	}
}

// TestSettleWithoutRecord: a submission the worker settles before (or
// without) its record in the account's records is added from the view
// the queue kept; one the client destroyed stays destroyed, and an
// account without records gets none.
func TestSettleWithoutRecord(t *testing.T) {
	kv := memstore.NewAdminKV()
	book := SubmissionBook(kv, 0, 0)
	opts := submissionDefaults(&SubmissionOptions{}, 0)

	view := func(id string) []byte {
		raw, _ := json.Marshal(&submission{ID: id, EmailID: "M1", SendAt: time.Now().UTC(), UndoStatus: undoPending})
		return raw
	}

	final := func(id string) *sendqueue.Record {
		return &sendqueue.Record{Account: "test", ID: id, State: sendqueue.StateFinal, View: view(id),
			Delivery: map[string]sendqueue.Delivery{"bob@example.net": {SMTPReply: "250 ok", Delivered: sendqueue.DeliveredQueued}}}
	}

	// No records at all (a purged account): nothing is created.
	if err := book.Settle(final("S01")); err != nil {
		t.Fatal(err)
	}

	if _, err := kv.Get(submissionKey("test")); err == nil {
		t.Fatal("settling created the records of an account without any")
	}

	// The account has records, one destroyed by the client.
	if err := updateSubmissions(kv, "test", opts, func(b *submissionBook) error {
		b.Subs = append(b.Subs, &submission{ID: "S02", UndoStatus: undoPending})
		b.note("S02", model.OpCreated)
		b.Subs = b.Subs[:0]
		b.note("S02", model.OpDestroyed)

		return nil
	}); err != nil {
		t.Fatal(err)
	}

	for _, id := range []string{"S02", "S03"} {
		if err := book.Settle(final(id)); err != nil {
			t.Fatal(err)
		}
	}

	b, err := loadSubmissions(kv, "test", opts)
	if err != nil {
		t.Fatal(err)
	}

	if b.find("S02") != nil {
		t.Error("a destroyed submission came back")
	}

	s := b.find("S03")
	if s == nil || s.UndoStatus != undoFinal || s.EmailID != "M1" || s.DeliveryStatus["bob@example.net"].Delivered != "queued" {
		t.Fatalf("the missing submission was not added: %#v", s)
	}
}

// TestDelayedRecordedBeforeScheduled: the account's record of a delayed
// submission exists before the queue has it, so a worker that sends at
// once settles it to final.
func TestDelayedRecordedBeforeScheduled(t *testing.T) {
	s, u, relay, q := delayedServer(t, time.Hour)
	ident := identityID(t, s)
	m := deliver(t, u, "Drafts", draftMessage)

	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})

	go func() {
		defer close(stopped)

		q.Run(ctx)
	}()

	defer func() { cancel(); <-stopped }()

	res := submitHold(t, s, ident, m.GUID, "HOLDFOR", "1", "")
	id := res["created"].(map[string]any)["k"].(map[string]any)[propID].(string)

	for range 400 {
		if relay.count() == 1 && submissionStatus(t, s, id) == undoFinal {
			return
		}

		time.Sleep(10 * time.Millisecond)
	}

	t.Fatalf("calls %d, status %q", relay.count(), submissionStatus(t, s, id))
}
