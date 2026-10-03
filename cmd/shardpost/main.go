// Command shardpost runs the ShardPost mail server: IMAP, POP3, LMTP and
// ManageSieve backed by RADOS. Configuration comes from YAML and environment.
package main

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"shardpost/internal/config"
	"shardpost/internal/jobs"
	"shardpost/internal/learnindex"
	"shardpost/internal/logging"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
	// The zone of fts.reshard_window_tz: the image need not carry the
	// zone files.
	_ "time/tzdata"

	"github.com/prometheus/client_golang/prometheus/promhttp"

	"shardpost/internal/auth"
	"shardpost/internal/cephstore"
	"shardpost/internal/certfile"
	"shardpost/internal/charset"
	"shardpost/internal/delivery"
	"shardpost/internal/extprograms"
	"shardpost/internal/fts"
	"shardpost/internal/imapproto"
	"shardpost/internal/imapsieve"
	"shardpost/internal/jmap"
	"shardpost/internal/lmtpd"
	"shardpost/internal/membudget"
	"shardpost/internal/model"
	"shardpost/internal/outbound"
	"shardpost/internal/pop3proto"
	"shardpost/internal/provision"
	"shardpost/internal/quota"
	"shardpost/internal/rookcreds"
	"shardpost/internal/sendqueue"
	"shardpost/internal/sharenotice"
	"shardpost/internal/sieve"
	"shardpost/internal/telemetry"
	"shardpost/internal/version"
)

// storeAdapter exposes the RADOS store through the protocol layer's
// interface. Accounts under an import lock are refused (temporary
// failure on every protocol); the admin import uses importStore.
type storeAdapter struct{ *cephstore.Store }

func (s storeAdapter) User(name string) (model.User, error) {
	u, err := s.Store.User(name)
	if err != nil {
		return nil, err
	}

	if err := model.RefuseDuringImport(u); err != nil {
		u.Close()
		return nil, err
	}

	return u, nil
}

// importStore is the RADOS store without the import refusal, for the
// admin import that holds the lock.
type importStore struct{ *cephstore.Store }

func (s importStore) User(name string) (model.User, error) { return s.Store.User(name) }

// draining flips readiness to "not ready" during shutdown.
var draining atomic.Bool

func main() {
	// "bench" has flags of its own and reports on standard output.
	if pre, args, ok := splitBench(os.Args[1:]); ok {
		os.Exit(runBench(pre, args))
	}

	// "system" (the placement tools) likewise.
	if pre, args, ok := splitSystem(os.Args[1:]); ok {
		os.Exit(runSystem(pre, args))
	}

	log := logging.New(os.Stdout, "info")

	settings, command, err := loadConfig(os.Args[1:])
	if err == nil {
		log = logging.New(os.Stdout, settings.Logging.Level).With("service", "shardpost")
		slog.SetDefault(log)
	}

	if err == nil && command == commandCheck {
		if err = checkGlobalScripts(settings, sieve.ParseSettings(settings.Sieve.Settings)); err == nil {
			log.Info("configuration valid")
			return
		}
	}

	if err == nil {
		err = run(log, settings, command)
	}

	if err != nil {
		log.Error("fatal", "err", err)
		os.Exit(1)
	}
}

// Sub-commands understood on the command line.
const (
	commandServe = "serve"
	commandScrub = "scrub"
	// commandScrubBatch submits the scrub of every account as a batch
	// job the servers run ("shardpost scrub --batch").
	commandScrubBatch = "scrub --batch"
	// commandScrubSweep lists both pools in every namespace and removes
	// what no account names ("shardpost scrub --sweep [--repair]
	// [--restart]", the flags follow in the command).
	commandScrubSweep = "scrub --sweep"
	commandCheck      = "check"
	commandPush       = "push"
	// commandRunner runs the background workers without listeners
	// ("shardpost runner", stage B18b).
	commandRunner = "runner"
	// commandBackfillAccounts fills the account directory and repairs it
	// ("shardpost index backfill accounts [--restart]").
	commandBackfillAccounts = "index backfill accounts"
	commandBackfillRestart  = "index backfill accounts --restart"
	// commandBackfillKV fills the admin store's hint indexes and due
	// queues ("shardpost index backfill kv [--restart]").
	commandBackfillKV        = "index backfill kv"
	commandBackfillKVRestart = "index backfill kv --restart"
	// commandShowKV prints the state of the admin store indexes and
	// queues ("shardpost index show kv [<key>...]", read-only); the keys
	// follow in the command, separated by spaces.
	commandShowKV = "index show kv"
	// commandBackfillLearning fills the learning queue's due queue and
	// owner index ("shardpost index backfill learning [--restart]").
	commandBackfillLearning        = "index backfill learning"
	commandBackfillLearningRestart = "index backfill learning --restart"
	// commandShowLearning prints the state of the learning queue indexes
	// ("shardpost index show learning [<id>...]", read-only).
	commandShowLearning = "index show learning"
)

