package jmap

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"shardpost/internal/auth"
	"shardpost/internal/memstore"
	"shardpost/internal/model"
	"shardpost/internal/webpush"
)

// pushServer is a front-end with push subscriptions over a store and an
// admin store the test keeps.
func pushServer(t *testing.T) (*Server, model.User, *memstore.AdminKV, *memstore.Store) {
	t.Helper()

	store := memstore.New()

	u, err := store.User("test")
	if err != nil {
		t.Fatal(err)
	}

	if err := u.CreateMailbox(inboxName, nil); err != nil {
		t.Fatal(err)
	}

	kv := memstore.NewAdminKV()
	s := newTestServer(t, func(o *Options) {
		o.Store = store
		o.Push = PushOptions{KV: kv, VAPIDPublicKey: "BTestKey", EventPoll: 50 * time.Millisecond}
	})

	return s, u, kv, store
}

// readEvent reads one server-sent event.
func readEvent(t *testing.T, r *bufio.Reader) (event, id, data string) {
	t.Helper()

	for {
		line, err := r.ReadString('\n')
		if err != nil {
			t.Fatalf("event stream: %v", err)
		}

		line = strings.TrimRight(line, "\n")

		switch {
		case line == "":
			return event, id, data
		case strings.HasPrefix(line, "event: "):
			event = line[len("event: "):]
		case strings.HasPrefix(line, "id: "):
			id = line[len("id: "):]
		case strings.HasPrefix(line, "data: "):
			data = line[len("data: "):]
		}
	}
}

// openEvents connects to the event source.
func openEvents(t *testing.T, base, query, lastID string) (*http.Response, *bufio.Reader) {
	t.Helper()

	req, _ := http.NewRequest(http.MethodGet, base+PathEvents+"?"+query, nil)
	req.SetBasicAuth("test", "secret")

	if lastID != "" {
		req.Header.Set("Last-Event-ID", lastID)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}

	if resp.StatusCode != http.StatusOK || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		t.Fatalf("event source answered %d %s", resp.StatusCode, resp.Header.Get("Content-Type"))
	}

	return resp, bufio.NewReader(resp.Body)
}

// TestEventSource: a delivery is a state event naming the email types,
// with the state as its id; closeafter=state ends the stream after it.
func TestEventSource(t *testing.T) {
	s, u, _, _ := pushServer(t)
	hs := httptest.NewServer(s)

	defer hs.Close()
	defer s.Close()

	resp, r := openEvents(t, hs.URL, "types=*&closeafter=state&ping=0", "")
	defer func() { _ = resp.Body.Close() }()

	deliver(t, u, inboxName, plainMessage)

	event, id, data := readEvent(t, r)
	if event != "state" || id == "" {
		t.Fatalf("got %q %q %q", event, id, data)
	}

	var sc stateChange
	if err := json.Unmarshal([]byte(data), &sc); err != nil || sc.Type != "StateChange" {
		t.Fatalf("data %s", data)
	}

	ts := sc.Changed["test"]
	for _, typ := range []string{typeEmail, typeThread, typeMailbox, typeEmailDelivery} {
		if ts[typ] != id {
			t.Errorf("%s is %q, want %q (%s)", typ, ts[typ], id, data)
		}
	}

	if _, err := io.ReadAll(r); err != nil {
		t.Errorf("the stream did not end cleanly: %v", err)
	}
}

