package model

import (
	"errors"
	"fmt"
	"strings"
)

// Limits bound how far one account can grow the omap objects that hold
// its state, so that no single object reaches the size Ceph warns about
// (LARGE_OMAP_OBJECTS) and a single account cannot slow down the OSD that
// serves it. Each field is a maximum; 0 means unlimited. The defaults of
// the configuration (config.Limits) are far above what any account
// reaches in normal use.
type Limits struct {
	// Mailboxes bounds the mailboxes of an account (the "m:" records of
	// its registry). INBOX is never refused.
	Mailboxes int
	// MailboxNames bounds the distinct names an account ever created a
	// mailbox under. The registry keeps the last UIDVALIDITY of every
	// such name ("uv:<name>") for good, so that a mailbox recreated
	// under the same name never reuses a UIDVALIDITY; creating a mailbox
	// under a name that has no such record is refused at the limit.
	// RENAME does not create a record and is not counted.
	MailboxNames int
	// KeywordsPerMailbox bounds the user keywords a mailbox knows (its
	// keyword list, which only grows: PERMANENTFLAGS and the JMAP
	// keywords of the mailbox). A change that would add a keyword
	// beyond it is refused; keywords the mailbox already knows are
	// always accepted.
	KeywordsPerMailbox int
	// MetadataEntries bounds the METADATA entries (RFC 5464) of an
	// account, shared and private, server and mailbox entries together.
	// Only the number of entries counts (each value is bounded on its
	// own); entries the server keeps for itself (ServerManagedPrefix)
	// are never refused, but they are counted.
	MetadataEntries int
	// SharedPointers bounds the pointers to mailboxes of other accounts
	// that an account receives through ACL grants ("sh:" records of its
	// registry). A grant that would add one beyond it is refused on the
	// granting side, before the ACL changes.
	SharedPointers int
}

// LimitKind names the limit an operation ran into.
type LimitKind string

// The limits of Limits.
const (
	LimitMailboxes      LimitKind = "mailboxes"
	LimitMailboxNames   LimitKind = "mailbox_names"
	LimitKeywords       LimitKind = "keywords_per_mailbox"
	LimitMetadata       LimitKind = "metadata_entries"
	LimitSharedPointers LimitKind = "shared_pointers"
)

// ErrLimit is what a store returns, wrapped in a *LimitError, when an
// operation would exceed one of the Limits.
var ErrLimit = errors.New("account limit reached")

// LimitError says which limit was reached; it matches ErrLimit.
type LimitError struct {
	Kind LimitKind
	Max  int
}

func (e *LimitError) Error() string {
	return fmt.Sprintf("%s: %s (at most %d)", ErrLimit, e.Kind, e.Max)
}

// Unwrap makes errors.Is(err, ErrLimit) hold.
func (e *LimitError) Unwrap() error { return ErrLimit }

// LimitReached returns the error for kind at limit.
func LimitReached(kind LimitKind, limit int) error { return &LimitError{Kind: kind, Max: limit} }

// LimitKindOf returns the kind of a limit error, "" for any other error.
func LimitKindOf(err error) LimitKind {
	var le *LimitError
	if errors.As(err, &le) {
		return le.Kind
	}

	return ""
}

// CheckKeywords reports whether flags would take a mailbox that knows
// the keywords known beyond limit distinct keywords (0: unlimited). System
// flags do not count; keywords compare without regard to case, as the
// stores keep them.
func CheckKeywords(known, flags []string, limit int) error {
	if limit <= 0 {
		return nil
	}

	n := len(known)

	var added []string

	for _, f := range flags {
		if strings.HasPrefix(f, `\`) || HasFlag(known, f) || HasFlag(added, f) {
			continue
		}

		added = append(added, f)
	}

	if len(added) > 0 && n+len(added) > limit {
		return LimitReached(LimitKeywords, limit)
	}

	return nil
}

// SystemFlags returns the system flags of flags (those starting with a
// backslash), dropping the keywords. A delivery that ran into the
// keyword limit stores the message with these rather than not at all.
func SystemFlags(flags []string) []string {
	out := make([]string, 0, len(flags))

	for _, f := range flags {
		if strings.HasPrefix(f, `\`) {
			out = append(out, f)
		}
	}

	return out
}
