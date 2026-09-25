package auth

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Nauthilus authenticates against Nauthilus, preferably over gRPC
// (nauthilus.auth.v1.AuthService, see GRPCTransport) or over the JSON API
// (POST /api/v1/auth/json), using the contract the nauthilus-director
// speaks:
//
//   - the service authenticates itself with HTTP Basic (backchannel) or a
//     static bearer token;
//   - 200 + {"ok":true,"account_field":..,"attributes":{..}} is success and
//     attributes[account_field][0] is the canonical account;
//   - 403 is a definitive failure, 5xx/429 a temporary one; the Auth-Wait
//     header (seconds) is honoured before the failure is returned;
//   - bearer tokens (XOAUTH2/OAUTHBEARER) are sent in the password field
//     with method=xoauth2|oauthbearer, as the director draft prescribes,
//     or validated locally against the OIDC introspection endpoint;
//   - ?mode=no-auth with method=recipient_lookup resolves LMTP recipients.
type Nauthilus struct {
	URL      string // e.g. http://nauthilus.auth.svc:8080/api/v1/auth/json
	User     string // backchannel basic auth
	Password string
	Token    string // alternative: static bearer token for the backchannel
	// TokenSource, when set, supplies the bearer token (OIDC
	// client_credentials); it takes precedence over Token and User.
	TokenSource TokenSource
	// BearerMode is "password" (token in password field) or "introspect".
	BearerMode string
	Introspect *Introspection
	// AccountClaim names, first match wins (introspection only).
	AccountClaims []string
	HTTP          *http.Client
	// GRPC, when set, is used instead of the HTTP API (preferred).
	GRPC *GRPCTransport
	// MaxWait caps the Auth-Wait delay that is honoured.
	MaxWait time.Duration

	mu       sync.Mutex
	attempts map[string]uint // per client address: auth_login_attempt
}

// Introspection configures RFC 7662 token validation.
type Introspection struct {
	URL          string // .../oidc/introspect
	ClientID     string
	ClientSecret string
	Audience     string // required aud (empty: any)
	Scope        string // required scope (empty: any)
}

type nauthilusRequest struct {
	Username         string `json:"username"`
	Password         string `json:"password,omitempty"`
	ClientIP         string `json:"client_ip,omitempty"`
	ClientPort       string `json:"client_port,omitempty"`
	LocalIP          string `json:"local_ip,omitempty"`
	LocalPort        string `json:"local_port,omitempty"`
	Protocol         string `json:"protocol,omitempty"`
	Method           string `json:"method,omitempty"`
	SSL              string `json:"ssl,omitempty"`
	AuthLoginAttempt uint   `json:"auth_login_attempt,omitempty"`
}

type nauthilusResponse struct {
	OK           bool             `json:"ok"`
	AccountField string           `json:"account_field"`
	DisplayField string           `json:"display_name_field"`
	Attributes   map[string][]any `json:"attributes"`
	Error        string           `json:"error"`
	Session      string           `json:"-"`
	Headers      http.Header      `json:"-"`
	Backend      int              `json:"backend"`
	Extra        map[string]any   `json:"-"`
}

func (n *Nauthilus) client() *http.Client {
	if n.HTTP != nil {
		return n.HTTP
	}

	return &http.Client{Timeout: 10 * time.Second}
}

func hostPort(a net.Addr) (string, string) {
	if a == nil {
		return "", ""
	}

	h, p, err := net.SplitHostPort(a.String())
	if err != nil {
		return a.String(), ""
	}

	return h, p
}

// attempt returns the 1-based login attempt counter for a client address
// and increments it; success resets it.
func (n *Nauthilus) attempt(client net.Addr, reset bool) uint {
	key := ""
	if client != nil {
		key, _ = hostPort(client)
	}

	n.mu.Lock()
	defer n.mu.Unlock()

	if n.attempts == nil {
		n.attempts = map[string]uint{}
	}

	if reset {
		delete(n.attempts, key)
		return 0
	}

	n.attempts[key]++
	if len(n.attempts) > 10000 { // bounded memory; brute force is Nauthilus' job
		n.attempts = map[string]uint{}
	}

	return n.attempts[key]
}

