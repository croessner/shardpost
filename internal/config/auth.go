package config

import "fmt"

// Auth configures the authentication layer in front of the backend.
type Auth struct {
	// Cache configures the auth cache in front of Nauthilus (the static
	// test backend is never cached).
	Cache AuthCache `mapstructure:"cache"`
}

// AuthCache configures the auth cache: repeated logins and lookups are
// answered from memory, identical concurrent ones share one backend
// call. Times are in seconds.
type AuthCache struct {
	// TTL is how long a successful login is answered from the cache. 0
	// switches the whole cache off (logins and lookups): the emergency
	// switch.
	TTL int64 `mapstructure:"ttl" env:"AUTH_CACHE_TTL" validate:"gte=0,lte=600"`
	// NegativeTTL is how long a refused login is answered from the
	// cache, for exactly the same credentials; 0 (the default) caches no
	// refusal.
	NegativeTTL int64 `mapstructure:"negative_ttl" env:"AUTH_CACHE_NEGATIVE_TTL" validate:"gte=0,lte=10"`
	// MaxEntries bounds the entries of logins and of lookups, each.
	MaxEntries int64 `mapstructure:"max_entries" env:"AUTH_CACHE_MAX_ENTRIES" validate:"gt=0,lte=10000000"`
	// KeyClientIP puts the client's IP address into the key of a login,
	// so that address-based rules in Nauthilus see every address.
	KeyClientIP bool `mapstructure:"key_client_ip" env:"AUTH_CACHE_KEY_CLIENT_IP"`
	// LookupTTL is how long a lookup that found an account is answered
	// from the cache; 0 caches no lookup.
	LookupTTL int64 `mapstructure:"lookup_ttl" env:"AUTH_CACHE_LOOKUP_TTL" validate:"gte=0,lte=600"`
	// LookupNegativeTTL is how long a lookup that found no account is
	// answered from the cache; at most LookupTTL, and ignored when
	// LookupTTL is 0.
	LookupNegativeTTL int64 `mapstructure:"lookup_negative_ttl" env:"AUTH_CACHE_LOOKUP_NEGATIVE_TTL" validate:"gte=0,lte=600"`
	// LeaderBudget bounds one backend call made for every caller waiting
	// on it (a bearer login is up to three requests); 0 takes twice
	// nauthilus.timeout.
	LeaderBudget int64 `mapstructure:"leader_budget" env:"AUTH_CACHE_LEADER_BUDGET" validate:"gte=0,lte=600"`
}

// Validate checks what the field tags cannot. With lookup_ttl 0 no
// lookup is cached, the negative ones neither, so lookup_negative_ttl
// does not matter then.
func (c AuthCache) Validate() error {
	if c.LookupTTL > 0 && c.LookupNegativeTTL > c.LookupTTL {
		return fmt.Errorf("auth.cache.lookup_negative_ttl must not exceed auth.cache.lookup_ttl")
	}

	return nil
}
