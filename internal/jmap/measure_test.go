package jmap

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"

	"shardpost/internal/auth"
	"shardpost/internal/memstore"
	"shardpost/internal/metrics"
	"shardpost/internal/model"
)

// The request metrics are process-wide, so every test reads them before
// and after and checks the difference.

// value is the value of a counter or a gauge.
func value(t *testing.T, c prometheus.Metric) float64 {
	t.Helper()

	var m dto.Metric
	if err := c.Write(&m); err != nil {
		t.Fatal(err)
	}

	if m.Counter != nil {
		return m.Counter.GetValue()
	}

	return m.Gauge.GetValue()
}

// observations are the count and the sum of a histogram.
func observations(t *testing.T, o prometheus.Observer) (uint64, float64) {
	t.Helper()

	var m dto.Metric
	if err := o.(prometheus.Metric).Write(&m); err != nil {
		t.Fatal(err)
	}

	return m.Histogram.GetSampleCount(), m.Histogram.GetSampleSum()
}

// probe remembers the value of a series and checks how far it moved.
type probe struct {
	t      *testing.T
	name   string
	read   func() float64
	before float64
}

func watch(t *testing.T, name string, read func() float64) *probe {
	t.Helper()

	return &probe{t: t, name: name, read: read, before: read()}
}

func (p *probe) moved(by float64) {
	p.t.Helper()

	if got := p.read() - p.before; got != by {
		p.t.Errorf("%s moved by %v, want %v", p.name, got, by)
	}
}

func httpRequests(t *testing.T, endpoint, result string) *probe {
	t.Helper()

	return watch(t, "http_requests{"+endpoint+","+result+"}", func() float64 {
		return value(t, metrics.JMAPRequests.WithLabelValues(endpoint, result))
	})
}

func methodCalls(t *testing.T, method, result string) *probe {
	t.Helper()

	return watch(t, "method_calls{"+method+","+result+"}", func() float64 {
		return value(t, metrics.JMAPMethodCalls.WithLabelValues(method, result))
	})
}

func setObjects(t *testing.T, method, outcome string) *probe {
	t.Helper()

	return watch(t, "set_objects{"+method+","+outcome+"}", func() float64 {
		return value(t, metrics.JMAPSetObjects.WithLabelValues(method, outcome))
	})
}

func inflight(t *testing.T, endpoint string) *probe {
	t.Helper()

	return watch(t, "inflight{"+endpoint+"}", func() float64 {
		return value(t, metrics.JMAPInflight.WithLabelValues(endpoint))
	})
}

func httpSeconds(t *testing.T, endpoint string) *probe {
	t.Helper()

	return watch(t, "http_request_seconds{"+endpoint+"}", func() float64 {
		n, _ := observations(t, metrics.JMAPRequestSeconds.WithLabelValues(endpoint))

		return float64(n)
	})
}

func methodSeconds(t *testing.T, method string) *probe {
	t.Helper()

	return watch(t, "method_seconds{"+method+"}", func() float64 {
		n, _ := observations(t, metrics.JMAPMethodSeconds.WithLabelValues(method))

		return float64(n)
	})
}

const usingMail = `"using":["urn:ietf:params:jmap:core","urn:ietf:params:jmap:mail"]`

