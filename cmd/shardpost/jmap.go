package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"shardpost/internal/auth"
	"shardpost/internal/cephstore"
	"shardpost/internal/config"
	"shardpost/internal/jmap"
	"shardpost/internal/model"
	"shardpost/internal/sharenotice"
	"shardpost/internal/version"
	"shardpost/internal/webpush"
)

// setupHTTP builds the health and metrics listener, the JMAP front-end
// and the admin API.
func (srv *servers) setupHTTP(log *slog.Logger, settings *config.Config, store *cephstore.Store, authn auth.Authenticator,
	flushLog *auth.FlushLog,
) error {
	srv.http = newHTTPServer(settings, store)

	if err := srv.setupJMAP(log, settings, store, authn); err != nil {
		return err
	}

	admin, err := newAdminServers(log, settings, authn, srv.delivery, store, srv.notices, flushLog, srv.sendq, srv.kv, srv.jobs, srv.learning)
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
		cfg, err := certConfig(log, cert, key, settings)
		if err != nil {
			return fmt.Errorf("jmap tls: %w", err)
		}

		srv.jmapTLS = cfg
	}

	push, err := srv.jmapPush(settings.JMAP.Push, store)
	if err != nil {
		return err
	}

	submission, err := srv.jmapSubmission(settings.JMAP.Submission, store)
	if err != nil {
		return err
	}

	front := jmap.New(jmap.Options{
		Store:         storeAdapter{store},
		Auth:          authn,
		Log:           log.With("proto", "jmap"),
		PublicBaseURL: settings.JMAP.PublicBaseURL,
		InsecureAuth:  settings.JMAP.InsecureAuth,
		Limits:        jmapLimits(settings.JMAP),
		Push:          push,
		Submission:    submission,
		Quota:         jmapQuota(settings.Quota),
		// Delivery sends the vacation reply, through its own relay.
		VacationResponse: settings.SMTP.Addr != "",
		Autocreate:       autocreate(settings.Server.Autocreate),
		Shard:            jmap.ShardGate{Attribute: strings.TrimSpace(settings.JMAP.ShardAttribute), Value: strings.TrimSpace(settings.JMAP.Shard)},
		// The scripts ManageSieve manages, compiled as ManageSieve does.
		Sieve: &jmap.SieveOptions{Config: srv.sieveConfig, GlobalScripts: settings.Sieve.GlobalDir},
		// The accounts other namespaces share, switched like the IMAP
		// namespaces of the same name.
		Sharing:    jmapSharing(settings),
		Principals: jmapPrincipals(settings.JMAP, srv.notices),
		Load:       jmapLoad(log, settings),
	})

	if strings.TrimSpace(settings.JMAP.Shard) == "" {
		log.Warn("jmap serves every authenticated account; behind a proxy that does not route JMAP per account, " +
			"set jmap.shard_attribute and jmap.shard")
	}

	srv.jmap = front.HTTPServer(settings.JMAP.Addr)
	srv.jmapFront = front

	return nil
}

// jmapLoad maps the load settings. A bound per client address behind a
// proxy that does not pass the client's address (PROXY protocol) counts
// the proxy, which is warned about.
func jmapLoad(log *slog.Logger, settings *config.Config) jmap.LoadOptions {
	c := settings.JMAP

	if c.MaxConcurrentPerIP > 0 && strings.TrimSpace(settings.Server.ProxyProtocolTrustedCidrs) == "" {
		log.Warn("jmap.max_concurrent_per_ip is set without server.proxy_protocol_trusted_cidrs: "+
			"behind a proxy every client shares the proxy's address and the bound applies to all of them together",
			"max_concurrent_per_ip", c.MaxConcurrentPerIP)
	}

	return jmap.LoadOptions{LimitWait: sec(c.LimitWait), RetryAfter: sec(c.RetryAfter), MaxConcurrentPerIP: int(c.MaxConcurrentPerIP)}
}

// jmapSharing maps the settings of the shared accounts, which the
// front-end and the push worker build alike: switched like the IMAP
// namespaces of the same name.
func jmapSharing(settings *config.Config) jmap.SharingOptions {
	return jmap.SharingOptions{
		SharedPrefix: settings.Server.SharedPrefix,
		PublicPrefix: settings.Server.PublicPrefix,
		MaxAccounts:  int(settings.JMAP.MaxSharedAccounts),
		MaxMailboxes: int(settings.JMAP.MaxSharedMailboxes),
		// 0 takes eight times MaxMailboxes.
		MaxSessionPointers: int(settings.JMAP.MaxSessionPointers),
	}
}

// jmapPrincipals maps the principal bounds; the notices are the ones IMAP
// and the admin API write as well.
func jmapPrincipals(c config.JMAP, notices *sharenotice.Recorder) jmap.PrincipalOptions {
	return jmap.PrincipalOptions{MaxLookups: int(c.MaxPrincipalLookups), MaxVisible: int(c.MaxPrincipals),
		MissPerMinute: int(c.PrincipalMissPerMinute), Notices: notices}
}

