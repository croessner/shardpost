// Package model holds the data types shared between the RADOS store and
// the IMAP protocol layer. It is free of cgo so the protocol layer can be
// unit-tested against an in-memory backend without librados.
package model

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"strings"
	"time"

	"github.com/emersion/go-imap/v2"
)

// Delim is the mailbox hierarchy delimiter.
const Delim = "/"

var (
	// ErrNoMailbox is returned when a mailbox does not exist.
	ErrNoMailbox = errors.New("no such mailbox")
	// ErrMailboxExists is returned by CreateMailbox for an existing mailbox.
	ErrMailboxExists = errors.New("mailbox already exists")
	// ErrTooLarge is returned for messages above the size limit.
	ErrTooLarge = errors.New("message too large")
	// ErrOverQuota is returned when a delivery would exceed the user's quota.
	ErrOverQuota = errors.New("quota exceeded")
	// ErrLocked is returned when the maildrop lock is held by someone else.
	ErrLocked = errors.New("mailbox is locked")
	// ErrNoScript is returned for an unknown Sieve script.
	ErrNoScript = errors.New("no such script")
	// ErrScriptActive is returned when deleting the active Sieve script.
	ErrScriptActive = errors.New("script is active")
	// ErrScriptExists is returned when renaming onto an existing script.
	ErrScriptExists = errors.New("script exists")
)

// MailboxRec is the registry entry of a mailbox.
type MailboxRec struct {
	ID          string   `json:"id"`
	UIDValidity uint32   `json:"uidvalidity"`
	Subscribed  bool     `json:"subscribed,omitempty"`
	SpecialUse  []string `json:"specialuse,omitempty"`
	// ACL maps an identifier (account name or "anyone") to RFC 4314
	// rights. The owner has all rights implicitly and is not listed.
	ACL map[string]string `json:"acl,omitempty"`
	// ACLGen counts ACL changes; the grantees' pointers carry the
	// generation they were derived from, so a late pointer write from
	// an older SETACL/DELETEACL cannot undo a newer one.
	ACLGen uint64 `json:"aclgen,omitempty"`
	// SortOrder is where a client puts the mailbox in a list of its
	// siblings, lowest first (JMAP Mailbox.sortOrder, RFC 8621 2). IMAP
	// has no such thing and never reads it; it is stored here because
	// it belongs to the mailbox, not to a session.
	SortOrder uint32 `json:"sortorder,omitempty"`
}

// SharedRef is a pointer in a user's registry to a mailbox of another
// user that granted rights (written by SetACL on the owner's side).
type SharedRef struct {
	Owner      string
	Mailbox    string
	Subscribed bool
	Gen        uint64 // ACL generation the pointer was written for (0: legacy)
}

// AllRights are the RFC 4314 rights, in canonical order.
const AllRights = "lrswipkxtea"

// NormalizeRights validates a rights string and returns it in canonical
// order; the obsolete "c" and "d" (RFC 2086) expand to "k" and "xte".
func NormalizeRights(r string) (string, bool) {
	set := map[rune]bool{}

	for _, ch := range r {
		switch ch {
		case 'c':
			set['k'] = true
		case 'd':
			set['x'], set['t'], set['e'] = true, true, true
		default:
			if !strings.ContainsRune(AllRights, ch) {
				return "", false
			}

			set[ch] = true
		}
	}

	var b strings.Builder

	for _, ch := range AllRights {
		if set[ch] {
			b.WriteRune(ch)
		}
	}

	return b.String(), true
}

// HasRight reports whether rights contains r.
func HasRight(rights string, r byte) bool { return strings.IndexByte(rights, r) >= 0 }

// Mailbox is a registry entry with its name.
type Mailbox struct {
	Name string
	MailboxRec
}

