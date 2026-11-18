package mcpserver

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"go.yaml.in/yaml/v3"

	adminspec "shardpost/api/admin"
	"shardpost/internal/admin"
	"shardpost/internal/config"
)

const (
	testResource = "https://mcp.example.test/mcp"
	testIssuer   = "https://login.example.test"
	testHost     = "mcp.example.test"
	testProxy    = "192.0.2.10"
	staticClient = "claude"
	testScope    = config.DefaultMCPScope
)

// fakeIntrospector answers from a fixed token table.
type fakeIntrospector struct {
	tokens map[string]map[string]any
}

// Introspect implements Introspector.
func (f fakeIntrospector) Introspect(_ context.Context, token string) (map[string]any, error) {
	if token == "unavailable" {
		return nil, ErrIntrospectionUnavailable
	}

	claims, ok := f.tokens[token]
	if !ok {
		return nil, ErrTokenInactive
	}

	return claims, nil
}

// claims returns the claims of an active user access token.
func claims(subject, client string, audience []any, scope string, change func(map[string]any)) map[string]any {
	out := map[string]any{"active": true, "iss": testIssuer, "sub": subject, "azp": client, "aud": audience, "scope": scope,
		"exp": json.Number("4102444800"), "groups": []any{"mail-support"}}
	if change != nil {
		change(out)
	}

	return out
}

// newIntrospector returns the standard token table.
func newIntrospector() fakeIntrospector {
	pair := []any{staticClient, testResource}

	return fakeIntrospector{tokens: map[string]map[string]any{
		"good":           claims("anna", staticClient, pair, "openid "+testScope, func(c map[string]any) { delete(c, "groups") }),
		"group-member":   claims("bert", staticClient, pair, testScope, nil),
		"stranger":       claims("carl", staticClient, pair, testScope, func(c map[string]any) { c["groups"] = []any{"other"} }),
		"no-scope":       claims("anna", staticClient, pair, "openid", nil),
		"wrong-issuer":   claims("anna", staticClient, pair, testScope, func(c map[string]any) { c["iss"] = "https://evil.test" }),
		"wrong-audience": claims("anna", staticClient, []any{staticClient, "https://mail.example.test/"}, testScope, nil),
		"extra-audience": claims("anna", staticClient, []any{staticClient, testResource, "https://x.test/"}, testScope, nil),
		"client-only":    claims("anna", staticClient, []any{staticClient}, testScope, nil),
		"unlisted":       claims("anna", "other", []any{"other", testResource}, testScope, nil),
		"expired":        claims("anna", staticClient, pair, testScope, func(c map[string]any) { c["exp"] = json.Number("1") }),
		"no-expiry":      claims("anna", staticClient, pair, testScope, func(c map[string]any) { delete(c, "exp") }),
		"service":        claims("anna", staticClient, pair, testScope, func(c map[string]any) { c["client_id"] = staticClient }),
		"no-subject":     claims("", staticClient, pair, testScope, nil),
	}}
}

// fakeAdmin records the calls and answers with a fixed body.
type fakeAdmin struct {
	mu      sync.Mutex
	calls   []string
	callers []admin.Caller
	status  int
	body    string
	// block, when set, holds every call until it is closed.
	block chan struct{}
}

