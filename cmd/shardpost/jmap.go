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
	"shardpost/internal/outbound"
	"shardpost/internal/webpush"
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
	})

	if strings.TrimSpace(settings.JMAP.Shard) == "" {
		log.Warn("jmap serves every authenticated account; behind a proxy that does not route JMAP per account, " +
			"set jmap.shard_attribute and jmap.shard")
	}

	srv.jmap = front.HTTPServer(settings.JMAP.Addr)

	return nil
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

	timeout := sec(c.TimeoutSeconds)
	if timeout <= 0 {
		timeout = 30 * time.Second
	}

	relay := &outbound.Relay{Addr: c.RelayAddr, HELO: c.Helo, StartTLS: c.Starttls, Implicit: c.TLS,
		ServerName: c.ServerName, Timeout: timeout}

	switch {
	case !c.XClient:
		relay.User, relay.Password = c.User, c.Password
	case c.User != "":
		slog.Warn("jmap.submission.user is ignored with xclient on: the relay is told the user, not logged into")
	}

	return &jmap.SubmissionOptions{
		Relay: relay, KV: srv.jmapKV, AddressAttributes: c.Attributes(), XClient: c.XClient, XClientAddr: c.XClientAddr,
		CheckSender: c.CheckSender, MaxRecipients: int(c.MaxRecipients), MaxSize: c.MaxSize,
		Retention: sec(c.RetentionSeconds), MaxKept: int(c.MaxKept),
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

// runPush runs the push worker ("shardpost push") until ctx ends, with the
// health and metrics listener beside it for the probes.
func runPush(ctx context.Context, log *slog.Logger, settings *config.Config, store *cephstore.Store) error {
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

	var authn auth.Authenticator

	if c.CheckAccounts && !strings.EqualFold(settings.Server.AuthBackend, "nauthilus") {
		return fmt.Errorf("jmap.push.check_accounts needs server.auth_backend nauthilus: " +
			"another backend would deny every account and the worker would destroy every subscription")
	}

	if c.CheckAccounts {
		if authn, err = buildAuth(log, settings); err != nil {
			return err
		}
	}

	health := newHTTPServer(settings, store)

	go func() {
		if err := health.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("push worker: http listener", "err", err)
		}
	}()

	defer func() { _ = health.Close() }()

	host, _ := os.Hostname()

	return jmap.RunPushWorker(ctx, jmap.WorkerOptions{
		Store:          storeAdapter{store},
		KV:             kv,
		Sender:         pushSender(c, vapid),
		Instance:       host + "-" + randomSuffix(),
		Auth:           authn,
		Log:            log.With("component", "push"),
		VAPIDPublicKey: vapid.PublicKey(),
		Scan:           sec(c.ScanSeconds),
		Poll:           sec(c.PollSeconds),
	})
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
		MaxSizeUpload:         c.MaxSizeUpload,
		MaxSizeRequest:        c.MaxSizeRequest,
		MaxConcurrentUpload:   int(c.MaxConcurrentUpload),
		MaxConcurrentRequests: int(c.MaxConcurrentRequests),
		MaxCallsInRequest:     int(c.MaxCallsInRequest),
		MaxObjectsInGet:       int(c.MaxObjectsInGet),
		MaxObjectsInSet:       int(c.MaxObjectsInSet),
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
