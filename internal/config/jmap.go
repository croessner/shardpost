package config

import (
	"fmt"
	"strings"
)

// JMAP configures the JMAP front-end (roadmap stage 6). It listens on an
// address of its own rather than sharing the one that serves metrics and
// the health probes: that one is cluster-internal, and JMAP is customer
// traffic with its own certificate and its own network policy.
type JMAP struct { //nolint:dupl // a configuration section is a list of tagged fields, like the others
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
	MaxSizeUpload int64 `mapstructure:"max_size_upload" env:"JMAP_MAX_SIZE_UPLOAD" validate:"gte=0"`
	// UploadTTLSeconds is how long an uploaded blob is kept (RFC 8620
	// 6.1 asks for at least an hour); the upload expiry worker deletes it
	// then.
	UploadTTLSeconds      int64 `mapstructure:"upload_ttl_seconds" env:"JMAP_UPLOAD_TTL_SECONDS" validate:"gte=3600,lte=2592000"`
	MaxSizeRequest        int64 `mapstructure:"max_size_request" env:"JMAP_MAX_SIZE_REQUEST" validate:"gte=0"`
	MaxConcurrentUpload   int64 `mapstructure:"max_concurrent_upload" env:"JMAP_MAX_CONCURRENT_UPLOAD" validate:"gte=0"`
	MaxConcurrentRequests int64 `mapstructure:"max_concurrent_requests" env:"JMAP_MAX_CONCURRENT_REQUESTS" validate:"gte=0"`
	MaxCallsInRequest     int64 `mapstructure:"max_calls_in_request" env:"JMAP_MAX_CALLS_IN_REQUEST" validate:"gte=0"`
	// MaxConcurrentDownloads bounds the blob downloads one account has
	// in flight, MaxEventSources its open event sources; 0 takes the
	// default (8 and 4).
	MaxConcurrentDownloads int64 `mapstructure:"max_concurrent_downloads" env:"JMAP_MAX_CONCURRENT_DOWNLOADS" validate:"gte=0"`
	MaxEventSources        int64 `mapstructure:"max_event_sources" env:"JMAP_MAX_EVENT_SOURCES" validate:"gte=0"`
	// LimitWait is how long, in seconds, a request waits for a slot when
	// its account is at a limit (requests, uploads, downloads) before it
	// is answered 429; 0 answers at once. Event sources never wait. It
	// stays well below the response timeouts of the proxies in front.
	LimitWait int64 `mapstructure:"limit_wait" env:"JMAP_LIMIT_WAIT" validate:"gte=0,lte=10"`
	// RetryAfter is the Retry-After, in seconds, of a 429 and of a 503
	// from an authentication backend that cannot decide.
	RetryAfter int64 `mapstructure:"retry_after" env:"JMAP_RETRY_AFTER" validate:"gte=1,lte=3600"`
	// MaxConcurrentPerIP bounds the requests one client IP address has
	// in flight, checked before authentication; 0 (the default) is no
	// bound. Behind a proxy it counts the proxy unless the PROXY
	// protocol passes the client's address
	// (server.proxy_protocol_trusted_cidrs).
	MaxConcurrentPerIP int64 `mapstructure:"max_concurrent_per_ip" env:"JMAP_MAX_CONCURRENT_PER_IP" validate:"gte=0"`
	MaxObjectsInGet    int64 `mapstructure:"max_objects_in_get" env:"JMAP_MAX_OBJECTS_IN_GET" validate:"gte=0"`
	MaxObjectsInSet    int64 `mapstructure:"max_objects_in_set" env:"JMAP_MAX_OBJECTS_IN_SET" validate:"gte=0"`
	// The limits of the blob methods (RFC 9404): the size of a blob
	// Blob/upload builds (never more than max_size_upload), its data
	// sources (at least 64), and the blob data one Blob/get returns.
	MaxSizeBlobSet int64 `mapstructure:"max_size_blob_set" env:"JMAP_MAX_SIZE_BLOB_SET" validate:"gte=0"`
	MaxDataSources int64 `mapstructure:"max_data_sources" env:"JMAP_MAX_DATA_SOURCES" validate:"gte=0"`
	MaxSizeBlobGet int64 `mapstructure:"max_size_blob_get" env:"JMAP_MAX_SIZE_BLOB_GET" validate:"gte=0"`
	// ShardAttribute and Shard restrict JMAP to the accounts the
	// authentication backend assigns to this shard (for Nauthilus with the
	// Director, mailShard). A proxy in front that does not route JMAP per
	// account lets every account of the deployment reach this server; one
	// whose mail lives elsewhere would see an empty account here, and what
	// it wrote would land in the wrong shard. Both empty serve every
	// authenticated account.
	ShardAttribute string `mapstructure:"shard_attribute" env:"JMAP_SHARD_ATTRIBUTE"`
	Shard          string `mapstructure:"shard" env:"JMAP_SHARD"`
	// MaxSharedAccounts bounds how many accounts of other users (owners
	// who share a mailbox with the user) the session resource lists; 0
	// takes the built-in default. The public folders are one account
	// more. Shared accounts follow server.shared_prefix and
	// server.public_prefix: an empty prefix switches that namespace off.
	MaxSharedAccounts int64 `mapstructure:"max_shared_accounts" env:"JMAP_MAX_SHARED_ACCOUNTS" validate:"gte=0"`
	// MaxSharedMailboxes bounds the mailboxes one shared account examines
	// (the user's pointers to one owner, or the visible public folders);
	// 0 takes the built-in default. What lies beyond is left out and
	// logged.
	MaxSharedMailboxes int64 `mapstructure:"max_shared_mailboxes" env:"JMAP_MAX_SHARED_MAILBOXES" validate:"gte=0"`
	// MaxSessionPointers bounds the pointers the session resource and the
	// session state of every API response examine in all; 0 takes eight
	// times max_shared_mailboxes.
	MaxSessionPointers int64 `mapstructure:"max_session_pointers" env:"JMAP_MAX_SESSION_POINTERS" validate:"gte=0"`
	// MaxPrincipalLookups bounds the addresses one API request may look
	// up in the authentication backend by an exact match (Principal/query
	// email, Principal/get and shareWith ids of accounts the user does not
	// see already); 0 takes 5.
	MaxPrincipalLookups int64 `mapstructure:"max_principal_lookups" env:"JMAP_MAX_PRINCIPAL_LOOKUPS" validate:"gte=0"`
	// MaxPrincipals bounds the principals a user sees without a lookup:
	// itself, the owners who share with it and the identifiers in the
	// ACLs of its own mailboxes; 0 takes 1000.
	MaxPrincipals int64 `mapstructure:"max_principals" env:"JMAP_MAX_PRINCIPALS" validate:"gte=0"`
	// PrincipalMissPerMinute bounds, per account and across requests, the
	// lookups of addresses that turn out to be no principal here (unknown,
	// an alias, another shard): the refill rate of a bucket of that size,
	// kept in each front-end's memory; 0 takes 30. A lookup that finds a
	// principal is not counted.
	PrincipalMissPerMinute int64 `mapstructure:"principal_miss_per_minute" env:"JMAP_PRINCIPAL_MISS_PER_MINUTE" validate:"gte=0"`
	// Search bounds what SearchSnippet/get and the hasAttachment filter
	// of Email/query read.
	Search JMAPSearch `mapstructure:"search"`
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
	// Delayed configures delayed sending (FUTURERELEASE, scaling plan
	// B10) and its worker.
	Delayed JMAPDelayedSend `mapstructure:"delayed"`
}

