package cephstore

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/ceph/go-ceph/rados"

	"shardpost/internal/mailmeta"
	"shardpost/internal/model"
	"shardpost/internal/quota"
)

// Object layout inside a user's RADOS namespace
//
//	index pool:
//	  "mailboxes"          the registry, omap:
//	                          "m:<name>"  -> MailboxRec JSON (id, uidvalidity, flags)
//	                          "q:bytes", "q:msgs"    quota usage counters
//	                          "q:lbytes", "q:lmsgs"  per-user limits (SETQUOTA)
//	  "mbox/<id>"          shard 0 of a mailbox, keyed by its stable id
//	                       (rename = registry update only), omap:
//	                          "h:uidnext", "h:uidvalidity", "h:modseq"
//	                          "h:keywords" -> JSON list of user keywords
//	                          "h:logstart" -> oldest modseq still in the change log
//	                          "c:<modseq>" -> {"u":[uids],"x":bool}   change log
//	                          "i:<id>"     -> MOVE intent (see Move)
//	                          "v:<modseq>" -> [uids] expunged (QRESYNC VANISHED)
//	                          "m:<uid>"    -> Meta JSON (entries of shard 0)
//	  "mbox/<id>/<n>"      shard n >= 1: "m:<uid>" entries only
//	  "fts/<shard>", "ftst/<shard>"   full-text index, see fts.go
//	data pool:
//	  "msg/<guid>/<n>"     chunk n of the raw RFC 5322 message, see blob.go
//
// Sharding: entry shard = (uid-1) / shardsize, so a mailbox grows into new
// objects instead of one ever-growing omap (the RGW bucket index model).
// The shard size is fixed per mailbox at creation and stored in its header
// (h:shardsize); the configured value only applies to new mailboxes, so a
// process with a different configuration can never misread an index.
// The header stays on shard 0. Writes to shard 0 are one atomic CAS;
// writes to other shards are two-phase (header first, entry second), which
// a crash can leave as a UID gap or an orphan blob, both reaped by the
// scrubber. Modseqs are allocated by the header and therefore global.
const (
	oidRegistry       = "mailboxes"
	mboxPrefix        = "mbox/"
	msgPrefix         = "msg/"
	keyUIDNext        = "h:uidnext"
	keyUIDValid       = "h:uidvalidity"
	keyModSeq         = "h:modseq"
	keyKeywords       = "h:keywords"
	keyLogStart       = "h:logstart"
	keyShardSize      = "h:shardsize"
	keyRecent         = "h:recent"
	keyIntentPrefix   = "i:"
	keyMsgPrefix      = "m:"
	keyVanPrefix      = "v:"
	keyChgPrefix      = "c:"
	regMboxPrefix     = "m:"
	regDeliveryPrefix = "d:"   // delivery intents (multi-target deliveries)
	regSubMissing     = "sub:" // subscriptions to names without a mailbox
	regLastValidity   = "uv:"  // last UIDVALIDITY used for a name
	regQuotaBytes     = "q:bytes"
	regQuotaMsgs      = "q:msgs"
	regQuotaLBytes    = "q:lbytes"
	regQuotaLMsgs     = "q:lmsgs"
	inboxName         = "INBOX"
	casRetries        = 16
	omapPageSize      = 1024
	MaxMessageLen     = 256 << 20 // test-profile limit (chunked, see blob.go)
	Delim             = model.Delim
	DefaultShardSize  = 100000
	// LogRetention bounds how many change/vanished records a mailbox keeps.
	LogRetention   = 10000
	registryTTL    = 2 * time.Second
	lockName       = "maildrop"
	DefaultLockTTL = 10 * time.Minute
)

// Shared data types live in package model (cgo-free, used by the protocol
// layer and its tests).
type (
	// MailboxRec is the registry record of a mailbox.
	MailboxRec = model.MailboxRec
	// Mailbox is a registered mailbox: its name and registry record.
	Mailbox = model.Mailbox
	// Meta is the index record of one message.
	Meta = model.Meta
	// MailboxInfo is the header state of a mailbox (UIDNEXT, modseq, ...).
	MailboxInfo = model.MailboxInfo
	// Quota is a usage or limit in bytes and messages.
	Quota = model.Quota
	// QuotaLimit is a quota limit (same representation as Quota).
	QuotaLimit = model.Quota
	// Change is one entry of a mailbox change log.
	Change = model.Change
)

// Errors returned by the store; they are the ones of package model.
var (
	ErrNoMailbox     = model.ErrNoMailbox
	ErrMailboxExists = model.ErrMailboxExists
	ErrTooLarge      = model.ErrTooLarge
	ErrOverQuota     = model.ErrOverQuota
	ErrLocked        = model.ErrLocked
	ErrNoScript      = model.ErrNoScript
	ErrScriptActive  = model.ErrScriptActive
	ErrScriptExists  = model.ErrScriptExists
	ErrMailboxLocked = model.ErrMailboxLocked
)

// HasFlag reports whether flags contains f (case-insensitive).
func HasFlag(flags []string, f string) bool { return model.HasFlag(flags, f) }

// User is a per-user view of the store (namespace-scoped IOContexts).
type User struct {
	learningHook func(model.Mailbox, model.Meta) (string, error)
	name         string
	store        *Store // to open other namespaces (shared folder pointers)
	idx          *rados.IOContext
	data         *rados.IOContext
	log          *slog.Logger
	chunkSize    int64
	ftsShards    int
	// ftsCfg are the layout settings, ftsHint the number of messages an
	// import or a rebuild is going to index (sizes a fresh layout 3
	// index; 0 unknown).
	ftsCfg  ftsUserCfg
	ftsHint int
	// lastCounts is what the last layoutCounts of the view found besides
	// the sums (FTSLayoutDue reports it).
	lastCounts struct {
		docs int64
		load float64
	}
	// ftsRS caches the layout and the trust of the index for the
	// searches of the view (ftsState); ftsHasIDs that the view has seen
	// ids in the id map, so that the index is not fresh (ensureDocIDs,
	// which posting shards may call at the same time).
	ftsMu     sync.Mutex
	ftsRS     *ftsReadState
	ftsHasIDs atomic.Bool
	// legacyDict caches whether the account still has the dictionary
	// objects of the layout before 0.35.0 (see legacyDictMatches).
	legacyDict    int
	acctRetention int
	limits        model.Limits
	uploadTTL     time.Duration
	shardSize     uint32
	limit         QuotaLimit
	rules         *quota.Policy
	recount       QuotaRecountConfig
	onQuota       func(account string, before, after, limit Quota)
	// hooks are test hooks between the steps of the online recount and
	// of the writers it captures (nil in production); recountRun is the
	// online recount running on this view (*quotaRecount, nil none).
	hooks      *recountHooks
	recountRun atomic.Value
	noEnforce  bool           // reservations skip the limit check (still counted)
	inflight   sync.WaitGroup // background work still using the IOContexts
	release    func()         // drops the reference on the connection
	idxPool    *verPool       // private contexts for versioned reads
	dataPool   *verPool

	cacheMu sync.Mutex
	cache   map[string]cachedMailbox
	// pubSeen caches the public folders whose "ps:" intent this view
	// knows to be recorded (under cacheMu).
	pubSeen map[string]bool

	// intents are finished intents waiting to be removed (intents.go).
	intents intentBatch

	// traceCtx is the context of the command currently running on this
	// view, spans the number of store spans it produced (see tracing.go).
	traceCtx atomic.Pointer[context.Context]
	spans    atomic.Int64
}

type cachedMailbox struct {
	mb  Mailbox
	exp time.Time
}

// Close releases the IOContexts once background work (asynchronous
// notifies, log trimming, watches) has finished. It never blocks the
// caller: destroying an IOContext that a notify still uses would crash
// inside librados.
func (u *User) Close() {
	u.stopIntentTimer()

	go func() {
		u.inflight.Wait()
		u.flushIntents(false)
		u.idx.Destroy()
		u.data.Destroy()
		u.idxPool.destroy()
		u.dataPool.destroy()

		if u.release != nil {
			u.release()
		}
	}()
}

// Name returns the user name.
func (u *User) Name() string { return u.name }

func mboxOID(id string) string { return mboxPrefix + id }
func (u *User) shardOID(id string, shard uint32) string {
	if shard == 0 {
		return mboxOID(id)
	}

	return fmt.Sprintf("%s%s/%d", mboxPrefix, id, shard)
}
func shardOf(uid, size uint32) uint32 {
	if size == 0 {
		size = DefaultShardSize
	}

	return (uid - 1) / size
}
func uidKey(uid uint32) string  { return fmt.Sprintf("%s%010d", keyMsgPrefix, uid) }
func vanKey(ms uint64) string   { return fmt.Sprintf("%s%020d", keyVanPrefix, ms) }
func chgKey(ms uint64) string   { return fmt.Sprintf("%s%020d", keyChgPrefix, ms) }
func regKey(name string) string { return regMboxPrefix + name }

func newGUID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}

	return hex.EncodeToString(b[:])
}

// ---------------------------------------------------------------------------
// Registry

// ListMailboxes returns all mailboxes of the user, sorted by name.
func (u *User) ListMailboxes() ([]Mailbox, error) {
	vals, err := u.idx.GetAllOmapValues(oidRegistry, "", regMboxPrefix, omapPageSize)
	if err != nil {
		if errCode(err) == syscall.ENOENT {
			return nil, nil
		}

		return nil, fmt.Errorf("list mailboxes: %w", err)
	}

	out := make([]Mailbox, 0, len(vals))
	for k, v := range vals {
		var rec MailboxRec
		if err := json.Unmarshal(v, &rec); err != nil {
			continue
		}

		out = append(out, Mailbox{Name: strings.TrimPrefix(k, regMboxPrefix), MailboxRec: rec})
	}

	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })

	return out, nil
}

var _ model.MailboxPager = (*User)(nil)

// ListMailboxesAfter implements model.MailboxPager: one bounded omap read
// of the registry, whose keys sort like the names.
func (u *User) ListMailboxesAfter(after string, limit int) ([]Mailbox, []string, error) {
	start := ""
	if after != "" {
		start = regMboxPrefix + after
	}

	vals, err := u.idx.GetOmapValues(oidRegistry, start, regMboxPrefix, int64(limit))
	if err != nil {
		if errCode(err) == syscall.ENOENT {
			return nil, nil, nil
		}

		return nil, nil, fmt.Errorf("list mailboxes: %w", err)
	}

	out := make([]Mailbox, 0, len(vals))
	scanned := make([]string, 0, len(vals))

	for k, v := range vals {
		name := strings.TrimPrefix(k, regMboxPrefix)
		scanned = append(scanned, name)

		var rec MailboxRec
		if err := json.Unmarshal(v, &rec); err != nil {
			continue
		}

		out = append(out, Mailbox{Name: name, MailboxRec: rec})
	}

	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	sort.Strings(scanned)

	return out, scanned, nil
}

// Lookup resolves a mailbox name. Results are cached briefly: the name to
// id mapping only changes on RENAME/DELETE, and a stale hit surfaces as
// ENOENT on the index object, which callers treat as "no such mailbox".
func (u *User) Lookup(name string) (*Mailbox, error) {
	name = canonical(name)

	u.cacheMu.Lock()
	if c, ok := u.cache[name]; ok && time.Now().Before(c.exp) {
		u.cacheMu.Unlock()

		mb := c.mb

		return &mb, nil
	}
	u.cacheMu.Unlock()

	vals, err := getOmapKeys(u.idx, oidRegistry, []string{regKey(name)})
	if err != nil {
		return nil, fmt.Errorf("lookup %q: %w", name, err)
	}

	v, ok := vals[regKey(name)]
	if !ok {
		return nil, ErrNoMailbox
	}

	var rec MailboxRec
	if err := json.Unmarshal(v, &rec); err != nil {
		return nil, fmt.Errorf("lookup %q: corrupt record: %w", name, err)
	}

	mb := Mailbox{Name: name, MailboxRec: rec}

	u.cacheMu.Lock()
	if u.cache == nil {
		u.cache = map[string]cachedMailbox{}
	}

	u.cache[name] = cachedMailbox{mb: mb, exp: time.Now().Add(registryTTL)}
	u.cacheMu.Unlock()

	return &mb, nil
}

func (u *User) invalidate() {
	u.cacheMu.Lock()
	u.cache = nil
	u.cacheMu.Unlock()
}

func canonical(name string) string {
	name = strings.Trim(name, Delim)
	if strings.EqualFold(name, inboxName) {
		return inboxName
	}

	if i := strings.Index(name, Delim); i == 5 && strings.EqualFold(name[:5], inboxName) {
		return inboxName + name[5:]
	}

	return name
}

// CreateMailbox creates a mailbox with the given special-use attributes.
// It is idempotent: an existing mailbox is left untouched and
// ErrMailboxExists is returned.
func (u *User) CreateMailbox(name string, specialUse []string) error {
	name = canonical(name)
	if name == "" {
		return fmt.Errorf("empty mailbox name")
	}

	rec := MailboxRec{ID: newGUID(), UIDValidity: uint32(time.Now().Unix()), SpecialUse: specialUse}
	// A recreated mailbox must not reuse the UIDVALIDITY of its
	// predecessor, even within the same second.
	known := false

	if vals, err := getOmapKeys(u.idx, oidRegistry, []string{regLastValidity + name}); err == nil {
		var v []byte
		if v, known = vals[regLastValidity+name]; known {
			if last, _ := strconv.ParseUint(string(v), 10, 32); uint32(last) >= rec.UIDValidity {
				rec.UIDValidity = uint32(last) + 1
			}
		}
	}

	// An existing mailbox is answered as before (the claim below finds
	// it), not as a limit.
	if _, err := u.Lookup(name); errors.Is(err, ErrNoMailbox) {
		if err := u.checkMailboxLimits(name, known); err != nil {
			return err
		}
	}

	acct, err := u.beginAccount(mailboxSpec(rec.ID, name))
	if err != nil {
		return err
	}

	// 1. The index object under its fresh id (nobody can race for it),
	//    announced by its intent first: an object the name never comes to
	//    point at is found through the intent, not by listing the pool.
	if err := u.createIntended(&rec, name, u.shardSize); err != nil {
		acct.withdraw()

		return fmt.Errorf("create mailbox %q: %w", name, err)
	}

	// 2. Claim the name in the registry (which drops the intent).
	//    Concurrent creators on different pods race here; exactly one
	//    wins, the others see the name taken.
	exists, err := u.claimName(name, &rec)
	if err != nil || exists {
		acct.settle(err, false, rec.ID)

		u.abandonCreated(rec.ID)

		if err != nil {
			return fmt.Errorf("register mailbox %q: %w", name, err)
		}

		return ErrMailboxExists
	}

	acct.commit(model.AccountChange{Op: model.OpCreated})
	u.invalidate()

	return nil
}

// claimName registers rec under name unless the name is taken; exists
// reports that it was. A subscription made before the mailbox existed
// carries over into the record.
func (u *User) claimName(name string, rec *MailboxRec) (exists bool, err error) {
	b, _ := json.Marshal(rec)
	locked := false

	defer func() {
		if err == nil && locked {
			err = ErrMailboxLocked
		}
	}()

	keys := append([]string{regKey(name), regSubMissing + name}, ancestorKeys(name)...)

	err = casOmap(u, u.idx, oidRegistry, keys, func(cur map[string][]byte) (map[string][]byte, []string) {
		if _, ok := cur[regKey(name)]; ok {
			exists = true

			return nil, nil
		}

		// Nothing is created under a mailbox a mailbox_delete locks (it
		// would be a child the plan does not know): the ancestors are
		// read in the CAS that claims the name.
		if lockedAncestor(cur, name, time.Now()) {
			locked = true

			return nil, nil
		}

		exists = false
		set := map[string][]byte{regKey(name): b, regLastValidity + name: []byte(strconv.FormatUint(uint64(rec.UIDValidity), 10))}

		rm := []string{createKey(rec.ID)}

		if _, ok := cur[regSubMissing+name]; ok {
			rec.Subscribed = true
			b, _ = json.Marshal(rec)
			set[regKey(name)] = b
			rm = append(rm, regSubMissing+name)
		}

		return set, rm
	})

	return exists, err
}

