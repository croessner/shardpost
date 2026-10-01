// Package jmap serves JMAP (RFC 8620) as a third front-end beside IMAP
// and POP3. It is a protocol layer on top of model.User, not a second
// data store: everything it answers comes from the same index, the same
// blobs and the same account change log that IMAP reads.
//
// The Core of RFC 8620 — the session resource, the API endpoint with
// batching and back-references, the error model, the limits, and blob
// upload and download — carries the data types built on top of it: mail
// and submission (RFC 8621), Sieve scripts, quotas, sharing and the
// others the session lists. A client that asks for a capability this
// server does not have is told so rather than served something
// half-correct.
package jmap

import (
	"context"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"shardpost/internal/auth"
	"shardpost/internal/model"
	"shardpost/internal/provision"
)

// CapCore is the URN of the JMAP Core capability.
const CapCore = "urn:ietf:params:jmap:core"

// CapWebPushVAPID is the capability that carries the VAPID public key a
// client hands its push service (RFC 9749).
const CapWebPushVAPID = "urn:ietf:params:jmap:webpush-vapid"

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
	// MaxConcurrentDownloads bounds the blob downloads and
	// MaxEventSources the open event sources of one account. They are
	// counted apart from the API requests, so that a client streaming an
	// attachment or holding its event sources open does not lock itself
	// out of the API.
	MaxConcurrentDownloads int
	MaxEventSources        int
	// MaxCallsInRequest bounds the method calls of one API request.
	MaxCallsInRequest int
	// MaxObjectsInGet and MaxObjectsInSet bound one /get and one /set.
	MaxObjectsInGet int
	MaxObjectsInSet int
	// MaxSizeBlobSet bounds one blob Blob/upload builds (RFC 9404 3.1);
	// it is never more than MaxSizeUpload, which bounds a blob however it
	// is made.
	MaxSizeBlobSet int64
	// MaxDataSources bounds the pieces of one Blob/upload creation; RFC
	// 9404 asks for at least 64.
	MaxDataSources int
	// MaxSizeBlobGet bounds the blob data one Blob/get returns, in bytes
	// before the base64 or JSON encoding. A client that wants more asks
	// for a range, or downloads the blob.
	MaxSizeBlobGet int64
	// SnippetReadBytes bounds how much of a message SearchSnippet/get
	// reads to find a match in the body: the front of the message, where
	// the text parts are. A match beyond it gives no preview snippet.
	SnippetReadBytes int64
	// MaxAttachmentReads bounds the messages one Email/query reads to
	// answer hasAttachment for an index record that does not carry it
	// (stored before the record had the field).
	MaxAttachmentReads int
	// AttachmentBackfills bounds the background fills of hasAttachment
	// that run at once in the process, and AttachmentBackfillTime how
	// long one may run.
	AttachmentBackfills    int
	AttachmentBackfillTime time.Duration
}

// DefaultLimits are the values a configuration that says nothing gets.
// They are deliberately modest: a JMAP client discovers them and adapts,
// and an operator who needs more says so.
var DefaultLimits = Limits{
	MaxSizeUpload:          50 << 20,
	MaxSizeRequest:         10 << 20,
	MaxConcurrentUpload:    4,
	MaxConcurrentRequests:  8,
	MaxConcurrentDownloads: 8,
	MaxEventSources:        4,
	MaxCallsInRequest:      16,
	MaxObjectsInGet:        500,
	MaxObjectsInSet:        500,
	MaxDataSources:         minDataSources,
	MaxSizeBlobGet:         10 << 20,
	SnippetReadBytes:       256 << 10,
	MaxAttachmentReads:     1000,
	AttachmentBackfills:    2,
	AttachmentBackfillTime: 10 * time.Minute,
}

// minDataSources is the fewest data sources per Blob/upload creation RFC
// 9404 3.1 lets a server accept.
const minDataSources = 64

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
	Push         PushOptions
	// Submission, when set, offers urn:ietf:params:jmap:submission.
	Submission *SubmissionOptions
	// Quota configures the Quota objects; urn:ietf:params:jmap:quota is
	// always offered.
	Quota QuotaOptions
	// VacationResponse offers urn:ietf:params:jmap:vacationresponse. It
	// is set when delivery can send the reply: when it has a relay
	// (smtp.addr).
	VacationResponse bool
	// Shard, when set, restricts the front-end to the accounts the
	// authentication backend assigns to this shard.
	Shard ShardGate
	// Autocreate are the mailboxes an account gets besides its INBOX
	// (server.autocreate), created when a client fetches the session.
	Autocreate []provision.Folder
	// Sieve, when set, offers urn:ietf:params:jmap:sieve over the
	// scripts ManageSieve manages.
	Sieve *SieveOptions
	// Sharing lists the accounts other namespaces share with the user:
	// shared mailboxes and public folders. The zero value lists none.
	Sharing SharingOptions
	// Principals configures Principal/* and ShareNotification/* (RFC
	// 9670); the zero value takes the defaults.
	Principals PrincipalOptions
	// Load configures how the front-end meets its limits.
	Load LoadOptions
}