// capabilityEnabled is the ManageSieve capability value for enabled features.
const capabilityEnabled = "yes"

func run(log *slog.Logger, settings *config.Config, command string) error {
	log.Info("logging configured", "event", "logging_configured", "log_level", settings.Logging.Level)

	sigCtx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	ctx, cancel := context.WithCancel(sigCtx)
	defer cancel()

	shutdownTracing := telemetry.Setup(ctx, settings.Telemetry.ServiceName, log, settings.Telemetry)
	defer func() { _ = shutdownTracing(context.Background()) }()

	cfg := cephConfig(settings)
	programs := extprograms.New(settings.ExtPrograms)

	// Quota warnings and the over-flag check run only while serving;
	// the scrubber's recount reports nothing.
	var quotaSvc *quota.Service
	if command == commandServe {
		quotaSvc = quota.NewService(settings.Quota, programs, log.With("component", "quota"))
		if quotaSvc != nil {
			cfg.OnQuotaChange = quotaSvc.Changed
		}
	}

	rotate, err := rookCredentials(ctx, log, settings, &cfg)
	if err != nil {
		return err
	}

	membudget.SetLimit(settings.Server.MemoryBudgetBytes)

	if err := textSettings(log, settings); err != nil {
		return err
	}

	lost := watchPlacement(&cfg, command, cancel)

	store, err := cephstore.Open(cfg, log)
	if err != nil {
		return err
	}
	defer store.Close()

	if rotate != nil {
		rotate(store)
	}

	err = dispatch(ctx, log, settings, command, store, programs, quotaSvc)

	select {
	case lerr := <-lost:
		return fmt.Errorf("system placement lost, the store must not be used any more: %w", lerr)
	default:
		return err
	}
}

// watchPlacement makes a server, the push worker and the job runner
// check the placement of the store's own namespaces every minute: a
// contradiction read positively (a migration began, the system pool was
// withdrawn) turns readiness off, cancels the run (the server drains)
// and is reported on the returned channel, so that the process ends
// with an error. The short-lived commands check it once, when the store
// opens.
func watchPlacement(cfg *cephstore.Config, command string, cancel func()) <-chan error {
	lost := make(chan error, 1)

	if command != commandServe && command != commandPush && command != commandRunner {
		return lost
	}

	cfg.OnPlacementLost = func(err error) {
		draining.Store(true)

		select {
		case lost <- err:
		default:
		}

		cancel()
	}

	return lost
}

// dispatch runs the command on the opened store. Every command that
// writes the store's own namespaces is registered for its runtime with
// its role; "index show" only reads.
func dispatch(ctx context.Context, log *slog.Logger, settings *config.Config, command string, store *cephstore.Store,
	programs *extprograms.Runner, quotaSvc *quota.Service,
) error {
	if run, ok := scrubCommand(ctx, command); ok {
		role := model.RoleScrub
		if strings.HasPrefix(command, commandScrubSweep) {
			role = model.RoleSweep
		}

		return registered(ctx, log, store, role, func() error { return run(store, log, settings) })
	}

	if backfill, ok := backfillCommand(command); ok {
		if strings.HasPrefix(command, "index show") {
			return backfill(store, log, settings)
		}

		return registered(ctx, log, store, model.RoleBackfill, func() error { return backfill(store, log, settings) })
	}

	// "shardpost push" runs the JMAP push worker; its registration shows
	// the partitions it holds and the accounts it watches.
	if command == commandPush {
		status := &jmap.PushStatus{}

		return registeredWith(ctx, log, store, model.RolePush, status.Get, func() error {
			return runPush(ctx, log, settings, store, status)
		})
	}

	// "shardpost runner" runs the background workers without listeners.
	if command == commandRunner {
		return runRunner(ctx, log, settings, store, programs)
	}

	// "shardpost runner" runs the background workers without listeners.
	if command == commandRunner {
		return runRunner(ctx, log, settings, store, programs)
	}

	logFTSLayout(log, settings, store)

	return serve(ctx, log, settings, store, programs, quotaSvc)
}

// registered runs fn while the process is registered with role.
func registered(ctx context.Context, log *slog.Logger, store *cephstore.Store, role string, fn func() error) error {
	return registeredWith(ctx, log, store, role, nil, fn)
}