// createIntended records the creation intent ("ci:") of a new mailbox
// and creates its index object with the shard size; it fails if the
// object exists (the intent is then dropped again).
func (u *User) createIntended(rec *MailboxRec, name string, shardSize uint32) error {
	// An id whose deletion is still under way (a live tombstone, stage
	// B17 M7) is not reused: the deletion would take the new mailbox's
	// shards with it. The import then chooses a fresh id.
	deleting := false
	done := u.takeIntents()

	err := casOmap(u, u.idx, oidRegistry, []string{deleteKey(rec.ID)}, func(cur map[string][]byte) (map[string][]byte, []string) {
		if v, ok := cur[deleteKey(rec.ID)]; ok && intentTime(v).After(time.Now().Add(-u.intentGrace())) {
			deleting = true
			return nil, nil
		}

		return map[string][]byte{createKey(rec.ID): encode(mailboxIntent{Time: time.Now().Unix(), Name: name})}, done
	})
	if err != nil || deleting {
		u.restoreIntents(done)
	}

	if err != nil {
		return fmt.Errorf("record intent: %w", err)
	}

	if deleting {
		return errIDDeleting
	}

	// On an error the intent stays: an object of that id may exist (a
	// creation by someone else, or this one if the error came after the
	// write), and the intent may be another creator's of the same id.
	// The scrubber settles it.
	return u.createIndexObjectSized(rec, shardSize)
}

// abandonCreated deletes the index object of a mailbox whose name was
// not registered, then its creation intent.
func (u *User) abandonCreated(id string) {
	if err := u.idx.Delete(mboxOID(id)); err != nil && errCode(err) != syscall.ENOENT {
		u.log.Warn("abandoned mailbox object not deleted", "id", id, "err", err)
		return // the intent stays: the scrubber deletes the object
	}

	u.dropCreateIntent(id)
}

// dropCreateIntent removes the creation intent of a mailbox object.
func (u *User) dropCreateIntent(id string) {
	if err := rmKeys(u.idx, oidRegistry, []string{createKey(id)}); err != nil {
		u.log.Warn("creation intent not removed", "id", id, "err", err)
	}
}

// createIndexObjectSized is createIndexObject with the given shard size.
func (u *User) createIndexObjectSized(rec *MailboxRec, shardSize uint32) error {
	op := rados.CreateWriteOp()
	op.Create(rados.CreateExclusive)
	op.SetOmap(map[string][]byte{
		keyUIDNext:   []byte("1"),
		keyUIDValid:  []byte(strconv.FormatUint(uint64(rec.UIDValidity), 10)),
		keyModSeq:    []byte("1"),
		keyKeywords:  []byte("[]"),
		keyLogStart:  []byte("1"),
		keyShardSize: []byte(strconv.FormatUint(uint64(shardSize), 10)),
	})
	err := op.Operate(u.idx, mboxOID(rec.ID), rados.OperationNoFlag)
	op.Release()

	return err
}

// EnsureMailbox creates the mailbox if it does not exist yet. The
// (cached) lookup comes first: every LMTP delivery calls this, and
// CreateMailbox would otherwise write and delete an index object each
// time, leaving one behind whenever the process dies in between.
func (u *User) EnsureMailbox(name string, specialUse []string) error {
	if _, err := u.Lookup(name); err == nil {
		return nil
	} else if !errors.Is(err, ErrNoMailbox) {
		return err
	}

	err := u.CreateMailbox(name, specialUse)
	if errors.Is(err, ErrMailboxExists) {
		return nil
	}

	return err
}

// DeleteMailbox removes the mailbox, its index shards and all its messages.
// Child mailboxes are kept (their parent becomes an implicit \Noselect
// node), as RFC 9051 requires.
//
// The registry entry goes first, replaced by a tombstone ("dm:", see
// intents.go) in the same CAS; then the header of shard 0 is retired, so
// that no delivery that looked the mailbox up before can allocate a UID
// in it any more; then the blobs are released and the shards deleted top
// down, shard 0 last; the tombstone goes at the end. A crash on the way
// leaves the tombstone, from which the scrubber completes the deletion
// without listing the pools. So does a delivery whose entry for a higher
// shard was still on its way when the header was retired: the tombstone
// is then kept for the scrubber.
func (u *User) DeleteMailbox(name string) error {
	mb, err := u.Lookup(name)
	if err != nil {
		return err
	}

	lease, err := u.lockLearning(mb.ID)
	if err != nil {
		return err
	}
	defer lease.Release()

	u.invalidate()

	current, err := u.Lookup(name)
	if err != nil || current.ID != mb.ID {
		return ErrNoMailbox
	}

	info, metas, err := u.readForDelete(mb)
	if err != nil {
		return err
	}

	if lease.Lost() {
		return ErrLocked
	}

	if err := u.preserveLearningReceipts(mb); err != nil {
		return err
	}

	if lease.Lost() {
		return ErrLocked
	}
	// Unregister first: from now on nobody can deliver into it. The CAS
	// checks that the registry still names this mailbox (same id): of two
	// concurrent DELETEs exactly one wins and releases the blobs, and a
	// mailbox re-created under the name meanwhile is left alone. The
	// subscription and the ACL come from that winning snapshot.
	snap, lost, err := u.unregisterLogged(mb, lease, metas, info)
	if err != nil {
		return fmt.Errorf("unregister mailbox %q: %w", name, err)
	}

	u.invalidate()

	if lost {
		return ErrNoMailbox
	}

	u.dropSharedRefs(snap.ACL, mb.Name)

	_, err = u.destroyUnregistered(mb, info, metas, name)

	return err
}

// destroyUnregistered is the part of DeleteMailbox after the registry
// entry was replaced by the tombstone: retire the header, release the
// blobs, delete the shards, drop the tombstone. metas (read before the
// unregistration) only serve the account log, which recorded them; what
// is released is what the shards hold once the header is retired, since
// another session may have removed (and released) an entry meanwhile.
//
// The usage of the entries is released in any case: the mailbox is no
// longer registered, so its entries count nowhere. It is released
// directly after the entries are read, before the blob references go
// (stage B16, temp/16 §4.3), and the tombstone says with q whether that
// happened. Whatever is left undone stays with the tombstone, from which
// the scrubber completes the deletion (it recounts the blobs, it never
// releases twice).
func (u *User) destroyUnregistered(mb *Mailbox, info *MailboxInfo, metas []Meta, name string) (Quota, error) {
	final, inFlight, err := u.retireDeleted(mb, info)
	if err != nil {
		u.log.Warn("delete: header not retired, left to the scrubber", "mailbox", name, "err", err)

		return sizeOf(metas), u.releaseDeleted(mb, metas)
	}

	if final == nil {
		// No shard 0 and no header read before. Higher shards that are
		// left keep the tombstone, which names them for the scrubber
		// (it probes above the range), and its q stays 0: their usage
		// was not released, and an online recount keeps counting what
		// they hold. Otherwise nothing is left.
		if u.lastShard(mb.ID, nil) != 0 {
			return Quota{}, nil
		}

		err := u.releaseDeleted(mb, nil)

		u.dropTombstone(mb, name)

		return Quota{}, err
	}

	entries, foreign, err := u.entriesToRelease(mb.ID, final)
	if err != nil {
		u.log.Warn("delete: entries not read, left to the scrubber", "mailbox", name, "err", err)
		return Quota{}, nil
	}

	q := sizeOf(entries)

	// A removal of another session whose release is still to come (its
	// r: key on shard 0) or a delivery whose entry is still on its way:
	// the tombstone (with the range the header had when it was retired)
	// and the shards stay for the scrubber, which counts what they hold
	// once the grace period is over.
	if inFlight || foreign {
		u.keepTombstone(mb, final, name)
	}

	rerr := u.releaseDeleted(mb, entries)

	_, failed := u.releaseMessages(entries)
	if failed > 0 || foreign || inFlight {
		// The shards are the only record of which blobs still hold a
		// reference too many: they stay with the tombstone.
		u.log.Warn("delete: shards left to the scrubber", "mailbox", name, "failed", failed, "foreign_removals", foreign,
			"in_flight", inFlight)

		return q, rerr
	}

	if err := u.deleteShardRange(mb.ID, u.lastShard(mb.ID, final)); err != nil {
		return q, errors.Join(err, rerr)
	}

	u.dropTombstone(mb, name)

	return q, rerr
}

// dropTombstone removes the tombstone of a finished deletion.
func (u *User) dropTombstone(mb *Mailbox, name string) {
	if err := rmKeys(u.idx, oidRegistry, []string{deleteKey(mb.ID)}); err != nil {
		u.log.Warn("delete: tombstone not removed", "mailbox", name, "err", err)
	}
}

// keepTombstone rewrites the tombstone with the range of the retired
// header and a fresh time, for the scrubber. Whether the usage is
// released (q) is carried over from the tombstone as it is: a tombstone
// that lost its q = 1 would be listed by every online recount as a
// release still to come.
func (u *User) keepTombstone(mb *Mailbox, final *MailboxInfo, name string) {
	done := u.takeIntents()

	err := casOmap(u, u.idx, oidRegistry, []string{deleteKey(mb.ID)}, func(cur map[string][]byte) (map[string][]byte, []string) {
		var old mailboxIntent

		_ = json.Unmarshal(cur[deleteKey(mb.ID)], &old)

		return map[string][]byte{deleteKey(mb.ID): deletionTombstone(mb, final, old.Released)}, done
	})
	if err != nil {
		u.restoreIntents(done)
		u.log.Warn("delete: tombstone not updated", "mailbox", name, "err", err)
	}
}

// entriesToRelease reads the entries a deleted mailbox holds after its
// header was retired (no session can remove one any more, unless its
// removal already started): every shard of the range and above it. An
// entry whose removal intent (r:) is on shard 0 belongs to the session
// that removes it and is left out; foreign reports that there was one.
func (u *User) entriesToRelease(id string, final *MailboxInfo) ([]Meta, bool, error) {
	if final == nil {
		return nil, false, nil
	}

	removals, err := u.removeIntents(id)
	if err != nil {
		return nil, false, err
	}

	theirs := make(map[uint32]bool, len(removals))
	for k := range removals {
		theirs[uidOfKey(strings.Replace(k, keyRemovePrefix, keyMsgPrefix, 1))] = true
	}

	var out []Meta

	for s := uint32(0); s <= u.lastShard(id, final); s++ {
		vals, err := u.idx.GetAllOmapValues(u.shardOID(id, s), "", keyMsgPrefix, omapPageSize)
		if err != nil && errCode(err) != syscall.ENOENT {
			return nil, false, err
		}

		for _, v := range vals {
			var m Meta
			if json.Unmarshal(v, &m) == nil && !theirs[m.UID] {
				out = append(out, m)
			}
		}
	}

	return out, len(removals) > 0, nil
}

// retireDeleted retires the header of an unregistered mailbox (UIDNEXT 0,
// the old value kept under h:retired) and returns the header as it was
// then and whether a delivery may still write an entry to a higher shard
// (a pending change younger than pendingMaxAge). A missing shard 0
// returns info unchanged.
func (u *User) retireDeleted(mb *Mailbox, info *MailboxInfo) (*MailboxInfo, bool, error) {
	var final *MailboxInfo

	err := casOmap(u, u.idx, mboxOID(mb.ID), []string{keyUIDNext, keyRetired, keyShardSize, keyModSeq},
		func(cur map[string][]byte) (map[string][]byte, []string) {
			final = nil

			if _, ok := cur[keyUIDNext]; !ok {
				return nil, nil // no shard 0: nothing to retire
			}

			n := max(retiredUIDNext(cur), 1)
			ss, _ := strconv.ParseUint(string(cur[keyShardSize]), 10, 32)
			ms, _ := strconv.ParseUint(string(cur[keyModSeq]), 10, 64)
			final = &MailboxInfo{Mailbox: *mb, UIDNext: n, ShardSize: uint32(ss), HighestModSeq: ms}

			if final.ShardSize == 0 {
				final.ShardSize = DefaultShardSize
			}

			return map[string][]byte{keyUIDNext: []byte("0"), keyRetired: []byte(strconv.FormatUint(uint64(n), 10))}, nil
		})
	if err != nil || final == nil {
		return info, false, err
	}

	inFlight := false

	if shardOf(max(final.UIDNext-1, 1), final.ShardSize) > 0 {
		_, pending, err := u.PendingFloor(mb.ID, final.HighestModSeq)
		inFlight = pending || err != nil
	}

	return final, inFlight, nil
}

// unregisterLogged unregisters a mailbox and records both the mailbox
// and its messages in the account change log. The message list was read
// before the mailbox was closed for writing, so a delivery may have
// squeezed in behind it. The entry is a delta nonetheless: a message that
// arrived after the list was read has an entry of its own that says it
// was created there, and a reader that finds the mailbox gone counts it
// as having come and gone. Marking the entry Partial instead would make
// every reader resynchronise after the deletion of a mailbox, which is
// an ordinary thing for a client to do — most often of an empty one.
func (u *User) unregisterLogged(mb *Mailbox, lease model.Lease, metas []Meta, info *MailboxInfo) (MailboxRec, bool, error) {
	return u.unregisterLoggedAs(mb, lease, metas, info, "")
}

// unregisterLoggedAs is unregisterLogged for the mailbox lock session
// ("" none: a locked mailbox is refused).
func (u *User) unregisterLoggedAs(mb *Mailbox, lease model.Lease, metas []Meta, info *MailboxInfo, session string) (MailboxRec, bool, error) {
	acct, err := u.beginAccount(emailSpec(mb.ID), mailboxSpec(mb.ID, mb.Name))
	if err != nil {
		return MailboxRec{}, false, err
	}

	snap, lost, err := u.unregisterMailbox(mb, lease, info, session)

	switch {
	case err != nil:
		acct.partial()
	case lost:
		acct.withdraw()
	default:
		acct.commit(emailChange(model.OpDestroyed, 0, metas), model.AccountChange{Op: model.OpDestroyed})
	}

	return snap, lost, err
}

// readForDelete reads the header and the index entries of a mailbox that
// is about to be deleted. A mailbox that is already gone is no error.
func (u *User) readForDelete(mb *Mailbox) (*MailboxInfo, []Meta, error) {
	info, _, err := u.readHeader(mb)
	if err != nil && !errors.Is(err, ErrNoMailbox) {
		return nil, nil, err
	}

	metas, err := u.ListMessages(mb.ID)
	if err != nil && !errors.Is(err, ErrNoMailbox) {
		return nil, nil, err
	}

	return info, metas, nil
}

// unregisterMailbox removes the registry entry of mb if it still names
// this mailbox (same id) and the lease is held, and records the deletion
// tombstone with the range of info in the same CAS. It returns the record
// it removed and lost=true if it did not remove it. With a mailbox lock
// session the record must carry that session's lock (a record a server
// before 0.52.0 rewrote lost it: the deletion is then not done, fail
// safe), and the session's overview record moves to the phase
// unregistering; without one a locked record is refused.
func (u *User) unregisterMailbox(mb *Mailbox, lease model.Lease, info *MailboxInfo, session string) (MailboxRec, bool, error) {
	var snap MailboxRec

	lost, locked := false, false
	tomb := map[string][]byte{deleteKey(mb.ID): deletionTombstone(mb, info, nil)}

	keys := []string{regKey(mb.Name)}
	if session != "" {
		keys = append(keys, mailboxLockKey(session))
	}

	err := casOmap(u, u.idx, oidRegistry, keys, func(cur map[string][]byte) (map[string][]byte, []string) {
		lost, locked = true, false

		if lease.Lost() {
			return nil, nil
		}

		v, ok := cur[regKey(mb.Name)]
		if !ok || json.Unmarshal(v, &snap) != nil || snap.ID != mb.ID {
			return nil, nil
		}

		if locked = !unregisterAllowed(&snap, session); locked {
			return nil, nil
		}

		lost = false

		set := maps.Clone(tomb)
		maps.Copy(set, unregisterPhase(cur, session))

		if snap.Subscribed { // the subscription survives as one to a missing name
			set[regSubMissing+mb.Name] = []byte("1")
		}

		return set, []string{regKey(mb.Name)}
	})

	if err == nil && locked {
		if session == "" {
			return snap, true, ErrMailboxLocked
		}

		return snap, true, fmt.Errorf("%w: the mailbox lock of %s is gone", model.ErrLocked, session)
	}

	return snap, lost, err
}

// unregisterAllowed reports whether the record may be unregistered:
// without a mailbox lock session the record must not be locked by one;
// with one it must carry exactly that session's lock (a record a server
// before 0.52.0 rewrote lost it: fail safe, nothing is deleted).
func unregisterAllowed(rec *MailboxRec, session string) bool {
	if session == "" {
		return !rec.Lock.Held("", time.Now())
	}

	return rec.Lock != nil && rec.Lock.Session == session
}

// unregisterPhase moves the session's lock record to the phase
// unregistering (nothing without a session or a record).
func unregisterPhase(cur map[string][]byte, session string) map[string][]byte {
	if session == "" {
		return nil
	}

	var r mailboxLockRec
	if json.Unmarshal(cur[mailboxLockKey(session)], &r) != nil || r.Phase == "" {
		return nil
	}

	r.Phase = model.MailboxLockUnregistering

	return map[string][]byte{mailboxLockKey(session): encode(r)}
}

