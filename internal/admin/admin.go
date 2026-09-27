// Package admin serves the internal admin API (roadmap stage 8) whose
// contract is api/admin/openapi.yaml. Callers are identified by their
// transport, never by a network address alone: on the Unix socket by the
// peer's user ID, on the TCP listener by a verified client certificate.
// Every operation requires a role, and every request is logged for audit
// (never message contents or secrets).
package admin

import (
	"cmp"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"path"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"

	"go.opentelemetry.io/otel/attribute"

	"shardpost/internal/adminapi"
	"shardpost/internal/auth"
	"shardpost/internal/delivery"
	"shardpost/internal/jobs"
	"shardpost/internal/sharenotice"
	"shardpost/internal/telemetry"
)

// Roles of the admin API.
const (
	RoleDiagnose         = "diagnose"
	RoleDeliver          = "deliver"
	RoleDeliverNoEnforce = "deliver:noenforce"
)

// labelCode names the result code in the metric and the audit line;
// opOther is the operation label of unknown paths.
const (
	labelCode = "code"
	opOther   = "other"
)

// mimeRFC822 is the media type of a message.
const mimeRFC822 = "message/rfc822"

var requests = promauto.NewCounterVec(prometheus.CounterOpts{Name: "shardpost_admin_requests_total", Help: "Admin API requests by operation and result code."}, []string{"operation", labelCode})

// Options configure the admin API.
type Options struct {
	Delivery *delivery.Service
	Auth     auth.Authenticator
	Log      *slog.Logger
	// Version is reported by GET /admin/v1/version.
	Version string
	// Hostname names this server in the Received header of deliveries.
	Hostname string
	// MaxMessageBytes bounds a delivered message; required (> 0).
	MaxMessageBytes int64
	// SharedPrefix and PublicPrefix are the client-visible prefixes of
	// the shared and the public namespace. The ACL explanation names a
	// mailbox the way a client sees it, and a new mailbox must not be
	// created below either prefix, where no client could address it.
	// "" means that namespace is switched off.
	SharedPrefix string
	PublicPrefix string
	// ShareNotices records the share notifications of the grantees whose
	// rights an ACL change alters (RFC 9670 3); nil records none.
	ShareNotices *sharenotice.Recorder
	// SocketRoles are the roles of local callers on the Unix socket.
	SocketRoles []string
	// Clients map client certificate identities to roles (TCP listener);
	// identities compare case-insensitively.
	Clients map[string][]string
	// ClientScopes limit the accounts a client certificate identity may
	// address (path.Match patterns); identities without an entry may
	// address every account. Socket callers are not limited.
	ClientScopes map[string][]string
	// Import opens accounts for imports; unlike the protocol store it
	// does not refuse accounts under an import lock. nil disables the
	// import operations.
	Import delivery.Store
	// ImportLockTTL is how long an import lock lasts without renewal.
	ImportLockTTL time.Duration
	// ImportMaxBatchBytes and ImportMaxBatchMessages bound one
	// importMessages request; ImportMaxInventory one inventory page.
	ImportMaxBatchBytes    int64
	ImportMaxBatchMessages int
	ImportMaxInventory     int
	// ImportMaxChanges bounds the flag changes plus expunges of one
	// importChanges request.
	ImportMaxChanges int
	// ImportShardSize is the shard size of mailboxes an import creates
	// (0 = the store's).
	ImportShardSize uint32
	// Accounts opens accounts for the trace switch (like Import, it does
	// not refuse accounts under an import lock). nil disables the trace
	// operations.
	Accounts delivery.Store
	// TraceDefaultDuration applies when a request names no duration,
	// TraceMaxDuration caps every switch-on.
	TraceDefaultDuration time.Duration
	TraceMaxDuration     time.Duration
	// Instance, StartedAt and ConfigFingerprint describe this server in
	// GET /admin/v1/server; StorePing checks the store (nil: unknown).
	Instance          string
	StartedAt         time.Time
	ConfigFingerprint string
	StorePing         func() error
	// Jobs is the maintenance job system (nil: the job operations answer
	// 403).
	Jobs *jobs.Manager
	// Learning is the learning queue (nil: its operations answer 503).
	Learning LearningAdmin
	// SearchScanLimit bounds the messages one search request examines.
	SearchScanLimit int
}

// Server implements the generated adminapi.ServerInterface.
type Server struct {
	o       Options
	handler http.Handler
}

var _ adminapi.ServerInterface = (*Server)(nil)

