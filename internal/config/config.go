// Package config loads the server configuration once, before opening any services.
package config

import (
	"fmt"
	"github.com/go-playground/validator/v10"
	"github.com/go-viper/mapstructure/v2"
	"github.com/spf13/viper"
	"math"
	"net"
	"os"
	"reflect"
	"slices"
	"strconv"
	"strings"

	"shardpost/internal/charset"
)

// Config is the complete server configuration, read from YAML and the
// environment by Load.
type Config struct {
	Logging   Logging   `mapstructure:"logging"`
	IMAPSieve IMAPSieve `mapstructure:"imapsieve"`
	Telemetry Telemetry `mapstructure:"telemetry"`
	Ceph      Ceph      `mapstructure:"ceph"`
	Server    Server    `mapstructure:"server"`
	Rook      Rook      `mapstructure:"rook"`
	TLS       TLS       `mapstructure:"tls"`
	Sieve     Sieve     `mapstructure:"sieve"`
	SMTP      SMTP      `mapstructure:"smtp"`
	Scrub     Scrub     `mapstructure:"scrub"`
	Purge     Purge     `mapstructure:"purge"`
	Nauthilus Nauthilus `mapstructure:"nauthilus"`
	// Auth configures the auth cache in front of Nauthilus.
	Auth Auth `mapstructure:"auth"`
	// ExtPrograms is the allowlist of external programs (Sieve execute/
	// pipe/filter and the learning rules).
	ExtPrograms ExtPrograms `mapstructure:"extprograms"`
	// Quota holds grace, mailbox rules, warnings and the over-quota flag.
	Quota Quota `mapstructure:"quota"`
	// Admin configures the internal admin API listeners.
	Admin Admin `mapstructure:"admin"`
	// Trace bounds the per-account protocol trace.
	Trace Trace `mapstructure:"trace"`
	// Jobs configures the maintenance jobs.
	Jobs Jobs `mapstructure:"jobs"`
	// JMAP configures the JMAP front-end (off without an address).
	JMAP JMAP `mapstructure:"jmap"`
	// FTS configures the text preparation of the full-text index.
	FTS FTS `mapstructure:"fts"`
	// Limits bounds what one account can grow (mailboxes, keywords,
	// METADATA entries, shared pointers).
	Limits Limits `mapstructure:"limits"`
}

// Logging configures the log output.
type Logging struct {
	Level string `mapstructure:"level" env:"LOG_LEVEL" validate:"oneof=debug info warn error"`
}

// Telemetry configures OpenTelemetry tracing.
type Telemetry struct {
	ServiceName string  `mapstructure:"service_name" env:"OTEL_SERVICE_NAME" validate:"required"`
	Endpoint    string  `mapstructure:"endpoint" env:"OTEL_EXPORTER_OTLP_ENDPOINT"`
	Protocol    string  `mapstructure:"protocol" env:"OTEL_EXPORTER_OTLP_PROTOCOL" validate:"oneof=grpc http/protobuf"`
	SampleRatio float64 `mapstructure:"sample_ratio" env:"OTEL_TRACES_SAMPLER_ARG" validate:"gte=0,lte=1"`
	// UserAttributes lets spans carry account and mailbox names and
	// client addresses. Off by default: traces then hold no personal data.
	UserAttributes bool `mapstructure:"user_attributes" env:"TELEMETRY_USER_ATTRIBUTES"`
}

// Ceph configures the RADOS connection and pools.
type Ceph struct {
	User      string `mapstructure:"user" env:"CEPH_USER"`
	Key       string `mapstructure:"key" env:"CEPH_KEY" secret:"true"`
	MonHost   string `mapstructure:"mon_host" env:"CEPH_MON_HOST"`
	IndexPool string `mapstructure:"index_pool" env:"CEPH_INDEX_POOL"`
	DataPool  string `mapstructure:"data_pool" env:"CEPH_DATA_POOL"`
	// SystemPool holds the store's own namespaces (admin store, account
	// directory, queues, locks) instead of the index pool; "" keeps them
	// in the index pool. A server with it starts only on a pool that
	// "shardpost system init" or "shardpost system migrate" prepared.
	SystemPool string `mapstructure:"system_pool" env:"CEPH_SYSTEM_POOL"`
	ChunkSize  int64  `mapstructure:"chunk_size" env:"CEPH_CHUNK_SIZE" validate:"gte=0"`
	// ClientOptions are librados options of the connection, limited to
	// ClientOptionNames (the objecter throttles and the op timeouts).
	ClientOptions map[string]string `mapstructure:"client_options"`
}

// ClientOptionNames are the librados options ceph.client_options may
// set: the objecter's throttles of the operations and bytes in flight
// per client, and the timeouts of OSD and monitor operations (seconds).
var ClientOptionNames = []string{"objecter_inflight_ops", "objecter_inflight_op_bytes", "rados_osd_op_timeout", "rados_mon_op_timeout"}

// checkCeph validates the pools and the client options.
func (c *Config) checkCeph() error {
	if p := c.Ceph.SystemPool; p != "" && (p == c.Ceph.IndexPool || p == c.Ceph.DataPool) {
		return fmt.Errorf("ceph.system_pool must differ from ceph.index_pool and ceph.data_pool")
	}

	for k, v := range c.Ceph.ClientOptions {
		if !slices.Contains(ClientOptionNames, k) {
			return fmt.Errorf("ceph.client_options: %q is not one of %s", k, strings.Join(ClientOptionNames, ", "))
		}

		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 0 || (n == 0 && strings.HasPrefix(k, "objecter_")) {
			return fmt.Errorf("ceph.client_options: %s must be a non-negative integer (the objecter throttles positive)", k)
		}
	}

	return nil
}