// deletionTombstone is the "dm:" value of a mailbox about to be deleted;
// released says whether its usage was released (nil: not yet, written
// as 0).
func deletionTombstone(mb *Mailbox, info *MailboxInfo, released *int) []byte {
	t := mailboxIntent{Time: time.Now().Unix(), Name: mb.Name, Released: released}
	if info != nil {
		t.UIDNext, t.ShardSize = info.UIDNext, info.ShardSize
	}

	if t.Released == nil {
		zero := 0
		t.Released = &zero
	}

	return encode(t)
}

// releaseMessages drops the blob references of the entries of a deleted
// mailbox and returns the (negative) quota delta and how many releases
// failed.
func (u *User) releaseMessages(metas []Meta) (Quota, int) {
	var q Quota

	failed := 0

	for i := range metas {
		if err := u.releaseBlob(&metas[i]); err != nil {
			u.log.Warn("orphan blob", "guid", metas[i].GUID, "err", err)

			failed++
		}

		q.Bytes -= metas[i].Size
		q.Msgs--
	}

	return q, failed
}

// deleteShards deletes the index objects of a deleted mailbox from the
// highest shard down, shard 0 (with the header that gives the range)
// last; name is the name used in the error.
func (u *User) deleteShards(mb *Mailbox, info *MailboxInfo, name string) error {
	if info == nil {
		return nil
	}

	for s := int64(shardOf(max(info.UIDNext-1, 1), info.ShardSize)); s >= 0; s-- {
		if err := u.idx.Delete(u.shardOID(mb.ID, uint32(s))); err != nil && errCode(err) != syscall.ENOENT {
			return fmt.Errorf("delete index %q shard %d: %w", name, s, err)
		}
	}

	return nil
}

// RenameMailbox renames a mailbox and all its children in one registry
// write. Renaming INBOX moves its messages into a new mailbox and leaves an
// empty INBOX behind (RFC 9051 6.3.6).
func (u *User) RenameMailbox(oldName, newName string) error {
	oldName, newName = canonical(oldName), canonical(newName)

	defer u.invalidate()

	if oldName == inboxName {
		return u.renameInbox(newName)
	}

	all, err := u.ListMailboxes()
	if err != nil {
		return err
	}
	// The old name may be an implicit \Noselect parent that exists only
	// through its children; renaming it renames the subtree.
	keys, subtree := renameKeys(all, oldName, newName)

	if !subtree {
		return ErrNoMailbox
	}

	keys = append(keys, regKey(newName))

	var conflict error

	acct, err := u.beginAccount(renameSpecs(all, oldName, newName)...)
	if err != nil {
		return err
	}

	// A rename across the quota rules books the moved sum after the
	// rename CAS (recountRenamed), not atomically with it. The rename CAS
	// records an intent for that booking, so that an online recount
	// started between the two waits for it (stage B16, temp/16 §4.2).
	intentKey := ""
	if u.renameFlips(all, oldName, newName) {
		intentKey = quotaIntentKey(newGUID())
	}

	err = casOmap(u, u.idx, oidRegistry, keys, u.renameUpdate(oldName, newName, intentKey, &conflict))
	if err != nil {
		acct.partial()
		return fmt.Errorf("rename %q: %w", oldName, err)
	}

	if conflict != nil {
		acct.withdraw()
	} else {
		acct.commit()
	}

	if conflict == nil {
		// Grantees keep their pointers under the new names.
		u.moveSubtreeSharedRefs(all, oldName, newName)
		u.recountRenamed(all, oldName, newName, intentKey)
	}

	return conflict
}

// renameFlips reports whether the rename moves a mailbox of the subtree
// in or out of the usage count.
func (u *User) renameFlips(all []Mailbox, oldName, newName string) bool {
	if !u.rules.RenameMayRecount() {
		return false
	}

	for i := range all {
		m := &all[i]
		if m.Name != oldName && !strings.HasPrefix(m.Name, oldName+Delim) {
			continue
		}

		renamed := Mailbox{Name: newName + m.Name[len(oldName):], MailboxRec: m.MailboxRec}
		if u.rules.Counts(m) != u.rules.Counts(&renamed) {
			return true
		}
	}

	return false
}

// recountRenamed moves the messages of renamed mailboxes that a name
// rule now ignores (or no longer ignores) out of (into) the usage. The
// rename is not refused for it, like a lowered limit; a failure here is
// left to the scrubber, which recounts from the indexes. The booking
// removes the rename intent of the rename CAS and ends an online
// recount that may be under way: the sum is one a run cannot see.
func (u *User) recountRenamed(all []Mailbox, oldName, newName string, intentKey string) {
	if intentKey == "" {
		return
	}

	var d Quota

	for i := range all {
		m := &all[i]
		if m.Name != oldName && !strings.HasPrefix(m.Name, oldName+Delim) {
			continue
		}

		renamed := Mailbox{Name: newName + m.Name[len(oldName):], MailboxRec: m.MailboxRec}

		was, is := u.rules.Counts(m), u.rules.Counts(&renamed)
		if was == is {
			continue
		}

		metas, err := u.ListMessages(m.ID)
		if err != nil {
			u.log.Warn("quota recount after rename failed", "mailbox", renamed.Name, "err", err)
			continue
		}

		q := sizeOf(metas)
		if was {
			q = Quota{Bytes: -q.Bytes, Msgs: -q.Msgs}
		}

		d.Bytes += q.Bytes
		d.Msgs += q.Msgs
	}

	if err := u.applyUsage(usageChange{d: d, notify: true, rm: []string{intentKey}, abort: true}); err != nil {
		u.log.Warn("quota recount after rename failed", "err", err)
	}
}

// renameInbox moves the messages of INBOX into the new mailbox newName;
// INBOX itself stays.
func (u *User) renameInbox(newName string) error {
	if err := u.CreateMailbox(newName, nil); err != nil {
		return err
	}

	metas, err := u.ListMessagesByName(inboxName)
	if err != nil {
		return err
	}

	_, err = u.Move(inboxName, metas, newName)

	return err
}

// renameKeys returns the registry keys of the mailboxes in the subtrees
// of oldName and newName, and whether the oldName subtree is non-empty.
func renameKeys(all []Mailbox, oldName, newName string) ([]string, bool) {
	subtree := false

	var keys []string

	for _, m := range all {
		if m.Name == oldName || strings.HasPrefix(m.Name, oldName+Delim) {
			subtree = true

			keys = append(keys, regKey(m.Name))
		}

		if m.Name == newName || strings.HasPrefix(m.Name, newName+Delim) {
			keys = append(keys, regKey(m.Name))
		}
	}

	return keys, subtree
}

// renameSpecs announces one mailbox change per mailbox the rename
// touches, under the name it will carry afterwards. A rename changes
// every child of the subtree as well, and a client that tracks mailboxes
// has to learn about each of them.
func renameSpecs(all []Mailbox, oldName, newName string) []acctSpec {
	var specs []acctSpec

	for i := range all {
		m := &all[i]
		if m.Name != oldName && !strings.HasPrefix(m.Name, oldName+Delim) {
			continue
		}

		specs = append(specs, mailboxSpec(m.ID, newName+m.Name[len(oldName):]))
	}

	return specs
}

// renameUpdate returns the registry CAS function of a rename of the
// oldName subtree to newName. *conflict receives why the rename was not
// done (nil if it was).
func (u *User) renameUpdate(oldName, newName, intentKey string, conflict *error) func(map[string][]byte) (map[string][]byte, []string) {
	return func(_ map[string][]byte) (map[string][]byte, []string) {
		*conflict = nil
		// Collision check against the live registry: any create after
		// this listing bumps the object version and fails the CAS.
		all, err := u.idx.GetAllOmapValues(oidRegistry, "", regMboxPrefix, omapPageSize)
		if err != nil {
			*conflict = err
			return nil, nil
		}

		now := time.Now()
		// A mailbox a mailbox_delete locks (stage B17) is neither renamed
		// nor moved under; the lock is read in the CAS that rewrites the
		// names, so it cannot be missed.
		if lockedAncestor(all, newName, now) {
			*conflict = ErrMailboxLocked
			return nil, nil
		}

		set := map[string][]byte{}

		var rm []string

		for k, v := range all {
			n := strings.TrimPrefix(k, regMboxPrefix)
			if n == oldName || strings.HasPrefix(n, oldName+Delim) {
				if lockedRecord(v, "", now) {
					*conflict = ErrMailboxLocked
					return nil, nil
				}

				target := regKey(newName + n[len(oldName):])
				// Every resulting name must be free: "X" may be an implicit
				// parent while "X/B" already exists.
				if _, taken := all[target]; taken {
					*conflict = ErrMailboxExists
					return nil, nil
				}

				set[target] = v

				rm = append(rm, k)
			}
		}

		if len(set) == 0 {
			*conflict = ErrNoMailbox
		}

		if intentKey != "" {
			set[intentKey] = newQuotaIntent(quotaIntentRename, "", Quota{})
		}

		return set, rm
	}
}

// moveSubtreeSharedRefs moves the grantees' pointers of every mailbox of
// the oldName subtree to its name under newName.
func (u *User) moveSubtreeSharedRefs(all []Mailbox, oldName, newName string) {
	for _, m := range all {
		if m.Name == oldName || strings.HasPrefix(m.Name, oldName+Delim) {
			u.moveSharedRefs(m.MailboxRec, m.Name, newName+m.Name[len(oldName):])
		}
	}
}

// SetSubscribed changes the subscription flag of a mailbox.
func (u *User) SetSubscribed(name string, subscribed bool) error {
	name = canonical(name)

	// A subscription to a name without a mailbox is an IMAP-only record
	// (RFC 5258); there is no object for it in the account log.
	acct, err := u.beginKnownMailbox(name)
	if err != nil {
		return err
	}

	var missing, changed bool

	var id string

	err = casOmap(u, u.idx, oidRegistry, []string{regKey(name), regSubMissing + name},
		subscribeUpdate(name, subscribed, &missing, &changed, &id))
	u.invalidate()

	acct.settle(err, changed, id)

	if err != nil {
		return err
	}

	if missing && false {
		return ErrNoMailbox
	}

	return nil
}

// SetSortOrder changes where a client sorts the mailbox among its
// siblings. Like a subscription it is a registry record change, and the
// account log records it as a change to the mailbox.
func (u *User) SetSortOrder(name string, order uint32) error {
	name = canonical(name)

	acct, err := u.beginKnownMailbox(name)
	if err != nil {
		return err
	}

	var missing, changed bool

	var id string

	err = casOmap(u, u.idx, oidRegistry, []string{regKey(name)}, sortOrderUpdate(name, order, &missing, &changed, &id))
	u.invalidate()

	acct.settle(err, changed, id)

	if err != nil {
		return fmt.Errorf("set sort order %q: %w", name, err)
	}

	if missing {
		return ErrNoMailbox
	}

	return nil
}

// sortOrderUpdate returns the registry CAS function of a sort order
// change. *missing reports that there is no such mailbox, *changed that
// the record was actually rewritten.
func sortOrderUpdate(name string, order uint32, missing, changed *bool, id *string) func(map[string][]byte) (map[string][]byte, []string) {
	return func(cur map[string][]byte) (map[string][]byte, []string) {
		*missing, *changed = false, false

		var rec MailboxRec

		v, ok := cur[regKey(name)]
		if !ok || json.Unmarshal(v, &rec) != nil {
			*missing = true

			return nil, nil
		}

		*id = rec.ID
		if rec.SortOrder == order {
			return nil, nil
		}

		rec.SortOrder = order
		*changed = true
		b, _ := json.Marshal(&rec)

		return map[string][]byte{regKey(name): b}, nil
	}
}

// subscribeUpdate returns the registry CAS function of a subscription
// change. *missing reports a name without a mailbox, which RFC 5258
// allows and which is recorded separately; *changed reports that the
// record was actually rewritten.
func subscribeUpdate(name string, subscribed bool, missing, changed *bool, id *string) func(map[string][]byte) (map[string][]byte, []string) {
	return func(cur map[string][]byte) (map[string][]byte, []string) {
		*missing, *changed = false, false

		v, ok := cur[regKey(name)]
		if !ok {
			*missing = true
			_, has := cur[regSubMissing+name]

			switch {
			case subscribed && !has:
				return map[string][]byte{regSubMissing + name: []byte("1")}, nil
			case !subscribed && has:
				return nil, []string{regSubMissing + name}
			}

			return nil, nil
		}

		var rec MailboxRec
		if json.Unmarshal(v, &rec) != nil {
			return nil, nil
		}

		*id = rec.ID
		if rec.Subscribed == subscribed {
			return nil, nil
		}

		rec.Subscribed = subscribed
		*changed = true
		b, _ := json.Marshal(&rec)

		return map[string][]byte{regKey(name): b}, nil
	}
}

// QuotaUsage returns the user's current usage.
func (u *User) QuotaUsage() (Quota, error) {
	vals, err := getOmapKeys(u.idx, oidRegistry, []string{regQuotaBytes, regQuotaMsgs})
	if err != nil {
		return Quota{}, err
	}

	b, _ := strconv.ParseInt(string(vals[regQuotaBytes]), 10, 64)
	m, _ := strconv.ParseInt(string(vals[regQuotaMsgs]), 10, 64)

	return Quota{Bytes: b, Msgs: m}, nil
}

// QuotaLimit returns the user's limit: a per-user value stored in the
// registry (SETQUOTA) or, if none, the configured default.
func (u *User) QuotaLimit() (Quota, error) {
	vals, err := getOmapKeys(u.idx, oidRegistry, []string{regQuotaLBytes, regQuotaLMsgs})
	if err != nil {
		return Quota{}, err
	}

	_, l := u.quotaState(vals)

	return l, nil
}

// quotaState reads the usage and the limit (per-user value or the
// configured default) from registry values.
func (u *User) quotaState(cur map[string][]byte) (usage, limit Quota) {
	usage.Bytes, _ = strconv.ParseInt(string(cur[regQuotaBytes]), 10, 64)
	usage.Msgs, _ = strconv.ParseInt(string(cur[regQuotaMsgs]), 10, 64)

	limit = u.limit
	if v, ok := cur[regQuotaLBytes]; ok {
		limit.Bytes, _ = strconv.ParseInt(string(v), 10, 64)
	}

	if v, ok := cur[regQuotaLMsgs]; ok {
		limit.Msgs, _ = strconv.ParseInt(string(v), 10, 64)
	}

	return usage, limit
}

// usageValues encodes the usage counters for a registry write.
func usageValues(q Quota) map[string][]byte {
	return map[string][]byte{
		regQuotaBytes: []byte(strconv.FormatInt(q.Bytes, 10)),
		regQuotaMsgs:  []byte(strconv.FormatInt(q.Msgs, 10)),
	}
}

// SetQuotaLimit stores a per-user limit (SETQUOTA).
func (u *User) SetQuotaLimit(l Quota) error {
	return u.setQuotaPair(regQuotaLBytes, regQuotaLMsgs, l)
}

// SetQuotaEnforced switches the limit check of this view's reservations
// (model.QuotaExemptable); the usage is counted either way.
func (u *User) SetQuotaEnforced(enforced bool) { u.noEnforce = !enforced }

// SilenceQuotaEvents implements model.QuotaEventSilencer for this view.
func (u *User) SilenceQuotaEvents() { u.onQuota = nil }

// SetStorageLimit stores a per-user storage limit; the message limit
// keeps following its own setting (or the configured default).
func (u *User) SetStorageLimit(bytes int64) error {
	return casOmap(u, u.idx, oidRegistry, nil, func(_ map[string][]byte) (map[string][]byte, []string) {
		return map[string][]byte{regQuotaLBytes: []byte(strconv.FormatInt(bytes, 10))}, nil
	})
}

// SetQuotaUsage overwrites the usage counters (the scrubber, the offline
// recount). Whoever overwrites the counters invalidates a running online
// recount: its run key and its captures go in the same write (the rest
// of a capture set beyond one page in further version-checked writes).
func (u *User) SetQuotaUsage(q Quota) error {
	more := false

	err := casOmapPrefix(u, u.idx, oidRegistry, []string{regQuotaRun, regQuotaCaptureHead},
		[]string{regQuotaReleasePrefix, regQuotaMailboxPrefix},
		func(cur map[string][]byte, pages map[string]prefixPage) (map[string][]byte, []string) {
			set := usageValues(q)
			rm := []string{}

			for _, k := range []string{regQuotaRun, regQuotaCaptureHead} {
				if _, ok := cur[k]; ok {
					rm = append(rm, k)
				}
			}

			more = false

			for _, p := range pages {
				more = more || p.more

				for k := range p.vals {
					rm = append(rm, k)
				}
			}

			return set, rm
		})
	if err != nil || !more {
		return err
	}

	return u.ClearQuotaRecount("")
}