// TestEventSourceCatchesUpAndFilters: Last-Event-ID brings what was
// missed, the types filter narrows it, and a ping carries its interval.
func TestEventSourceCatchesUpAndFilters(t *testing.T) {
	s, u, _, _ := pushServer(t)
	hs := httptest.NewServer(s)

	defer hs.Close()
	defer s.Close()

	before, _ := u.AccountState()

	if err := u.CreateMailbox("Folder", nil); err != nil {
		t.Fatal(err)
	}

	resp, r := openEvents(t, hs.URL, "types=Mailbox&closeafter=no&ping=15", stateString(before))
	defer func() { _ = resp.Body.Close() }()

	event, _, data := readEvent(t, r)
	if event != "state" || !strings.Contains(data, `"Mailbox"`) || strings.Contains(data, `"Email"`) {
		t.Fatalf("got %q %s", event, data)
	}

	if p, err := readEventParams(httptest.NewRequest(http.MethodGet, "/?types=*&ping=1", nil)); err != nil || p.ping != minPing {
		t.Errorf("ping 1 is clamped to %v (%v)", p, err)
	}

	if _, err := readEventParams(httptest.NewRequest(http.MethodGet, "/?types=*&closeafter=maybe", nil)); err == nil {
		t.Error("closeafter=maybe was accepted")
	}
}

// TestPushSubscriptionLifecycle: create, the verification queued for
// the worker, verify, extend, get without url and keys, destroy.
func TestPushSubscriptionLifecycle(t *testing.T) {
	s, _, kv, _ := pushServer(t)

	res := mailCall(t, s, "PushSubscription/set", `{"create":{"p":{"deviceClientId":"dev1",
		"url":"https://push.example.net/abc","types":["Email"]}}}`)

	created, _ := res["created"].(map[string]any)["p"].(map[string]any)
	id, _ := created[propID].(string)

	if id == "" || created[propExpires] == nil {
		t.Fatalf("created %#v (%#v)", created, res["notCreated"])
	}

	if raw, err := kv.Get(pushVerifyQueue); err != nil || !strings.Contains(string(raw), id) {
		t.Errorf("no verification queued: %s %v", raw, err)
	}

	code := storedCode(t, kv, id)

	res = mailCall(t, s, "PushSubscription/set", `{"update":{"`+id+`":{"verificationCode":"wrong"}}}`)
	if got := setErrorType(res, "notUpdated", id); got != setInvalidProperties {
		t.Errorf("a wrong code gave %q", got)
	}

	mailCall(t, s, "PushSubscription/set", `{"update":{"`+id+`":{"verificationCode":"`+code+`","expires":"2999-01-01T00:00:00Z"}}}`)

	checkVerified(t, s, code)

	if got := mailError(t, s, "PushSubscription/get", `{"properties":["url"]}`); got != errForbidden {
		t.Errorf("asking for url gave %s", got)
	}

	res = mailCall(t, s, "PushSubscription/set", `{"destroy":["`+id+`"]}`)
	if d, _ := res["destroyed"].([]any); len(d) != 1 {
		t.Errorf("destroy gave %#v", res)
	}

	if _, err := kv.Get(pushAcctKey("test")); err == nil {
		t.Error("the record is still there")
	}
}

// storedCode is the verification code the admin store holds.
func storedCode(t *testing.T, kv model.AdminKV, id string) string {
	t.Helper()

	pa, err := loadPushAccount(kv, "test")
	if err != nil || pa.find(id) == nil {
		t.Fatalf("no record: %v", err)
	}

	return pa.find(id).Code
}

// checkVerified checks the one subscription as get returns it.
func checkVerified(t *testing.T, s *Server, code string) {
	t.Helper()

	got := mailCall(t, s, "PushSubscription/get", `{"ids":null}`)
	list, _ := got["list"].([]any)

	if len(list) != 1 {
		t.Fatalf("get gave %#v", got)
	}

	obj := list[0].(map[string]any)
	if obj[propVerificationCode] != code || obj[propURL] != nil || obj[propPushKeys] != nil {
		t.Errorf("get gave %#v", obj)
	}

	if exp, _ := time.Parse(time.RFC3339, obj[propExpires].(string)); exp.After(time.Now().Add(8 * 24 * time.Hour)) {
		t.Errorf("expires %v is not bounded", exp)
	}
}

