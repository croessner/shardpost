package model

import "errors"

// The account change log answers the one question the IMAP data model
// cannot: what changed anywhere in this account. RFC 7162 counts per
// mailbox, so a MODSEQ says nothing about the mailbox next to it. JMAP
// (RFC 8620) wants one state per account and data type and a /changes
// against it, and a token that is a single number.
//
// The log is a second record next to the mailbox change log, never a
// second number space: an entry says which object changed and carries
// the account counter, while the mailbox MODSEQ stays exactly what it
// is. The mailbox log remains authoritative for what happened inside a
// mailbox; the account log orders those events account-wide.
//
// An entry is written in two phases, because a mailbox and the account
// log are separate objects and no store can write both atomically: the
// writer first announces that it is about to change a mailbox, then does
// the work, then records what it did. A reader never reads past an
// announcement that has no record yet, so it cannot advance its state
// over a change that is still being written. An announcement whose
// writer died becomes a Partial entry: the mailbox is named, the
// messages are not, and the reader has to resynchronise that mailbox.

// AccountChangeKind is the data type an account change belongs to.
type AccountChangeKind string

const (
	// KindMailbox is a change to a mailbox itself: creation, rename,
	// subscription, access rights, deletion.
	KindMailbox AccountChangeKind = "mailbox"
	// KindEmail is a change to the messages of one mailbox.
	KindEmail AccountChangeKind = "email"
)

// AccountOp is what happened to the object. For KindEmail it is relative
// to the mailbox the entry names, not to the account: a message moved
// out of one mailbox into another is destroyed in the first and created
// in the second, although the message itself never stopped existing.
type AccountOp string

const (
	// OpCreated is an object that came into existence.
	OpCreated AccountOp = "created"
	// OpUpdated is an object that changed.
	OpUpdated AccountOp = "updated"
	// OpDestroyed is an object that is gone.
	OpDestroyed AccountOp = "destroyed"
)

// EmailRef names one message of an account change: its UID inside the
// mailbox and its GUID, which is the identity a client sees (the IMAP
// EMAILID, and the JMAP Email id). The GUID is recorded rather than
// looked up later because a destroyed message can no longer be read.
type EmailRef struct {
	UID  uint32 `json:"u"`
	GUID string `json:"g"`
}

// AccountChange is one entry of the account change log.
type AccountChange struct {
	// Kind is the data type the entry belongs to.
	Kind AccountChangeKind `json:"k"`
	// Op is what happened.
	Op AccountOp `json:"o"`
	// Mailbox is the stable id of the mailbox that changed (KindMailbox)
	// or in which messages changed (KindEmail).
	Mailbox string `json:"m,omitempty"`
	// Name is the mailbox name the change was made under (KindMailbox).
	// A rename records the new name; the old one is in the entry the
	// rename replaced, if the log still holds it.
	Name string `json:"n,omitempty"`
	// Msgs are the messages the change touched (KindEmail).
	Msgs []EmailRef `json:"e,omitempty"`
	// Seq is the account counter the entry was committed at. It is
	// monotonic per account and is what a state token names.
	Seq uint64 `json:"-"`
	// ModSeq is the mailbox MODSEQ the change was written at (KindEmail).
	ModSeq uint64 `json:"s,omitempty"`
	// Partial marks an entry that does not enumerate what changed: the
	// mailbox is known to have changed, its objects are not listed. It
	// is written where listing them would mean listing a whole mailbox
	// (an import, a repair) and where a process died between the
	// announcement and the record. A reader resynchronises the named
	// mailbox instead of applying a delta. A Partial entry without a
	// Mailbox says even less — something of that kind changed — and a
	// reader resynchronises the whole data type.
	Partial bool `json:"p,omitempty"`
}

// Two rules bind every reader of the log, and breaking either of them
// loses changes silently:
//
// The state a reader reports after applying a page of changes is the Seq
// of the last entry it applied, never a fresh AccountState: entries
// between the two would be skipped. A reader that applied no entry keeps
// the state it started from.
//
// An entry that is Partial is not a delta. The reader has to re-read the
// mailbox it names (or, without a mailbox, everything of that kind) and
// may not pretend the entry described the change.

// Blobs is implemented by stores that can hold and serve a blob that no
// mailbox references yet. JMAP (RFC 8620 6.1) lets a client upload data
// first and reference it afterwards — from Email/import, from a message
// it composes, from EmailSubmission — and lets it download any blob it
// can name. Until something references it, an upload is debris that the
// store's own garbage collection removes.
//
// The interface is optional: a front-end that needs it asks for it and
// says so when the store cannot do it, rather than every store having to
// grow the methods.
type Blobs interface {
	// PutBlob stores raw under a fresh GUID and returns it.
	PutBlob(raw []byte) (guid string, err error)
	// BlobSize returns the size of a stored blob. ErrNoBlob: there is
	// none under that GUID.
	BlobSize(guid string) (int64, error)
	// ReadBlob returns length bytes of the blob from offset. size is the
	// blob's size as BlobSize reported it: a reader that streams a blob
	// asks once and passes it back, instead of making the store look it
	// up again for every buffer it fills. A length of -1 reads to the
	// end; a range beyond the end is truncated, not an error.
	ReadBlob(guid string, size, offset, length int64) ([]byte, error)
}

// AccountWatcher is implemented by stores that can tell a front-end,
// wherever in the cluster it runs, that the account change log has grown.
// It is what JMAP push waits on. The events are hints: one may be lost
// or coalesced with the next, so a watcher compares AccountState with
// what it last saw rather than counting events.
type AccountWatcher interface {
	WatchAccount() (Watch, error)
}

// ErrNoBlob is returned for a GUID no blob is stored under.
var ErrNoBlob = errors.New("no such blob")
