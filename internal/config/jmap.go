package config

import (
	"fmt"
	"strings"
)

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
	// ShardAttribute and Shard restrict JMAP to the accounts the
	// authentication backend assigns to this shard (for Nauthilus with the
	// Director, mailShard). A proxy in front that does not route JMAP per
	// account lets every account of the deployment reach this server; one
	// whose mail lives elsewhere would see an empty account here, and what
	// it wrote would land in the wrong shard. Both empty serve every
	// authenticated account.
	ShardAttribute string `mapstructure:"shard_attribute" env:"JMAP_SHARD_ATTRIBUTE"`
	Shard          string `mapstructure:"shard" env:"JMAP_SHARD"`
	// Push configures push subscriptions and the push worker.
	Push JMAPPush `mapstructure:"push"`
	// Submission configures sending (EmailSubmission) and identities.
	Submission JMAPSubmission `mapstructure:"submission"`
}

// JMAPSubmission configures EmailSubmission and Identity (RFC 8621 6 and
// 7): the relay the messages are handed to, and what ShardPost checks
// before. Without relay_addr the submission capability is not offered.
type JMAPSubmission struct {
	// RelayAddr is the relay's submission port (host:port).
	RelayAddr string `mapstructure:"relay_addr" env:"JMAP_SUBMISSION_RELAY_ADDR"`
	Helo      string `mapstructure:"helo" env:"JMAP_SUBMISSION_HELO"`
	// Starttls requires STARTTLS (on by default: XCLIENT names the user,
	// and a relay whose STARTTLS a man in the middle strips must not get
	// that in the clear); TLS is implicit TLS (port 465).
	Starttls   bool   `mapstructure:"starttls" env:"JMAP_SUBMISSION_STARTTLS"`
	TLS        bool   `mapstructure:"tls" env:"JMAP_SUBMISSION_TLS"`
	ServerName string `mapstructure:"server_name" env:"JMAP_SUBMISSION_SERVER_NAME"`
	// XClient names the user to the relay with XCLIENT LOGIN and ADDR, so
	// that the relay's rules for that user apply (on by default). Off,
	// the relay is logged into with User and Password, if set.
	XClient bool `mapstructure:"xclient" env:"JMAP_SUBMISSION_XCLIENT"`
	// XClientAddr passes the JMAP client's address with XCLIENT (ADDR, on
	// by default), so the relay's policies and rate limits see it. Turn it
	// off for a relay that relays by ShardPost's own address (mynetworks):
	// with ADDR the session would no longer come from there.
	XClientAddr  bool   `mapstructure:"xclient_addr" env:"JMAP_SUBMISSION_XCLIENT_ADDR"`
	User         string `mapstructure:"user" env:"JMAP_SUBMISSION_USER"`
	Password     string `mapstructure:"password" env:"JMAP_SUBMISSION_PASSWORD" secret:"true"`
	PasswordFile string `mapstructure:"password_file" env:"JMAP_SUBMISSION_PASSWORD_FILE"`
	// TimeoutSeconds bounds one conversation with the relay; 0 = 30.
	TimeoutSeconds int64 `mapstructure:"timeout_seconds" env:"JMAP_SUBMISSION_TIMEOUT_SECONDS" validate:"gte=0"`
	// CheckSender refuses an envelope sender or From that is not one of
	// the account's addresses before the relay is asked (on by default).
	CheckSender bool `mapstructure:"check_sender" env:"JMAP_SUBMISSION_CHECK_SENDER"`
	// AddressAttributes are the comma-separated attributes of the
	// identity lookup that carry the addresses an account may send from.
	AddressAttributes string `mapstructure:"address_attributes" env:"JMAP_SUBMISSION_ADDRESS_ATTRIBUTES"`
	// MaxRecipients (0 = 100), MaxSize (0 = jmap.max_size_upload), and
	// how long (0 = 7 days) and how many (0 = 500) submission records an
	// account keeps.
	MaxRecipients    int64 `mapstructure:"max_recipients" env:"JMAP_SUBMISSION_MAX_RECIPIENTS" validate:"gte=0"`
	MaxSize          int64 `mapstructure:"max_size" env:"JMAP_SUBMISSION_MAX_SIZE" validate:"gte=0"`
	RetentionSeconds int64 `mapstructure:"retention_seconds" env:"JMAP_SUBMISSION_RETENTION_SECONDS" validate:"gte=0"`
	MaxKept          int64 `mapstructure:"max_kept" env:"JMAP_SUBMISSION_MAX_KEPT" validate:"gte=0"`
}

// Enabled reports whether submission is configured.
func (s JMAPSubmission) Enabled() bool { return s.RelayAddr != "" }

// Validate refuses a relay that nobody checks: without XCLIENT the relay
// sees ShardPost, not the user, and then ShardPost has to check the
// sender itself.
func (s JMAPSubmission) Validate() error {
	if !s.Enabled() {
		return nil
	}

	if err := checkListener(s.RelayAddr); err != nil {
		return fmt.Errorf("jmap.submission.relay_addr: %w", err)
	}

	if !s.XClient && !s.CheckSender {
		return fmt.Errorf("jmap.submission: with xclient off, check_sender has to be on")
	}

	return nil
}

