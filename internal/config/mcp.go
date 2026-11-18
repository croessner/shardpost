package config

import (
	"fmt"
	"net/netip"
	"net/url"
	"path"
	"slices"
	"strings"
	"time"
)

// MCPTLSTerminationTrustedReverseProxy terminates TLS at a trusted
// reverse proxy in front of the MCP listener (the only mode, as in
// Nauthilus).
const MCPTLSTerminationTrustedReverseProxy = "trusted_reverse_proxy"

// Defaults and ceilings of the MCP endpoint. They are those of the
// Nauthilus MCP endpoint, so that both read the same way.
const (
	defaultMCPAddress                   = "127.0.0.1:9097"
	defaultMCPTimeout                   = 15 * time.Second
	maxMCPTimeout                       = time.Minute
	defaultMCPMaxConcurrent             = 8
	maxMCPMaxConcurrent                 = 64
	defaultMCPMaxConcurrentPerPrincipal = 2
	defaultMCPMaxRequestBytes           = int64(64 << 10)
	maxMCPMaxRequestBytes               = int64(1 << 20)
	defaultMCPMaxResponseBytes          = int64(1 << 20)
	minMCPMaxResponseBytes              = int64(4 << 10)
	maxMCPMaxResponseBytes              = int64(16 << 20)
	defaultMCPFailedAuthPerMinute       = 30

	// DefaultMCPScope is the OAuth scope that grants the read-only
	// tools when mcp.oauth.scopes is empty.
	DefaultMCPScope = "shardpost:diagnose"

	defaultMCPPrincipalClaim = "sub"
	defaultMCPGroupsClaim    = "groups"

	mcpPath = "mcp"

	// URI schemes of the MCP resource.
	SchemeHTTP  = "http"
	SchemeHTTPS = "https"
)

// MCP configures the optional read-only MCP endpoint (Model Context
// Protocol, stateless Streamable HTTP). It is built like the MCP
// endpoint of Nauthilus: a listener of its own behind a TLS-terminating
// reverse proxy, the resource server of one OAuth protected resource,
// with the same transport, limit and audit keys. ShardPost is not an
// authorization server, so the protected resource is described in
// mcp.oauth and its tokens are checked by RFC 7662 introspection at
// the issuer (Nauthilus). The tools are the read-only operations of the
// admin API with the role diagnose; mcp.principals map the token's
// person to that role and an account scope, like admin.clients map a
// client certificate. Every key is read at startup.
type MCP struct {
	TrustedProxies        []string `mapstructure:"trusted_proxies"`
	AllowedHosts          []string `mapstructure:"allowed_hosts"`
	AllowedBrowserOrigins []string `mapstructure:"allowed_browser_origins"`
	// Principals grant the role diagnose and an account scope to the
	// people whose tokens name them.
	Principals     []MCPPrincipal `mapstructure:"principals" validate:"dive"`
	Limits         MCPLimits      `mapstructure:"limits"`
	Audit          MCPAudit       `mapstructure:"audit"`
	OAuth          MCPOAuth       `mapstructure:"oauth"`
	Address        string         `mapstructure:"address" env:"MCP_ADDRESS"`
	TLSTermination string         `mapstructure:"tls_termination"`
	Enabled        bool           `mapstructure:"enabled" env:"MCP_ENABLED"`
}

// MCPLimits bounds the MCP endpoint; zero takes the default.
type MCPLimits struct {
	Timeout                   time.Duration `mapstructure:"timeout"`
	MaxConcurrent             int           `mapstructure:"max_concurrent"`
	MaxConcurrentPerPrincipal int           `mapstructure:"max_concurrent_per_principal"`
	MaxRequestBytes           int64         `mapstructure:"max_request_bytes"`
	MaxResponseBytes          int64         `mapstructure:"max_response_bytes"`
	FailedAuthPerMinute       int           `mapstructure:"failed_auth_per_minute"`
}

// MCPAudit toggles the transport audit line of the MCP endpoint; it is
// on unless switched off explicitly. The admin API writes its own audit
// line for every tool call in any case.
type MCPAudit struct {
	Enabled *bool `mapstructure:"enabled"`
}

