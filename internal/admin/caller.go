package admin

import (
	"net/http"
	"slices"

	"shardpost/internal/adminapi"
)

// Caller is a caller another transport of this process authenticated,
// such as the MCP endpoint: a person with the roles and the account
// scope that transport granted.
type Caller struct {
	// Name identifies the caller in the audit line and the trace.
	Name string
	// Transport names the transport in the audit line (e.g. "mcp").
	Transport string
	Roles     []string
	// Accounts and AllAccounts are the account scope, as for a client
	// certificate (admin.clients).
	Accounts    []string
	AllAccounts bool
}

// ServeAs serves a request through the API as caller. The API checks
// the roles and the account scope, counts the request and writes its
// audit line exactly as for its own listeners, so another transport
// can never do more than the same caller could do here.
func (s *Server) ServeAs(w http.ResponseWriter, r *http.Request, c Caller) {
	if c.Name == "" || c.Transport == "" {
		writeError(w, http.StatusUnauthorized, adminapi.Unauthenticated, "unknown caller")

		return
	}

	p := newPrincipal(c.Name, c.Transport, c.Roles)
	p.accounts = slices.Clone(c.Accounts)
	p.allAccounts = c.AllAccounts

	s.handler.ServeHTTP(w, r.WithContext(withPrincipal(r.Context(), p)))
}
