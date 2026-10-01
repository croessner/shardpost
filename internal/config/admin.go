package config

import (
	"fmt"
	"path"
	"path/filepath"
	"strings"
)

// Admin configures the internal admin API (roadmap stage 8). Both
// listeners are off unless configured. The Unix socket admits only
// processes running as the server's own user (peer credentials); mutual
// TLS admits clients whose certificate identity is listed in Clients.
type Admin struct {
	// Socket is the path of the Unix socket ("" disables it).
	Socket string `mapstructure:"socket" env:"ADMIN_SOCKET"`
	// SocketRoles are the roles of local callers on the socket.
	SocketRoles []string `mapstructure:"socket_roles" validate:"dive,oneof=diagnose deliver deliver:noenforce import trace provision repair delete content support export"`
	// Addr is the mutual-TLS listener ("" disables it).
	Addr string `mapstructure:"addr" env:"ADMIN_ADDR"`
	// TLSCert, TLSKey and ClientCA are files; they are re-read when they
	// change (certificate rotation without a restart).
	TLSCert  string `mapstructure:"tls_cert" env:"ADMIN_TLS_CERT"`
	TLSKey   string `mapstructure:"tls_key" env:"ADMIN_TLS_KEY"`
	ClientCA string `mapstructure:"client_ca" env:"ADMIN_CLIENT_CA"`
	// Clients map certificate identities to roles.
	Clients []AdminClient `mapstructure:"clients" validate:"dive"`
	// Import bounds the account import (role import).
	Import AdminImport `mapstructure:"import"`
	// SearchScanLimit bounds the messages one message search examines
	// (role content); a longer search continues with its cursor.
	SearchScanLimit int `mapstructure:"search_scan_limit" validate:"gte=100,lte=10000000"`
	// KVIndex is the mode of the admin store's hint indexes and due
	// queues (scaling plan B3): off writes none, write keeps them up to
	// date while readers list the namespace, read also reads from them.
	// read needs a completed "shardpost index backfill kv" that started
	// after the last server without the indexes stopped.
	KVIndex string `mapstructure:"kv_index" env:"ADMIN_KV_INDEX" validate:"oneof=off write read"`
	// KVQueueShards is the shard count of a due queue when its first
	// writer creates it; an existing queue keeps the count in its head.
	KVQueueShards int `mapstructure:"kv_queue_shards" validate:"gte=1,lte=8"`
}

// Jobs configures the durable maintenance jobs (admin API, roadmap 8c).
type Jobs struct {
	// Enabled runs the job runner in this server (jobs are shared by all).
	Enabled bool `mapstructure:"enabled"`
	// PollSeconds is how often the runner looks for queued jobs.
	PollSeconds int `mapstructure:"poll_seconds" validate:"gte=1,lte=300"`
	// LeaseSeconds is how long a claim lasts without renewal (a job of a
	// dead server is taken over after it).
	LeaseSeconds int `mapstructure:"lease_seconds" validate:"gte=10,lte=3600"`
	// RetentionDays keeps finished jobs and plans for their status.
	RetentionDays int `mapstructure:"retention_days" validate:"gte=1,lte=365"`
	// PlanTTLSeconds is how long a plan can be applied.
	PlanTTLSeconds int `mapstructure:"plan_ttl_seconds" validate:"gte=60,lte=86400"`
	// MaxParallel bounds the jobs one server runs at once.
	MaxParallel int `mapstructure:"max_parallel" validate:"gte=1,lte=16"`
	// SettleSeconds is how long an exclusive job waits after taking the
	// maintenance lock for deliveries and sessions that opened the
	// account before (sessions re-check the lock every 5 seconds).
	SettleSeconds int `mapstructure:"settle_seconds" validate:"gte=0,lte=300"`
	// ExpirySeconds is how often the expiry worker (one server of the
	// cluster, admin.kv_index write or read) deletes the jobs, plans and
	// idempotency keys whose retention ended, and how often the other
	// servers try to take its lease.
	ExpirySeconds int `mapstructure:"expiry_seconds" validate:"gte=5,lte=3600"`
	// BatchParallel is how many accounts a batch over all accounts works
	// on at once in the server that runs it (scrub batches use
	// scrub.parallel).
	BatchParallel int `mapstructure:"batch_parallel" validate:"gte=1,lte=32"`
}

