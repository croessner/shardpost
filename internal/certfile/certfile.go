// Package certfile serves a TLS certificate from files and picks up a
// renewed one without a restart. cert-manager and similar tools replace
// the files of a mounted Secret in place (Kubernetes swaps a symlink);
// a listener that loaded the pair once would go on presenting the old
// certificate until it expires.
package certfile

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"sync/atomic"
	"time"
)

// Source is a certificate kept in step with its files. Use GetCertificate
// (or Config) in a server tls.Config.
type Source struct {
	certFile, keyFile string
	every             time.Duration
	log               *slog.Logger

	cert atomic.Pointer[tls.Certificate]
	// next is when the files are looked at again, in Unix nanoseconds.
	next atomic.Int64

	// refreshing keeps a second handshake from reading the files while
	// one already does; certPEM and keyPEM are the loaded files' content.
	refreshing      sync.Mutex
	certPEM, keyPEM []byte
}

// New loads the pair once and returns a source that looks for a new one
// at most every interval; an interval of 0 never looks again. The first
// load has to succeed: a server without its certificate does not start.
func New(certFile, keyFile string, every time.Duration, log *slog.Logger) (*Source, error) {
	if log == nil {
		log = slog.Default()
	}

	s := &Source{certFile: certFile, keyFile: keyFile, every: every, log: log}

	certPEM, keyPEM, pair, err := s.load()
	if err != nil {
		return nil, err
	}

	s.cert.Store(pair)
	s.certPEM, s.keyPEM = certPEM, keyPEM
	s.next.Store(time.Now().Add(every).UnixNano())

	return s, nil
}

// Config is a server tls.Config that presents the source's certificate.
func (s *Source) Config() *tls.Config {
	return &tls.Config{GetCertificate: s.GetCertificate, MinVersion: tls.VersionTLS12}
}

// GetCertificate returns the current certificate. When the interval has
// passed, the handshake that notices it reads the files and loads a
// changed pair; every other handshake meanwhile gets the current
// certificate without waiting. A changed pair that does not load is
// logged and the previous certificate kept, so a half-written renewal
// does not take the listener down.
func (s *Source) GetCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	if s.every > 0 && time.Now().UnixNano() >= s.next.Load() && s.refreshing.TryLock() {
		s.next.Store(time.Now().Add(s.every).UnixNano())
		s.refresh()
		s.refreshing.Unlock()
	}

	return s.cert.Load(), nil
}

// refresh loads the pair again when the content of its files differs
// from the loaded one. The caller holds refreshing.
func (s *Source) refresh() {
	certPEM, err := os.ReadFile(s.certFile)
	if err != nil {
		s.log.Warn("tls certificate file unreadable, keeping the loaded certificate", "cert", s.certFile, "err", err)

		return
	}

	keyPEM, err := os.ReadFile(s.keyFile)
	if err != nil {
		s.log.Warn("tls key file unreadable, keeping the loaded certificate", "key", s.keyFile, "err", err)

		return
	}

	if bytes.Equal(certPEM, s.certPEM) && bytes.Equal(keyPEM, s.keyPEM) {
		return
	}

	pair, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		s.log.Warn("renewed tls certificate does not load, keeping the loaded one", "cert", s.certFile, "err", err)

		return
	}

	s.cert.Store(&pair)
	s.certPEM, s.keyPEM = certPEM, keyPEM
	s.log.Info("tls certificate reloaded", append([]any{"cert", s.certFile}, leafAttrs(&pair)...)...)
}

// load reads and parses the pair.
func (s *Source) load() ([]byte, []byte, *tls.Certificate, error) {
	certPEM, err := os.ReadFile(s.certFile)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("read tls certificate: %w", err)
	}

	keyPEM, err := os.ReadFile(s.keyFile)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("read tls key: %w", err)
	}

	pair, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("load tls keypair: %w", err)
	}

	return certPEM, keyPEM, &pair, nil
}

// leafAttrs are the log attributes naming the leaf certificate: its
// serial and when it expires.
func leafAttrs(c *tls.Certificate) []any {
	if len(c.Certificate) == 0 {
		return nil
	}

	leaf, err := x509.ParseCertificate(c.Certificate[0])
	if err != nil {
		return nil
	}

	return []any{"serial", leaf.SerialNumber.String(), "not_after", leaf.NotAfter.UTC().Format(time.RFC3339)}
}
