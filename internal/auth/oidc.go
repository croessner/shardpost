package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// TokenSource supplies the bearer token for the Nauthilus backchannel.
type TokenSource interface {
	// Token returns a currently valid access token.
	Token(ctx context.Context) (string, error)
	// Invalidate drops the cached token after the server refused it.
	Invalidate()
}

// ClientCredentials obtains access tokens with the OAuth 2.0
// client_credentials grant (RFC 6749 §4.4), the way nauthilus-director
// and doppelgaenger authenticate against the Nauthilus authority. Tokens
// are cached and renewed shortly before they expire; concurrent callers
// share one request.
type ClientCredentials struct {
	TokenURL     string
	ClientID     string
	ClientSecret string
	Scopes       []string
	Audience     string // optional "audience" parameter
	// AuthMethod is client_secret_post (default) or client_secret_basic.
	AuthMethod string
	HTTP       *http.Client
	// RefreshBefore renews the token this long before expiry (default 1m).
	RefreshBefore time.Duration

	mu     sync.Mutex
	token  string
	expiry time.Time
	ttl    time.Duration
}

// ErrTokenEndpoint wraps failures of the token endpoint (temporary).
var ErrTokenEndpoint = errors.New("oidc token endpoint")

func (c *ClientCredentials) client() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}

	return &http.Client{Timeout: 10 * time.Second}
}

// Token implements TokenSource.
func (c *ClientCredentials) Token(ctx context.Context) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.token != "" && time.Now().Add(c.refreshBefore(c.ttl)).Before(c.expiry) {
		return c.token, nil
	}

	tok, ttl, err := c.fetch(ctx)
	if err != nil {
		// Keep serving a still-valid token while the endpoint is down.
		if c.token != "" && time.Now().Before(c.expiry) {
			return c.token, nil
		}

		return "", err
	}

	c.token, c.expiry, c.ttl = tok, time.Now().Add(ttl), ttl

	return tok, nil
}

// refreshBefore is the configured lead time, capped at half the token's
// lifetime so that short-lived tokens are not refetched on every call.
func (c *ClientCredentials) refreshBefore(ttl time.Duration) time.Duration {
	rb := c.RefreshBefore
	if rb == 0 {
		rb = time.Minute
	}

	if ttl > 0 && rb > ttl/2 {
		rb = ttl / 2
	}

	return rb
}

// Invalidate implements TokenSource.
func (c *ClientCredentials) Invalidate() {
	c.mu.Lock()
	c.token, c.expiry = "", time.Time{}
	c.mu.Unlock()
}

func (c *ClientCredentials) fetch(ctx context.Context) (string, time.Duration, error) {
	req, err := c.newTokenRequest(ctx)
	if err != nil {
		return "", 0, err
	}

	resp, err := c.client().Do(req)
	if err != nil {
		return "", 0, fmt.Errorf("%w: %v", ErrTokenEndpoint, err)
	}

	defer func() { _ = resp.Body.Close() }()

	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		var e struct {
			Error string `json:"error"`
			Desc  string `json:"error_description"`
		}

		_ = json.Unmarshal(data, &e)

		return "", 0, fmt.Errorf("%w: status %d %s %s", ErrTokenEndpoint, resp.StatusCode, e.Error, e.Desc)
	}

	return parseTokenResponse(data)
}

// newTokenRequest builds the client_credentials token request.
func (c *ClientCredentials) newTokenRequest(ctx context.Context) (*http.Request, error) {
	form := url.Values{"grant_type": {"client_credentials"}}
	if len(c.Scopes) > 0 {
		form.Set("scope", strings.Join(c.Scopes, " "))
	}

	if c.Audience != "" {
		form.Set("audience", c.Audience)
	}

	basic := strings.EqualFold(c.AuthMethod, "client_secret_basic")
	if !basic {
		form.Set("client_id", c.ClientID)
		form.Set("client_secret", c.ClientSecret)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.TokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}

	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	if basic {
		req.SetBasicAuth(url.QueryEscape(c.ClientID), url.QueryEscape(c.ClientSecret))
	}

	return req, nil
}

// parseTokenResponse decodes a successful token endpoint response.
func parseTokenResponse(data []byte) (string, time.Duration, error) {
	var out struct {
		AccessToken string `json:"access_token"`
		TokenType   string `json:"token_type"`
		ExpiresIn   int64  `json:"expires_in"`
	}
	if err := json.Unmarshal(data, &out); err != nil || out.AccessToken == "" {
		return "", 0, fmt.Errorf("%w: bad token response", ErrTokenEndpoint)
	}

	if out.TokenType != "" && !strings.EqualFold(out.TokenType, "bearer") {
		return "", 0, fmt.Errorf("%w: unsupported token type %q", ErrTokenEndpoint, out.TokenType)
	}

	ttl := time.Duration(out.ExpiresIn) * time.Second
	if ttl <= 0 {
		ttl = 5 * time.Minute
	}

	return out.AccessToken, ttl, nil
}

var _ TokenSource = (*ClientCredentials)(nil)