// Meta is the per-message index record. Besides the location of the blob
// it caches everything a client list view needs, so FETCH (FLAGS ENVELOPE
// INTERNALDATE RFC822.SIZE), SORT and THREAD do not read the blob.
type Meta struct {
	UID          uint32         `json:"uid"`
	GUID         string         `json:"guid"`
	Size         int64          `json:"size"`
	Chunks       int            `json:"chunks"`
	Flags        []string       `json:"flags,omitempty"`
	InternalDate time.Time      `json:"idate"`
	SaveDate     time.Time      `json:"sdate"`
	ModSeq       uint64         `json:"modseq"`
	Envelope     *imap.Envelope `json:"env,omitempty"`
	References   []string       `json:"refs,omitempty"` // Message-IDs from References/In-Reply-To
	Preview      string         `json:"preview,omitempty"`
	// HasAttachment is JMAP's hasAttachment (RFC 8621 4.1.4), computed
	// when the message is stored (mailmeta.HasAttachment). Nil in a
	// record written before the field existed: unknown, not false.
	HasAttachment *bool `json:"att,omitempty"`
	// POP3UIDL is the POP3 unique-id kept from an imported source; ""
	// means the GUID (see UIDL).
	POP3UIDL string `json:"pop3uidl,omitempty"`
}

// UIDL is the POP3 unique-id of the message: the imported one if any,
// otherwise the GUID.
func (m *Meta) UIDL() string {
	if m.POP3UIDL != "" {
		return m.POP3UIDL
	}

	return m.GUID
}

// MailboxInfo is the header of a mailbox index object.
type MailboxInfo struct {
	Mailbox
	// RecentFrom is the \Recent watermark: messages with UID >= RecentFrom
	// have not been seen by a SELECT yet (0 = all messages). SELECT moves
	// it to UIDNext after reporting.
	RecentFrom    uint32
	UIDNext       uint32
	HighestModSeq uint64
	LogStart      uint64 // oldest modseq the change log still covers
	ShardSize     uint32 // UIDs per index shard, fixed at creation
	Keywords      []string
}

// Change is one change log record: the UIDs touched at a modseq. Pending
// means the writer has announced the change but not yet written all
// entries; readers must not skip past it yet.
type Change struct {
	ModSeq   uint64
	UIDs     []uint32
	Expunged bool
	Pending  bool
}

// Quota is a storage usage or limit (0 = unlimited for limits).
type Quota struct {
	Bytes int64
	Msgs  int64
}

// FlagsResult is what SetFlags returns.
type FlagsResult struct {
	Updated  []Meta   // records whose flags changed, with their new modseq
	Modified []uint32 // UIDs skipped because of UNCHANGEDSINCE
}

// Lease is a held maildrop lock.
type Lease interface {
	// Lost reports whether the lock could not be renewed; the holder must
	// stop mutating the mailbox.
	Lost() bool
	Release()
}

// Watch is a live subscription to changes of one mailbox.
type Watch interface {
	Events() <-chan struct{}
	Errors() <-chan error
	Close()
}

// QuotaExemptable is implemented by user views that can store without
// enforcing the quota limit (the usage is still counted), for local
// delivery that must not bounce, like dovecot-lda's noenforcing. The
// setting applies to the view it is called on.
type QuotaExemptable interface {
	SetQuotaEnforced(enforced bool)
}

// QuotaEventSilencer is implemented by user views whose quota changes
// can be kept from the quota event hook (warnings, over-quota flag), for
// an import into an account that is not live here yet.
type QuotaEventSilencer interface {
	SilenceQuotaEvents()
}

