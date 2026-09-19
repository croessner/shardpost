package model

import (
	"errors"
	"strings"
)

// EffectiveRights returns the rights account holds through a mailbox ACL
// of another owner: the union of its own entry and the "anyone" entry,
// in canonical order. The owner itself has AllRights; callers check that.
// IMAP and the admin API's ACL explanation share this computation.
func EffectiveRights(acl map[string]string, account string) string {
	set := map[rune]bool{}
	for _, r := range acl[account] + acl["anyone"] {
		set[r] = true
	}

	var b strings.Builder

	for _, r := range AllRights {
		if set[r] {
			b.WriteRune(r)
		}
	}

	return b.String()
}

// MailboxPager is implemented by user views that list mailboxes page by
// page from the registry instead of reading all of them.
type MailboxPager interface {
	// ListMailboxesAfter reads up to limit registry entries whose names
	// sort after the given one ("" = from the start), in name order. It
	// returns the readable mailboxes and the names of all entries read,
	// unreadable ones included, so a caller can page past them.
	ListMailboxesAfter(after string, limit int) (boxes []Mailbox, scanned []string, err error)
}

// ErrVersionConflict means a versioned change found another version than
// the caller expected (admin API If-Match: 412).
var ErrVersionConflict = errors.New("version conflict")

// ErrPointerSync means an ACL change was stored but the grantee's shared
// pointer could not be written; the ACL is what counts, a later change
// (or replacing with the same rights) writes the pointer.
var ErrPointerSync = errors.New("shared pointer not updated")

// ACLModifier is implemented by user views that change one ACL entry in a
// single versioned step (admin API), so relative changes do not race.
type ACLModifier interface {
	// ModifyACL replaces the rights of identifier with change(current
	// rights) ("" removes the entry). With expectGen >= 0 the mailbox's
	// ACL generation must equal it, else ErrVersionConflict. It returns
	// the resulting rights and generation; the grantee's pointer follows
	// as with SetACL.
	ModifyACL(name, identifier string, expectGen int64, change func(old string) string) (rights string, gen uint64, err error)
}

// specialUses are the special-use attributes (RFC 6154, RFC 8457) in
// their canonical spelling, by lower-case name.
var specialUses = map[string]string{
	`\all`: `\All`, `\archive`: `\Archive`, `\drafts`: `\Drafts`, `\flagged`: `\Flagged`,
	`\important`: `\Important`, `\junk`: `\Junk`, `\sent`: `\Sent`, `\trash`: `\Trash`,
}

// CanonicalSpecialUse returns the canonical spelling of a special-use
// attribute; ok is false for an unknown one (which is returned as is).
func CanonicalSpecialUse(a string) (string, bool) {
	if c, ok := specialUses[strings.ToLower(a)]; ok {
		return c, true
	}

	return a, false
}
