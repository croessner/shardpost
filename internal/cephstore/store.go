// Package cephstore implements a mail store directly on top of RADOS
// (librados). No CephFS, no RGW: every message is one immutable RADOS
// object, every mailbox is one RADOS index object whose omap (the key/value
// area RocksDB keeps per object on the OSD) holds the UID -> metadata map.
//
// Design goals for very large deployments:
//
//   - Stateless front-ends: any IMAP/LMTP pod can serve any user because all
//     state lives in RADOS. Horizontal scaling is just "more replicas".
//   - No central database: the mailbox index is the omap of a single object.
//     RADOS serialises all writes to one object on its primary OSD, so we get
//     compare-and-swap semantics for free (see WriteOp.AssertVersion).
//   - Tenant isolation without extra pools: each user gets a RADOS namespace
//     inside the shared pools. Namespaces are cheap (a prefix in the object
//     key), pools are not.
//   - Metadata and payload use different pools: the index pool is replicated
//     (omap needs a replicated pool), the message pool may be erasure coded
//     to bring the storage overhead for the bulk data down to e.g. 1.5x.
package cephstore

import (
	"fmt"
	"log/slog"
	"sync"

	"github.com/ceph/go-ceph/rados"

	"shardpost/internal/model"
	"shardpost/internal/quota"
)

// Config describes how to reach the cluster.
type Config struct {
	// MonHost is the value of the librados "mon_host" option.
	MonHost string
	// User is the cephx entity without the "client." prefix, e.g. "imap".
	User string
	// Key is the base64 cephx secret for User.
	Key string
	// IndexPool holds mailbox index objects (omap). Must be replicated.
	IndexPool string
	// DataPool holds message blobs. May be erasure coded.
	DataPool string
	// ChunkSize is the blob chunk size in bytes (default 4 MiB).
	ChunkSize int64
	// FTSShards is the number of full-text index shard objects per user.
	FTSShards int
	// Quota is the per-user limit enforced on delivery (0 = unlimited).
	Quota QuotaLimit
	// PublicQuota is the limit of the public namespace, which belongs to
	// no account and therefore has no user database entry
	// (0 = unlimited). Mail filed into a public folder counts here, not
	// against the account that filed it.
	PublicQuota QuotaLimit
	// QuotaRules are the mailbox rules and the grace (nil: none).
	QuotaRules *quota.Policy
	// OnQuotaChange, if set, is called after a delivery, copy, move,
	// expunge or delete changed the usage of an account (the limit is
	// the base limit, without extras). It must not block.
	OnQuotaChange func(account string, before, after, limit Quota)
	// ShardSize is the number of UIDs per mailbox index shard.
	ShardSize uint32
	// AccountLogRetention is the number of entries the account change
	// log of an account keeps (0: DefaultAccountLogRetention). A client
	// that was away for longer than that resynchronises.
	AccountLogRetention int
}

// Store owns the librados connection and hands out per-user views. The
// connection can be replaced at runtime (cephx key rotation): new User
// views use the new connection, existing views keep the old one until they
// are closed, and the old connection is shut down once unused.
type Store struct {
	mu  sync.RWMutex
	cur *connRef
	cfg Config
	log *slog.Logger
}

// connRef is a reference-counted librados connection.
type connRef struct {
	conn *rados.Conn
	refs sync.WaitGroup
	old  bool
}

// Open connects to the Ceph cluster. The connection is shared by all
// sessions of this process; librados multiplexes requests internally.
func Open(cfg Config, log *slog.Logger) (*Store, error) {
	conn, err := connect(cfg, log)
	if err != nil {
		return nil, err
	}

	return &Store{cur: &connRef{conn: conn}, cfg: cfg, log: log}, nil
}

// Rotate replaces the cephx key (and optionally the mon list) by opening a
// new connection. Called when Rook rotates the CephClient secret.
func (s *Store) Rotate(key, monHost string) error {
	cfg := s.cfg

	cfg.Key = key
	if monHost != "" {
		cfg.MonHost = monHost
	}

	conn, err := connect(cfg, s.log)
	if err != nil {
		return err
	}

	s.mu.Lock()
	old := s.cur
	s.cur = &connRef{conn: conn}
	s.cfg = cfg
	s.mu.Unlock()

	old.old = true
	go func() {
		old.refs.Wait()
		old.conn.Shutdown()
		s.log.Info("old ceph connection closed after key rotation")
	}()

	return nil
}

