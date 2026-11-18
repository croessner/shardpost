package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// maxIntrospectionBytes bounds an introspection answer.
const maxIntrospectionBytes = 64 << 10

// ErrIntrospectionUnavailable is returned when the issuer cannot say
// whether a token is active (network error, 429, 5xx, or an answer that
// is not RFC 7662).
var ErrIntrospectionUnavailable = errors.New("token introspection unavailable")

// ErrTokenInactive is returned for a token the issuer reports inactive.
var ErrTokenInactive = errors.New("token inactive")

// Introspector asks the issuer about one token (RFC 7662). It returns
// the claims of an active token, ErrTokenInactive for an inactive one,
// and ErrIntrospectionUnavailable when the issuer cannot decide.
type Introspector interface {
	Introspect(ctx context.Context, token string) (map[string]any, error)
}

// HTTPIntrospector introspects at an RFC 7662 endpoint with HTTP Basic
// client authentication, as the issuer (Nauthilus) expects of a
// confidential client that owns the resource.
type HTTPIntrospector struct {
	HTTP         *http.Client
	URL          string
	ClientID     string
	ClientSecret string
}

// Introspect implements Introspector.
func (h *HTTPIntrospector) Introspect(ctx context.Context, token string) (map[string]any, error) {
	form := url.Values{"token": {token}, "token_type_hint": {"access_token"}}

	request, err := http.NewRequestWithContext(ctx, http.MethodPost, h.URL, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrIntrospectionUnavailable, err)
	}

	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Accept", "application/json")
	request.SetBasicAuth(h.ClientID, h.ClientSecret)

	client := h.HTTP
	if client == nil {
		client = http.DefaultClient
	}

	response, err := client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrIntrospectionUnavailable, err)
	}

	defer func() { _ = response.Body.Close() }()

	if response.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, maxIntrospectionBytes))

		return nil, fmt.Errorf("%w: status %d", ErrIntrospectionUnavailable, response.StatusCode)
	}

	var claims map[string]any

	decoder := json.NewDecoder(io.LimitReader(response.Body, maxIntrospectionBytes))
	decoder.UseNumber()

	if err = decoder.Decode(&claims); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrIntrospectionUnavailable, err)
	}

	if active, ok := claims["active"].(bool); !ok || !active {
		return nil, ErrTokenInactive
	}

	return claims, nil
}