// setQuotaPair overwrites the registry keys bytesKey and msgsKey with the
// values of q.
func (u *User) setQuotaPair(bytesKey, msgsKey string, q Quota) error {
	return casOmap(u, u.idx, oidRegistry, nil, func(_ map[string][]byte) (map[string][]byte, []string) {
		return map[string][]byte{
			bytesKey: []byte(strconv.FormatInt(q.Bytes, 10)),
			msgsKey:  []byte(strconv.FormatInt(q.Msgs, 10)),
		}, nil
	})
}

// reservation is quota taken for an operation before it writes
// anything. settle ends it: the used part is kept and reported as one
// change, the rest is given back silently. The reported change starts
// at the usage seen when reserving; other writers may have moved the
// counters since, so "after" is this operation's own step (which is
// what the edge-triggered warnings need), not the current usage. A reservation for a mailbox
// that a rule ignores is inactive and settles to nothing.
//
// tag is the hash of the online recount that captured the reservation
// ("" none): the entries it produces carry it (Meta.QuotaTag), and only
// such a reservation is uncaptured when it is given back. intent is a
// registry intent the reservation set ("qi:" of a COPY or MOVE) that
// settle removes: at once in the write that gives something back,
// otherwise with the next registry write of the view.
type reservation struct {
	u      *User
	total  Quota
	before Quota
	limit  Quota
	active bool
	tag    string
	intent string
}

// reserveWith checks the limit for saving total into mb (with the
// mailbox's extra allowance and the grace) and adds it to the usage in
// one compare-and-swap on the registry object, so concurrent deliveries
// and COPY batches cannot jointly overshoot the limit (each sees the
// reservations of the others). ErrOverQuota leaves the counters
// untouched. The registry keys intents are set in the same CAS (written
// even when nothing is to be reserved, then in a CAS of their own, and
// not at all on ErrOverQuota). Finished intents of the view go along
// with either write (intents.go).
//
// The CAS reads the mailbox record of mb as well: a mailbox a
// mailbox_delete job locks (stage B17) takes nothing (ErrMailboxLocked),
// exactly, in the same write that would reserve. A live online recount
// (stage B16) captures the reservation.
func (u *User) reserveWith(mb *Mailbox, total Quota, intents map[string][]byte) (*reservation, error) {
	r := &reservation{u: u, total: total}
	if !u.rules.Counts(mb) || (total.Bytes == 0 && total.Msgs == 0) {
		if len(intents) > 0 {
			if err := u.setIntentFor(mb, intents); err != nil {
				return nil, err
			}
		}

		return r, nil
	}

	out, err := u.reserveRounds(mb, total, intents, r)
	if err != nil {
		return nil, err
	}

	if err := out.refusal(); err != nil {
		return nil, err
	}

	if r.tag != "" {
		mh.recountCapture("reserve")
	}

	r.active = true

	return r, nil
}

// reserveRounds runs the reservation CAS, again after a hold of an
// online recount (at most three times).
func (u *User) reserveRounds(mb *Mailbox, total Quota, intents map[string][]byte, r *reservation) (reserveOutcome, error) {
	var out reserveOutcome

	keys := append(append([]string(nil), quotaKeys...), regKey(mb.Name))

	for round := 0; round < 3; round++ {
		done := u.takeIntents()

		err := casOmap(u, u.idx, oidRegistry, keys, func(cur map[string][]byte) (map[string][]byte, []string) {
			return u.reserveRound(mb, total, intents, done, r, &out, cur)
		})
		if err != nil || out.refusal() != nil {
			u.restoreIntents(done)
		}

		if err != nil {
			return out, err
		}

		if out.hold.IsZero() {
			return out, nil
		}

		u.recountHeld(out.hold)
	}

	return out, nil
}

// reserveOutcome is why a round of the reservation CAS wrote nothing.
type reserveOutcome struct {
	over, locked bool
	hold         time.Time
}

// refusal is the error the outcome stands for (nil: reserved).
func (o reserveOutcome) refusal() error {
	switch {
	case o.locked:
		return ErrMailboxLocked
	case o.over:
		return ErrOverQuota
	case !o.hold.IsZero():
		return fmt.Errorf("quota: the usage counters are held by a recount, try again")
	}

	return nil
}

// reserveRound is one round of the reservation CAS.
func (u *User) reserveRound(mb *Mailbox, total Quota, intents map[string][]byte, done []string, r *reservation, out *reserveOutcome,
	cur map[string][]byte,
) (map[string][]byte, []string) {
	*out = reserveOutcome{}
	r.before, r.limit = u.quotaState(cur)

	if lockedRecord(cur[regKey(mb.Name)], "", time.Now()) {
		out.locked = true
		return nil, nil
	}

	st := readCapture(cur)
	if h := st.holdUntil(); !h.IsZero() {
		out.hold = h
		return nil, nil
	}

	if !u.noEnforce && !u.rules.Allows(r.before, u.rules.Limit(r.limit, mb), total.Bytes, total.Msgs) {
		out.over = true
		return nil, nil
	}

	set := usageValues(Quota{Bytes: max(r.before.Bytes+total.Bytes, 0), Msgs: max(r.before.Msgs+total.Msgs, 0)})
	maps.Copy(set, intents)

	r.tag = ""

	if st.live() {
		st.arrive(total)
		r.tag = st.hash()
	}

	set, rm, aborted := st.finish(set, done, u.recount)
	if aborted {
		mh.recountCapture("abort")
	}

	return set, rm
}

// setIntentFor is setIntent for a write into mb that reserves nothing
// (an ignored mailbox): the mailbox lock is checked in the same CAS.
func (u *User) setIntentFor(mb *Mailbox, set map[string][]byte) error {
	done := u.takeIntents()
	locked := false

	err := casOmap(u, u.idx, oidRegistry, []string{regKey(mb.Name)}, func(cur map[string][]byte) (map[string][]byte, []string) {
		if locked = lockedRecord(cur[regKey(mb.Name)], "", time.Now()); locked {
			return nil, nil
		}

		return set, done
	})
	if err != nil || locked {
		u.restoreIntents(done)
	}

	if err != nil {
		return fmt.Errorf("record intent: %w", err)
	}

	if locked {
		return ErrMailboxLocked
	}

	return nil
}

// settle keeps used of the reservation and gives back the rest.
func (r *reservation) settle(used Quota) {
	if !r.active {
		r.dropIntent(false)
		return
	}

	r.active = false

	if rest := (Quota{Bytes: used.Bytes - r.total.Bytes, Msgs: used.Msgs - r.total.Msgs}); rest != (Quota{}) {
		r.giveBack(rest)
	} else {
		r.dropIntent(false)
	}

	if used != (Quota{}) {
		r.u.quotaChanged(r.before, Quota{Bytes: r.before.Bytes + used.Bytes, Msgs: r.before.Msgs + used.Msgs}, r.limit)
	}
}

// giveBack returns rest (negative) and removes the reservation's intent
// in the same write: a standing intent of an abandoned copy would hold
// the barrier of a recount up.
func (r *reservation) giveBack(rest Quota) {
	ch := usageChange{d: rest, tag: r.tag}
	if r.intent != "" {
		ch.rm = []string{r.intent}
		r.u.intentAgain(r.intent)
	}

	if err := r.u.applyUsage(ch); err != nil {
		r.u.log.Warn("quota release failed", "err", err)
		r.dropIntent(true)
	}
}

// dropIntent queues the reservation's intent for removal (now: at once).
func (r *reservation) dropIntent(now bool) {
	if r.intent == "" {
		return
	}

	if now {
		if err := rmKeys(r.u.idx, oidRegistry, []string{r.intent}); err != nil {
			r.u.log.Warn("intent not removed", "key", r.intent, "err", err)
		}

		return
	}

	r.u.intentDone(r.intent)
}

// moveUsage releases the entries a MOVE removed from src when they now
// sit in a mailbox that a rule ignores. The opposite direction is
// reserved up front by Move.
func (u *User) moveUsage(src, dst *Mailbox, removed []Meta) error {
	if !u.rules.Counts(src) || u.rules.Counts(dst) {
		return nil
	}

	return u.releaseUsage(src, removed)
}

// releaseUsage takes the entries removed from mb out of the usage,
// unless a rule ignores mb. A live online recount captures them with
// their identity (an empty release too, which costs nothing).
func (u *User) releaseUsage(mb *Mailbox, removed []Meta) error {
	if !u.rules.Counts(mb) {
		return nil
	}

	q := sizeOf(removed)

	return u.applyUsage(usageChange{d: Quota{Bytes: -q.Bytes, Msgs: -q.Msgs}, notify: true, mailbox: mb.ID, released: removed})
}

// releaseDeleted takes the entries of a deleted mailbox out of the usage
// (unless a rule ignores it) and marks its tombstone released in the
// same write; a live online recount captures the tagged entries under
// "qm:". The tombstone is marked even for an ignored mailbox and even
// when there is nothing to release: a tombstone with q = 0 is listed by
// every run as a release still to come.
func (u *User) releaseDeleted(mb *Mailbox, entries []Meta) error {
	ch := usageChange{notify: true, mailbox: mb.ID, released: entries, tombstone: true}

	if u.rules.Counts(mb) {
		q := sizeOf(entries)
		ch.d = Quota{Bytes: -q.Bytes, Msgs: -q.Msgs}
	}

	return u.applyUsage(ch)
}

// quotaChanged reports a committed change of the usage.
func (u *User) quotaChanged(before, after, limit Quota) {
	if u.onQuota != nil && before != after {
		u.onQuota(u.name, before, after, limit)
	}
}

// ---------------------------------------------------------------------------
// Mailbox header (shard 0)

// readHeader reads the header keys of shard 0 and returns the object
// version they were read at, for compare-and-swap writes.
func (u *User) readHeader(mb *Mailbox) (hdr *MailboxInfo, version uint64, err error) {
	defer mh.timer("header_read")()

	end := u.trace(spanHeaderRead, attrOID(mboxOID(mb.ID)))
	defer func() { end(err) }()

	vals, ver, err := u.getKeysVer(u.idx, mboxOID(mb.ID), []string{keyUIDNext, keyUIDValid, keyModSeq, keyKeywords, keyLogStart, keyShardSize, keyRecent})
	if err != nil {
		return nil, 0, fmt.Errorf("read header %q: %w", mb.Name, err)
	}

	if ver == 0 {
		return nil, 0, ErrNoMailbox
	}

	info := &MailboxInfo{Mailbox: *mb}
	n, _ := strconv.ParseUint(string(vals[keyUIDNext]), 10, 32)
	info.UIDNext = uint32(n)
	info.HighestModSeq, _ = strconv.ParseUint(string(vals[keyModSeq]), 10, 64)
	info.LogStart, _ = strconv.ParseUint(string(vals[keyLogStart]), 10, 64)
	ss, _ := strconv.ParseUint(string(vals[keyShardSize]), 10, 32)
	info.ShardSize = uint32(ss)
	rc, _ := strconv.ParseUint(string(vals[keyRecent]), 10, 32)

	info.RecentFrom = uint32(rc)
	if info.ShardSize == 0 {
		info.ShardSize = DefaultShardSize // mailboxes created before sharding
	}

	_ = json.Unmarshal(vals[keyKeywords], &info.Keywords)
	if info.UIDNext == 0 {
		return nil, 0, ErrNoMailbox
	}

	return info, ver, nil
}

// Info returns the mailbox header.
func (u *User) Info(name string) (*MailboxInfo, error) {
	mb, err := u.Lookup(name)
	if err != nil {
		return nil, err
	}

	info, _, err := u.readHeader(mb)

	return info, err
}

// changeRec is a change log entry. Pending marks a two-phase change whose
// entries on other shards are not yet written; readers wait for it
// (bounded by pendingMaxAge) instead of skipping the UIDs.
type changeRec struct {
	UIDs     []uint32 `json:"u"`
	Expunged bool     `json:"x,omitempty"`
	Pending  bool     `json:"p,omitempty"`
	Time     int64    `json:"t,omitempty"`
}

// pendingMaxAge bounds how long readers wait for a pending change; after
// that a crashed writer is assumed and the change is treated as done.
const pendingMaxAge = 30 * time.Second

// headerChange is what a header mutation wants to do.
type headerChange struct {
	set map[string][]byte // omap keys to set on shard 0 (besides the header keys)
	rm  []string          // omap keys to remove on shard 0
	chg *changeRec        // change record (nil: no modseq bump, no publish)
}

// casHeader runs fn against the current header and applies the returned
// changes to shard 0 atomically, guarded by the object version that the
// header was read at. If another writer got in between, RADOS rejects the
// write and we retry (the scheme RGW uses for bucket indexes; no lock
// service). A change record bumps h:modseq and is logged; fn receives the
// modseq the change will carry.
func (u *User) casHeader(mb *Mailbox, fn func(info *MailboxInfo, modseq uint64) (*headerChange, error)) (*MailboxInfo, uint64, error) {
	for attempt := 0; attempt < casRetries; attempt++ {
		info, ver, err := u.readHeader(mb)
		if err != nil {
			return nil, 0, err
		}

		modseq := info.HighestModSeq + 1

		hc, err := fn(info, modseq)
		if err != nil {
			return nil, 0, err
		}

		if hc == nil || (len(hc.set) == 0 && len(hc.rm) == 0 && hc.chg == nil) {
			return info, info.HighestModSeq, nil
		}

		set := headerSet(hc, modseq)

		err = u.writeHeader(mb, ver, set, hc.rm)
		if err == nil {
			return u.headerWritten(mb, info, hc, modseq)
		}

		if c := errCode(err); c == syscall.ERANGE || c == syscall.EOVERFLOW {
			mh.casRetry("header", casClassMbox)
			continue
		}

		return nil, 0, fmt.Errorf("update index %q: %w", mb.Name, err)
	}

	return nil, 0, fmt.Errorf("update index %q: too many concurrent writers", mb.Name)
}

// headerSet returns the omap keys a header change sets on shard 0: its
// own keys plus, for a change record, the new modseq, the change record
// (stamped with the current time) and, for an expunge, the vanished
// record.
func headerSet(hc *headerChange, modseq uint64) map[string][]byte {
	set := map[string][]byte{}
	for k, v := range hc.set {
		set[k] = v
	}

	if hc.chg != nil {
		set[keyModSeq] = []byte(strconv.FormatUint(modseq, 10))
		hc.chg.Time = time.Now().Unix()
		b, _ := json.Marshal(hc.chg)
		set[chgKey(modseq)] = b

		if hc.chg.Expunged {
			vb, _ := json.Marshal(hc.chg.UIDs)
			set[vanKey(modseq)] = vb
		}
	}

	return set
}

// writeHeader writes set and removes rm on shard 0 of mb, guarded by the
// object version ver.
func (u *User) writeHeader(mb *Mailbox, ver uint64, set map[string][]byte, rm []string) error {
	op := rados.CreateWriteOp()
	op.AssertVersion(ver)

	if len(set) > 0 {
		op.SetOmap(set)
	}

	if len(rm) > 0 {
		op.RmOmapKeys(rm)
	}

	done := mh.timer("header_cas")
	endSpan := u.trace(spanHeaderCAS, attrOID(mboxOID(mb.ID)))
	err := op.Operate(u.idx, mboxOID(mb.ID), rados.OperationNoFlag)

	endSpan(err)
	op.Release()
	done()

	return err
}

// headerWritten finishes a successful header write: it updates info,
// notifies watchers of a committed change and trims the log now and
// then. It returns what casHeader returns.
func (u *User) headerWritten(mb *Mailbox, info *MailboxInfo, hc *headerChange, modseq uint64) (*MailboxInfo, uint64, error) {
	if hc.chg == nil {
		return info, info.HighestModSeq, nil
	}

	info.HighestModSeq = modseq

	if !hc.chg.Pending {
		u.notify(mb.ID)
	}

	if modseq%256 == 0 {
		u.background(func() { u.trimLog(mb.ID) })
	}

	return info, modseq, nil
}

// commitPending flips a pending change record to committed (and removes
// extra keys, e.g. a MOVE intent) in one write. It is the second phase of
// a two-phase change; no modseq is consumed.
func (u *User) commitPending(mb *Mailbox, modseq uint64, chg *changeRec, rm []string, extra map[string][]byte) error {
	chg.Pending = false
	b, _ := json.Marshal(chg)

	op := rados.CreateWriteOp()
	defer op.Release()

	// A mailbox deleted meanwhile must not get its shard 0 back as an
	// object that holds nothing but this record.
	op.AssertExists()

	set := map[string][]byte{chgKey(modseq): b}
	maps.Copy(set, extra)

	if chg.Expunged {
		vb, _ := json.Marshal(chg.UIDs)
		set[vanKey(modseq)] = vb
	}

	op.SetOmap(set)

	if len(rm) > 0 {
		op.RmOmapKeys(rm)
	}

	if err := op.Operate(u.idx, mboxOID(mb.ID), rados.OperationNoFlag); err != nil {
		return fmt.Errorf("commit change %d: %w", modseq, err)
	}

	u.notify(mb.ID)

	return nil
}

