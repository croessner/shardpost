package main

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"time"

	"shardpost/internal/admin"
	"shardpost/internal/auth"
	"shardpost/internal/cephstore"
	"shardpost/internal/config"
	"shardpost/internal/delivery"
	"shardpost/internal/version"
)

// adminServers are the configured admin API listeners (both optional).
type adminServers struct {
	unixSrv *http.Server
	unixLn  net.Listener
	tlsSrv  *http.Server
}

// newAdminServers builds the admin API listeners from the configuration;
// nothing is opened when neither the socket nor the TLS address is set.
func newAdminServers(log *slog.Logger, settings *config.Config, authn auth.Authenticator, deliverer *delivery.Service, store *cephstore.Store) (*adminServers, error) {
	c := settings.Admin
	a := &adminServers{}

	if c.Socket == "" && c.Addr == "" {
		return a, nil
	}

	clients, scopes := map[string][]string{}, map[string][]string{}
	for _, cl := range c.Clients {
		clients[cl.Identity] = cl.Roles

		if len(cl.Accounts) > 0 {
			scopes[cl.Identity] = cl.Accounts
		}
	}

	importer := importStore{store}
	instance, _ := os.Hostname()

	s := admin.New(admin.Options{Delivery: deliverer, Auth: authn, Log: log.With("component", "admin"), Version: version.Version,
		Hostname: settings.Server.LmtpHostname, MaxMessageBytes: cephstore.MaxMessageLen, SocketRoles: c.SocketRoles, Clients: clients,
		Import: importer, ImportLockTTL: time.Duration(c.Import.LockTTLSeconds) * time.Second,
		ImportMaxBatchBytes: c.Import.MaxBatchBytes, ImportMaxBatchMessages: c.Import.MaxBatchMessages, ImportMaxInventory: c.Import.MaxInventory, ImportMaxChanges: c.Import.MaxChanges,
		ImportShardSize: c.Import.ShardSize, Accounts: importer,
		TraceDefaultDuration: time.Duration(settings.Trace.DefaultDurationSeconds) * time.Second,
		TraceMaxDuration:     time.Duration(settings.Trace.MaxDurationSeconds) * time.Second,
		ClientScopes:         scopes, Instance: instance, StartedAt: time.Now(), ConfigFingerprint: settings.Fingerprint(),
		StorePing: store.Ping})

	if c.Addr != "" {
		srv, err := s.TLSServer(c.TLSCert, c.TLSKey, c.ClientCA)
		if err != nil {
			return nil, err
		}

		srv.Addr, a.tlsSrv = c.Addr, srv
	}

	if c.Socket != "" {
		ln, err := admin.ListenUnix(c.Socket)
		if err != nil {
			return nil, fmt.Errorf("admin socket: %w", err)
		}

		a.unixLn, a.unixSrv = ln, s.UnixServer()
	}

	return a, nil
}

// start serves the configured listeners; fatal errors go to errc.
func (a *adminServers) start(errc chan<- error) {
	if a.unixSrv != nil {
		go func() { errc <- fmt.Errorf("admin socket: %w", a.unixSrv.Serve(a.unixLn)) }()
	}

	if a.tlsSrv != nil {
		// The certificates come from GetConfigForClient (reloaded on change).
		go func() { errc <- fmt.Errorf("admin tls: %w", a.tlsSrv.ListenAndServeTLS("", "")) }()
	}
}

// shutdown stops accepting and waits for running requests until ctx ends.
func (a *adminServers) shutdown(ctx context.Context) {
	for _, srv := range []*http.Server{a.unixSrv, a.tlsSrv} {
		if srv != nil {
			_ = srv.Shutdown(ctx)
		}
	}
}
