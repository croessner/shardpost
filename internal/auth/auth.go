// Package auth defines the authentication contract of the front-ends. The
// front-ends never see how a credential is verified; they hand over what
// the client presented (password or bearer token) plus connection context
// and get back an account name to open the store with.
package auth

import (
	"context"
	"errors"
	"net"
	"strings"
)

// Request is one authentication attempt.
type Request struct {
	Username  string
	Password  string // for PLAIN / LOGIN / USER+PASS
	Token     string // for XOAUTH2 / OAUTHBEARER
	Mechanism string // PLAIN, LOGIN, XOAUTH2, OAUTHBEARER
	Protocol  string // imap, pop3, lmtp, sieve
	Client    net.Addr
	Local     net.Addr
	TLS       bool
}

// Result is a successful authentication.
type Result struct {
	// Account is the canonical account name the store is opened with.
	// It may differ from the login name (aliases, case folding).
	Account string
	// Attributes are the backend's attributes (LDAP fields such as the
	// quota), when the backend returns any.
	Attributes map[string][]string
}

var (
	// ErrInvalid is a definitive "wrong credentials".
	ErrInvalid = errors.New("authentication failed")
	// ErrTemporary means the backend could not decide right now.
	ErrTemporary = errors.New("authentication temporarily unavailable")
)

// Authenticator verifies credentials and resolves recipients.
type Authenticator interface {
	Authenticate(ctx context.Context, r Request) (*Result, error)
	// Lookup resolves an envelope recipient (LMTP RCPT TO) to the account
	// that owns it, without credentials. ErrInvalid: no such recipient.
	Lookup(ctx context.Context, address, protocol string, client net.Addr) (*Result, error)
}

// Static is the test-profile user database ("USERS=test:test,load*:test"). A name
// ending in "*" matches every user with that prefix (load tests).
type Static map[string]string

func (s Static) password(user string) (string, bool) {
	if pw, ok := s[user]; ok {
		return pw, true
	}

	for pattern, pw := range s {
		if strings.HasSuffix(pattern, "*") && strings.HasPrefix(user, strings.TrimSuffix(pattern, "*")) {
			return pw, true
		}
	}

	return "", false
}

// Authenticate implements Authenticator (passwords only; tokens are
// accepted when they equal the password, which is enough for tests).
func (s Static) Authenticate(_ context.Context, r Request) (*Result, error) {
	pw, ok := s.password(r.Username)
	if !ok {
		return nil, ErrInvalid
	}

	secret := r.Password
	if r.Token != "" {
		secret = r.Token
	}

	if secret != pw {
		return nil, ErrInvalid
	}

	return &Result{Account: r.Username}, nil
}

// Lookup implements Authenticator: the local part of the address is the
// account.
func (s Static) Lookup(_ context.Context, address, _ string, _ net.Addr) (*Result, error) {
	user := strings.ToLower(strings.TrimSpace(strings.Trim(address, "<>")))
	if i := strings.LastIndex(user, "@"); i >= 0 {
		user = user[:i]
	}

	if _, ok := s.password(user); !ok {
		return nil, ErrInvalid
	}

	return &Result{Account: user}, nil
}