// defaultAddressAttribute is the attribute that carries an account's
// addresses when none is configured.
const defaultAddressAttribute = "mail"

// Attributes are the address attributes as a list; "mail" when none.
func (s JMAPSubmission) Attributes() []string {
	var out []string

	for _, a := range strings.Split(s.AddressAttributes, ",") {
		if a = strings.TrimSpace(a); a != "" {
			out = append(out, a)
		}
	}

	if len(out) == 0 {
		out = []string{defaultAddressAttribute}
	}

	return out
}

// JMAPPush configures JMAP push subscriptions (RFC 8620 7.2) and the
// worker that sends them ("shardpost push"). The event source needs none
// of it. Push subscriptions are offered when a VAPID key is configured;
// the front-ends read its public half, the worker signs with it.
type JMAPPush struct {
	// VAPIDKeyFile is a P-256 private key in PEM (RFC 8292).
	VAPIDKeyFile string `mapstructure:"vapid_key_file" env:"JMAP_PUSH_VAPID_KEY_FILE"`
	// VAPIDSubject is a mailto: or https: contact for push services.
	VAPIDSubject string `mapstructure:"vapid_subject" env:"JMAP_PUSH_VAPID_SUBJECT"`
	// MaxSubscriptions bounds the subscriptions of one account; 0 = 16.
	MaxSubscriptions int64 `mapstructure:"max_subscriptions" env:"JMAP_PUSH_MAX_SUBSCRIPTIONS" validate:"gte=0"`
	// MaxExpirySeconds bounds how far ahead a subscription expires;
	// 0 = 7 days, and never less than the 48 hours RFC 8620 asks for.
	MaxExpirySeconds int64 `mapstructure:"max_expiry_seconds" env:"JMAP_PUSH_MAX_EXPIRY_SECONDS" validate:"gte=0"`
	// EventPollSeconds is how often an open event source checks the
	// account without a notification; 0 = 30.
	EventPollSeconds int64 `mapstructure:"event_poll_seconds" env:"JMAP_PUSH_EVENT_POLL_SECONDS" validate:"gte=0"`
	// AllowInternalEndpoints accepts push URLs inside the server's own
	// network (an in-house push service). Off, they are refused.
	AllowInternalEndpoints bool `mapstructure:"allow_internal_endpoints" env:"JMAP_PUSH_ALLOW_INTERNAL_ENDPOINTS"`
	// CheckAccounts has the worker look every account with subscriptions
	// up in the authentication backend (hourly) and destroy the
	// subscriptions of one it no longer knows. It needs the same backend
	// settings as the server; a worker without them would find no account.
	CheckAccounts bool `mapstructure:"check_accounts" env:"JMAP_PUSH_CHECK_ACCOUNTS"`
	// The worker: how often it reads the subscriptions, how often it
	// checks a watched account without a notification, how long a request
	// to a push service may take, and the TTL a push service keeps an
	// undelivered message for. 0 takes the default (30, 60, 10, 86400).
	ScanSeconds       int64 `mapstructure:"scan_seconds" env:"JMAP_PUSH_SCAN_SECONDS" validate:"gte=0"`
	PollSeconds       int64 `mapstructure:"poll_seconds" env:"JMAP_PUSH_POLL_SECONDS" validate:"gte=0"`
	TimeoutSeconds    int64 `mapstructure:"timeout_seconds" env:"JMAP_PUSH_TIMEOUT_SECONDS" validate:"gte=0"`
	MessageTTLSeconds int64 `mapstructure:"message_ttl_seconds" env:"JMAP_PUSH_MESSAGE_TTL_SECONDS" validate:"gte=0"`
}

// Enabled reports whether push subscriptions are configured.
func (p JMAPPush) Enabled() bool { return p.VAPIDKeyFile != "" }

// Validate checks the VAPID settings.
func (p JMAPPush) Validate() error {
	if !p.Enabled() {
		return nil
	}

	if !strings.HasPrefix(p.VAPIDSubject, "mailto:") && !strings.HasPrefix(p.VAPIDSubject, "https:") {
		return fmt.Errorf("jmap.push.vapid_subject must be a mailto: or https: URI")
	}

	return nil
}

// Enabled reports whether the JMAP listener is configured.
func (j JMAP) Enabled() bool { return j.Addr != "" }

// Validate checks the listener and the certificate pair. It is given the
// global TLS settings, because an empty JMAP pair falls back to them.
func (j JMAP) Validate(global TLS) error {
	if err := j.Push.Validate(); err != nil {
		return err
	}

	if err := j.Submission.Validate(); err != nil {
		return err
	}

	if (strings.TrimSpace(j.ShardAttribute) == "") != (strings.TrimSpace(j.Shard) == "") {
		return fmt.Errorf("jmap.shard_attribute and jmap.shard must be set together")
	}

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
