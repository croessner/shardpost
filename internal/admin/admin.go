// Package admin serves the internal admin API (roadmap stage 8) whose
// contract is api/admin/openapi.yaml. Callers are identified by their
// transport, never by a network address alone: on the Unix socket by the
// peer's user ID, on the TCP listener by a verified client certificate.
// Every operation requires a role, and every request is logged for audit
// (never message contents or secrets).
package admin

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"

	"shardpost/internal/adminapi"
	"shardpost/internal/auth"
	"shardpost/internal/delivery"
)

// Roles of the admin API.
const (
	RoleDiagnose         = "diagnose"
	RoleDeliver          = "deliver"
	RoleDeliverNoEnforce = "deliver:noenforce"
)

// labelCode names the result code in the metric and the audit line.
const labelCode = "code"

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
	// SocketRoles are the roles of local callers on the Unix socket.
	SocketRoles []string
	// Clients map client certificate identities to roles (TCP listener);
	// identities compare case-insensitively.
	Clients map[string][]string
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

	clients := make(map[string][]string, len(o.Clients))
	for id, roles := range o.Clients {
		clients[strings.ToLower(id)] = roles
	}

	o.Clients = clients
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
			next.ServeHTTP(rec, r.WithContext(ctx))
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
			return newPrincipal(id, "mtls", roles)
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
	default:
		return "other"
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