// Server configures listeners, limits, authentication and storage tuning.
type Server struct {
	Pop3Addr      string `mapstructure:"pop3_addr" env:"POP3_ADDR"`
	Pop3SAddr     string `mapstructure:"pop3s_addr" env:"POP3S_ADDR"`
	Pop3MarkSeen  bool   `mapstructure:"pop3_mark_seen" env:"POP3_MARK_SEEN"`
	FtsShards     int64  `mapstructure:"fts_shards" env:"FTS_SHARDS" validate:"gte=0"`
	QuotaBytes    int64  `mapstructure:"quota_bytes" env:"QUOTA_BYTES" validate:"gte=0"`
	QuotaMsgs     int64  `mapstructure:"quota_msgs" env:"QUOTA_MSGS" validate:"gte=0"`
	MboxShardSize int64  `mapstructure:"mbox_shard_size" env:"MBOX_SHARD_SIZE" validate:"gte=0"`
	// AccountLogRetention is how many entries the account change log of
	// an account keeps. It is what JMAP state tokens point into: a
	// client whose token is older than the oldest entry resynchronises.
	AccountLogRetention int64 `mapstructure:"account_log_retention" env:"ACCOUNT_LOG_RETENTION" validate:"gte=0"`
	// ShareNotificationsMax and ShareNotificationsRetentionSeconds bound
	// the share notifications (RFC 9670 3) an account keeps: how many
	// (at most 200, 0 = 50) and how long (0 = 30 days). They are written
	// by IMAP SETACL, the admin API and JMAP alike, and read by JMAP.
	ShareNotificationsMax              int64 `mapstructure:"share_notifications_max" env:"SHARE_NOTIFICATIONS_MAX" validate:"gte=0,lte=200"`
	ShareNotificationsRetentionSeconds int64 `mapstructure:"share_notifications_retention_seconds" env:"SHARE_NOTIFICATIONS_RETENTION_SECONDS" validate:"gte=0"`
	// PublicPrefix is the client-visible prefix of the public folders.
	PublicPrefix string `mapstructure:"public_prefix" env:"PUBLIC_PREFIX" validate:"omitempty,endswith=/"`
	// SharedPrefix is the client-visible prefix of the mailboxes other
	// accounts share; "" switches that namespace off.
	SharedPrefix string `mapstructure:"shared_prefix" env:"SHARED_PREFIX" validate:"omitempty,endswith=/"`
	// Autocreate are the mailboxes every account gets besides its
	// INBOX, in the order they are created. An empty list leaves an
	// account with its INBOX alone.
	Autocreate []AutocreateFolder `mapstructure:"autocreate"`
	// PublicQuotaBytes and PublicQuotaMsgs bound the public namespace as
	// a whole (0 = unlimited). Public folders do not count against the
	// quota of the account that files into them.
	PublicQuotaBytes  int64 `mapstructure:"public_quota_bytes" env:"PUBLIC_QUOTA_BYTES" validate:"gte=0"`
	PublicQuotaMsgs   int64 `mapstructure:"public_quota_msgs" env:"PUBLIC_QUOTA_MSGS" validate:"gte=0"`
	KeyPollSeconds    int64 `mapstructure:"key_poll_seconds" env:"KEY_POLL_SECONDS" validate:"gte=0,lte=86400"`
	MemoryBudgetBytes int64 `mapstructure:"memory_budget_bytes" env:"MEMORY_BUDGET_BYTES" validate:"gte=0"`
	InsecureAuth      bool  `mapstructure:"insecure_auth" env:"INSECURE_AUTH"`
	MaxConnsPerIP     int64 `mapstructure:"max_conns_per_ip" env:"MAX_CONNS_PER_IP" validate:"gte=0"`
	MaxConnsPerUser   int64 `mapstructure:"max_conns_per_user" env:"MAX_CONNS_PER_USER" validate:"gte=0"`
	// ImapMaxWatches bounds the change watches one IMAP connection
	// follows through the process-wide watch hub (IDLE one, always kept
	// free for it; NOTIFY one or two, or one per mailbox without an
	// account watch); beyond it the connection polls.
	ImapMaxWatches   int64  `mapstructure:"imap_max_watches" env:"IMAP_MAX_WATCHES" validate:"gte=2,lte=1024"`
	QuotaSelfService bool   `mapstructure:"quota_self_service" env:"QUOTA_SELF_SERVICE"`
	QuotaAttribute   string `mapstructure:"quota_attribute" env:"QUOTA_ATTRIBUTE"`
	LmtpAddr         string `mapstructure:"lmtp_addr" env:"LMTP_ADDR"`
	LmtpHostname     string `mapstructure:"lmtp_hostname" env:"LMTP_HOSTNAME"`
	LmtpAuthUser     string `mapstructure:"lmtp_auth_user" env:"LMTP_AUTH_USER"`
	LmtpAuthPassword string `mapstructure:"lmtp_auth_password" env:"LMTP_AUTH_PASSWORD" secret:"true"`
	// LmtpReturnPath adds "Return-Path: <envelope sender>" to every
	// delivered copy (LMTP and local delivery) and removes the
	// Return-Path fields the message brought along.
	LmtpReturnPath bool `mapstructure:"lmtp_return_path" env:"LMTP_RETURN_PATH"`
	// LmtpDeliveredTo selects the Delivered-To field of every delivered
	// copy: "final" the envelope recipient, "none" no field (after
	// Dovecot's lmtp_hdr_delivery_address).
	LmtpDeliveredTo           string `mapstructure:"lmtp_delivered_to" env:"LMTP_DELIVERED_TO" validate:"oneof=final none"`
	HTTPAddr                  string `mapstructure:"http_addr" env:"HTTP_ADDR"`
	ImapAddr                  string `mapstructure:"imap_addr" env:"IMAP_ADDR"`
	ImapsAddr                 string `mapstructure:"imaps_addr" env:"IMAPS_ADDR"`
	DrainSeconds              int64  `mapstructure:"drain_seconds" env:"DRAIN_SECONDS" validate:"gte=0,lte=86400"`
	AuthBackend               string `mapstructure:"auth_backend" env:"AUTH_BACKEND" validate:"oneof=static nauthilus"`
	Users                     string `mapstructure:"users" env:"USERS" secret:"true"`
	MasterUser                string `mapstructure:"master_user" env:"MASTER_USER"`
	MasterPassword            string `mapstructure:"master_password" env:"MASTER_PASSWORD" secret:"true"`
	MasterUserSeparator       string `mapstructure:"master_user_separator" env:"MASTER_USER_SEPARATOR"`
	ProxyProtocolTrustedCidrs string `mapstructure:"proxy_protocol_trusted_cidrs" env:"PROXY_PROTOCOL_TRUSTED_CIDRS"`
	LmtpAuthPasswordFile      string `mapstructure:"lmtp_auth_password_file" env:"LMTP_AUTH_PASSWORD_FILE"`
	MasterPasswordFile        string `mapstructure:"master_password_file" env:"MASTER_PASSWORD_FILE"`
}

// Rook locates the Ceph client credentials in a Rook-managed cluster.
type Rook struct {
	Namespace    string `mapstructure:"namespace" env:"ROOK_NAMESPACE"`
	ClientSecret string `mapstructure:"client_secret" env:"ROOK_CLIENT_SECRET" secret:"true"`
}

// TLS names the certificate and key for STARTTLS and implicit TLS.
type TLS struct {
	Cert string `mapstructure:"cert" env:"TLS_CERT"`
	Key  string `mapstructure:"key" env:"TLS_KEY"`
	// ReloadSeconds is how often the listeners look whether their
	// certificate files changed and load a renewed pair; 0 loads once.
	ReloadSeconds int64 `mapstructure:"reload_seconds" env:"TLS_RELOAD_SECONDS" validate:"gte=0,lte=86400"`
}