// LoadOptions configure what happens at a limit: how long a request
// waits for a slot of its account, what Retry-After a 429 or a 503 of
// an undecided authentication carries, and the optional bound per
// client address checked before authentication.
type LoadOptions struct {
	// LimitWait is how long an API request, an upload or a download
	// waits for a slot when its account is at the limit; 0 answers 429
	// at once. Event sources never wait.
	LimitWait time.Duration
	// RetryAfter is the Retry-After of a 429 and of a 503 from the
	// authentication backend; 0 takes one second.
	RetryAfter time.Duration
	// MaxConcurrentPerIP bounds the requests one client address has in
	// flight, before authentication; 0 is no bound.
	MaxConcurrentPerIP int
}

// DefaultLoad are the load settings a configuration that says nothing
// gets.
var DefaultLoad = LoadOptions{LimitWait: 2 * time.Second, RetryAfter: time.Second}

// ShardGate names the backend attribute that carries an account's shard
// and the shard this server holds. Behind a proxy that does not route
// JMAP per account, every account of a deployment can reach this
// front-end; one that lives on another shard has no mail here — it
// would see an empty account, and what it wrote would land in the wrong
// shard. The zero value serves every authenticated account.
type ShardGate struct {
	Attribute string
	Value     string
}

// serves reports whether the account of an authentication result belongs
// here, and the shard values the result carries: always without a gate,
// otherwise only when one of the attribute's values is this shard. The
// attribute name is matched without regard to case, as LDAP names are; a
// missing attribute (nil values) is not this shard.
func (g ShardGate) serves(res *auth.Result) (bool, []string) {
	if g.Value == "" {
		return true, nil
	}

	var values []string

	for name, vals := range res.Attributes {
		if strings.EqualFold(name, g.Attribute) {
			values = append(values, vals...)
		}
	}

	for _, v := range values {
		if strings.EqualFold(strings.TrimSpace(v), g.Value) {
			return true, values
		}
	}

	return false, values
}

// PushOptions configure push (RFC 8620 7). The event source works with
// the zero value; push subscriptions need the shared store they are kept
// in, and Web Push the VAPID key of the worker that sends them.
type PushOptions struct {
	// KV keeps the push subscriptions, where the push worker finds them.
	// Without it PushSubscription/* is not offered.
	KV model.AdminKV
	// VAPIDPublicKey is the public half of the worker's VAPID key
	// (base64url), advertised as urn:ietf:params:jmap:webpush-vapid.
	VAPIDPublicKey string
	// MaxSubscriptions bounds the push subscriptions of one account.
	MaxSubscriptions int
	// MaxExpiry bounds how far ahead a subscription may expire; RFC 8620
	// 7.2 asks for at least 48 hours and recommends 7 days.
	MaxExpiry time.Duration
	// EventPoll is how often an open event source checks the account
	// state on its own, in case a change notification was lost.
	EventPoll time.Duration
	// AllowInternalURLs accepts push URLs that point into the server's
	// own network — a push service run in-house. The worker needs the
	// same setting to reach them.
	AllowInternalURLs bool
	// MaxSharedWatches bounds the shared accounts one event source (and
	// the push worker, per account) watches; the others are checked when
	// it polls. The watches on one namespace are shared by every event
	// source of the process.
	MaxSharedWatches int
}

// DefaultPush are the push settings a configuration that says nothing
// gets.
var DefaultPush = PushOptions{MaxSubscriptions: 16, MaxExpiry: 7 * 24 * time.Hour, EventPoll: 30 * time.Second,
	MaxSharedWatches: 8}

// minPushExpiry is the shortest maximum expiry RFC 8620 7.2 allows.
const minPushExpiry = 48 * time.Hour

