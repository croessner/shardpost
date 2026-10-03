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
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ceph/go-ceph/rados"

	"shardpost/internal/learnindex"
	"shardpost/internal/model"
	"shardpost/internal/quota"
	"shardpost/internal/sortedindex"
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
	// SystemPool holds the index part of the store's own namespaces
	// (model.SystemNamespace: the admin store, the account directory,
	// the learning queue's indexes, the server metadata) instead of
	// IndexPool; "" keeps them in IndexPool (the behaviour before
	// 0.47.0). Must be replicated. A store opened with it starts only
	// on a system pool that "shardpost system init" or "shardpost
	// system migrate" prepared (placement.go).
	SystemPool string
	// PlacementTool opens the store without the placement check and its
	// periodic repetition: for the tools that read and write the fence
	// and the mark themselves (the system tools, the bench).
	PlacementTool bool
	// OnPlacementLost, if set, starts the periodic placement check of a
	// long-running process: it is called once, when a check reads a
	// placement that contradicts the configuration (a read error is no
	// contradiction). The process must stop using the store and end.
	OnPlacementLost func(error)
	// PlacementEvery is the interval of the periodic check
	// (model.ServerHeartbeat when 0).
	PlacementEvery time.Duration
	// ChunkSize is the blob chunk size in bytes (default 4 MiB).
	ChunkSize int64
	// FTSShards is the number of full-text index shard objects per user
	// (layout 2, and the target of a conversion back to it).
	FTSShards int
	// FTS are the layout settings of the full-text index (stage B14b).
	FTS FTSConfig
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
	// KVIndex is the mode of the admin store's hint indexes and queues
	// ("" = model.KVIndexWrite).
	KVIndex model.KVIndexMode
	// KVQueueShards is the shard count of a due queue its first writer
	// creates (0: model.DefaultQueueShards).
	KVQueueShards int
	// LearningIndex is the mode of the learning queue's due queue and
	// owner index ("" = model.KVIndexWrite); LearningQueueShards the
	// shard count of the due queue its first writer creates.
	LearningIndex       model.KVIndexMode
	LearningQueueShards int
	// Limits bounds what one account can grow (0 fields: unlimited).
	Limits model.Limits
	// UploadTTL is how long a JMAP upload is kept (0: DefaultUploadTTL).
	UploadTTL time.Duration
	// OrphanSource is where a scrub finds what a crash left behind
	// ("" = model.OrphanSourceShadow, see scrubintents.go).
	OrphanSource model.OrphanSource
	// IntentGrace is how old an intent must be before a scrub takes it for
	// the leftover of a crash (0: DefaultIntentGrace).
	IntentGrace time.Duration
	// PurgeListing makes a purge delete what a listing of the account's
	// namespaces finds (the behaviour before 0.42.0) instead of what the
	// data model names; see purge.go.
	PurgeListing bool
	// ClientOptions are further librados options, set on the connection
	// after the store's own: ceph.client_options of the configuration
	// (objecter throttles and op timeouts), and the bench turns the
	// objecter log on with them to count operations.
	ClientOptions map[string]string
}