// Sieve configures Sieve settings, global scripts and ManageSieve.
type Sieve struct {
	Settings  string `mapstructure:"settings" env:"SIEVE_SETTINGS"`
	GlobalDir string `mapstructure:"global_dir" env:"SIEVE_GLOBAL_DIR"`
	// BeforeDir and AfterDir hold the scripts (<name>.sieve, run in name
	// order) that every delivery runs before and after the account's
	// active script; "" has none.
	BeforeDir string `mapstructure:"before_dir" env:"SIEVE_BEFORE_DIR"`
	AfterDir  string `mapstructure:"after_dir" env:"SIEVE_AFTER_DIR"`
	Addr      string `mapstructure:"addr" env:"SIEVE_ADDR"`
}

// SMTP configures the outbound relay for Sieve redirect and vacation.
type SMTP struct {
	Addr         string `mapstructure:"addr" env:"SMTP_RELAY_ADDR"`
	Helo         string `mapstructure:"helo" env:"SMTP_RELAY_HELO"`
	Starttls     bool   `mapstructure:"starttls" env:"SMTP_RELAY_STARTTLS"`
	TLS          bool   `mapstructure:"tls" env:"SMTP_RELAY_TLS"`
	User         string `mapstructure:"user" env:"SMTP_RELAY_USER"`
	Password     string `mapstructure:"password" env:"SMTP_RELAY_PASSWORD" secret:"true"`
	ServerName   string `mapstructure:"server_name" env:"SMTP_RELAY_SERVER_NAME"`
	PasswordFile string `mapstructure:"password_file" env:"SMTP_RELAY_PASSWORD_FILE"`
}

// Scrub configures the scrub: the "shardpost scrub" run and the batch
// scrub job over all accounts ("shardpost scrub --batch" submits it).
type Scrub struct {
	Users        string `mapstructure:"users" env:"SCRUB_USERS"`
	GraceSeconds int64  `mapstructure:"grace_seconds" env:"SCRUB_GRACE_SECONDS" validate:"gte=0,lte=86400"`
	DryRun       bool   `mapstructure:"dry_run" env:"SCRUB_DRY_RUN"`
	// Parallel is how many accounts a batch scrub works on at once in the
	// server that runs it.
	Parallel int `mapstructure:"parallel" env:"SCRUB_PARALLEL" validate:"gte=1,lte=32"`
	// IntervalDays bounds how long a batch scrub with skip_idle leaves an
	// idle account alone: an account whose last complete scrub is older
	// is scrubbed again in any case.
	IntervalDays int `mapstructure:"interval_days" env:"SCRUB_INTERVAL_DAYS" validate:"gte=1,lte=365"`
	// OrphanSource is where a scrub finds what a crash left behind:
	// "listing" lists the account's namespaces (before 0.42.0), "shadow"
	// lists and acts on the listing but also works out what the intents
	// name and reports where they differ, "intents" reads the intents
	// only and lists nothing.
	OrphanSource string `mapstructure:"orphan_source" env:"SCRUB_ORPHAN_SOURCE" validate:"oneof=listing shadow intents"`
	// IntentGraceSeconds is how old an intent must be before a scrub
	// takes it for the leftover of a crash.
	IntentGraceSeconds int64 `mapstructure:"intent_grace_seconds" env:"SCRUB_INTENT_GRACE_SECONDS" validate:"gte=600,lte=604800"`
	// SweepSpillShards is how many spill objects "shardpost scrub
	// --sweep" spreads the object names of a pool over.
	SweepSpillShards int `mapstructure:"sweep_spill_shards" env:"SCRUB_SWEEP_SPILL_SHARDS" validate:"gte=1,lte=4096"`
	// SweepClockMarginSeconds is how far before reading a namespace the
	// sweep draws the line after which nothing is a remnant: object
	// mtimes have whole seconds and come from the writer's clock.
	SweepClockMarginSeconds int64 `mapstructure:"sweep_clock_margin_seconds" env:"SCRUB_SWEEP_CLOCK_MARGIN_SECONDS" validate:"gte=1,lte=3600"`
}

// Purge configures the account purge job.
type Purge struct {
	// Listing makes a purge delete what a listing of the account's
	// namespaces finds (before 0.42.0) instead of what its data model
	// names; with it on, the purge also reports what the data model
	// missed.
	Listing bool `mapstructure:"listing" env:"PURGE_LISTING"`
}

// Nauthilus configures the Nauthilus authentication backend.
type Nauthilus struct { //nolint:dupl // a configuration section is a list of tagged fields, like the others
	URL        string `mapstructure:"url" env:"NAUTHILUS_URL"`
	User       string `mapstructure:"user" env:"NAUTHILUS_USER"`
	Password   string `mapstructure:"password" env:"NAUTHILUS_PASSWORD" secret:"true"`
	Token      string `mapstructure:"token" env:"NAUTHILUS_TOKEN" secret:"true"`
	BearerMode string `mapstructure:"bearer_mode" env:"NAUTHILUS_BEARER_MODE" validate:"oneof=password introspect"`
	MaxWait    int64  `mapstructure:"max_wait" env:"NAUTHILUS_MAX_WAIT" validate:"gte=0,lte=86400"`
	// Timeout bounds one request to Nauthilus (gRPC or HTTP), to the
	// introspection endpoint and to the token endpoint, in seconds.
	Timeout int64 `mapstructure:"timeout" env:"NAUTHILUS_TIMEOUT" validate:"gte=1,lte=300"`
	// HTTPMaxIdlePerHost is how many idle connections per host the HTTP
	// client keeps that the JSON API, the introspection and the token
	// endpoint share.
	HTTPMaxIdlePerHost int64  `mapstructure:"http_max_idle_per_host" env:"NAUTHILUS_HTTP_MAX_IDLE_PER_HOST" validate:"gte=1,lte=1024"`
	IntrospectURL      string `mapstructure:"introspect_url" env:"NAUTHILUS_INTROSPECT_URL"`
	OidcClientID       string `mapstructure:"oidc_client_id" env:"NAUTHILUS_OIDC_CLIENT_ID"`
	OidcClientSecret   string `mapstructure:"oidc_client_secret" env:"NAUTHILUS_OIDC_CLIENT_SECRET" secret:"true"`
	OidcAudience       string `mapstructure:"oidc_audience" env:"NAUTHILUS_OIDC_AUDIENCE"`
	OidcScope          string `mapstructure:"oidc_scope" env:"NAUTHILUS_OIDC_SCOPE"`
	AccountClaims      string `mapstructure:"account_claims" env:"NAUTHILUS_ACCOUNT_CLAIMS"`
	// IntrospectClientID and IntrospectClientSecret are the client the
	// introspection endpoint is asked with. Empty falls back to
	// oidc_client_id and oidc_client_secret; a separate client is what a
	// server needs whose service client may not introspect the tokens of
	// other clients.
	IntrospectClientID         string `mapstructure:"introspect_client_id" env:"NAUTHILUS_INTROSPECT_CLIENT_ID"`
	IntrospectClientSecret     string `mapstructure:"introspect_client_secret" env:"NAUTHILUS_INTROSPECT_CLIENT_SECRET" secret:"true"`
	IntrospectClientSecretFile string `mapstructure:"introspect_client_secret_file" env:"NAUTHILUS_INTROSPECT_CLIENT_SECRET_FILE"`
	OidcTokenURL               string `mapstructure:"oidc_token_url" env:"NAUTHILUS_OIDC_TOKEN_URL"`
	OidcScopes                 string `mapstructure:"oidc_scopes" env:"NAUTHILUS_OIDC_SCOPES"`
	OidcTokenAudience          string `mapstructure:"oidc_token_audience" env:"NAUTHILUS_OIDC_TOKEN_AUDIENCE"`
	OidcAuthMethod             string `mapstructure:"oidc_auth_method" env:"NAUTHILUS_OIDC_AUTH_METHOD" validate:"oneof=client_secret_post client_secret_basic"`
	Transport                  string `mapstructure:"transport" env:"NAUTHILUS_TRANSPORT" validate:"oneof=grpc http"`
	GrpcAddr                   string `mapstructure:"grpc_addr" env:"NAUTHILUS_GRPC_ADDR"`
	GrpcInsecure               bool   `mapstructure:"grpc_insecure" env:"NAUTHILUS_GRPC_INSECURE"`
	GrpcCa                     string `mapstructure:"grpc_ca" env:"NAUTHILUS_GRPC_CA"`
	GrpcServerName             string `mapstructure:"grpc_server_name" env:"NAUTHILUS_GRPC_SERVER_NAME"`
	GrpcClientCert             string `mapstructure:"grpc_client_cert" env:"NAUTHILUS_GRPC_CLIENT_CERT"`
	GrpcClientKey              string `mapstructure:"grpc_client_key" env:"NAUTHILUS_GRPC_CLIENT_KEY"`
	PasswordFile               string `mapstructure:"password_file" env:"NAUTHILUS_PASSWORD_FILE"`
	TokenFile                  string `mapstructure:"token_file" env:"NAUTHILUS_TOKEN_FILE"`
	OidcClientSecretFile       string `mapstructure:"oidc_client_secret_file" env:"NAUTHILUS_OIDC_CLIENT_SECRET_FILE"`
}