// jmapPush builds the push settings of the front-end. With a VAPID key,
// push subscriptions are offered and kept in the admin store, where the
// push worker reads them.
func (srv *servers) jmapPush(c config.JMAPPush, store *cephstore.Store) (jmap.PushOptions, error) {
	push := jmap.PushOptions{
		MaxSubscriptions:  int(c.MaxSubscriptions),
		MaxExpiry:         sec(c.MaxExpirySeconds),
		EventPoll:         sec(c.EventPollSeconds),
		AllowInternalURLs: c.AllowInternalEndpoints,
		MaxSharedWatches:  int(c.MaxSharedWatches),
	}

	if !c.Enabled() {
		return push, nil
	}

	vapid, err := loadVAPID(c)
	if err != nil {
		return push, err
	}

	kv, err := store.AdminKV()
	if err != nil {
		return push, fmt.Errorf("jmap push: admin store: %w", err)
	}

	srv.jmapKV = kv
	push.KV, push.VAPIDPublicKey = kv, vapid.PublicKey()

	return push, nil
}

// jmapSubmission builds the submission settings: the relay, and the
// admin store the submission records are kept in. Without a relay the
// capability is not offered.
func (srv *servers) jmapSubmission(c config.JMAPSubmission, store *cephstore.Store) (*jmap.SubmissionOptions, error) {
	if !c.Enabled() {
		return nil, nil
	}

	if srv.jmapKV == nil {
		kv, err := store.AdminKV()
		if err != nil {
			return nil, fmt.Errorf("jmap submission: admin store: %w", err)
		}

		srv.jmapKV = kv
	}

	relay := submissionRelay(c)

	if c.XClient && c.User != "" {
		slog.Warn("jmap.submission.user is ignored with xclient on: the relay is told the user, not logged into")
	}

	if c.Delayed.Enabled() && srv.sendq == nil {
		return nil, fmt.Errorf("jmap.submission.delayed needs the submission queue (admin.kv_index write or read)")
	}

	return &jmap.SubmissionOptions{
		Relay: relay, KV: srv.jmapKV, AddressAttributes: c.Attributes(), XClient: c.XClient, XClientAddr: c.XClientAddr,
		CheckSender: c.CheckSender, MaxRecipients: int(c.MaxRecipients), MaxSize: c.MaxSize,
		Retention: sec(c.RetentionSeconds), MaxKept: int(c.MaxKept), Delayed: delayedOptions(c.Delayed, srv.sendq),
	}, nil
}

// loadVAPID reads the VAPID key.
func loadVAPID(c config.JMAPPush) (*webpush.VAPID, error) {
	raw, err := os.ReadFile(c.VAPIDKeyFile)
	if err != nil {
		return nil, fmt.Errorf("jmap push: %w", err)
	}

	return webpush.LoadVAPID(raw, c.VAPIDSubject)
}

// pushAuth is the authenticator of the push worker: none without
// jmap.push.check_accounts. Its lookups are cached like the servers' and
// flushed with them; stop ends following the flush log.
func pushAuth(ctx context.Context, log *slog.Logger, settings *config.Config, kv *cephstore.AdminKV) (auth.Authenticator, func(), error) {
	if !settings.JMAP.Push.CheckAccounts {
		return nil, func() {}, nil
	}

	if !strings.EqualFold(settings.Server.AuthBackend, "nauthilus") {
		return nil, nil, fmt.Errorf("jmap.push.check_accounts needs server.auth_backend nauthilus: " +
			"another backend would deny every account and the worker would destroy every subscription")
	}

	authn, cache, err := buildAuth(log, settings)
	if err != nil {
		return nil, nil, err
	}

	if cache == nil {
		return authn, func() {}, nil
	}

	_, stop := runFlushLog(ctx, log, kv.Omap(), cache, func() {})

	return authn, stop, nil
}

// runPush runs the push worker ("shardpost push") until ctx ends, with the
// health and metrics listener beside it for the probes. status is what
// the registration shows of it.
func runPush(ctx context.Context, log *slog.Logger, settings *config.Config, store *cephstore.Store, status *jmap.PushStatus) error {
	c := settings.JMAP.Push
	if !c.Enabled() {
		return fmt.Errorf("shardpost push needs jmap.push.vapid_key_file")
	}

	vapid, err := loadVAPID(c)
	if err != nil {
		return err
	}

	kv, err := store.AdminKV()
	if err != nil {
		return fmt.Errorf("push worker: admin store: %w", err)
	}
	defer kv.Close()

	authn, stopAuth, err := pushAuth(ctx, log, settings, kv)
	if err != nil {
		return err
	}
	defer stopAuth()

	health := newHTTPServer(settings, store)

	go func() {
		if err := health.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("push worker: http listener", "err", err)
		}
	}()

	defer func() { _ = health.Close() }()

	o := pushWorkerOptions(settings, store)
	o.KV, o.Sender, o.Auth, o.Log, o.VAPIDPublicKey, o.Status = kv, pushSender(c, vapid), authn, log.With("component", "push"), vapid.PublicKey(), status

	return jmap.RunPushWorker(ctx, o)
}

