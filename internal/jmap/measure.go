package jmap

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"shardpost/internal/metrics"
)

// Request metrics. Every request but the health check is measured once,
// around the whole handler chain: authentication, the wait for a slot
// and the response are all inside. A batch is one request; its method
// calls are measured one by one in runCall. See docs/observability.md
// for what each series means.

// setLike are the methods whose response lists what became of each
// object: true for a /set (created, updated, destroyed and their
// failures), false for the ones that only create.
var setLike = func() map[string]bool {
	out := map[string]bool{methodEmailCopy: false, methodEmailImport: false, methodBlobUpload: false}

	for name := range methods {
		if strings.HasSuffix(name, "/set") {
			out[name] = true
		}
	}

	return out
}()

// implicitMethods are the methods another method calls on the client's
// behalf.
var implicitMethods = []string{methodEmailSet}

// initMetrics creates the series of the request metrics once per
// process, when the first front-end is built: a process that serves no
// JMAP exposes none of them.
var initMetrics = sync.OnceFunc(func() {
	names := make([]string, 0, len(methods))
	for name := range methods {
		names = append(names, name)
	}

	sort.Strings(names)
	metrics.InitJMAP(names, setLike, implicitMethods)
})

// endpointOf maps the pattern of a route to its endpoint category.
var endpointOf = map[string]string{
	"GET " + PathSession:                                  metrics.JMAPEndpointSession,
	"POST " + PathAPI:                                     metrics.JMAPEndpointAPI,
	"POST " + PathUpload + "{accountId}/":                 metrics.JMAPEndpointUpload,
	"GET " + PathDownload + "{accountId}/{blobId}/{name}": metrics.JMAPEndpointDownload,
	"GET " + PathEvents:                                   metrics.JMAPEndpointEvents,
}

// healthPattern is the route of the health check, which is not measured.
const healthPattern = "GET " + PathHealth

// measured is the response writer of a measured request. It remembers
// the status, counts the body bytes as they pass, and for an event
// source marks the moment the stream is established. One goroutine,
// the handler's, uses it.
type measured struct {
	http.ResponseWriter
	ctx      context.Context
	endpoint string
	start    time.Time
	out      prometheus.Counter
	status   int
	// stream is set once an event source has answered 200: from then on
	// it is an open stream, not a request in flight.
	stream bool
	// gone is set when the client was away by the time the answer was
	// written, or a write to it failed.
	gone bool
}

// countedBody counts the bytes read from a request body as they pass.
type countedBody struct {
	io.ReadCloser
	in prometheus.Counter
}

// Read implements io.Reader.
func (b *countedBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if n > 0 {
		b.in.Add(float64(n))
	}

	return n, err
}

// WriteHeader implements http.ResponseWriter. An informational status
// is not the answer.
func (m *measured) WriteHeader(code int) {
	if m.status == 0 && code >= http.StatusOK {
		m.answered(code)
	}

	m.ResponseWriter.WriteHeader(code)
}

// Write implements http.ResponseWriter.
func (m *measured) Write(b []byte) (int, error) {
	if m.status == 0 {
		m.answered(http.StatusOK)
	}

	n, err := m.ResponseWriter.Write(b)
	if n > 0 {
		m.out.Add(float64(n))
	}

	if err != nil {
		m.gone = true
	}

	return n, err
}

// Unwrap gives http.ResponseController the writer underneath.
func (m *measured) Unwrap() http.ResponseWriter { return m.ResponseWriter }

// answered records the status. An event source that answers 200 is
// established: it is counted now, since its handler returns hours later.
func (m *measured) answered(code int) {
	m.status = code
	m.gone = m.ctx.Err() != nil

	if m.endpoint == metrics.JMAPEndpointEvents && code == http.StatusOK {
		m.stream = true

		metrics.JMAPRequests.WithLabelValues(m.endpoint, metrics.JMAPResultOK).Inc()
		metrics.JMAPEventStreams.Inc()
	}
}

// canceled reports whether the client went away before it had its
// answer: nothing was written for it, it was gone when the answer was
// written, or a write to it failed. A client that closes the connection
// after the whole answer, while the handler still tidies up, is not
// canceled.
func (m *measured) canceled() bool {
	return m.ctx.Err() != nil && (m.status == 0 || m.gone)
}

// done records the request when its handler has returned (or panicked).
func (m *measured) done(panicked bool) {
	elapsed := time.Since(m.start).Seconds()

	if m.stream {
		metrics.JMAPEventStreams.Dec()
		metrics.JMAPEventStreamSeconds.Observe(elapsed)

		return
	}

	if m.endpoint == metrics.JMAPEndpointEvents {
		metrics.JMAPRequests.WithLabelValues(m.endpoint, httpResult(m.status, m.canceled(), panicked)).Inc()

		return
	}

	metrics.JMAPInflight.WithLabelValues(m.endpoint).Dec()

	// A redirect is the router's answer to a path that is almost a
	// route (a missing slash): the client comes again, and that request
	// is the endpoint's.
	counted := m.endpoint
	if redirected(m.status) {
		counted = metrics.JMAPEndpointOther
	}

	metrics.JMAPRequests.WithLabelValues(counted, httpResult(m.status, m.canceled(), panicked)).Inc()
	metrics.JMAPRequestSeconds.WithLabelValues(counted).Observe(elapsed)
}

// redirected reports whether a status is one the router redirects with.
func redirected(status int) bool {
	return status == http.StatusMovedPermanently || status == http.StatusTemporaryRedirect ||
		status == http.StatusPermanentRedirect
}