// Default protocols of the OTLP exporter and the Nauthilus client.
const (
	defaultOTLPProtocol       = "grpc"
	defaultNauthilusTransport = "grpc"
)

// productName is the default telemetry service name and cephx user.
const productName = "shardpost"

// AutocreateFolder is a mailbox the server creates for an account that
// does not have it. It is what Dovecot calls "auto": create the
// mailbox, or create and subscribe it. A mailbox is only subscribed
// when it is created, so an account that unsubscribed one keeps it
// that way.
type AutocreateFolder struct {
	Name       string `mapstructure:"name" validate:"required"`
	SpecialUse string `mapstructure:"special_use"`
	// Subscribe defaults to true; set it to false for a mailbox that
	// should exist without appearing in every client.
	Subscribe *bool `mapstructure:"subscribe"`
}

// Subscribed reports whether a newly created mailbox is subscribed.
func (f AutocreateFolder) Subscribed() bool { return f.Subscribe == nil || *f.Subscribe }

// defaultAutocreate is what an account gets without a configuration:
// the five special-use mailboxes every mail client expects, subscribed.
var defaultAutocreate = autocreateDefaults(
	"Drafts", `\Drafts`,
	"Sent", `\Sent`,
	"Trash", `\Trash`,
	"Junk", `\Junk`,
	"Archive", `\Archive`,
)

// autocreateDefaults builds the default list from name and attribute
// pairs, in the shape Viper hands to the decoder.
func autocreateDefaults(pairs ...string) []map[string]any {
	out := make([]map[string]any, 0, len(pairs)/2)
	for i := 0; i+1 < len(pairs); i += 2 {
		out = append(out, map[string]any{"name": pairs[i], "special_use": pairs[i+1]})
	}

	return out
}

// Default returns the built-in default of a configuration key ("" when
// it has none or it is no string). The bench refuses the default pool
// names whatever the configuration says.
func Default(key string) string {
	for _, d := range defaults {
		if d.key == key {
			s, _ := d.value.(string)
			return s
		}
	}

	return ""
}

// indexModeRead is the default mode of the admin store's indexes and of
// the learning queue's (read from them once their backfill marker
// exists).
const indexModeRead = "read"