// FTSConfig are the layout settings of the full-text index
// (configuration fts.layout and the keys after it).
type FTSConfig struct {
	// Layout is the layout of indexes created from now on (2 or 3; 3
	// needs the cluster mark; the zero value means 2 — the server
	// passes fts.layout, whose built-in default is 3).
	Layout int
	// ShardsMin, ShardsMax and ShardTargetKeys size the posting and
	// header objects of layout 3 generations, DictShardsMax and
	// DictTargetKeys their dictionary objects (ShardsMin all three).
	ShardsMin, ShardsMax, ShardTargetKeys int
	DictShardsMax, DictTargetKeys         int
	// ConvertBufferKeys is the least write buffer of a conversion's copy
	// (P5), ConvertWriters its writes in flight.
	ConvertBufferKeys, ConvertWriters int
	// AutoReshard queues accounts whose shards outgrew the target.
	AutoReshard bool
	// FrozenWait is how long an indexing waits, once, for a conversion
	// that froze the account's posting objects.
	FrozenWait time.Duration
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
	// fsid identifies the cluster in the account directory.
	fsid string

	// held are the worker locks Rotate gives up (holdLock); handles are
	// the long-lived views Rotate moves to the new connection
	// (handle.go).
	heldMu  sync.Mutex
	held    map[*lease]struct{}
	handles map[*userHandle]struct{}

	// learnQ is the learning queue of the process (LearningQueue).
	learnOnce sync.Once
	learnQ    *learnindex.Queue
	learnErr  error

	// uploadsQ is the due queue of the JMAP uploads (uploads.go).
	uploadsOnce sync.Once
	uploadsQ    *sortedindex.Queue
	uploadsErr  error

	// ftsDueQ is the reshard queue of the full-text index (ftslayout.go);
	// ftsDueSeen remembers what this server queued, ftsCappedSeen the
	// generations it reported at their shard cap.
	ftsDueOnce    sync.Once
	ftsDueQ       *sortedindex.Queue
	ftsDueErr     error
	ftsDueSeen    ftsDueMemo
	ftsCappedSeen ftsCappedMemo

	// place is the periodic placement check and placeState the state of
	// the last check (placement.go).
	place      placementWatch
	placeState atomic.Pointer[string]

	// serverMeta caches the server metadata (GetMetadata of "~server").
	serverMeta model.ServerMetadataCache

	// ftsMarkSeen is the layout 3 cluster mark as this server last read
	// it (ftsMarkUnknown, ftsMarkOff, ftsMarkOn): a change is logged once
	// (noteFTSLayout). ftsMarkMu serializes the read of the mark with
	// the update, so that concurrent readers cannot leave an older
	// answer in the log and the gauges (readFTSMark).
	ftsMarkMu   sync.Mutex
	ftsMarkSeen int32
}

// connRef is a reference-counted librados connection.
type connRef struct {
	conn *rados.Conn
	refs sync.WaitGroup
	old  bool
}

// Open connects to the Ceph cluster. The connection is shared by all
// sessions of this process; librados multiplexes requests internally.
//
// Unless cfg.PlacementTool is set, Open checks where the store's own
// namespaces live (placement.go) and refuses a configuration that does
// not match the cluster: a fence in the index pool without
// cfg.SystemPool, a system pool without its mark. The check reads two
// objects and, only to word a refusal, stats a few more; it never lists.
func Open(cfg Config, log *slog.Logger) (*Store, error) {
	conn, err := connect(cfg, log)
	if err != nil {
		return nil, err
	}

	fsid, _ := conn.GetFSID()

	s := &Store{cur: &connRef{conn: conn}, cfg: cfg, log: log, fsid: fsid}

	if cfg.PlacementTool {
		return s, nil
	}

	if err := s.openPlacement(); err != nil {
		conn.Shutdown()
		return nil, err
	}

	if cfg.OnPlacementLost != nil {
		s.startPlacementWatch(orDefault(cfg.PlacementEvery, model.ServerHeartbeat), cfg.OnPlacementLost)
	}

	return s, nil
}

