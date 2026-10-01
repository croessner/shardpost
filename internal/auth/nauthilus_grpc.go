package auth

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"fmt"
	"os"
	"strings"
	"time"

	authv1 "github.com/croessner/nauthilus/v4/api/auth/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// GRPCTransport talks to the Nauthilus gRPC authority
// (runtime.servers.grpc.authority). The caller authenticates with the
// same backchannel credentials as over HTTP, sent as "authorization"
// metadata (Bearer from a TokenSource or static, or Basic). The response
// carries an explicit decision (OK / FAIL / TEMPFAIL) and a status
// message, so no HTTP status mapping is needed.
type GRPCTransport struct {
	Address     string      // host:port
	TLS         *tls.Config // nil: plaintext
	User        string
	Password    string
	Token       string
	TokenSource TokenSource
	Timeout     time.Duration

	conn   *grpc.ClientConn
	client authv1.AuthServiceClient
}

// GRPCOptions configures DialGRPC.
type GRPCOptions struct {
	Address               string
	CAFile                string // PEM bundle; "" with TLS on uses system roots
	Insecure              bool   // plaintext (inside the cluster)
	ServerName            string
	ClientCert, ClientKey string // optional mTLS
	User, Password, Token string
	TokenSource           TokenSource
	Timeout               time.Duration
}

// DialGRPC creates the transport. The connection is lazy; Ping checks it.
func DialGRPC(o GRPCOptions) (*GRPCTransport, error) {
	t := &GRPCTransport{Address: o.Address, User: o.User, Password: o.Password, Token: o.Token, TokenSource: o.TokenSource, Timeout: o.Timeout}
	if t.Timeout <= 0 {
		t.Timeout = DefaultTimeout
	}

	var creds credentials.TransportCredentials
	if o.Insecure {
		creds = insecure.NewCredentials()
	} else {
		cfg := &tls.Config{MinVersion: tls.VersionTLS12, ServerName: o.ServerName}
		if o.CAFile != "" {
			pem, err := os.ReadFile(o.CAFile)
			if err != nil {
				return nil, fmt.Errorf("nauthilus ca: %w", err)
			}

			pool := x509.NewCertPool()
			if !pool.AppendCertsFromPEM(pem) {
				return nil, fmt.Errorf("nauthilus ca: no certificates in %s", o.CAFile)
			}

			cfg.RootCAs = pool
		}

		if o.ClientCert != "" {
			cert, err := tls.LoadX509KeyPair(o.ClientCert, o.ClientKey)
			if err != nil {
				return nil, fmt.Errorf("nauthilus client cert: %w", err)
			}

			cfg.Certificates = []tls.Certificate{cert}
		}

		t.TLS = cfg
		creds = credentials.NewTLS(cfg)
	}

	conn, err := grpc.NewClient(o.Address, grpc.WithTransportCredentials(creds))
	if err != nil {
		return nil, fmt.Errorf("nauthilus grpc: %w", err)
	}

	t.conn = conn
	t.client = authv1.NewAuthServiceClient(conn)

	return t, nil
}

// Close closes the connection.
func (t *GRPCTransport) Close() error {
	if t.conn != nil {
		return t.conn.Close()
	}

	return nil
}

// WithClient injects a client (tests).
func (t *GRPCTransport) WithClient(c authv1.AuthServiceClient) *GRPCTransport {
	t.client = c
	return t
}

func (t *GRPCTransport) ctx(ctx context.Context) (context.Context, context.CancelFunc, error) {
	ctx, cancel := context.WithTimeout(ctx, t.Timeout)
	switch {
	case t.TokenSource != nil:
		tok, err := t.TokenSource.Token(ctx)
		if err != nil {
			cancel()
			return nil, nil, err
		}

		ctx = metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+tok)
	case t.Token != "":
		ctx = metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+t.Token)
	case t.User != "":
		ctx = metadata.AppendToOutgoingContext(ctx, "authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(t.User+":"+t.Password)))
	}

	return ctx, cancel, nil
}

