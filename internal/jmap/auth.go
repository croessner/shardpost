package jmap

import (
	"context"
	"encoding/base64"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"strings"

	"shardpost/internal/auth"
	"shardpost/internal/model"
)

// session is what an authenticated request carries: who is asking, the
// store view opened for them, and the creation ids of the API request
// it belongs to. One request is served by one goroutine, so the map
// needs no lock; a session that serves no API request leaves it nil.
type session struct {
	account string
	user    model.User
	created map[string]string
	// using are the capabilities the request listed; Blob/lookup names
	// only the data types of those (RFC 9404 4.3).
	using  map[string]bool
	log    *slog.Logger
	limits Limits
	srv    *Server
	// remote is the client's IP address, for the relay (XCLIENT ADDR).
	remote string
	// releaseAddress gives the request's slot of the bound per client
	// address back early (an event source, which stays open for hours,
	// does that once it holds its own slot); calling it again is a no-op.
	releaseAddress func()
	// auth is the authentication result, whose attributes provisioning
	// reads (the quota).
	auth *auth.Result
	// own is the store view of the user's own namespace. user is the
	// view of the account a method call works on, which for a shared
	// account is another namespace; own never is.
	own model.User
	// acct is the account a method call works on; nil means the personal
	// account (see home).
	acct *account
	// views are the other namespaces the request opened.
	views *views
	// principals is what the request learned about principals; the
	// sessions of its method calls share it.
	principals *principalCache
}

// home is the personal account of the session.
func (s *session) home() *account {
	own := s.own
	if own == nil {
		own = s.user
	}

	return personalAccount(s.account, own)
}

// current is the account the method call works on.
func (s *session) current() *account {
	if s.acct == nil {
		s.acct = personalAccount(s.account, s.user)
		if s.own != nil {
			s.acct.own = s.own
		}
	}

	return s.acct
}

// accountID is the id of the account the method call works on.
func (s *session) accountID() string { return s.current().id }

// with is the session of a method call on an account: the same request,
// the store view of that account.
func (s *session) with(a *account) *session {
	if a.personal {
		cp := *s
		cp.acct = a
		cp.user = a.user

		return &cp
	}

	cp := *s
	cp.acct = a
	cp.user = a.user
	cp.log = s.log.With("shared", a.id)

	return &cp
}

// ctxKey is the context key of the authenticated session.
type ctxKey struct{}

// sessionOf returns the session of an authenticated request.
func sessionOf(r *http.Request) *session {
	s, _ := r.Context().Value(ctxKey{}).(*session)

	return s
}

// authenticated verifies the Authorization header, opens the store view
// and hands both to next. RFC 8620 8.2 asks for Basic and Bearer; both
// go to the same backend the other front-ends use, so that the answer is
// the same one IMAP would give — the server does not trust a proxy in
// front to have checked already.
func (s *Server) authenticated(next func(http.ResponseWriter, *http.Request)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.o.InsecureAuth && r.TLS == nil {
			writeProblem(w, http.StatusForbidden, errURNNotRequest, "authentication requires TLS", "")

			return
		}

		release, ok := s.addressSlot(w, r)
		if !ok {
			return
		}
		defer release()

		req, ok := credentials(r)
		if !ok {
			unauthorized(w)

			return
		}

		res, err := s.o.Auth.Authenticate(r.Context(), req)
		if err != nil {
			s.authFailed(w, r, req.Username, err)

			return
		}

		if !s.servedHere(w, res) {
			return
		}

		u, err := s.o.Store.User(res.Account)
		if err != nil {
			// An account under an import or maintenance lock (a
			// full-text conversion, a purge) is a passing state: the
			// client is told when to come back, the log does not
			// call it an error.
			if errors.Is(err, model.ErrImportLocked) {
				s.o.Log.Info("jmap: account locked", "account", res.Account, "err", err)
			} else {
				s.o.Log.Error("jmap: open account", "account", res.Account, "err", err)
			}

			s.retryAfter(w)
			writeProblem(w, http.StatusServiceUnavailable, errURNNotRequest, "account unavailable", "")

			return
		}
		defer u.Close()

		// The store hangs its operations below the request's span, and
		// below a method call's once one runs (callSpan).
		tracedUser(r.Context(), res.Account)
		model.UseRequest(r.Context(), u)

		others := &views{store: s.o.Store, ctx: r.Context()}
		defer others.close()

		ctx := context.WithValue(r.Context(), ctxKey{}, &session{account: res.Account, user: u, own: u, limits: s.o.Limits, srv: s, remote: clientIP(r), releaseAddress: release,
			log: s.o.Log.With("account", res.Account), auth: res, views: others,
			principals: &principalCache{}})
		next(w, r.WithContext(ctx))
	}
}

