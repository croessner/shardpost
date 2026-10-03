package model

import (
	"errors"
	"hash/fnv"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Layouts of the full-text index (stage B14b). Layout 2 is the fixed set
// of shard objects every version before 0.45.0 writes ("fts/<nn>",
// "ftst/<nnn>", "ftsh/<nn>"); layout 3 keeps an index generation of S
// posting, D dictionary and H header objects ("ftx/<gen>/p<i>", "d<j>",
// "h<k>") whose counts follow the account's size. The layout record in
// the account's id map says which one an account has.
const (
	FTSLayout2 = 2
	FTSLayout3 = 3
)

// FTSLayoutMarkKey is the admin store record whose presence allows
// layout 3 in the cluster ("shardpostctl fts layout3 enable"). It is read
// without a cache wherever a layout 3 index is created, so removing it
// stops new layout 3 indexes at once.
const FTSLayoutMarkKey = "fts/layout3"

// EffectiveFTSLayout is the layout a new full-text index gets: layout 3
// only when fts.layout is 3 and the cluster mark is set, layout 2 in
// every other case (fts.layout 2, or 3 without the mark). The built-in
// default of fts.layout is 3 since 0.46.0, so it is the mark that
// switches a cluster to layout 3.
func EffectiveFTSLayout(configured int, marked bool) int {
	if configured == FTSLayout3 && marked {
		return FTSLayout3
	}

	return FTSLayout2
}

// FTSLayoutMark is the value of FTSLayoutMarkKey.
type FTSLayoutMark struct {
	EnabledAt time.Time `json:"enabled_at"`
	Principal string    `json:"principal,omitempty"`
	Pods      int       `json:"pods"`
	Forced    bool      `json:"forced,omitempty"`
}

// Server registrations (stage B14b): every server writes
// "servers/<instance>" at startup and then every ServerHeartbeat, so
// that "fts layout3 enable --pods n" can tell when every running server
// knows layout 3.
const (
	ServersPrefix   = "servers/"
	ServerHeartbeat = time.Minute
	// ServerFresh is how recent a registration must be to count as a
	// running server.
	ServerFresh = 3 * time.Minute
	// ServerExpiry is how long a registration that is not renewed is
	// kept.
	ServerExpiry = 24 * time.Hour
	// CapFTSLayout3 is the capability of a server that reads and writes
	// layout 3.
	CapFTSLayout3 = "fts_layout3"
	// CapSystemPool is the capability of a process that knows the system
	// pool (ceph.system_pool, 0.47.0): it routes the store's own
	// namespaces by it and checks the placement fence and mark.
	CapSystemPool = "system_pool"
)

// Roles of a registration: what the registered process is. Every
// process that writes the store's own namespaces registers for its
// whole runtime, so that a migration of the system state can tell that
// nothing runs any more. Records before 0.47.0 have no role; they are
// servers. The system tools do not register here: their heartbeat would
// change the very records a migration copies.
const (
	RoleServe    = "serve"
	RolePush     = "push"
	RoleScrub    = "scrub"
	RoleBackfill = "backfill"
	RoleSweep    = "sweep"
)

// PoolRef names a pool by its id (what placement checks compare: a pool
// can be renamed) and by its name (for messages).
type PoolRef struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
}

// ServerRecord is the value of a server registration.
type ServerRecord struct {
	Version string    `json:"version"`
	Caps    []string  `json:"caps"`
	Seen    time.Time `json:"seen"`
	// Role is what the process is (RoleServe, ...); empty in records
	// before 0.47.0, which are servers.
	Role string `json:"role,omitempty"`
	// SystemPool is the system pool the process works with (nil: the
	// store's own namespaces live in the index pool).
	SystemPool *PoolRef `json:"system_pool,omitempty"`
	// StartedAt is when the process started; StoppedAt when it ended in
	// order. A process that crashed leaves StoppedAt zero; its record
	// stops counting as running ServerFresh after its last heartbeat.
	StartedAt time.Time `json:"started_at,omitzero"`
	StoppedAt time.Time `json:"stopped_at,omitzero"`
}

// IsServe reports whether the record is the one of a server ("shardpost
// serve"): role serve, or no role (a record before 0.47.0).
func (r ServerRecord) IsServe() bool { return r.Role == "" || r.Role == RoleServe }

// Running reports whether the record counts as a running process at
// now: seen within ServerFresh and not stopped.
func (r ServerRecord) Running(now time.Time) bool {
	return now.Sub(r.Seen) <= ServerFresh && r.StoppedAt.IsZero()
}

// HasCap reports whether the server registered the capability.
func (r ServerRecord) HasCap(c string) bool {
	for _, x := range r.Caps {
		if x == c {
			return true
		}
	}

	return false
}

