package jmap

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	"shardpost/internal/memstore"
	"shardpost/internal/model"
	"shardpost/internal/telemetry"
)

// contextStore hands out views that remember the span context the
// front-end set on them, which is what a real store hangs its spans on.
type contextStore struct {
	inner *memstore.Store
	mu    sync.Mutex
	seen  []trace.SpanContext
}

func (s *contextStore) User(name string) (model.User, error) {
	u, err := s.inner.User(name)
	if err != nil {
		return nil, err
	}

	return &contextUser{User: u, Index: u.(model.Index), Blobs: u.(model.Blobs), s: s}, nil
}

type contextUser struct {
	model.User
	model.Index
	model.Blobs
	s *contextStore
}

func (u *contextUser) UseContext(ctx context.Context) {
	u.s.mu.Lock()
	defer u.s.mu.Unlock()

	if sc := trace.SpanContextFromContext(ctx); sc.IsValid() {
		u.s.seen = append(u.s.seen, sc)
	}
}

// recordSpans points the tracer at a recorder for the test.
func recordSpans(t *testing.T) *tracetest.SpanRecorder {
	t.Helper()

	rec := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec))
	old := otel.GetTracerProvider()
	telemetry.Tracer = tp.Tracer("shardpost")

	t.Cleanup(func() { telemetry.Tracer = old.Tracer("shardpost") })

	return rec
}

// TestTracing: a span per request and per method call, the store pointed
// at the call's span, and no credentials or filter values in the
// attributes.
func TestTracing(t *testing.T) {
	rec := recordSpans(t)

	inner := memstore.New()
	store := &contextStore{inner: inner}
	s := newTestServer(t, func(o *Options) { o.Store = store })

	u, _ := inner.User("test")
	if err := u.CreateMailbox(inboxName, nil); err != nil {
		t.Fatal(err)
	}

	call(t, s, `{"using":["urn:ietf:params:jmap:core","urn:ietf:params:jmap:mail"],"methodCalls":[
		["Email/query",{"accountId":"test","filter":{"text":"secretword"}},"a"],
		["Mailbox/get",{"accountId":"nobody"},"b"]]}`)

	spans := spansByName(t, rec.Ended(), "jmap.api", "jmap.Email/query", "jmap.Mailbox/get")
	api, query, get := spans[0], spans[1], spans[2]

	if query.Parent().SpanID() != api.SpanContext().SpanID() || get.Parent().SpanID() != api.SpanContext().SpanID() {
		t.Error("a method span is not a child of the request span")
	}

	if attr(api, attrUser) != "test" || attr(api, "http.status_code") != "200" || attr(query, attrAcct) != "test" {
		t.Errorf("attributes: %v / %v", api.Attributes(), query.Attributes())
	}

	if attr(get, attrError) != errAccountNotFound || get.Status().Code == codes.Error {
		t.Errorf("a client error is traced as %v, %v", get.Attributes(), get.Status())
	}

	noSecrets(t, rec.Ended())

	if !store.saw(query.SpanContext()) {
		t.Error("the store never saw the span of the method call")
	}
}

// spansByName finds the spans of the given names, failing the test when
// one is missing.
func spansByName(t *testing.T, ended []sdktrace.ReadOnlySpan, want ...string) []sdktrace.ReadOnlySpan {
	t.Helper()

	byName := map[string]sdktrace.ReadOnlySpan{}
	for _, sp := range ended {
		byName[sp.Name()] = sp
	}

	out := make([]sdktrace.ReadOnlySpan, 0, len(want))

	for _, n := range want {
		sp, ok := byName[n]
		if !ok {
			t.Fatalf("no span %s among %v", n, names(ended))
		}

		out = append(out, sp)
	}

	return out
}

// saw reports whether a view was pointed at the span.
func (s *contextStore) saw(sc trace.SpanContext) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	for _, got := range s.seen {
		if got.SpanID() == sc.SpanID() {
			return true
		}
	}

	return false
}

// noSecrets fails the test when an attribute carries the password or
// the search word of the test request.
func noSecrets(t *testing.T, spans []sdktrace.ReadOnlySpan) {
	t.Helper()

	for _, sp := range spans {
		for _, kv := range sp.Attributes() {
			if v := kv.Value.String(); strings.Contains(v, "secret") {
				t.Errorf("span %s carries %s=%s", sp.Name(), kv.Key, v)
			}
		}
	}
}

// TestTracingRefused: a request that does not authenticate is traced
// with its status, and without an account.
func TestTracingRefused(t *testing.T) {
	rec := recordSpans(t)
	s := newTestServer(t)

	w := httptest.NewRecorder()
	s.ServeHTTP(w, httptest.NewRequest(http.MethodPost, PathAPI, strings.NewReader(`{}`)))

	if len(rec.Ended()) != 1 || attr(rec.Ended()[0], "http.status_code") != "401" || attr(rec.Ended()[0], attrUser) != "" {
		t.Errorf("refused request: %v (status %d)", names(rec.Ended()), w.Code)
	}
}

// names lists span names.
func names(spans []sdktrace.ReadOnlySpan) []string {
	out := make([]string, 0, len(spans))
	for _, sp := range spans {
		out = append(out, sp.Name())
	}

	return out
}

// attr is the value of one span attribute, "" without it.
func attr(sp sdktrace.ReadOnlySpan, key string) string {
	for _, kv := range sp.Attributes() {
		if string(kv.Key) == key {
			return kv.Value.String()
		}
	}

	return ""
}