// background runs f without blocking; Close waits for it.
func (u *User) background(f func()) {
	u.inflight.Add(1)
	go func() {
		defer u.inflight.Done()

		f()
	}()
}

// trimLog keeps the change and vanished logs bounded and moves h:logstart
// forward (never backward: a slower concurrent trim must not undo a newer
// one, so the write is a CAS against the current value). It reads the
// header first and the log only when the window h:logstart..h:modseq is
// over the retention, and then only the records below the new start; see
// model.PlanLogTrim for the rule and how it relates to the count-based
// one it replaces.
func (u *User) trimLog(id string) {
	oid := mboxOID(id)

	hv, err := getOmapKeys(u.idx, oid, []string{keyModSeq, keyLogStart})
	if err != nil {
		return
	}

	head, _ := strconv.ParseUint(string(hv[keyModSeq]), 10, 64)
	logStart, _ := strconv.ParseUint(string(hv[keyLogStart]), 10, 64)

	start, drop, err := model.PlanLogTrim(head, logStart, LogRetention, omapPageSize, chgKey, u.logPageReader(oid, keyChgPrefix))
	if err != nil || start == 0 {
		return
	}

	rm := append([]string{}, drop...)
	for _, k := range drop {
		rm = append(rm, keyVanPrefix+strings.TrimPrefix(k, keyChgPrefix))
	}

	_ = casOmap(u, u.idx, oid, []string{keyLogStart}, func(cur map[string][]byte) (map[string][]byte, []string) {
		have, _ := strconv.ParseUint(string(cur[keyLogStart]), 10, 64)
		if have >= start {
			return nil, nil
		}

		return map[string][]byte{keyLogStart: []byte(strconv.FormatUint(start, 10))}, rm
	})
}

// logPageReader lists the keys of one log (a key prefix of oid) for
// model.PlanLogTrim.
func (u *User) logPageReader(oid, prefix string) model.LogPageReader {
	return func(after string, limit int) ([]string, error) {
		keys := make([]string, 0, limit)

		_, err := listOmapKeys(u.idx, oid, after, prefix, uint64(limit), func(key string) { keys = append(keys, key) })

		return keys, err
	}
}

// Changes returns the change records after since, oldest first. ok is
// false when the log no longer covers since (caller reloads fully).
func (u *User) Changes(id string, since uint64) (changes []Change, ok bool, err error) {
	vals, err := u.idx.GetAllOmapValues(mboxOID(id), chgKey(since), keyChgPrefix, omapPageSize)
	if err != nil {
		if errCode(err) == syscall.ENOENT {
			return nil, false, ErrNoMailbox
		}

		return nil, false, err
	}

	hv, err := getOmapKeys(u.idx, mboxOID(id), []string{keyLogStart})
	if err != nil {
		return nil, false, err
	}

	start, _ := strconv.ParseUint(string(hv[keyLogStart]), 10, 64)
	if since+1 < start {
		return nil, false, nil
	}

	keys := make([]string, 0, len(vals))
	for k := range vals {
		keys = append(keys, k)
	}

	sort.Strings(keys)

	for _, k := range keys {
		var rec changeRec
		if json.Unmarshal(vals[k], &rec) != nil {
			continue
		}

		ms, _ := strconv.ParseUint(strings.TrimPrefix(k, keyChgPrefix), 10, 64)
		pending := rec.Pending && time.Since(time.Unix(rec.Time, 0)) < pendingMaxAge
		changes = append(changes, Change{ModSeq: ms, UIDs: rec.UIDs, Expunged: rec.Expunged, Pending: pending})
	}

	return changes, true, nil
}

// PendingFloor returns the modseq just before the oldest still-pending
// change (within pendingMaxAge), so a session that loaded a full snapshot
// resumes its incremental polling from there and does not miss the
// change once it is committed. ok is false when nothing is pending.
//
// Only records younger than pendingMaxAge can be pending, so the scan
// starts where the log is older than that: the start is found by probing
// single records at doubling distances from the head (log2 reads), not by
// a fixed window, so a busy mailbox with thousands of changes per minute
// is covered as well as an idle one.
func (u *User) PendingFloor(id string, highest uint64) (uint64, bool, error) {
	from, err := u.pendingScanStart(id, highest)
	if err != nil {
		return 0, false, err
	}

	vals, err := u.idx.GetAllOmapValues(mboxOID(id), chgKey(from), keyChgPrefix, omapPageSize)
	if err != nil {
		if errCode(err) == syscall.ENOENT {
			return 0, false, ErrNoMailbox
		}

		return 0, false, err
	}

	var oldest uint64

	for k, v := range vals {
		var rec changeRec
		if json.Unmarshal(v, &rec) != nil || !rec.Pending || time.Since(time.Unix(rec.Time, 0)) >= pendingMaxAge {
			continue
		}

		ms, _ := strconv.ParseUint(strings.TrimPrefix(k, keyChgPrefix), 10, 64)
		if oldest == 0 || ms < oldest {
			oldest = ms
		}
	}

	if oldest == 0 {
		return 0, false, nil
	}

	return oldest - 1, true, nil
}

// pendingScanStart finds a modseq at or before which every change record
// is older than pendingMaxAge (or the log begins).
func (u *User) pendingScanStart(id string, highest uint64) (uint64, error) {
	start := uint64(0)

	for step := uint64(1); step < highest && step < 1<<40; step *= 2 {
		probe := highest - step

		vals, err := getOmapKeys(u.idx, mboxOID(id), []string{chgKey(probe)})
		if err != nil {
			if errCode(err) == syscall.ENOENT {
				return 0, ErrNoMailbox
			}

			return 0, err
		}

		v, ok := vals[chgKey(probe)]
		if !ok {
			continue // gap or trimmed: look further back
		}

		var rec changeRec
		if json.Unmarshal(v, &rec) == nil && time.Since(time.Unix(rec.Time, 0)) >= pendingMaxAge {
			start = probe
			break
		}
	}

	return start, nil
}

// Vanished returns the UIDs expunged after the given modseq, for QRESYNC.
func (u *User) Vanished(id string, since uint64) ([]uint32, error) {
	vals, err := u.idx.GetAllOmapValues(mboxOID(id), vanKey(since), keyVanPrefix, omapPageSize)
	if err != nil {
		if errCode(err) == syscall.ENOENT {
			return nil, ErrNoMailbox
		}

		return nil, err
	}

	var out []uint32

	for _, v := range vals {
		var uids []uint32
		if json.Unmarshal(v, &uids) == nil {
			out = append(out, uids...)
		}
	}

	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })

	return out, nil
}

// ---------------------------------------------------------------------------
// Entries (all shards)

