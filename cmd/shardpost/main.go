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
	"shardpost/internal/logging"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"

	"shardpost/internal/auth"
	"shardpost/internal/cephstore"
	"shardpost/internal/delivery"
	"shardpost/internal/extprograms"
	"shardpost/internal/imapproto"
	"shardpost/internal/imapsieve"
	"shardpost/internal/lmtpd"
	"shardpost/internal/membudget"
	"shardpost/internal/model"
	"shardpost/internal/outbound"
	"shardpost/internal/pop3proto"
	"shardpost/internal/provision"
	"shardpost/internal/quota"
	"shardpost/internal/rookcreds"
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
	log := logging.New(os.Stdout, "info")

	settings, command, err := loadConfig(os.Args[1:])
	if err == nil {
		log = logging.New(os.Stdout, settings.Logging.Level).With("service", "shardpost")
		slog.SetDefault(log)
	}

	if err == nil && command == commandCheck {
		log.Info("configuration valid")
		return
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
	commandCheck = "check"
)

// capabilityEnabled is the ManageSieve capability value for enabled features.
const capabilityEnabled = "yes"

func run(log *slog.Logger, settings *config.Config, command string) error {
	log.Info("logging configured", "event", "logging_configured", "log_level", settings.Logging.Level)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	shutdownTracing := telemetry.Setup(ctx, settings.Telemetry.ServiceName, log, settings.Telemetry)
	defer func() { _ = shutdownTracing(context.Background()) }()

	cfg := cephConfig(settings)
	programs := extprograms.New(settings.ExtPrograms)

	// Quota warnings and the over-flag check run only while serving;
	// the scrubber's recount reports nothing.
	var quotaSvc *quota.Service
	if command != commandScrub {
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

	store, err := cephstore.Open(cfg, log)
	if err != nil {
		return err
	}
	defer store.Close()

	if rotate != nil {
		rotate(store)
	}

	// "shardpost scrub" runs the scrubber over all users and exits.
	if command == commandScrub {
		return scrub(store, log, settings)
	}

	return serve(ctx, log, settings, store, programs, quotaSvc)
}

// cephConfig maps the configuration onto the RADOS store settings.
func cephConfig(settings *config.Config) cephstore.Config {
	return cephstore.Config{
		User:       settings.Ceph.User,
		Key:        settings.Ceph.Key,
		MonHost:    settings.Ceph.MonHost,
		IndexPool:  settings.Ceph.IndexPool,
		DataPool:   settings.Ceph.DataPool,
		ChunkSize:  settings.Ceph.ChunkSize,
		FTSShards:  int(settings.Server.FtsShards),
		Quota:      cephstore.QuotaLimit{Bytes: settings.Server.QuotaBytes, Msgs: settings.Server.QuotaMsgs},
		QuotaRules: quota.NewPolicy(settings.Quota),
		ShardSize:  uint32(settings.Server.MboxShardSize),
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
func loadTLS(settings *config.Config) (*tls.Config, error) {
	cert, key := settings.TLS.Cert, settings.TLS.Key
	if cert == "" || key == "" {
		return nil, nil
	}

	c, err := tls.LoadX509KeyPair(cert, key)
	if err != nil {
		return nil, fmt.Errorf("load tls keypair: %w", err)
	}

	return &tls.Config{Certificates: []tls.Certificate{c}, MinVersion: tls.VersionTLS12}, nil
}

// newIMAPSieve builds the IMAPSieve engine and, when enabled, starts the
// learning worker. The returned function stops the worker and waits for it.
func newIMAPSieve(ctx context.Context, log *slog.Logger, settings *config.Config, store *cephstore.Store,
	programs *extprograms.Runner, sieveCfg map[string]string,
) (*imapsieve.Engine, func()) {
	imapSieve := imapsieve.New(settings.IMAPSieve, storeAdapter{store}, log)
	stopWorker := func() {}

	imapSieve.Programs = programs
	if settings.IMAPSieve.Enabled && settings.IMAPSieve.Learning.Enabled {
		worker := imapsieve.NewLearningWorker(settings.IMAPSieve.Learning, cephstore.LearningQueue{Store: store}, programs)
		imapSieve.Learner = worker
		workerCtx, workerCancel := context.WithCancel(ctx)

		workerDone := make(chan struct{})
		go func() { defer close(workerDone); worker.Run(workerCtx) }()

		stopWorker = func() { workerCancel(); <-workerDone }
	}

	imapSieve.Sender = &outbound.Relay{Addr: settings.SMTP.Addr, HELO: settings.SMTP.Helo, StartTLS: settings.SMTP.Starttls, Implicit: settings.SMTP.TLS, User: settings.SMTP.User, Password: settings.SMTP.Password, ServerName: settings.SMTP.ServerName, Timeout: time.Duration(settings.IMAPSieve.TimeoutMS) * time.Millisecond}
	imapSieve.GlobalScripts = settings.Sieve.GlobalDir
	imapSieve.SieveConfig = sieveCfg

	return imapSieve, stopWorker
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
	// delivery is shared by LMTP and the local delivery API.
	delivery *delivery.Service
	admin    *adminServers
}

// serve starts all listeners and blocks until a signal or a fatal listener
// error, then drains.
func serve(ctx context.Context, log *slog.Logger, settings *config.Config, store *cephstore.Store,
	programs *extprograms.Runner, quotaSvc *quota.Service,
) error {
	authn, err := buildAuth(log, settings)
	if err != nil {
		return err
	}

	tlsCfg, err := loadTLS(settings)
	if err != nil {
		return err
	}

	sieveCfg := sieve.ParseSettings(settings.Sieve.Settings)
	provision.Quota = quotaSvc

	imapSieve, stopWorker := newIMAPSieve(ctx, log, settings, store, programs, sieveCfg)
	defer stopWorker()

	srv := newServers(log, settings, store, authn, tlsCfg, imapSieve, programs, sieveCfg)
	if a := settings.SMTP.Addr; a != "" {
		log.Info("outbound relay for sieve redirect/vacation", "addr", a)
	} else {
		log.Warn("no SMTP_RELAY_ADDR: sieve redirect keeps messages, vacation replies are not sent")
	}

	srv.http = newHTTPServer(settings, store)

	srv.admin, err = newAdminServers(log, settings, authn, srv.delivery, importStore{store})
	if err != nil {
		return err
	}

	errc := srv.start(settings, tlsCfg)

	log.Info("listening", "imap", settings.Server.ImapAddr, "pop3", settings.Server.Pop3Addr, "managesieve", settings.Sieve.Addr, "lmtp", settings.Server.LmtpAddr, "http", settings.Server.HTTPAddr, "tls", tlsCfg != nil,
		"admin_socket", settings.Admin.Socket, "admin_addr", settings.Admin.Addr, "version", version.Version)

	select {
	case <-ctx.Done():
		log.Info("shutting down")
	case err := <-errc:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	}

	srv.drain(settings)

	// Quota programs may still deliver warnings through the admin
	// socket, so the admin API stops after them, with its own time for
	// deliveries in progress.
	qctx, cancel := context.WithTimeout(context.Background(), quotaDrain)
	quotaSvc.Close(qctx)
	cancel()

	actx, cancel := context.WithTimeout(context.Background(), adminDrain)
	srv.admin.shutdown(actx)
	cancel()

	log.Info("shutdown complete")

	return nil
}

// newServers builds the protocol servers.
func newServers(log *slog.Logger, settings *config.Config, store *cephstore.Store, authn auth.Authenticator, tlsCfg *tls.Config,
	imapSieve *imapsieve.Engine, programs *extprograms.Runner, sieveCfg map[string]string,
) *servers {
	insecure := settings.Server.InsecureAuth
	manageConfig := manageSieveConfig(settings, sieveCfg, programs)

	var srv servers

	srv.imap = imapproto.New(imapproto.Options{
		IMAPSieve: imapSieve,
		Store:     storeAdapter{store}, Auth: authn, Log: log.With("proto", "imap"), TLS: tlsCfg,
		InsecureAuth:     insecure,
		Autocreate:       imapproto.DefaultSpecialFolders,
		MaxLiteral:       cephstore.MaxMessageLen,
		MaxConnsPerIP:    int(settings.Server.MaxConnsPerIP),
		MaxConnsPerUser:  int(settings.Server.MaxConnsPerUser),
		QuotaSelfService: settings.Server.QuotaSelfService,
		Trace: imapproto.TraceOptions{MaxLine: settings.Trace.MaxLineBytes,
			Recheck: time.Duration(settings.Trace.RecheckSeconds) * time.Second},
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
	return &delivery.Service{Store: storeAdapter{store}, Log: log.With("component", "delivery"),
		GlobalScripts: settings.Sieve.GlobalDir, SieveConfig: sieveCfg, Programs: programs,
		Relay: &outbound.Relay{Addr: settings.SMTP.Addr, HELO: settings.SMTP.Helo,
			StartTLS: settings.SMTP.Starttls, Implicit: settings.SMTP.TLS,
			User: settings.SMTP.User, Password: settings.SMTP.Password, ServerName: settings.SMTP.ServerName}}
}

// start launches every listener in its own goroutine; the first fatal error
// of each is delivered on the returned channel.
func (srv *servers) start(settings *config.Config, tlsCfg *tls.Config) <-chan error {
	// One slot per listener goroutine, so none blocks on a full channel.
	errc := make(chan error, 9)

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
}

// scrub walks all users (or SCRUB_USERS) and repairs what a crash may
// have left behind. SCRUB_DRY_RUN=true only reports.
func scrub(store *cephstore.Store, log *slog.Logger, settings *config.Config) error {
	users := strings.Split(settings.Scrub.Users, ",")
	if strings.TrimSpace(users[0]) == "" {
		var err error
		if users, err = store.ListUsers(); err != nil {
			return fmt.Errorf("list users: %w", err)
		}
	}

	opts := cephstore.ScrubOptions{
		Grace:  time.Duration(settings.Scrub.GraceSeconds) * time.Second,
		DryRun: settings.Scrub.DryRun,
		Log:    log,
	}
	failed := 0

	for _, name := range users {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}

		u, err := store.User(name)
		if err != nil {
			log.Error("scrub: open user", "user", name, "err", err)

			failed++

			continue
		}

		if _, err := u.Scrub(opts); err != nil {
			log.Error("scrub failed", "user", name, "err", err)

			failed++
		}

		u.Close()
	}

	log.Info("scrub finished", "users", len(users), "failed", failed, "dryRun", opts.DryRun)

	if failed > 0 {
		return fmt.Errorf("%d users failed", failed)
	}

	return nil
}
