// Package certfile serves a TLS certificate from files and picks up a
// renewed one without a restart. cert-manager and similar tools replace
// the files of a mounted Secret in place (Kubernetes swaps a symlink);
// a listener that loaded the pair once would go on presenting the old
// certificate until it expires.
package certfile

import (
	"crypto/tls"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"time"
)

// Source is a certificate kept in step with its files. Use GetCertificate
// in a tls.Config.
type Source struct {
	certFile, keyFile string
	every             time.Duration
	log               *slog.Logger

	mu      sync.Mutex
	cert    *tls.Certificate
	checked time.Time
	stamp   stamp
}

// stamp identifies a version of the two files.
type stamp struct {
	certMod, keyMod   time.Time
	certSize, keySize int64
}

// New loads the pair once and returns a source that looks for a new one
// at most every interval; an interval of 0 never looks again. The first
// load has to succeed: a server without its certificate does not start.
func New(certFile, keyFile string, every time.Duration, log *slog.Logger) (*Source, error) {
	if log == nil {
		log = slog.Default()
	}

	s := &Source{certFile: certFile, keyFile: keyFile, every: every, log: log}

	st, err := s.stat()
	if err != nil {
		return nil, err
	}

	pair, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, fmt.Errorf("load tls keypair: %w", err)
	}

	s.cert, s.stamp, s.checked = &pair, st, time.Now()

	return s, nil
}

// Config is a server tls.Config that presents the source's certificate.
func (s *Source) Config() *tls.Config {
	return &tls.Config{GetCertificate: s.GetCertificate, MinVersion: tls.VersionTLS12}
}

// GetCertificate returns the current certificate, loading a new pair
// first when the files changed since the last look. A new pair that does
// not load is logged and the previous certificate kept, so a half-written
// renewal does not take the listener down.
func (s *Source) GetCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.every > 0 && time.Since(s.checked) >= s.every {
		s.checked = time.Now()
		s.refresh()
	}

	return s.cert, nil
}

// refresh loads the pair again when its files are not the ones loaded.
func (s *Source) refresh() {
	st, err := s.stat()
	if err != nil {
		s.log.Warn("tls certificate files unreadable, keeping the loaded certificate", "cert", s.certFile, "err", err)

		return
	}

	if st == s.stamp {
		return
	}

	pair, err := tls.LoadX509KeyPair(s.certFile, s.keyFile)
	if err != nil {
		s.log.Warn("renewed tls certificate does not load, keeping the loaded one", "cert", s.certFile, "err", err)

		return
	}

	s.cert, s.stamp = &pair, st
	s.log.Info("tls certificate reloaded", "cert", s.certFile)
}

// stat reads the modification time and size of both files, following
// symlinks, which is how a mounted Secret changes.
func (s *Source) stat() (stamp, error) {
	c, err := os.Stat(s.certFile)
	if err != nil {
		return stamp{}, err
	}

	k, err := os.Stat(s.keyFile)
	if err != nil {
		return stamp{}, err
	}

	return stamp{certMod: c.ModTime(), keyMod: k.ModTime(), certSize: c.Size(), keySize: k.Size()}, nil
}