// ServeAs implements AdminAPI.
func (f *fakeAdmin) ServeAs(w http.ResponseWriter, r *http.Request, c admin.Caller) {
	f.mu.Lock()
	f.calls = append(f.calls, r.URL.RequestURI())
	f.callers = append(f.callers, c)
	f.mu.Unlock()

	if f.block != nil {
		<-f.block
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(cmpStatus(f.status))
	_, _ = io.WriteString(w, f.body)
}

// cmpStatus defaults a status to 200.
func cmpStatus(status int) int {
	if status == 0 {
		return http.StatusOK
	}

	return status
}

// testSettings is an enabled endpoint behind the test proxy.
func testSettings() config.MCP {
	return config.MCP{Enabled: true, TrustedProxies: []string{testProxy + "/32"},
		OAuth: config.MCPOAuth{Resource: testResource, Issuer: testIssuer, StaticClients: []string{staticClient}},
		Principals: []config.MCPPrincipal{
			{Identity: "anna", Roles: []string{admin.RoleDiagnose}, Accounts: []string{"*@example.org"}},
			{Group: "mail-support", Roles: []string{admin.RoleDiagnose}, AllAccounts: true},
		},
		Limits: config.MCPLimits{FailedAuthPerMinute: 3}}
}

// newTestServer builds the endpoint over the fakes.
func newTestServer(t *testing.T, api AdminAPI, adjust func(*config.MCP)) *Server {
	t.Helper()

	settings := testSettings()
	if adjust != nil {
		adjust(&settings)
	}

	server, err := New(Deps{Settings: settings, Tokens: newIntrospector(), Admin: api, Version: "test"})
	if err != nil {
		t.Fatalf("New() = %v", err)
	}

	return server
}

// proxied returns a request as forwarded by the trusted TLS proxy.
func proxied(method string, path string, body string, token string) *http.Request {
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	request.RemoteAddr = testProxy + ":40000"
	request.Host = testHost
	request.Header.Set(headerForwardedProto, "https")
	request.Header.Set(headerForwardedFor, "203.0.113.7")
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json, text/event-stream")

	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}

	return request
}

// serve runs one request against the endpoint.
func serve(server *Server, request *http.Request) *httptest.ResponseRecorder {
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, request)

	return recorder
}

// toolCall renders a tools/call JSON-RPC request.
func toolCall(name string, arguments string) string {
	return `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"` + name + `","arguments":` + arguments + `}}`
}

// TestTransportGuard pins the TLS-proxy boundary, the Host allowlist, the Origin deny gate, and the method filter.
func TestTransportGuard(t *testing.T) {
	server := newTestServer(t, &fakeAdmin{}, nil)

	for name, tc := range map[string]struct {
		mutate func(*http.Request)
		status int
	}{
		"direct untrusted peer": {func(r *http.Request) { r.RemoteAddr = "198.51.100.1:5000" }, http.StatusForbidden},
		"proxy without https":   {func(r *http.Request) { r.Header.Set(headerForwardedProto, "http") }, http.StatusForbidden},
		"foreign host":          {func(r *http.Request) { r.Host = "evil.example.test" }, http.StatusForbidden},
		"foreign origin":        {func(r *http.Request) { r.Header.Set(headerOrigin, "https://evil.example.test") }, http.StatusForbidden},
		"options method":        {func(r *http.Request) { r.Method = http.MethodOptions }, http.StatusMethodNotAllowed},
		"accepted transport":    {func(*http.Request) {}, http.StatusUnauthorized},
	} {
		t.Run(name, func(t *testing.T) {
			request := proxied(http.MethodPost, "/mcp", `{}`, "")
			tc.mutate(request)

			if recorder := serve(server, request); recorder.Code != tc.status {
				t.Fatalf("status = %d body = %s, want %d", recorder.Code, recorder.Body.String(), tc.status)
			}
		})
	}

	if serve(server, proxied(http.MethodGet, "/other", "", "good")).Code != http.StatusNotFound {
		t.Fatal("unknown paths must not be served")
	}
}

// TestForwardedClientIgnoresSpoofedHops pins that only the right-most untrusted X-Forwarded-For hop is the client.
func TestForwardedClientIgnoresSpoofedHops(t *testing.T) {
	server := newTestServer(t, &fakeAdmin{}, func(s *config.MCP) { s.TrustedProxies = []string{testProxy + "/32", "10.0.0.0/8"} })

	for name, tc := range map[string]struct {
		values []string
		want   string
		ok     bool
	}{
		"spoofed left hop": {[]string{"1.2.3.4, 203.0.113.7"}, "203.0.113.7", true},
		"internal hops":    {[]string{"203.0.113.7, 10.1.1.1", "10.2.2.2"}, "203.0.113.7", true},
		"hop with port":    {[]string{"203.0.113.7:51234"}, "203.0.113.7", true},
		"ipv6 with port":   {[]string{"[2001:db8::7]:443"}, "2001:db8::7", true},
		"garbage hop":      {[]string{"203.0.113.7, not-an-ip"}, "", false},
		"only proxies":     {[]string{"10.1.1.1"}, "", false},
		"no header":        {nil, "", false},
	} {
		t.Run(name, func(t *testing.T) {
			got, ok := server.forwardedClient(tc.values)
			if ok != tc.ok || (ok && got.String() != tc.want) {
				t.Fatalf("forwardedClient(%v) = %v, %v; want %s, %v", tc.values, got, ok, tc.want, tc.ok)
			}
		})
	}
}