// defaults are the configuration values used when neither the YAML file
// nor the environment sets a key.
var defaults = []struct {
	key   string
	value any
}{
	{"logging.level", "info"},
	{"imapsieve.enabled", false},
	{"imapsieve.user_scripts", true},
	{"imapsieve.timeout_ms", 500},
	{"imapsieve.max_parallel", 4},
	{"imapsieve.learning.max_attempts", 5},
	{"imapsieve.learning.max_message_bytes", 4 << 20},
	{"imapsieve.learning.index", indexModeRead},
	{"imapsieve.learning.lock_retry_seconds", 20},
	{"imapsieve.learning.queue_shards", 4},
	{"extprograms.bin_dir", "/usr/local/lib/shardpost/programs"},
	{"quota.grace", ""},
	{"admin.socket", ""},
	{"admin.addr", ""},
	{"admin.tls_cert", ""},
	{"admin.tls_key", ""},
	{"admin.client_ca", ""},
	{"admin.search_scan_limit", 50000},
	{"admin.kv_index", indexModeRead},
	{"admin.kv_queue_shards", 4},
	{"admin.import.lock_ttl_seconds", 900},
	{"admin.import.max_batch_bytes", 64 << 20},
	{"admin.import.max_batch_messages", 500},
	{"admin.import.max_inventory", 5000},
	{"admin.import.max_changes", 10000},
	{"admin.import.shard_size", 0},
	{"jobs.enabled", true},
	{"jobs.poll_seconds", 5},
	{"jobs.lease_seconds", 60},
	{"jobs.retention_days", 14},
	{"jobs.plan_ttl_seconds", 3600},
	{"jobs.max_parallel", 2},
	{"jobs.settle_seconds", 10},
	{"jobs.expiry_seconds", 60},
	{"jobs.batch_parallel", 2},
	{"trace.max_line_bytes", 2048},
	{"trace.max_duration_seconds", 86400},
	{"trace.default_duration_seconds", 1800},
	{"trace.recheck_seconds", 60},
	{"fts.fallback_charset", charset.DefaultFallback},
	{"fts.html", "text"},
	{"fts.imap_search", "substring"},
	{"fts.verify_parallel", 8},
	{"fts.layout", 3},
	{"fts.shards_min", 1},
	{"fts.shards_max", 128},
	{"fts.dict_shards_max", 512},
	{keyShardTargetKeys, 100000},
	{keyDictTargetKeys, 100000},
	{"fts.convert_buffer_keys", 262144},
	{"fts.convert_writers", 8},
	{"fts.auto_reshard", true},
	{"fts.frozen_wait_seconds", 10},
	{"fts.import_reshard_sync_seconds", 60},
	{"fts.dict_min_terms", 0},
	{"fts.dict_filter", "none"},
	{"fts.dict_id_min_runes", 25},
	{"fts.attached_headers", "all"},
	{"fts.online_build", OnlineBuildOff},
	{"fts.reshard_window", ""},
	{"fts.reshard_window_tz", "UTC"},
	{"fts.build_max_running", 1},
	{"fts.build_catchup_rounds", 5},
	{"fts.build_hold_ms", 2000},
	{"fts.build_ms_per_message_copy", 3.0},
	{"fts.build_ms_per_message_reindex", 20.0},
	{"fts.reshard_urgent_factor", 1.8},
	{"fts.reindex_batch_docs", 200},
	{"fts.reindex_batch_bytes", 8 << 20},
	{"limits.mailboxes", defaultLimitMailboxes},
	{"limits.mailbox_names", defaultLimitMailboxNames},
	{"limits.keywords_per_mailbox", defaultLimitKeywords},
	{"limits.metadata_entries", defaultLimitMetadataEntries},
	{"limits.shared_pointers", defaultLimitSharedPointers},
	{"quota.over_flag.enabled", false},
	{"quota.over_flag.attribute", ""},
	{"quota.over_flag.over_value", "TRUE"},
	{"quota.over_flag.program", ""},
	{"quota.over_flag.recheck_seconds", 300},
	{"telemetry.service_name", productName},
	{"telemetry.endpoint", ""},
	{"telemetry.protocol", defaultOTLPProtocol},
	{"telemetry.sample_ratio", 1.0},
	{"telemetry.user_attributes", false},
	{"server.pop3_addr", ":1110"},
	{"server.pop3s_addr", ":1995"},
	{"server.pop3_mark_seen", true},
	{"ceph.user", productName},
	{"ceph.key", ""},
	{"ceph.mon_host", ""},
	{"ceph.index_pool", "shardpost-index"},
	{"ceph.data_pool", "shardpost-data"},
	{"ceph.system_pool", ""},
	{"ceph.chunk_size", 4 << 20},
	{"server.fts_shards", 16},
	{"server.quota_bytes", 0},
	{"server.quota_msgs", 0},
	{"server.mbox_shard_size", 100000},
	{"server.account_log_retention", 10000},
	{"server.share_notifications_max", 50},
	{"server.share_notifications_retention_seconds", 30 * 24 * 3600},
	{"jmap.addr", ""},
	{"jmap.shard_attribute", ""},
	{"jmap.shard", ""},
	{"jmap.public_base_url", ""},
	{"jmap.insecure_auth", false},
	{"jmap.max_size_upload", 0},
	{"jmap.max_size_request", 0},
	{"jmap.max_concurrent_upload", 0},
	{"jmap.max_concurrent_requests", 0},
	{"jmap.max_calls_in_request", 0},
	{"jmap.max_objects_in_get", 0},
	{"jmap.max_objects_in_set", 0},
	{"jmap.max_size_blob_set", 0},
	{"jmap.max_data_sources", 0},
	{"jmap.max_size_blob_get", 0},
	{"jmap.search.snippet_read_bytes", 0},
	{"jmap.search.max_attachment_reads", 0},
	{"jmap.search.attachment_backfills", 0},
	{"jmap.search.attachment_backfill_seconds", 0},
	{"jmap.max_shared_accounts", 0},
	{"jmap.max_shared_mailboxes", 0},
	{"jmap.max_session_pointers", 0},
	{"jmap.max_principal_lookups", 0},
	{"jmap.max_principals", 0},
	{"jmap.principal_miss_per_minute", 0},
	{"jmap.push.vapid_key_file", ""},
	{"jmap.push.vapid_subject", ""},
	{"jmap.push.max_subscriptions", 0},
	{"jmap.push.max_expiry_seconds", 0},
	{"jmap.push.event_poll_seconds", 0},
	{"jmap.push.allow_internal_endpoints", false},
	{"jmap.push.check_accounts", false},
	{"jmap.push.max_shared_watches", 0},
	{"jmap.push.worker_max_watches", 0},
	{"jmap.submission.relay_addr", ""},
	{"jmap.submission.helo", ""},
	{"jmap.submission.starttls", true},
	{"jmap.submission.tls", false},
	{"jmap.submission.server_name", ""},
	{"jmap.submission.xclient", true},
	{"jmap.submission.xclient_addr", true},
	{"jmap.submission.user", ""},
	{"jmap.submission.password", ""},
	{"jmap.submission.password_file", ""},
	{"jmap.submission.timeout_seconds", 0},
	{"jmap.submission.check_sender", true},
	{"jmap.submission.address_attributes", defaultAddressAttribute},
	{"jmap.submission.max_recipients", 0},
	{"jmap.submission.max_size", 0},
	{"jmap.submission.retention_seconds", 0},
	{"jmap.submission.max_kept", 0},
	{"jmap.submission.delayed.max_delay_seconds", 0},
	{"jmap.submission.delayed.worker", true},
	{"jmap.submission.delayed.partitions", 1},
	{"jmap.submission.delayed.max_partitions", 0},
	{"jmap.submission.delayed.poll_seconds", 30},
	{"jmap.submission.delayed.sending_timeout_seconds", 3600},
	{"jmap.submission.delayed.concurrency", 4},
	{"jmap.submission.delayed.retry_attempts", 5},
	{"jmap.push.scan_seconds", 0},
	{"jmap.push.poll_seconds", 0},
	{"jmap.push.timeout_seconds", 0},
	{"jmap.push.message_ttl_seconds", 0},
	{"server.public_prefix", "Public/"},
	{"server.shared_prefix", "Shared/"},
	{"server.autocreate", defaultAutocreate},
	{"server.public_quota_bytes", 0},
	{"server.public_quota_msgs", 0},
	{"rook.namespace", "rook-ceph"},
	{"rook.client_secret", ""},
	{"server.key_poll_seconds", 60},
	{"server.memory_budget_bytes", 256 << 20},
	{"tls.cert", ""},
	{"tls.key", ""},
	{"tls.reload_seconds", 60},
	{"server.insecure_auth", false},
	{"server.max_conns_per_ip", 0},
	{"server.max_conns_per_user", 0},
	{"server.imap_max_watches", 8},
	{"server.quota_self_service", false},
	{"sieve.settings", ""},
	{"sieve.global_dir", ""},
	{"sieve.before_dir", ""},
	{"sieve.after_dir", ""},
	{"server.quota_attribute", "rnsMSQuota"},
	{"server.lmtp_addr", ":2424"},
	{"server.lmtp_hostname", ""},
	{"server.lmtp_auth_user", ""},
	{"server.lmtp_auth_password", ""},
	{"server.lmtp_return_path", true},
	{"server.lmtp_delivered_to", "final"},
	{"smtp.addr", ""},
	{"smtp.helo", ""},
	{"smtp.starttls", true},
	{"smtp.tls", false},
	{"smtp.user", ""},
	{"smtp.password", ""},
	{"smtp.server_name", ""},
	{"server.http_addr", ":8080"},
	{"server.imap_addr", ":1143"},
	{"sieve.addr", ":14190"},
	{"server.imaps_addr", ":1993"},
	{"server.drain_seconds", 20},
	{"scrub.users", ""},
	{"scrub.grace_seconds", 3600},
	{"scrub.dry_run", false},
	{"scrub.parallel", 2},
	{"scrub.interval_days", 7},
	{"scrub.orphan_source", "shadow"},
	{"scrub.intent_grace_seconds", 3600},
	{"scrub.sweep_spill_shards", 256},
	{"scrub.sweep_clock_margin_seconds", 60},
	{"purge.listing", true},
	{"jmap.upload_ttl_seconds", 86400},
	{"server.auth_backend", "static"},
	{"server.users", "test:test"},
	{"nauthilus.url", ""},
	{"nauthilus.user", ""},
	{"nauthilus.password", ""},
	{"nauthilus.token", ""},
	{"nauthilus.bearer_mode", "password"},
	{"nauthilus.max_wait", 5},
	{"nauthilus.timeout", 10},
	{"nauthilus.http_max_idle_per_host", 32},
	{"auth.cache.ttl", 30},
	{"auth.cache.negative_ttl", 0},
	{"auth.cache.max_entries", 10000},
	{"auth.cache.key_client_ip", true},
	{"auth.cache.lookup_ttl", 30},
	{"auth.cache.lookup_negative_ttl", 5},
	{"auth.cache.leader_budget", 0},
	{"jmap.limit_wait", 2},
	{"jmap.retry_after", 1},
	{"jmap.max_concurrent_per_ip", 0},
	{"nauthilus.introspect_url", ""},
	{"nauthilus.oidc_client_id", ""},
	{"nauthilus.oidc_client_secret", ""},
	{"nauthilus.oidc_audience", ""},
	{"nauthilus.oidc_scope", ""},
	{"nauthilus.account_claims", ""},
	{"nauthilus.introspect_client_id", ""},
	{"nauthilus.introspect_client_secret", ""},
	{"nauthilus.introspect_client_secret_file", ""},
	{"nauthilus.oidc_token_url", ""},
	{"nauthilus.oidc_scopes", "nauthilus:authenticate nauthilus:lookup_identity"},
	{"nauthilus.oidc_token_audience", ""},
	{"nauthilus.oidc_auth_method", "client_secret_post"},
	{"nauthilus.transport", defaultNauthilusTransport},
	{"nauthilus.grpc_addr", "nauthilus.auth.svc.cluster.local:9444"},
	{"nauthilus.grpc_insecure", false},
	{"nauthilus.grpc_ca", ""},
	{"nauthilus.grpc_server_name", ""},
	{"nauthilus.grpc_client_cert", ""},
	{"nauthilus.grpc_client_key", ""},
	{"server.master_user", ""},
	{"server.master_password", ""},
	{"server.master_user_separator", "*"},
	{"server.proxy_protocol_trusted_cidrs", ""},
	{"server.lmtp_auth_password_file", ""},
	{"smtp.password_file", ""},
	{"nauthilus.password_file", ""},
	{"nauthilus.token_file", ""},
	{"nauthilus.oidc_client_secret_file", ""},
	{"server.master_password_file", ""},
}