// ListMessages returns the index records of a mailbox (by id) in UID
// order, reading all shards in parallel.
func (u *User) ListMessages(id string) (list []Meta, err error) {
	defer mh.timer("list_entries")()

	end := u.trace(spanListEntry, attrOID(mboxOID(id)))
	defer func() { end(err) }()

	n, ss, err := u.listRange(id)
	if err != nil {
		return nil, err
	}

	last := shardOf(uint32(max(n-1, 1)), uint32(ss))

	shards := map[string][]string{}
	for s := uint32(0); s <= last; s++ {
		shards[u.shardOID(id, s)] = nil
	}

	var (
		mu    sync.Mutex
		metas []Meta
	)

	err = parallel(shards, func(oid string, _ []string) error {
		vals, err := u.idx.GetAllOmapValues(oid, "", keyMsgPrefix, omapPageSize)
		if err != nil {
			if errCode(err) == syscall.ENOENT {
				return nil
			}

			return err
		}

		mu.Lock()
		defer mu.Unlock()

		for k, v := range vals {
			var m Meta
			if err := json.Unmarshal(v, &m); err != nil {
				u.log.Warn("corrupt index record", "mailbox", id, "key", k, "err", err)
				return fmt.Errorf("corrupt index record %s in %s", k, oid)
			}

			// An entry at or above UIDNEXT is the leftover of an
			// interrupted import step, not a message yet.
			if uint64(m.UID) < n {
				metas = append(metas, m)
			}
		}

		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("list %s: %w", id, err)
	}

	sort.Slice(metas, func(i, j int) bool { return metas[i].UID < metas[j].UID })

	return metas, nil
}

// listRange reads UIDNEXT and the shard size of a mailbox; UIDNEXT 0 is
// an index object retired by an import replacement (no mailbox).
func (u *User) listRange(id string) (uint64, uint64, error) {
	hv, ver, err := u.getKeysVer(u.idx, mboxOID(id), []string{keyUIDNext, keyShardSize})
	if err != nil {
		return 0, 0, fmt.Errorf("list %s: %w", id, err)
	}

	if ver == 0 {
		return 0, 0, ErrNoMailbox
	}

	n, _ := strconv.ParseUint(string(hv[keyUIDNext]), 10, 32)
	if n == 0 {
		return 0, 0, ErrNoMailbox
	}

	ss, _ := strconv.ParseUint(string(hv[keyShardSize]), 10, 32)

	return n, ss, nil
}

// ListMessagesByName is ListMessages for a mailbox name.
func (u *User) ListMessagesByName(name string) ([]Meta, error) {
	mb, err := u.Lookup(name)
	if err != nil {
		return nil, err
	}

	return u.ListMessages(mb.ID)
}

// GetMessages returns the records of the given UIDs (missing ones are
// skipped), grouped by shard and read in parallel.
func (u *User) GetMessages(id string, uids []uint32) (found []Meta, err error) {
	defer mh.timer("get_entries")()

	end := u.trace(spanGetEntry, attrOID(mboxOID(id)), attrCount(len(uids)))
	defer func() { end(err) }()

	if len(uids) == 0 {
		return nil, nil
	}

	hv, err := getOmapKeys(u.idx, mboxOID(id), []string{keyShardSize})
	if err != nil {
		return nil, err
	}

	ss, _ := strconv.ParseUint(string(hv[keyShardSize]), 10, 32)

	return u.getEntries(id, uint32(ss), uids)
}

func (u *User) getEntries(id string, shardSize uint32, uids []uint32) ([]Meta, error) {
	byShard := map[string][]string{}

	for _, uid := range uids {
		oid := u.shardOID(id, shardOf(uid, shardSize))
		byShard[oid] = append(byShard[oid], uidKey(uid))
	}

	var (
		mu    sync.Mutex
		metas []Meta
	)

	err := parallel(byShard, func(oid string, keys []string) error {
		vals, err := getOmapKeys(u.idx, oid, keys)
		if err != nil {
			return err
		}

		mu.Lock()
		defer mu.Unlock()

		for _, v := range vals {
			var m Meta
			if json.Unmarshal(v, &m) == nil {
				metas = append(metas, m)
			}
		}

		return nil
	})

	sort.Slice(metas, func(i, j int) bool { return metas[i].UID < metas[j].UID })

	return metas, err
}

// writeEntries sets/removes entries on one shard object (second phase of a
// two-phase change) with a compare-and-swap on the shard. Records are only
// replaced when the stored one is older (lower modseq), so a slow writer
// cannot overtake a newer one; with updateOnly a record that no longer
// exists is not recreated (a late STORE must not resurrect an expunged
// message).
func (u *User) writeEntries(oid string, set map[string][]byte, rm []string, updateOnly bool) error {
	_, err := u.modifyEntries(oid, set, rm, updateOnly)
	return err
}

// modifyEntries is writeEntries returning the keys actually removed.
func (u *User) modifyEntries(oid string, set map[string][]byte, rm []string, updateOnly bool) ([]string, error) {
	keys := make([]string, 0, len(set)+len(rm))
	for k := range set {
		keys = append(keys, k)
	}

	keys = append(keys, rm...)

	var removed []string

	err := casOmap(u, u.idx, oid, keys, func(cur map[string][]byte) (map[string][]byte, []string) {
		out := map[string][]byte{}

		for k, v := range set {
			old, exists := cur[k]
			if updateOnly && !exists {
				continue
			}

			var mine, theirs Meta
			if exists && json.Unmarshal(old, &theirs) == nil && json.Unmarshal(v, &mine) == nil && theirs.ModSeq >= mine.ModSeq {
				continue
			}

			out[k] = v
		}

		removed = removed[:0]

		for _, k := range rm {
			if _, ok := cur[k]; ok {
				removed = append(removed, k)
			}
		}

		return out, removed
	})

	return removed, err
}

// applyEntries performs a read-modify-write of entries on one higher
// shard under the shard's own version: fn sees the current records and
// returns the new ones. This is what makes concurrent STOREs on the same
// shard serialise correctly.
func (u *User) applyEntries(oid string, keys []string, fn func(cur map[string]Meta) map[string]Meta) error {
	return casOmap(u, u.idx, oid, keys, func(raw map[string][]byte) (map[string][]byte, []string) {
		cur := map[string]Meta{}

		for k, v := range raw {
			var m Meta
			if json.Unmarshal(v, &m) == nil {
				cur[k] = m
			}
		}

		set := map[string][]byte{}

		for k, m := range fn(cur) {
			b, _ := json.Marshal(&m)
			set[k] = b
		}

		return set, nil
	})
}

// ---------------------------------------------------------------------------
// Messages

// Deliver stores a message in the mailbox and returns its index record.
//
// Order of writes: the immutable blob first, then the FTS postings, then
// the index update that makes the message visible. A crash before the
// last step leaves at most an orphan blob (and postings pointing at a
// GUID no mailbox lists, which searches simply do not match), never a UID
// without data.
func (u *User) Deliver(name string, raw []byte, flags []string, when time.Time) (*Meta, error) {
	return u.DeliverGUID(name, newGUID(), raw, flags, when)
}

// DeliverGUID is Deliver with a caller-chosen GUID (delivery intents).
//
// The blob intent "bi:<guid>" is set in the reservation's CAS, before the
// blob is written, and queued for removal once the entry is linked (or
// the blob is deleted again); see intents.go.
func (u *User) DeliverGUID(name, guid string, raw []byte, flags []string, when time.Time) (stored *Meta, err error) {
	end := u.trace(spanDeliver, attrBytes(len(raw)))
	defer func() { end(err) }()

	if g, err := hex.DecodeString(guid); err != nil || len(g) != guidBytes {
		return nil, fmt.Errorf("bad guid %q", guid)
	}

	if int64(len(raw)) > MaxMessageLen {
		return nil, ErrTooLarge
	}

	mb, err := u.Lookup(name)
	if err != nil {
		return nil, err
	}
	// The quota is reserved before anything is written; a failure below
	// gives it back. Mailboxes a rule ignores reserve nothing.
	size := Quota{Bytes: int64(len(raw)), Msgs: 1}
	intent := blobIntentKey(guid)

	u.intentAgain(intent)

	res, err := u.reserveWith(mb, size, map[string][]byte{intent: u.newBlobIntent(size.Bytes, mb.ID)})
	if err != nil {
		return nil, err
	}

	// undo gives back what was done when the message cannot be stored:
	// the blob goes, and with it the intent (unless the blob may still
	// be there; then the scrubber deletes it).
	undo := func(chunks int, indexed bool) {
		if indexed {
			_ = u.ftsRemoveWait(&Meta{GUID: guid, Size: size.Bytes, Chunks: chunks}, 0)
		}

		// After a failed blob write (chunks 0) the write removed what it
		// created; the intent stays for anything it could not.
		if chunks > 0 && u.deleteBlobAll(guid, chunks) {
			u.intentDone(intent)
		}

		res.settle(Quota{})
	}

	meta, err := u.storeAndLink(mb, guid, raw, flags, when, res.tag, undo)
	if err != nil {
		return nil, err
	}

	res.settle(size)
	u.intentDone(intent)

	return meta, nil
}

// storeAndLink writes the blob and its postings and links the entry; on
// failure it calls undo with what is to be given back. tag is the online
// recount that captured the reservation ("" none): the entry carries it.
func (u *User) storeAndLink(mb *Mailbox, guid string, raw []byte, flags []string, when time.Time, tag string, undo func(chunks int, indexed bool)) (*Meta, error) {
	if when.IsZero() {
		when = time.Now()
	}

	chunks, err := u.writeBlob(guid, raw)
	if err != nil {
		undo(0, false)
		return nil, fmt.Errorf("write blob: %w", err)
	}

	if err := u.ftsIndex(guid, raw); err != nil {
		// What was indexed before the failure goes again; the blob is
		// still there for that.
		undo(chunks, true)

		return nil, err
	}

	p := mailmeta.Parse(raw)
	m := Meta{GUID: guid, Size: int64(len(raw)), Chunks: chunks, Flags: flags,
		InternalDate: when.UTC(), SaveDate: time.Now().UTC(), Envelope: p.Envelope, Routes: p.Routes, References: p.Refs, Preview: p.Preview,
		HasAttachment: mailmeta.HasAttachment(raw), QuotaTag: tag}

	meta, err := u.linkLogged(mb, m)
	if err != nil {
		undo(chunks, true)

		return nil, err
	}

	return meta, nil
}

// linkLogged links an index entry and records it in the account change
// log, announced before the write and recorded after it.
func (u *User) linkLogged(mb *Mailbox, m Meta) (*Meta, error) {
	acct, err := u.beginAccount(emailSpec(mb.ID))
	if err != nil {
		return nil, err
	}

	meta, err := u.link(mb, m)
	if err != nil {
		acct.partial()

		return nil, err
	}

	acct.commit(emailChange(model.OpCreated, meta.ModSeq, []Meta{*meta}))

	return meta, nil
}

// link adds an index entry for an existing blob to the mailbox and
// allocates its UID. New keywords in the flags are registered, within
// the keyword limit of the mailbox (checked in the header CAS, so it is
// exact). Entries on shard 0 are written in the header CAS; other shards
// two-phase (pending change, entry write, commit).
func (u *User) link(mb *Mailbox, m Meta) (*Meta, error) {
	return u.linkAs(mb, m, true)
}

// linkAs is link; without limited the keyword limit is not applied. That
// is for the steps that complete what was already accepted: the links of
// a MOVE once its intent is written (the scrubber would complete it
// anyway) and the scrubber's own completion of such a MOVE.
func (u *User) linkAs(mb *Mailbox, m Meta, limited bool) (*Meta, error) {
	var learningLease model.Lease

	if u.learningHook != nil {
		var err error

		learningLease, err = u.lockLearning(mb.ID)
		if err != nil {
			return nil, err
		}
		defer learningLease.Release()

		u.invalidate()

		if !u.mailboxCurrent(mb) {
			return nil, ErrNoMailbox
		}
	}

	receipt, err := u.learningReceipt(mb, m)
	if err != nil {
		return nil, err
	}

	e := linkEntry{receipt: receipt}

	var chg *changeRec

	_, modseq, err := u.casHeader(mb, func(info *MailboxInfo, modseq uint64) (*headerChange, error) {
		if leaseLost(learningLease) {
			return nil, ErrLocked
		}

		if limited {
			if err := model.CheckKeywords(info.Keywords, m.Flags, u.limits.KeywordsPerMailbox); err != nil {
				return nil, err
			}
		}

		hc, err := u.linkHeader(mb, &m, info, modseq, &e)
		if err != nil {
			return nil, err
		}

		chg = hc.chg

		return hc, nil
	})
	if err != nil {
		return nil, err
	}

	if chg.Pending {
		if err := u.linkPending(mb, learningLease, e, modseq, chg); err != nil {
			return nil, err
		}
	}

	return &m, nil
}

// linkHeader builds the header change of one link attempt: it allocates
// the UID of m, encodes the entry into e.rec and decides its shard e.oid.
// Entries on shard 0 are part of the header change; others are only
// announced (pending).
func (u *User) linkHeader(mb *Mailbox, m *Meta, info *MailboxInfo, modseq uint64, e *linkEntry) (*headerChange, error) {
	m.UID = info.UIDNext
	m.ModSeq = modseq
	e.uid = m.UID

	var err error
	if e.rec, err = json.Marshal(m); err != nil {
		return nil, err
	}

	set := map[string][]byte{keyUIDNext: []byte(strconv.FormatUint(uint64(info.UIDNext+1), 10))}
	if e.receipt != "" {
		set["learning:"+e.receipt] = []byte(strconv.FormatUint(uint64(m.UID), 10))
	}

	e.oid = u.shardOID(mb.ID, shardOf(m.UID, info.ShardSize))

	chg := &changeRec{UIDs: []uint32{m.UID}, Pending: e.oid != mboxOID(mb.ID)}
	if !chg.Pending {
		set[uidKey(m.UID)] = e.rec
	}

	setMergedKeywords(set, info.Keywords, m.Flags)

	return &headerChange{set: set, chg: chg}, nil
}

// mailboxCurrent reports whether the name of mb still names mb (same id).
func (u *User) mailboxCurrent(mb *Mailbox) bool {
	current, err := u.Lookup(mb.Name)

	return err == nil && current.ID == mb.ID
}

// leaseLost reports whether l is held and was lost; a nil lease is never
// lost.
func leaseLost(l model.Lease) bool {
	return l != nil && l.Lost()
}

// setMergedKeywords sets the mailbox keyword list in set if flags add
// keywords to known.
func setMergedKeywords(set map[string][]byte, known, flags []string) {
	if kw, changed := mergeKeywords(known, flags); changed {
		b, _ := json.Marshal(kw)
		set[keyKeywords] = b
	}
}

// learningReceipt runs the learning hook (if any) for a message about to
// be linked into mb and returns its receipt.
func (u *User) learningReceipt(mb *Mailbox, m Meta) (string, error) {
	receipt := ""

	if u.learningHook != nil {
		var err error

		receipt, err = u.learningHook(*mb, m)
		if err != nil {
			return "", err
		}
	}

	return receipt, nil
}

// linkEntry is an index entry link writes on a higher shard.
type linkEntry struct {
	oid     string // shard object
	uid     uint32
	rec     []byte // the encoded Meta
	receipt string // learning receipt, "" for none
}

// linkPending is the second phase of link for an entry on a higher
// shard: write the entry, then commit the pending change.
func (u *User) linkPending(mb *Mailbox, learningLease model.Lease, e linkEntry, modseq uint64, chg *changeRec) error {
	if leaseLost(learningLease) {
		return ErrLocked
	}

	entrySet := map[string][]byte{uidKey(e.uid): e.rec}
	if e.receipt != "" {
		entrySet["learning:"+e.receipt] = []byte("committed")
	}

	if err := u.writeEntries(e.oid, entrySet, nil, false); err != nil {
		// The header already announced the UID (pending). Leave the
		// announcement to expire; the UID stays a gap.
		return fmt.Errorf("write entry shard: %w", err)
	}

	if err := u.commitPending(mb, modseq, chg, nil, nil); err != nil {
		// The entry is durable and visible to full reloads; the
		// pending mark expires on its own. Not an error for the caller:
		// undoing (deleting the blob) would leave an entry without data.
		u.log.Warn("commit of pending change failed, entry is written", "mailbox", mb.Name, "modseq", modseq, "err", err)
	}

	return nil
}

// mergeKeywords adds user keywords (flags not starting with a backslash)
// to the mailbox keyword list.
func mergeKeywords(known, flags []string) ([]string, bool) {
	out := append([]string{}, known...)
	changed := false

	for _, f := range flags {
		if strings.HasPrefix(f, "\\") || HasFlag(out, f) {
			continue
		}

		out = append(out, strings.ToLower(f))
		changed = true
	}

	return out, changed
}

// Read returns the raw message for an index record. Callers account the
// buffer against membudget for as long as they hold it.
func (u *User) Read(m *Meta) ([]byte, error) {
	return u.readBlob(m)
}

// SetFlags replaces the flags of the given UIDs (apply decides the new
// set). Messages whose modseq is above unchangedSince (if non-zero) are
// skipped and reported in Modified, as CONDSTORE requires.
//
// Shard 0 entries are read and written inside the header CAS. Entries on
// higher shards are handled in a second phase as a read-modify-write
// under the shard's own version: the header CAS only allocates the modseq
// and announces the UIDs as pending; the decision (flags, UNCHANGEDSINCE,
// existence) is taken against the shard's current records, so concurrent
// STOREs on the same message serialise and an expunged message is never
// recreated. The commit then records what actually changed.
func (u *User) SetFlags(name string, uids []uint32, unchangedSince uint64, apply func(old []string) []string) (res *model.FlagsResult, err error) {
	end := u.trace(spanFlags, attrCount(len(uids)))
	defer func() { end(err) }()

	mb, err := u.Lookup(name)
	if err != nil {
		return nil, err
	}

	if lockedMailbox(mb, "", time.Now()) {
		return nil, ErrMailboxLocked
	}

	acct, err := u.beginAccount(emailSpec(mb.ID))
	if err != nil {
		return nil, err
	}

	res, err = u.setFlags(name, uids, unchangedSince, apply, false, 0)
	if err != nil {
		acct.partial()
		return nil, err
	}

	if len(res.Updated) == 0 {
		acct.withdraw()
	} else {
		acct.commit(emailChange(model.OpUpdated, 0, res.Updated))
	}

	return res, nil
}

// setFlags is SetFlags with restamp: write the record even when the
// flags do not change, so that it carries a fresh modseq. That is needed
// after a modseq collision on a higher shard (see phase 2): the record
// was written with the later writer's modseq, and a client that had seen
// that modseq would otherwise miss the newer flag state. depth bounds the
// recursion.
func (u *User) setFlags(name string, uids []uint32, unchangedSince uint64, apply func(old []string) []string, restamp bool, depth int) (*model.FlagsResult, error) {
	mb, err := u.Lookup(name)
	if err != nil {
		return nil, err
	}

	res := &model.FlagsResult{}

	higher := map[string][]string{} // shard oid -> keys of UIDs living there

	updated, chg, modseq, err := u.setFlagsHeader(mb, uids, unchangedSince, apply, res, higher)
	if err != nil {
		return nil, err
	}

	if chg == nil || !chg.Pending {
		res.Updated = updated
		return res, nil
	}

	return u.setFlagsHigher(mb, name, flagsApply{unchangedSince: unchangedSince, apply: apply, restamp: restamp}, higher, modseq, chg, updated, res, depth)
}

// flagsApply is what a STORE applies: the flag function and its
// conditions.
type flagsApply struct {
	unchangedSince uint64
	apply          func(old []string) []string
	restamp        bool
}

// setFlagsHeader is phase 1 of setFlags: the header CAS allocates the
// modseq, updates the shard 0 entries and announces the UIDs on higher
// shards (filled into higher) as pending. It returns the updated shard 0
// entries and the change record (nil if nothing changed). The keyword
// limit is checked in the same CAS (flagEntries).
func (u *User) setFlagsHeader(mb *Mailbox, uids []uint32, unchangedSince uint64, apply func(old []string) []string, res *model.FlagsResult, higher map[string][]string) ([]Meta, *changeRec, uint64, error) {
	var (
		updated []Meta
		chg     *changeRec
	)

	_, modseq, err := u.casHeader(mb, func(info *MailboxInfo, modseq uint64) (*headerChange, error) {
		updated, res.Modified = updated[:0], nil

		for k := range higher {
			delete(higher, k)
		}

		shard0 := u.partitionUIDs(mb, info.ShardSize, uids, higher)

		cur, err := u.flagEntries(mb, info, shard0, apply)
		if err != nil {
			return nil, err
		}

		set := map[string][]byte{}
		kw := info.Keywords
		chg = &changeRec{Pending: len(higher) > 0}

		for _, m := range cur {
			if unchangedSince != 0 && m.ModSeq > unchangedSince {
				res.Modified = append(res.Modified, m.UID)
				continue
			}

			nf := apply(m.Flags)
			if equalFlags(nf, m.Flags) {
				continue
			}

			m.Flags = nf
			m.ModSeq = modseq
			rec, _ := json.Marshal(&m)
			set[uidKey(m.UID)] = rec
			chg.UIDs = append(chg.UIDs, m.UID)
			updated = append(updated, m)
			kw, _ = mergeKeywords(kw, nf)
		}

		announceHigher(chg, higher) // announced; trimmed at commit

		if len(updated) == 0 && len(higher) == 0 {
			return nil, nil
		}

		if len(kw) != len(info.Keywords) {
			b, _ := json.Marshal(kw)
			set[keyKeywords] = b
		}

		return &headerChange{set: set, chg: chg}, nil
	})

	return updated, chg, modseq, err
}

// flagEntries checks the keyword limit of a STORE against the header
// info and reads the shard 0 entries it changes. The limit is checked on
// what apply can add (apply(nil): the flags of +FLAGS and FLAGS, none for
// -FLAGS), because the entries on higher shards are not read here.
func (u *User) flagEntries(mb *Mailbox, info *MailboxInfo, shard0 []uint32, apply func(old []string) []string) ([]Meta, error) {
	if err := model.CheckKeywords(info.Keywords, apply(nil), u.limits.KeywordsPerMailbox); err != nil {
		return nil, err
	}

	return u.getEntries(mb.ID, info.ShardSize, shard0)
}

// partitionUIDs returns the UIDs that live on shard 0 of mb and adds the
// entry keys of the others to higher (shard oid -> keys).
func (u *User) partitionUIDs(mb *Mailbox, shardSize uint32, uids []uint32, higher map[string][]string) []uint32 {
	var shard0 []uint32

	for _, uid := range uids {
		oid := u.shardOID(mb.ID, shardOf(uid, shardSize))
		if oid == mboxOID(mb.ID) {
			shard0 = append(shard0, uid)
		} else {
			higher[oid] = append(higher[oid], uidKey(uid))
		}
	}

	return shard0
}

// announceHigher adds the UIDs of the entry keys in higher to the change
// record.
func announceHigher(chg *changeRec, higher map[string][]string) {
	for _, keys := range higher {
		for _, k := range keys {
			uid := uidOfKey(k)
			chg.UIDs = append(chg.UIDs, uid)
		}
	}
}

// flagsShardResult is the outcome of phase 2 of setFlags on one shard.
type flagsShardResult struct {
	updated  []Meta
	modified []uint32
	keywords []string
	collided []uint32 // records that already carried a modseq >= ours
}

// setFlagsHigher is phase 2 of setFlags: it applies the flags on the
// higher shards, registers new keywords, commits the pending change and
// restamps records whose modseq collided.
func (u *User) setFlagsHigher(mb *Mailbox, name string, fa flagsApply, higher map[string][]string, modseq uint64, chg *changeRec, updated []Meta, res *model.FlagsResult, depth int) (*model.FlagsResult, error) {
	// Phase 2: per shard, against the shard's current records. Each CAS
	// attempt computes its result from scratch; only the result of the
	// attempt that succeeded is taken over, so a conflicting first
	// attempt cannot leave a UID in Updated that the retry skipped.
	results, err := u.flagsHigherShards(fa, higher, modseq)
	if err != nil {
		return nil, fmt.Errorf("write flags shard: %w", err)
	}

	var (
		newKeywords []string
		collided    []uint32
	)

	for _, r := range results {
		updated = append(updated, r.updated...)
		res.Modified = append(res.Modified, r.modified...)
		newKeywords, _ = mergeKeywords(newKeywords, r.keywords)
		collided = append(collided, r.collided...)
	}

	// Commit with what actually changed; register keywords seen on higher shards.
	chg.UIDs = chg.UIDs[:0]
	for _, m := range updated {
		chg.UIDs = append(chg.UIDs, m.UID)
	}

	if len(newKeywords) > 0 {
		u.registerKeywords(mb, newKeywords)
	}

	if err := u.commitPending(mb, modseq, chg, nil, nil); err != nil {
		u.log.Warn("commit of pending change failed, entries are written", "mailbox", mb.Name, "modseq", modseq, "err", err)
	}

	if len(collided) > 0 && depth < 3 {
		if err := u.restampFlags(name, collided, fa.apply, depth, updated); err != nil {
			return nil, err
		}
	}

	sort.Slice(updated, func(i, j int) bool { return updated[i].UID < updated[j].UID })
	res.Updated = updated

	return res, nil
}

// flagsHigherShards applies the flags to the entries on the higher
// shards, each shard as a read-modify-write under its own version.
func (u *User) flagsHigherShards(fa flagsApply, higher map[string][]string, modseq uint64) (map[string]*flagsShardResult, error) {
	unchangedSince, apply, restamp := fa.unchangedSince, fa.apply, fa.restamp
	results := map[string]*flagsShardResult{}

	var mu sync.Mutex

	err := parallel(higher, func(oid string, keys []string) error {
		var r flagsShardResult

		err := u.applyEntries(oid, keys, func(cur map[string]Meta) map[string]Meta {
			r = flagsShardResult{}
			out := map[string]Meta{}

			for _, k := range keys {
				m, ok := cur[k]
				if !ok {
					continue
				}

				if unchangedSince != 0 && m.ModSeq > unchangedSince {
					r.modified = append(r.modified, m.UID)
					continue
				}

				nf := apply(m.Flags)
				if equalFlags(nf, m.Flags) && !restamp {
					continue
				}

				m.Flags = nf
				// The modseq was allocated in the header CAS, but a
				// concurrent STORE with a later modseq may have written
				// this record first. A message's modseq never goes
				// backwards: keep the later one for now and note the
				// collision; the record is then stamped again with a
				// modseq allocated after it (restamp), so the newer flag
				// state is visible to CHANGEDSINCE.
				if m.ModSeq >= modseq {
					r.collided = append(r.collided, m.UID)
					m.ModSeq = max(m.ModSeq, modseq)
				} else {
					m.ModSeq = modseq
				}

				out[k] = m
				r.updated = append(r.updated, m)
				r.keywords, _ = mergeKeywords(r.keywords, nf)
			}

			return out
		})
		if err != nil {
			return err
		}

		mu.Lock()
		results[oid] = &r
		mu.Unlock()

		return nil
	})

	return results, err
}

// registerKeywords adds keywords to the mailbox keyword list (best
// effort: a failure is logged).
func (u *User) registerKeywords(mb *Mailbox, newKeywords []string) {
	if _, _, err := u.casHeader(mb, func(info *MailboxInfo, _ uint64) (*headerChange, error) {
		kw, changed := mergeKeywords(info.Keywords, newKeywords)
		if !changed {
			return nil, nil
		}

		b, _ := json.Marshal(kw)

		return &headerChange{set: map[string][]byte{keyKeywords: b}}, nil
	}); err != nil {
		u.log.Warn("keyword registration failed", "err", err)
	}
}

// restampFlags stamps the collided records again with a fresh modseq and
// replaces their entries in updated (in place) by the restamped ones.
func (u *User) restampFlags(name string, collided []uint32, apply func(old []string) []string, depth int, updated []Meta) error {
	mh.casRetry("modseq_restamp", casClassMbox)

	again, err := u.setFlags(name, collided, 0, apply, true, depth+1)
	if err != nil {
		return err
	}

	fresh := map[uint32]Meta{}
	for _, m := range again.Updated {
		fresh[m.UID] = m
	}

	for i := range updated {
		if m, ok := fresh[updated[i].UID]; ok {
			updated[i] = m
		}
	}

	return nil
}

// uidOfKey parses the UID out of an index entry key ("m:<uid>"); a key
// that does not parse yields 0.
func uidOfKey(k string) uint32 {
	var uid uint32

	_, _ = fmt.Sscanf(k, keyMsgPrefix+"%d", &uid)

	return uid
}

func equalFlags(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}

	for _, f := range a {
		if !HasFlag(b, f) {
			return false
		}
	}

	return true
}

