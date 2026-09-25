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
	log     *slog.Logger
	limits  Limits
	srv     *Server
	// remote is the client's IP address, for the relay (XCLIENT ADDR).
	remote string
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

		u, err := s.o.Store.User(res.Account)
		if err != nil {
			s.o.Log.Error("jmap: open account", "account", res.Account, "err", err)
			writeProblem(w, http.StatusServiceUnavailable, errURNNotRequest, "account unavailable", "")

			return
		}
		defer u.Close()

		ctx := context.WithValue(r.Context(), ctxKey{}, &session{account: res.Account, user: u, limits: s.o.Limits, srv: s, remote: clientIP(r),
			log: s.o.Log.With("account", res.Account)})
		next(w, r.WithContext(ctx))
	}
}

// authFailed answers a rejected or undecidable authentication. A backend
// that cannot decide right now is a 503, not a 401: telling the client
// its password is wrong would make it ask the user for a new one.
func (s *Server) authFailed(w http.ResponseWriter, r *http.Request, user string, err error) {
	if errors.Is(err, auth.ErrTemporary) {
		s.o.Log.Warn("jmap: authentication backend unavailable", "err", err)
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

// clientIP is the IP address of the client, without the port.
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return ""
	}

	return host
}

// addr adapts a string address to net.Addr.
type addr struct{ s string }

func (a *addr) Network() string { return "tcp" }
func (a *addr) String() string  { return a.s }