// TestMetricsBatch: a batch is one HTTP request and as many method
// calls as it has; an error response inside the 200 is a result of its
// call, not of the request, and an unknown name has one fixed label.
func TestMetricsBatch(t *testing.T) {
	s := newTestServer(t)

	requests := httpRequests(t, metrics.JMAPEndpointAPI, metrics.JMAPResultOK)
	seconds := httpSeconds(t, metrics.JMAPEndpointAPI)
	flying := inflight(t, metrics.JMAPEndpointAPI)
	echo := methodCalls(t, "Core/echo", metrics.JMAPResultOK)
	echoSeconds := methodSeconds(t, "Core/echo")
	unknown := methodCalls(t, metrics.JMAPMethodUnknown, metrics.JMAPResultClientError)
	unknownSeconds := methodSeconds(t, metrics.JMAPMethodUnknown)
	refused := methodCalls(t, "Mailbox/get", metrics.JMAPResultClientError)
	unused := methodCalls(t, "Quota/get", metrics.JMAPResultClientError)
	in := watch(t, "request_bytes{api}", func() float64 {
		return value(t, metrics.JMAPRequestBytes.WithLabelValues(metrics.JMAPEndpointAPI))
	})
	out := watch(t, "response_bytes{api}", func() float64 {
		return value(t, metrics.JMAPResponseBytes.WithLabelValues(metrics.JMAPEndpointAPI))
	})
	batches, calls := observations(t, metrics.JMAPRequestCalls)

	// Quota/get is a method of the server, but the request does not use
	// its capability: unknownMethod under the method's own label.
	body := `{` + usingMail + `,"methodCalls":[
		["Core/echo",{"a":1},"c1"],
		["Core/echo",{"a":2},"c2"],
		["Nope/get",{},"c3"],
		["Mailbox/get",{"accountId":"someone-else"},"c4"],
		["Quota/get",{"accountId":"test"},"c5"]]}`

	w := do(t, s, http.MethodPost, PathAPI, body)
	if w.Code != http.StatusOK {
		t.Fatalf("api answered %d: %s", w.Code, w.Body.String())
	}

	requests.moved(1)
	seconds.moved(1)
	flying.moved(0)
	echo.moved(2)
	echoSeconds.moved(2)
	unknown.moved(1)
	unknownSeconds.moved(1)
	refused.moved(1)
	unused.moved(1)
	in.moved(float64(len(body)))
	out.moved(float64(w.Body.Len()))

	if b, c := observations(t, metrics.JMAPRequestCalls); b-batches != 1 || c-calls != 5 {
		t.Errorf("request_method_calls moved by %d requests and %v calls, want 1 and 5", b-batches, c-calls)
	}
}

// TestMetricsSetPartialFailure: the objects a /set could not create or
// destroy count as not_created and not_destroyed of a call that itself
// is ok; a /set that fails as a whole counts as a failed call and no
// object.
func TestMetricsSetPartialFailure(t *testing.T) {
	s := newTestServer(t)

	ok := methodCalls(t, "Mailbox/set", metrics.JMAPResultOK)
	failed := methodCalls(t, "Mailbox/set", metrics.JMAPResultClientError)
	created := setObjects(t, "Mailbox/set", metrics.JMAPSetCreated)
	notCreated := setObjects(t, "Mailbox/set", metrics.JMAPSetNotCreated)
	notUpdated := setObjects(t, "Mailbox/set", metrics.JMAPSetNotUpdated)
	notDestroyed := setObjects(t, "Mailbox/set", metrics.JMAPSetNotDestroyed)
	destroyed := setObjects(t, "Mailbox/set", metrics.JMAPSetDestroyed)

	res := mailCall(t, s, "Mailbox/set", `{"accountId":"test",
		"create":{"a":{"name":"Projects"},"b":{"name":""},"c":{"name":"x","nope":1}},
		"update":{"no-such-mailbox":{"name":"y"}},
		"destroy":["no-such-mailbox","nor-this-one"]}`)

	if len(res["created"].(map[string]any)) != 1 || len(res["notCreated"].(map[string]any)) != 2 {
		t.Fatalf("the test's premise does not hold: %#v", res)
	}

	ok.moved(1)
	failed.moved(0)
	created.moved(1)
	notCreated.moved(2)
	notUpdated.moved(1)
	notDestroyed.moved(2)
	destroyed.moved(0)

	out := call(t, s, `{`+usingMail+`,"methodCalls":[["Mailbox/set",
		{"accountId":"test","ifInState":"not-the-state","create":{"a":{"name":"Other"}}},"c1"]]}`)
	if got := responses(t, out)[0].([]any); got[0] != kwError {
		t.Fatalf("a stale ifInState was accepted: %#v", got)
	}

	ok.moved(1)
	failed.moved(1)
	created.moved(1)
	notCreated.moved(2)
}

