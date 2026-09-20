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
	"strconv"
	"strings"
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
	Nauthilus Nauthilus `mapstructure:"nauthilus"`
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
}

// Ceph configures the RADOS connection and pools.
type Ceph struct {
	User      string `mapstructure:"user" env:"CEPH_USER"`
	Key       string `mapstructure:"key" env:"CEPH_KEY" secret:"true"`
	MonHost   string `mapstructure:"mon_host" env:"CEPH_MON_HOST"`
	IndexPool string `mapstructure:"index_pool" env:"CEPH_INDEX_POOL"`
	DataPool  string `mapstructure:"data_pool" env:"CEPH_DATA_POOL"`
	ChunkSize int64  `mapstructure:"chunk_size" env:"CEPH_CHUNK_SIZE" validate:"gte=0"`
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
	// PublicPrefix is the client-visible prefix of the public folders.
	PublicPrefix string `mapstructure:"public_prefix" env:"PUBLIC_PREFIX" validate:"omitempty,endswith=/"`
	// SharedPrefix is the client-visible prefix of the mailboxes other
	// accounts share; "" switches that namespace off.
	SharedPrefix string `mapstructure:"shared_prefix" env:"SHARED_PREFIX" validate:"omitempty,endswith=/"`
	// PublicQuotaBytes and PublicQuotaMsgs bound the public namespace as
	// a whole (0 = unlimited). Public folders do not count against the
	// quota of the account that files into them.
	PublicQuotaBytes          int64  `mapstructure:"public_quota_bytes" env:"PUBLIC_QUOTA_BYTES" validate:"gte=0"`
	PublicQuotaMsgs           int64  `mapstructure:"public_quota_msgs" env:"PUBLIC_QUOTA_MSGS" validate:"gte=0"`
	KeyPollSeconds            int64  `mapstructure:"key_poll_seconds" env:"KEY_POLL_SECONDS" validate:"gte=0,lte=86400"`
	MemoryBudgetBytes         int64  `mapstructure:"memory_budget_bytes" env:"MEMORY_BUDGET_BYTES" validate:"gte=0"`
	InsecureAuth              bool   `mapstructure:"insecure_auth" env:"INSECURE_AUTH"`
	MaxConnsPerIP             int64  `mapstructure:"max_conns_per_ip" env:"MAX_CONNS_PER_IP" validate:"gte=0"`
	MaxConnsPerUser           int64  `mapstructure:"max_conns_per_user" env:"MAX_CONNS_PER_USER" validate:"gte=0"`
	QuotaSelfService          bool   `mapstructure:"quota_self_service" env:"QUOTA_SELF_SERVICE"`
	QuotaAttribute            string `mapstructure:"quota_attribute" env:"QUOTA_ATTRIBUTE"`
	LmtpAddr                  string `mapstructure:"lmtp_addr" env:"LMTP_ADDR"`
	LmtpHostname              string `mapstructure:"lmtp_hostname" env:"LMTP_HOSTNAME"`
	LmtpAuthUser              string `mapstructure:"lmtp_auth_user" env:"LMTP_AUTH_USER"`
	LmtpAuthPassword          string `mapstructure:"lmtp_auth_password" env:"LMTP_AUTH_PASSWORD" secret:"true"`
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
}