// JMAPDelayedSend configures delayed sending: EmailSubmission with
// HOLDFOR or HOLDUNTIL (RFC 4865, RFC 8621 7), kept in the submission
// queue of the admin store and sent by a partitioned worker.
type JMAPDelayedSend struct {
	// MaxDelaySeconds is the longest delay a client may ask for
	// (maxDelayedSend); 0, the default, offers no delayed sending.
	MaxDelaySeconds int64 `mapstructure:"max_delay_seconds" env:"JMAP_SUBMISSION_DELAYED_MAX_DELAY_SECONDS" validate:"gte=0,lte=31536000"`
	// Worker runs the worker in this server (needs relay_addr). It keeps
	// running when max_delay_seconds is 0, so that a queue left behind
	// is still sent.
	Worker bool `mapstructure:"worker" env:"JMAP_SUBMISSION_DELAYED_WORKER"`
	// Partitions splits the accounts among the workers (by hash), each
	// partition with a lease of its own; MaxPartitions bounds how many
	// one server holds (0: all).
	Partitions    int64 `mapstructure:"partitions" env:"JMAP_SUBMISSION_DELAYED_PARTITIONS" validate:"gte=1,lte=64"`
	MaxPartitions int64 `mapstructure:"max_partitions" env:"JMAP_SUBMISSION_DELAYED_MAX_PARTITIONS" validate:"gte=0,lte=64"`
	// PollSeconds is how often a worker looks at the queue without a
	// notification.
	PollSeconds int64 `mapstructure:"poll_seconds" env:"JMAP_SUBMISSION_DELAYED_POLL_SECONDS" validate:"gte=1,lte=3600"`
	// SendingTimeoutSeconds is how long a submission may stay in
	// "sending" before it counts as final with an unknown result (it is
	// never sent again). It has to exceed the longest conversation with
	// the relay.
	SendingTimeoutSeconds int64 `mapstructure:"sending_timeout_seconds" env:"JMAP_SUBMISSION_DELAYED_SENDING_TIMEOUT_SECONDS" validate:"gte=1800,lte=86400"`
	// Concurrency bounds the conversations with the relay one worker
	// holds at once.
	Concurrency int64 `mapstructure:"concurrency" env:"JMAP_SUBMISSION_DELAYED_CONCURRENCY" validate:"gte=1,lte=64"`
	// RetryAttempts bounds the attempts of a submission whose relay
	// failed temporarily before the message was transferred.
	RetryAttempts int64 `mapstructure:"retry_attempts" env:"JMAP_SUBMISSION_DELAYED_RETRY_ATTEMPTS" validate:"gte=1,lte=20"`
}