// Load accepts an optional YAML file. Explicit environment values override it.
// Errors name fields, never values, because configuration may contain secrets.
func Load(path string) (*Config, error) {
	v := viper.New()
	v.SetConfigType("yaml")

	for _, d := range defaults {
		v.SetDefault(d.key, d.value)
	}

	v.AllowEmptyEnv(true)

	c, err := decode(v, path)
	if err != nil {
		return nil, err
	}

	if c.Rook.ClientSecret == "" {
		c.Rook.ClientSecret = "rook-ceph-client-" + c.Ceph.User
	}

	if err := resolveSecrets(reflect.ValueOf(c).Elem()); err != nil {
		return nil, err
	}

	if err := c.checkServer(); err != nil {
		return nil, err
	}

	if err := c.checkAuth(); err != nil {
		return nil, err
	}

	if err := c.checkPrograms(); err != nil {
		return nil, err
	}

	return c, nil
}

// decode binds the environment, reads the optional file and decodes and
// validates the struct.
func decode(v *viper.Viper, path string) (*Config, error) {
	var c Config
	if err := bind(v, reflect.TypeOf(c), ""); err != nil {
		return nil, err
	}

	if path != "" {
		v.SetConfigFile(path)

		if err := v.ReadInConfig(); err != nil {
			return nil, fmt.Errorf("cannot read or parse configuration file")
		}
	}

	if err := applyEnv(v, reflect.TypeOf(c), ""); err != nil {
		return nil, err
	}

	if err := v.UnmarshalExact(&c, func(d *mapstructure.DecoderConfig) { d.WeaklyTypedInput = false; d.DecodeHook = strictTypes }); err != nil {
		return nil, fmt.Errorf("configuration contains unknown keys or invalid types")
	}

	if err := validator.New(validator.WithRequiredStructEnabled()).Struct(c); err != nil {
		if fields, ok := err.(validator.ValidationErrors); ok {
			return nil, fmt.Errorf("invalid configuration field %s (%s)", fields[0].Namespace(), fields[0].Tag())
		}

		return nil, fmt.Errorf("invalid configuration")
	}

	return &c, nil
}

// checkServer validates TLS, storage sizes, listener addresses (including
// the admin listeners) and the PROXY protocol peers; it normalizes the
// trusted CIDR list.
func (c *Config) checkServer() error {
	if (c.TLS.Cert == "") != (c.TLS.Key == "") {
		return fmt.Errorf("tls.cert and tls.key must be set together")
	}

	if !c.sizesInRange() {
		return fmt.Errorf("storage sizes and polling interval must be positive and within range")
	}

	if err := c.checkCeph(); err != nil {
		return err
	}

	for _, addr := range []string{c.Server.ImapAddr, c.Server.ImapsAddr, c.Server.Pop3Addr, c.Server.Pop3SAddr, c.Server.LmtpAddr, c.Server.HTTPAddr, c.Sieve.Addr} {
		if err := checkListener(addr); err != nil {
			return err
		}
	}

	if err := c.Admin.Validate(); err != nil {
		return err
	}

	if err := c.checkJMAP(); err != nil {
		return err
	}

	if err := c.FTS.Validate(); err != nil {
		return err
	}

	if err := c.checkNamespaces(); err != nil {
		return err
	}

	if err := c.checkAutocreate(); err != nil {
		return err
	}

	cidrs, err := normalizeTrustedCIDRs(c.Server.ProxyProtocolTrustedCidrs)
	if err != nil {
		return err
	}

	c.Server.ProxyProtocolTrustedCidrs = cidrs

	return nil
}