// Expunge removes the given messages from the index, records them in the
// change and vanished logs and releases the blobs of the entries that
// were actually removed (a stale snapshot may name UIDs another session
// already expunged; their references are not released twice).
func (u *User) Expunge(name string, metas []Meta) (err error) {
	end := u.trace(spanExpunge, attrCount(len(metas)))
	defer func() { end(err) }()

	mb, err := u.Lookup(name)
	if err != nil {
		return err
	}

	if len(metas) == 0 {
		return nil
	}

	if lockedMailbox(mb, "", time.Now()) {
		return ErrMailboxLocked
	}

	return u.expunge(mb, metas)
}

// ExpungeOwn implements model.OwnExpunger: Expunge without the mailbox
// lock check, for entries the caller wrote itself (the rollback of a
// delivery, the scrub's rollback of an abandoned one). A copy a
// delivery stored before the lock came would otherwise stay until the
// scrub removes it, and the client would deliver again: a duplicate.
func (u *User) ExpungeOwn(name string, metas []Meta) error {
	mb, err := u.Lookup(name)
	if err != nil {
		return err
	}

	if len(metas) == 0 {
		return nil
	}

	return u.expunge(mb, metas)
}

// expunge is Expunge after the checks. The usage is released directly
// after the removal (stage B16, temp/16 §4.3), before the blob
// references go: the window in which an entry is removed but still in
// the counters holds no blob access any more, and the removal intents
// say which section an entry is in (d without f).
func (u *User) expunge(mb *Mailbox, metas []Meta) error {
	acct, err := u.beginAccount(emailSpec(mb.ID))
	if err != nil {
		return err
	}

	res, err := u.unlink(mb, metas, "", true)
	if err != nil {
		acct.partial()
		// Phase 2 failed on a shard: what phase 1 removed is released
		// all the same (the intents of the rest stay for the scrubber).
		if len(res.removed) > 0 {
			err = errors.Join(err, u.releaseAfterUnlink(mb, res))
		}

		return err
	}

	if len(res.removed) == 0 {
		acct.withdraw()
	} else {
		acct.commit(emailChange(model.OpDestroyed, res.modseq, res.removed))
	}
	// Index no longer references the blobs; drop our reference. A failure
	// there leaves the removal intent for the scrubber, never dangling
	// index entries.
	return u.releaseAfterUnlink(mb, res)
}

// unlinkResult is what unlink did: the entries it removed, the source
// MODSEQ the removal was written at, the "r:" keys it set and the nonce
// it set them with.
type unlinkResult struct {
	removed []Meta
	modseq  uint64
	rkeys   []string
	nonce   string
}

// unlink removes index entries and writes the change/vanished records.
// Shard 0 entries are removed inside the header CAS (only those that
// still exist); higher shards in a second phase under the shard's own
// version, and the commit records what was actually removed. intentID,
// if set, is the MOVE intent to drop with the last durable step. Returns
// the entries that were removed, so callers release exactly those
// references; when phase 2 fails, the entries phase 1 removed come with
// the error (their usage is released all the same).
//
// The header CAS also records a removal intent ("r:<uid>", intents.go) for
// every entry it removes from shard 0 or announces on a higher shard;
// their keys are returned as rkeys, and the caller drops them with
// releaseRemoved once the blob references are released. The value says
// whether this removal (nonce) removed the entry: set by the header CAS
// for shard 0, by the commit of the pending change for higher shards. An
// intent that is already there is only refreshed, never overwritten:
// what another session recorded about its removal stays. released says
// that a release of the usage follows (Expunge, a MOVE into an ignored
// mailbox); otherwise the intents say at once that nothing is to be
// released (quotacapture.go).
func (u *User) unlink(mb *Mailbox, metas []Meta, intentID string, released bool) (unlinkResult, error) {
	byUID := map[uint32]Meta{}
	for _, m := range metas {
		byUID[m.UID] = m
	}

	higher := map[string][]string{}

	var extraRm []string
	if intentID != "" {
		extraRm = []string{keyIntentPrefix + intentID}
	}

	res := unlinkResult{nonce: newGUID()}

	removed, chg, modseq, rkeys, err := u.unlinkHeader(mb, metas, extraRm, higher, res.nonce, released)
	if err != nil {
		return unlinkResult{}, err
	}

	res.removed, res.modseq, res.rkeys = removed, modseq, rkeys

	if chg == nil || !chg.Pending {
		return res, nil
	}

	res.removed, err = u.unlinkHigher(mb, byUID, higher, removed, chg, modseq, extraRm, res.nonce, released)

	return res, err
}

// unlinkHeader is phase 1 of unlink: the header CAS removes the shard 0
// entries that still exist and announces the UIDs on higher shards
// (filled into higher) as pending. It returns the removed shard 0 entries
// and the change record.
func (u *User) unlinkHeader(mb *Mailbox, metas []Meta, extraRm []string, higher map[string][]string, nonce string, released bool) ([]Meta, *changeRec, uint64, []string, error) {
	var (
		removed []Meta
		chg     *changeRec
		rkeys   []string
	)

	_, modseq, err := u.casHeader(mb, func(info *MailboxInfo, _ uint64) (*headerChange, error) {
		removed, rkeys = removed[:0], rkeys[:0]

		for k := range higher {
			delete(higher, k)
		}

		shard0 := u.partitionMetas(mb, info.ShardSize, metas, higher)

		cur, existing, err := u.readUnlinkKeys(mb.ID, shard0, metas)
		if err != nil {
			return nil, err
		}

		chg = &changeRec{Expunged: true, Pending: len(higher) > 0}

		var rm []string
		for _, m := range cur {
			rm = append(rm, uidKey(m.UID))
			chg.UIDs = append(chg.UIDs, m.UID)
			removed = append(removed, m)
		}

		announceHigher(chg, higher)

		if len(removed) == 0 && len(higher) == 0 {
			if len(extraRm) > 0 {
				return &headerChange{rm: extraRm}, nil
			}

			return nil, nil
		}

		set := removalSet(time.Now().Unix(), removed, announcedMetas(metas, higher), existing, nonce, released)
		for k := range set {
			rkeys = append(rkeys, k)
		}

		hc := &headerChange{set: set, rm: rm, chg: chg}
		if !chg.Pending {
			hc.rm = append(hc.rm, extraRm...)
		}

		return hc, nil
	})

	return removed, chg, modseq, rkeys, err
}

// readUnlinkKeys reads, in one op on shard 0, the entries of the shard 0
// UIDs and the removal intents of all UIDs of a removal: the entries to
// remove in the header CAS and the intents that are already there.
func (u *User) readUnlinkKeys(id string, shard0 []uint32, metas []Meta) ([]Meta, map[string][]byte, error) {
	keys := make([]string, 0, len(shard0)+len(metas))
	for _, uid := range shard0 {
		keys = append(keys, uidKey(uid))
	}

	for i := range metas {
		keys = append(keys, removeKey(metas[i].UID))
	}

	if len(keys) == 0 {
		return nil, nil, nil
	}

	vals, err := getOmapKeys(u.idx, mboxOID(id), keys)
	if err != nil {
		return nil, nil, err
	}

	var cur []Meta

	existing := map[string][]byte{}

	for k, v := range vals {
		if strings.HasPrefix(k, keyRemovePrefix) {
			existing[k] = v
			continue
		}

		var m Meta
		if json.Unmarshal(v, &m) == nil {
			cur = append(cur, m)
		}
	}

	sort.Slice(cur, func(i, j int) bool { return cur[i].UID < cur[j].UID })

	return cur, existing, nil
}

// announcedMetas returns the entries of metas whose keys higher lists
// (the entries announced for higher shards).
func announcedMetas(metas []Meta, higher map[string][]string) []Meta {
	if len(higher) == 0 {
		return nil
	}

	want := map[string]bool{}

	for _, keys := range higher {
		for _, k := range keys {
			want[k] = true
		}
	}

	var out []Meta

	for _, m := range metas {
		if want[uidKey(m.UID)] {
			out = append(out, m)
		}
	}

	return out
}

// partitionMetas returns the UIDs of the entries that live on shard 0 of
// mb and adds the entry keys of the others to higher (shard oid -> keys).
func (u *User) partitionMetas(mb *Mailbox, shardSize uint32, metas []Meta, higher map[string][]string) []uint32 {
	var shard0 []uint32

	for _, m := range metas {
		oid := u.shardOID(mb.ID, shardOf(m.UID, shardSize))
		if oid == mboxOID(mb.ID) {
			shard0 = append(shard0, m.UID)
		} else {
			higher[oid] = append(higher[oid], uidKey(m.UID))
		}
	}

	return shard0
}

// unlinkHigher is phase 2 of unlink: it removes the entries on the higher
// shards under each shard's own version and commits what was actually
// removed (dropping the entries of extraRm with it, and recording in
// their removal intents that this removal removed them). When a shard
// fails, the entries removed so far (phase 1 and the shards that went
// through) are returned with the error.
func (u *User) unlinkHigher(mb *Mailbox, byUID map[uint32]Meta, higher map[string][]string, removed []Meta, chg *changeRec, modseq uint64,
	extraRm []string, nonce string, released bool,
) ([]Meta, error) {
	var mu sync.Mutex

	phase1 := len(removed)

	err := parallel(higher, func(oid string, keys []string) error {
		gone, err := u.modifyEntries(oid, nil, keys, false)
		if err != nil {
			return err
		}

		mu.Lock()
		defer mu.Unlock()

		for _, k := range gone {
			uid := uidOfKey(k)

			if m, ok := byUID[uid]; ok {
				removed = append(removed, m)
			}
		}

		return nil
	})
	if err != nil {
		return removed, fmt.Errorf("remove entries shard: %w", err)
	}

	chg.UIDs = chg.UIDs[:0]
	for _, m := range removed {
		chg.UIDs = append(chg.UIDs, m.UID)
	}

	now := time.Now().Unix()
	set := map[string][]byte{}

	for i := phase1; i < len(removed); i++ {
		set[removeKey(removed[i].UID)] = removalValue(now, &removed[i], true, nonce, !released)
	}

	if err := u.commitPending(mb, modseq, chg, extraRm, set); err != nil {
		u.log.Warn("commit of pending expunge failed, entries are removed", "mailbox", mb.Name, "modseq", modseq, "err", err)
	}

	sort.Slice(removed, func(i, j int) bool { return removed[i].UID < removed[j].UID })

	return removed, nil
}

// releaseAfterUnlink is what follows a removal that releases the usage
// (Expunge, the scrub's rollback): the usage goes first, directly after
// the removal, then the flag on the removal intents, then the blob
// references (and with them the intents), as before.
func (u *User) releaseAfterUnlink(mb *Mailbox, res unlinkResult) error {
	u.at(hookAfterUnlink, mb.ID)

	err := u.releaseUsage(mb, res.removed)

	u.at(hookAfterRelease, mb.ID)
	u.flagReleased(mb, res.removed, res.nonce)
	u.at(hookAfterFlag, mb.ID)
	u.releaseRemoved(mb, res.removed, res.rkeys)

	return err
}

// Copy links the messages into dest. Only the reference count on the
// blob and the destination index change; no payload is copied. The quota
// for the whole batch is reserved up front (atomically against other
// writers); what a failed batch did not use is given back.
func (u *User) Copy(metas []Meta, dest string) (copied []Meta, err error) {
	end := u.trace(spanCopy, attrCount(len(metas)))
	defer func() { end(err) }()

	mb, err := u.Lookup(dest)
	if err != nil {
		return nil, err
	}

	if err := u.checkKeywordRoom(mb, metas); err != nil {
		return nil, err
	}

	if lockedMailbox(mb, "", time.Now()) {
		return nil, ErrMailboxLocked
	}
	// The intent "qi:" names the batch for the barriers of an online
	// recount and of a mailbox lock, as "bi:" names a delivery; it goes
	// with the reservation and leaves with it (reservation.settle).
	total := sizeOf(metas)
	key := quotaIntentKey(newGUID())

	res, err := u.reserveWith(mb, total, map[string][]byte{key: newQuotaIntent(quotaIntentCopy, mb.ID, total)})
	if err != nil {
		return nil, err
	}

	res.intent = key

	acct, err := u.beginAccount(emailSpec(mb.ID))
	if err != nil {
		res.settle(Quota{})
		return nil, err
	}

	out, used, err := u.copyLinks(mb, metas, res.tag)
	if err != nil {
		acct.partial()
		res.settle(used)

		return nil, err
	}

	acct.commit(emailChange(model.OpCreated, 0, out))
	res.settle(used)

	return out, nil
}

// copyLinks takes a blob reference and links an entry into mb for each
// of metas (with the tag of the reservation); it returns the entries
// and the usage of those linked, also on an error.
func (u *User) copyLinks(mb *Mailbox, metas []Meta, tag string) ([]Meta, Quota, error) {
	out := make([]Meta, 0, len(metas))

	var used Quota

	for _, m := range metas {
		if _, err := u.blobRef(m.GUID, +1); err != nil {
			return out, used, err
		}

		nm, err := u.link(mb, relinkTagged(m, tag))
		if err != nil {
			_, _ = u.blobRef(m.GUID, -1)

			return out, used, err
		}

		out = append(out, *nm)
		used.Bytes += m.Size
		used.Msgs++
	}

	return out, used, nil
}

// moveIntent records a MOVE in progress on the source mailbox. If the
// process dies between the two index updates, the scrubber finds the
// intent and finishes the move deterministically instead of guessing from
// reference counts.
type moveIntent struct {
	ID      string   `json:"id"`
	Dest    string   `json:"dest"` // destination mailbox id
	GUIDs   []string `json:"guids"`
	UIDs    []uint32 `json:"uids"` // source UIDs
	Started int64    `json:"started"`
}