// pushDefaults fills in the push settings left at zero.
func pushDefaults(p PushOptions) PushOptions {
	if p.MaxSubscriptions <= 0 {
		p.MaxSubscriptions = DefaultPush.MaxSubscriptions
	}

	if p.MaxExpiry <= 0 {
		p.MaxExpiry = DefaultPush.MaxExpiry
	}

	p.MaxExpiry = max(p.MaxExpiry, minPushExpiry)

	if p.EventPoll <= 0 {
		p.EventPoll = DefaultPush.EventPoll
	}

	if p.MaxSharedWatches <= 0 {
		p.MaxSharedWatches = DefaultPush.MaxSharedWatches
	}

	return p
}

// Server is the JMAP front-end.
type Server struct {
	o   Options
	mux *http.ServeMux
	// The per-account limits: API requests, uploads (the endpoint and
	// Blob/upload), downloads and open event sources; and the optional
	// limit per client address (nil: none).
	inAPI    *limiter
	inUpload *limiter
	inDown   *limiter
	inEvents *limiter
	perIP    *limiter
	// closing is closed when the server shuts down; open event sources
	// end on it, since http.Server.Shutdown waits for them otherwise.
	closing   chan struct{}
	closeOnce sync.Once
	// noShardAttr logs a shard gate without its attribute once.
	noShardAttr sync.Once
	// misses bounds the principal lookups that find nothing, per account.
	misses *missBudget
	// capLogged remembers the shared accounts already logged as cut at
	// jmap.max_shared_mailboxes.
	capLogged sync.Map
	// stateKeys caches the state keys of namespaces (stateKeyOf).
	stateKeys sync.Map
	// watches are the watches on the account objects of shared accounts
	// the event sources follow, one per namespace.
	watches *watchHub
	// The background fills of hasAttachment (attachment.go): the
	// namespaces being filled, the free slots, the fills running, and
	// the context that ends them at shutdown.
	fills      sync.Map
	fillSlots  chan struct{}
	fillWG     sync.WaitGroup
	fillCtx    context.Context
	fillCancel context.CancelFunc
	// fillMu orders starting a fill against Close: no fill is added to
	// fillWG once Close waits for it.
	fillMu sync.Mutex
	// unreadable are the messages a query could not read lately.
	unreadable unreadableSet
}

// New builds the server and its routes.
func New(o Options) *Server {
	if o.Log == nil {
		o.Log = slog.Default()
	}

	o.Limits = withDefaults(o.Limits)
	o.Push = pushDefaults(o.Push)
	o.Submission = submissionDefaults(o.Submission, o.Limits.MaxSizeUpload)
	o.Sharing = sharingDefaults(o.Sharing)
	o.Principals = principalDefaults(o.Principals, o.Store)

	if o.Sieve != nil && (o.Sieve.MaxScript <= 0 || o.Sieve.MaxScript > model.MaxScriptSize) {
		// The store holds nothing larger, whatever ManageSieve says.
		sv := *o.Sieve
		sv.MaxScript = model.MaxScriptSize
		o.Sieve = &sv
	}

	if o.Load.RetryAfter <= 0 {
		o.Load.RetryAfter = DefaultLoad.RetryAfter
	}

	s := &Server{o: o, mux: http.NewServeMux(),
		inAPI:    newLimiter("requests", o.Limits.MaxConcurrentRequests),
		inUpload: newLimiter("uploads", o.Limits.MaxConcurrentUpload),
		inDown:   newLimiter("downloads", o.Limits.MaxConcurrentDownloads),
		inEvents: newLimiter("event_sources", o.Limits.MaxEventSources),
		closing:  make(chan struct{}), misses: newMissBudget(o.Principals.MissPerMinute)}

	if o.Load.MaxConcurrentPerIP > 0 {
		s.perIP = newLimiter("per_ip", o.Load.MaxConcurrentPerIP)
	}

	s.watches = newWatchHub(o.Store, 0, o.Log)
	s.fillSlots = make(chan struct{}, o.Limits.AttachmentBackfills)
	s.fillCtx, s.fillCancel = context.WithCancel(context.Background())

	// The health endpoint has no authentication and no side effects: it
	// answers the backend check of a proxy in front, nothing else.
	s.mux.HandleFunc("GET "+PathHealth, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set(fieldContentType, "text/plain; charset=utf-8")
		_, _ = w.Write([]byte("ok\n"))
	})

	s.mux.HandleFunc("GET "+PathSession, traced("jmap.session", s.authenticated(s.handleSession)))
	s.mux.HandleFunc("POST "+PathAPI, traced("jmap.api", s.authenticated(s.handleAPI)))
	s.mux.HandleFunc("POST "+PathUpload+"{accountId}/", traced("jmap.upload", s.authenticated(s.handleUpload)))
	s.mux.HandleFunc("GET "+PathDownload+"{accountId}/{blobId}/{name}",
		traced("jmap.download", s.authenticated(s.handleDownload)))
	// Not traced: an event source stays open for hours (see tracing.go).
	s.mux.HandleFunc("GET "+PathEvents, s.authenticated(s.handleEvents))

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

	if l.MaxConcurrentDownloads <= 0 {
		l.MaxConcurrentDownloads = d.MaxConcurrentDownloads
	}

	if l.MaxEventSources <= 0 {
		l.MaxEventSources = d.MaxEventSources
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

	if l.SnippetReadBytes <= 0 {
		l.SnippetReadBytes = d.SnippetReadBytes
	}

	return attachmentDefaults(blobDefaults(l))
}

