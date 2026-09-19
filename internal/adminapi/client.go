// Package adminapi is the transport layer of the ShardPost admin API:
// types, server interface and client generated from
// api/admin/openapi.yaml (api.gen.go) plus the connection helpers here.
package adminapi

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"time"
)

// socketServer is the placeholder base URL of Unix socket clients; the
// transport ignores the host.
const socketServer = "http://shardpost"

// TransportOption adjusts the HTTP client of the admin API.
type TransportOption func(*http.Client, *http.Transport)

// Streaming drops the overall timeout (it would cut off a long download
// such as an account export); the timeout then bounds the wait for the
// response headers. The caller bounds idle reads itself.
func Streaming(c *http.Client, t *http.Transport) {
	t.ResponseHeaderTimeout, c.Timeout = c.Timeout, 0
}

func newClient(server string, transport *http.Transport, timeout time.Duration, opts []TransportOption) (*ClientWithResponses, error) {
	c := &http.Client{Transport: transport, Timeout: timeout}
	for _, o := range opts {
		o(c, transport)
	}

	return NewClientWithResponses(server, WithHTTPClient(c))
}

// NewSocketClient returns a client for the admin API on a Unix socket.
func NewSocketClient(path string, timeout time.Duration, opts ...TransportOption) (*ClientWithResponses, error) {
	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", path)
		},
	}

	return newClient(socketServer, transport, timeout, opts)
}

// NewTLSClient returns a client for the admin API with mutual TLS; the
// server is verified against the CA file, the client presents the key
// pair.
func NewTLSClient(server, certFile, keyFile, caFile string, timeout time.Duration, opts ...TransportOption) (*ClientWithResponses, error) {
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, fmt.Errorf("client key pair: %w", err)
	}

	pem, err := os.ReadFile(caFile)
	if err != nil {
		return nil, fmt.Errorf("server CA: %w", err)
	}

	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		return nil, errors.New("server CA contains no certificate")
	}

	transport := &http.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{cert}, RootCAs: pool}}

	return newClient(server, transport, timeout, opts)
}