// New builds the API handler.
func New(o Options) *Server {
	if o.MaxMessageBytes <= 0 {
		panic("admin: MaxMessageBytes must be positive")
	}

	if o.Hostname == "" {
		o.Hostname = "shardpost"
	}

	o.ImportLockTTL = cmp.Or(o.ImportLockTTL, 15*time.Minute)
	o.ImportMaxBatchBytes = cmp.Or(o.ImportMaxBatchBytes, 64<<20)
	o.ImportMaxBatchMessages = cmp.Or(o.ImportMaxBatchMessages, 500)
	o.ImportMaxInventory = cmp.Or(o.ImportMaxInventory, defaultPageSize)
	o.ImportMaxChanges = cmp.Or(o.ImportMaxChanges, 10000)
	o.TraceMaxDuration = cmp.Or(o.TraceMaxDuration, 24*time.Hour)
	o.TraceDefaultDuration = min(cmp.Or(o.TraceDefaultDuration, 30*time.Minute), o.TraceMaxDuration)

	clients := make(map[string][]string, len(o.Clients))
	for id, roles := range o.Clients {
		clients[strings.ToLower(id)] = roles
	}

	o.Clients = clients

	scopes := make(map[string][]string, len(o.ClientScopes))
	for id, patterns := range o.ClientScopes {
		scopes[strings.ToLower(id)] = patterns
	}

	o.ClientScopes = scopes
	s := &Server{o: o}
	mux := http.NewServeMux()
	adminapi.HandlerWithOptions(s, adminapi.StdHTTPServerOptions{
		BaseRouter: mux,
		ErrorHandlerFunc: func(w http.ResponseWriter, _ *http.Request, err error) {
			writeError(w, http.StatusBadRequest, adminapi.BadRequest, err.Error())
		},
	})
	s.handler = s.authenticate(mux)

	return s
}

// Handler is the HTTP handler for both listeners.
func (s *Server) Handler() http.Handler { return s.handler }

// GetVersion reports the server version.
func (s *Server) GetVersion(w http.ResponseWriter, r *http.Request) {
	if !require(w, r, RoleDiagnose) {
		return
	}

	writeJSON(w, http.StatusOK, adminapi.Version{Version: s.o.Version, GoVersion: runtime.Version()})
}

// principal is the authenticated caller of a request.
type principal struct {
	name      string
	transport string
	roles     map[string]bool
	// accounts limits the addressable accounts (path.Match patterns on
	// the lower-cased account); empty means all.
	accounts []string
}

// inScope reports whether the principal may address account.
func (p *principal) inScope(account string) bool {
	if len(p.accounts) == 0 {
		return true
	}

	// Store namespaces are case-sensitive while the patterns are not: a
	// limited client may only address the lower-case (canonical) form.
	if account != strings.ToLower(account) {
		return false
	}

	for _, pattern := range p.accounts {
		if ok, _ := path.Match(strings.ToLower(pattern), account); ok {
			return true
		}
	}

	return false
}

type principalKey struct{}

// withPrincipal attaches the caller of a connection to its context.
func withPrincipal(ctx context.Context, p *principal) context.Context {
	return context.WithValue(ctx, principalKey{}, p)
}

func principalOf(ctx context.Context) *principal {
	p, _ := ctx.Value(principalKey{}).(*principal)
	return p
}

// newPrincipal builds a principal with the given roles.
func newPrincipal(name, transport string, roles []string) *principal {
	p := &principal{name: name, transport: transport, roles: map[string]bool{}}
	for _, r := range roles {
		p.roles[r] = true
	}

	return p
}

// audit collects the fields of one request for the audit log line.
type audit struct {
	fields []any
}

type auditKey struct{}

// auditf adds fields to the audit line of the request.
func auditf(r *http.Request, kv ...any) {
	if a, ok := r.Context().Value(auditKey{}).(*audit); ok {
		a.fields = append(a.fields, kv...)
	}
}

// authenticate resolves the caller (connection context or client
// certificate), refuses unknown callers and writes the audit line.
func (s *Server) authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &recorder{ResponseWriter: w, status: http.StatusOK}
		a := &audit{}

		p := principalOf(r.Context())
		if p == nil && r.TLS != nil {
			p = s.certPrincipal(r)
		}

		if p == nil {
			a.fields = append(a.fields, "cert_identity", presentedIdentity(r))

			writeError(rec, http.StatusUnauthorized, adminapi.Unauthenticated, "unknown caller")
		} else {
			ctx := context.WithValue(withPrincipal(r.Context(), p), auditKey{}, a)

			// One trace per request; the store hangs its operations below
			// it. The operation label carries no account name.
			ctx, span := telemetry.Span(ctx, "admin."+operation(r.URL.Path),
				attribute.String("admin.principal", p.name), attribute.String("http.method", r.Method))

			next.ServeHTTP(rec, r.WithContext(ctx))
			span.SetAttributes(attribute.Int("http.status_code", rec.status))
			span.End()
		}

		name, transport := "", ""
		if p != nil {
			name, transport = p.name, p.transport
		}

		requests.WithLabelValues(operation(r.URL.Path), rec.result()).Inc()

		fields := append([]any{"event", "admin_request", "principal", name, "transport", transport,
			"method", r.Method, "path", r.URL.Path, "status", rec.status, labelCode, rec.result(),
			"duration_ms", time.Since(start).Milliseconds()}, a.fields...)
		s.o.Log.Info("admin request", fields...)
	})
}

