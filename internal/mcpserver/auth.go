package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"

	"shardpost/internal/admin"
	"shardpost/internal/config"
)

const (
	extraCredential     = "shardpost.credential"
	extraRequestContext = "shardpost.request_context"
	extraPending        = "shardpost.pending"

	errorInvalidToken      = "invalid_token"
	errorInsufficientScope = "insufficient_scope"
	retryAfterSeconds      = "60"

	// transportMCP names the endpoint in the admin API's audit line.
	transportMCP = "mcp"

	// principalSeparator joins client and person in an identity.
	principalSeparator = "#"
)

var (
	// errInvalidToken rejects a token with 401; the reason is never published.
	errInvalidToken = errors.New("invalid token")

	// errInsufficientScope rejects a valid token without any scope of the resource with 403.
	errInsufficientScope = errors.New("insufficient scope")

	// errNotGranted rejects a valid token of a person mcp.principals grants nothing with 403.
	errNotGranted = errors.New("not granted")

	// errUnavailable answers 503 when the issuer cannot say whether the token is active.
	errUnavailable = errors.New("token authority unavailable")
)

// credential is what a verified token establishes: the client and the
// person, the resource scopes it carries, and the admin API caller the
// principals grant.
type credential struct {
	clientID string
	subject  string
	scopes   []string
	caller   admin.Caller
}

// identity is the principal identity of the gate and the audit line.
func (c credential) identity() string {
	if c.clientID == "" || c.subject == "" {
		return ""
	}

	return c.clientID + principalSeparator + c.subject
}

// verifiedKey carries the SDK token info of an authenticated request to the SDK bridge.
type verifiedKey struct{}

// authenticate verifies the bearer token of every endpoint request, answers RFC 6750 / RFC 9728 challenges, bounds
// failed attempts per client IP and in-flight requests per principal, inspects the JSON-RPC body, and hands the
// verified facts to the SDK. No anonymous request reaches a tool.
func (s *Server) authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		clientIP := clientIPOf(request.Context())

		if s.limiter.blocked(clientIP) {
			writer.Header().Set("Retry-After", retryAfterSeconds)
			s.reject(writer, request, http.StatusTooManyRequests, resultRateLimited, "failed_authentication_limit")

			return
		}

		token, present := bearerToken(request)
		if !present {
			s.challenge(writer, request, http.StatusUnauthorized, "", "missing_token")

			return
		}

		cred, expiry, err := s.verify(request.Context(), token)
		if !s.answerVerification(writer, request, cred, err) {
			return
		}

		release, acquired := s.gate.acquire(request.Context(), cred.identity())
		if !acquired {
			writer.Header().Set("Retry-After", "1")
			s.rejectAs(writer, request, http.StatusTooManyRequests, resultBusy, "too_many_requests", cred.identity())

			return
		}

		// A tool call that outlives its budget is answered at once, while its admin API call may still run: the
		// slot is released only when that call returned, so the bounds hold for the work, not only the answers.
		pending := &sync.WaitGroup{}

		defer func() { go func() { pending.Wait(); release() }() }()

		if !s.inspectBody(writer, request) {
			return
		}

		info := &auth.TokenInfo{Scopes: cred.scopes, Expiration: expiry, UserID: cred.identity(),
			Extra: map[string]any{extraCredential: cred, extraRequestContext: request.Context(), extraPending: pending}}

		next.ServeHTTP(writer, request.WithContext(context.WithValue(request.Context(), verifiedKey{}, info)))
	})
}

// answerVerification answers a failed verification and reports whether the request may continue.
func (s *Server) answerVerification(writer http.ResponseWriter, request *http.Request, cred credential, err error) bool {
	switch {
	case err == nil:
		return true
	case errors.Is(err, errUnavailable):
		writer.Header().Set("Retry-After", "5")
		s.reject(writer, request, http.StatusServiceUnavailable, resultError, "token_authority_unavailable")
	case errors.Is(err, errInsufficientScope):
		s.challengeAs(writer, request, http.StatusForbidden, errorInsufficientScope, "insufficient_scope", cred.identity())
	case errors.Is(err, errNotGranted):
		s.rejectAs(writer, request, http.StatusForbidden, resultForbidden, "not_granted", cred.identity())
	default:
		s.limiter.fail(clientIPOf(request.Context()))
		s.challenge(writer, request, http.StatusUnauthorized, errorInvalidToken, "invalid_token")
	}

	return false
}