// registeredWith is registered with a status supplier for the record.
func registeredWith(ctx context.Context, log *slog.Logger, store *cephstore.Store, role string,
	status func() (partitions, watched int), fn func() error,
) error {
	_, stop, err := startRegistration(ctx, log, store, role, status, nil)
	if err != nil {
		return err
	}
	defer stop()

	return fn()
}

// textSettings applies the text preparation settings. Load validated
// them; they are package settings because the index, IMAP SEARCH and the
// preview share one text preparation.
func textSettings(log *slog.Logger, settings *config.Config) error {
	if err := charset.SetFallback(settings.FTS.FallbackCharset); err != nil {
		return err
	}

	if err := fts.SetHTMLMode(settings.FTS.HTML); err != nil {
		return err
	}

	if err := fts.SetAttachedHeaders(settings.FTS.AttachedHeaders); err != nil {
		return err
	}

	for _, w := range settings.FTS.ShardTargetWarnings() {
		log.Warn(w)
	}

	return nil
}

// cephConfig maps the configuration onto the RADOS store settings.
func cephConfig(settings *config.Config) cephstore.Config {
	return cephstore.Config{
		User:          settings.Ceph.User,
		Key:           settings.Ceph.Key,
		MonHost:       settings.Ceph.MonHost,
		IndexPool:     settings.Ceph.IndexPool,
		DataPool:      settings.Ceph.DataPool,
		SystemPool:    settings.Ceph.SystemPool,
		ClientOptions: settings.Ceph.ClientOptions,
		ChunkSize:     settings.Ceph.ChunkSize,
		FTSShards:     int(settings.Server.FtsShards),
		FTS: cephstore.FTSConfig{Layout: settings.FTS.Layout, ShardsMin: settings.FTS.ShardsMin, ShardsMax: settings.FTS.ShardsMax,
			ShardTargetKeys: settings.FTS.ShardTargetKeys, DictShardsMax: settings.FTS.DictShardsMax,
			DictTargetKeys: settings.FTS.DictTargetKeys, ConvertBufferKeys: settings.FTS.ConvertBufferKeys,
			ConvertWriters: settings.FTS.ConvertWriters, AutoReshard: settings.FTS.AutoReshard,
			FrozenWait: time.Duration(settings.FTS.FrozenWaitSeconds) * time.Second,
			DictFilter: settings.FTS.DictIDFilter() > 0, DictIDMinRunes: settings.FTS.DictIDMinRunes,
			AttachedHeaders:    settings.FTS.AttachedHeaders,
			BuildCatchupRounds: settings.FTS.BuildCatchupRounds,
			BuildHold:          time.Duration(settings.FTS.BuildHoldMS) * time.Millisecond, BuildNoHold: settings.FTS.BuildHoldMS == 0,
			ReindexBatchDocs: settings.FTS.ReindexBatchDocs, ReindexBatchBytes: settings.FTS.ReindexBatchBytes},
		Quota:       cephstore.QuotaLimit{Bytes: settings.Server.QuotaBytes, Msgs: settings.Server.QuotaMsgs},
		PublicQuota: cephstore.QuotaLimit{Bytes: settings.Server.PublicQuotaBytes, Msgs: settings.Server.PublicQuotaMsgs},
		QuotaRules:  quota.NewPolicy(settings.Quota),
		ShardSize:   uint32(settings.Server.MboxShardSize),

		AccountLogRetention: int(settings.Server.AccountLogRetention),
		KVIndex:             model.KVIndexMode(settings.Admin.KVIndex),
		KVQueueShards:       settings.Admin.KVQueueShards,
		LearningIndex:       model.KVIndexMode(settings.IMAPSieve.Learning.Index),
		LearningQueueShards: settings.IMAPSieve.Learning.QueueShards,
		Limits:              accountLimits(settings.Limits),
		UploadTTL:           time.Duration(settings.JMAP.UploadTTLSeconds) * time.Second,
		OrphanSource:        model.OrphanSource(settings.Scrub.OrphanSource),
		IntentGrace:         time.Duration(settings.Scrub.IntentGraceSeconds) * time.Second,
		PurgeListing:        settings.Purge.Listing,
	}
}

// accountLimits converts the configured account limits for the store.
func accountLimits(l config.Limits) model.Limits {
	return model.Limits{
		Mailboxes:          int(l.Mailboxes),
		MailboxNames:       int(l.MailboxNames),
		KeywordsPerMailbox: int(l.KeywordsPerMailbox),
		MetadataEntries:    int(l.MetadataEntries),
		SharedPointers:     int(l.SharedPointers),
	}
}

