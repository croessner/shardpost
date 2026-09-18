package model

import (
	"errors"
	"fmt"
	"time"
)

// ErrImportLocked means the account is being imported (or another import
// session holds it); protocol access is refused temporarily.
var ErrImportLocked = errors.New("account import in progress")

// ErrImportNotHeld means the import session does not hold the account's
// import lock (never opened, released, or expired and taken over).
var ErrImportNotHeld = errors.New("import session does not hold the lock")

// ImportConflictError means imported data contradicts the target: a UID
// that is taken by a different message, a mailbox with a different
// UIDVALIDITY that is not empty, a name taken by another mailbox.
type ImportConflictError struct{ Reason string }

func (e *ImportConflictError) Error() string { return "import conflict: " + e.Reason }

// ImportConflict returns an *ImportConflictError.
func ImportConflict(format string, a ...any) error {
	return &ImportConflictError{Reason: fmt.Sprintf(format, a...)}
}

// ImportMailbox describes a source mailbox whose identity is kept.
type ImportMailbox struct {
	Name        string
	UIDValidity uint32
	// ID is the preferred stable mailbox id (32 hex digits, e.g. the
	// Dovecot mailbox GUID); "" or an id in use gives a fresh one. An
	// existing mailbox with this id under another name is renamed.
	ID         string
	SpecialUse []string
	Subscribed bool
	// ShardSize is the index shard size of a mailbox the import creates
	// (0 = the store's configured size). Source UIDs can be sparse and
	// high; a small shard size would make every full read visit many
	// empty shard objects.
	ShardSize uint32
	// Replace allows replacing an existing mailbox with another
	// UIDVALIDITY that holds messages (they are expunged first).
	Replace bool
}

// ImportMessage is one source message with its identity. UID and
// ModSeq are kept; GUID, if a valid and unused (or identical) blob id,
// is kept too, so the same source message in several mailboxes shares
// one blob. POP3UIDL, if set, is what POP3 UIDL reports instead of the
// GUID.
type ImportMessage struct {
	UID          uint32
	ModSeq       uint64
	Flags        []string
	InternalDate time.Time
	SaveDate     time.Time
	GUID         string
	POP3UIDL     string
	Raw          []byte
}

// ImportFlags sets the flags of an imported message with the source
// modseq (delta runs).
type ImportFlags struct {
	UID    uint32
	ModSeq uint64
	Flags  []string
}

// ImportResult counts what ImportMessages did.
type ImportResult struct {
	Imported int   // new entries
	Existing int   // UIDs already present with the same size (resume)
	Shared   int   // new entries that reference an existing blob
	Bytes    int64 // size of the new entries
}

// Importer is implemented by user views that can import an account with
// its identities (UIDVALIDITY, UIDs, MODSEQ). Every mutation expects the
// caller to hold the account's import lock; the store does not check it
// again per call (the admin service renews it before each call).
type Importer interface {
	// ImportLock takes or renews the account's import lock for session;
	// ErrImportLocked if another unexpired session holds it.
	ImportLock(session string, ttl time.Duration) (expires time.Time, err error)
	// ImportRenew extends the lock of session; ErrImportNotHeld if the
	// session does not hold it (an expired lock of the same session that
	// nobody took over is renewed).
	ImportRenew(session string, ttl time.Duration) (expires time.Time, err error)
	// ImportUnlock releases the lock if session holds it.
	ImportUnlock(session string) error
	// ImportLocked reports whether an unexpired import lock exists.
	ImportLocked() (bool, error)

	// MailboxByID resolves a mailbox by its stable id.
	MailboxByID(id string) (*Mailbox, error)
	// ImportMailbox creates the mailbox with the source UIDVALIDITY or
	// adopts an existing one with the same UIDVALIDITY; an existing empty
	// mailbox with another UIDVALIDITY is replaced.
	ImportMailbox(spec ImportMailbox) (*Mailbox, error)
	// ImportMessages stores messages at their source UIDs (ascending).
	ImportMessages(id string, msgs []ImportMessage) (*ImportResult, error)
	// ImportChanges applies source flag changes and removes UIDs.
	ImportChanges(id string, flags []ImportFlags, expunge []uint32) error
	// ImportFinalize sets UIDNEXT and HIGHESTMODSEQ to at least the
	// source values and makes the change log start after them.
	ImportFinalize(id string, uidNext uint32, highestModSeq uint64) (*MailboxInfo, error)
	// ListMessagesAfter returns up to limit records with UID > after, in
	// UID order (inventory for delta runs).
	ListMessagesAfter(id string, after uint32, limit int) ([]Meta, error)
}

// ValidGUID reports whether s is a message or mailbox id of this store
// (16 bytes, lowercase hex).
func ValidGUID(s string) bool {
	if len(s) != 32 {
		return false
	}

	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}

	return true
}

// ValidPOP3UIDL reports whether s can be a POP3 unique-id (RFC 1939 7:
// 1 to 70 characters 0x21 to 0x7E).
func ValidPOP3UIDL(s string) bool {
	if s == "" || len(s) > 70 {
		return false
	}

	for i := 0; i < len(s); i++ {
		if s[i] < 0x21 || s[i] > 0x7e {
			return false
		}
	}

	return true
}

// ValidFlag reports whether f can be stored as a flag or keyword: an
// IMAP atom, optionally with a leading backslash. \Recent is session
// state and never stored.
func ValidFlag(f string) bool {
	if f == "" || len(f) > 256 || HasFlag([]string{f}, `\Recent`) {
		return false
	}

	for i := 0; i < len(f); i++ {
		c := f[i]
		if c <= 0x20 || c >= 0x7f || (c == '\\' && i > 0) {
			return false
		}

		switch c {
		case '(', ')', '{', '%', '*', '"', ']':
			return false
		}
	}

	return true
}

// RefuseDuringImport returns ErrImportLocked when u is being imported
// (u implements Importer and holds an unexpired lock). Protocol entry
// points call it when they open a user, so no session or delivery
// writes into an account while its identities are imported.
func RefuseDuringImport(u User) error {
	imp, ok := u.(Importer)
	if !ok {
		return nil
	}

	locked, err := imp.ImportLocked()
	if err != nil {
		return err
	}

	if locked {
		return ErrImportLocked
	}

	return nil
}