// TestPushSubscriptionRefuses: what a creation may not be.
func TestPushSubscriptionRefuses(t *testing.T) {
	s, _, _, _ := pushServer(t)

	for _, c := range []string{
		`{"url":"https://push.example.net/"}`,
		`{"deviceClientId":"d","url":"http://push.example.net/"}`,
		`{"deviceClientId":"d","url":"https://127.0.0.1/"}`,
		`{"deviceClientId":"d","url":"https://localhost/"}`,
		`{"deviceClientId":"d","url":"https://push.example.net:8443/"}`,
		`{"deviceClientId":"d","url":"https://push.example.net/","verificationCode":"x"}`,
		`{"deviceClientId":"d","url":"https://push.example.net/","keys":{"p256dh":"AAAA","auth":"AAAA"}}`,
		`{"deviceClientId":"d","url":"https://push.example.net/","id":"x"}`,
	} {
		res := mailCall(t, s, "PushSubscription/set", `{"create":{"p":`+c+`}}`)
		if got := setErrorType(res, "notCreated", "p"); got != setInvalidProperties {
			t.Errorf("%s gave %q", c, got)
		}
	}

	plain := newTestServer(t)
	if got := mailError(t, plain, "PushSubscription/get", `{}`); got != errUnknownMethod {
		t.Errorf("without push configured get gave %s", got)
	}
}

// fakePushService records what it is sent.
type fakePushService struct {
	mu     sync.Mutex
	bodies []string
	status int
}

func (f *fakePushService) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body)

	f.mu.Lock()
	defer f.mu.Unlock()

	f.bodies = append(f.bodies, string(b))
	w.WriteHeader(f.status)
}

func (f *fakePushService) got() []string {
	f.mu.Lock()
	defer f.mu.Unlock()

	return append([]string(nil), f.bodies...)
}

// waitFor polls until cond holds.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()

	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}

		time.Sleep(20 * time.Millisecond)
	}
}

// testSender is a sender that trusts the fake push service.
func testSender(t *testing.T) (*webpush.Sender, string) {
	t.Helper()

	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	der, _ := x509.MarshalPKCS8PrivateKey(key)

	v, err := webpush.LoadVAPID(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), "mailto:ops@example.org")
	if err != nil {
		t.Fatal(err)
	}

	client := webpush.NewClient(2*time.Second, true)
	client.Transport.(*http.Transport).TLSClientConfig = &tls.Config{InsecureSkipVerify: true} //nolint:gosec // test server

	return &webpush.Sender{Client: client, VAPID: v, TTL: time.Minute}, v.PublicKey()
}

// TestPushWorker: the worker sends the verification, then, once the
// subscription is verified, a StateChange when mail arrives; a
// subscription the push service calls gone is destroyed.
func TestPushWorker(t *testing.T) {
	fake := &fakePushService{status: http.StatusCreated}
	ps := httptest.NewTLSServer(fake)

	defer ps.Close()

	sender, vapid := testSender(t)
	s, u, kv, store := pushServer(t)
	s.o.Push.VAPIDPublicKey = vapid
	s.o.Push.AllowInternalURLs = true

	res := mailCall(t, s, "PushSubscription/set", `{"create":{"p":{"deviceClientId":"dev","url":"`+ps.URL+`/x"}}}`)
	id, _ := res["created"].(map[string]any)["p"].(map[string]any)[propID].(string)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})

	go func() {
		defer close(done)

		_ = RunPushWorker(ctx, WorkerOptions{Store: store, KV: kv, Sender: sender, Instance: "w1", VAPIDPublicKey: vapid,
			Scan: 50 * time.Millisecond, Verify: 20 * time.Millisecond, Poll: 50 * time.Millisecond, Coalesce: 10 * time.Millisecond})
	}()

	defer func() { cancel(); <-done }()

	waitFor(t, "the verification", func() bool { return len(fake.got()) == 1 })

	var v map[string]string
	if err := json.Unmarshal([]byte(fake.got()[0]), &v); err != nil || v["@type"] != "PushVerification" || v["pushSubscriptionId"] != id {
		t.Fatalf("verification %s", fake.got()[0])
	}

	mailCall(t, s, "PushSubscription/set", `{"update":{"`+id+`":{"verificationCode":"`+v["verificationCode"]+`"}}}`)

	time.Sleep(150 * time.Millisecond) // the worker picks the verified subscription up
	deliver(t, u, inboxName, plainMessage)

	waitFor(t, "the state change", func() bool { return len(fake.got()) >= 2 })

	if !strings.Contains(fake.got()[1], `"StateChange"`) || !strings.Contains(fake.got()[1], `"EmailDelivery"`) {
		t.Errorf("push %s", fake.got()[1])
	}

	fake.mu.Lock()
	fake.status = http.StatusGone
	fake.mu.Unlock()

	deliver(t, u, inboxName, "Subject: again\r\n\r\nx\r\n")

	waitFor(t, "the subscription to go", func() bool {
		pa, _ := loadPushAccount(kv, "test")

		return pa.find(id) == nil
	})
}

