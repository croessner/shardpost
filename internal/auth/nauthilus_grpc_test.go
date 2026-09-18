package auth

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"

	authv1 "github.com/croessner/nauthilus/v4/api/auth/v1"
	commonv1 "github.com/croessner/nauthilus/v4/api/common/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// fakeAuthority is an in-process nauthilus.auth.v1.AuthService.
type fakeAuthority struct {
	authv1.UnimplementedAuthServiceServer
	last   *authv1.AuthRequest
	lookup *authv1.LookupIdentityRequest
	accept func(authz string) bool // nil: Basic svc:secret
}

func (f *fakeAuthority) checkCaller(ctx context.Context) error {
	md, _ := metadata.FromIncomingContext(ctx)
	v := md.Get("authorization")

	ok := len(v) == 1
	if ok {
		if f.accept != nil {
			ok = f.accept(v[0])
		} else {
			ok = v[0] == "Basic c3ZjOnNlY3JldA==" // svc:secret
		}
	}

	if !ok {
		return status.Error(codes.Unauthenticated, "bad caller")
	}

	return nil
}

func okResp(account string) *authv1.AuthResponse {
	return &authv1.AuthResponse{Ok: true, Decision: authv1.AuthDecision_AUTH_DECISION_OK, AccountField: "account",
		Attributes: map[string]*commonv1.AttributeValues{"account": {Values: []string{account}}, "mailShard": {Values: []string{"a"}}}}
}

func (f *fakeAuthority) Authenticate(ctx context.Context, r *authv1.AuthRequest) (*authv1.AuthResponse, error) {
	if err := f.checkCaller(ctx); err != nil {
		return nil, err
	}

	f.last = r
	switch {
	case r.Username == "alice" && r.Password == "secret", r.Username == "alice" && r.Password == "tok" && r.Method == "oauthbearer":
		return okResp("Alice@Example.org"), nil
	case r.Username == "temp":
		return &authv1.AuthResponse{Decision: authv1.AuthDecision_AUTH_DECISION_TEMPFAIL, StatusMessage: "Temporary server problem, try again later"}, nil
	}

	return &authv1.AuthResponse{Decision: authv1.AuthDecision_AUTH_DECISION_FAIL, StatusMessage: "Invalid login or password"}, nil
}

func (f *fakeAuthority) LookupIdentity(ctx context.Context, r *authv1.LookupIdentityRequest) (*authv1.AuthResponse, error) {
	if err := f.checkCaller(ctx); err != nil {
		return nil, err
	}

	f.lookup = r
	if strings.HasPrefix(strings.ToLower(r.Username), "alias") {
		return okResp("alice@example.org"), nil
	}

	return &authv1.AuthResponse{Decision: authv1.AuthDecision_AUTH_DECISION_FAIL}, nil
}

func TestNauthilusGRPC(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	srv := grpc.NewServer()
	fa := &fakeAuthority{}

	authv1.RegisterAuthServiceServer(srv, fa)

	go func() { _ = srv.Serve(ln) }()

	defer srv.Stop()

	tr, err := DialGRPC(GRPCOptions{Address: ln.Addr().String(), Insecure: true, User: "svc", Password: "secret"})
	if err != nil {
		t.Fatal(err)
	}

	defer func() { _ = tr.Close() }()

	testGRPCAuthenticate(t, &Nauthilus{GRPC: tr}, fa)
	testGRPCBearer(t, &Nauthilus{GRPC: tr}, fa)
	testGRPCLookup(t, &Nauthilus{GRPC: tr}, fa)

	// wrong backchannel credentials are an operator problem: temporary
	bad, _ := DialGRPC(GRPCOptions{Address: ln.Addr().String(), Insecure: true, User: "svc", Password: "nope"})

	defer func() { _ = bad.Close() }()

	if _, err := (&Nauthilus{GRPC: bad}).Authenticate(context.Background(), Request{Username: "alice", Password: "secret"}); !errors.Is(err, ErrTemporary) {
		t.Fatal(err)
	}
}

func testGRPCAuthenticate(t *testing.T, n *Nauthilus, fa *fakeAuthority) {
	t.Helper()

	ctx := context.Background()
	client := &net.TCPAddr{IP: net.IPv4(10, 0, 0, 9), Port: 5555}

	res, err := n.Authenticate(ctx, Request{Username: "alice", Password: "secret", Mechanism: "PLAIN", Protocol: "imap", Client: client, TLS: true})
	if err != nil || res.Account != "alice@example.org" {
		t.Fatalf("%v %v", res, err)
	}

	if fa.last.ClientIp != "10.0.0.9" || fa.last.ClientPort != "5555" || fa.last.Ssl != "on" || fa.last.Method != "plain" || fa.last.AuthLoginAttempt != 1 {
		t.Fatalf("request: %+v", fa.last)
	}

	if _, err = n.Authenticate(ctx, Request{Username: "alice", Password: "wrong", Mechanism: "PLAIN", Client: client}); !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}

	if _, err = n.Authenticate(ctx, Request{Username: "temp", Password: "x", Mechanism: "PLAIN"}); !errors.Is(err, ErrTemporary) {
		t.Fatal(err)
	}
}

func testGRPCBearer(t *testing.T, n *Nauthilus, fa *fakeAuthority) {
	t.Helper()

	res, err := n.Authenticate(context.Background(), Request{Username: "alice", Token: "tok", Mechanism: "OAUTHBEARER", Protocol: "imap"})
	if err != nil || res.Account != "alice@example.org" || fa.last.Password != "tok" {
		t.Fatalf("bearer: %v %v %+v", res, err, fa.last)
	}
}

func testGRPCLookup(t *testing.T, n *Nauthilus, fa *fakeAuthority) {
	t.Helper()

	client := &net.TCPAddr{IP: net.IPv4(10, 0, 0, 9), Port: 5555}

	res, err := n.Lookup(context.Background(), "<Alias7@Example.ORG>", "lmtp", client)
	if err != nil || res.Account != "alice@example.org" || fa.lookup.Method != "recipient_lookup" || fa.lookup.Username != "Alias7@example.org" {
		t.Fatalf("lookup: %v %v %+v", res, err, fa.lookup)
	}
}