// TestMetricsImplicitCall: the Email/set an EmailSubmission/set makes is
// an implicit call, not a call of the client; the objects it changes
// count under Email/set once.
func TestMetricsImplicitCall(t *testing.T) {
	s, u, _ := submissionServer(t)
	m := deliver(t, u, "Drafts", draftMessage, `\Draft`)
	boxes := boxIDs(t, u)
	ident := identityID(t, s)

	submission := methodCalls(t, "EmailSubmission/set", metrics.JMAPResultOK)
	explicit := methodCalls(t, methodEmailSet, metrics.JMAPResultOK)
	implicit := watch(t, "implicit_calls{Email/set,ok}", func() float64 {
		return value(t, metrics.JMAPImplicitCalls.WithLabelValues(methodEmailSet, metrics.JMAPResultOK))
	})
	sent := setObjects(t, "EmailSubmission/set", metrics.JMAPSetCreated)
	moved := setObjects(t, methodEmailSet, metrics.JMAPSetUpdated)
	seconds := methodSeconds(t, methodEmailSet)

	out := call(t, s, `{"using":["urn:ietf:params:jmap:core","urn:ietf:params:jmap:mail","urn:ietf:params:jmap:submission"],
		"methodCalls":[["EmailSubmission/set",{"accountId":"test","create":{"k":{"identityId":"`+ident+`","emailId":"`+m.GUID+`"}},
		"onSuccessUpdateEmail":{"#k":{"mailboxIds/`+boxes["Drafts"]+`":null,"mailboxIds/`+boxes["Sent"]+`":true,"keywords/$draft":null}}},"c1"]]}`)

	if rs := responses(t, out); len(rs) != 2 || rs[1].([]any)[0] != methodEmailSet {
		t.Fatalf("responses %#v", rs)
	}

	submission.moved(1)
	explicit.moved(0)
	implicit.moved(1)
	sent.moved(1)
	moved.moved(1)
	seconds.moved(0)
}

// TestMetricsRefusedEarly: a request refused before its handler runs is
// counted by why: no or wrong credentials, a backend that cannot
// decide, a body that is no request, a path that is no route. The
// health check is not counted at all.
func TestMetricsRefusedEarly(t *testing.T) {
	s := newTestServer(t, func(o *Options) {
		o.Auth = flakyAuth{Static: auth.Static{"test": "secret"}}
	})

	unauthenticated := httpRequests(t, metrics.JMAPEndpointAPI, metrics.JMAPResultUnauthenticated)
	unavailable := httpRequests(t, metrics.JMAPEndpointSession, metrics.JMAPResultUnavailable)
	malformed := httpRequests(t, metrics.JMAPEndpointAPI, metrics.JMAPResultClientError)
	noRoute := httpRequests(t, metrics.JMAPEndpointOther, metrics.JMAPResultClientError)
	anyCall := methodCalls(t, "Core/echo", metrics.JMAPResultOK)
	flying := inflight(t, metrics.JMAPEndpointAPI)

	all := watch(t, "http_requests (all)", func() float64 { return allRequests(t) })

	echo := `{"using":["urn:ietf:params:jmap:core"],"methodCalls":[["Core/echo",{},"c1"]]}`

	for _, c := range []struct {
		target, user, pass, body string
		want                     int
	}{
		{PathAPI, "", "", echo, http.StatusUnauthorized},
		{PathAPI, "test", "wrong", echo, http.StatusUnauthorized},
		{PathSession, "down", "x", "", http.StatusServiceUnavailable},
		{PathAPI, "test", "secret", `{"using":`, http.StatusBadRequest},
		{"/jmap/nothing-here", "test", "secret", "", http.StatusNotFound},
		{PathHealth, "", "", "", http.StatusOK},
	} {
		if got := send(s, c.target, c.user, c.pass, c.body); got != c.want {
			t.Fatalf("%s answered %d, want %d", c.target, got, c.want)
		}
	}

	unauthenticated.moved(2)
	unavailable.moved(1)
	malformed.moved(1)
	noRoute.moved(1)
	anyCall.moved(0)
	flying.moved(0)
	all.moved(5)
}

// send sends a request (a POST when it has a body) with the given
// credentials, or none, and returns the status.
func send(s *Server, target, user, pass, body string) int {
	method := http.MethodPost
	if body == "" {
		method = http.MethodGet
	}

	r := httptest.NewRequest(method, target, strings.NewReader(body))
	if user != "" {
		r.SetBasicAuth(user, pass)
	}

	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)

	return w.Code
}