// Trace configures the per-account protocol trace (role trace): an
// operator switches it on for an account and a bounded time, and new
// sessions of the account log their protocol lines (literals only by
// size).
type Trace struct {
	// MaxLineBytes is the logged length of one line.
	MaxLineBytes int `mapstructure:"max_line_bytes" validate:"gte=80,lte=65536"`
	// MaxDurationSeconds bounds how long one switch-on lasts.
	MaxDurationSeconds int `mapstructure:"max_duration_seconds" validate:"gte=60,lte=604800"`
	// DefaultDurationSeconds applies when the request names no duration.
	DefaultDurationSeconds int `mapstructure:"default_duration_seconds" validate:"gte=60,ltefield=MaxDurationSeconds"`
	// RecheckSeconds is how often a traced session re-reads the switch,
	// so that switching it off reaches running sessions.
	RecheckSeconds int `mapstructure:"recheck_seconds" validate:"gte=1,lte=3600"`
}

// AdminImport configures the account import with preserved identities.
type AdminImport struct {
	// LockTTLSeconds is how long the account's import lock lasts without
	// renewal (every import call renews it).
	LockTTLSeconds int `mapstructure:"lock_ttl_seconds" validate:"gte=60,lte=86400"`
	// MaxBatchBytes and MaxBatchMessages bound one importMessages request.
	MaxBatchBytes    int64 `mapstructure:"max_batch_bytes" validate:"gte=1048576"`
	MaxBatchMessages int   `mapstructure:"max_batch_messages" validate:"gte=1,lte=10000"`
	// MaxInventory bounds one inventory page, MaxChanges the flag
	// changes plus expunges of one changes request.
	MaxInventory int `mapstructure:"max_inventory" validate:"gte=100,lte=100000"`
	MaxChanges   int `mapstructure:"max_changes" validate:"gte=100,lte=100000"`
	// ShardSize is the index shard size of mailboxes an import creates
	// (0 = server.mbox_shard_size). Imported UIDs keep the source's gaps,
	// so a small shard size makes full reads visit many empty shards.
	ShardSize uint32 `mapstructure:"shard_size" validate:"lte=1000000"`
}

// AdminClient grants roles to the client certificate whose subject
// common name or one of whose DNS or URI SANs equals Identity.
type AdminClient struct {
	Identity string   `mapstructure:"identity" validate:"required"`
	Roles    []string `mapstructure:"roles" validate:"required,min=1,dive,oneof=diagnose deliver deliver:noenforce import trace provision repair delete content support export"`
	// Accounts limits the accounts the client may address (path.Match
	// patterns on the lower-cased canonical account, e.g. "*@example.org");
	// empty means all accounts.
	Accounts []string `mapstructure:"accounts" validate:"dive,required"`
	// AllAccounts grants the scope of all accounts: listing the account
	// directory and planning, applying and cancelling batches over every
	// account (each still needs its role). Socket callers always have it;
	// it excludes Accounts.
	AllAccounts bool `mapstructure:"all_accounts"`
}

// Validate checks the listener settings and refuses settings without
// effect (a listener nobody may use, TLS material without a listener).
func (c Admin) Validate() error {
	if err := c.validateSocket(); err != nil {
		return err
	}

	if c.Addr == "" {
		if c.TLSCert != "" || c.TLSKey != "" || c.ClientCA != "" || len(c.Clients) > 0 {
			return fmt.Errorf("admin TLS settings and clients require admin.addr")
		}

		return nil
	}

	return c.validateTLS()
}

// validateSocket checks the Unix socket settings.
func (c Admin) validateSocket() error {
	if c.Socket != "" && !filepath.IsAbs(c.Socket) {
		return fmt.Errorf("admin.socket must be an absolute path")
	}

	if (c.Socket == "") != (len(c.SocketRoles) == 0) {
		return fmt.Errorf("admin.socket and admin.socket_roles must be set together")
	}

	return nil
}

// validateTLS checks the mutual-TLS listener settings.
func (c Admin) validateTLS() error {
	if err := checkListener(c.Addr); err != nil {
		return fmt.Errorf("admin.addr: %w", err)
	}

	if c.TLSCert == "" || c.TLSKey == "" || c.ClientCA == "" || len(c.Clients) == 0 {
		return fmt.Errorf("admin.addr requires tls_cert, tls_key, client_ca and clients")
	}

	seen := map[string]bool{}

	for _, cl := range c.Clients {
		id := strings.ToLower(cl.Identity)
		if seen[id] {
			return fmt.Errorf("duplicate admin client identity")
		}

		seen[id] = true

		if cl.AllAccounts && len(cl.Accounts) > 0 {
			return fmt.Errorf("admin client %q: all_accounts and accounts exclude each other", cl.Identity)
		}

		for _, p := range cl.Accounts {
			if _, err := path.Match(p, ""); err != nil {
				return fmt.Errorf("admin client %q: invalid account pattern %q", cl.Identity, p)
			}
		}
	}

	return nil
}