// Move relinks the messages from src to dest. The blob and its reference
// count are untouched. The two index updates are on two objects and thus
// not atomic; an intent record on the source object brackets them:
//
//  1. write intent i:<id> on src
//  2. take a blob reference and link the entry into dest (like COPY)
//  3. unlink entries from src; the intent is removed together with the
//     last durable step (in the header CAS, or in the commit of the
//     pending change when higher shards are involved)
//  4. release the references of the entries step 3 actually removed
//
// The destination holds its own reference from step 2 on, so an EXPUNGE
// of the source that runs between 2 and 3 cannot take the blob's last
// reference away under the destination entry; it merely makes step 3
// remove nothing. A message whose blob is already gone (ErrGone) has been
// expunged and is skipped. A crash after 2 leaves the message in both
// mailboxes plus the intent; the scrubber completes steps 3 and 4. A
// crash after 1 leaves only the intent; the scrubber checks dest for the
// GUIDs and finishes either way. A crash between 2's reference and its
// link is a refcount too high, which the scrubber's refcount check fixes.
func (u *User) Move(src string, metas []Meta, dest string) (moved []Meta, err error) {
	end := u.trace(spanMove, attrCount(len(metas)))
	defer func() { end(err) }()

	smb, err := u.Lookup(src)
	if err != nil {
		return nil, err
	}

	dmb, err := u.Lookup(dest)
	if err != nil {
		return nil, err
	}

	if err := u.checkKeywordRoom(dmb, metas); err != nil {
		return nil, err
	}

	if now := time.Now(); lockedMailbox(smb, "", now) || lockedMailbox(dmb, "", now) {
		return nil, ErrMailboxLocked
	}

	res, err := u.reserveMove(smb, dmb, metas)
	if err != nil {
		return nil, err
	}
	// Both mailboxes are announced together: the log must not end up
	// holding the removal without the arrival, or the other way round.
	acct, err := u.beginAccount(emailSpec(dmb.ID), emailSpec(smb.ID))
	if err != nil {
		res.settle(Quota{})
		return nil, err
	}

	mv, err := u.moveLinks(smb, dmb, metas, res.tag)
	if err != nil {
		acct.partial()
		// Once the intent is written the scrubber completes the move
		// into dest, so the reservation stays except for entries that
		// were expunged meanwhile.
		if mv.started {
			res.settle(Quota{Bytes: res.total.Bytes - mv.skipped.Bytes, Msgs: res.total.Msgs - mv.skipped.Msgs})
		} else {
			res.settle(Quota{})
		}

		return nil, err
	}

	acct.commit(
		emailChange(model.OpCreated, 0, mv.out),
		emailChange(model.OpDestroyed, mv.modseq, mv.removed),
	)
	res.settle(sizeOf(mv.out))

	return mv.out, nil
}

// reserveMove reserves the usage of a MOVE. A move between two counted
// (or two ignored) mailboxes leaves the usage alone. Out of an ignored
// mailbox into a counted one the batch is reserved like a COPY (with an
// intent, as a COPY); into an ignored one the removed source entries
// are released afterwards.
func (u *User) reserveMove(smb, dmb *Mailbox, metas []Meta) (*reservation, error) {
	var (
		want    Quota
		intents map[string][]byte
		key     string
	)

	if !u.rules.Counts(smb) && u.rules.Counts(dmb) {
		want = sizeOf(metas)
		key = quotaIntentKey(newGUID())
		intents = map[string][]byte{key: newQuotaIntent(quotaIntentMove, dmb.ID, want)}
	}

	res, err := u.reserveWith(dmb, want, intents)
	if err != nil {
		return nil, err
	}

	res.intent = key

	return res, nil
}

// moveResult is what moveLinks did: the entries linked into dest, the
// size of the source entries it removed and of those it skipped because
// another session expunged them meanwhile, and whether the intent was
// written.
type moveResult struct {
	out            []Meta
	removed        []Meta
	freed, skipped Quota
	// modseq is the source MODSEQ the removal was written at.
	modseq  uint64
	started bool
}

// moveLinks runs the intent, link, unlink and release steps of Move. tag
// is the online recount that captured the reservation of a move out of
// an ignored mailbox ("" none); a move between two counted mailboxes
// goes through the gate (moveGate) after its intent, which captures it
// when a run is live. Into an ignored mailbox the usage of the removed
// source entries is released directly after the removal, before the
// blob references go (stage B16, temp/16 §4.3).
func (u *User) moveLinks(smb, dmb *Mailbox, metas []Meta, tag string) (moveResult, error) {
	var mv moveResult

	intentID, err := u.writeMoveIntent(smb, dmb, metas)
	if err != nil {
		return mv, err
	}

	mv.started = true

	counted := u.rules.Counts(smb) && u.rules.Counts(dmb)

	if counted {
		gateTag, err := u.moveGate(smb, dmb, metas)
		if err != nil {
			// Nothing is linked yet: the intent goes, the move is off.
			_, uerr := u.unlink(smb, nil, intentID, false)

			return moveResult{}, errors.Join(err, uerr)
		}

		tag = gateTag
	}

	skipped, err := u.moveLinkAll(dmb, metas, tag, &mv)
	if err != nil {
		return mv, err // intent stays; scrubber reconciles
	}

	if counted && tag != "" && len(skipped) > 0 {
		u.moveGateSkipped(tag, skipped)
	}

	return mv, u.moveUnlink(smb, dmb, metas, intentID, &mv)
}

// writeMoveIntent writes the MOVE intent on the source mailbox and
// returns its id.
func (u *User) writeMoveIntent(smb, dmb *Mailbox, metas []Meta) (string, error) {
	intent := moveIntent{ID: newGUID(), Dest: dmb.ID, Started: time.Now().Unix()}
	for _, m := range metas {
		intent.GUIDs = append(intent.GUIDs, m.GUID)
		intent.UIDs = append(intent.UIDs, m.UID)
	}

	ib, _ := json.Marshal(&intent)

	if err := casOmap(u, u.idx, mboxOID(smb.ID), nil, func(_ map[string][]byte) (map[string][]byte, []string) {
		return map[string][]byte{keyIntentPrefix + intent.ID: ib}, nil
	}); err != nil {
		return "", fmt.Errorf("move intent: %w", err)
	}

	return intent.ID, nil
}

// moveUnlink is the last step of moveLinks: the source entries are
// unlinked, their usage released when the destination is ignored, then
// their blob references.
func (u *User) moveUnlink(smb, dmb *Mailbox, metas []Meta, intentID string, mv *moveResult) error {
	released := u.rules.Counts(smb) && !u.rules.Counts(dmb)

	res, err := u.unlink(smb, metas, intentID, released)
	if err != nil {
		return err
	}

	mv.modseq = res.modseq
	mv.removed = res.removed
	mv.freed = sizeOf(res.removed)

	u.at(hookAfterUnlink, smb.ID)

	if released {
		if err := u.moveUsage(smb, dmb, res.removed); err != nil {
			u.log.Warn("quota release after move failed", "err", err)
		}

		u.flagReleased(smb, res.removed, res.nonce)
	}

	u.releaseRemoved(smb, res.removed, res.rkeys)

	return nil
}

// moveLinkAll takes a blob reference and links an entry into dmb for
// each of metas; entries whose blob is gone (expunged meanwhile) are
// skipped and returned.
func (u *User) moveLinkAll(dmb *Mailbox, metas []Meta, tag string, mv *moveResult) ([]Meta, error) {
	mv.out = make([]Meta, 0, len(metas))

	var skipped []Meta

	for _, m := range metas {
		if _, err := u.blobRef(m.GUID, +1); err != nil {
			if errors.Is(err, ErrGone) {
				// expunged meanwhile; the source unlink removes nothing
				mv.skipped.Bytes += m.Size
				mv.skipped.Msgs++
				skipped = append(skipped, m)

				continue
			}

			return skipped, err
		}

		// The keyword limit was checked before the intent (Move); from
		// here on the move completes, as the scrubber would complete it.
		nm, err := u.linkAs(dmb, relinkTagged(m, tag), false)
		if err != nil {
			_, _ = u.blobRef(m.GUID, -1)
			return skipped, err
		}

		mv.out = append(mv.out, *nm)
	}

	return skipped, nil
}

// sizeOf sums the sizes and the number of the entries.
func sizeOf(metas []Meta) Quota {
	var q Quota
	for _, m := range metas {
		q.Bytes += m.Size
		q.Msgs++
	}

	return q
}

// ---------------------------------------------------------------------------
// Delivery intents
//
// A delivery that stores one message into several mailboxes (Sieve with
// several fileinto actions) or several messages in one command
// (MULTIAPPEND) is all-or-nothing for the client, but consists of
// several independent index updates. The intent on the registry object
// records what has been stored so far; the caller removes it when the
// delivery is complete. An intent that outlives deliveryIntentMaxAge
// belongs to a crashed process and the scrubber rolls it back (the LMTP
// client or IMAP client never got a success and retries).

// deliveryIntentMaxAge is how old an intent must be before the scrubber
// treats it as abandoned.
const deliveryIntentMaxAge = 10 * time.Minute

type deliveryIntent struct {
	ID      string          `json:"id"`
	Started int64           `json:"started"`
	Entries []deliveryEntry `json:"entries"`
}

type deliveryEntry struct {
	MailboxID string `json:"mbox"`    // the mailbox as it was at delivery time
	Mailbox   string `json:"mailbox"` // its name then (for logs)
	GUID      string `json:"guid"`    // the message; recorded before it is stored
}

// BeginDelivery records a new intent and returns its id.
func (u *User) BeginDelivery() (string, error) {
	in := deliveryIntent{ID: newGUID(), Started: time.Now().Unix()}
	b, _ := json.Marshal(&in)

	err := casOmap(u, u.idx, oidRegistry, nil, func(_ map[string][]byte) (map[string][]byte, []string) {
		return map[string][]byte{regDeliveryPrefix + in.ID: b}, nil
	})
	if err != nil {
		return "", fmt.Errorf("delivery intent: %w", err)
	}

	return in.ID, nil
}

// RecordDelivery adds the message about to be stored to the intent
// (id "" is a no-op). The mailbox is recorded by id, so a rollback never
// touches a mailbox that was deleted and recreated under the same name.
func (u *User) RecordDelivery(id, mailbox, guid string) error {
	if id == "" {
		return nil
	}

	mb, err := u.Lookup(mailbox)
	if err != nil {
		return err
	}

	key := regDeliveryPrefix + id

	return casOmap(u, u.idx, oidRegistry, []string{key}, func(cur map[string][]byte) (map[string][]byte, []string) {
		var in deliveryIntent
		if json.Unmarshal(cur[key], &in) != nil || in.ID == "" {
			in = deliveryIntent{ID: id, Started: time.Now().Unix()}
		}

		in.Entries = append(in.Entries, deliveryEntry{MailboxID: mb.ID, Mailbox: mailbox, GUID: guid})
		b, _ := json.Marshal(&in)

		return map[string][]byte{key: b}, nil
	})
}

// EndDelivery removes the intent (id "" is a no-op), together with the
// blob intents of the deliveries it covered (intents.go).
func (u *User) EndDelivery(id string) error {
	if id == "" {
		return nil
	}

	done := u.takeIntents()

	err := casOmap(u, u.idx, oidRegistry, nil, func(_ map[string][]byte) (map[string][]byte, []string) {
		return nil, append([]string{regDeliveryPrefix + id}, done...)
	})
	if err != nil {
		u.restoreIntents(done)
	}

	return err
}

// deliveryIntents lists the recorded intents (scrubber).
func (u *User) deliveryIntents() ([]deliveryIntent, error) {
	vals, err := u.idx.GetAllOmapValues(oidRegistry, "", regDeliveryPrefix, omapPageSize)
	if err != nil {
		if errCode(err) == syscall.ENOENT {
			return nil, nil
		}

		return nil, err
	}

	var out []deliveryIntent

	for _, v := range vals {
		var in deliveryIntent
		if json.Unmarshal(v, &in) == nil && in.ID != "" {
			out = append(out, in)
		}
	}

	return out, nil
}

// pendingIntents lists the MOVE intents recorded on a mailbox.
func (u *User) pendingIntents(id string) ([]moveIntent, error) {
	vals, err := u.idx.GetAllOmapValues(mboxOID(id), "", keyIntentPrefix, omapPageSize)
	if err != nil {
		if errCode(err) == syscall.ENOENT {
			return nil, nil
		}

		return nil, err
	}

	var out []moveIntent

	for _, v := range vals {
		var in moveIntent
		if json.Unmarshal(v, &in) == nil {
			out = append(out, in)
		}
	}

	return out, nil
}

// relink prepares the entry of a copied or moved message. An imported
// POP3 UIDL stays with the source entry: it must stay unique per maildrop.
func relink(m Meta) Meta {
	m.UID, m.ModSeq, m.POP3UIDL, m.QuotaTag = 0, 0, "", ""
	m.SaveDate = time.Now().UTC()

	return m
}

// relinkTagged is relink with the tag of the online recount that
// captured the copy or move ("" none): the copy's own tag, never the
// source's, which a run that never captured the copy may have set.
func relinkTagged(m Meta, tag string) Meta {
	m = relink(m)
	m.QuotaTag = tag

	return m
}

// ---------------------------------------------------------------------------
// Maildrop lock (POP3)

// Lock takes the exclusive maildrop lock of a mailbox (RADOS advisory
// lock on shard 0, auto-expiring after ttl) and renews it every ttl/2
// until the returned function is called. ErrLocked if held elsewhere.
// The returned lease reports whether a renewal failed (the session must
// then stop mutating).
func (u *User) Lock(id string, ttl time.Duration) (model.Lease, error) {
	return u.lockNamed(id, lockName, ttl)
}

// Internal event commit coordination waits briefly for normal concurrent commands.
func (u *User) lockLearning(id string) (model.Lease, error) {
	deadline := time.Now().Add(10 * time.Second)

	for {
		l, err := u.lockNamed(id, "imapsieve-link", 2*time.Minute)
		if !errors.Is(err, ErrLocked) || !time.Now().Before(deadline) {
			return l, err
		}

		time.Sleep(20 * time.Millisecond)
	}
}
func (u *User) lockNamed(id, lockName string, ttl time.Duration) (model.Lease, error) {
	cookie := newGUID()

	take := func(flags *byte) error {
		ret, err := u.idx.LockExclusive(mboxOID(id), lockName, cookie, "maildrop", ttl, flags)
		if err != nil {
			return err
		}

		if ret != 0 {
			if syscall.Errno(-ret) == syscall.EBUSY {
				return ErrLocked
			}

			return fmt.Errorf("lock: ret=%d", ret)
		}

		return nil
	}
	if err := take(nil); err != nil {
		return nil, err
	}

	l := &lease{stop: make(chan struct{}), done: make(chan struct{})}

	u.inflight.Add(1)
	go func() {
		defer u.inflight.Done()
		defer close(l.done)

		renew := byte(1) // LIBRADOS_LOCK_FLAG_MAY_RENEW

		t := time.NewTicker(ttl / 2)
		defer t.Stop()

		for {
			select {
			case <-l.stop:
				return
			case <-t.C:
				if err := take(&renew); err != nil {
					u.log.Warn("maildrop lock renewal failed", "mailbox", id, "err", err)
					l.lost.Store(true)

					return
				}
			}
		}
	}()

	l.unlock = func() { _, _ = u.idx.Unlock(mboxOID(id), lockName, cookie) }

	return l, nil
}

type lease struct {
	stop chan struct{}
	// done is closed when the renewal goroutine ended (nil: none).
	done   chan struct{}
	once   sync.Once
	lost   atomic.Bool
	unlock func()
}

func (l *lease) Lost() bool { return l.lost.Load() }

// Release stops the renewal, waits until a renewal in flight is over (it
// would otherwise take the lock again after the unlock) and unlocks.
func (l *lease) Release() {
	l.once.Do(func() {
		close(l.stop)

		if l.done != nil {
			<-l.done
		}

		l.unlock()
	})
}

// errCode extracts the errno of a go-ceph error (0 if none).
func errCode(err error) syscall.Errno {
	var ec interface{ ErrorCode() int }
	if errors.As(err, &ec) {
		c := ec.ErrorCode()
		if c < 0 {
			c = -c
		}

		return syscall.Errno(c)
	}

	return 0
}

// ListSubscribedMissing implements model.User.
func (u *User) ListSubscribedMissing() ([]string, error) {
	vals, err := u.idx.GetAllOmapValues(oidRegistry, "", regSubMissing, omapPageSize)
	if err != nil {
		if errCode(err) == syscall.ENOENT {
			return nil, nil
		}

		return nil, err
	}

	out := make([]string, 0, len(vals))
	for k := range vals {
		out = append(out, strings.TrimPrefix(k, regSubMissing))
	}

	sort.Strings(out)

	return out, nil
}

// MarkRecent implements model.User: the \Recent watermark only moves
// forward.
func (u *User) MarkRecent(id string, uidNext uint32) error {
	return casOmap(u, u.idx, mboxOID(id), []string{keyRecent}, func(cur map[string][]byte) (map[string][]byte, []string) {
		have, _ := strconv.ParseUint(string(cur[keyRecent]), 10, 32)
		if uint32(have) >= uidNext {
			return nil, nil
		}

		return map[string][]byte{keyRecent: []byte(strconv.FormatUint(uint64(uidNext), 10))}, nil
	})
}

// SetLearningHook is scoped to one protocol command on this per-session view.
func (u *User) SetLearningHook(h func(model.Mailbox, model.Meta) (string, error)) { u.learningHook = h }