// checkJMAP validates the JMAP settings and what they need of others:
// delayed sending needs a relay and the admin store's queues.
func (c *Config) checkJMAP() error {
	if err := c.JMAP.Validate(c.TLS); err != nil {
		return err
	}

	if d := c.JMAP.Submission.Delayed; d.Enabled() && (!c.JMAP.Submission.Enabled() || c.Admin.KVIndex == "off") {
		return fmt.Errorf("jmap.submission.delayed.max_delay_seconds needs jmap.submission.relay_addr and admin.kv_index write or read")
	}

	return nil
}

// checkNamespaces validates the client-visible namespace prefixes. Two
// namespaces that share a name, or one that lies inside the other,
// would leave the second unreachable: a name is resolved by its prefix,
// and the first match wins. A prefix below INBOX would capture the
// account's own mailboxes.
func (c *Config) checkNamespaces() error {
	shared, public := c.Server.SharedPrefix, c.Server.PublicPrefix

	for _, p := range []string{shared, public} {
		if p == "" {
			continue
		}

		if strings.HasPrefix(strings.ToUpper(p), "INBOX/") {
			return fmt.Errorf("a namespace prefix must not lie below INBOX: %q", p)
		}

		if err := checkPrefixChars(p); err != nil {
			return err
		}
	}

	if shared == "" || public == "" {
		return nil
	}

	if shared == public || strings.HasPrefix(shared, public) || strings.HasPrefix(public, shared) {
		return fmt.Errorf("server.shared_prefix %q and server.public_prefix %q must not name the same namespace", shared, public)
	}

	return nil
}

// checkAutocreate validates the mailboxes every account gets: a usable
// name, a well-formed special-use attribute, no duplicates, and nothing
// below a namespace prefix, where no client could open it.
func (c *Config) checkAutocreate() error {
	seen := map[string]bool{}

	for _, f := range c.Server.Autocreate {
		name := strings.Trim(f.Name, " ")

		switch {
		case name == "" || strings.HasSuffix(name, "/") || strings.Contains(name, "//") || hasControl(name):
			return fmt.Errorf("server.autocreate: unusable mailbox name %q", f.Name)
		case seen[name]:
			return fmt.Errorf("server.autocreate: %q is listed twice", f.Name)
		case shadowsPrefix(name, c.Server.SharedPrefix, c.Server.PublicPrefix):
			return fmt.Errorf("server.autocreate: %q lies below a namespace prefix, where no client could open it", f.Name)
		case f.SpecialUse != "" && !validSpecialUse(f.SpecialUse):
			return fmt.Errorf("server.autocreate: %q is no special-use attribute (it starts with a backslash)", f.SpecialUse)
		}

		seen[name] = true
	}

	return nil
}

// shadowsPrefix reports whether a name lies below one of the namespace
// prefixes (empty prefixes are switched off).
func shadowsPrefix(name string, prefixes ...string) bool {
	for _, p := range prefixes {
		if p != "" && strings.HasPrefix(name, p) {
			return true
		}
	}

	return false
}

// validSpecialUse reports whether s is a special-use attribute:
// a backslash and letters, as RFC 6154 writes them.
func validSpecialUse(s string) bool {
	if len(s) < 2 || s[0] != '\\' {
		return false
	}

	for _, r := range s[1:] {
		if (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') {
			return false
		}
	}

	return true
}

// hasControl reports whether s contains a control character.
func hasControl(s string) bool {
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return true
		}
	}

	return false
}

// checkPrefixChars rejects a namespace prefix a client could not read
// back: the prefix goes into the NAMESPACE reply and into every mailbox
// name below it. IMAP quotes it, matches it against LIST patterns and
// (without UTF8=ACCEPT) expects ASCII, so quotes, backslashes, the
// wildcards, control characters, non-ASCII, an empty level and the
// delimiter alone are out.
func checkPrefixChars(p string) error {
	if p == "/" || strings.Contains(p, "//") {
		return fmt.Errorf("a namespace prefix must name a level: %q", p)
	}

	// One level, so that LIST can show the namespace where a client
	// walking the hierarchy with "%" finds it.
	if strings.Count(p, "/") != 1 {
		return fmt.Errorf("a namespace prefix must be a single level: %q", p)
	}

	for _, r := range p {
		switch {
		case r == '"' || r == '\\' || r == '*' || r == '%':
			return fmt.Errorf("a namespace prefix must not contain %q: %q", r, p)
		case r < 0x20 || r > 0x7e:
			return fmt.Errorf("a namespace prefix must be printable ASCII: %q", p)
		}
	}

	return nil
}

// sizesInRange reports whether the storage sizes and the key polling
// interval are positive and within their limits.
func (c *Config) sizesInRange() bool {
	return c.Ceph.ChunkSize > 0 && c.Ceph.ChunkSize <= 67108864 &&
		c.Server.FtsShards > 0 && c.Server.FtsShards <= 4096 &&
		c.Server.MboxShardSize > 0 && c.Server.MboxShardSize <= 4294967295 &&
		c.Server.MemoryBudgetBytes > 0 && c.Server.KeyPollSeconds > 0
}

// checkListener validates a host:port listener address.
func checkListener(addr string) error {
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("invalid listener address")
	}

	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 {
		return fmt.Errorf("invalid listener port")
	}

	return nil
}

// normalizeTrustedCIDRs validates the comma-separated PROXY protocol peers
// (CIDRs or single addresses) and returns them with whitespace trimmed.
func normalizeTrustedCIDRs(list string) (string, error) {
	cidrs := strings.Split(list, ",")
	for i, cidr := range cidrs {
		cidrs[i] = strings.TrimSpace(cidr)
		if strings.TrimSpace(cidr) == "" {
			if len(cidrs) > 1 {
				return "", fmt.Errorf("empty proxy protocol address")
			}

			continue
		}

		if _, _, err := net.ParseCIDR(strings.TrimSpace(cidr)); err != nil && net.ParseIP(strings.TrimSpace(cidr)) == nil {
			return "", fmt.Errorf("invalid proxy protocol trusted CIDR")
		}
	}

	return strings.Join(cidrs, ","), nil
}

