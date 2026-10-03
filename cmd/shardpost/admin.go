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
	"shardpost/internal/backfill"
	"shardpost/internal/cephstore"
	"shardpost/internal/config"
	"shardpost/internal/delivery"
	"shardpost/internal/jobs"
	"shardpost/internal/learnindex"

	"shardpost/internal/model"
	"shardpost/internal/sendqueue"
	"shardpost/internal/sharenotice"
	"shardpost/internal/version"
)

// adminServers are the configured admin API listeners (both optional).
type adminServers struct {
	unixSrv *http.Server
	unixLn  net.Listener
	tlsSrv  *http.Server
	// api is the handler behind them; its import reshard runs stop at
	// shutdown, before the store closes.
	api *admin.Server
}

// newAdminServers builds the admin API listeners from the configuration;
// nothing is opened when neither the socket nor the TLS address is set.
// The job manager and the learning queue are the background's (the job
// system runs there, or in a runner pod); the API submits, lists and
// cancels through them.
func newAdminServers(log *slog.Logger, settings *config.Config, authn auth.Authenticator, deliverer *delivery.Service, store *cephstore.Store,
	notices *sharenotice.Recorder, flushLog *auth.FlushLog, sendq *sendqueue.Service, kv *cephstore.AdminKV,
	jobsMgr *jobs.Manager, learning *learnindex.Queue,
) (*adminServers, error) {
	c := settings.Admin
	a := &adminServers{}

	if c.Socket == "" && c.Addr == "" {
		return a, nil
	}

	clients, scopes, all := adminClients(c.Clients)
	instance, _ := os.Hostname()

	// A typed nil would not compare equal to nil in the interface.
	var flusher admin.AuthFlusher
	if flushLog != nil {
		flusher = flushLog
	}

	s := admin.New(adminOptions(log, settings, authn, deliverer, store, notices, flusher, sendq, kv, jobsMgr, learning, clients, scopes, all, instance))
	a.api = s

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

// adminOptions are the options of the admin API.
func adminOptions(log *slog.Logger, settings *config.Config, authn auth.Authenticator, deliverer *delivery.Service, store *cephstore.Store,
	notices *sharenotice.Recorder, flusher admin.AuthFlusher, sendq *sendqueue.Service, kv *cephstore.AdminKV, jobsMgr *jobs.Manager,
	learning *learnindex.Queue, clients, scopes map[string][]string, all []string, instance string,
) admin.Options {
	c := settings.Admin
	importer := importStore{store}

	return admin.Options{AuthFlush: flusher, Delivery: deliverer, Auth: authn, Log: log.With("component", "admin"), Version: version.Version,
		Hostname: settings.Server.LmtpHostname, MaxMessageBytes: cephstore.MaxMessageLen, SocketRoles: c.SocketRoles, Clients: clients,
		Import: importer, ImportLockTTL: time.Duration(c.Import.LockTTLSeconds) * time.Second,
		ImportMaxBatchBytes: c.Import.MaxBatchBytes, ImportMaxBatchMessages: c.Import.MaxBatchMessages, ImportMaxInventory: c.Import.MaxInventory, ImportMaxChanges: c.Import.MaxChanges,
		ImportShardSize: c.Import.ShardSize, Accounts: importer,
		TraceDefaultDuration: time.Duration(settings.Trace.DefaultDurationSeconds) * time.Second,
		TraceMaxDuration:     time.Duration(settings.Trace.MaxDurationSeconds) * time.Second,
		ClientScopes:         scopes, AllAccountsClients: all, Instance: instance, StartedAt: time.Now(), ConfigFingerprint: settings.Fingerprint(),
		StorePing: store.Ping, Placement: store.Placement, Jobs: jobsMgr, Directory: store, Learning: learning, Submissions: submissionAdmin(sendq), SearchScanLimit: c.SearchScanLimit,
		SharedPrefix: settings.Server.SharedPrefix, PublicPrefix: settings.Server.PublicPrefix, ShareNotices: notices,
		KV: kv, FTSLayouts: store, ImportReshardSettle: time.Duration(settings.Jobs.SettleSeconds) * time.Second,
		ImportReshardSync: time.Duration(settings.FTS.ImportReshardSyncSeconds) * time.Second, FTSStatsTestAccount: settings.FTS.StatsTestAccount}
}

// submissionAdmin is the queue as the admin API's interface (nil, not a
// typed nil, without one).
func submissionAdmin(q *sendqueue.Service) admin.SubmissionAdmin {
	if q == nil {
		return nil
	}

	return q
}

// adminClients maps the configured client identities to their roles and,
// for clients with an account scope, to their account patterns; all are
// the identities with the scope of all accounts.
func adminClients(cs []config.AdminClient) (clients, scopes map[string][]string, all []string) {
	clients, scopes = map[string][]string{}, map[string][]string{}

	for _, cl := range cs {
		clients[cl.Identity] = cl.Roles

		if len(cl.Accounts) > 0 {
			scopes[cl.Identity] = cl.Accounts
		}

		if cl.AllAccounts {
			all = append(all, cl.Identity)
		}
	}

	return clients, scopes, all
}

// newJobsManager builds the maintenance job system (roadmap 8c): the
// manager over an admin store of its own (closed by the caller after
// the manager stopped), for a server's background or a runner pod.
func newJobsManager(log *slog.Logger, settings *config.Config, store *cephstore.Store, learning *learnindex.Queue,
	sendq *sendqueue.Service,
) (*jobs.Manager, *cephstore.AdminKV, error) {
	kv, err := store.AdminKV()
	if err != nil {
		return nil, nil, fmt.Errorf("admin store: %w", err)
	}

	c := settings.Jobs
	sec := func(n int) time.Duration { return time.Duration(n) * time.Second }
	instance, _ := os.Hostname()
	env := &jobs.Env{Accounts: importStore{store}, Log: log.With("component", "jobs"), Learning: learning, Directory: store, Layout3: store,
		FTSOnline: store, QuotaOnline: store, MailboxLock: store}

	// validated with the configuration
	win, err := settings.FTS.Window()
	if err != nil {
		kv.Close()
		return nil, nil, err
	}

	if sendq != nil {
		env.Submissions = sendq
	}

	// The fts_due worker reshards the accounts the writers queued.
	if settings.FTS.AutoReshard {
		q, lock, err := store.FTSDueQueue()
		if err != nil {
			kv.Close()
			return nil, nil, fmt.Errorf("fts reshard queue: %w", err)
		}

		env.FTSDue = &jobs.FTSDue{Queue: q, Lock: lock}
	}

	mgr := jobs.NewManager(kv, env, jobs.Config{Poll: sec(c.PollSeconds), LeaseTTL: sec(c.LeaseSeconds),
		Retention: time.Duration(c.RetentionDays) * 24 * time.Hour, PlanTTL: sec(c.PlanTTLSeconds), MaxParallel: c.MaxParallel,
		Settle: sec(c.SettleSeconds), ExpiryPoll: sec(c.ExpirySeconds), BatchParallel: c.BatchParallel,
		ScrubParallel: settings.Scrub.Parallel, ScrubInterval: time.Duration(settings.Scrub.IntervalDays) * 24 * time.Hour,
		FTSBuild: jobs.FTSBuild{OnlineAuto: settings.FTS.OnlineBuild == config.OnlineBuildAuto, Window: win, MaxRunning: settings.FTS.BuildMaxRunning,
			MsPerMessageCopy: settings.FTS.BuildMsPerMessageCopy, MsPerMessageReindex: settings.FTS.BuildMsPerMessageReindex,
			UrgentFactor: settings.FTS.ReshardUrgentFactor},
		QuotaOnlineAuto:         settings.Quota.OnlineRecount == config.OnlineAuto,
		MailboxDeleteOnlineAuto: settings.Jobs.MailboxDeleteOnline == config.OnlineAuto,
		MailboxLockBarrier:      sec(c.MailboxLockBarrierSeconds),
		MailboxLockSettle:       max(sec(c.SettleSeconds), jobs.MinLockSettle),
		DeadIntent:              sec(settings.Quota.RecountDeadIntentSeconds)},
		instance, jobs.Kinds(time.Duration(settings.Scrub.GraceSeconds)*time.Second)...)

	warnUnbackfilled(log, kv)

	return mgr, kv, nil
}

// warnUnbackfilled reports at startup how admin.kv_index=read is met.
// Without a completed "shardpost index backfill kv" the records of older
// servers would be missing from the indexes, so the store behaves as in
// write mode until the marker exists (it logs that as a warning itself).
func warnUnbackfilled(log *slog.Logger, kv *cephstore.AdminKV) {
	if kv.ConfiguredMode() != model.KVIndexRead || kv.IndexMode() != model.KVIndexRead {
		return
	}

	if rep, ok, err := backfill.KVBackfilled(kv); err == nil && ok {
		log.Info("admin.kv_index=read", "backfill_started", rep.Started, "backfill_finished", rep.Finished)
	}
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

// shutdown stops accepting and waits for running requests until ctx ends
// (the job runner, if this process runs one, stops with the background
// and hands its jobs over).
func (a *adminServers) shutdown(ctx context.Context) {
	for _, srv := range []*http.Server{a.unixSrv, a.tlsSrv} {
		if srv != nil {
			_ = srv.Shutdown(ctx)
		}
	}

	// srv.Shutdown does not wait for goroutines: the import reshard runs
	// end here, before the caller closes the store (R4.2).
	if a.api != nil {
		a.api.Close()
	}
}
