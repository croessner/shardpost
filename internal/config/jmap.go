package config

import "fmt"

// JMAP configures the JMAP front-end (roadmap stage 6). It listens on an
// address of its own rather than sharing the one that serves metrics and
// the health probes: that one is cluster-internal, and JMAP is customer
// traffic with its own certificate and its own network policy.
type JMAP struct {
	// Addr is the listener; empty means JMAP is switched off.
	Addr string `mapstructure:"addr" env:"JMAP_ADDR"`
	// TLSCert and TLSKey are the certificate of the JMAP listener. Both
	// empty falls back to tls.cert and tls.key, which is what a
	// deployment with one certificate for every front-end wants. JMAP
	// without TLS is refused unless insecure_auth is on.
	TLSCert string `mapstructure:"tls_cert" env:"JMAP_TLS_CERT"`
	TLSKey  string `mapstructure:"tls_key" env:"JMAP_TLS_KEY"`
	// PublicBaseURL is the address clients reach, which behind the
	// Director is the Director's and not this pod's: its default mode
	// rewrites nothing, so the session resource has to advertise URLs
	// that work from outside. Empty builds them from the request, which
	// is right for a direct connection only.
	PublicBaseURL string `mapstructure:"public_base_url" env:"JMAP_PUBLIC_BASE_URL"`
	// InsecureAuth allows authentication without TLS. Only for tests and
	// for a listener behind a proxy that terminates TLS itself.
	InsecureAuth bool `mapstructure:"insecure_auth" env:"JMAP_INSECURE_AUTH"`
	// The limits the session resource advertises and the server
	// enforces; 0 takes the built-in default.
	MaxSizeUpload         int64 `mapstructure:"max_size_upload" env:"JMAP_MAX_SIZE_UPLOAD" validate:"gte=0"`
	MaxSizeRequest        int64 `mapstructure:"max_size_request" env:"JMAP_MAX_SIZE_REQUEST" validate:"gte=0"`
	MaxConcurrentUpload   int64 `mapstructure:"max_concurrent_upload" env:"JMAP_MAX_CONCURRENT_UPLOAD" validate:"gte=0"`
	MaxConcurrentRequests int64 `mapstructure:"max_concurrent_requests" env:"JMAP_MAX_CONCURRENT_REQUESTS" validate:"gte=0"`
	MaxCallsInRequest     int64 `mapstructure:"max_calls_in_request" env:"JMAP_MAX_CALLS_IN_REQUEST" validate:"gte=0"`
	MaxObjectsInGet       int64 `mapstructure:"max_objects_in_get" env:"JMAP_MAX_OBJECTS_IN_GET" validate:"gte=0"`
	MaxObjectsInSet       int64 `mapstructure:"max_objects_in_set" env:"JMAP_MAX_OBJECTS_IN_SET" validate:"gte=0"`
}

// Enabled reports whether the JMAP listener is configured.
func (j JMAP) Enabled() bool { return j.Addr != "" }

// Validate checks the listener and the certificate pair. It is given the
// global TLS settings, because an empty JMAP pair falls back to them.
func (j JMAP) Validate(global TLS) error {
	if !j.Enabled() {
		return nil
	}

	if err := checkListener(j.Addr); err != nil {
		return fmt.Errorf("jmap.addr: %w", err)
	}

	if (j.TLSCert == "") != (j.TLSKey == "") {
		return fmt.Errorf("jmap.tls_cert and jmap.tls_key must be set together")
	}

	if j.TLSCert == "" && global.Cert == "" && !j.InsecureAuth {
		return fmt.Errorf("jmap needs a certificate (jmap.tls_cert or tls.cert), or jmap.insecure_auth")
	}

	return nil
}

// Certificate is the certificate pair of the JMAP listener: its own if
// it has one, otherwise the global one. An empty pair means the listener
// is plaintext, which Validate only allows with insecure_auth.
func (j JMAP) Certificate(global TLS) (cert, key string) {
	if j.TLSCert != "" {
		return j.TLSCert, j.TLSKey
	}

	return global.Cert, global.Key
}