// User is the per-user storage API the protocol layer talks to.
type User interface {
	Close()

	ListMailboxes() ([]Mailbox, error)
	Lookup(name string) (*Mailbox, error)
	CreateMailbox(name string, specialUse []string) error
	EnsureMailbox(name string, specialUse []string) error
	DeleteMailbox(name string) error
	RenameMailbox(oldName, newName string) error
	SetSubscribed(name string, subscribed bool) error
	// SetSortOrder records where a client sorts the mailbox among its
	// siblings. It is a JMAP property with no IMAP counterpart, so
	// nothing else in the server reads it.
	SetSortOrder(name string, order uint32) error
	Info(name string) (*MailboxInfo, error)

	ListMessages(id string) ([]Meta, error)
	GetMessages(id string, uids []uint32) ([]Meta, error)
	Changes(id string, since uint64) (changes []Change, ok bool, err error)
	Vanished(id string, since uint64) ([]uint32, error)
	Deliver(name string, raw []byte, flags []string, when time.Time) (*Meta, error)
	// DeliverGUID is Deliver with a caller-chosen GUID (32 hex digits, see
	// NewGUID), so a delivery intent can name the message before it exists.
	DeliverGUID(name, guid string, raw []byte, flags []string, when time.Time) (*Meta, error)
	Read(m *Meta) ([]byte, error)
	SetFlags(name string, uids []uint32, unchangedSince uint64, apply func(old []string) []string) (*FlagsResult, error)
	Expunge(name string, metas []Meta) error
	Copy(metas []Meta, dest string) ([]Meta, error)
	Move(src string, metas []Meta, dest string) ([]Meta, error)

	SearchField(field, query string) (map[string]bool, error)
	SearchFields(fields []string, query string) (map[string]bool, error)

	QuotaUsage() (Quota, error)
	QuotaLimit() (Quota, error)
	SetQuotaLimit(Quota) error
	// SetStorageLimit sets only the storage limit (userdb attribute).
	SetStorageLimit(bytes int64) error

	Watch(id string) (Watch, error)
	Lock(id string, ttl time.Duration) (Lease, error)
	// PendingFloor returns the modseq before the oldest pending change
	// (ok=false: none), used after a full snapshot load.
	PendingFloor(id string, highest uint64) (floor uint64, ok bool, err error)

	// MarkRecent moves the \Recent watermark of the mailbox to uidNext.
	MarkRecent(id string, uidNext uint32) error
	// ListSubscribedMissing lists subscribed names without a mailbox
	// (RFC 5258 \NonExistent).
	ListSubscribedMissing() ([]string, error)

	// Access control (RFC 4314). GetACL returns the explicit entries of a
	// mailbox; SetACL replaces the rights of one identifier (empty rights
	// removes it) and maintains the grantee's SharedRef. ListShared lists
	// the mailboxes of other users this user was granted rights on.
	GetACL(name string) (map[string]string, error)
	SetACL(name, identifier, rights string) error
	ListShared() ([]SharedRef, error)
	SetSharedSubscribed(owner, mailbox string, subscribed bool) error
	// RemoveSharedRef drops the pointer ref unless a newer generation
	// replaced it meanwhile (ref.Gen 0 removes unconditionally).
	RemoveSharedRef(ref SharedRef) error

	// Annotations (METADATA, RFC 5464). mailbox "" addresses the server
	// entries. Keys are full entry names ("/shared/comment",
	// "/private/comment"); private entries belong to the accessing
	// account. SetMetadata with a nil value removes the entry.
	GetMetadata(mailbox, accessor string) (map[string]string, error)
	SetMetadata(mailbox, accessor string, entries map[string]*string) error
	// SwapMetadata sets one entry (nil: removes it) only while it still
	// holds old ("" is an absent entry), in one atomic step; swapped is
	// false, and nothing written, when it held something else.
	SwapMetadata(mailbox, accessor, entry, old string, next *string) (swapped bool, err error)

	// Delivery intents bracket a delivery that consists of several index
	// updates (Sieve fileinto into several mailboxes, MULTIAPPEND): what
	// was stored is recorded, and an intent left behind by a crash is
	// rolled back by the scrubber. id "" is accepted as "no intent".
	// RecordDelivery is called before the message is stored: the intent
	// names mailbox and GUID, so a crash right after the store is covered.
	// EndDelivery must succeed before the client is told of success.
	BeginDelivery() (id string, err error)
	RecordDelivery(id, mailbox, guid string) error
	EndDelivery(id string) error

	// VacationSeen manages the auto-reply suppression record of key. With
	// ttl > 0 it records a sent reply valid for ttl and reports whether
	// an unexpired record already existed; with ttl < 0 it only reports
	// (nothing is written); with ttl == 0 it removes the record (a
	// reservation whose send failed).
	VacationSeen(key string, ttl time.Duration) (already bool, err error)

	// Private \Seen marks of public folders. A public folder belongs to
	// nobody, so "read" is a statement about a person, not about the
	// folder: every account keeps its own marks, while every other flag
	// stays shared. The marks are keyed by mailbox id and UIDVALIDITY,
	// so a folder that is recreated does not inherit them.
	// PrivateSeen returns every mark of the folder, PrivateSeenOf only
	// those of the given UIDs — a folder somebody has read for years
	// has many, and a session that learns of one change should not read
	// them all. SetPrivateSeen records a change under a modseq that the
	// caller allocated from the mailbox's own counter with TouchModSeq:
	// private marks and shared flags share one number space, so
	// CONDSTORE and QRESYNC see a single, monotonic MODSEQ per mailbox.
	// PrunePrivateSeen drops marks of messages that are gone (keep holds
	// the UIDs that exist) and every mark of another UIDVALIDITY, at most
	// limit of them per call, because an EXPUNGE by one account cannot
	// reach into another account's namespace.
	PrivateSeen(mailboxID string, uidValidity uint32) (marks map[uint32]PrivateMark, err error)
	PrivateSeenOf(mailboxID string, uidValidity uint32, uids []uint32) (marks map[uint32]PrivateMark, err error)
	SetPrivateSeen(mailboxID string, uidValidity uint32, uids []uint32, seen bool, modseq uint64) error
	PrunePrivateSeen(mailboxID string, uidValidity uint32, keep map[uint32]bool, limit int) error

	// TouchModSeq allocates the next MODSEQ of a mailbox and announces
	// the UIDs as changed without changing a message. write runs with
	// that number while the change is still pending, so what it writes
	// elsewhere — the private \Seen marks of one account — is in place
	// before any session learns of the modseq. The change is published
	// when write returns without an error.
	TouchModSeq(mailbox string, uids []uint32, write func(modseq uint64) error) (modseq uint64, err error)

	// Public folder subscriptions. Public folders belong to no account
	// (see PublicNamespace), so every account keeps its own set of
	// subscriptions to them, by mailbox id: a folder that is renamed
	// stays subscribed.
	PublicSubscriptions() (map[string]bool, error)
	SetPublicSubscribed(mailboxID string, subscribed bool) error

	// Account change log (see account.go). AccountState returns the
	// account's change counter, the number a client stores as its state
	// token. AccountChanges returns the entries after since, oldest
	// first, at most limit of them; a full page means there may be more,
	// and the caller continues from the Seq of the last entry. ok is
	// false when the log no longer covers since, and the caller has to
	// resynchronise instead of applying a delta.
	AccountState() (uint64, error)
	AccountChanges(since uint64, limit int) (changes []AccountChange, ok bool, err error)

	// Sieve scripts (ManageSieve, LMTP filtering).
	ListScripts() (names []string, active string, err error)
	GetScript(name string) (string, error)
	ActiveScript() (string, error)
	PutScript(name, text string) error
	DeleteScript(name string) error
	SetActiveScript(name string) error
	RenameScript(oldName, newName string) error
}

// PrivateMark is one account's \Seen mark on a message of a public
// folder, with the modseq at which it last changed. A cleared mark is
// kept with Seen false, so a client that asks for changes since a modseq
// learns about it.
type PrivateMark struct {
	Seen   bool
	ModSeq uint64
}

// HasFlag reports whether flags contains f (case-insensitive).
func HasFlag(flags []string, f string) bool {
	for _, x := range flags {
		if equalFold(x, f) {
			return true
		}
	}

	return false
}

func equalFold(a, b string) bool {
	if len(a) != len(b) {
		return false
	}

	for i := 0; i < len(a); i++ {
		ca, cb := a[i], b[i]
		if ca >= 'A' && ca <= 'Z' {
			ca += 32
		}

		if cb >= 'A' && cb <= 'Z' {
			cb += 32
		}

		if ca != cb {
			return false
		}
	}

	return true
}

// NewGUID returns a random message GUID (16 bytes, hex).
func NewGUID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}

	return hex.EncodeToString(b[:])
}
