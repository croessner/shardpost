package auth

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// writeJSON encodes v as the response body; a failed write only makes the
// test fail on the client side.
func writeJSON(w http.ResponseWriter, v any) {
	_ = json.NewEncoder(w).Encode(v)
}

// fakeIntrospect answers RFC 7662 introspection requests.
func fakeIntrospect(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		w.WriteHeader(400)
		return
	}

	if r.Form.Get("token") == "good-token" {
		writeJSON(w, map[string]any{"active": true, "aud": "mail", "scope": "email mail:imap", "email": "alice@example.org"})
	} else {
		writeJSON(w, map[string]any{"active": false})
	}
}

// fakeLookup answers a lookup without a password (mode=no-auth): every
// alias resolves to alice, bob is an account of his own, temp fails.
func fakeLookup(w http.ResponseWriter, user string) {
	switch {
	case user == "bob@example.org":
		w.Header().Set("Auth-User", "bob@example.org")
		writeJSON(w, map[string]any{"ok": true, "account_field": "account", "attributes": map[string]any{"account": []string{"bob@example.org"}}})
	case user == "temp@example.org":
		w.WriteHeader(500)
		_, _ = w.Write([]byte(`{"error":"Temporary server problem, try again later"}`))
	case strings.HasPrefix(strings.ToLower(user), "alias") || user == "alice@example.org":
		w.Header().Set("Auth-User", "alice@example.org")
		writeJSON(w, map[string]any{"ok": true, "account_field": "account", "attributes": map[string]any{"account": []string{"alice@example.org"}}})
	default:
		w.Header().Set("Auth-Status", "Invalid login or password")
		w.WriteHeader(403)
		_, _ = w.Write([]byte("null"))
	}
}

// fakeAuthJSON answers POST /api/v1/auth/json for the decoded request.
func fakeAuthJSON(w http.ResponseWriter, r *http.Request, req map[string]any) {
	w.Header().Set("X-Nauthilus-Session", "sess-1")

	user, _ := req["username"].(string)

	if r.URL.Query().Get("mode") == "no-auth" {
		fakeLookup(w, user)

		return
	}

	pw, _ := req["password"].(string)
	switch {
	case user == "alice" && pw == "secret", user == "alice" && pw == "tok-123" && req["method"] == "xoauth2":
		writeJSON(w, map[string]any{"ok": true, "account_field": "account", "backend": 2, "attributes": map[string]any{"account": []string{"Alice@Example.org"}, "mailShard": []string{"a"}}})
	case user == "temp":
		w.WriteHeader(500)
		_, _ = w.Write([]byte(`{"error":"Temporary server problem, try again later"}`))
	default:
		w.Header().Set("Auth-Wait", "0.2")
		w.WriteHeader(403)
		_, _ = w.Write([]byte("null"))
	}
}

// fakeNauthilus mimics the documented JSON auth surface.
func fakeNauthilus(t *testing.T) (*httptest.Server, *[]map[string]any) {
	var seen []map[string]any

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if u, p, ok := r.BasicAuth(); !ok || u != "svc" || p != "secret" {
			w.WriteHeader(401)
			return
		}

		if r.URL.Path == "/oidc/introspect" {
			fakeIntrospect(w, r)
			return
		}

		var req map[string]any

		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, &req); err != nil {
			w.WriteHeader(400)
			return
		}

		req["_mode"] = r.URL.Query().Get("mode")
		seen = append(seen, req)

		fakeAuthJSON(w, r, req)
	}))
	t.Cleanup(srv.Close)

	return srv, &seen
}

// nauthilusFixture bundles the fake server and a client for the sub-tests.
type nauthilusFixture struct {
	srv    *httptest.Server
	seen   *[]map[string]any
	n      *Nauthilus
	client *net.TCPAddr
	ctx    context.Context
}

func (f *nauthilusFixture) last() map[string]any {
	return (*f.seen)[len(*f.seen)-1]
}

func TestNauthilus(t *testing.T) {
	srv, seen := fakeNauthilus(t)
	f := &nauthilusFixture{
		srv:    srv,
		seen:   seen,
		n:      &Nauthilus{URL: srv.URL + "/api/v1/auth/json", User: "svc", Password: "secret", MaxWait: time.Second},
		client: &net.TCPAddr{IP: net.IPv4(10, 1, 2, 3), Port: 4444},
		ctx:    context.Background(),
	}

	testNauthilusPassword(t, f)
	testNauthilusFailures(t, f)
	testNauthilusBearer(t, f)
	testNauthilusBareBearer(t, f)
	testNauthilusLookup(t, f)
	testNauthilusMasterUser(t, f)
}