// allRequests is the sum of the request counter over every endpoint
// and result.
func allRequests(t *testing.T) float64 {
	t.Helper()

	sum := 0.0

	for _, ep := range metrics.JMAPEndpoints {
		for _, res := range metrics.JMAPHTTPResults {
			sum += value(t, metrics.JMAPRequests.WithLabelValues(ep, res))
		}
	}

	return sum
}

// flakyAuth cannot decide about the user "down".
type flakyAuth struct{ auth.Static }

func (a flakyAuth) Authenticate(ctx context.Context, req auth.Request) (*auth.Result, error) {
	if req.Username == "down" {
		return nil, fmt.Errorf("backend away: %w", auth.ErrTemporary)
	}

	return a.Static.Authenticate(ctx, req)
}

// TestMetricsCanceled: a request whose client went away is canceled,
// whatever was written for it, and leaves nothing in flight.
func TestMetricsCanceled(t *testing.T) {
	s := newTestServer(t)

	canceled := httpRequests(t, metrics.JMAPEndpointAPI, metrics.JMAPResultCanceled)
	ok := httpRequests(t, metrics.JMAPEndpointAPI, metrics.JMAPResultOK)
	flying := inflight(t, metrics.JMAPEndpointAPI)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	r := httptest.NewRequest(http.MethodPost, PathAPI,
		strings.NewReader(`{"using":["urn:ietf:params:jmap:core"],"methodCalls":[["Core/echo",{},"c1"]]}`)).WithContext(ctx)
	r.SetBasicAuth("test", "secret")
	s.ServeHTTP(httptest.NewRecorder(), r)

	canceled.moved(1)
	ok.moved(0)
	flying.moved(0)
}

// panickyAuth panics instead of answering.
type panickyAuth struct{ auth.Static }

func (panickyAuth) Authenticate(context.Context, auth.Request) (*auth.Result, error) {
	panic("the backend client is broken")
}

// TestMetricsPanic: a handler that panics is a server_error, leaves
// nothing in flight, and the panic still reaches net/http.
func TestMetricsPanic(t *testing.T) {
	s := newTestServer(t, func(o *Options) { o.Auth = panickyAuth{} })

	failed := httpRequests(t, metrics.JMAPEndpointSession, metrics.JMAPResultServerError)
	seconds := httpSeconds(t, metrics.JMAPEndpointSession)
	flying := inflight(t, metrics.JMAPEndpointSession)

	func() {
		defer func() {
			if recover() == nil {
				t.Error("the panic did not pass through the measuring")
			}
		}()

		send(s, PathSession, "test", "secret", "")
	}()

	failed.moved(1)
	seconds.moved(1)
	flying.moved(0)
}

// TestMetricsRedirect: the router's redirect of a path that lacks its
// slash is not a request of the endpoint.
func TestMetricsRedirect(t *testing.T) {
	s := newTestServer(t)

	api := httpRequests(t, metrics.JMAPEndpointAPI, metrics.JMAPResultOK)
	other := httpRequests(t, metrics.JMAPEndpointOther, metrics.JMAPResultOK)
	flying := inflight(t, metrics.JMAPEndpointAPI)

	if got := send(s, strings.TrimSuffix(PathAPI, "/"), "test", "secret", "{}"); got != http.StatusTemporaryRedirect &&
		got != http.StatusPermanentRedirect && got != http.StatusMovedPermanently {
		t.Skipf("the router answered %d, not a redirect", got)
	}

	api.moved(0)
	other.moved(1)
	flying.moved(0)
}

// failingUser is an account whose state cannot be read once broken is
// set.
type failingUser struct {
	model.User
	broken *atomic.Bool
}

func (f failingUser) AccountState() (uint64, error) {
	if f.broken.Load() {
		return 0, errors.New("the index pool is away")
	}

	return f.User.AccountState()
}

// failingStore hands out failing accounts.
type failingStore struct {
	*memstore.Store
	broken *atomic.Bool
}

func (f failingStore) User(name string) (model.User, error) {
	u, err := f.Store.User(name)
	if err != nil {
		return nil, err
	}

	return failingUser{User: u, broken: f.broken}, nil
}