// servedHere checks the shard gate; an account that lives elsewhere is
// answered 403.
func (s *Server) servedHere(w http.ResponseWriter, res *auth.Result) bool {
	ok, shard := s.o.Shard.serves(res)
	if ok {
		return true
	}

	if shard == nil {
		// Once per process: a misspelt attribute would otherwise log on
		// every request of every account.
		s.noShardAttr.Do(func() {
			s.o.Log.Warn("jmap: authentication result has no shard attribute; check jmap.shard_attribute",
				"account", res.Account, "attribute", s.o.Shard.Attribute)
		})
	} else {
		s.o.Log.Info("jmap: account lives on another shard", "account", res.Account, "shard", shard)
	}

	writeProblem(w, http.StatusForbidden, errURNNotRequest, "this account is not served here", "")

	return false
}

// addressSlot takes a slot of the bound per client address
// (jmap.max_concurrent_per_ip) before the authentication, so that a
// flood of requests does not reach the backend at all. Without the
// bound it always succeeds.
func (s *Server) addressSlot(w http.ResponseWriter, r *http.Request) (func(), bool) {
	if s.perIP == nil {
		return func() {}, true
	}

	return s.slot(w, r, s.perIP, clientIP(r), false, "too many requests in flight from this address", "")
}

// authFailed answers a rejected or undecidable authentication. A backend
// that cannot decide right now is a 503, not a 401: telling the client
// its password is wrong would make it ask the user for a new one.
func (s *Server) authFailed(w http.ResponseWriter, r *http.Request, user string, err error) {
	if errors.Is(err, auth.ErrTemporary) {
		s.o.Log.Warn("jmap: authentication backend unavailable", "err", err)
		s.retryAfter(w)
		writeProblem(w, http.StatusServiceUnavailable, errURNNotRequest, "authentication unavailable", "")

		return
	}

	s.o.Log.Info("jmap: authentication failed", "user", user, "remote", r.RemoteAddr)
	unauthorized(w)
}

// unauthorized asks for credentials.
func unauthorized(w http.ResponseWriter) {
	w.Header().Set("WWW-Authenticate", `Basic realm="jmap", Bearer realm="jmap"`)
	writeProblem(w, http.StatusUnauthorized, errURNNotRequest, "authentication required", "")
}

// credentials reads the Authorization header into an auth request.
func credentials(r *http.Request) (auth.Request, bool) {
	req := auth.Request{Protocol: "jmap", Client: remoteAddr(r), TLS: r.TLS != nil}

	header := r.Header.Get("Authorization")

	scheme, rest, found := strings.Cut(header, " ")
	if !found {
		return req, false
	}

	switch {
	case strings.EqualFold(scheme, "bearer"):
		req.Token, req.Mechanism = strings.TrimSpace(rest), "OAUTHBEARER"

		return req, req.Token != ""
	case strings.EqualFold(scheme, "basic"):
		raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(rest))
		if err != nil {
			return req, false
		}

		user, pass, ok := strings.Cut(string(raw), ":")
		if !ok || user == "" {
			return req, false
		}

		req.Username, req.Password, req.Mechanism = user, pass, "PLAIN"

		return req, true
	}

	return req, false
}

// remoteAddr is the client address of the request, for the
// authentication backend's rate limiting and logging.
func remoteAddr(r *http.Request) *addr { return &addr{s: r.RemoteAddr} }

// clientIP is the IP address of the client, without the port; a remote
// address without a port is taken as it is.
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}

	return host
}

// addr adapts a string address to net.Addr.
type addr struct{ s string }

// clientAddr is the session's client address for backend lookups, or nil
// when the session has none (such as the push worker's own lookups).
func (s *session) clientAddr() net.Addr {
	if s == nil || s.remote == "" {
		return nil
	}

	return &addr{s: s.remote}
}

func (a *addr) Network() string { return "tcp" }
func (a *addr) String() string  { return a.s }
