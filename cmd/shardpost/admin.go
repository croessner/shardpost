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
	"shardpost/internal/jobs"
	"shardpost/internal/version"
)

// adminServers are the configured admin API listeners (both optional).
type adminServers struct {
	unixSrv *http.Server
	unixLn  net.Listener
	tlsSrv  *http.Server

	// jobs is the maintenance job runner (nil when disabled).
	jobs      *jobs.Manager
	jobsKV    *cephstore.AdminKV
	jobsStop  context.CancelFunc
	jobsEnded chan struct{}
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

	if settings.Jobs.Enabled {
		if err := a.newJobs(log, settings, store, importer, instance); err != nil {
			return nil, err
		}
	}

	s := admin.New(admin.Options{Delivery: deliverer, Auth: authn, Log: log.With("component", "admin"), Version: version.Version,
		Hostname: settings.Server.LmtpHostname, MaxMessageBytes: cephstore.MaxMessageLen, SocketRoles: c.SocketRoles, Clients: clients,
		Import: importer, ImportLockTTL: time.Duration(c.Import.LockTTLSeconds) * time.Second,
		ImportMaxBatchBytes: c.Import.MaxBatchBytes, ImportMaxBatchMessages: c.Import.MaxBatchMessages, ImportMaxInventory: c.Import.MaxInventory, ImportMaxChanges: c.Import.MaxChanges,
		ImportShardSize: c.Import.ShardSize, Accounts: importer,
		TraceDefaultDuration: time.Duration(settings.Trace.DefaultDurationSeconds) * time.Second,
		TraceMaxDuration:     time.Duration(settings.Trace.MaxDurationSeconds) * time.Second,
		ClientScopes:         scopes, Instance: instance, StartedAt: time.Now(), ConfigFingerprint: settings.Fingerprint(),
		StorePing: store.Ping, Jobs: a.jobs, Learning: cephstore.LearningQueue{Store: store}, SearchScanLimit: c.SearchScanLimit,
		SharedPrefix: settings.Server.SharedPrefix, PublicPrefix: settings.Server.PublicPrefix})

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

// newJobs builds the maintenance job system (roadmap 8c).
func (a *adminServers) newJobs(log *slog.Logger, settings *config.Config, store *cephstore.Store, importer importStore, instance string) error {
	kv, err := store.AdminKV()
	if err != nil {
		return fmt.Errorf("admin store: %w", err)
	}

	c := settings.Jobs
	sec := func(n int) time.Duration { return time.Duration(n) * time.Second }
	env := &jobs.Env{Accounts: importer, Log: log.With("component", "jobs"), Learning: cephstore.LearningQueue{Store: store}}

	a.jobsKV = kv
	a.jobs = jobs.NewManager(kv, env, jobs.Config{Poll: sec(c.PollSeconds), LeaseTTL: sec(c.LeaseSeconds),
		Retention: time.Duration(c.RetentionDays) * 24 * time.Hour, PlanTTL: sec(c.PlanTTLSeconds), MaxParallel: c.MaxParallel,
		Settle: sec(c.SettleSeconds)},
		instance, jobs.Kinds(time.Duration(settings.Scrub.GraceSeconds)*time.Second)...)

	return nil
}

// start serves the configured listeners; fatal errors go to errc.
func (a *adminServers) start(errc chan<- error) {
	if a.jobs != nil {
		ctx, cancel := context.WithCancel(context.Background())
		a.jobsStop, a.jobsEnded = cancel, make(chan struct{})

		go func() { a.jobs.Run(ctx); close(a.jobsEnded) }()
	}

	if a.unixSrv != nil {
		go func() { errc <- fmt.Errorf("admin socket: %w", a.unixSrv.Serve(a.unixLn)) }()
	}

	if a.tlsSrv != nil {
		// The certificates come from GetConfigForClient (reloaded on change).
		go func() { errc <- fmt.Errorf("admin tls: %w", a.tlsSrv.ListenAndServeTLS("", "")) }()
	}
}

// shutdown stops accepting and waits for running requests until ctx ends;
// running jobs are handed over to the other servers.
func (a *adminServers) shutdown(ctx context.Context) {
	if a.jobsStop != nil {
		a.jobsStop()

		select {
		case <-a.jobsEnded:
		case <-ctx.Done():
		}

		a.jobsKV.Close()
	}

	for _, srv := range []*http.Server{a.unixSrv, a.tlsSrv} {
		if srv != nil {
			_ = srv.Shutdown(ctx)
		}
	}
}