// fakeWatch is a watch whose events and errors a test sends.
type fakeWatch struct {
	events chan struct{}
	errs   chan error
	closed chan struct{}
}

func (w *fakeWatch) Events() <-chan struct{} { return w.events }
func (w *fakeWatch) Errors() <-chan error    { return w.errs }
func (w *fakeWatch) Close()                  { close(w.closed) }

// fakeWatcher hands out fake watches.
type fakeWatcher struct {
	model.User
	watches chan *fakeWatch
}

func (f *fakeWatcher) WatchAccount() (model.Watch, error) {
	w := &fakeWatch{events: make(chan struct{}), errs: make(chan error), closed: make(chan struct{})}
	f.watches <- w

	return w, nil
}

// TestAccountFeedRewatches: a watch that reports an error is closed, set
// up again, and the feed fires for what it may have missed.
func TestAccountFeedRewatches(t *testing.T) {
	fw := &fakeWatcher{watches: make(chan *fakeWatch, 2)}
	feed := newAccountFeed(fw)

	defer feed.Close()

	first := <-fw.watches
	first.events <- struct{}{}

	<-feed.Events()

	first.errs <- io.ErrUnexpectedEOF

	select {
	case <-first.closed:
	case <-time.After(2 * time.Second):
		t.Fatal("the broken watch was not closed")
	}

	<-feed.Events()

	select {
	case <-fw.watches:
	case <-time.After(3 * time.Second):
		t.Fatal("no new watch")
	}
}

// denyAll denies every account, as a worker pointed at the wrong
// backend would.
type denyAll struct{ auth.Static }

func (denyAll) Lookup(context.Context, string, string, net.Addr) (*auth.Result, error) {
	return nil, auth.ErrInvalid
}

// TestAccountCheckSuspectsTheBackend: when most accounts are denied, none
// is dropped; a single denied account is dropped only after three denials.
func TestAccountCheckSuspectsTheBackend(t *testing.T) {
	w := &worker{o: workerDefaults(WorkerOptions{Auth: denyAll{}, AccountCheck: time.Nanosecond}),
		checked: map[string]*accountCheck{}}

	many := map[string][]*pushRecord{"a": nil, "b": nil, "c": nil, "d": nil}
	for range 5 {
		if gone := w.goneAccounts(context.Background(), many); len(gone) != 0 {
			t.Fatalf("dropped %v although every account was denied", gone)
		}
	}

	one := map[string][]*pushRecord{"x": nil}
	for i := 1; i <= deniedToDrop; i++ {
		gone := w.goneAccounts(context.Background(), one)
		if (i < deniedToDrop) != (len(gone) == 0) {
			t.Fatalf("denial %d gave %v", i, gone)
		}
	}
}