// The account-wide indexes of the layouts, in AccountsNamespace.
const (
	// FTSLayoutIndexName is the presence index of the accounts that have
	// a layout 3 index ("ix.fts_layout/<nn>": account -> {"since"}). An
	// account is entered before its layout 3 objects are created and
	// removed after its index went back to layout 2, so an empty index
	// means that no account needs layout 3 any more.
	FTSLayoutIndexName = "fts_layout"
	// FTSLayoutIndexShards applies to a presence index its first writer
	// creates; an existing one keeps the count of its head. 256 since
	// 0.47.0 (16 before): a million accounts with layout 3 are about
	// 4000 keys per shard.
	FTSLayoutIndexShards = 256
	// FTSDueQueue holds the accounts whose index outgrew its shard target
	// ("ix.fts_due"): id the account, gen FTSDueGen of the generation
	// that outgrew it. The fts_due worker alone removes entries.
	FTSDueQueue = "fts_due"
)

// FTSLayoutEntry is the value of an account in the presence index.
type FTSLayoutEntry struct {
	Since time.Time `json:"since"`
}

// FTSDueGen is the queue generation of an index generation: its FNV-64a
// hash, so that an entry names the generation it was written for and the
// key stays parsable by ParseDueKey.
func FTSDueGen(gen string) uint64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(gen))

	return h.Sum64()
}

// FTSDueRecordPrefix is the admin store collection of the records the
// fts_due worker keeps per account: the job it planned for it.
const FTSDueRecordPrefix = "fts_due/"

// FTSDueRecordKey is the admin store key of the account's record.
func FTSDueRecordKey(account string) string { return FTSDueRecordPrefix + url.PathEscape(account) }

// FTSDueRecord is the value of FTSDueRecordKey.
type FTSDueRecord struct {
	JobID string `json:"job_id"`
	Gen   string `json:"gen"`
	// NoopAt is when the planned job found nothing to do: the worker
	// leaves the account alone for FTSDueNoopBackoff after it.
	NoopAt time.Time `json:"noop_at,omitzero"`
	// Waits counts the times the worker postponed the entry for this job
	// (the backoff doubles with each).
	Waits int `json:"waits,omitempty"`
}

// FTSDueBackoff is the first backoff of the fts_due worker; it doubles
// with every look up to FTSDueNoopBackoff.
const FTSDueBackoff = 5 * time.Minute

// FTSDueBackoffAfter is the backoff after n looks.
func FTSDueBackoffAfter(n int) time.Duration {
	d := FTSDueBackoff
	for range min(max(n, 0), 16) {
		d *= 2
		if d >= FTSDueNoopBackoff {
			return FTSDueNoopBackoff
		}
	}

	return d
}

// FTSDueNoopBackoff is how long the worker waits after a reshard that
// found nothing to do, and the longest backoff between two looks.
const FTSDueNoopBackoff = 6 * time.Hour

// FTSLayoutRequest is one run of a layout conversion or reshard.
type FTSLayoutRequest struct {
	// Layout is the target layout (2 or 3; 0 keeps the account's).
	Layout int
	// Shards forces the object counts of a layout 3 target (0: from the
	// key counts and fts.shard_target_keys).
	Shards int
	// DueOnly ends without a change when the counts the targets come out
	// at are the ones the index has.
	DueOnly bool
	// Tok identifies the run: "job-<id>/<lease generation>" for a job,
	// "<session>/<nonce>" for the import reshard. A run continues an
	// unfinished conversion only of exactly the same token.
	Tok string
	// Fence is called before every mutation phase and before every batch
	// of writes; an error ends the run.
	Fence func() error
	// Progress reports the phase and the progress inside it (optional).
	Progress func(phase string, done, total int)
}

// FTSLayoutShape is one generation of the index.
type FTSLayoutShape struct {
	Layout int    `json:"layout"`
	Gen    string `json:"gen,omitempty"`
	S      int    `json:"s,omitempty"`
	D      int    `json:"d,omitempty"`
	H      int    `json:"h,omitempty"`
	// Capped names the kinds of objects (postings, dictionary, headers)
	// whose count a cap cut, in the target FTSLayoutDue reports.
	Capped []string `json:"capped,omitempty"`
}

// FTSLayoutReport is what a conversion did.
type FTSLayoutReport struct {
	From FTSLayoutShape `json:"from"`
	To   FTSLayoutShape `json:"to"`
	// Noop is set when nothing was changed (Reason says why).
	Noop    bool   `json:"noop,omitempty"`
	Reason  string `json:"reason,omitempty"`
	Resumed bool   `json:"resumed,omitempty"`
	// Cleared names a foreign unfinished conversion that was removed.
	Cleared string `json:"cleared,omitempty"`
	Terms   int64  `json:"terms"`
	Dict    int64  `json:"dict_keys"`
	Headers int64  `json:"headers"`
	Docs    int64  `json:"docs"`
}