// TestBearerVerification pins which tokens reach a tool and how the others are answered.
func TestBearerVerification(t *testing.T) {
	server := newTestServer(t, &fakeAdmin{body: `{}`}, func(s *config.MCP) { s.Limits.FailedAuthPerMinute = 100 })

	for token, status := range map[string]int{
		"":               http.StatusUnauthorized,
		"inactive":       http.StatusUnauthorized,
		"wrong-issuer":   http.StatusUnauthorized,
		"wrong-audience": http.StatusUnauthorized,
		"extra-audience": http.StatusUnauthorized,
		"client-only":    http.StatusUnauthorized,
		"unlisted":       http.StatusUnauthorized,
		"expired":        http.StatusUnauthorized,
		"no-expiry":      http.StatusUnauthorized,
		"service":        http.StatusUnauthorized,
		"no-subject":     http.StatusUnauthorized,
		"no-scope":       http.StatusForbidden,
		"stranger":       http.StatusForbidden,
		"unavailable":    http.StatusServiceUnavailable,
		"good":           http.StatusOK,
		"group-member":   http.StatusOK,
	} {
		t.Run(token, func(t *testing.T) {
			recorder := serve(server, proxied(http.MethodPost, "/mcp", toolCall("server_version", `{}`), token))
			if recorder.Code != status {
				t.Fatalf("status = %d body = %s, want %d", recorder.Code, recorder.Body.String(), status)
			}

			challenge := recorder.Header().Get("WWW-Authenticate")
			if status == http.StatusUnauthorized && !strings.Contains(challenge,
				`resource_metadata="https://mcp.example.test/.well-known/oauth-protected-resource/mcp"`) {
				t.Fatalf("WWW-Authenticate = %q", challenge)
			}

			if token == "no-scope" && !strings.Contains(challenge, `error="insufficient_scope", scope="`+testScope+`"`) {
				t.Fatalf("WWW-Authenticate = %q", challenge)
			}
		})
	}
}

// TestFailedBearerLimiter pins that failed tokens of one client are bounded per window.
func TestFailedBearerLimiter(t *testing.T) {
	server := newTestServer(t, &fakeAdmin{}, nil)

	for attempt := range 3 {
		if code := serve(server, proxied(http.MethodPost, "/mcp", `{}`, "forged")).Code; code != http.StatusUnauthorized {
			t.Fatalf("attempt %d = %d", attempt, code)
		}
	}

	if code := serve(server, proxied(http.MethodPost, "/mcp", `{}`, "good")).Code; code != http.StatusTooManyRequests {
		t.Fatalf("after the limit = %d, want 429 even for a valid token", code)
	}

	other := proxied(http.MethodPost, "/mcp", `{}`, "forged")
	other.Header.Set(headerForwardedFor, "203.0.113.8")

	if code := serve(server, other).Code; code != http.StatusUnauthorized {
		t.Fatalf("another client = %d, want 401", code)
	}

	limiter := newFailureLimiter(1, time.Minute, func() time.Time { return time.Unix(0, 0) })
	client := netip.MustParseAddr("203.0.113.9")
	limiter.fail(client)

	if !limiter.blocked(client) {
		t.Fatal("limiter must block after the limit")
	}

	limiter.now = func() time.Time { return time.Unix(61, 0) }

	if limiter.blocked(client) {
		t.Fatal("limiter must reset after the window")
	}
}

// TestProtectedResourceMetadata pins the RFC 9728 document at both locations.
func TestProtectedResourceMetadata(t *testing.T) {
	server := newTestServer(t, &fakeAdmin{}, nil)

	for _, path := range []string{"/.well-known/oauth-protected-resource/mcp", "/.well-known/oauth-protected-resource"} {
		recorder := serve(server, proxied(http.MethodGet, path, "", ""))
		if recorder.Code != http.StatusOK {
			t.Fatalf("%s = %d", path, recorder.Code)
		}

		var document struct {
			Resource             string   `json:"resource"`
			AuthorizationServers []string `json:"authorization_servers"`
			Scopes               []string `json:"scopes_supported"`
			BearerMethods        []string `json:"bearer_methods_supported"`
		}

		if err := json.Unmarshal(recorder.Body.Bytes(), &document); err != nil {
			t.Fatal(err)
		}

		if document.Resource != testResource || !slices.Equal(document.AuthorizationServers, []string{testIssuer}) ||
			!slices.Equal(document.Scopes, []string{testScope}) || !slices.Equal(document.BearerMethods, []string{"header"}) {
			t.Fatalf("%s = %+v", path, document)
		}
	}
}