// attachmentDefaults fills in the bounds of the hasAttachment reads.
func attachmentDefaults(l Limits) Limits {
	d := DefaultLimits

	if l.MaxAttachmentReads <= 0 {
		l.MaxAttachmentReads = d.MaxAttachmentReads
	}

	if l.AttachmentBackfills <= 0 {
		l.AttachmentBackfills = d.AttachmentBackfills
	}

	if l.AttachmentBackfillTime <= 0 {
		l.AttachmentBackfillTime = d.AttachmentBackfillTime
	}

	return l
}

// blobDefaults fills in the limits of the blob methods: a blob built by
// Blob/upload is bounded like an upload, and a server may not accept
// fewer than 64 data sources.
func blobDefaults(l Limits) Limits {
	if l.MaxSizeBlobSet <= 0 || l.MaxSizeBlobSet > l.MaxSizeUpload {
		l.MaxSizeBlobSet = l.MaxSizeUpload
	}

	l.MaxDataSources = max(l.MaxDataSources, minDataSources)

	if l.MaxSizeBlobGet <= 0 {
		l.MaxSizeBlobGet = DefaultLimits.MaxSizeBlobGet
	}

	return l
}

// ServeHTTP implements http.Handler.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) { s.mux.ServeHTTP(w, r) }

// HTTPServer wraps the front-end in a server with the timeouts a
// public listener needs. A read or write timeout is deliberately absent:
// an event source stays open for hours. Shutting the server down ends
// the open event sources.
func (s *Server) HTTPServer(addr string) *http.Server {
	srv := &http.Server{
		Addr:              addr,
		Handler:           s,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}

	srv.RegisterOnShutdown(s.Close)

	return srv
}

// fillStopWait is how long Close waits for the background fills to end.
const fillStopWait = 10 * time.Second

// Close ends the open event sources and the background fills, and waits
// for the fills: the store may be closed after Close returns. A second
// call waits for the first.
func (s *Server) Close() {
	s.closeOnce.Do(func() {
		close(s.closing)

		s.fillMu.Lock()
		s.fillCancel()
		s.fillMu.Unlock()

		done := make(chan struct{})

		go func() {
			s.fillWG.Wait()
			close(done)
		}()

		select {
		case <-done:
		case <-time.After(fillStopWait):
			s.o.Log.Warn("jmap: background fills still running at shutdown")
		}
	})
}

// slot takes a slot of a limit for key. A request that waits (wait
// true) queues up to jmap.limit_wait for a free one; without a slot it
// is answered 429 with Retry-After, and slot reports false. A limit the
// session advertises (limitName, such as maxConcurrentRequests) gets
// the RFC 8620 3.6.1 limit error naming it; RFC 8620 reserves that
// error for those, so a limit it does not know (limitName empty:
// downloads, event sources, the bound per address) gets a plain problem
// (type about:blank).
func (s *Server) slot(w http.ResponseWriter, r *http.Request, lim *limiter, key string, wait bool, detail, limitName string) (func(), bool) {
	d := time.Duration(0)
	if wait {
		d = s.o.Load.LimitWait
	}

	release, ok := lim.acquire(r.Context(), key, d)
	if !ok {
		urn := errURNLimit
		if limitName == "" {
			urn = problemAboutBlank
		}

		s.retryAfter(w)
		writeProblem(w, http.StatusTooManyRequests, urn, detail, limitName)

		return nil, false
	}

	return release, true
}

// problemAboutBlank is the problem type of an error that is described
// by its status alone (RFC 7807 4.2).
const problemAboutBlank = "about:blank"

// retryAfter sets the Retry-After of a 429 or a 503 (whole seconds, at
// least one).
func (s *Server) retryAfter(w http.ResponseWriter) {
	w.Header().Set("Retry-After", strconv.Itoa(max(int(s.o.Load.RetryAfter/time.Second), 1)))
}