// Authenticate implements Authenticator.
func (n *Nauthilus) Authenticate(ctx context.Context, r Request) (*Result, error) {
	req := nauthilusRequest{
		Username: r.Username, Password: r.Password, Protocol: r.Protocol,
		Method: strings.ToLower(r.Mechanism), SSL: sslFlag(r.TLS),
		AuthLoginAttempt: n.attempt(r.Client, false),
	}
	req.ClientIP, req.ClientPort = hostPort(r.Client)

	req.LocalIP, req.LocalPort = hostPort(r.Local)
	if req.Method == mechUser {
		req.Method = "userpass"
	}

	mode := ""

	if r.Token != "" {
		switch strings.ToLower(n.BearerMode) {
		case "", "password":
			req.Password = r.Token
		case "introspect":
			account, err := n.introspect(ctx, r.Token)
			if err != nil {
				return nil, err
			}

			if r.Username != "" && !strings.EqualFold(r.Username, account) {
				return nil, ErrInvalid
			}

			req.Username, req.Password = account, ""
			mode = modeNoAuth
		default:
			return nil, fmt.Errorf("nauthilus: unknown bearer mode %q", n.BearerMode)
		}
	}

	res, err := n.call(ctx, mode, req)
	if err != nil {
		return nil, err
	}

	n.attempt(r.Client, true)

	return res, nil
}

// Lookup implements Authenticator: identity resolution without a
// password (LMTP recipients, the target user of a master-user login).
// protocol is the session's protocol (lmtp, imap, pop3, sieve); the
// method name is the one nauthilus-director uses for recipient lookups.
func (n *Nauthilus) Lookup(ctx context.Context, address, protocol string, client net.Addr) (*Result, error) {
	addr := strings.TrimSpace(strings.Trim(address, "<>"))
	if i := strings.LastIndex(addr, "@"); i >= 0 {
		addr = addr[:i] + strings.ToLower(addr[i:])
	}

	if protocol == "" {
		protocol = "lmtp"
	}

	req := nauthilusRequest{Username: addr, Protocol: protocol, Method: "recipient_lookup"}
	req.ClientIP, req.ClientPort = hostPort(client)

	return n.call(ctx, modeNoAuth, req)
}

func sslFlag(on bool) string {
	if on {
		return "on"
	}

	return "off"
}

// outcome is the transport-independent result of one Nauthilus call.
type outcome struct {
	ok           bool
	temp         bool   // temporary failure
	refused      bool   // our backchannel credentials were refused
	message      string // status message for logs
	account      string
	wait         time.Duration
	accountField string
	displayField string
	attributes   map[string][]string
}

// modeNoAuth is the Nauthilus mode for identity lookups without credentials.
const modeNoAuth = "no-auth"

// mechUser is the POP3 USER/PASS mechanism name; Nauthilus calls it userpass.
const mechUser = "user"

// transport performs one call; mode is "" (authenticate) or modeNoAuth.
type transport interface {
	call(ctx context.Context, mode string, body nauthilusRequest) (*outcome, error)
}

// call performs one request over the configured transport and maps the
// outcome to Result / ErrInvalid / ErrTemporary, honouring Auth-Wait.
func (n *Nauthilus) call(ctx context.Context, mode string, body nauthilusRequest) (*Result, error) {
	var t transport = httpTransport{n}
	if n.GRPC != nil {
		t = n.GRPC
	}

	o, err := t.call(ctx, mode, body)
	if err != nil {
		return nil, err
	}

	switch {
	case o.temp:
		return nil, fmt.Errorf("%w: %s", ErrTemporary, o.message)
	case !o.ok:
		sleepCtx(ctx, o.wait)
		return nil, ErrInvalid
	}

	account := o.account
	if account == "" {
		if vals := o.attributes[o.accountField]; len(vals) > 0 {
			account = vals[0]
		}
	}

	if account == "" {
		return nil, fmt.Errorf("%w: nauthilus returned no account", ErrTemporary)
	}

	res := &Result{Account: strings.ToLower(strings.TrimSpace(account)), Attributes: o.attributes}
	if vals := o.attributes[o.displayField]; o.displayField != "" && len(vals) > 0 {
		res.DisplayName = strings.TrimSpace(vals[0])
	}

	return res, nil
}