// sdkBridge hands the already verified token info to the SDK, which only accepts it through its bearer middleware.
// The bridge verifier never validates anything itself; a request without verified facts is refused.
func sdkBridge(next http.Handler) http.Handler {
	return auth.RequireBearerToken(func(_ context.Context, _ string, request *http.Request) (*auth.TokenInfo, error) {
		info, ok := request.Context().Value(verifiedKey{}).(*auth.TokenInfo)
		if !ok || info == nil {
			return nil, auth.ErrInvalidToken
		}

		return info, nil
	}, nil)(next)
}

// bearerToken returns the token of exactly one Authorization header in the Bearer scheme.
func bearerToken(request *http.Request) (string, bool) {
	values := request.Header.Values("Authorization")
	if len(values) != 1 {
		return "", false
	}

	scheme, token, found := strings.Cut(values[0], " ")
	if !found || !strings.EqualFold(scheme, "Bearer") || token == "" || strings.ContainsAny(token, " \t") {
		return "", false
	}

	return token, true
}

// verify accepts only an active user access token of the configured issuer whose audience is exactly the admitted
// client plus this resource, with at least one resource scope, of a person the principals grant a role. The issuer
// answers for signature, expiry and revocation; the claims are checked here again so that a misconfigured issuer
// cannot widen what the endpoint accepts.
func (s *Server) verify(ctx context.Context, token string) (credential, time.Time, error) {
	claims, err := s.deps.Tokens.Introspect(ctx, token)
	if err != nil {
		if errors.Is(err, ErrIntrospectionUnavailable) {
			// The cause (status, TLS, network) is logged here; it never names the token.
			s.logger().LogAttrs(context.WithoutCancel(ctx), slog.LevelWarn, "MCP token introspection failed",
				slog.String("error", err.Error()))

			return credential{}, time.Time{}, errUnavailable
		}

		return credential{}, time.Time{}, errInvalidToken
	}

	oauth := s.deps.Settings.OAuth

	issuer, _ := claims["iss"].(string)
	subject, _ := claims["sub"].(string)
	clientID, ok := issuingClient(claims)

	expiry, live := expiresAt(claims, s.now())
	if issuer != oauth.Issuer || subject == "" || !ok || !live || hasClaim(claims, "client_id") ||
		!slices.Contains(oauth.StaticClients, clientID) || !exactAudience(claims, clientID, s.deps.Settings.OAuth.Resource) {
		return credential{}, time.Time{}, errInvalidToken
	}

	granted := strings.Fields(stringClaim(claims, "scope"))
	scopes := slices.DeleteFunc(oauth.GetScopes(), func(scope string) bool { return !slices.Contains(granted, scope) })

	// From here client and person are verified: a refusal names them in the audit line.
	known := credential{clientID: clientID, subject: subject}

	if len(scopes) == 0 {
		return known, time.Time{}, errInsufficientScope
	}

	caller, ok := s.grant(claims)
	if !ok {
		return known, time.Time{}, errNotGranted
	}

	cred := credential{clientID: clientID, subject: subject, scopes: scopes, caller: caller}
	cred.caller.Name = transportMCP + ":" + clientID + principalSeparator + caller.Name
	cred.caller.Transport = transportMCP

	return cred, expiry, nil
}

// grant maps the person of a token to the union of the roles and account scopes of every principal that names
// them, by identity (principal_claim) or by group (groups_claim). Name is the matched identity value.
func (s *Server) grant(claims map[string]any) (admin.Caller, bool) {
	oauth := s.deps.Settings.OAuth
	identity := stringClaim(claims, oauth.GetPrincipalClaim())
	groups := stringsClaim(claims[oauth.GetGroupsClaim()])

	caller := admin.Caller{Name: identity}
	matched, unlimited := false, false

	for _, principal := range s.deps.Settings.Principals {
		if !names(principal, identity, groups) {
			continue
		}

		matched = true

		for _, role := range principal.Roles {
			if !slices.Contains(caller.Roles, role) {
				caller.Roles = append(caller.Roles, role)
			}
		}

		caller.AllAccounts = caller.AllAccounts || principal.AllAccounts
		unlimited = unlimited || len(principal.Accounts) == 0
		caller.Accounts = append(caller.Accounts, principal.Accounts...)
	}

	if !matched || identity == "" {
		return admin.Caller{}, false
	}

	// One principal without an account list grants every account.
	if unlimited {
		caller.Accounts = nil
	}

	return caller, true
}

// names reports whether a principal names the person: by identity, or by one of their groups.
func names(principal config.MCPPrincipal, identity string, groups []string) bool {
	if principal.Identity != "" {
		return identity != "" && principal.Identity == identity
	}

	return principal.Group != "" && slices.Contains(groups, principal.Group)
}

