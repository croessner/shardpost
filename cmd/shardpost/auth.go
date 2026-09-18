package main

import (
	"fmt"
	"log/slog"
	"shardpost/internal/config"
	"strings"
	"time"

	"shardpost/internal/auth"
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
//	        NAUTHILUS_INTROSPECT_URL, NAUTHILUS_OIDC_CLIENT_ID/SECRET,
//	        NAUTHILUS_OIDC_AUDIENCE, NAUTHILUS_OIDC_SCOPE, NAUTHILUS_MAX_WAIT
//	MASTER_USER / MASTER_PASSWORD / MASTER_USER_SEPARATOR   (director master_user mode)
//
// Secrets may also be given as *_FILE paths.
func buildAuth(log *slog.Logger, settings *config.Config) (auth.Authenticator, error) {
	var a auth.Authenticator

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
			return nil, err
		}

		a = n
	default:
		return nil, fmt.Errorf("unknown AUTH_BACKEND %q", settings.Server.AuthBackend)
	}

	if mu := settings.Server.MasterUser; mu != "" {
		a = &auth.MasterUser{Next: a, Name: mu, Password: settings.Server.MasterPassword, Separator: settings.Server.MasterUserSeparator}
		log.Info("master user login enabled", "master", mu)
	}

	return a, nil
}

// buildNauthilus assembles the Nauthilus authenticator and its transport.
func buildNauthilus(log *slog.Logger, settings *config.Config) (*auth.Nauthilus, error) {
	n := &auth.Nauthilus{
		URL: settings.Nauthilus.URL, User: settings.Nauthilus.User, Password: settings.Nauthilus.Password,
		Token: settings.Nauthilus.Token, BearerMode: settings.Nauthilus.BearerMode,
		MaxWait: time.Duration(settings.Nauthilus.MaxWait) * time.Second,
	}
	if u := settings.Nauthilus.IntrospectURL; u != "" {
		n.Introspect = &auth.Introspection{URL: u, ClientID: settings.Nauthilus.OidcClientID, ClientSecret: settings.Nauthilus.OidcClientSecret,
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
			User: n.User, Password: n.Password, Token: n.Token, TokenSource: n.TokenSource,
		})
		if err != nil {
			return nil, err
		}

		n.GRPC = t
		log.Info("auth backend: nauthilus", "transport", "grpc", "addr", t.Address, "bearerMode", n.BearerMode)
	case "http":
		if n.URL == "" {
			return nil, fmt.Errorf("NAUTHILUS_URL is required for the http transport")
		}

		log.Info("auth backend: nauthilus", "transport", "http", "url", n.URL, "bearerMode", n.BearerMode)
	default:
		return nil, fmt.Errorf("unknown NAUTHILUS_TRANSPORT %q", settings.Nauthilus.Transport)
	}

	return n, nil
}

// setNauthilusTokenSource configures backchannel caller authentication with
// OIDC client_credentials (the way nauthilus-director authenticates): the
// token endpoint issues short-lived bearer tokens for the gRPC/HTTP calls.
func setNauthilusTokenSource(log *slog.Logger, settings *config.Config, n *auth.Nauthilus) error {
	tu := settings.Nauthilus.OidcTokenURL
	if tu == "" {
		return nil
	}

	cc := &auth.ClientCredentials{
		TokenURL: tu, ClientID: settings.Nauthilus.OidcClientID, ClientSecret: settings.Nauthilus.OidcClientSecret,
		Scopes:   strings.Fields(settings.Nauthilus.OidcScopes),
		Audience: settings.Nauthilus.OidcTokenAudience, AuthMethod: settings.Nauthilus.OidcAuthMethod,
	}
	if cc.ClientID == "" || cc.ClientSecret == "" {
		return fmt.Errorf("NAUTHILUS_OIDC_CLIENT_ID and NAUTHILUS_OIDC_CLIENT_SECRET are required with NAUTHILUS_OIDC_TOKEN_URL")
	}

	n.TokenSource = cc
	log.Info("nauthilus backchannel: oidc client_credentials", "tokenURL", tu, "clientID", cc.ClientID, "scopes", cc.Scopes)

	return nil
}