// TestBodyInspection pins the refusals before the SDK.
func TestBodyInspection(t *testing.T) {
	server := newTestServer(t, &fakeAdmin{}, func(s *config.MCP) { s.Limits.MaxRequestBytes = 512 })

	for name, tc := range map[string]struct {
		body    string
		headers map[string]string
		status  int
	}{
		"duplicate names":     {`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"a","name":"b"}}`, nil, http.StatusBadRequest},
		"batch":               {`[` + toolCall("server_version", `{}`) + `]`, nil, http.StatusBadRequest},
		"oversized":           {toolCall("server_version", `{"x":"`+strings.Repeat("a", 600)+`"}`), nil, http.StatusRequestEntityTooLarge},
		"method mismatch":     {toolCall("server_version", `{}`), map[string]string{headerMCPMethod: "tools/list"}, http.StatusBadRequest},
		"name mismatch":       {toolCall("server_version", `{}`), map[string]string{headerMCPName: "jobs_list"}, http.StatusBadRequest},
		"method case variant": {`{"jsonrpc":"2.0","id":1,"method":"tools/call","Method":"ping","params":{"name":"server_version"}}`, nil, http.StatusBadRequest},
		"name case variant":   {`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"jobs_list","Name":"server_version"}}`, nil, http.StatusBadRequest},
	} {
		t.Run(name, func(t *testing.T) {
			request := proxied(http.MethodPost, "/mcp", tc.body, "good")
			for key, value := range tc.headers {
				request.Header.Set(key, value)
			}

			if recorder := serve(server, request); recorder.Code != tc.status {
				t.Fatalf("status = %d body = %s, want %d", recorder.Code, recorder.Body.String(), tc.status)
			}
		})
	}
}

// proxyTransport adds the bearer token and the trusted proxy headers to every SDK client request.
type proxyTransport struct {
	token string
}

// RoundTrip forwards the request as a trusted TLS-terminating proxy on the loopback address would.
func (p proxyTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	clone := request.Clone(request.Context())
	clone.Host = testHost
	clone.Header.Set("Authorization", "Bearer "+p.token)
	clone.Header.Set("X-Forwarded-Proto", "https")
	clone.Header.Set("X-Forwarded-For", "203.0.113.50")

	return http.DefaultTransport.RoundTrip(clone)
}

// connect runs an SDK client against the endpoint with the given protocol version.
func connect(t *testing.T, server *Server, token string, version string) *mcp.ClientSession {
	t.Helper()

	listener := httptest.NewServer(server.Handler())
	t.Cleanup(listener.Close)

	client := mcp.NewClient(&mcp.Implementation{Name: "shardpost-test", Version: "1"}, nil)

	session, err := client.Connect(context.Background(), &mcp.StreamableClientTransport{Endpoint: listener.URL + "/mcp",
		HTTPClient: &http.Client{Transport: proxyTransport{token: token}}}, &mcp.ClientSessionOptions{ProtocolVersion: version})
	if err != nil {
		t.Fatalf("Connect(%s) = %v", version, err)
	}

	t.Cleanup(func() { _ = session.Close() })

	return session
}

// loopbackTrusted makes the test listener's loopback peer the trusted proxy.
func loopbackTrusted(s *config.MCP) { s.TrustedProxies = []string{"127.0.0.1/32", "::1/128"} }