// issuingClient returns the client a user access token was issued to: azp, or the only audience without it.
func issuingClient(claims map[string]any) (string, bool) {
	if raw, present := claims["azp"]; present {
		azp, ok := raw.(string)

		return azp, ok && azp != ""
	}

	audiences, ok := audienceSet(claims)
	if !ok || len(audiences) != 1 {
		return "", false
	}

	for audience := range audiences {
		return audience, audience != ""
	}

	return "", false
}

// exactAudience reports whether the audience set is exactly the expected values.
func exactAudience(claims map[string]any, expected ...string) bool {
	audiences, ok := audienceSet(claims)
	if !ok {
		return false
	}

	wanted := map[string]bool{}

	for _, audience := range expected {
		if audience == "" {
			return false
		}

		wanted[audience] = true
	}

	if len(audiences) != len(wanted) {
		return false
	}

	for audience := range wanted {
		if !audiences[audience] {
			return false
		}
	}

	return true
}

// audienceSet returns the aud claim (a string or a list of strings) as a set.
func audienceSet(claims map[string]any) (map[string]bool, bool) {
	set := map[string]bool{}

	switch aud := claims["aud"].(type) {
	case string:
		set[aud] = true
	case []any:
		for _, entry := range aud {
			value, ok := entry.(string)
			if !ok {
				return nil, false
			}

			set[value] = true
		}
	default:
		return nil, false
	}

	return set, len(set) > 0
}

// expiresAt returns the exp claim and whether it lies in the future; a token without exp is refused.
func expiresAt(claims map[string]any, now time.Time) (time.Time, bool) {
	number, ok := claims["exp"].(json.Number)
	if !ok {
		return time.Time{}, false
	}

	seconds, err := number.Int64()
	if err != nil {
		value, floatErr := number.Float64()
		if floatErr != nil {
			return time.Time{}, false
		}

		seconds = int64(value)
	}

	expiry := time.Unix(seconds, 0)

	return expiry, expiry.After(now)
}

// hasClaim reports whether a claim is present.
func hasClaim(claims map[string]any, name string) bool {
	_, present := claims[name]

	return present
}

// stringClaim returns a string claim or "".
func stringClaim(claims map[string]any, name string) string {
	value, _ := claims[name].(string)

	return value
}

// stringsClaim returns a claim that is a string or a list of strings.
func stringsClaim(raw any) []string {
	switch value := raw.(type) {
	case string:
		return []string{value}
	case []any:
		out := make([]string, 0, len(value))

		for _, entry := range value {
			if text, ok := entry.(string); ok {
				out = append(out, text)
			}
		}

		return out
	default:
		return nil
	}
}

// challenge answers 401 or 403 with an RFC 6750 challenge that points to the RFC 9728 protected resource metadata.
func (s *Server) challenge(writer http.ResponseWriter, request *http.Request, status int, errorCode string, reason string) {
	s.challengeAs(writer, request, status, errorCode, reason, "")
}

// challengeAs is challenge for a request whose client and person are already verified.
func (s *Server) challengeAs(writer http.ResponseWriter, request *http.Request, status int, errorCode, reason, principal string) {
	params := []string{fmt.Sprintf("resource_metadata=%q", s.metadataURL)}

	if errorCode != "" {
		params = append(params, fmt.Sprintf("error=%q", errorCode))
	}

	params = append(params, fmt.Sprintf("scope=%q", strings.Join(s.deps.Settings.OAuth.GetScopes(), " ")))

	writer.Header().Set("WWW-Authenticate", "Bearer "+strings.Join(params, ", "))

	result := resultUnauthorized
	if status == http.StatusForbidden {
		result = resultForbidden
	}

	s.rejectAs(writer, request, status, result, reason, principal)
}

// protectedResourceMetadata serves the RFC 9728 document of the resource.
func (s *Server) protectedResourceMetadata(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodGet && request.Method != http.MethodHead {
		writeJSONError(writer, http.StatusMethodNotAllowed, "method_not_allowed")

		return
	}

	writer.Header().Set("Content-Type", "application/json")
	writer.Header().Set("Cache-Control", "max-age=60")

	_ = json.NewEncoder(writer).Encode(map[string]any{
		"resource":                 s.deps.Settings.OAuth.Resource,
		"authorization_servers":    []string{s.deps.Settings.OAuth.Issuer},
		"scopes_supported":         s.deps.Settings.OAuth.GetScopes(),
		"bearer_methods_supported": []string{"header"},
		"resource_name":            implementationTitle,
	})
}