func testNauthilusPassword(t *testing.T, f *nauthilusFixture) {
	t.Helper()

	n, ctx, client := f.n, f.ctx, f.client

	res, err := n.Authenticate(ctx, Request{Username: "alice", Password: "secret", Mechanism: "PLAIN", Protocol: "imap", Client: client, TLS: true})
	if err != nil || res.Account != "alice@example.org" {
		t.Fatalf("%v %v", res, err)
	}

	last := f.last()
	if last["client_ip"] != "10.1.2.3" || last["client_port"] != "4444" || last["ssl"] != "on" || last["method"] != "plain" || last["auth_login_attempt"] != float64(1) {
		t.Fatalf("request fields: %v", last)
	}
}

func testNauthilusFailures(t *testing.T, f *nauthilusFixture) {
	t.Helper()

	n, ctx, client := f.n, f.ctx, f.client
	start := time.Now()

	_, err := n.Authenticate(ctx, Request{Username: "alice", Password: "wrong", Mechanism: "PLAIN", Protocol: "imap", Client: client})
	if !errors.Is(err, ErrInvalid) || time.Since(start) < 150*time.Millisecond {
		t.Fatalf("expected ErrInvalid after Auth-Wait, got %v after %s", err, time.Since(start))
	}

	if _, err = n.Authenticate(ctx, Request{Username: "alice", Password: "wrong", Mechanism: "PLAIN", Client: client}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("expected ErrInvalid, got %v", err)
	}

	if last := f.last(); last["auth_login_attempt"] != float64(2) {
		t.Fatalf("attempt counter: %v", last)
	}

	_, err = n.Authenticate(ctx, Request{Username: "temp", Password: "x", Mechanism: "PLAIN"})
	if !errors.Is(err, ErrTemporary) {
		t.Fatalf("expected ErrTemporary, got %v", err)
	}
}

func testNauthilusBearer(t *testing.T, f *nauthilusFixture) {
	t.Helper()

	ctx := f.ctx

	// bearer in password field (director contract)
	res, err := f.n.Authenticate(ctx, Request{Username: "alice", Token: "tok-123", Mechanism: "XOAUTH2", Protocol: "imap"})
	if err != nil || res.Account != "alice@example.org" {
		t.Fatalf("xoauth2: %v %v", res, err)
	}

	if last := f.last(); last["password"] != "tok-123" || last["method"] != "xoauth2" {
		t.Fatalf("bearer request: %v", last)
	}

	// bearer via introspection
	n2 := &Nauthilus{URL: f.srv.URL + "/api/v1/auth/json", User: "svc", Password: "secret", BearerMode: "introspect",
		Introspect: &Introspection{URL: f.srv.URL + "/oidc/introspect", ClientID: "svc", ClientSecret: "secret", Audience: "mail", Scope: "mail:imap"}}

	res, err = n2.Authenticate(ctx, Request{Token: "good-token", Mechanism: "OAUTHBEARER"})
	if err != nil || res.Account != "alice@example.org" {
		t.Fatalf("introspect: %v %v", res, err)
	}

	if last := f.last(); last["_mode"] != "no-auth" || last["username"] != "alice@example.org" {
		t.Fatalf("introspect lookup: %v", last)
	}

	if _, err = n2.Authenticate(ctx, Request{Token: "bad", Mechanism: "OAUTHBEARER"}); !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}

	testNauthilusIntrospectedUserName(t, n2)
}

// testNauthilusIntrospectedUserName: a user name sent with an introspected
// token (IMAP XOAUTH2, OAUTHBEARER) is an alias or address as often as the
// canonical account; it counts when it resolves to the token's account.
func testNauthilusIntrospectedUserName(t *testing.T, n *Nauthilus) {
	t.Helper()

	ctx := context.Background()

	for _, user := range []string{"alice@example.org", "ALICE@example.org", "alias1@example.org", "Alias2@Example.ORG"} {
		res, err := n.Authenticate(ctx, Request{Username: user, Token: "good-token", Mechanism: "XOAUTH2", Protocol: "imap"})
		if err != nil || res.Account != "alice@example.org" {
			t.Fatalf("user name %q with alice's token: %v %v", user, res, err)
		}
	}

	for _, user := range []string{"bob@example.org", "nobody@example.org"} {
		if _, err := n.Authenticate(ctx, Request{Username: user, Token: "good-token", Mechanism: "XOAUTH2", Protocol: "imap"}); !errors.Is(err, ErrInvalid) {
			t.Fatalf("user name %q with alice's token: %v", user, err)
		}
	}

	if _, err := n.Authenticate(ctx, Request{Username: "temp@example.org", Token: "good-token", Mechanism: "XOAUTH2", Protocol: "imap"}); !errors.Is(err, ErrTemporary) {
		t.Fatalf("user name lookup failing temporarily: %v", err)
	}
}