// TestProtocolErasListAndCallTools pins that every protocol era lists the read-only tools and runs a call.
func TestProtocolErasListAndCallTools(t *testing.T) {
	api := &fakeAdmin{body: `{"version":"1.0"}`}
	server := newTestServer(t, api, loopbackTrusted)

	for _, version := range supportedProtocolVersions {
		t.Run(version, func(t *testing.T) {
			session := connect(t, server, "good", version)

			listed, err := session.ListTools(context.Background(), nil)
			if err != nil {
				t.Fatal(err)
			}

			if len(listed.Tools) != len(tools) {
				t.Fatalf("tools = %d, want %d", len(listed.Tools), len(tools))
			}

			for _, listedTool := range listed.Tools {
				if listedTool.Annotations == nil || !listedTool.Annotations.ReadOnlyHint || *listedTool.Annotations.DestructiveHint ||
					!strings.Contains(listedTool.Description, "untrusted data") {
					t.Fatalf("tool %s annotations = %+v", listedTool.Name, listedTool.Annotations)
				}
			}

			result, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: "server_version"})
			if err != nil || result.IsError || !strings.Contains(result.Content[0].(*mcp.TextContent).Text, `"version":"1.0"`) {
				t.Fatalf("CallTool = %+v, %v", result, err)
			}
		})
	}
}

// TestToolCallsReachTheAdminAPI pins the request a call becomes and the caller the principals grant.
func TestToolCallsReachTheAdminAPI(t *testing.T) {
	api := &fakeAdmin{body: `[{"name":"INBOX"}]`}
	server := newTestServer(t, api, loopbackTrusted)
	session := connect(t, server, "good", "2025-06-18")

	result, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: "mailbox_acl_explain",
		Arguments: map[string]any{"account": "anna@example.org", "id": "a/b", "principal": "bert@example.org"}})
	if err != nil || result.IsError {
		t.Fatalf("CallTool = %+v, %v", result, err)
	}

	if text := result.Content[0].(*mcp.TextContent).Text; text != `{"result":[{"name":"INBOX"}]}` {
		t.Fatalf("a list answer must be wrapped, got %s", text)
	}

	jobs, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: "jobs_list",
		Arguments: map[string]any{"limit": 5, "state": "running"}})
	if err != nil || jobs.IsError {
		t.Fatalf("jobs_list = %+v, %v", jobs, err)
	}

	want := []string{"/admin/v1/accounts/anna@example.org/mailboxes/a%2Fb/acl/explain?principal=bert%40example.org",
		"/admin/v1/jobs?limit=5&state=running"}
	if !slices.Equal(api.calls, want) {
		t.Fatalf("calls = %v, want %v", api.calls, want)
	}

	caller := api.callers[0]
	if caller.Name != "mcp:claude#anna" || caller.Transport != "mcp" || !slices.Equal(caller.Roles, []string{admin.RoleDiagnose}) ||
		!slices.Equal(caller.Accounts, []string{"*@example.org"}) || caller.AllAccounts {
		t.Fatalf("caller = %+v", caller)
	}
}

// TestInvalidArgumentsNeverReachTheAdminAPI pins that arguments outside the input schema are refused before the
// admin API, whether the SDK's schema validation or the endpoint's own decoding refuses them.
func TestInvalidArgumentsNeverReachTheAdminAPI(t *testing.T) {
	api := &fakeAdmin{body: `{}`}
	session := connect(t, newTestServer(t, api, loopbackTrusted), "good", "2025-06-18")

	for name, arguments := range map[string]map[string]any{
		"missing path parameter": {},
		"omitted argument":       {"account": "anna@example.org", "exact": true},
		"wrong type":             {"account": 7},
	} {
		refused, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: "account_fts_stats", Arguments: arguments})
		if err == nil && !refused.IsError {
			t.Fatalf("%s must be refused, got %+v", name, refused)
		}
	}

	if len(api.calls) != 0 {
		t.Fatalf("refused calls must not reach the admin API: %v", api.calls)
	}
}

// TestGroupPrincipalGrantsAllAccounts pins the union of a group grant.
func TestGroupPrincipalGrantsAllAccounts(t *testing.T) {
	api := &fakeAdmin{body: `{}`}
	server := newTestServer(t, api, loopbackTrusted)

	if _, err := connect(t, server, "group-member", "2025-06-18").CallTool(context.Background(),
		&mcp.CallToolParams{Name: "accounts_list"}); err != nil {
		t.Fatal(err)
	}

	if caller := api.callers[0]; !caller.AllAccounts || caller.Accounts != nil || caller.Name != "mcp:claude#bert" {
		t.Fatalf("caller = %+v", caller)
	}
}