// httpTransport talks to POST /api/v1/auth/json.
type httpTransport struct{ n *Nauthilus }

func (h httpTransport) call(ctx context.Context, mode string, body nauthilusRequest) (*outcome, error) {
	n := h.n

	req, o, err := h.newRequest(ctx, mode, body)
	if req == nil {
		return o, err
	}

	resp, err := n.client().Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrTemporary, err)
	}

	defer func() { _ = resp.Body.Close() }()

	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

	o = &outcome{wait: n.authWait(resp.Header), message: resp.Header.Get("Auth-Status")}
	if done := h.statusOutcome(o, resp.StatusCode, data); done {
		return o, nil
	}

	var out nauthilusResponse
	if err := json.Unmarshal(data, &out); err != nil {
		o.temp, o.message = true, "bad nauthilus response"
		return o, nil
	}

	o.ok, o.accountField, o.displayField = out.OK, out.AccountField, out.DisplayField
	if !out.OK && out.Error != "" {
		o.temp, o.message = true, out.Error
		return o, nil
	}

	o.attributes = map[string][]string{}

	for k, vals := range out.Attributes {
		for _, v := range vals {
			o.attributes[k] = append(o.attributes[k], fmt.Sprint(v))
		}
	}

	o.account = resp.Header.Get("Auth-User")
	if vals := o.attributes[o.accountField]; len(vals) > 0 {
		o.account = vals[0]
	}

	return o, nil
}

// newRequest builds the authenticated POST request. When it returns a nil
// request, the call ends with the returned outcome and error.
func (h httpTransport) newRequest(ctx context.Context, mode string, body nauthilusRequest) (*http.Request, *outcome, error) {
	n := h.n

	u, err := url.Parse(n.URL)
	if err != nil {
		return nil, nil, fmt.Errorf("nauthilus url: %w", err)
	}

	if mode != "" {
		q := u.Query()
		q.Set("mode", mode)
		u.RawQuery = q.Encode()
	}

	b, _ := json.Marshal(&body)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), bytes.NewReader(b))
	if err != nil {
		return nil, nil, err
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	switch {
	case n.TokenSource != nil:
		tok, err := n.TokenSource.Token(ctx)
		if err != nil {
			return nil, &outcome{temp: true, message: "nauthilus backchannel token: " + err.Error()}, nil
		}

		req.Header.Set("Authorization", "Bearer "+tok)
	case n.Token != "":
		req.Header.Set("Authorization", "Bearer "+n.Token)
	case n.User != "":
		req.SetBasicAuth(n.User, n.Password)
	}

	return req, nil, nil
}

// statusOutcome maps a non-200 HTTP status onto o. It returns true when the
// call is finished; false means the body carries the decision.
func (h httpTransport) statusOutcome(o *outcome, code int, data []byte) bool {
	switch code {
	case http.StatusOK:
		return false
	case http.StatusForbidden:
		return true
	case http.StatusUnauthorized:
		// our own backchannel credentials were refused: operator problem,
		// never a user failure. A cached token is dropped so the next call
		// fetches a fresh one.
		if h.n.TokenSource != nil {
			h.n.TokenSource.Invalidate()
		}

		o.temp, o.message = true, "nauthilus backchannel credentials refused"

		return true
	case http.StatusBadRequest:
		o.temp, o.message = true, "nauthilus rejected the request: "+strings.TrimSpace(string(data))
		return true
	default:
		o.temp, o.message = true, fmt.Sprintf("nauthilus status %d", code)
		return true
	}
}