// MCPOAuth describes the protected resource and how its tokens are
// checked.
type MCPOAuth struct {
	// Resource is the RFC 8707 resource indicator, the public URL of the
	// endpoint (e.g. https://mcp.example.org/mcp). Its path is the
	// endpoint path.
	Resource string `mapstructure:"resource"`
	// Issuer is the authorization server (the Nauthilus issuer URL); a
	// token must name it as iss, and the RFC 9728 metadata points to it.
	Issuer string `mapstructure:"issuer"`
	// Scopes grant the read-only tools; a token needs one of them.
	Scopes []string `mapstructure:"scopes"`
	// StaticClients are the OAuth clients (azp) whose tokens are
	// accepted, e.g. the client of an MCP host.
	StaticClients []string `mapstructure:"static_clients"`
	// IntrospectURL is the RFC 7662 endpoint; empty takes
	// nauthilus.introspect_url.
	IntrospectURL string `mapstructure:"introspect_url"`
	// IntrospectClientID and its secret are the client that owns the
	// resource at the issuer (Nauthilus: token_introspection.resources);
	// empty takes the introspection client of the nauthilus section.
	IntrospectClientID         string `mapstructure:"introspect_client_id"`
	IntrospectClientSecret     string `mapstructure:"introspect_client_secret" env:"MCP_INTROSPECT_CLIENT_SECRET" secret:"true"`
	IntrospectClientSecretFile string `mapstructure:"introspect_client_secret_file" env:"MCP_INTROSPECT_CLIENT_SECRET_FILE"`
	// PrincipalClaim names the claim mcp.principals[].identity is
	// compared with (default sub), GroupsClaim the claim of the groups
	// mcp.principals[].group is compared with (default groups).
	PrincipalClaim string `mapstructure:"principal_claim"`
	GroupsClaim    string `mapstructure:"groups_claim"`
}

// MCPPrincipal grants roles and an account scope to the person a token
// names, by identity or by group (exactly one of them).
type MCPPrincipal struct {
	Identity string `mapstructure:"identity"`
	Group    string `mapstructure:"group"`
	// Roles are the admin API roles; the endpoint is read-only, so only
	// diagnose is accepted.
	Roles []string `mapstructure:"roles" validate:"required,min=1,dive,oneof=diagnose"`
	// Accounts and AllAccounts are those of admin.clients.
	Accounts    []string `mapstructure:"accounts" validate:"dive,required"`
	AllAccounts bool     `mapstructure:"all_accounts"`
}

// IsEnabled reports whether transport audit lines are written.
func (a MCPAudit) IsEnabled() bool { return a.Enabled == nil || *a.Enabled }

// GetAddress returns the listen address or the loopback default.
func (c MCP) GetAddress() string { return cmpString(c.Address, defaultMCPAddress) }

// GetTLSTermination returns the TLS termination mode.
func (c MCP) GetTLSTermination() string {
	return cmpString(c.TLSTermination, MCPTLSTerminationTrustedReverseProxy)
}

// GetTimeout returns the per-call time budget.
func (l MCPLimits) GetTimeout() time.Duration { return positiveOr(l.Timeout, defaultMCPTimeout) }

// GetMaxConcurrent returns the in-flight request bound of the endpoint.
func (l MCPLimits) GetMaxConcurrent() int {
	return positiveOr(l.MaxConcurrent, defaultMCPMaxConcurrent)
}

// GetMaxConcurrentPerPrincipal returns the in-flight bound of one person.
func (l MCPLimits) GetMaxConcurrentPerPrincipal() int {
	return positiveOr(l.MaxConcurrentPerPrincipal, defaultMCPMaxConcurrentPerPrincipal)
}

// GetMaxRequestBytes returns the request body bound.
func (l MCPLimits) GetMaxRequestBytes() int64 {
	return positiveOr(l.MaxRequestBytes, defaultMCPMaxRequestBytes)
}

// GetMaxResponseBytes returns the bound of one tool result (structured
// content and its text copy together).
func (l MCPLimits) GetMaxResponseBytes() int64 {
	return positiveOr(l.MaxResponseBytes, defaultMCPMaxResponseBytes)
}

// GetFailedAuthPerMinute returns the failed bearer attempts one client
// address may make per minute.
func (l MCPLimits) GetFailedAuthPerMinute() int {
	return positiveOr(l.FailedAuthPerMinute, defaultMCPFailedAuthPerMinute)
}

// GetScopes returns the scopes that grant the tools.
func (o MCPOAuth) GetScopes() []string {
	if len(o.Scopes) == 0 {
		return []string{DefaultMCPScope}
	}

	return slices.Clone(o.Scopes)
}

// GetPrincipalClaim returns the claim a principal identity is matched with.
func (o MCPOAuth) GetPrincipalClaim() string {
	return cmpString(o.PrincipalClaim, defaultMCPPrincipalClaim)
}

// GetGroupsClaim returns the claim a principal group is matched with.
func (o MCPOAuth) GetGroupsClaim() string { return cmpString(o.GroupsClaim, defaultMCPGroupsClaim) }

// MCPIntrospection is the effective introspection endpoint and client.
type MCPIntrospection struct {
	URL          string
	ClientID     string
	ClientSecret string
}