// TestAdminErrorsAndResponseCap pins that an admin API error is a tool error and that a large result is replaced.
func TestAdminErrorsAndResponseCap(t *testing.T) {
	api := &fakeAdmin{status: http.StatusForbidden, body: `{"code":"forbidden","message":"account outside the client's scope"}`}
	server := newTestServer(t, api, loopbackTrusted)

	result, err := connect(t, server, "good", "2025-06-18").CallTool(context.Background(),
		&mcp.CallToolParams{Name: "account_get", Arguments: map[string]any{"account": "x@other.org"}})
	if err != nil || !result.IsError || !strings.Contains(result.Content[0].(*mcp.TextContent).Text, "forbidden") {
		t.Fatalf("CallTool = %+v, %v", result, err)
	}

	big := &fakeAdmin{body: `{"x":"` + strings.Repeat("a", 200) + `"}`}
	capped := newTestServer(t, big, func(s *config.MCP) {
		loopbackTrusted(s)
		s.Limits.MaxResponseBytes = 128
	})

	result, err = connect(t, capped, "good", "2025-06-18").CallTool(context.Background(), &mcp.CallToolParams{Name: "server_version"})
	if err != nil || !result.IsError || !strings.Contains(result.Content[0].(*mcp.TextContent).Text, codeResponseTooLarge) {
		t.Fatalf("CallTool = %+v, %v", result, err)
	}
}

// TestRealAdminAPIEnforcesRoleAndScope runs calls through the real admin API handler.
func TestRealAdminAPIEnforcesRoleAndScope(t *testing.T) {
	api := admin.New(admin.Options{MaxMessageBytes: 1, Log: slog.New(slog.DiscardHandler), Version: "9.9.9"})
	defer api.Close()

	server := newTestServer(t, api, loopbackTrusted)
	session := connect(t, server, "good", "2025-06-18")

	result, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: "server_version"})
	if err != nil || result.IsError || !strings.Contains(result.Content[0].(*mcp.TextContent).Text, `"version":"9.9.9"`) {
		t.Fatalf("server_version = %+v, %v", result, err)
	}

	// anna's account scope is *@example.org: listing every account needs the scope of all accounts.
	result, err = session.CallTool(context.Background(), &mcp.CallToolParams{Name: "accounts_list"})
	if err != nil || !result.IsError || !strings.Contains(result.Content[0].(*mcp.TextContent).Text, "forbidden") {
		t.Fatalf("accounts_list = %+v, %v", result, err)
	}
}

// TestToolTableMatchesContract pins that every tool is a GET of the contract that needs the role diagnose, that its
// schema names its path parameters as required, and that no tool name repeats.
func TestToolTableMatchesContract(t *testing.T) {
	var contract struct {
		Paths map[string]map[string]any `yaml:"paths"`
	}

	if err := yaml.Unmarshal(adminspec.OpenAPI, &contract); err != nil {
		t.Fatal(err)
	}

	resolved, err := operations()
	if err != nil {
		t.Fatal(err)
	}

	seen := map[string]bool{}

	for index, op := range resolved {
		if seen[op.name] {
			t.Fatalf("tool %s is listed twice", op.name)
		}

		seen[op.name] = true

		get, _ := contract.Paths[tools[index].path]["get"].(map[string]any)
		description, _ := get["description"].(string)

		if !strings.Contains(strings.Join(strings.Fields(description), " "), "role `diagnose`") {
			t.Fatalf("tool %s: the operation does not name the role diagnose", op.name)
		}

		var schema struct {
			Required   []string       `json:"required"`
			Properties map[string]any `json:"properties"`
		}

		if err := json.Unmarshal(op.schema, &schema); err != nil {
			t.Fatal(err)
		}

		for _, param := range op.params {
			if param.in == paramInPath && !slices.Contains(schema.Required, param.name) {
				t.Fatalf("tool %s: path parameter %s is not required", op.name, param.name)
			}
		}

		for _, omitted := range tools[index].omit {
			if _, present := schema.Properties[omitted]; present {
				t.Fatalf("tool %s offers the omitted parameter %s", op.name, omitted)
			}
		}
	}
}