// TestMetricsServerFailure: a store that fails is a server_error of the
// method inside a request that is ok, and an unavailable event source.
func TestMetricsServerFailure(t *testing.T) {
	broken := &atomic.Bool{}
	s := newTestServer(t, func(o *Options) {
		o.Store = failingStore{Store: memstore.New(), broken: broken}
	})

	broken.Store(true)

	failed := methodCalls(t, "Mailbox/get", metrics.JMAPResultServerError)
	clientError := methodCalls(t, "Mailbox/get", metrics.JMAPResultClientError)
	request := httpRequests(t, metrics.JMAPEndpointAPI, metrics.JMAPResultOK)
	stream := httpRequests(t, metrics.JMAPEndpointEvents, metrics.JMAPResultUnavailable)
	streams := watch(t, "event_streams", func() float64 { return value(t, metrics.JMAPEventStreams) })

	out := call(t, s, `{`+usingMail+`,"methodCalls":[["Mailbox/get",{"accountId":"test"},"c1"]]}`)

	got := responses(t, out)[0].([]any)
	if got[0] != kwError || got[1].(map[string]any)["type"] != errServerFail {
		t.Fatalf("the failing store answered %#v", got)
	}

	if w := do(t, s, http.MethodGet, PathEvents+"?types=*&closeafter=no&ping=0", ""); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("the event source answered %d", w.Code)
	}

	failed.moved(1)
	clientError.moved(0)
	request.moved(1)
	stream.moved(1)
	streams.moved(0)
}

// TestMetricsBlobVolume: what the upload endpoint reads and the
// download endpoint writes is counted in bytes; an upload refused for
// its size is a client error whose bytes were read all the same.
func TestMetricsBlobVolume(t *testing.T) {
	s := newTestServer(t, func(o *Options) { o.Limits.MaxSizeUpload = 1 << 10 })

	uploaded := watch(t, "request_bytes{upload}", func() float64 {
		return value(t, metrics.JMAPRequestBytes.WithLabelValues(metrics.JMAPEndpointUpload))
	})
	downloaded := watch(t, "response_bytes{download}", func() float64 {
		return value(t, metrics.JMAPResponseBytes.WithLabelValues(metrics.JMAPEndpointDownload))
	})
	uploads := httpRequests(t, metrics.JMAPEndpointUpload, metrics.JMAPResultOK)
	tooLarge := httpRequests(t, metrics.JMAPEndpointUpload, metrics.JMAPResultClientError)
	downloads := httpRequests(t, metrics.JMAPEndpointDownload, metrics.JMAPResultOK)

	payload := strings.Repeat("blob", 100)
	id := upload(t, s, payload, "application/octet-stream")

	uploaded.moved(float64(len(payload)))
	uploads.moved(1)

	w := do(t, s, http.MethodGet, PathDownload+"test/"+id+"/file.bin", "")
	if w.Code != http.StatusOK || w.Body.String() != payload {
		t.Fatalf("the download answered %d with %d bytes", w.Code, w.Body.Len())
	}

	downloaded.moved(float64(len(payload)))
	downloads.moved(1)

	if w := do(t, s, http.MethodPost, PathUpload+"test/", strings.Repeat("x", 2<<10)); w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("the oversized upload answered %d", w.Code)
	}

	tooLarge.moved(1)
	// The handler stops reading one byte past the limit.
	uploaded.moved(float64(len(payload) + 1<<10 + 1))
}