// call performs the request. A refused backchannel token is dropped and
// the request retried once with a fresh one (token expiry races), but
// only when the context leaves time for it: at least as long as the
// first attempt took.
func (t *GRPCTransport) call(ctx context.Context, mode string, body nauthilusRequest) (*outcome, error) {
	start := time.Now()

	o, err := t.callOnce(ctx, mode, body)
	if err == nil && o.temp && o.refused && t.TokenSource != nil {
		t.TokenSource.Invalidate()

		if !budgetLeft(ctx, time.Since(start)) {
			return o, nil
		}

		o, err = t.callOnce(ctx, mode, body)
	}

	return o, err
}

// budgetLeft reports whether ctx leaves at least need before its
// deadline (always, without a deadline) and is not done.
func budgetLeft(ctx context.Context, need time.Duration) bool {
	if ctx.Err() != nil {
		return false
	}

	deadline, ok := ctx.Deadline()

	return !ok || time.Until(deadline) >= need
}

func (t *GRPCTransport) callOnce(parent context.Context, mode string, body nauthilusRequest) (*outcome, error) {
	ctx, cancel, err := t.ctx(parent)
	if err != nil {
		return &outcome{temp: true, message: "nauthilus backchannel token: " + err.Error()}, nil
	}
	defer cancel()

	var resp *authv1.AuthResponse

	if mode == modeNoAuth {
		resp, err = t.client.LookupIdentity(ctx, &authv1.LookupIdentityRequest{
			Username: body.Username, ClientIp: body.ClientIP, ClientPort: body.ClientPort,
			LocalIp: body.LocalIP, LocalPort: body.LocalPort, Protocol: body.Protocol, Method: body.Method, Ssl: body.SSL,
		})
	} else {
		resp, err = t.client.Authenticate(ctx, &authv1.AuthRequest{
			Username: body.Username, Password: body.Password, ClientIp: body.ClientIP, ClientPort: body.ClientPort,
			LocalIp: body.LocalIP, LocalPort: body.LocalPort, Protocol: body.Protocol, Method: body.Method, Ssl: body.SSL,
			AuthLoginAttempt: uint32(body.AuthLoginAttempt),
		})
	}

	if err != nil {
		return grpcErrorOutcome(err), nil
	}

	return grpcResponseOutcome(resp), nil
}

// grpcErrorOutcome maps a failed gRPC call onto a temporary outcome.
func grpcErrorOutcome(err error) *outcome {
	st, _ := status.FromError(err)
	switch st.Code() {
	case codes.Unauthenticated, codes.PermissionDenied:
		// our own backchannel credentials are wrong: operator problem
		return &outcome{temp: true, refused: true, message: "nauthilus backchannel: " + st.Message()}
	case codes.InvalidArgument:
		return &outcome{temp: true, message: "nauthilus rejected the request: " + st.Message()}
	}

	return &outcome{temp: true, message: "nauthilus grpc: " + st.Message()}
}

// grpcResponseOutcome maps an AuthResponse onto an outcome.
func grpcResponseOutcome(resp *authv1.AuthResponse) *outcome {
	o := &outcome{message: resp.GetStatusMessage(), accountField: resp.GetAccountField(),
		displayField: resp.GetDisplayNameField(), attributes: map[string][]string{}}
	for k, v := range resp.GetAttributes() {
		o.attributes[k] = append([]string{}, v.GetValues()...)
	}

	switch resp.GetDecision() {
	case authv1.AuthDecision_AUTH_DECISION_OK:
		o.ok = true
	case authv1.AuthDecision_AUTH_DECISION_FAIL:
	case authv1.AuthDecision_AUTH_DECISION_TEMPFAIL:
		o.temp = true
	default: // servers without decision: fall back to ok/error
		o.ok = resp.GetOk()
		if !o.ok && resp.GetError() != "" {
			o.temp = true
			o.message = resp.GetError()
		}
	}

	if o.message == "" && !o.ok {
		o.message = strings.TrimSpace(resp.GetError())
	}

	return o
}

var _ transport = (*GRPCTransport)(nil)