// TestNewRequiresCollaborators pins the constructor boundary.
func TestNewRequiresCollaborators(t *testing.T) {
	if _, err := New(Deps{}); err == nil {
		t.Fatal("empty deps must be rejected")
	}

	if _, err := New(Deps{Settings: config.MCP{OAuth: config.MCPOAuth{Resource: "relative"}}, Tokens: newIntrospector(),
		Admin: &fakeAdmin{}}); err == nil {
		t.Fatal("a relative resource must be rejected")
	}
}

// TestHTTPIntrospector pins the RFC 7662 client: basic auth, active claims, inactive tokens, and unavailability.
func TestHTTPIntrospector(t *testing.T) {
	issuer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, password, ok := r.BasicAuth()
		if !ok || user != "shardpost-mcp" || password != "secret" || r.FormValue("token_type_hint") != "access_token" {
			w.WriteHeader(http.StatusUnauthorized)

			return
		}

		switch r.FormValue("token") {
		case "active":
			_, _ = io.WriteString(w, `{"active":true,"sub":"anna","exp":4102444800}`)
		case "busy":
			w.WriteHeader(http.StatusServiceUnavailable)
		default:
			_, _ = io.WriteString(w, `{"active":false}`)
		}
	}))
	defer issuer.Close()

	introspector := &HTTPIntrospector{URL: issuer.URL, ClientID: "shardpost-mcp", ClientSecret: "secret", HTTP: issuer.Client()}

	got, err := introspector.Introspect(context.Background(), "active")
	if err != nil || got["sub"] != "anna" || got["exp"] != json.Number("4102444800") {
		t.Fatalf("active = %v, %v", got, err)
	}

	if _, err = introspector.Introspect(context.Background(), "old"); err != ErrTokenInactive {
		t.Fatalf("inactive = %v", err)
	}

	if _, err = introspector.Introspect(context.Background(), "busy"); !strings.Contains(err.Error(), ErrIntrospectionUnavailable.Error()) {
		t.Fatalf("busy = %v", err)
	}
}

// TestCallBudgetHoldsTheSlot pins that a call past mcp.limits.timeout is answered with a timeout tool error while
// its slot stays taken until the admin API call returned.
func TestCallBudgetHoldsTheSlot(t *testing.T) {
	api := &fakeAdmin{body: `{}`, block: make(chan struct{})}
	server := newTestServer(t, api, func(s *config.MCP) {
		s.TrustedProxies = []string{"127.0.0.1/32", "::1/128", testProxy + "/32"}
		s.Limits.Timeout = 50 * time.Millisecond
		s.Limits.MaxConcurrent, s.Limits.MaxConcurrentPerPrincipal = 1, 1
	})
	session := connect(t, server, "good", "2025-06-18")

	result, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: "server_version"})
	if err != nil || !result.IsError || !strings.Contains(result.Content[0].(*mcp.TextContent).Text, codeTimeout) {
		t.Fatalf("CallTool = %+v, %v", result, err)
	}

	if code := serve(server, proxied(http.MethodPost, "/mcp", toolCall("server_version", `{}`), "good")).Code; code != http.StatusTooManyRequests {
		t.Fatalf("while the abandoned call runs = %d, want 429", code)
	}

	close(api.block)

	deadline := time.Now().Add(2 * time.Second)
	for serve(server, proxied(http.MethodPost, "/mcp", toolCall("server_version", `{}`), "good")).Code != http.StatusOK {
		if time.Now().After(deadline) {
			t.Fatal("the slot was not released after the admin API call returned")
		}

		time.Sleep(10 * time.Millisecond)
	}
}

// TestDotSegmentsAreRefused pins that . and .. never become path segments of the admin request.
func TestDotSegmentsAreRefused(t *testing.T) {
	api := &fakeAdmin{body: `{}`}
	session := connect(t, newTestServer(t, api, loopbackTrusted), "good", "2025-06-18")

	for _, account := range []string{".", ".."} {
		result, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: "account_quota",
			Arguments: map[string]any{"account": account}})
		if err != nil || !result.IsError || !strings.Contains(result.Content[0].(*mcp.TextContent).Text, codeBadRequest) {
			t.Fatalf("%q = %+v, %v", account, result, err)
		}
	}

	if len(api.calls) != 0 {
		t.Fatalf("calls = %v", api.calls)
	}
}