// openPlacement is the placement check of Open. A placement read
// positively that does not match refuses. A check that could not read
// refuses a short-lived command too; a long-running process (one with
// OnPlacementLost, whose periodic check reads it again every minute)
// starts with a warning and the state "unknown" instead, so that a Ceph
// outage does not keep the servers from coming up.
func (s *Store) openPlacement() error {
	_, err := s.checkPlacement(true)

	if errors.Is(err, errPlacementRead) && s.cfg.OnPlacementLost != nil {
		s.log.Warn("system placement could not be checked at startup, starting anyway; the periodic check reads it again",
			"event", "system_placement_unknown", "err", err)

		return nil
	}

	return err
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

	// Worker locks are unlocked through the old connection while its key
	// is still valid; their holders take them again on the new one.
	s.releaseHeld()

	// Long-lived views (the admin store) move to the new connection;
	// their requests that run finish on the old one.
	s.rotateHandles()

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

	for k, v := range cfg.ClientOptions {
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

// WithConn runs fn with the store's current librados connection, which
// stays open until fn returns. It is for tools that read the pools
// beyond the data model (the bench's inventory); the store's own code
// does not use it.
func (s *Store) WithConn(fn func(*rados.Conn) error) error {
	r := s.ref()
	defer r.refs.Done()

	return fn(r.conn)
}

// Drain closes the store's own long-lived views (the queues it opens
// once per process: learning, uploads, reshards) and waits until every
// view and index the store handed out on its current connection has
// been closed and the background work of the views has finished, at
// most timeout; it reports whether that happened. It is called before
// Close, when nothing uses the store any more: shutting the connection
// down under a view's background work crashes inside librados. A queue
// the store returned before fails with ErrHandleClosed afterwards.
func (s *Store) Drain(timeout time.Duration) bool {
	s.stopPlacementWatch()
	s.closeOwnedHandles()

	s.mu.RLock()
	r := s.cur
	s.mu.RUnlock()

	done := make(chan struct{})

	go func() {
		r.refs.Wait()
		close(done)
	}()

	t := time.NewTimer(timeout)
	defer t.Stop()

	select {
	case <-done:
		return true
	case <-t.C:
		return false
	}
}

// Close ends the periodic placement check and shuts the librados
// connection down.
func (s *Store) Close() {
	s.stopPlacementWatch()

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
// into another user's data by accident. The empty name and reserved
// names other than the store's own namespaces are refused
// (model.CheckNamespace): a registry in the default namespace would
// belong to nobody. The index context is on the namespace's index pool
// (indexPoolOf: the system pool for the store's own namespaces when one
// is configured).
func (s *Store) User(name string) (*User, error) {
	return s.userIn(name, s.indexPoolOf(name))
}

// userIn is User with the index context on idxPool.
func (s *Store) userIn(name, idxPool string) (*User, error) {
	if err := model.CheckNamespace(name); err != nil {
		return nil, err
	}

	r := s.ref()
	s.mu.RLock()
	scfg := s.cfg
	s.mu.RUnlock()

	idx, err := r.conn.OpenIOContext(idxPool)
	if err != nil {
		r.refs.Done()
		return nil, fmt.Errorf("open index pool %q: %w", idxPool, err)
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
		idxPool: newVerPool(open(idxPool)), dataPool: newVerPool(open(cfg.DataPool)),
		chunkSize: cfg.ChunkSize, ftsShards: cfg.FTSShards, shardSize: cfg.ShardSize, limit: limit,
		acctRetention: acctRetention(cfg.AccountLogRetention),
		limits:        cfg.Limits,
		uploadTTL:     orDefault(cfg.UploadTTL, DefaultUploadTTL),
		rules:         cfg.QuotaRules, onQuota: cfg.OnQuotaChange,
		ftsCfg: ftsUserConfig(cfg.FTS)}, nil
}

// indexPoolOf is the pool of the index part of namespace ns: SystemPool
// for the store's own namespaces when it is set, IndexPool for every
// other namespace. The data part of a namespace is always in DataPool.
// Code that has to reach the index pool whatever the namespace (the
// sweep, the placement fence) opens IndexPool itself.
func (c Config) indexPoolOf(ns string) string {
	if c.SystemPool != "" && model.SystemNamespace(ns) {
		return c.SystemPool
	}

	return c.IndexPool
}

// indexPoolOf is Config.indexPoolOf of the current configuration.
func (s *Store) indexPoolOf(ns string) string {
	s.mu.RLock()
	defer s.mu.RUnlock()

	return s.cfg.indexPoolOf(ns)
}

// orDefault is d unless d is zero, then def.
func orDefault(d, def time.Duration) time.Duration {
	if d <= 0 {
		return def
	}

	return d
}