// Sieve configures Sieve settings, global scripts and ManageSieve.
type Sieve struct {
	Settings  string `mapstructure:"settings" env:"SIEVE_SETTINGS"`
	GlobalDir string `mapstructure:"global_dir" env:"SIEVE_GLOBAL_DIR"`
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

// Scrub configures the "shardpost scrub" repair run.
type Scrub struct {
	Users        string `mapstructure:"users" env:"SCRUB_USERS"`
	GraceSeconds int64  `mapstructure:"grace_seconds" env:"SCRUB_GRACE_SECONDS" validate:"gte=0,lte=86400"`
	DryRun       bool   `mapstructure:"dry_run" env:"SCRUB_DRY_RUN"`
}

// Nauthilus configures the Nauthilus authentication backend.
type Nauthilus struct {
	URL                  string `mapstructure:"url" env:"NAUTHILUS_URL"`
	User                 string `mapstructure:"user" env:"NAUTHILUS_USER"`
	Password             string `mapstructure:"password" env:"NAUTHILUS_PASSWORD" secret:"true"`
	Token                string `mapstructure:"token" env:"NAUTHILUS_TOKEN" secret:"true"`
	BearerMode           string `mapstructure:"bearer_mode" env:"NAUTHILUS_BEARER_MODE" validate:"oneof=password introspect"`
	MaxWait              int64  `mapstructure:"max_wait" env:"NAUTHILUS_MAX_WAIT" validate:"gte=0,lte=86400"`
	IntrospectURL        string `mapstructure:"introspect_url" env:"NAUTHILUS_INTROSPECT_URL"`
	OidcClientID         string `mapstructure:"oidc_client_id" env:"NAUTHILUS_OIDC_CLIENT_ID"`
	OidcClientSecret     string `mapstructure:"oidc_client_secret" env:"NAUTHILUS_OIDC_CLIENT_SECRET" secret:"true"`
	OidcAudience         string `mapstructure:"oidc_audience" env:"NAUTHILUS_OIDC_AUDIENCE"`
	OidcScope            string `mapstructure:"oidc_scope" env:"NAUTHILUS_OIDC_SCOPE"`
	AccountClaims        string `mapstructure:"account_claims" env:"NAUTHILUS_ACCOUNT_CLAIMS"`
	OidcTokenURL         string `mapstructure:"oidc_token_url" env:"NAUTHILUS_OIDC_TOKEN_URL"`
	OidcScopes           string `mapstructure:"oidc_scopes" env:"NAUTHILUS_OIDC_SCOPES"`
	OidcTokenAudience    string `mapstructure:"oidc_token_audience" env:"NAUTHILUS_OIDC_TOKEN_AUDIENCE"`
	OidcAuthMethod       string `mapstructure:"oidc_auth_method" env:"NAUTHILUS_OIDC_AUTH_METHOD" validate:"oneof=client_secret_post client_secret_basic"`
	Transport            string `mapstructure:"transport" env:"NAUTHILUS_TRANSPORT" validate:"oneof=grpc http"`
	GrpcAddr             string `mapstructure:"grpc_addr" env:"NAUTHILUS_GRPC_ADDR"`
	GrpcInsecure         bool   `mapstructure:"grpc_insecure" env:"NAUTHILUS_GRPC_INSECURE"`
	GrpcCa               string `mapstructure:"grpc_ca" env:"NAUTHILUS_GRPC_CA"`
	GrpcServerName       string `mapstructure:"grpc_server_name" env:"NAUTHILUS_GRPC_SERVER_NAME"`
	GrpcClientCert       string `mapstructure:"grpc_client_cert" env:"NAUTHILUS_GRPC_CLIENT_CERT"`
	GrpcClientKey        string `mapstructure:"grpc_client_key" env:"NAUTHILUS_GRPC_CLIENT_KEY"`
	PasswordFile         string `mapstructure:"password_file" env:"NAUTHILUS_PASSWORD_FILE"`
	TokenFile            string `mapstructure:"token_file" env:"NAUTHILUS_TOKEN_FILE"`
	OidcClientSecretFile string `mapstructure:"oidc_client_secret_file" env:"NAUTHILUS_OIDC_CLIENT_SECRET_FILE"`
}

// Default protocols of the OTLP exporter and the Nauthilus client.
const (
	defaultOTLPProtocol       = "grpc"
	defaultNauthilusTransport = "grpc"
)

// productName is the default telemetry service name and cephx user.
const productName = "shardpost"

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
	{"extprograms.bin_dir", "/usr/local/lib/shardpost/programs"},
	{"quota.grace", ""},
	{"admin.socket", ""},
	{"admin.addr", ""},
	{"admin.tls_cert", ""},
	{"admin.tls_key", ""},
	{"admin.client_ca", ""},
	{"admin.search_scan_limit", 50000},
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
	{"trace.max_line_bytes", 2048},
	{"trace.max_duration_seconds", 86400},
	{"trace.default_duration_seconds", 1800},
	{"trace.recheck_seconds", 60},
	{"quota.over_flag.enabled", false},
	{"quota.over_flag.attribute", ""},
	{"quota.over_flag.over_value", "TRUE"},
	{"quota.over_flag.program", ""},
	{"quota.over_flag.recheck_seconds", 300},
	{"telemetry.service_name", productName},
	{"telemetry.endpoint", ""},
	{"telemetry.protocol", defaultOTLPProtocol},
	{"telemetry.sample_ratio", 1.0},
	{"server.pop3_addr", ":1110"},
	{"server.pop3s_addr", ":1995"},
	{"server.pop3_mark_seen", true},
	{"ceph.user", productName},
	{"ceph.key", ""},
	{"ceph.mon_host", ""},
	{"ceph.index_pool", "shardpost-index"},
	{"ceph.data_pool", "shardpost-data"},
	{"ceph.chunk_size", 4 << 20},
	{"server.fts_shards", 16},
	{"server.quota_bytes", 0},
	{"server.quota_msgs", 0},
	{"server.mbox_shard_size", 100000},
	{"server.public_prefix", "Public/"},
	{"server.shared_prefix", "Shared/"},
	{"server.public_quota_bytes", 0},
	{"server.public_quota_msgs", 0},
	{"rook.namespace", "rook-ceph"},
	{"rook.client_secret", ""},
	{"server.key_poll_seconds", 60},
	{"server.memory_budget_bytes", 256 << 20},
	{"tls.cert", ""},
	{"tls.key", ""},
	{"server.insecure_auth", true},
	{"server.max_conns_per_ip", 0},
	{"server.max_conns_per_user", 0},
	{"server.quota_self_service", false},
	{"sieve.settings", ""},
	{"sieve.global_dir", ""},
	{"server.quota_attribute", "rnsMSQuota"},
	{"server.lmtp_addr", ":2424"},
	{"server.lmtp_hostname", ""},
	{"server.lmtp_auth_user", ""},
	{"server.lmtp_auth_password", ""},
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
	{"server.auth_backend", "static"},
	{"server.users", "test:test"},
	{"nauthilus.url", ""},
	{"nauthilus.user", ""},
	{"nauthilus.password", ""},
	{"nauthilus.token", ""},
	{"nauthilus.bearer_mode", "password"},
	{"nauthilus.max_wait", 5},
	{"nauthilus.introspect_url", ""},
	{"nauthilus.oidc_client_id", ""},
	{"nauthilus.oidc_client_secret", ""},
	{"nauthilus.oidc_audience", ""},
	{"nauthilus.oidc_scope", ""},
	{"nauthilus.account_claims", ""},
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

	for _, addr := range []string{c.Server.ImapAddr, c.Server.ImapsAddr, c.Server.Pop3Addr, c.Server.Pop3SAddr, c.Server.LmtpAddr, c.Server.HTTPAddr, c.Sieve.Addr} {
		if err := checkListener(addr); err != nil {
			return err
		}
	}

	if err := c.Admin.Validate(); err != nil {
		return err
	}

	cidrs, err := normalizeTrustedCIDRs(c.Server.ProxyProtocolTrustedCidrs)
	if err != nil {
		return err
	}

	c.Server.ProxyProtocolTrustedCidrs = cidrs

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