// MCPIntrospection returns the endpoint and client the MCP endpoint
// introspects with: its own, or those of the nauthilus section (its
// introspection client, or the service client that one falls back to).
func (c *Config) MCPIntrospection() MCPIntrospection {
	o, n := c.MCP.OAuth, c.Nauthilus
	in := MCPIntrospection{URL: cmpString(o.IntrospectURL, n.IntrospectURL)}

	switch {
	case o.IntrospectClientID != "":
		in.ClientID, in.ClientSecret = o.IntrospectClientID, o.IntrospectClientSecret
	case n.IntrospectClientID != "":
		in.ClientID, in.ClientSecret = n.IntrospectClientID, n.IntrospectClientSecret
	default:
		in.ClientID, in.ClientSecret = n.OidcClientID, n.OidcClientSecret
	}

	return in
}

// cmpString returns value unless it is empty.
func cmpString(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}

	return value
}

// positiveOr returns value when positive, otherwise fallback.
func positiveOr[T int | int64 | time.Duration](value, fallback T) T {
	if value <= 0 {
		return fallback
	}

	return value
}

// checkMCP rejects an MCP configuration that could only fail at runtime.
// A disabled endpoint is not checked.
func (c *Config) checkMCP() error {
	m := c.MCP
	if !m.Enabled {
		return nil
	}

	resource, err := checkMCPOAuth(m.OAuth)
	if err != nil {
		return err
	}

	in := c.MCPIntrospection()
	if !secureURL(in.URL) {
		return fmt.Errorf("%s.oauth.introspect_url (or nauthilus.introspect_url) must be an https URL (http only on loopback)", mcpPath)
	}

	if m.OAuth.IntrospectClientID == "" && m.OAuth.IntrospectClientSecret != "" {
		return fmt.Errorf("%s.oauth.introspect_client_secret needs %s.oauth.introspect_client_id", mcpPath, mcpPath)
	}

	if in.ClientID == "" || in.ClientSecret == "" {
		return fmt.Errorf("%s.oauth needs an introspection client with its secret (its own or that of the nauthilus section)", mcpPath)
	}

	for _, check := range []func(MCP, *url.URL) error{checkMCPListener, checkMCPProxies, checkMCPHostsAndOrigins, checkMCPLimits, checkMCPPrincipals} {
		if err := check(m, resource); err != nil {
			return err
		}
	}

	return nil
}

// checkMCPOAuth checks the resource, the issuer, the scopes and the
// clients, and returns the parsed resource.
func checkMCPOAuth(o MCPOAuth) (*url.URL, error) {
	resource, err := checkMCPResource(o.Resource)
	if err != nil {
		return nil, err
	}

	if !secureURL(o.Issuer) {
		return nil, fmt.Errorf("%s.oauth.issuer must be an https URL (http only on loopback)", mcpPath)
	}

	for i, s := range o.GetScopes() {
		if s == "" || strings.ContainsAny(s, " \t\"\\") {
			return nil, fmt.Errorf("%s.oauth.scopes[%d] must be a scope token", mcpPath, i)
		}
	}

	if len(o.StaticClients) == 0 || slices.Contains(o.StaticClients, "") {
		return nil, fmt.Errorf("%s.oauth.static_clients must name the admitted clients", mcpPath)
	}

	return resource, nil
}

// checkMCPResource parses the resource: an absolute http(s) URL without query or fragment.
// Its path becomes a route of the listener, so it is limited to plain path characters.
func checkMCPResource(raw string) (*url.URL, error) {
	resource, err := url.Parse(raw)
	if err == nil && resource.IsAbs() && resource.Host != "" && resource.User == nil && resource.Fragment == "" &&
		resource.RawQuery == "" && !resource.ForceQuery && (resource.Scheme == SchemeHTTPS || resource.Scheme == SchemeHTTP) &&
		strings.Trim(resource.Path, mcpPathChars) == "" && resource.RawPath == "" {
		return resource, nil
	}

	return nil, fmt.Errorf("%s.oauth.resource must be an absolute http(s) URL without user, query or fragment, "+
		"with a path of letters, digits and -._~/", mcpPath)
}

// mcpPathChars are the characters a resource path may contain.
const mcpPathChars = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-._~/"

// secureURL reports whether raw is an absolute https URL, or an http URL of a loopback host.
func secureURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || !u.IsAbs() || u.Host == "" {
		return false
	}

	return u.Scheme == SchemeHTTPS || (u.Scheme == SchemeHTTP && isLoopbackHost(u.Hostname()))
}

// isLoopbackHost reports whether a host is localhost or a loopback address.
func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}

	addr, err := netip.ParseAddr(host)

	return err == nil && addr.IsLoopback()
}

// checkMCPListener requires a host:port address and the supported TLS
// termination.
func checkMCPListener(m MCP, _ *url.URL) error {
	if err := checkListener(m.GetAddress()); err != nil {
		return fmt.Errorf("%s.address must be host:port", mcpPath)
	}

	if m.GetTLSTermination() != MCPTLSTerminationTrustedReverseProxy {
		return fmt.Errorf("%s.tls_termination must be %s", mcpPath, MCPTLSTerminationTrustedReverseProxy)
	}

	return nil
}

