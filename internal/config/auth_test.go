package config

import "testing"

// TestAuthCacheAndLoadDefaults: the auth cache, the Nauthilus client and
// the JMAP load settings have their documented defaults.
func TestAuthCacheAndLoadDefaults(t *testing.T) {
	c, err := Load(configFile(t, "logging:\n  level: info\n"))
	if err != nil {
		t.Fatal(err)
	}

	want := AuthCache{TTL: 30, NegativeTTL: 0, MaxEntries: 10000, KeyClientIP: true, LookupTTL: 30, LookupNegativeTTL: 5}
	if c.Auth.Cache != want {
		t.Errorf("auth.cache defaults %+v", c.Auth.Cache)
	}

	if c.Nauthilus.Timeout != 10 || c.Nauthilus.HTTPMaxIdlePerHost != 32 {
		t.Errorf("nauthilus defaults: timeout %d, idle %d", c.Nauthilus.Timeout, c.Nauthilus.HTTPMaxIdlePerHost)
	}

	if c.JMAP.LimitWait != 2 || c.JMAP.RetryAfter != 1 || c.JMAP.MaxConcurrentPerIP != 0 {
		t.Errorf("jmap load defaults: %d %d %d", c.JMAP.LimitWait, c.JMAP.RetryAfter, c.JMAP.MaxConcurrentPerIP)
	}

	c, err = Load(configFile(t, "auth:\n  cache:\n    ttl: 0\n    key_client_ip: false\n"))
	if err != nil || c.Auth.Cache.TTL != 0 || c.Auth.Cache.KeyClientIP {
		t.Errorf("emergency switch: %+v, %v", c.Auth.Cache, err)
	}
}

// TestAuthCacheEnv: the auth cache keys have environment names.
func TestAuthCacheEnv(t *testing.T) {
	t.Setenv("AUTH_CACHE_NEGATIVE_TTL", "3")

	c, err := Load(configFile(t, "logging:\n  level: info\n"))
	if err != nil || c.Auth.Cache.NegativeTTL != 3 {
		t.Errorf("env: %+v, %v", c.Auth.Cache, err)
	}
}

// TestAuthCacheAndLoadBounds: values outside the documented ranges are
// refused.
func TestAuthCacheAndLoadBounds(t *testing.T) {
	for _, body := range []string{
		"auth:\n  cache:\n    ttl: 601\n",
		"auth:\n  cache:\n    negative_ttl: 11\n",
		"auth:\n  cache:\n    max_entries: 0\n",
		"auth:\n  cache:\n    lookup_ttl: 4\n",
		"auth:\n  cache:\n    lookup_negative_ttl: 31\n",
		"nauthilus:\n  timeout: 0\n",
		"nauthilus:\n  http_max_idle_per_host: 0\n",
		"jmap:\n  limit_wait: 11\n",
		"jmap:\n  retry_after: 0\n",
		"jmap:\n  max_concurrent_per_ip: -1\n",
		"jmap:\n  max_event_sources: -1\n",
	} {
		if _, err := Load(configFile(t, body)); err == nil {
			t.Errorf("accepted %q", body)
		}
	}
}