// rookCredentials fills in the key and the mon list from Rook when no key is
// configured (inside Kubernetes). It returns a function that starts the key
// rotation watcher for the opened store, or nil when no rotation applies.
func rookCredentials(ctx context.Context, log *slog.Logger, settings *config.Config, cfg *cephstore.Config) (func(*cephstore.Store), error) {
	if cfg.Key != "" {
		return nil, nil
	}

	creds, err := rookcreds.Fetch(ctx,
		settings.Rook.Namespace,
		settings.Rook.ClientSecret,
		cfg.User)
	if err != nil {
		return nil, fmt.Errorf("fetch ceph credentials: %w", err)
	}

	cfg.Key = creds.Key
	if cfg.MonHost == "" {
		cfg.MonHost = creds.MonHost
	}

	startKey := creds.Key
	user := cfg.User

	return func(store *cephstore.Store) {
		go rookcreds.Watch(ctx, settings.Rook.Namespace, settings.Rook.ClientSecret, user, startKey,
			time.Duration(settings.Server.KeyPollSeconds)*time.Second, func(key string) error {
				if err := store.Rotate(key, ""); err != nil {
					log.Error("cephx key rotation failed, will retry", "err", err)
					return err
				}

				log.Info("cephx key rotated, new connections use the new key")

				return nil
			})
	}, nil
}

// loadTLS loads the optional certificate: STARTTLS on the plain ports,
// implicit TLS on IMAPS_ADDR. It returns nil when no certificate is set.
func loadTLS(log *slog.Logger, settings *config.Config) (*tls.Config, error) {
	cert, key := settings.TLS.Cert, settings.TLS.Key
	if cert == "" || key == "" {
		return nil, nil
	}

	return certConfig(log, cert, key, settings)
}

// certConfig is a server tls.Config for a certificate pair that picks up a
// renewed pair every tls.reload_seconds, so a certificate cert-manager
// renews reaches the listener without a restart.
func certConfig(log *slog.Logger, cert, key string, settings *config.Config) (*tls.Config, error) {
	src, err := certfile.New(cert, key, time.Duration(settings.TLS.ReloadSeconds)*time.Second, log)
	if err != nil {
		return nil, err
	}

	return src.Config(), nil
}

// newIMAPSieve builds the IMAPSieve engine and, when enabled, starts the
// learning worker. The returned function stops the worker and waits for it.
func newIMAPSieve(log *slog.Logger, settings *config.Config, store *cephstore.Store, programs *extprograms.Runner,
	sieveCfg map[string]string, learner *imapsieve.LearningWorker,
) *imapsieve.Engine {
	imapSieve := imapsieve.New(settings.IMAPSieve, storeAdapter{store}, log)
	imapSieve.Programs = programs

	// The learner enqueues and prepares; whether this process also runs
	// the queue is the workers' business (startLearning).
	if learner != nil {
		imapSieve.Learner = learner
	}

	imapSieve.Sender = &outbound.Relay{Addr: settings.SMTP.Addr, HELO: settings.SMTP.Helo, StartTLS: settings.SMTP.Starttls, Implicit: settings.SMTP.TLS, User: settings.SMTP.User, Password: settings.SMTP.Password, ServerName: settings.SMTP.ServerName, Timeout: time.Duration(settings.IMAPSieve.TimeoutMS) * time.Millisecond}
	imapSieve.GlobalScripts = settings.Sieve.GlobalDir
	imapSieve.SharedPrefix = settings.Server.SharedPrefix
	imapSieve.SieveConfig = sieveCfg

	return imapSieve
}

// autocreate maps the configured mailboxes to what the IMAP server
// provisions for an account.
func autocreate(folders []config.AutocreateFolder) []imapproto.SpecialFolder {
	out := make([]imapproto.SpecialFolder, 0, len(folders))
	for _, f := range folders {
		out = append(out, imapproto.SpecialFolder{Name: f.Name, Use: f.SpecialUse, Subscribe: f.Subscribed()})
	}

	return out
}

// checkGlobalScripts checks the Sieve settings (sieve.CheckSettings) and
// compiles the scripts that run before and after the account's own for
// every delivery: a setting that cannot work, a directory that cannot be
// read or a script that does not compile is a startup error, not a
// silent gap in the filtering of every account.
func checkGlobalScripts(settings *config.Config, sieveCfg map[string]string) error {
	if err := sieve.CheckSettings(sieveCfg); err != nil {
		return fmt.Errorf("sieve.settings: %w", err)
	}

	hooks := sieve.Hooks{Include: sieve.IncludeLoader(nil, settings.Sieve.GlobalDir), Config: sieveCfg}

	if err := sieve.CompileDir(settings.Sieve.BeforeDir, sieve.LabelBefore, hooks); err != nil {
		return fmt.Errorf("sieve.before_dir: %w", err)
	}

	if err := sieve.CompileDir(settings.Sieve.AfterDir, sieve.LabelAfter, hooks); err != nil {
		return fmt.Errorf("sieve.after_dir: %w", err)
	}

	return nil
}

