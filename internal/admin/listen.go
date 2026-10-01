package admin

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Server timeouts of both listeners; deliveries of large messages need
// the long read timeout.
const (
	readHeaderTimeout = 10 * time.Second
	readTimeout       = 5 * time.Minute
	writeTimeout      = 5 * time.Minute
	idleTimeout       = time.Minute
)

// reloadInterval bounds how often certificate files are checked (a
// variable for tests).
var reloadInterval = 10 * time.Second

// ListenUnix opens the admin socket at path with mode 0600. A stale
// socket from an earlier run is removed; any other file there is an
// error.
func ListenUnix(path string) (net.Listener, error) {
	if st, err := os.Lstat(path); err == nil {
		if st.Mode().Type() != fs.ModeSocket {
			return nil, fmt.Errorf("admin socket path %s exists and is not a socket", path)
		}

		if err := os.Remove(path); err != nil {
			return nil, err
		}
	}

	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}

	if err := os.Chmod(path, 0o600); err != nil {
		_ = ln.Close()
		return nil, err
	}

	return ln, nil
}

// ownUID is the user ID admitted on the socket (a variable for tests).
var ownUID = func() uint32 { return uint32(os.Getuid()) }

// UnixServer serves the API on the socket. Only processes running as
// the server's own user are admitted; they get the socket roles.
func (s *Server) UnixServer() *http.Server {
	own := ownUID()

	return &http.Server{Handler: s.handler, ReadHeaderTimeout: readHeaderTimeout, ReadTimeout: readTimeout, WriteTimeout: writeTimeout, IdleTimeout: idleTimeout,
		ConnContext: func(ctx context.Context, c net.Conn) context.Context {
			uc, ok := c.(*net.UnixConn)
			if !ok {
				return ctx
			}

			uid, err := peerUID(uc)
			if err != nil {
				s.o.Log.Warn("admin socket caller refused, no peer credentials", "err", err)
				return ctx
			}

			if uid != own {
				s.o.Log.Warn("admin socket caller refused", "uid", uid)
				return ctx
			}

			p := newPrincipal("uid:"+strconv.FormatUint(uint64(uid), 10), "unix", s.o.SocketRoles)
			p.allAccounts = true // the socket is the server's own user

			return withPrincipal(ctx, p)
		}}
}

// TLSServer serves the API with mutual TLS. Certificate, key and client
// CA are re-read when their files change.
func (s *Server) TLSServer(certFile, keyFile, caFile string) (*http.Server, error) {
	rl := &reloader{certFile: certFile, keyFile: keyFile, caFile: caFile, log: s.o.Log}
	if _, err := rl.current(); err != nil {
		return nil, err
	}

	cfg := &tls.Config{MinVersion: tls.VersionTLS12,
		GetConfigForClient: func(*tls.ClientHelloInfo) (*tls.Config, error) {
			m, err := rl.current()
			if err != nil {
				return nil, err
			}

			return &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{*m.cert},
				ClientAuth: tls.RequireAndVerifyClientCert, ClientCAs: m.pool}, nil
		}}

	return &http.Server{Handler: s.handler, TLSConfig: cfg, ReadHeaderTimeout: readHeaderTimeout,
		ReadTimeout: readTimeout, WriteTimeout: writeTimeout, IdleTimeout: idleTimeout}, nil
}

// material is one loaded set of certificate files.
type material struct {
	cert  *tls.Certificate
	pool  *x509.CertPool
	stamp string
}

// reloader keeps the certificate material current: at most every
// reloadInterval it compares size and modification time of the files
// and loads them again when they changed. A broken new version keeps
// the old material and is logged.
type reloader struct {
	certFile, keyFile, caFile string
	log                       *slog.Logger

	mu      sync.Mutex
	cur     *material
	checked time.Time
}

func (rl *reloader) current() (*material, error) {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	if rl.cur != nil && time.Since(rl.checked) < reloadInterval {
		return rl.cur, nil
	}

	rl.checked = time.Now()

	stamp, err := fileStamp(rl.certFile, rl.keyFile, rl.caFile)
	if err != nil {
		return rl.fallback(err)
	}

	if rl.cur != nil && rl.cur.stamp == stamp {
		return rl.cur, nil
	}

	m, err := loadMaterial(rl.certFile, rl.keyFile, rl.caFile)
	if err != nil {
		return rl.fallback(err)
	}

	m.stamp = stamp
	rl.cur = m

	return m, nil
}

// fallback keeps serving the last good material.
func (rl *reloader) fallback(err error) (*material, error) {
	if rl.cur != nil {
		rl.log.Warn("admin TLS material not reloaded, keeping the previous one", "err", err)
		return rl.cur, nil
	}

	return nil, err
}

// fileStamp summarizes size and modification time of the files.
func fileStamp(files ...string) (string, error) {
	var b strings.Builder

	for _, f := range files {
		st, err := os.Stat(f)
		if err != nil {
			return "", err
		}

		fmt.Fprintf(&b, "%d/%d;", st.Size(), st.ModTime().UnixNano())
	}

	return b.String(), nil
}

// loadMaterial reads the key pair and the client CA bundle.
func loadMaterial(certFile, keyFile, caFile string) (*material, error) {
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, fmt.Errorf("admin TLS key pair: %w", err)
	}

	pem, err := os.ReadFile(caFile)
	if err != nil {
		return nil, fmt.Errorf("admin client CA: %w", err)
	}

	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		return nil, errors.New("admin client CA contains no certificate")
	}

	return &material{cert: &cert, pool: pool}, nil
}

// certIdentities are the names a client certificate can be configured
// under: the subject common name and the DNS and URI SANs.
func certIdentities(c *x509.Certificate) []string {
	var ids []string
	if c.Subject.CommonName != "" {
		ids = append(ids, c.Subject.CommonName)
	}

	ids = append(ids, c.DNSNames...)
	for _, u := range c.URIs {
		ids = append(ids, u.String())
	}

	return ids
}