// TestMetricsEventStream: an event source is counted when its stream is
// established and is an open stream from then on, not a request in
// flight; when it ends the gauge returns and its lifetime is observed.
// One beyond the account's limit is limited.
func TestMetricsEventStream(t *testing.T) {
	s, u, _, _ := pushServer(t)
	hs := httptest.NewServer(s)

	defer hs.Close()
	defer s.Close()

	established := httpRequests(t, metrics.JMAPEndpointEvents, metrics.JMAPResultOK)
	limited := httpRequests(t, metrics.JMAPEndpointEvents, metrics.JMAPResultLimited)
	streams := watch(t, "event_streams", func() float64 { return value(t, metrics.JMAPEventStreams) })
	lifetimes := watch(t, "event_stream_seconds", func() float64 {
		n, _ := observations(t, metrics.JMAPEventStreamSeconds)

		return float64(n)
	})
	sent := watch(t, "response_bytes{eventsource}", func() float64 {
		return value(t, metrics.JMAPResponseBytes.WithLabelValues(metrics.JMAPEndpointEvents))
	})

	open := make([]*http.Response, 0, DefaultLimits.MaxEventSources)

	for range DefaultLimits.MaxEventSources {
		resp, _ := openEvents(t, hs.URL, "types=*&closeafter=no&ping=0", "")
		open = append(open, resp)
	}

	// The streams are open: counted already, although no handler returned.
	established.moved(float64(len(open)))
	streams.moved(float64(len(open)))
	lifetimes.moved(0)

	// What a stream sends is counted as it passes, not when it ends.
	deliver(t, u, inboxName, plainMessage)
	waitUntil(t, func() bool { return sent.read() > sent.before })

	req, _ := http.NewRequest(http.MethodGet, hs.URL+PathEvents+"?types=*&closeafter=no&ping=0", nil)
	req.SetBasicAuth("test", "secret")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}

	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()

	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("the event source beyond the limit answered %d", resp.StatusCode)
	}

	limited.moved(1)
	streams.moved(float64(len(open)))

	for _, resp := range open {
		_ = resp.Body.Close()
	}

	waitUntil(t, func() bool { return streams.read() == streams.before })

	established.moved(float64(len(open)))
	lifetimes.moved(float64(len(open)))
}

// TestMetricsParallel: requests in parallel are each counted once and
// leave nothing in flight.
func TestMetricsParallel(t *testing.T) {
	s := newTestServer(t, func(o *Options) { o.Limits.MaxConcurrentRequests = 64 })

	const clients, rounds = 16, 20

	requests := httpRequests(t, metrics.JMAPEndpointAPI, metrics.JMAPResultOK)
	calls := methodCalls(t, "Core/echo", metrics.JMAPResultOK)
	seconds := methodSeconds(t, "Core/echo")
	flying := inflight(t, metrics.JMAPEndpointAPI)

	var wg sync.WaitGroup

	for range clients {
		wg.Go(func() {
			for range rounds {
				r := httptest.NewRequest(http.MethodPost, PathAPI, strings.NewReader(
					`{"using":["urn:ietf:params:jmap:core"],"methodCalls":[["Core/echo",{},"a"],["Core/echo",{},"b"]]}`))
				r.SetBasicAuth("test", "secret")

				w := httptest.NewRecorder()
				s.ServeHTTP(w, r)

				if w.Code != http.StatusOK {
					t.Errorf("api answered %d", w.Code)
				}
			}
		})
	}

	wg.Wait()

	requests.moved(clients * rounds)
	calls.moved(2 * clients * rounds)
	seconds.moved(2 * clients * rounds)
	flying.moved(0)
}

// series is the number of series a collector exposes.
func series(c prometheus.Collector) int {
	ch := make(chan prometheus.Metric, 4096)

	go func() {
		c.Collect(ch)
		close(ch)
	}()

	n := 0
	for range ch {
		n++
	}

	return n
}

