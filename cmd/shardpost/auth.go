package main

import (
	"context"
	"fmt"
	"log/slog"
	"shardpost/internal/config"
	"strings"
	"time"

	"shardpost/internal/auth"
	"shardpost/internal/cephstore"
	"shardpost/internal/model"
)

// buildAuth assembles the authenticator from the environment:
//
//	AUTH_BACKEND=static (default)   USERS="test:test,alice:secret"
//	AUTH_BACKEND=nauthilus          NAUTHILUS_TRANSPORT=grpc|http
//	  grpc: NAUTHILUS_GRPC_ADDR, NAUTHILUS_GRPC_INSECURE, NAUTHILUS_GRPC_CA,
//	        NAUTHILUS_GRPC_SERVER_NAME, NAUTHILUS_GRPC_CLIENT_CERT/KEY
//	  http: NAUTHILUS_URL (…/api/v1/auth/json)
//	  both: NAUTHILUS_USER/NAUTHILUS_PASSWORD (backchannel basic) or
//	        NAUTHILUS_TOKEN (bearer), NAUTHILUS_BEARER_MODE=password|introspect,
//	        NAUTHILUS_INTROSPECT_URL, NAUTHILUS_INTROSPECT_CLIENT_ID/SECRET
//	        (falling back to NAUTHILUS_OIDC_CLIENT_ID/SECRET),
//	        NAUTHILUS_OIDC_AUDIENCE, NAUTHILUS_OIDC_SCOPE, NAUTHILUS_MAX_WAIT,
//	        NAUTHILUS_TIMEOUT, NAUTHILUS_HTTP_MAX_IDLE_PER_HOST
//	AUTH_CACHE_* (auth.cache)        the cache in front of Nauthilus
//	MASTER_USER / MASTER_PASSWORD / MASTER_USER_SEPARATOR   (director master_user mode)
//
// Secrets may also be given as *_FILE paths. The layers are
// MasterUser{Cache{Nauthilus}}: the master password is checked before
// the cache and never cached. The returned cache is nil when there is
// none (the static backend, or auth.cache.ttl 0).
func buildAuth(log *slog.Logger, settings *config.Config) (auth.Authenticator, *auth.Cache, error) {
	var (
		a     auth.Authenticator
		cache *auth.Cache
	)

	switch strings.ToLower(settings.Server.AuthBackend) {
	case "static":
		st := auth.Static{}

		for _, ent := range strings.Split(settings.Server.Users, ",") {
			if u, p, ok := strings.Cut(strings.TrimSpace(ent), ":"); ok {
				st[u] = p
			}
		}

		a = st
		log.Info("auth backend: static", "users", len(st))
	case "nauthilus":
		n, err := buildNauthilus(log, settings)
		if err != nil {
			return nil, nil, err
		}

		a = n

		if cache = buildCache(log, settings, n); cache != nil {
			a = cache
		}
	default:
		return nil, nil, fmt.Errorf("unknown AUTH_BACKEND %q", settings.Server.AuthBackend)
	}

	if mu := settings.Server.MasterUser; mu != "" {
		a = &auth.MasterUser{Next: a, Name: mu, Password: settings.Server.MasterPassword, Separator: settings.Server.MasterUserSeparator}
		log.Info("master user login enabled", "master", mu)
	}

	return a, cache, nil
}

// flushLogStore opens the admin store the flush log lives in.
type flushLogStore interface {
	AdminKV() (*cephstore.AdminKV, error)
}

// startFlushLog follows the auth cache flush log of the cluster until
// ctx ends or the returned stop runs. Without a cache it does nothing
// and returns nil.
func startFlushLog(ctx context.Context, log *slog.Logger, store flushLogStore, cache *auth.Cache) (*auth.FlushLog, func(), error) {
	if cache == nil {
		return nil, func() {}, nil
	}

	kv, err := store.AdminKV()
	if err != nil {
		return nil, nil, fmt.Errorf("auth cache flush log: admin store: %w", err)
	}

	fl, stop := runFlushLog(ctx, log, kv.Omap(), cache, kv.Close)

	return fl, stop, nil
}

// runFlushLog runs the flush log over omap; stop ends it and then calls
// release.
func runFlushLog(ctx context.Context, log *slog.Logger, omap model.AdminOmap, cache *auth.Cache, release func()) (*auth.FlushLog, func()) {
	fl := auth.NewFlushLog(omap, cache, auth.DefaultFlushPoll, log.With("component", "auth-cache"))
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})

	go func() { defer close(done); fl.Run(runCtx) }()

	return fl, func() { cancel(); <-done; release() }
}

