package main

import (
	"crypto/tls"
	"fmt"
	"log/slog"

	"shardpost/internal/auth"
	"shardpost/internal/cephstore"
	"shardpost/internal/config"
	"shardpost/internal/jmap"
)

// setupHTTP builds the health and metrics listener, the JMAP front-end
// and the admin API.
func (srv *servers) setupHTTP(log *slog.Logger, settings *config.Config, store *cephstore.Store, authn auth.Authenticator) error {
	srv.http = newHTTPServer(settings, store)

	if err := srv.setupJMAP(log, settings, store, authn); err != nil {
		return err
	}

	admin, err := newAdminServers(log, settings, authn, srv.delivery, store)
	if err != nil {
		return err
	}

	srv.admin = admin

	return nil
}

// setupJMAP builds the JMAP front-end and its listener. Without an
// address JMAP is off and nothing is built.
func (srv *servers) setupJMAP(log *slog.Logger, settings *config.Config, store *cephstore.Store, authn auth.Authenticator) error {
	if !settings.JMAP.Enabled() {
		return nil
	}

	cert, key := settings.JMAP.Certificate(settings.TLS)

	if cert != "" {
		pair, err := tls.LoadX509KeyPair(cert, key)
		if err != nil {
			return fmt.Errorf("jmap tls: %w", err)
		}

		srv.jmapTLS = &tls.Config{Certificates: []tls.Certificate{pair}, MinVersion: tls.VersionTLS12}
	}

	front := jmap.New(jmap.Options{
		Store:         storeAdapter{store},
		Auth:          authn,
		Log:           log.With("proto", "jmap"),
		PublicBaseURL: settings.JMAP.PublicBaseURL,
		InsecureAuth:  settings.JMAP.InsecureAuth,
		Limits:        jmapLimits(settings.JMAP),
	})

	srv.jmap = front.HTTPServer(settings.JMAP.Addr)

	return nil
}

// jmapLimits maps the configured limits; a zero keeps the default.
func jmapLimits(c config.JMAP) jmap.Limits {
	return jmap.Limits{
		MaxSizeUpload:         c.MaxSizeUpload,
		MaxSizeRequest:        c.MaxSizeRequest,
		MaxConcurrentUpload:   int(c.MaxConcurrentUpload),
		MaxConcurrentRequests: int(c.MaxConcurrentRequests),
		MaxCallsInRequest:     int(c.MaxCallsInRequest),
		MaxObjectsInGet:       int(c.MaxObjectsInGet),
		MaxObjectsInSet:       int(c.MaxObjectsInSet),
	}
}