// TestPushWorkerKeepsWhatItCouldNotSend: a push service that answers 503
// gets the change later, not never.
func TestPushWorkerKeepsWhatItCouldNotSend(t *testing.T) {
	fake := &fakePushService{status: http.StatusCreated}
	ps := httptest.NewTLSServer(fake)

	defer ps.Close()

	sender, vapid := testSender(t)
	s, u, kv, store := pushServer(t)
	s.o.Push.VAPIDPublicKey, s.o.Push.AllowInternalURLs = vapid, true

	res := mailCall(t, s, "PushSubscription/set", `{"create":{"p":{"deviceClientId":"dev","url":"`+ps.URL+`/x"}}}`)
	id, _ := res["created"].(map[string]any)["p"].(map[string]any)[propID].(string)
	mailCall(t, s, "PushSubscription/set", `{"update":{"`+id+`":{"verificationCode":"`+storedCode(t, kv, id)+`"}}}`)

	fake.mu.Lock()
	fake.status = http.StatusServiceUnavailable
	fake.mu.Unlock()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})

	go func() {
		defer close(done)

		_ = RunPushWorker(ctx, WorkerOptions{Store: store, KV: kv, Sender: sender, Instance: "w1", VAPIDPublicKey: vapid,
			Scan: 50 * time.Millisecond, Verify: 20 * time.Millisecond, Poll: 50 * time.Millisecond, Coalesce: 10 * time.Millisecond})
	}()

	defer func() { cancel(); <-done }()

	time.Sleep(150 * time.Millisecond)
	deliver(t, u, inboxName, plainMessage)

	waitFor(t, "the refused push", func() bool { return len(fake.got()) >= 1 })

	fake.mu.Lock()
	fake.status = http.StatusCreated
	n := len(fake.bodies)
	fake.mu.Unlock()

	waitFor(t, "the push after the wait", func() bool {
		got := fake.got()

		return len(got) > n && strings.Contains(got[len(got)-1], "EmailDelivery")
	})
}

// TestPushWorkerPrunes: the worker forgets the backoff of subscriptions and
// the checks of accounts a scan no longer finds.
func TestPushWorkerPrunes(t *testing.T) {
	w := &worker{backoff: map[string]time.Time{"kept": {}, "gone": {}},
		checked: map[string]*accountCheck{"kept@example.org": {}, "gone@example.org": {}}}

	w.prune(map[string]bool{"kept": true}, map[string][]*pushRecord{"kept@example.org": nil})

	if _, ok := w.backoff["gone"]; ok || len(w.backoff) != 1 {
		t.Fatalf("backoff after prune: %v", w.backoff)
	}

	if _, ok := w.checked["gone@example.org"]; ok || len(w.checked) != 1 {
		t.Fatalf("checks after prune: %v", w.checked)
	}
}

// TestVerifyKeepsTheQueue: the verification queue is emptied only of what
// a round handled, after it handled it; what the front-ends queue in the
// meantime stays, and a round that dies before its update leaves the
// queue whole.
func TestVerifyKeepsTheQueue(t *testing.T) {
	kv := memstore.NewAdminKV()

	var sent atomic.Int32

	ps := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		sent.Add(1)
		// A front-end queues another verification while the worker works.
		_ = queueVerify(kv, "test", "later")

		w.WriteHeader(http.StatusCreated)
	}))
	defer ps.Close()

	sender, _ := testSender(t)

	if err := updatePushAccount(kv, "test", func(pa *pushAccount) error {
		pa.Account = "test"
		pa.Subs = append(pa.Subs, &pushRecord{ID: "first", Account: "test", URL: ps.URL + "/x", Code: "c",
			Expires: time.Now().Add(time.Hour)})

		return nil
	}); err != nil {
		t.Fatal(err)
	}

	if err := queueVerify(kv, "test", "first"); err != nil {
		t.Fatal(err)
	}

	w := &worker{o: workerDefaults(WorkerOptions{KV: kv, Sender: sender}), backoff: map[string]time.Time{}}
	w.verify(context.Background())

	raw, _ := kv.Get(pushVerifyQueue)

	var q []verifyItem
	_ = json.Unmarshal(raw, &q)

	if sent.Load() != 1 || len(q) != 1 || q[0].ID != "later" {
		t.Fatalf("sent %d, queue %+v; want one sent and the later one left", sent.Load(), q)
	}

	if got := withoutItems([]verifyItem{{ID: "a"}, {ID: "a"}, {ID: "b"}}, []verifyItem{{ID: "a"}}); len(got) != 2 {
		t.Errorf("withoutItems took more than it handled: %+v", got)
	}
}