// httpResult is the result class of a request. A client that went away
// before its answer is canceled whatever the handler wrote after it:
// the status of a request nobody waits for says nothing about the
// server (an authentication cut short would otherwise count as a
// refused one).
func httpResult(status int, canceled, panicked bool) string {
	switch {
	case panicked:
		return metrics.JMAPResultServerError
	case canceled:
		return metrics.JMAPResultCanceled
	case status < http.StatusBadRequest:
		// 0 is a handler that wrote nothing, which net/http answers 200.
		return metrics.JMAPResultOK
	case status == http.StatusUnauthorized:
		return metrics.JMAPResultUnauthenticated
	case status == http.StatusForbidden:
		return metrics.JMAPResultForbidden
	case status == http.StatusTooManyRequests:
		return metrics.JMAPResultLimited
	case status == http.StatusServiceUnavailable:
		return metrics.JMAPResultUnavailable
	case status >= http.StatusInternalServerError:
		return metrics.JMAPResultServerError
	}

	return metrics.JMAPResultClientError
}

// ServeHTTP implements http.Handler: it measures the request and hands
// it to the routes.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	_, pattern := s.mux.Handler(r)
	if pattern == healthPattern {
		s.mux.ServeHTTP(w, r)

		return
	}

	endpoint, ok := endpointOf[pattern]
	if !ok {
		endpoint = metrics.JMAPEndpointOther
	}

	if r.Body != nil && (endpoint == metrics.JMAPEndpointAPI || endpoint == metrics.JMAPEndpointUpload) {
		r.Body = &countedBody{ReadCloser: r.Body, in: metrics.JMAPRequestBytes.WithLabelValues(endpoint)}
	}

	m := &measured{ResponseWriter: w, ctx: r.Context(), endpoint: endpoint, start: time.Now(),
		out: metrics.JMAPResponseBytes.WithLabelValues(endpoint)}

	if endpoint != metrics.JMAPEndpointEvents {
		metrics.JMAPInflight.WithLabelValues(endpoint).Inc()
	}

	panicked := true

	defer func() { m.done(panicked) }()

	s.mux.ServeHTTP(m, r)

	panicked = false
}

// methodLabel is the method label of a call: its name when this server
// has the method, and one fixed value for every other name.
func methodLabel(name string) string {
	if _, ok := methods[name]; ok {
		return name
	}

	return metrics.JMAPMethodUnknown
}

// methodResult is the result class of a method call.
func methodResult(ctx context.Context, merr *methodError) string {
	switch {
	case merr == nil:
		return metrics.JMAPResultOK
	case ctx.Err() != nil:
		return metrics.JMAPResultCanceled
	case merr.Type == errServerUnavailable:
		return metrics.JMAPResultUnavailable
	case serverError(merr.Type):
		return metrics.JMAPResultServerError
	}

	return metrics.JMAPResultClientError
}

// observeCall records one method call of a client: its result, its
// duration, and for a set-like method that succeeded what became of its
// objects (read from the response the client gets).
func observeCall(ctx context.Context, name string, start time.Time, response json.RawMessage, merr *methodError) {
	label := methodLabel(name)

	metrics.JMAPMethodCalls.WithLabelValues(label, methodResult(ctx, merr)).Inc()
	metrics.JMAPMethodSeconds.WithLabelValues(label).Observe(time.Since(start).Seconds())

	if merr == nil {
		countSetObjects(label, response)
	}
}

// observeImplicit records the response of an implicit call.
func observeImplicit(ctx context.Context, inv invocation) {
	if inv.Name != kwError {
		label := methodLabel(inv.Name)

		metrics.JMAPImplicitCalls.WithLabelValues(label, metrics.JMAPResultOK).Inc()
		countSetObjects(label, inv.Args)

		return
	}

	// The error response does not name its method; the one implicit
	// call there is is Email/set.
	var merr methodError

	_ = json.Unmarshal(inv.Args, &merr)

	metrics.JMAPImplicitCalls.WithLabelValues(methodEmailSet, methodResult(ctx, &merr)).Inc()
}

// setOutcome is the part of a set-like response that says what became
// of each object. The values are not decoded, only counted.
type setOutcome struct {
	Created      map[string]json.RawMessage `json:"created"`
	Updated      map[string]json.RawMessage `json:"updated"`
	NotCreated   map[string]json.RawMessage `json:"notCreated"`
	NotUpdated   map[string]json.RawMessage `json:"notUpdated"`
	NotDestroyed map[string]json.RawMessage `json:"notDestroyed"`
	Destroyed    []json.RawMessage          `json:"destroyed"`
}

// countSetObjects counts the objects of a set-like response by outcome.
func countSetObjects(method string, response json.RawMessage) {
	full, ok := setLike[method]
	if !ok {
		return
	}

	var out setOutcome
	if err := json.Unmarshal(response, &out); err != nil {
		return
	}

	add := func(outcome string, n int) {
		if n > 0 {
			metrics.JMAPSetObjects.WithLabelValues(method, outcome).Add(float64(n))
		}
	}

	add(metrics.JMAPSetCreated, len(out.Created))
	add(metrics.JMAPSetNotCreated, len(out.NotCreated))

	if !full {
		return
	}

	add(metrics.JMAPSetUpdated, len(out.Updated))
	add(metrics.JMAPSetDestroyed, len(out.Destroyed))
	add(metrics.JMAPSetNotUpdated, len(out.NotUpdated))
	add(metrics.JMAPSetNotDestroyed, len(out.NotDestroyed))
}
