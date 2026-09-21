// Package jmap serves JMAP (RFC 8620) as a third front-end beside IMAP
// and POP3. It is a protocol layer on top of model.User, not a second
// data store: everything it answers comes from the same index, the same
// blobs and the same account change log that IMAP reads.
//
// This is the Core of RFC 8620: the session resource, the API endpoint
// with batching and back-references, the error model, the limits, and
// blob upload and download. The mail data types (RFC 8621) are built on
// top of it and are not here yet; a client that asks for a capability
// this server does not have is told so rather than served something
// half-correct.
package jmap

import (
	"log/slog"
	"net/http"
	"sync"
	"time"

	"shardpost/internal/auth"
	"shardpost/internal/model"
)

// CapCore is the URN of the JMAP Core capability.
const CapCore = "urn:ietf:params:jmap:core"

// Paths served by the JMAP front-end. The well-known location is fixed
// by RFC 8620 2.2; the rest sits under one prefix so that a reverse
// proxy in front of the server can route by path.
const (
	PathSession  = "/.well-known/jmap"
	PathAPI      = "/jmap/api/"
	PathUpload   = "/jmap/upload/"
	PathDownload = "/jmap/download/"
	PathEvents   = "/jmap/eventsource/"
	PathHealth   = "/jmap/healthz"
)

// Store opens per-user views, like the IMAP front-end's.
type Store interface {
	User(name string) (model.User, error)
}

// Limits are the bounds the session resource advertises and the server
// enforces. A client is told them up front, so an oversized request is a
// client bug, not a surprise.
type Limits struct {
	// MaxSizeUpload bounds one blob upload in bytes.
	MaxSizeUpload int64
	// MaxSizeRequest bounds one API request body in bytes.
	MaxSizeRequest int64
	// MaxConcurrentUpload and MaxConcurrentRequests bound what one
	// account may have in flight at the same time.
	MaxConcurrentUpload   int
	MaxConcurrentRequests int
	// MaxCallsInRequest bounds the method calls of one API request.
	MaxCallsInRequest int
	// MaxObjectsInGet and MaxObjectsInSet bound one /get and one /set.
	MaxObjectsInGet int
	MaxObjectsInSet int
}

// DefaultLimits are the values a configuration that says nothing gets.
// They are deliberately modest: a JMAP client discovers them and adapts,
// and an operator who needs more says so.
var DefaultLimits = Limits{
	MaxSizeUpload:         50 << 20,
	MaxSizeRequest:        10 << 20,
	MaxConcurrentUpload:   4,
	MaxConcurrentRequests: 4,
	MaxCallsInRequest:     16,
	MaxObjectsInGet:       500,
	MaxObjectsInSet:       500,
}

// Options configure the JMAP front-end.
type Options struct {
	Store Store
	Auth  auth.Authenticator
	Log   *slog.Logger
	// PublicBaseURL is the origin (and optional path prefix) the session
	// resource advertises: the address clients reach, which behind the
	// Director is the Director's, not this pod's. Without it the server
	// builds the URLs from the request, which is right for a direct
	// connection and wrong behind a proxy that does not rewrite.
	PublicBaseURL string
	// InsecureAuth allows authentication on a plaintext connection. It
	// exists for tests and for a listener that terminates TLS in front.
	InsecureAuth bool
	Limits       Limits
}

// Server is the JMAP front-end.
type Server struct {
	o       Options
	mux     *http.ServeMux
	mu      sync.Mutex
	inFlong map[string]int // API requests in flight, per account
	inFlup  map[string]int // uploads in flight, per account
}

// New builds the server and its routes.
func New(o Options) *Server {
	if o.Log == nil {
		o.Log = slog.Default()
	}

	o.Limits = withDefaults(o.Limits)

	s := &Server{o: o, mux: http.NewServeMux(), inFlong: map[string]int{}, inFlup: map[string]int{}}

	// The health endpoint has no authentication and no side effects: it
	// answers the backend check of a proxy in front, nothing else.
	s.mux.HandleFunc("GET "+PathHealth, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte("ok\n"))
	})

	s.mux.HandleFunc("GET "+PathSession, s.authenticated(s.handleSession))
	s.mux.HandleFunc("POST "+PathAPI, s.authenticated(s.handleAPI))
	s.mux.HandleFunc("POST "+PathUpload+"{accountId}/", s.authenticated(s.handleUpload))
	s.mux.HandleFunc("GET "+PathDownload+"{accountId}/{blobId}/{name}", s.authenticated(s.handleDownload))
	// The session resource has to advertise an eventSourceUrl, so the
	// URL exists and says what it is: push is a later stage, and a
	// client learns that from a JMAP answer rather than a bare 404.
	s.mux.HandleFunc("GET "+PathEvents, s.authenticated(func(w http.ResponseWriter, _ *http.Request) {
		writeProblem(w, http.StatusNotImplemented, errURNNotRequest, "this server does not push yet", "")
	}))

	return s
}

// withDefaults fills in the limits a configuration left at zero.
func withDefaults(l Limits) Limits {
	d := DefaultLimits

	if l.MaxSizeUpload <= 0 {
		l.MaxSizeUpload = d.MaxSizeUpload
	}

	if l.MaxSizeRequest <= 0 {
		l.MaxSizeRequest = d.MaxSizeRequest
	}

	if l.MaxConcurrentUpload <= 0 {
		l.MaxConcurrentUpload = d.MaxConcurrentUpload
	}

	if l.MaxConcurrentRequests <= 0 {
		l.MaxConcurrentRequests = d.MaxConcurrentRequests
	}

	if l.MaxCallsInRequest <= 0 {
		l.MaxCallsInRequest = d.MaxCallsInRequest
	}

	if l.MaxObjectsInGet <= 0 {
		l.MaxObjectsInGet = d.MaxObjectsInGet
	}

	if l.MaxObjectsInSet <= 0 {
		l.MaxObjectsInSet = d.MaxObjectsInSet
	}

	return l
}

// ServeHTTP implements http.Handler.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) { s.mux.ServeHTTP(w, r) }

// HTTPServer wraps the front-end in a server with the timeouts a
// public listener needs. A read timeout is deliberately absent: an
// EventSource connection (stage 5) stays open for hours.
func (s *Server) HTTPServer(addr string) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           s,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}
}

// enter takes a slot of the given per-account counter; it reports false
// when the account is already at its limit.
func (s *Server) enter(counters map[string]int, account string, limit int) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	if counters[account] >= limit {
		return false
	}

	counters[account]++

	return true
}

// leave gives a slot back.
func (s *Server) leave(counters map[string]int, account string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	counters[account]--
	if counters[account] <= 0 {
		delete(counters, account)
	}
}
