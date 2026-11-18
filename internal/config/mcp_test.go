package config

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// mcpBase is a complete MCP section behind a TLS proxy, with the
// introspection client of the nauthilus section.
const mcpBase = `nauthilus:
  introspect_url: https://idp.example.org/oidc/introspect
  introspect_client_id: shardpost
  introspect_client_secret: s
mcp:
  enabled: true
  address: 0.0.0.0:9097
  trusted_proxies: [10.0.0.0/8]
  limits:
    timeout: 20s
    max_concurrent: 16
  oauth:
    resource: https://mcp.example.org/mcp
    issuer: https://idp.example.org
    static_clients: [claude]
  principals:
    - identity: anna
      roles: [diagnose]
      accounts: ["*@example.org"]
    - group: mail-support
      roles: [diagnose]
      all_accounts: true
`

// TestMCPLoads pins the keys, the Nauthilus-style durations, the defaults
// and the introspection fallback.
func TestMCPLoads(t *testing.T) {
	c, err := Load(configFile(t, mcpBase))
	if err != nil {
		t.Fatal(err)
	}

	m := c.MCP
	got := []any{m.Enabled, m.GetAddress(), m.Limits.GetTimeout(), m.Limits.GetMaxConcurrent(), m.Limits.GetMaxConcurrentPerPrincipal(),
		m.Limits.GetMaxRequestBytes(), m.Audit.IsEnabled(), m.GetTLSTermination()}
	want := []any{true, "0.0.0.0:9097", 20 * time.Second, 16, 2, int64(64 << 10), true, MCPTLSTerminationTrustedReverseProxy}

	if !slices.Equal(got, want) {
		t.Fatalf("mcp = %v, want %v", got, want)
	}

	if scopes := m.OAuth.GetScopes(); len(scopes) != 1 || scopes[0] != DefaultMCPScope {
		t.Fatalf("scopes = %v", scopes)
	}

	if in := c.MCPIntrospection(); in.URL != "https://idp.example.org/oidc/introspect" || in.ClientID != "shardpost" || in.ClientSecret != "s" {
		t.Fatalf("introspection = %+v", in)
	}
}

// TestMCPOwnIntrospectionSecretFile pins the endpoint's own client and
// its secret file.
func TestMCPOwnIntrospectionSecretFile(t *testing.T) {
	secret := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(secret, []byte("own\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	body := strings.Replace(mcpBase, "    static_clients: [claude]\n", "    static_clients: [claude]\n"+
		"    introspect_client_id: shardpost-mcp\n    introspect_client_secret_file: "+secret+"\n", 1)

	c, err := Load(configFile(t, body))
	if err != nil {
		t.Fatal(err)
	}

	if in := c.MCPIntrospection(); in.ClientID != "shardpost-mcp" || in.ClientSecret != "own" {
		t.Fatalf("introspection = %+v", in)
	}

	// The runner serves no MCP endpoint: its secret file may be missing.
	if _, err := LoadRunner(configFile(t, strings.Replace(body, secret, secret+".missing", 1))); err != nil {
		t.Fatalf("runner = %v", err)
	}
}

// TestMCPRejectsInvalid pins the load-time checks.
func TestMCPRejectsInvalid(t *testing.T) {
	for name, change := range map[string][2]string{
		"no resource":          {"    resource: https://mcp.example.org/mcp\n", ""},
		"relative resource":    {"https://mcp.example.org/mcp", "/mcp"},
		"resource fragment":    {"https://mcp.example.org/mcp", "https://mcp.example.org/mcp#x"},
		"no issuer":            {"    issuer: https://idp.example.org\n", ""},
		"no clients":           {"    static_clients: [claude]\n", ""},
		"https without proxy":  {"  trusted_proxies: [10.0.0.0/8]\n", ""},
		"bad proxy":            {"10.0.0.0/8", "proxy"},
		"bad address":          {"0.0.0.0:9097", "nowhere"},
		"tls termination":      {"  enabled: true\n", "  enabled: true\n  tls_termination: listener\n"},
		"timeout ceiling":      {"timeout: 20s", "timeout: 2m"},
		"timeout no duration":  {"timeout: 20s", "timeout: 20"},
		"per principal":        {"max_concurrent: 16", "max_concurrent: 1\n    max_concurrent_per_principal: 2"},
		"write role":           {"      roles: [diagnose]\n      accounts", "      roles: [delete]\n      accounts"},
		"identity and group":   {"    - identity: anna\n", "    - identity: anna\n      group: x\n"},
		"all and accounts":     {"      all_accounts: true\n", "      all_accounts: true\n      accounts: [a]\n"},
		"no introspection url": {"  introspect_url: https://idp.example.org/oidc/introspect\n", ""},
		"bad origin":           {"  trusted_proxies:", "  allowed_browser_origins: ['*']\n  trusted_proxies:"},
		"unknown key":          {"  enabled: true\n", "  enabled: true\n  hosted_resource: x\n"},
		"route characters":     {"https://mcp.example.org/mcp", "https://mcp.example.org/{mcp}"},
		"resource user":        {"https://mcp.example.org/mcp", "https://u@mcp.example.org/mcp"},
		"http issuer":          {"issuer: https://idp.example.org", "issuer: http://idp.example.org"},
		"http introspection":   {"introspect_url: https://idp", "introspect_url: http://idp"},
		"tiny response bound":  {"max_concurrent: 16", "max_concurrent: 16\n    max_response_bytes: 1"},
		"secret without id":    {"    static_clients: [claude]\n", "    static_clients: [claude]\n    introspect_client_secret: x\n"},
	} {
		t.Run(name, func(t *testing.T) {
			body := strings.Replace(mcpBase, change[0], change[1], 1)
			if body == mcpBase {
				t.Fatalf("the change %q does not apply", change[0])
			}

			if _, err := Load(configFile(t, body)); err == nil {
				t.Fatal("accepted an invalid mcp section")
			}
		})
	}

	if _, err := Load(configFile(t, mcpBase[:strings.Index(mcpBase, "  principals:")])); err == nil ||
		!strings.Contains(err.Error(), "principals") {
		t.Fatalf("no principals = %v", err)
	}

	noProxy := strings.Replace(mcpBase, "  trusted_proxies: [10.0.0.0/8]\n", "", 1)
	if _, err := Load(configFile(t, strings.Replace(noProxy, "https://mcp.example.org/mcp", "http://mcp.example.org/mcp", 1))); err == nil {
		t.Fatal("accepted a non-loopback http resource without trusted proxies")
	}

	if _, err := Load(configFile(t, strings.Replace(noProxy, "https://mcp.example.org/mcp", "http://127.0.0.1:9097/mcp", 1))); err != nil {
		t.Fatalf("loopback http resource = %v", err)
	}

	// A disabled section is not checked.
	if _, err := Load(configFile(t, "mcp:\n  enabled: false\n  oauth:\n    resource: relative\n")); err != nil {
		t.Fatalf("disabled = %v", err)
	}
}
