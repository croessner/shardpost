package model

import "strings"

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