// certPrincipal maps the verified client certificate to its roles; nil
// when the certificate identity is not configured.
func (s *Server) certPrincipal(r *http.Request) *principal {
	if len(r.TLS.VerifiedChains) == 0 || len(r.TLS.VerifiedChains[0]) == 0 {
		return nil
	}

	for _, id := range certIdentities(r.TLS.VerifiedChains[0][0]) {
		if roles, ok := s.o.Clients[strings.ToLower(id)]; ok {
			p := newPrincipal(id, "mtls", roles)
			p.accounts = s.o.ClientScopes[strings.ToLower(id)]

			return p
		}
	}

	return nil
}

// presentedIdentity is the first identity of a client certificate that
// was not accepted ("" without one), for the audit line.
func presentedIdentity(r *http.Request) string {
	if r.TLS == nil || len(r.TLS.PeerCertificates) == 0 {
		return ""
	}

	if ids := certIdentities(r.TLS.PeerCertificates[0]); len(ids) > 0 {
		return ids[0]
	}

	return ""
}

// require writes 403 unless the caller holds role.
func require(w http.ResponseWriter, r *http.Request, role string) bool {
	if p := principalOf(r.Context()); p != nil && p.roles[role] {
		return true
	}

	writeError(w, http.StatusForbidden, adminapi.Forbidden, "missing role "+role)

	return false
}

// requireAccount writes 403 unless the caller holds role and may address
// account (client scope).
func requireAccount(w http.ResponseWriter, r *http.Request, role, account string) bool {
	if !require(w, r, role) {
		return false
	}

	if !principalOf(r.Context()).inScope(account) {
		writeError(w, http.StatusForbidden, adminapi.Forbidden, "account outside the client's scope")
		return false
	}

	return true
}

// recorder remembers the status and error code of a response.
type recorder struct {
	http.ResponseWriter
	status int
	code   string
}

// result is the error code, "ok", or the bare status of a response
// without error document (such as an unknown path).
func (r *recorder) result() string {
	switch {
	case r.code != "":
		return r.code
	case r.status < http.StatusBadRequest:
		return "ok"
	default:
		return strconv.Itoa(r.status)
	}
}

// operation is the metric label of a path: the known operations by
// name, everything else as "other" (bounded cardinality).
func operation(path string) string {
	switch path {
	case "/admin/v1/version":
		return "version"
	case "/admin/v1/deliveries":
		return "deliver"
	case "/admin/v1/server":
		return "server"
	case "/admin/v1/jobs":
		return "jobs"
	default:
		if rest, ok := strings.CutPrefix(path, "/admin/v1/jobs/"); ok {
			if strings.HasSuffix(rest, "/cancel") {
				return "job_cancel"
			}

			return "job"
		}

		if strings.HasPrefix(path, "/admin/v1/plans/") {
			return "plan"
		}

		if rest, ok := strings.CutPrefix(path, "/admin/v1/accounts/"); ok {
			return accountOperation(path, strings.Split(rest, "/"))
		}

		return opOther
	}
}

// Path segments below an account that are also metric labels.
const (
	segMailboxes = "mailboxes"
	segACL       = "acl"
)

// accountOperation is the metric label of a path below an account
// (parts[0] is the account).
func accountOperation(path string, parts []string) string {
	if len(parts) == 1 {
		return "account"
	}

	switch parts[1] {
	case "import":
		return importOperation(path)
	case "trace", "quota", "shared", "plans":
		if len(parts) == 2 {
			return parts[1]
		}
	case segMailboxes:
		return mailboxOperation(parts[2:])
	}

	return opOther
}

// mailboxOperation labels the paths below /mailboxes.
func mailboxOperation(parts []string) string {
	switch {
	case len(parts) == 0:
		return segMailboxes
	case len(parts) == 1:
		return "mailbox"
	case len(parts) == 2 && parts[1] == segACL:
		return segACL
	case len(parts) == 3 && parts[1] == segACL && parts[2] == "explain":
		return "acl_explain"
	case len(parts) == 3 && parts[1] == segACL:
		return "acl_entry"
	default:
		return opOther
	}
}

// importOperation is the metric label of an import path (account and
// mailbox ids are not labels).
func importOperation(path string) string {
	switch {
	case strings.HasSuffix(path, "/import"):
		return "import_session"
	case strings.HasSuffix(path, "/import/mailboxes"):
		return "import_mailboxes"
	case strings.HasSuffix(path, "/messages"):
		return "import_messages"
	case strings.HasSuffix(path, "/changes"):
		return "import_changes"
	case strings.HasSuffix(path, "/finalize"):
		return "import_finalize"
	case strings.HasSuffix(path, "/acl"):
		return "import_acl"
	case strings.HasSuffix(path, "/import/sieve"):
		return "import_sieve"
	case strings.Contains(path, "/import/mailboxes/"):
		return "import_mailbox"
	default:
		return opOther
	}
}

func (r *recorder) WriteHeader(status int) {
	r.status = status
	r.ResponseWriter.WriteHeader(status)
}

// writeJSON writes v with status.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// writeError writes the error document of the contract.
func writeError(w http.ResponseWriter, status int, code adminapi.ErrorCode, msg string) {
	if rec, ok := w.(*recorder); ok {
		rec.code = string(code)
	}

	writeJSON(w, status, adminapi.Error{Code: code, Message: msg})
}