// checkAuth validates the master user and the Nauthilus settings.
func (c *Config) checkAuth() error {
	if c.Server.MasterUser != "" && c.Server.MasterPassword == "" {
		return fmt.Errorf("master user requires a password")
	}

	if c.Nauthilus.OidcTokenURL != "" && (c.Nauthilus.OidcClientID == "" || c.Nauthilus.OidcClientSecret == "") {
		return fmt.Errorf("OIDC token URL requires client ID and secret")
	}

	if c.Server.AuthBackend == "nauthilus" && c.Nauthilus.Transport == "http" && c.Nauthilus.URL == "" {
		return fmt.Errorf("HTTP authentication requires nauthilus.url")
	}

	if err := c.Auth.Cache.Validate(); err != nil {
		return err
	}

	return c.Nauthilus.checkIntrospection()
}

// checkIntrospection refuses introspection settings that could only fail at
// runtime: a bearer mode that introspects without an endpoint, and an
// endpoint without a client that has its secret (its own, or the service
// client it falls back to).
func (n Nauthilus) checkIntrospection() error {
	if strings.EqualFold(n.BearerMode, "introspect") && n.IntrospectURL == "" {
		return fmt.Errorf("nauthilus.bearer_mode introspect requires nauthilus.introspect_url")
	}

	if n.IntrospectURL == "" {
		return nil
	}

	if n.IntrospectClientID != "" {
		if n.IntrospectClientSecret == "" {
			return fmt.Errorf("nauthilus.introspect_client_id requires nauthilus.introspect_client_secret or its file")
		}

		return nil
	}

	if n.OidcClientID == "" || n.OidcClientSecret == "" {
		return fmt.Errorf("nauthilus.introspect_url requires an introspection client or the OIDC service client")
	}

	return nil
}

// checkPrograms validates IMAPSieve, the external programs and the learning
// and quota rules that refer to them.
func (c *Config) checkPrograms() error {
	if err := c.IMAPSieve.Validate(); err != nil {
		return err
	}
	// Viper normalizes map keys to lowercase, including environment names.
	for name, p := range c.ExtPrograms.Programs {
		env := map[string]string{}
		for k, v := range p.Env {
			env[strings.ToUpper(k)] = v
		}

		p.Env = env
		c.ExtPrograms.Programs[name] = p
	}

	if err := c.ExtPrograms.Validate(); err != nil {
		return err
	}

	for _, r := range c.IMAPSieve.Learning.Rules {
		if err := c.checkLearningProgram(r); err != nil {
			return err
		}
	}

	if err := c.Quota.Validate(); err != nil {
		return err
	}

	return c.checkQuotaPrograms()
}

// checkLearningProgram verifies that a learning rule names a program that
// may be used for learning.
func (c *Config) checkLearningProgram(r LearningRule) error {
	if _, ok := c.ExtPrograms.Programs[r.Program]; !ok {
		return fmt.Errorf("learning rule %s names a program that is not in extprograms.programs", r.Name)
	}

	if !c.programAllows(r.Program, "learning") {
		return fmt.Errorf("learning rule %s program does not allow learning", r.Name)
	}

	return nil
}

func bind(v *viper.Viper, t reflect.Type, prefix string) error {
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)

		key := prefix + f.Tag.Get("mapstructure")
		if f.Type.Kind() == reflect.Struct {
			if err := bind(v, f.Type, key+"."); err != nil {
				return err
			}

			continue
		}

		name := f.Tag.Get("env")
		if name == "" {
			continue
		}

		if err := v.BindEnv(key, name); err != nil {
			return fmt.Errorf("bind %s", key)
		}

		if raw, ok := os.LookupEnv(name); ok {
			switch f.Type.Kind() {
			case reflect.Bool:
				if !isBoolLiteral(raw) {
					return fmt.Errorf("%s must be true or false", name)
				}
			case reflect.Int64:
				if _, err := strconv.ParseInt(raw, 10, 64); err != nil {
					return fmt.Errorf("%s must be an integer", name)
				}
			}
		}
	}

	return nil
}

func resolveSecrets(v reflect.Value) error {
	t := v.Type()
	for i := 0; i < v.NumField(); i++ {
		f := v.Field(i)
		sf := t.Field(i)

		if f.Kind() == reflect.Struct {
			if err := resolveSecrets(f); err != nil {
				return err
			}

			continue
		}

		if !strings.HasSuffix(sf.Name, "File") || f.String() == "" {
			continue
		}

		dst := v.FieldByName(strings.TrimSuffix(sf.Name, "File"))
		if !dst.IsValid() {
			continue
		}

		b, err := os.ReadFile(f.String())
		if err != nil {
			return fmt.Errorf("cannot read secret file for %s", sf.Tag.Get("env"))
		}

		value := strings.TrimSpace(string(b))
		if value == "" {
			return fmt.Errorf("empty secret file for %s", sf.Tag.Get("env"))
		}

		dst.SetString(value)
	}

	return nil
}

func strictTypes(from, to reflect.Type, data any) (any, error) {
	if to.Kind() == reflect.Int64 && from.Kind() != reflect.Int && from.Kind() != reflect.Int64 {
		return nil, fmt.Errorf("expected integer")
	}

	if to.Kind() == reflect.Bool && from.Kind() != reflect.Bool {
		return nil, fmt.Errorf("expected boolean")
	}

	return data, nil
}

// Literal spellings accepted for boolean environment values.
const (
	envTrue  = "true"
	envFalse = "false"
)

// isBoolLiteral reports whether raw is an accepted boolean spelling.
func isBoolLiteral(raw string) bool {
	return raw == envTrue || raw == envFalse
}

func applyEnv(v *viper.Viper, t reflect.Type, prefix string) error {
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)

		key := prefix + f.Tag.Get("mapstructure")
		if f.Type.Kind() == reflect.Struct {
			if err := applyEnv(v, f.Type, key+"."); err != nil {
				return err
			}

			continue
		}

		raw, ok := os.LookupEnv(f.Tag.Get("env"))
		if !ok {
			continue
		}

		value, err := envValue(f.Type.Kind(), raw, key)
		if err != nil {
			return err
		}

		v.Set(key, value)
	}

	return nil
}

// envValue converts an environment value to the type of its field.
func envValue(kind reflect.Kind, raw, key string) (any, error) {
	switch kind {
	case reflect.Int64:
		n, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("invalid integer for %s", key)
		}

		return n, nil
	case reflect.Float64:
		n, err := strconv.ParseFloat(raw, 64)
		if err != nil || math.IsNaN(n) || math.IsInf(n, 0) {
			return nil, fmt.Errorf("invalid number for %s", key)
		}

		return n, nil
	case reflect.Bool:
		if !isBoolLiteral(raw) {
			return nil, fmt.Errorf("invalid boolean for %s", key)
		}

		return raw == envTrue, nil
	default:
		return raw, nil
	}
}
