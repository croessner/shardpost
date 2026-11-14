package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// Label names of the JMAP request metrics.
const (
	labelEndpoint = "endpoint"
	labelMethod   = "method"
)

// The endpoint categories of the JMAP front-end: one per route, and
// other for a request no route matches. The health endpoint is not
// measured, a probe is no client.
const (
	JMAPEndpointSession  = "session"
	JMAPEndpointAPI      = "api"
	JMAPEndpointUpload   = "upload"
	JMAPEndpointDownload = "download"
	JMAPEndpointEvents   = "eventsource"
	JMAPEndpointOther    = "other"
)

// The results of a JMAP HTTP request: ok (below 400), unauthenticated
// (401), forbidden (403), limited (429), client_error (every other 4xx),
// unavailable (503), server_error (every other 5xx, and a handler that
// panicked) and canceled (the client went away before the handler was
// done, whatever status was written by then).
const (
	JMAPResultOK              = "ok"
	JMAPResultClientError     = "client_error"
	JMAPResultUnauthenticated = "unauthenticated"
	JMAPResultForbidden       = "forbidden"
	JMAPResultLimited         = "limited"
	JMAPResultUnavailable     = "unavailable"
	JMAPResultServerError     = "server_error"
	JMAPResultCanceled        = "canceled"
)

// JMAPMethodUnknown is the method label of a call whose name is not a
// method of this server: the name is the client's, so it is no label.
const JMAPMethodUnknown = "unknown"

// The outcomes of one object of a /set, an Email/copy, an Email/import
// or a Blob/upload.
const (
	JMAPSetCreated      = "created"
	JMAPSetUpdated      = "updated"
	JMAPSetDestroyed    = "destroyed"
	JMAPSetNotCreated   = "not_created"
	JMAPSetNotUpdated   = "not_updated"
	JMAPSetNotDestroyed = "not_destroyed"
)

// JMAPEndpoints are the endpoint categories, JMAPHTTPResults the results
// of a request and JMAPMethodResults the results of a method call:
// ok, client_error (an error response that blames the request),
// unavailable (serverUnavailable), server_error (serverFail,
// serverPartialFail) and canceled (an error after the client went away).
var (
	JMAPEndpoints = []string{JMAPEndpointSession, JMAPEndpointAPI, JMAPEndpointUpload,
		JMAPEndpointDownload, JMAPEndpointEvents, JMAPEndpointOther}
	JMAPHTTPResults = []string{JMAPResultOK, JMAPResultClientError, JMAPResultUnauthenticated,
		JMAPResultForbidden, JMAPResultLimited, JMAPResultUnavailable, JMAPResultServerError, JMAPResultCanceled}
	JMAPMethodResults = []string{JMAPResultOK, JMAPResultClientError, JMAPResultUnavailable,
		JMAPResultServerError, JMAPResultCanceled}
)