// manageSieveConfig derives the ManageSieve capability settings.
func manageSieveConfig(settings *config.Config, sieveCfg map[string]string, programs *extprograms.Runner) map[string]string {
	manageConfig := map[string]string{}
	for k, v := range sieveCfg {
		manageConfig[k] = v
	}

	if settings.IMAPSieve.Enabled && settings.IMAPSieve.UserScripts {
		manageConfig["imapsieve_enabled"] = capabilityEnabled
	}

	if programs.Enabled() {
		manageConfig["extprograms_enabled"] = capabilityEnabled
	}

	return manageConfig
}

// newHTTPServer serves metrics, liveness and readiness.
func newHTTPServer(settings *config.Config, store *cephstore.Store) *http.Server {
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.Handler())
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = fmt.Fprintln(w, "ok") })
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) {
		if draining.Load() {
			http.Error(w, "draining", http.StatusServiceUnavailable)
			return
		}

		if err := store.Ping(); err != nil {
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
			return
		}

		_, _ = fmt.Fprintln(w, "ok")
	})

	return &http.Server{Addr: settings.Server.HTTPAddr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
}

// servers groups the protocol servers for start-up and drain.
type servers struct {
	imap  *imapproto.Server
	pop3  *pop3proto.Server
	sieve *sieve.Server
	lmtp  interface {
		Serve(net.Listener) error
		Close() error
	}
	http *http.Server
	// jmap serves the JMAP front-end on its own listener (nil: off).
	jmap    *http.Server
	jmapTLS *tls.Config
	// jmapFront is the front-end behind jmap; closing it waits for its
	// background work, which uses the store.
	jmapFront *jmap.Server
	// jmapKV is the admin store the JMAP push subscriptions are kept in.
	jmapKV *cephstore.AdminKV
	// jobs is the job manager of the admin API (nil without the job
	// system), learning the learning queue it administers; both are the
	// background's (startBackground).
	jobs     *jobs.Manager
	learning *learnindex.Queue
	// kv is the admin store of the server registration, the full-text
	// layout mark and the admin API (open for the server's lifetime).
	kv *cephstore.AdminKV
	// sieveConfig are the Sieve settings ManageSieve compiles with; JMAP
	// compiles scripts with the same.
	sieveConfig map[string]string
	// delivery is shared by LMTP and the local delivery API.
	delivery *delivery.Service
	admin    *adminServers
	// notices writes the share notifications (RFC 9670 3) of every ACL
	// change: IMAP SETACL, the admin API and JMAP shareWith.
	notices *sharenotice.Recorder
	// sendq is the delayed submission queue (nil without the admin store
	// indexes).
	sendq *sendqueue.Service
}

// logFTSLayout logs the layout new full-text indexes get: fts.layout
// (default 3) takes effect only with the cluster mark. A failed read is
// not fatal; the mark is read again whenever an index is created.
func logFTSLayout(log *slog.Logger, settings *config.Config, store *cephstore.Store) {
	if err := store.LogFTSLayout(); err != nil {
		log.Warn("full-text layout: cannot read the layout 3 mark", "event", "fts_layout", "configured", settings.FTS.Layout, "err", err)
	}
}