// checkMCPProxies requires trusted proxies for an https resource; plain
// http is served only to a loopback peer of a loopback resource.
func checkMCPProxies(m MCP, resource *url.URL) error {
	for i, entry := range m.TrustedProxies {
		if _, err := netip.ParsePrefix(entry); err == nil {
			continue
		}

		if _, err := netip.ParseAddr(entry); err != nil {
			return fmt.Errorf("%s.trusted_proxies[%d] must be an IP address or CIDR", mcpPath, i)
		}
	}

	if len(m.TrustedProxies) == 0 && (resource.Scheme == SchemeHTTPS || !isLoopbackHost(resource.Hostname())) {
		return fmt.Errorf("%s.trusted_proxies must list the reverse proxy for an https or non-loopback resource", mcpPath)
	}

	return nil
}

// checkMCPHostsAndOrigins requires plain host values and absolute
// browser origins.
func checkMCPHostsAndOrigins(m MCP, _ *url.URL) error {
	for i, host := range m.AllowedHosts {
		if host == "" || strings.ContainsAny(host, "/ @") {
			return fmt.Errorf("%s.allowed_hosts[%d] must be a host or host:port", mcpPath, i)
		}
	}

	for i, origin := range m.AllowedBrowserOrigins {
		parsed, err := url.Parse(origin)
		if err != nil || parsed.Scheme == "" || parsed.Host == "" || (parsed.Path != "" && parsed.Path != "/") ||
			parsed.RawQuery != "" || slices.Contains([]string{"*", "null"}, origin) {
			return fmt.Errorf("%s.allowed_browser_origins[%d] must be an origin such as https://host", mcpPath, i)
		}
	}

	return nil
}

// checkMCPLimits keeps every limit inside its ceiling.
func checkMCPLimits(m MCP, _ *url.URL) error {
	l := m.Limits
	p := mcpPath + ".limits"

	if l.FailedAuthPerMinute < 0 {
		return fmt.Errorf("%s.failed_auth_per_minute must not be negative", p)
	}

	switch {
	case l.Timeout < 0 || l.GetTimeout() > maxMCPTimeout:
		return fmt.Errorf("%s.timeout must be at most 1m", p)
	case l.MaxConcurrent < 0 || l.GetMaxConcurrent() > maxMCPMaxConcurrent:
		return fmt.Errorf("%s.max_concurrent must be at most %d", p, maxMCPMaxConcurrent)
	case l.MaxConcurrentPerPrincipal < 0 || l.GetMaxConcurrentPerPrincipal() > l.GetMaxConcurrent():
		return fmt.Errorf("%s.max_concurrent_per_principal must not exceed max_concurrent", p)
	case l.MaxRequestBytes < 0 || l.GetMaxRequestBytes() > maxMCPMaxRequestBytes:
		return fmt.Errorf("%s.max_request_bytes must be at most %d", p, maxMCPMaxRequestBytes)
	case l.MaxResponseBytes < 0 || !within(l.GetMaxResponseBytes(), minMCPMaxResponseBytes, maxMCPMaxResponseBytes):
		return fmt.Errorf("%s.max_response_bytes must be between %d and %d", p, minMCPMaxResponseBytes, maxMCPMaxResponseBytes)
	default:
		return nil
	}
}

// within reports whether value lies in [low, high].
func within(value, low, high int64) bool { return value >= low && value <= high }

// checkMCPPrincipals requires at least one principal, each naming
// exactly one identity or group, with valid account patterns.
func checkMCPPrincipals(m MCP, _ *url.URL) error {
	if len(m.Principals) == 0 {
		return fmt.Errorf("%s.principals must grant at least one person or group", mcpPath)
	}

	seen := map[string]bool{}

	for i, p := range m.Principals {
		if (p.Identity == "") == (p.Group == "") {
			return fmt.Errorf("%s.principals[%d] must name exactly one of identity and group", mcpPath, i)
		}

		key := "i:" + p.Identity
		if p.Group != "" {
			key = "g:" + p.Group
		}

		if seen[key] {
			return fmt.Errorf("%s.principals[%d] is listed twice", mcpPath, i)
		}

		seen[key] = true

		if p.AllAccounts && len(p.Accounts) > 0 {
			return fmt.Errorf("%s.principals[%d]: all_accounts and accounts exclude each other", mcpPath, i)
		}

		for _, pattern := range p.Accounts {
			if _, err := path.Match(pattern, ""); err != nil {
				return fmt.Errorf("%s.principals[%d]: invalid account pattern %q", mcpPath, i, pattern)
			}
		}
	}

	return nil
}