func testNauthilusLookup(t *testing.T, f *nauthilusFixture) {
	t.Helper()

	// LMTP recipient lookup
	res, err := f.n.Lookup(f.ctx, "<Alias1@Example.ORG>", "lmtp", f.client)
	if err != nil || res.Account != "alice@example.org" {
		t.Fatalf("lookup: %v %v", res, err)
	}

	if last := f.last(); last["_mode"] != "no-auth" || last["method"] != "recipient_lookup" || last["username"] != "Alias1@example.org" || last["protocol"] != "lmtp" {
		t.Fatalf("lookup request: %v", last)
	}

	if _, err = f.n.Lookup(f.ctx, "nobody@example.org", "lmtp", f.client); !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}
}

func testNauthilusMasterUser(t *testing.T, f *nauthilusFixture) {
	t.Helper()

	ctx := f.ctx
	m := &MasterUser{Next: f.n, Name: "director", Password: "masterpw"}

	res, err := m.Authenticate(ctx, Request{Username: "alias2@example.org*director", Password: "masterpw", Mechanism: "PLAIN"})
	if err != nil || res.Account != "alice@example.org" {
		t.Fatalf("master: %v %v", res, err)
	}

	for _, pw := range []string{"wrong", "masterp", "masterpw2"} {
		if _, err = m.Authenticate(ctx, Request{Username: "alias2@example.org*director", Password: pw, Mechanism: "PLAIN"}); !errors.Is(err, ErrInvalid) {
			t.Fatalf("password %q: %v", pw, err)
		}
	}

	if res, err := m.Authenticate(ctx, Request{Username: "alice", Password: "secret", Mechanism: "PLAIN"}); err != nil || res.Account != "alice@example.org" {
		t.Fatalf("passthrough: %v %v", res, err)
	}
}

func TestSASLParsers(t *testing.T) {
	u, p, err := ParsePlain("AHRlc3QAdGVzdA==")
	if err != nil || u != "test" || p != "test" {
		t.Fatal(u, p, err)
	}

	u, tok, err := ParseXOAUTH2("dXNlcj1hbGljZQFhdXRoPUJlYXJlciB0b2sBAQ==") // user=alice^Aauth=Bearer tok^A^A
	if err != nil || u != "alice" || tok != "tok" {
		t.Fatal(u, tok, err)
	}

	u, tok, err = ParseOAuthBearer("bixhPWFsaWNlLAFhdXRoPUJlYXJlciB0b2sBAQ==") // n,a=alice,^Aauth=Bearer tok^A^A
	if err != nil || u != "alice" || tok != "tok" {
		t.Fatal(u, tok, err)
	}
}

// testNauthilusBareBearer: in password mode a token with a user name goes
// to Nauthilus, one without (JMAP) is introspected, and without
// introspection it is refused.
func testNauthilusBareBearer(t *testing.T, f *nauthilusFixture) {
	t.Helper()

	ctx := f.ctx

	n3 := &Nauthilus{URL: f.srv.URL + "/api/v1/auth/json", User: "svc", Password: "secret", BearerMode: "password",
		Introspect: &Introspection{URL: f.srv.URL + "/oidc/introspect", ClientID: "svc", ClientSecret: "secret", Audience: "mail", Scope: "mail:imap"}}

	if res, err := n3.Authenticate(ctx, Request{Token: "good-token", Mechanism: "OAUTHBEARER"}); err != nil || res.Account != "alice@example.org" {
		t.Fatalf("password mode without a user name: %v %v", res, err)
	}

	if last := f.last(); last["_mode"] != "no-auth" {
		t.Fatalf("password mode without a user name did not introspect: %v", last)
	}

	n3.Introspect = nil
	if _, err := n3.Authenticate(ctx, Request{Token: "good-token", Mechanism: "OAUTHBEARER"}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("a bare token without introspection gave %v", err)
	}
}