var (
	// JMAPRequests counts the HTTP requests of the JMAP front-end by
	// endpoint and result. A batch is one request, however many methods
	// it calls. A request is counted when its handler returns; an event
	// source is counted when its stream is established (or refused),
	// since it then stays open for hours.
	JMAPRequests = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace, Subsystem: subsystemJMAP, Name: "http_requests_total",
		Help: "JMAP HTTP requests by endpoint and result.",
	}, []string{labelEndpoint, labelResult})
	// JMAPRequestSeconds is how long a request took from its arrival to
	// the return of its handler, authentication and waiting for a slot
	// included. Event sources are not in it (see JMAPEventStreamSeconds).
	JMAPRequestSeconds = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: namespace, Subsystem: subsystemJMAP, Name: "http_request_seconds",
		Help:    "Duration of JMAP HTTP requests by endpoint (event sources excluded).",
		Buckets: []float64{.005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10, 30, 60},
	}, []string{labelEndpoint})
	// JMAPInflight is the number of requests being served, by endpoint.
	// Event sources are not in it.
	JMAPInflight = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: namespace, Subsystem: subsystemJMAP, Name: "http_inflight_requests",
		Help: "JMAP HTTP requests in flight by endpoint (event sources excluded).",
	}, []string{labelEndpoint})
	// JMAPRequestBytes counts the bytes read from the request bodies of
	// the API and the upload endpoint; with JMAPResponseBytes, what the
	// upload endpoint took in and the download endpoint gave out is the
	// blob volume. Both move as the bytes pass, which are not held for
	// it; a redirected request counts under the endpoint it aimed at.
	JMAPRequestBytes = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace, Subsystem: subsystemJMAP, Name: "http_request_bytes_total",
		Help: "Bytes read from JMAP request bodies by endpoint.",
	}, []string{labelEndpoint})
	// JMAPResponseBytes counts the bytes written to response bodies, by
	// endpoint (see JMAPRequestBytes).
	JMAPResponseBytes = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace, Subsystem: subsystemJMAP, Name: "http_response_bytes_total",
		Help: "Bytes written to JMAP response bodies by endpoint.",
	}, []string{labelEndpoint})
	// JMAPEventStreams is the number of established event sources.
	JMAPEventStreams = promauto.NewGauge(prometheus.GaugeOpts{
		Namespace: namespace, Subsystem: subsystemJMAP, Name: "event_streams",
		Help: "Established JMAP event sources.",
	})
	// JMAPEventStreamSeconds is how long an event source stayed open,
	// observed when it ends.
	JMAPEventStreamSeconds = promauto.NewHistogram(prometheus.HistogramOpts{
		Namespace: namespace, Subsystem: subsystemJMAP, Name: "event_stream_seconds",
		Help:    "Lifetime of JMAP event sources, observed when one ends.",
		Buckets: []float64{1, 10, 60, 300, 900, 1800, 3600, 7200, 14400, 43200, 86400},
	})
	// JMAPRequestCalls is the number of method calls of one API request.
	JMAPRequestCalls = promauto.NewHistogram(prometheus.HistogramOpts{
		Namespace: namespace, Subsystem: subsystemJMAP, Name: "request_method_calls",
		Help:    "Method calls per JMAP API request.",
		Buckets: []float64{1, 2, 3, 4, 6, 8, 12, 16, 32, 64},
	})
	// JMAPMethodCalls counts the method calls a client made, by method
	// and result; an error response inside an HTTP 200 is a result other
	// than ok here. An implicit call is not in it (JMAPImplicitCalls).
	JMAPMethodCalls = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace, Subsystem: subsystemJMAP, Name: "method_calls_total",
		Help: "JMAP method calls by method and result.",
	}, []string{labelMethod, labelResult})
	// JMAPMethodSeconds is how long a method call took, its implicit
	// call included.
	JMAPMethodSeconds = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: namespace, Subsystem: subsystemJMAP, Name: "method_seconds",
		Help:    "Duration of JMAP method calls by method.",
		Buckets: []float64{.001, .005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10},
	}, []string{labelMethod})
	// JMAPImplicitCalls counts the calls a method made on the client's
	// behalf (the Email/set of EmailSubmission/set and of Email/copy), by
	// method and result. They are no calls of the client and their time
	// is part of the calling method's.
	JMAPImplicitCalls = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace, Subsystem: subsystemJMAP, Name: "implicit_calls_total",
		Help: "Implicit JMAP method calls by method and result.",
	}, []string{labelMethod, labelResult})
	// JMAPSetObjects counts the objects of successful /set, Email/copy,
	// Email/import and Blob/upload calls (implicit ones included, under
	// their own method) by what became of them. The not_ outcomes are
	// the partial failures inside a call that itself succeeded; a call
	// that failed as a whole counts in JMAPMethodCalls and not here.
	JMAPSetObjects = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace, Subsystem: subsystemJMAP, Name: "set_objects_total",
		Help: "Objects of successful JMAP set-like calls by method and outcome.",
	}, []string{labelMethod, labelOutcome})
)

// InitJMAP creates the series of the JMAP request metrics, so that a
// server that has seen no request yet shows zero rather than nothing.
// methods are the methods the server implements; setMethods the ones of
// them whose response says what became of each object (true: a /set,
// false: one that only creates); implicit the ones another method calls
// on the client's behalf.
func InitJMAP(methods []string, setMethods map[string]bool, implicit []string) {
	for _, ep := range JMAPEndpoints {
		for _, res := range JMAPHTTPResults {
			JMAPRequests.WithLabelValues(ep, res)
		}

		JMAPResponseBytes.WithLabelValues(ep)

		if ep != JMAPEndpointEvents {
			JMAPRequestSeconds.WithLabelValues(ep)
			JMAPInflight.WithLabelValues(ep)
		}
	}

	JMAPRequestBytes.WithLabelValues(JMAPEndpointAPI)
	JMAPRequestBytes.WithLabelValues(JMAPEndpointUpload)

	for _, m := range methods {
		for _, res := range JMAPMethodResults {
			JMAPMethodCalls.WithLabelValues(m, res)
		}

		JMAPMethodSeconds.WithLabelValues(m)
	}

	// An unknown method has one result.
	JMAPMethodCalls.WithLabelValues(JMAPMethodUnknown, JMAPResultClientError)
	JMAPMethodSeconds.WithLabelValues(JMAPMethodUnknown)

	for _, m := range implicit {
		for _, res := range JMAPMethodResults {
			JMAPImplicitCalls.WithLabelValues(m, res)
		}
	}

	for m, full := range setMethods {
		JMAPSetObjects.WithLabelValues(m, JMAPSetCreated)
		JMAPSetObjects.WithLabelValues(m, JMAPSetNotCreated)

		if full {
			for _, o := range []string{JMAPSetUpdated, JMAPSetDestroyed, JMAPSetNotUpdated, JMAPSetNotDestroyed} {
				JMAPSetObjects.WithLabelValues(m, o)
			}
		}
	}
}
