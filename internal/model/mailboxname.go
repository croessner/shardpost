package model

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// Bounds of a new mailbox name, in bytes of its UTF-8 form.
const (
	// MaxMailboxNameBytes bounds the whole name, delimiters included.
	MaxMailboxNameBytes = 1024
	// MaxMailboxLevelBytes bounds one level of the hierarchy, which mail
	// clients and file-based stores keep as a file or directory name.
	MaxMailboxLevelBytes = 255
)

// MailboxNameError is a name a new mailbox must not get. Reason says
// why, in words a client can be shown.
type MailboxNameError struct {
	Reason string
}

func (e *MailboxNameError) Error() string { return "mailbox name " + e.Reason }

// CheckNewMailboxName refuses a name that CREATE, RENAME, Mailbox/set,
// the admin API, a Sieve fileinto or server.autocreate must not give a
// mailbox. name is the stored form: UTF-8, levels joined by Delim,
// without a delimiter at either end. Existing names are never checked
// against it, so whatever the store holds stays reachable.
//
// The rules keep a name usable in the common mail clients and in other
// servers a mailbox may later move to:
//
//   - Net-Unicode (RFC 5198, RFC 6855 3, RFC 9051 5.1): valid UTF-8,
//     no control characters, no line or paragraph separator.
//   - No bidirectional embedding, override or isolate controls, which
//     can make a name display as another one, no zero width space, word
//     joiner or invisible operator, no byte order mark and no
//     noncharacters. Joiners, direction marks, combining marks and
//     variation selectors stay allowed: emoji sequences and several
//     scripts need them; a level needs one visible character, though.
//   - No LIST wildcard ("*", "%"): a client that lists the children of
//     such a mailbox with "parent/%" gets the wrong ones.
//   - No backslash: Outlook does not allow it in folder names, and it is
//     the escape character of IMAP quoted strings.
//   - Every level non-empty, not "." or "..", without white space at
//     either end (clients and file systems trim it) and at most
//     MaxMailboxLevelBytes long; the whole name at most
//     MaxMailboxNameBytes.
func CheckNewMailboxName(name string) error {
	return checkNewLevels(name, "")
}

// MailboxLister is what CheckNewMailbox and CheckMailboxRename read of
// an account.
type MailboxLister interface {
	ListMailboxes() ([]Mailbox, error)
}

// CheckNewMailbox is CheckNewMailboxName for a name in the account u:
// the levels that already exist there — those of the deepest mailbox
// above name, or of a mailbox that exists only through its children —
// keep the name they have and are not checked again, so a mailbox
// below an old name the rules would refuse can still be created. The
// account is read only when the name as a whole fails the rules.
func CheckNewMailbox(u MailboxLister, name string) error {
	if CheckNewMailboxName(name) == nil {
		return nil
	}

	all, err := u.ListMailboxes()
	if err != nil {
		return err
	}

	return checkNewLevels(name, existingPrefix(all, name))
}

// CheckMailboxRename checks the new name of a rename of old in the
// account u: the new levels follow CheckNewMailbox, and no mailbox the
// rename moves along with old may end up longer than
// MaxMailboxNameBytes.
func CheckMailboxRename(u MailboxLister, old, name string) error {
	all, err := u.ListMailboxes()
	if err != nil {
		return err
	}

	if CheckNewMailboxName(name) != nil {
		if err := checkNewLevels(name, existingPrefix(all, name)); err != nil {
			return err
		}
	}

	return CheckSubtreeLength(all, old, name)
}

// CheckSubtreeLength refuses a rename of old to name that would give a
// mailbox below old a name longer than MaxMailboxNameBytes. A rename of
// INBOX moves its messages, not its children.
func CheckSubtreeLength(all []Mailbox, old, name string) error {
	if strings.EqualFold(old, "INBOX") {
		return nil
	}

	for _, mb := range all {
		if strings.HasPrefix(mb.Name, old+Delim) && len(name)+len(mb.Name)-len(old) > MaxMailboxNameBytes {
			return &MailboxNameError{Reason: "would make the name of a mailbox below it longer than 1024 bytes"}
		}
	}

	return nil
}

// existingPrefix returns the longest leading part of name, cut at a
// delimiter, that is the name of a mailbox in all or lies above one; ""
// when there is none.
func existingPrefix(all []Mailbox, name string) string {
	best := ""

	for _, mb := range all {
		p := mb.Name
		for p != "" && len(p) > len(best) {
			if p == name || strings.HasPrefix(name, p+Delim) {
				best = p
				break
			}

			i := strings.LastIndex(p, Delim)
			if i < 0 {
				break
			}

			p = p[:i]
		}
	}

	return best
}

// checkNewLevels applies the rules to name, except to the levels of
// kept, a leading part of name (cut at a delimiter) that exists already.
// The bounds of the whole name always apply.
func checkNewLevels(name, kept string) error {
	switch {
	case name == "":
		return &MailboxNameError{Reason: "is empty"}
	case !utf8.ValidString(name):
		return &MailboxNameError{Reason: "is not valid UTF-8"}
	case len(name) > MaxMailboxNameBytes:
		return &MailboxNameError{Reason: "is longer than 1024 bytes"}
	case kept == name:
		return nil
	}

	fresh := name
	if kept != "" {
		fresh = strings.TrimPrefix(name, kept+Delim)
	}

	for _, r := range fresh {
		if reason := badNameRune(r); reason != "" {
			return &MailboxNameError{Reason: reason}
		}
	}

	for level := range strings.SplitSeq(fresh, Delim) {
		if reason := badNameLevel(level); reason != "" {
			return &MailboxNameError{Reason: reason}
		}
	}

	return nil
}

// badNameRune says why r must not appear in a new mailbox name, or "".
func badNameRune(r rune) string {
	switch {
	case unicode.IsControl(r), r == 0x2028, r == 0x2029:
		return "has a control character"
	case r == '*', r == '%':
		return "has a wildcard character (* or %)"
	case r == '\\':
		return "has a backslash"
	case r >= 0x202a && r <= 0x202e, r >= 0x2066 && r <= 0x2069:
		return "has a bidirectional control character"
	case r == 0xfeff, r == 0x200b, r >= 0x2060 && r <= 0x2064, isNoncharacter(r):
		return "has an invisible or reserved code point"
	}

	return ""
}

// isNoncharacter reports the 66 Unicode noncharacters: U+FDD0..U+FDEF
// and the last two code points of every plane.
func isNoncharacter(r rune) bool {
	return (r >= 0xfdd0 && r <= 0xfdef) || r&0xfffe == 0xfffe
}

// badNameLevel says why level must not be one level of a new mailbox
// name, or "".
func badNameLevel(level string) string {
	switch {
	case level == "":
		return "has an empty level"
	case level == "." || level == "..":
		return `has a level "." or ".."`
	case len(level) > MaxMailboxLevelBytes:
		return "has a level longer than 255 bytes"
	}

	first, _ := utf8.DecodeRuneInString(level)
	last, _ := utf8.DecodeLastRuneInString(level)

	if unicode.IsSpace(first) || unicode.IsSpace(last) {
		return "has a level that starts or ends with white space"
	}

	if strings.IndexFunc(level, visible) < 0 {
		return "has a level without a visible character"
	}

	return ""
}

// visible reports a letter, digit, punctuation mark or symbol: a level
// of joiners, direction marks, variation selectors or combining marks
// alone shows as nothing.
func visible(r rune) bool {
	return unicode.In(r, unicode.L, unicode.N, unicode.P, unicode.S)
}
