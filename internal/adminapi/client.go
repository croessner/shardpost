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

// NewSocketClient returns a client for the admin API on a Unix socket.
func NewSocketClient(path string, timeout time.Duration) (*ClientWithResponses, error) {
	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", path)
		},
	}

	return NewClientWithResponses(socketServer, WithHTTPClient(&http.Client{Transport: transport, Timeout: timeout}))
}

// NewTLSClient returns a client for the admin API with mutual TLS; the
// server is verified against the CA file, the client presents the key
// pair.
func NewTLSClient(server, certFile, keyFile, caFile string, timeout time.Duration) (*ClientWithResponses, error) {
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

	return NewClientWithResponses(server, WithHTTPClient(&http.Client{Transport: transport, Timeout: timeout}))
}