// Enabled reports whether delayed sending is offered.
func (d JMAPDelayedSend) Enabled() bool { return d.MaxDelaySeconds > 0 }

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

	if s.Delayed.MaxPartitions > s.Delayed.Partitions {
		return fmt.Errorf("jmap.submission.delayed.max_partitions must not exceed partitions")
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
	// MaxSharedWatches bounds the shared accounts one event source, and
	// the worker for one account, watch; the others are checked on the
	// poll. 0 = 8.
	MaxSharedWatches int64 `mapstructure:"max_shared_watches" env:"JMAP_PUSH_MAX_SHARED_WATCHES" validate:"gte=0"`
	// WorkerMaxWatches bounds the namespaces the worker watches for
	// shared accounts in all; one watch serves every account that shares
	// it. 0 = 1000.
	WorkerMaxWatches int64 `mapstructure:"worker_max_watches" env:"JMAP_PUSH_WORKER_MAX_WATCHES" validate:"gte=0"`
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
	// The partitions of the worker (stage B18a). The accounts are split
	// into as many partitions as the hint index of the subscriptions has
	// shards (64), each with a lease of its own, and the push pods share
	// them out among themselves by their membership record. The defaults
	// are in the defaults table (config.go); none of these is "0 takes
	// the default".
	//
	// MaxPartitions bounds how many partitions one pod holds (0: all).
	MaxPartitions int64 `mapstructure:"max_partitions" env:"JMAP_PUSH_MAX_PARTITIONS" validate:"gte=0,lte=64"`
	// LeaseSeconds is the TTL of a partition lease (30), renewed every
	// third of it; a holder that could not renew stops the partition
	// itself five seconds before the TTL is up and releases it. A
	// member's entry in the membership expires after the same time.
	LeaseSeconds int64 `mapstructure:"lease_seconds" env:"JMAP_PUSH_LEASE_SECONDS" validate:"gte=15,lte=300"`
	// TakeSeconds is the cadence of taking and shedding partitions (5,
	// ± 20 %), and the deadline of one lock or membership operation;
	// RetrySeconds (2) the faster cadence for partitions whose holder is
	// gone or that are contested.
	TakeSeconds  int64 `mapstructure:"take_seconds" env:"JMAP_PUSH_TAKE_SECONDS" validate:"gte=1,lte=60"`
	RetrySeconds int64 `mapstructure:"retry_seconds" env:"JMAP_PUSH_RETRY_SECONDS" validate:"gte=1,lte=60"`
	// OrphanHoldSeconds (600) is how long a pod keeps a partition it took
	// as a replacement (orphaned or contested) before it may shed it to
	// even the load.
	OrphanHoldSeconds int64 `mapstructure:"orphan_hold_seconds" env:"JMAP_PUSH_ORPHAN_HOLD_SECONDS" validate:"gte=0,lte=3600"`
	// FullScanSeconds (3600) is how often a scan reads every subscription
	// record of a partition whatever the checksum in the index says, the
	// safety net for index values written by older servers; 0 never.
	FullScanSeconds int64 `mapstructure:"full_scan_seconds" env:"JMAP_PUSH_FULL_SCAN_SECONDS" validate:"gte=0"`
	// RebalanceSeconds (10) is the pause between two partitions a pod
	// sheds; RebalanceSettleSeconds (60) how long the set of pods has to
	// stand still before one sheds at all.
	RebalanceSeconds       int64 `mapstructure:"rebalance_seconds" env:"JMAP_PUSH_REBALANCE_SECONDS" validate:"gte=1,lte=600"`
	RebalanceSettleSeconds int64 `mapstructure:"rebalance_settle_seconds" env:"JMAP_PUSH_REBALANCE_SETTLE_SECONDS" validate:"gte=0,lte=3600"`
	// HandoffSeconds (5) is how long the watchers of a partition get to
	// end their round when it is shed, and the period of the waves in
	// which a pod gives its partitions up at shutdown; it stays below
	// the pod's termination grace period.
	HandoffSeconds int64 `mapstructure:"handoff_seconds" env:"JMAP_PUSH_HANDOFF_SECONDS" validate:"gte=1,lte=30"`
	// MaxWatchedAccounts (50000) bounds the accounts one pod watches: it
	// takes no partition that would put it over, by the estimate the
	// membership gives. StartRate (5000) bounds the watchers a pod starts
	// per second when it takes a partition.
	MaxWatchedAccounts int64 `mapstructure:"max_watched_accounts" env:"JMAP_PUSH_MAX_WATCHED_ACCOUNTS" validate:"gte=100"`
	StartRate          int64 `mapstructure:"start_rate" env:"JMAP_PUSH_START_RATE" validate:"gte=10,lte=100000"`
}