func connect(cfg Config, log *slog.Logger) (*rados.Conn, error) {
	if cfg.ChunkSize <= 0 {
		cfg.ChunkSize = DefaultChunkSize
	}

	if cfg.FTSShards <= 0 {
		cfg.FTSShards = DefaultFTSShards
	}

	if cfg.ShardSize == 0 {
		cfg.ShardSize = DefaultShardSize
	}

	conn, err := rados.NewConnWithUser(cfg.User)
	if err != nil {
		return nil, fmt.Errorf("rados: new conn: %w", err)
	}

	opts := map[string]string{
		"mon_host":             cfg.MonHost,
		"key":                  cfg.Key,
		"log_to_stderr":        "false",
		"err_to_stderr":        "true",
		"log_file":             "",
		"client_mount_timeout": "15",
		"rados_mon_op_timeout": "30",
		"rados_osd_op_timeout": "60",
	}
	for k, v := range opts {
		if err := conn.SetConfigOption(k, v); err != nil {
			return nil, fmt.Errorf("rados: set %s: %w", k, err)
		}
	}

	if err := conn.Connect(); err != nil {
		return nil, fmt.Errorf("rados: connect: %w", err)
	}

	fsid, _ := conn.GetFSID()
	log.Info("connected to ceph", "fsid", fsid, "user", cfg.User,
		"indexPool", cfg.IndexPool, "dataPool", cfg.DataPool)

	return conn, nil
}

func (s *Store) ref() *connRef {
	s.mu.RLock()
	defer s.mu.RUnlock()

	s.cur.refs.Add(1)

	return s.cur
}

// Close shuts the librados connection down.
func (s *Store) Close() {
	s.mu.RLock()
	defer s.mu.RUnlock()

	s.cur.conn.Shutdown()
}

// Ping verifies that the cluster is reachable (used for readiness probes).
func (s *Store) Ping() error {
	r := s.ref()
	defer r.refs.Done()

	_, err := r.conn.GetClusterStats()

	return err
}

// User returns a view on the given user's data. The two IOContexts are
// pinned to the user's RADOS namespace, so an object id can never leak
// into another user's data by accident.
func (s *Store) User(name string) (*User, error) {
	r := s.ref()
	s.mu.RLock()
	scfg := s.cfg
	s.mu.RUnlock()

	idx, err := r.conn.OpenIOContext(scfg.IndexPool)
	if err != nil {
		r.refs.Done()
		return nil, fmt.Errorf("open index pool %q: %w", scfg.IndexPool, err)
	}

	data, err := r.conn.OpenIOContext(scfg.DataPool)
	if err != nil {
		idx.Destroy()
		r.refs.Done()

		return nil, fmt.Errorf("open data pool %q: %w", scfg.DataPool, err)
	}

	idx.SetNamespace(name)
	data.SetNamespace(name)

	cfg := scfg
	open := func(pool string) func() (*rados.IOContext, error) {
		return func() (*rados.IOContext, error) {
			c, err := r.conn.OpenIOContext(pool)
			if err != nil {
				return nil, err
			}

			c.SetNamespace(name)

			return c, nil
		}
	}

	limit := cfg.Quota
	if model.IsPublic(name) {
		limit = cfg.PublicQuota
	}

	return &User{name: name, store: s, idx: idx, data: data, log: s.log.With("user", name), release: r.refs.Done,
		idxPool: newVerPool(open(cfg.IndexPool)), dataPool: newVerPool(open(cfg.DataPool)),
		chunkSize: cfg.ChunkSize, ftsShards: cfg.FTSShards, shardSize: cfg.ShardSize, limit: limit,
		acctRetention: acctRetention(cfg.AccountLogRetention),
		rules:         cfg.QuotaRules, onQuota: cfg.OnQuotaChange}, nil
}