// buildCache builds the auth cache around next from auth.cache; nil when
// ttl is 0 (the emergency switch).
func buildCache(log *slog.Logger, settings *config.Config, next auth.Authenticator) *auth.Cache {
	c := settings.Auth.Cache
	if c.TTL <= 0 {
		log.Warn("auth cache off (auth.cache.ttl 0): every login and lookup asks nauthilus")
		return nil
	}

	budget := sec(c.LeaderBudget)
	if budget <= 0 {
		budget = 2 * sec(settings.Nauthilus.Timeout)
	}

	cfg := auth.CacheConfig{
		TTL: sec(c.TTL), NegativeTTL: sec(c.NegativeTTL), MaxEntries: int(c.MaxEntries), KeyClientIP: c.KeyClientIP,
		LookupTTL: sec(c.LookupTTL), LookupNegativeTTL: sec(c.LookupNegativeTTL), LeaderBudget: budget,
	}

	log.Info("auth cache", "ttl", cfg.TTL, "negative_ttl", cfg.NegativeTTL, "max_entries", cfg.MaxEntries,
		"key_client_ip", cfg.KeyClientIP, "lookup_ttl", cfg.LookupTTL, "lookup_negative_ttl", cfg.LookupNegativeTTL,
		"leader_budget", cfg.LeaderBudget)

	return auth.NewCache(next, cfg)
}

// buildNauthilus assembles the Nauthilus authenticator and its transport.
func buildNauthilus(log *slog.Logger, settings *config.Config) (*auth.Nauthilus, error) {
	timeout := sec(settings.Nauthilus.Timeout)

	n := &auth.Nauthilus{
		URL: settings.Nauthilus.URL, User: settings.Nauthilus.User, Password: settings.Nauthilus.Password,
		Token: settings.Nauthilus.Token, BearerMode: settings.Nauthilus.BearerMode,
		MaxWait: time.Duration(settings.Nauthilus.MaxWait) * time.Second,
		// One client for the JSON API, the introspection and the token
		// endpoint.
		HTTP: auth.NewHTTPClient(timeout, int(settings.Nauthilus.HTTPMaxIdlePerHost)),
	}
	if u := settings.Nauthilus.IntrospectURL; u != "" {
		id, secret := settings.Nauthilus.IntrospectClientID, settings.Nauthilus.IntrospectClientSecret
		if id == "" {
			id, secret = settings.Nauthilus.OidcClientID, settings.Nauthilus.OidcClientSecret
		}

		n.Introspect = &auth.Introspection{URL: u, ClientID: id, ClientSecret: secret,
			Audience: settings.Nauthilus.OidcAudience, Scope: settings.Nauthilus.OidcScope}
	}

	if c := settings.Nauthilus.AccountClaims; c != "" {
		n.AccountClaims = strings.Split(c, ",")
	}

	if err := setNauthilusTokenSource(log, settings, n); err != nil {
		return nil, err
	}

	switch strings.ToLower(settings.Nauthilus.Transport) {
	case "grpc":
		t, err := auth.DialGRPC(auth.GRPCOptions{
			Address:  settings.Nauthilus.GrpcAddr,
			Insecure: settings.Nauthilus.GrpcInsecure, CAFile: settings.Nauthilus.GrpcCa,
			ServerName: settings.Nauthilus.GrpcServerName, ClientCert: settings.Nauthilus.GrpcClientCert, ClientKey: settings.Nauthilus.GrpcClientKey,
			User: n.User, Password: n.Password, Token: n.Token, TokenSource: n.TokenSource, Timeout: timeout,
		})
		if err != nil {
			return nil, err
		}

		n.GRPC = t
		log.Info("auth backend: nauthilus", "transport", "grpc", "addr", t.Address, "bearerMode", n.BearerMode, "timeout", timeout)
	case "http":
		if n.URL == "" {
			return nil, fmt.Errorf("NAUTHILUS_URL is required for the http transport")
		}

		log.Info("auth backend: nauthilus", "transport", "http", "url", n.URL, "bearerMode", n.BearerMode, "timeout", timeout)
	default:
		return nil, fmt.Errorf("unknown NAUTHILUS_TRANSPORT %q", settings.Nauthilus.Transport)
	}

	return n, nil
}

// setNauthilusTokenSource configures backchannel caller authentication with
// OIDC client_credentials (the way nauthilus-director authenticates): the
// token endpoint issues short-lived bearer tokens for the gRPC/HTTP calls.
// The token source shares the HTTP client of n.
func setNauthilusTokenSource(log *slog.Logger, settings *config.Config, n *auth.Nauthilus) error {
	tu := settings.Nauthilus.OidcTokenURL
	if tu == "" {
		return nil
	}

	cc := &auth.ClientCredentials{
		TokenURL: tu, ClientID: settings.Nauthilus.OidcClientID, ClientSecret: settings.Nauthilus.OidcClientSecret,
		Scopes:   strings.Fields(settings.Nauthilus.OidcScopes),
		Audience: settings.Nauthilus.OidcTokenAudience, AuthMethod: settings.Nauthilus.OidcAuthMethod,
		HTTP: n.HTTP,
	}
	if cc.ClientID == "" || cc.ClientSecret == "" {
		return fmt.Errorf("NAUTHILUS_OIDC_CLIENT_ID and NAUTHILUS_OIDC_CLIENT_SECRET are required with NAUTHILUS_OIDC_TOKEN_URL")
	}

	n.TokenSource = cc
	log.Info("nauthilus backchannel: oidc client_credentials", "tokenURL", tu, "clientID", cc.ClientID, "scopes", cc.Scopes)

	return nil
}