// TestMetricsCardinality: the series exist before the first request and
// their number does not depend on what clients send: a name, a path or
// an account of the client's never becomes a label.
func TestMetricsCardinality(t *testing.T) {
	s := newTestServer(t)

	vectors := map[string]prometheus.Collector{
		"http_requests": metrics.JMAPRequests, "http_request_seconds": metrics.JMAPRequestSeconds,
		"http_inflight": metrics.JMAPInflight, "request_bytes": metrics.JMAPRequestBytes,
		"response_bytes": metrics.JMAPResponseBytes, "method_calls": metrics.JMAPMethodCalls,
		"method_seconds": metrics.JMAPMethodSeconds, "implicit_calls": metrics.JMAPImplicitCalls,
		"set_objects": metrics.JMAPSetObjects,
	}

	before := map[string]int{}
	for name, c := range vectors {
		before[name] = series(c)
	}

	want := map[string]int{
		"http_requests":        len(metrics.JMAPEndpoints) * len(metrics.JMAPHTTPResults),
		"http_request_seconds": len(metrics.JMAPEndpoints) - 1,
		"http_inflight":        len(metrics.JMAPEndpoints) - 1,
		"request_bytes":        2,
		"response_bytes":       len(metrics.JMAPEndpoints),
		"method_calls":         len(methods)*len(metrics.JMAPMethodResults) + 1,
		"method_seconds":       len(methods) + 1,
		"implicit_calls":       len(metrics.JMAPMethodResults),
	}

	// An earlier test of the package may have met a result that is not
	// created up front (an unknown method under a canceled request):
	// never more than every result of every label value.
	most := map[string]int{"method_calls": (len(methods) + 1) * len(metrics.JMAPMethodResults)}

	for name, n := range want {
		if before[name] < n || before[name] > max(n, most[name]) {
			t.Errorf("%s has %d series before the first request, want %d", name, before[name], n)
		}
	}

	for i := range 40 {
		name := fmt.Sprintf("Made/up%d", i)

		out := call(t, s, `{`+usingMail+`,"methodCalls":[["`+name+`",{},"c"],
			["Mailbox/set",{"accountId":"test","create":{"k`+name+`":{"name":"`+name+`"}}},"d"]]}`)
		if len(responses(t, out)) != 2 {
			t.Fatalf("responses %#v", out)
		}

		do(t, s, http.MethodGet, fmt.Sprintf("/jmap/made-up/%d", i), "")
		do(t, s, http.MethodGet, fmt.Sprintf("%stest/no-blob-%d/name%d", PathDownload, i, i), "")
	}

	for name, c := range vectors {
		if got := series(c); got != before[name] {
			t.Errorf("%s grew from %d to %d series", name, before[name], got)
		}
	}
}

// TestHTTPResult: the result classes of a request.
func TestHTTPResult(t *testing.T) {
	for _, c := range []struct {
		status             int
		canceled, panicked bool
		want               string
	}{
		{0, false, false, metrics.JMAPResultOK},
		{http.StatusCreated, false, false, metrics.JMAPResultOK},
		{http.StatusNotModified, false, false, metrics.JMAPResultOK},
		{http.StatusBadRequest, false, false, metrics.JMAPResultClientError},
		{http.StatusRequestEntityTooLarge, false, false, metrics.JMAPResultClientError},
		{http.StatusUnauthorized, false, false, metrics.JMAPResultUnauthenticated},
		{http.StatusForbidden, false, false, metrics.JMAPResultForbidden},
		{http.StatusTooManyRequests, false, false, metrics.JMAPResultLimited},
		{http.StatusServiceUnavailable, false, false, metrics.JMAPResultUnavailable},
		{http.StatusInternalServerError, false, false, metrics.JMAPResultServerError},
		{http.StatusUnauthorized, true, false, metrics.JMAPResultCanceled},
		{http.StatusOK, true, false, metrics.JMAPResultCanceled},
		{http.StatusOK, true, true, metrics.JMAPResultServerError},
	} {
		if got := httpResult(c.status, c.canceled, c.panicked); got != c.want {
			t.Errorf("status %d canceled %v panicked %v is %s, want %s", c.status, c.canceled, c.panicked, got, c.want)
		}
	}
}

// TestMethodResult: the result classes of a method call.
func TestMethodResult(t *testing.T) {
	gone, cancel := context.WithCancel(context.Background())
	cancel()

	for _, c := range []struct {
		ctx  context.Context
		merr *methodError
		want string
	}{
		{context.Background(), nil, metrics.JMAPResultOK},
		{gone, nil, metrics.JMAPResultOK},
		{context.Background(), &methodError{Type: errInvalidArguments}, metrics.JMAPResultClientError},
		{context.Background(), &methodError{Type: errStateMismatch}, metrics.JMAPResultClientError},
		{context.Background(), &methodError{Type: errServerFail}, metrics.JMAPResultServerError},
		{context.Background(), &methodError{Type: errServerPartialFail}, metrics.JMAPResultServerError},
		{context.Background(), &methodError{Type: errServerUnavailable}, metrics.JMAPResultUnavailable},
		{gone, &methodError{Type: errServerFail}, metrics.JMAPResultCanceled},
	} {
		if got := methodResult(c.ctx, c.merr); got != c.want {
			t.Errorf("%v is %s, want %s", c.merr, got, c.want)
		}
	}
}
