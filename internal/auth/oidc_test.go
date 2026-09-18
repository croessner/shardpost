package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	authv1 "github.com/croessner/nauthilus/v4/api/auth/v1"
	"google.golang.org/grpc"
)

// tokenServer issues tok1, tok2, ... and records the requests.
func tokenServer(t *testing.T, expiresIn int64) (*httptest.Server, *atomic.Int32) {
	t.Helper()

	var n atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil || r.Form.Get("grant_type") != "client_credentials" ||
			r.Form.Get("client_id") != "poc" || r.Form.Get("client_secret") != "s3cret" ||
			r.Form.Get("scope") != "nauthilus:authenticate nauthilus:lookup_identity" {
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "invalid_client"})

			return
		}

		i := n.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": fmt.Sprintf("tok%d", i), "token_type": "Bearer", "expires_in": expiresIn})
	}))
	t.Cleanup(srv.Close)

	return srv, &n
}

func TestClientCredentialsCachesAndRefreshes(t *testing.T) {
	srv, fetched := tokenServer(t, 3600)
	cc := &ClientCredentials{TokenURL: srv.URL, ClientID: "poc", ClientSecret: "s3cret",
		Scopes: []string{"nauthilus:authenticate", "nauthilus:lookup_identity"}}

	ctx := context.Background()
	for i := 0; i < 3; i++ {
		tok, err := cc.Token(ctx)
		if err != nil || tok != "tok1" {
			t.Fatalf("call %d: %q %v", i, tok, err)
		}
	}

	if fetched.Load() != 1 {
		t.Fatalf("fetched %d times, want 1", fetched.Load())
	}

	cc.Invalidate()

	if tok, _ := cc.Token(ctx); tok != "tok2" {
		t.Fatalf("after invalidate: %q", tok)
	}
	// A token close to expiry is renewed ahead of time.
	cc.expiry = time.Now().Add(30 * time.Second)
	if tok, _ := cc.Token(ctx); tok != "tok3" {
		t.Fatalf("near expiry: %q", tok)
	}
}

func TestClientCredentialsErrors(t *testing.T) {
	srv, _ := tokenServer(t, 3600)

	cc := &ClientCredentials{TokenURL: srv.URL, ClientID: "poc", ClientSecret: "wrong"}
	if _, err := cc.Token(context.Background()); !errors.Is(err, ErrTokenEndpoint) {
		t.Fatalf("want ErrTokenEndpoint, got %v", err)
	}
	// Endpoint down but cached token still valid: keep serving it.
	cc = &ClientCredentials{TokenURL: "http://127.0.0.1:1", ClientID: "poc", ClientSecret: "s3cret",
		token: "cached", expiry: time.Now().Add(30 * time.Second)}
	if tok, err := cc.Token(context.Background()); err != nil || tok != "cached" {
		t.Fatalf("cached fallback: %q %v", tok, err)
	}
}

func TestNauthilusGRPCWithTokenSource(t *testing.T) {
	srv, fetched := tokenServer(t, 3600)
	cc := &ClientCredentials{TokenURL: srv.URL, ClientID: "poc", ClientSecret: "s3cret",
		Scopes: []string{"nauthilus:authenticate", "nauthilus:lookup_identity"}}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	var valid atomic.Value
	valid.Store("Bearer tok1")

	fa := &fakeAuthority{accept: func(v string) bool { return v == valid.Load().(string) }}
	gs := grpc.NewServer()

	authv1.RegisterAuthServiceServer(gs, fa)

	go func() { _ = gs.Serve(ln) }()

	defer gs.Stop()

	tr, err := DialGRPC(GRPCOptions{Address: ln.Addr().String(), Insecure: true, TokenSource: cc})
	if err != nil {
		t.Fatal(err)
	}

	defer func() { _ = tr.Close() }()

	n := &Nauthilus{GRPC: tr}
	ctx := context.Background()

	res, err := n.Authenticate(ctx, Request{Username: "alice", Password: "secret", Protocol: "imap"})
	if err != nil || res.Account != "alice@example.org" {
		t.Fatalf("auth: %+v %v", res, err)
	}

	if fetched.Load() != 1 {
		t.Fatalf("fetched %d, want 1", fetched.Load())
	}
	// The authority revokes tok1: one transparent retry with a fresh token.
	valid.Store("Bearer tok2")

	if res, err = n.Authenticate(ctx, Request{Username: "alice", Password: "secret", Protocol: "imap"}); err != nil || res.Account == "" {
		t.Fatalf("after revoke: %+v %v", res, err)
	}

	if fetched.Load() != 2 {
		t.Fatalf("fetched %d, want 2", fetched.Load())
	}
	// Token endpoint gone and token revoked again: temporary failure, not
	// a user failure.
	srv.Close()
	valid.Store("Bearer tok3")

	if _, err = n.Authenticate(ctx, Request{Username: "alice", Password: "secret", Protocol: "imap"}); !errors.Is(err, ErrTemporary) {
		t.Fatalf("want ErrTemporary, got %v", err)
	}
}