func (n *Nauthilus) authWait(h http.Header) time.Duration {
	secs, err := strconv.ParseFloat(strings.TrimSpace(h.Get("Auth-Wait")), 64)
	if err != nil || secs <= 0 {
		return 0
	}

	d := time.Duration(secs * float64(time.Second))
	if limit := n.MaxWait; limit > 0 && d > limit {
		d = limit
	}

	return d
}

func sleepCtx(ctx context.Context, d time.Duration) {
	if d <= 0 {
		return
	}

	select {
	case <-ctx.Done():
	case <-time.After(d):
	}
}

// introspect validates a bearer token (RFC 7662) and returns the account.
func (n *Nauthilus) introspect(ctx context.Context, token string) (string, error) {
	claims, err := n.introspectClaims(ctx, token)
	if err != nil {
		return "", err
	}

	if active, _ := claims["active"].(bool); !active {
		return "", ErrInvalid
	}

	if n.Introspect.Audience != "" && !claimHas(claims["aud"], n.Introspect.Audience) {
		return "", ErrInvalid
	}

	if n.Introspect.Scope != "" {
		scope, _ := claims["scope"].(string)
		if !containsWord(scope, n.Introspect.Scope) {
			return "", ErrInvalid
		}
	}

	names := n.AccountClaims
	if len(names) == 0 {
		names = []string{"account", "account_key", "mail_account", "username", "preferred_username", "email", "sub"}
	}

	for _, c := range names {
		if v, ok := claims[c].(string); ok && v != "" {
			return v, nil
		}
	}

	return "", ErrInvalid
}

// introspectClaims queries the introspection endpoint and decodes the
// claims of the token.
func (n *Nauthilus) introspectClaims(ctx context.Context, token string) (map[string]any, error) {
	if n.Introspect == nil || n.Introspect.URL == "" {
		return nil, fmt.Errorf("%w: introspection not configured", ErrTemporary)
	}

	form := url.Values{"token": {token}, "token_type_hint": {"access_token"}}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, n.Introspect.URL, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}

	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetBasicAuth(n.Introspect.ClientID, n.Introspect.ClientSecret)

	resp, err := n.client().Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrTemporary, err)
	}

	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%w: introspection status %d", ErrTemporary, resp.StatusCode)
	}

	var claims map[string]any
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&claims); err != nil {
		return nil, fmt.Errorf("%w: bad introspection response", ErrTemporary)
	}

	return claims, nil
}

func claimHas(v any, want string) bool {
	switch x := v.(type) {
	case string:
		return x == want
	case []any:
		for _, e := range x {
			if s, ok := e.(string); ok && s == want {
				return true
			}
		}
	}

	return false
}

func containsWord(s, w string) bool {
	for _, x := range strings.Fields(s) {
		if x == w {
			return true
		}
	}

	return false
}

// --- master user ------------------------------------------------------------

// MasterUser wraps an Authenticator with Dovecot-style master-user logins
// ("<user><sep><master>" with the master password), which is how the
// nauthilus-director logs into backends in master_user mode. The wrapped
// authenticator resolves the target account (without credentials) so
// aliases and case folding still apply.
type MasterUser struct {
	Next      Authenticator
	Separator string // default "*"
	Name      string
	Password  string
}

// Authenticate implements Authenticator.
func (m *MasterUser) Authenticate(ctx context.Context, r Request) (*Result, error) {
	sep := m.Separator
	if sep == "" {
		sep = "*"
	}

	if m.Name == "" || r.Password == "" || !strings.Contains(r.Username, sep) {
		return m.Next.Authenticate(ctx, r)
	}

	target, master, _ := strings.Cut(r.Username, sep)
	if master != m.Name || r.Password != m.Password {
		return nil, ErrInvalid
	}

	res, err := m.Next.Lookup(ctx, target, r.Protocol, r.Client)
	if err != nil {
		return nil, err
	}

	return res, nil
}

// Lookup implements Authenticator.
func (m *MasterUser) Lookup(ctx context.Context, address, protocol string, client net.Addr) (*Result, error) {
	return m.Next.Lookup(ctx, address, protocol, client)
}

var _ Authenticator = (*Nauthilus)(nil)
var _ Authenticator = (*MasterUser)(nil)