// The defaults of the worker's partition settings (the defaults table
// of config.go takes them; the worker reads the loaded values).
const (
	DefaultPushLeaseSeconds           = 30
	DefaultPushTakeSeconds            = 5
	DefaultPushRetrySeconds           = 2
	DefaultPushOrphanHoldSeconds      = 600
	DefaultPushFullScanSeconds        = 3600
	DefaultPushRebalanceSeconds       = 10
	DefaultPushRebalanceSettleSeconds = 60
	DefaultPushHandoffSeconds         = 5
	DefaultPushMaxWatchedAccounts     = 50000
	DefaultPushStartRate              = 5000
	// defaultPushScanSeconds is what scan_seconds 0 means (the worker
	// fills it in).
	defaultPushScanSeconds = 30
	// PushSelfStopMargin is how many seconds before its TTL is up a
	// holder stops a partition it could not renew.
	PushSelfStopMargin = 5
)

// Enabled reports whether push subscriptions are configured.
func (p JMAPPush) Enabled() bool { return p.VAPIDKeyFile != "" }

// Validate checks the VAPID settings and the relations between the
// partition settings: the lease has to outlast three cadences (a
// partition free for two of them is contested), a member's entry has to
// expire before another pod sheds to it, a hand-over has to end before
// the holder would stop the partition itself, the fast cadence may not
// exceed the slow one, and a full pass is at least one scan apart. They
// are checked whenever the key is set: the push pods have the same
// configuration as the front-ends.
func (p JMAPPush) Validate() error {
	if !p.Enabled() {
		return nil
	}

	if !strings.HasPrefix(p.VAPIDSubject, "mailto:") && !strings.HasPrefix(p.VAPIDSubject, "https:") {
		return fmt.Errorf("jmap.push.vapid_subject must be a mailto: or https: URI")
	}

	scan := p.ScanSeconds
	if scan <= 0 {
		scan = defaultPushScanSeconds
	}

	switch {
	case p.LeaseSeconds < 3*p.TakeSeconds:
		return fmt.Errorf("jmap.push.lease_seconds (%d) must be at least three times take_seconds (%d)", p.LeaseSeconds, p.TakeSeconds)
	case p.RebalanceSettleSeconds < p.LeaseSeconds:
		return fmt.Errorf("jmap.push.rebalance_settle_seconds (%d) must not be below lease_seconds (%d)", p.RebalanceSettleSeconds, p.LeaseSeconds)
	case p.HandoffSeconds >= p.LeaseSeconds-PushSelfStopMargin:
		return fmt.Errorf("jmap.push.handoff_seconds (%d) must be below lease_seconds - %d", p.HandoffSeconds, PushSelfStopMargin)
	case p.RetrySeconds > p.TakeSeconds:
		return fmt.Errorf("jmap.push.retry_seconds (%d) must not exceed take_seconds (%d)", p.RetrySeconds, p.TakeSeconds)
	case p.FullScanSeconds != 0 && p.FullScanSeconds < scan:
		return fmt.Errorf("jmap.push.full_scan_seconds (%d) must be 0 or at least scan_seconds (%d)", p.FullScanSeconds, scan)
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

// JMAPSearch bounds the reads behind SearchSnippet/get and the
// hasAttachment filter of Email/query; 0 takes the default everywhere.
type JMAPSearch struct {
	// SnippetReadBytes bounds how much of each message SearchSnippet/get
	// reads to find a match in the body (its front, where the text parts
	// are); 0 takes 256 KiB. A match beyond it gives no preview snippet.
	SnippetReadBytes int64 `mapstructure:"snippet_read_bytes" env:"JMAP_SEARCH_SNIPPET_READ_BYTES" validate:"gte=0"`
	// MaxAttachmentReads bounds the messages one Email/query may read to
	// answer hasAttachment for messages whose index record does not carry
	// it (stored before the record had the field); 0 takes 1000. A query
	// past it starts a background fill and answers serverUnavailable.
	MaxAttachmentReads int64 `mapstructure:"max_attachment_reads" env:"JMAP_SEARCH_MAX_ATTACHMENT_READS" validate:"gte=0"`
	// AttachmentBackfills bounds the background fills of hasAttachment
	// (one per namespace) that run at once in the process; 0 takes 2.
	// AttachmentBackfillSeconds bounds how long one runs; 0 takes 600.
	AttachmentBackfills       int64 `mapstructure:"attachment_backfills" env:"JMAP_SEARCH_ATTACHMENT_BACKFILLS" validate:"gte=0"`
	AttachmentBackfillSeconds int64 `mapstructure:"attachment_backfill_seconds" env:"JMAP_SEARCH_ATTACHMENT_BACKFILL_SECONDS" validate:"gte=0"`
}