// serve starts all listeners and blocks until a signal or a fatal listener
// error, then drains.
func serve(ctx context.Context, log *slog.Logger, settings *config.Config, store *cephstore.Store,
	programs *extprograms.Runner, quotaSvc *quota.Service,
) error {
	authn, cache, err := buildAuth(log, settings)
	if err != nil {
		return err
	}

	flushLog, stopFlushLog, err := startFlushLog(ctx, log, store, cache)
	if err != nil {
		return err
	}
	defer stopFlushLog()

	tlsCfg, err := loadTLS(log, settings)
	if err != nil {
		return err
	}

	sieveCfg := sieve.ParseSettings(settings.Sieve.Settings)
	provision.Quota = quotaSvc

	if err := checkGlobalScripts(settings, sieveCfg); err != nil {
		return err
	}

	bg, err := startBackground(ctx, log, settings, store, programs, sieveCfg)
	if err != nil {
		return err
	}
	defer bg.stop()

	srv := newServers(log, settings, store, authn, tlsCfg, bg.imapSieve, programs, sieveCfg)
	srv.sendq, srv.kv, srv.jobs, srv.learning = bg.sendq, bg.kv, bg.jobs, bg.learning

	logRelay(log, settings)

	defer srv.closeJMAP() // before the caller closes the store, which the fills read

	if err := srv.setupHTTP(log, settings, store, authn, flushLog); err != nil {
		return err
	}

	errc := srv.start(settings, tlsCfg)

	log.Info("listening", "imap", settings.Server.ImapAddr, "pop3", settings.Server.Pop3Addr, "managesieve", settings.Sieve.Addr, "lmtp", settings.Server.LmtpAddr, "http", settings.Server.HTTPAddr, "tls", tlsCfg != nil,
		"jmap", settings.JMAP.Addr, "admin_socket", settings.Admin.Socket, "admin_addr", settings.Admin.Addr, "version", version.Version)

	select {
	case <-ctx.Done():
		log.Info("shutting down")
	case err := <-errc:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	}

	srv.shutdown(settings, quotaSvc)
	log.Info("shutdown complete")

	return nil
}

// scrubCommand returns the run of a scrub command: "shardpost scrub" runs
// the scrubber over all users and exits, "shardpost scrub --batch"
// submits the batch scrub job instead, "shardpost scrub --sweep" sweeps
// the pools.
func scrubCommand(ctx context.Context, command string) (func(*cephstore.Store, *slog.Logger, *config.Config) error, bool) {
	switch {
	case command == commandScrub:
		return scrub, true
	case command == commandScrubBatch:
		return scrubBatch, true
	case strings.HasPrefix(command, commandScrubSweep):
		return func(store *cephstore.Store, log *slog.Logger, settings *config.Config) error {
			return scrubSweep(ctx, store, log, settings, strings.Contains(command, flagRepair), strings.Contains(command, flagRestart))
		}, true
	}

	return nil, false
}

// processStart is when the process started (the registration's
// started_at).
var processStart = time.Now()

// registrationInstance is the name of the registration: the host name
// for a server (the pod); the host name with the role for the push
// worker, stable across restarts of its container, so that it finds
// its own membership record and its registration (stage B18a); the
// host name with the role and a random part for every other process,
// which may run in the pod of a server more than once.
func registrationInstance(role string) string {
	host, _ := os.Hostname()

	switch role {
	case model.RoleServe:
		return host
	case model.RolePush:
		return host + "-" + role
	}

	return host + "-" + role + "-" + randomSuffix()
}

// startRegistration opens an admin store of its own and registers the
// process in it (servers/<instance>, with its role and system pool)
// now and every model.ServerHeartbeat, whether or not a server runs jobs
// or an admin listener (N8, F8d). The returned function stops the
// heartbeat, records the orderly end and closes the admin store; serve
// defers it, so all of it happens before the store closes.
func startRegistration(ctx context.Context, log *slog.Logger, store *cephstore.Store, role string,
	status func() (partitions, watched int), workers *bool,
) (*cephstore.AdminKV, func(), error) {
	pool, err := store.SystemPool()
	if err != nil {
		return nil, nil, fmt.Errorf("registration: %w", err)
	}

	kv, err := store.AdminKV()
	if err != nil {
		return nil, nil, fmt.Errorf("admin store: %w", err)
	}

	reg := jobs.Registration{Instance: registrationInstance(role), Version: version.Version, Role: role, SystemPool: pool,
		StartedAt: processStart, Status: status, Workers: workers}
	// The heartbeat goes on until the returned function is called, not
	// until the signal: a server that drains still writes.
	ctx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	done := make(chan struct{})

	go func() {
		defer close(done)

		jobs.RunRegistration(ctx, kv, reg, model.ServerHeartbeat, log.With("component", "registration", "role", role))
	}()

	return kv, func() {
		cancel()
		<-done
		kv.Close()
		shutdownStep("registration")
	}, nil
}

// shutdownStep is told each step of serve's shutdown that has to come
// before another: the admin API (which uses the registration's admin
// store) before the registration closes that store. A test records the
// order.
var shutdownStep = func(string) {}

// uploadExpiryPoll is how often the upload expiry worker looks for
// expired JMAP uploads, and how often the other servers try its lease.
const uploadExpiryPoll = time.Minute

// startUploadExpiry runs the upload expiry worker (one server of the
// cluster holds its lease at a time) until the returned function is
// called.
func startUploadExpiry(ctx context.Context, store *cephstore.Store) func() {
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})

	go func() {
		defer close(done)

		store.RunUploadExpiry(ctx, uploadExpiryPoll)
	}()

	return func() {
		cancel()
		<-done
	}
}

