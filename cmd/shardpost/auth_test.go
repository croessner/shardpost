package main

import (
	"bytes"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"shardpost/internal/auth"
	"shardpost/internal/config"
)

func quietLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// TestBuildAuthLayers: Nauthilus gets the cache, wrapped by the master
// user; the static backend and ttl 0 get none.
func TestBuildAuthLayers(t *testing.T) {
	settings := &config.Config{}
	settings.Server.AuthBackend = "nauthilus"
	settings.Server.MasterUser, settings.Server.MasterPassword = "director", "pw"
	settings.Nauthilus.Transport, settings.Nauthilus.URL = "http", "http://nauthilus.invalid/api/v1/auth/json"
	settings.Nauthilus.BearerMode = "password"
	settings.Nauthilus.Timeout, settings.Nauthilus.HTTPMaxIdlePerHost = 4, 8
	settings.Auth.Cache = config.AuthCache{TTL: 30, MaxEntries: 10, KeyClientIP: true, LookupTTL: 30, LookupNegativeTTL: 5}

	a, cache, err := buildAuth(quietLog(), settings)
	if err != nil || cache == nil {
		t.Fatalf("nauthilus: cache %v, err %v", cache, err)
	}

	m, ok := a.(*auth.MasterUser)
	if !ok || m.Next != cache {
		t.Fatalf("layers: %T around %T", a, m.Next)
	}

	n, err := buildNauthilus(quietLog(), settings)
	if err != nil || n.HTTP == nil || n.HTTP.Timeout != 4*time.Second {
		t.Fatalf("nauthilus client: %+v, %v", n.HTTP, err)
	}

	settings.Auth.Cache.TTL = 0
	if _, cache, _ = buildAuth(quietLog(), settings); cache != nil {
		t.Fatal("ttl 0 built a cache")
	}

	settings.Auth.Cache.TTL = 30
	settings.Server.AuthBackend, settings.Server.Users = "static", "test:test"

	if _, cache, _ = buildAuth(quietLog(), settings); cache != nil {
		t.Fatal("the static backend got a cache")
	}
}

// TestJMAPLoadWarnsWithoutProxyProtocol: a bound per client address
// without the PROXY protocol counts the proxy, and says so at startup.
func TestJMAPLoadWarnsWithoutProxyProtocol(t *testing.T) {
	var logs bytes.Buffer

	log := slog.New(slog.NewTextHandler(&logs, nil))
	settings := &config.Config{}
	settings.JMAP.LimitWait, settings.JMAP.RetryAfter, settings.JMAP.MaxConcurrentPerIP = 2, 1, 16

	got := jmapLoad(log, settings)
	if got.LimitWait != 2*time.Second || got.RetryAfter != time.Second || got.MaxConcurrentPerIP != 16 {
		t.Fatalf("load %+v", got)
	}

	if !strings.Contains(logs.String(), "proxy_protocol_trusted_cidrs") {
		t.Fatalf("no warning: %s", logs.String())
	}

	logs.Reset()

	settings.Server.ProxyProtocolTrustedCidrs = "10.0.0.0/8"
	jmapLoad(log, settings)

	if logs.Len() != 0 {
		t.Fatalf("warned with the PROXY protocol on: %s", logs.String())
	}
}
