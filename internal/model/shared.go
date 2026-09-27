package model

import (
	"strings"
	"unicode/utf8"
)

// SharedPager is implemented by user views that can read their shared
// pointers (see SharedRef) a bounded number at a time. ListShared reads
// all of them, and anybody can add pointers to a user's registry by
// granting it rights; a reader that runs on every request — the JMAP
// session state — must not cost what another account decided.
type SharedPager interface {
	// ListSharedN returns at most limit pointers in (owner, mailbox)
	// order — only those of one owner when owner is not "" — and whether
	// the registry holds more beyond them. Revoked pointers the store
	// still keeps count against the limit, so the answer covers the same
	// stretch of the registry every time.
	ListSharedN(owner string, limit int) (refs []SharedRef, more bool, err error)
	// NextSharedOwner returns the first owner whose pointers follow, in
	// the store's key order, all of those of the given one ("" starts at
	// the beginning), without reading the pointers in between: one owner
	// with many pointers costs a lister one step, not one per pointer.
	// ok is false when no owner follows.
	NextSharedOwner(after string) (owner string, ok bool, err error)
}

// SharedKeyEnd is a key suffix that sorts after every mailbox name of an
// owner's pointers ("<owner>/<mailbox>"): no mailbox name holds a byte
// of 0xff, which is not UTF-8.
const SharedKeyEnd = "/\xff"

// ListSharedN reads a user's pointers with SharedPager when the view has
// it, and with ListShared, sorted and cut, when it has not.
func ListSharedN(u User, owner string, limit int) ([]SharedRef, bool, error) {
	if p, ok := u.(SharedPager); ok {
		return p.ListSharedN(owner, limit)
	}

	all, err := u.ListShared()
	if err != nil {
		return nil, false, err
	}

	var out []SharedRef

	for _, r := range all {
		if owner != "" && r.Owner != owner {
			continue
		}

		if len(out) == limit {
			return out, true, nil
		}

		out = append(out, r)
	}

	return out, false, nil
}

// NextSharedOwner is SharedPager.NextSharedOwner for any view: the
// store's own when it has one, otherwise from ListShared.
func NextSharedOwner(u User, after string) (string, bool, error) {
	if p, ok := u.(SharedPager); ok {
		return p.NextSharedOwner(after)
	}

	all, err := u.ListShared()
	if err != nil {
		return "", false, err
	}

	return nextOwner(all, after)
}

// nextOwner finds the next owner in a sorted pointer list.
func nextOwner(sorted []SharedRef, after string) (string, bool, error) {
	for _, r := range sorted {
		if after == "" || r.Owner+"/"+r.Mailbox > after+SharedKeyEnd {
			return r.Owner, true, nil
		}
	}

	return "", false, nil
}

// NextSharedOwnerOf is NextSharedOwner over a list sorted in key order,
// for stores that keep their pointers in memory.
func NextSharedOwnerOf(sorted []SharedRef, after string) (string, bool, error) {
	return nextOwner(sorted, after)
}

// ValidACLIdentifier reports whether an ACL identifier can be stored: not
// empty, UTF-8, and without the bytes a shared pointer's key uses as
// separators — a "/" would move the owner/mailbox boundary of the key, a
// NUL end it early, and 0xff is what SharedKeyEnd relies on no key
// holding.
func ValidACLIdentifier(id string) bool {
	return id != "" && utf8.ValidString(id) && !strings.ContainsAny(id, "/\x00")
}