// shutdown drains the listeners, then the quota programs, then the
// admin API: quota programs may still deliver warnings through the
// admin socket, so the admin API stops after them, with its own time
// for deliveries in progress.
func (srv *servers) shutdown(settings *config.Config, quotaSvc *quota.Service) {
	srv.drain(settings)

	qctx, cancel := context.WithTimeout(context.Background(), quotaDrain)
	quotaSvc.Close(qctx)
	cancel()

	actx, cancel := context.WithTimeout(context.Background(), adminDrain)
	srv.admin.shutdown(actx)
	cancel()
	shutdownStep("admin")
}

// newServers builds the protocol servers.
func newServers(log *slog.Logger, settings *config.Config, store *cephstore.Store, authn auth.Authenticator, tlsCfg *tls.Config,
	imapSieve *imapsieve.Engine, programs *extprograms.Runner, sieveCfg map[string]string,
) *servers {
	insecure := settings.Server.InsecureAuth
	manageConfig := manageSieveConfig(settings, sieveCfg, programs)

	srv := servers{sieveConfig: manageConfig, notices: &sharenotice.Recorder{
		Store: storeAdapter{store}, Log: log.With("component", "sharenotice"),
		Limits: sharenotice.Limits{Max: int(settings.Server.ShareNotificationsMax),
			MaxAge: time.Duration(settings.Server.ShareNotificationsRetentionSeconds) * time.Second},
	}}

	srv.imap = imapproto.New(imapproto.Options{
		IMAPSieve: imapSieve,
		Store:     storeAdapter{store}, Auth: authn, Log: log.With("proto", "imap"), TLS: tlsCfg,
		InsecureAuth:     insecure,
		Autocreate:       autocreate(settings.Server.Autocreate),
		PublicPrefix:     settings.Server.PublicPrefix,
		SharedPrefix:     settings.Server.SharedPrefix,
		MaxLiteral:       cephstore.MaxMessageLen,
		MaxConnsPerIP:    int(settings.Server.MaxConnsPerIP),
		MaxConnsPerUser:  int(settings.Server.MaxConnsPerUser),
		WatchStore:       importStore{store},
		MaxWatches:       int(settings.Server.ImapMaxWatches),
		QuotaSelfService: settings.Server.QuotaSelfService,
		WordSearch:       settings.FTS.IMAPWordSearch(),
		VerifyParallel:   settings.FTS.VerifyParallel,
		Trace: imapproto.TraceOptions{MaxLine: settings.Trace.MaxLineBytes,
			Recheck: time.Duration(settings.Trace.RecheckSeconds) * time.Second},
		ShareNotices: srv.notices,
	})
	srv.sieve = sieve.New(sieve.Options{GlobalScripts: settings.Sieve.GlobalDir, Config: manageConfig,
		Store: storeAdapter{store}, Auth: authn, Log: log.With("proto", "managesieve"), TLS: tlsCfg, InsecureAuth: insecure,
	})
	srv.pop3 = pop3proto.New(pop3proto.Options{
		Store: storeAdapter{store}, Auth: authn, Log: log.With("proto", "pop3"), TLS: tlsCfg,
		InsecureAuth: insecure, MarkSeen: settings.Server.Pop3MarkSeen,
	})
	provision.QuotaAttribute = settings.Server.QuotaAttribute

	srv.delivery = newDeliveryService(log, settings, store, programs, sieveCfg)
	srv.lmtp = lmtpd.NewServer(settings.Server.LmtpAddr, &lmtpd.Backend{Delivery: srv.delivery, Auth: authn, Log: log.With("proto", "lmtp"),
		Host: settings.Server.LmtpHostname, ServiceUser: settings.Server.LmtpAuthUser, ServicePassword: settings.Server.LmtpAuthPassword},
		tlsCfg, insecure)

	return &srv
}

// newDeliveryService builds the delivery pipeline shared by LMTP and the
// local delivery API.
func newDeliveryService(log *slog.Logger, settings *config.Config, store *cephstore.Store, programs *extprograms.Runner, sieveCfg map[string]string) *delivery.Service {
	// The Reporting-UA of a rejection names server.lmtp_hostname, or the
	// recipient's domain when it is unset — never the pod's host name.
	host := settings.Server.LmtpHostname

	return &delivery.Service{Store: storeAdapter{store}, Log: log.With("component", "delivery"),
		GlobalScripts: settings.Sieve.GlobalDir, SieveConfig: sieveCfg, Programs: programs,
		ScriptsBefore: settings.Sieve.BeforeDir, ScriptsAfter: settings.Sieve.AfterDir,
		PublicPrefix: settings.Server.PublicPrefix,
		SharedPrefix: settings.Server.SharedPrefix,
		ReturnPath:   settings.Server.LmtpReturnPath, DeliveredTo: settings.Server.LmtpDeliveredTo,
		Hostname: host,
		Relay: &outbound.Relay{Addr: settings.SMTP.Addr, HELO: settings.SMTP.Helo,
			StartTLS: settings.SMTP.Starttls, Implicit: settings.SMTP.TLS,
			User: settings.SMTP.User, Password: settings.SMTP.Password, ServerName: settings.SMTP.ServerName}}
}