// FTSLayoutState is the layout record of an account as an operator sees
// it.
type FTSLayoutState struct {
	Current FTSLayoutShape `json:"current"`
	// Next is the conversion in progress, if any.
	Next *FTSLayoutShape `json:"next,omitempty"`
	// NextTok and NextAt identify it (NextAt: its last heartbeat, for a
	// conversion of the import reshard endpoint).
	NextTok string    `json:"next_tok,omitempty"`
	NextAt  time.Time `json:"next_at,omitzero"`
	// Old are source generations a conversion has not removed yet, OldToks
	// the runs that left them and OldAt their last heartbeats (zero for a
	// job), in the same order.
	Old     []FTSLayoutShape `json:"old,omitempty"`
	OldToks []string         `json:"old_toks,omitempty"`
	OldAt   []time.Time      `json:"old_at,omitempty"`
}

// FTSLayouter is implemented by user views whose full-text index has
// layouts (stage B14b).
type FTSLayouter interface {
	// FTSLayout reads the layout record (one read).
	FTSLayout() (FTSLayoutState, error)
	// FTSLayoutDue reports the targets the key counts ask for, without a
	// change (S + D + H + 1 reads), and whether they differ from the
	// current shape.
	FTSLayoutDue() (target FTSLayoutShape, due bool, err error)
	// ConvertFTSLayout converts or reshards the index; the caller holds
	// the account's maintenance lock.
	ConvertFTSLayout(req FTSLayoutRequest) (FTSLayoutReport, error)
	// ClearFTSLayoutNext removes the unfinished conversion of tok
	// exactly: the freeze it set is lifted, its record removed, its
	// target objects deleted. false when no conversion of tok is there.
	ClearFTSLayoutNext(tok string) (bool, error)
	// ClearForeignFTSLayoutNext is ClearFTSLayoutNext for a conversion
	// of any token but own; it returns the token it cleared ("" for
	// none). Whoever takes the account lock calls it.
	ClearForeignFTSLayoutNext(own string) (string, error)
	// ClearFTSLayoutOld removes the sources the run tok left behind after
	// its switch (a crash between P6 and P7): they are deleted and the
	// record's "old" entry goes. false when there is none of tok.
	ClearFTSLayoutOld(tok string) (bool, error)
	// HeartbeatFTSLayoutNext records a heartbeat of the run tok in its
	// unfinished conversion (the import reshard endpoint), after its
	// switch in the sources it has still to remove; false when the
	// record carries no entry of tok.
	HeartbeatFTSLayoutNext(tok string, at time.Time) (bool, error)
}

// FTSLayoutMarked is implemented by stores that read the cluster mark.
type FTSLayoutMarked interface {
	FTSLayout3Enabled() (bool, error)
}

// ErrIndexUnavailable is wrapped by the errors of a full-text index
// that cannot be used now: an object of the generation its layout record
// names is missing (repair: fts_rebuild), a conversion holds it frozen
// longer than fts.frozen_wait_seconds, its generation changed too often.
// IMAP answers NO [UNAVAILABLE], LMTP a temporary failure.
var ErrIndexUnavailable = errors.New("full-text index temporarily unavailable")

// ErrFTSLayoutMark refuses a conversion to layout 3 without the cluster
// mark.
var ErrFTSLayoutMark = errors.New("layout 3 is not enabled in this cluster (shardpostctl fts layout3 enable)")

// ErrFTSDictLayout3 refuses fts_dict_convert for an account whose index
// has layout 3: the job is a legacy kind for layout 2 indexes written
// before 0.35.0, and layout 3 always has its dictionary.
var ErrFTSDictLayout3 = errors.New("the account's full-text index has layout 3, which needs no dictionary conversion " +
	"(fts_dict_convert is a legacy job for layout 2 indexes written before 0.35.0)")

// ImportHint is what an importer knows of the account before it imports
// it: the number of messages it is going to store, from which the store
// sizes a fresh full-text index.
type ImportHint struct {
	Messages int `json:"messages"`
}

// ImportHinter is implemented by user views that size a fresh index from
// an import hint.
type ImportHinter interface {
	SetImportHint(h ImportHint)
}

// JobLockToken is the token of a job run: the job id and the generation
// of its lease (see FTSLayoutRequest.Tok).
func JobLockToken(session string, leaseGen uint64) string {
	return session + "/" + strconv.FormatUint(leaseGen, 10)
}

// TokenSession returns the lock session a conversion token belongs to
// (the part before the last slash).
func TokenSession(tok string) string {
	if i := strings.LastIndexByte(tok, '/'); i > 0 {
		return tok[:i]
	}

	return tok
}