// pushWorkerOptions maps the configured push settings (the partitions
// of stage B18a among them); the store, the sender, the authenticator,
// the log, the key and the status are the caller's.
func pushWorkerOptions(settings *config.Config, store *cephstore.Store) jmap.WorkerOptions {
	c := settings.JMAP.Push

	return jmap.WorkerOptions{
		Store: storeAdapter{store},
		// The same name as the registration: stable across restarts of
		// the container, so the worker finds its own membership record.
		Instance: registrationInstance(model.RolePush),
		Version:  version.Version,
		Scan:     sec(c.ScanSeconds),
		Poll:     sec(c.PollSeconds),
		// The shared accounts, built as the front-ends build them.
		Sharing:          jmapSharing(settings),
		MaxSharedWatches: int(c.MaxSharedWatches),
		MaxWatches:       int(c.WorkerMaxWatches),
		// The partitions.
		LeaseTTL:           sec(c.LeaseSeconds),
		Take:               sec(c.TakeSeconds),
		Retry:              sec(c.RetrySeconds),
		OrphanHold:         sec(c.OrphanHoldSeconds),
		FullScan:           sec(c.FullScanSeconds),
		Rebalance:          sec(c.RebalanceSeconds),
		RebalanceSettle:    sec(c.RebalanceSettleSeconds),
		Handoff:            sec(c.HandoffSeconds),
		MaxPartitions:      int(c.MaxPartitions),
		MaxWatchedAccounts: int(c.MaxWatchedAccounts),
		StartRate:          int(c.StartRate),
		SelfStopMargin:     config.PushSelfStopMargin * time.Second,
		Rotate:             store,
	}
}

// jmapQuota finds the warnLimit of the Quota objects: per resource the
// lowest quota warning that fires on the way up. A warning on the way
// down marks the end of a warning, not a limit to stay below.
func jmapQuota(c config.Quota) jmap.QuotaOptions {
	var o jmap.QuotaOptions

	for _, w := range c.Warnings {
		if w.Direction != "" && w.Direction != "over" {
			continue
		}

		p := &o.StorageWarnPercent
		if w.Resource == "messages" {
			p = &o.MessagesWarnPercent
		}

		if *p == 0 || w.Percent < *p {
			*p = w.Percent
		}
	}

	return o
}

// jmapLimits maps the configured limits; a zero keeps the default.
func jmapLimits(c config.JMAP) jmap.Limits {
	return jmap.Limits{
		MaxSizeUpload:          c.MaxSizeUpload,
		MaxSizeRequest:         c.MaxSizeRequest,
		MaxConcurrentUpload:    int(c.MaxConcurrentUpload),
		MaxConcurrentRequests:  int(c.MaxConcurrentRequests),
		MaxConcurrentDownloads: int(c.MaxConcurrentDownloads),
		MaxEventSources:        int(c.MaxEventSources),
		MaxCallsInRequest:      int(c.MaxCallsInRequest),
		MaxObjectsInGet:        int(c.MaxObjectsInGet),
		MaxObjectsInSet:        int(c.MaxObjectsInSet),
		MaxSizeBlobSet:         c.MaxSizeBlobSet,
		MaxDataSources:         int(c.MaxDataSources),
		MaxSizeBlobGet:         c.MaxSizeBlobGet,
		SnippetReadBytes:       c.Search.SnippetReadBytes,
		MaxAttachmentReads:     int(c.Search.MaxAttachmentReads),
		AttachmentBackfills:    int(c.Search.AttachmentBackfills),
		AttachmentBackfillTime: sec(c.Search.AttachmentBackfillSeconds),
	}
}

// pushSender is the Web Push sender of the worker.
func pushSender(c config.JMAPPush, vapid *webpush.VAPID) *webpush.Sender {
	timeout := sec(c.TimeoutSeconds)
	if timeout <= 0 {
		timeout = 10 * time.Second
	}

	ttl := sec(c.MessageTTLSeconds)
	if ttl <= 0 {
		ttl = 24 * time.Hour
	}

	return &webpush.Sender{Client: webpush.NewClient(timeout, c.AllowInternalEndpoints), VAPID: vapid, TTL: ttl}
}

// randomSuffix makes a worker's name unique even on one host.
func randomSuffix() string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)

	return hex.EncodeToString(b)
}

// sec is a number of seconds from the configuration as a duration.
func sec(n int64) time.Duration { return time.Duration(n) * time.Second }