// start launches every listener in its own goroutine; the first fatal error
// of each is delivered on the returned channel.
func (srv *servers) start(settings *config.Config, tlsCfg *tls.Config) <-chan error {
	// One slot per listener goroutine, so none blocks on a full channel.
	errc := make(chan error, 10)

	serveOn := func(name, addr string, implicitTLS bool, s interface{ Serve(net.Listener) error }) {
		var cfg *tls.Config
		if implicitTLS {
			cfg = tlsCfg
		}

		ln, err := listen(addr, cfg, settings)
		if err != nil {
			errc <- fmt.Errorf("%s: %w", name, err)
			return
		}

		errc <- fmt.Errorf("%s: %w", name, s.Serve(ln))
	}
	go serveOn("imap", settings.Server.ImapAddr, false, srv.imap)
	go serveOn("pop3", settings.Server.Pop3Addr, false, srv.pop3)
	go serveOn("managesieve", settings.Sieve.Addr, false, srv.sieve)

	if tlsCfg != nil {
		go serveOn("imaps", settings.Server.ImapsAddr, true, srv.imap)
		go serveOn("pop3s", settings.Server.Pop3SAddr, true, srv.pop3)
	}

	go func() {
		ln, err := listen(settings.Server.LmtpAddr, nil, settings)
		if err != nil {
			errc <- fmt.Errorf("lmtp: %w", err)
			return
		}

		errc <- fmt.Errorf("lmtp: %w", srv.lmtp.Serve(ln))
	}()
	go func() { errc <- fmt.Errorf("http: %w", srv.http.ListenAndServe()) }()

	if srv.jmap != nil {
		go func() {
			ln, err := listen(srv.jmap.Addr, srv.jmapTLS, settings)
			if err != nil {
				errc <- fmt.Errorf("jmap: %w", err)
				return
			}

			errc <- fmt.Errorf("jmap: %w", srv.jmap.Serve(ln))
		}()
	}

	srv.admin.start(errc)

	return errc
}

// quotaDrain is how long shutdown waits for running quota programs and
// adminDrain for admin requests in progress after them.
const (
	quotaDrain = 10 * time.Second
	adminDrain = 10 * time.Second
)

// drain shuts down gracefully: readiness goes red first so the Service stops
// routing new connections, then clients get a BYE and time to reconnect to
// another pod.
func (srv *servers) drain(settings *config.Config) {
	draining.Store(true)

	grace := time.Duration(settings.Server.DrainSeconds) * time.Second

	shCtx, cancel := context.WithTimeout(context.Background(), grace+5*time.Second)
	defer cancel()

	time.Sleep(min(grace/4, 5*time.Second)) // let endpoints propagate

	_ = srv.lmtp.Close()
	_ = srv.sieve.Close()
	done := make(chan struct{})

	go func() {
		_ = srv.imap.Shutdown(grace)
		_ = srv.pop3.Shutdown(grace)

		close(done)
	}()

	select {
	case <-done:
	case <-shCtx.Done():
	}

	_ = srv.http.Shutdown(shCtx)

	if srv.jmap != nil {
		_ = srv.jmap.Shutdown(shCtx)
	}

	srv.closeJMAP()

	if srv.jmapKV != nil {
		srv.jmapKV.Close()
	}
}

// logRelay says whether Sieve has an outbound relay.
func logRelay(log *slog.Logger, settings *config.Config) {
	if a := settings.SMTP.Addr; a != "" {
		log.Info("outbound relay for sieve redirect/vacation", "addr", a)
	} else {
		log.Warn("no SMTP_RELAY_ADDR: sieve redirect keeps messages, vacation replies are not sent")
	}
}

// closeJMAP ends the JMAP front-end's background work and waits for it.
// Shutdown runs the front-end's Close in a goroutine of its own; this
// call waits for that one, so the store may be closed afterwards.
func (srv *servers) closeJMAP() {
	if srv.jmapFront != nil {
		srv.jmapFront.Close()
	}
}
