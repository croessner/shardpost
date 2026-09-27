package jmap

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strings"

	"shardpost/internal/model"
	"shardpost/internal/sharenotice"
)

// Mailbox.shareWith and MailboxRights.mayShare (RFC 9670 4, JMAP Mail
// Sharing). shareWith is the mailbox's RFC 4314 ACL seen as JMAP rights,
// and writing it writes the ACL the way IMAP SETACL does: one versioned
// change per identifier (model.ACLModifier), with the grantee's pointer
// and the ACL generation following, and a share notification for the
// grantee.
//
// The two rights models do not map one to one. IMAP has rights JMAP has
// no property for (p, and l or t without its partner), and JMAP has one
// IMAP lacks (maySubmit). So a write changes only the IMAP rights of the
// JMAP properties whose value it changes: a property whose value stays
// as the client read it leaves its IMAP rights as they are, and a right
// JMAP cannot express is never dropped by an update. Only removing the
// principal (null) removes its whole entry.

// The shareWith property and the right that allows writing it.
const (
	propShareWith = "shareWith"
	propMayShare  = "mayShare"
)

// shareRight is one MailboxRights property and the RFC 4314 rights it
// stands for; none for maySubmit, which IMAP has no right for.
type shareRight struct {
	key  string
	imap string
}

// shareRights is the table of the mapping, documented in docs/jmap.md.
// mayRename and mayDelete are one IMAP right, x.
var shareRights = []shareRight{
	{"mayReadItems", "lr"},
	{"mayAddItems", "i"},
	{"mayRemoveItems", "te"},
	{"maySetSeen", "s"},
	{"maySetKeywords", "w"},
	{"mayCreateChild", "k"},
	{"mayRename", "x"},
	{"mayDelete", "x"},
	{"maySubmit", ""},
	{propMayShare, "a"},
}

// Two properties that are one IMAP right.
const (
	rightRename = "mayRename"
	rightDelete = "mayDelete"
	rightSubmit = "maySubmit"
)

// shareRightOf is the IMAP rights of a property, and whether it is one.
func shareRightOf(key string) (string, bool) {
	for _, r := range shareRights {
		if r.key == key {
			return r.imap, true
		}
	}

	return "", false
}

// holdsAll reports whether rights has every one of letters; no letters
// is never held.
func holdsAll(rights, letters string) bool {
	if letters == "" {
		return false
	}

	for i := 0; i < len(letters); i++ {
		if !model.HasRight(rights, letters[i]) {
			return false
		}
	}

	return true
}

// rightsMap is an ACL entry as a MailboxRights object: a property is true
// when the entry has all of its IMAP rights.
func rightsMap(rights string) map[string]bool {
	out := make(map[string]bool, len(shareRights))
	for _, r := range shareRights {
		out[r.key] = holdsAll(rights, r.imap)
	}

	return out
}

// applyRights changes an ACL entry by the JMAP properties a client gave:
// a property whose value differs from what the entry shows now adds or
// removes all of its IMAP rights; one that shows what it asks for, and
// every right JMAP cannot express, stays as it is.
func applyRights(old string, want map[string]bool) string {
	set := map[byte]bool{}
	for i := 0; i < len(old); i++ {
		set[old[i]] = true
	}

	cur := rightsMap(old)

	for _, r := range shareRights {
		v, given := want[r.key]
		if !given || r.imap == "" || cur[r.key] == v {
			continue
		}

		for i := 0; i < len(r.imap); i++ {
			set[r.imap[i]] = v
		}
	}

	var b strings.Builder

	for i := 0; i < len(model.AllRights); i++ {
		if set[model.AllRights[i]] {
			b.WriteByte(model.AllRights[i])
		}
	}

	return b.String()
}

// shareProps reports whether the request uses the capabilities that give
// Mailbox shareWith and myRights mayShare; without them a Mailbox looks
// as it did before RFC 9670.
func (s *session) shareProps() bool { return s.using[CapPrincipals] || s.using[CapMailShare] }

// mayShare reports whether the user may read and write the shareWith of
// a mailbox: always in its own account, with right a elsewhere.
func (a *account) mayShare(mailboxID string) bool { return a.personal || a.may(mailboxID, 'a') }

// shareWithOf is Mailbox.shareWith: the ACL entries of accounts other
// than the owner, as rights objects, or null when there are none. The
// ACL is only shown to a user who may change it, as IMAP GETACL needs a;
// anyone else sees null. "anyone" and negative or reserved identifiers
// are not principals and are left out — and left alone by a write.
func shareWithOf(a *account, mb *model.Mailbox) any {
	if !a.mayShare(mb.ID) {
		return nil
	}

	out := map[string]map[string]bool{}

	for id, rights := range mb.ACL {
		if id != a.owner && isPrincipalID(id) {
			out[id] = rightsMap(rights)
		}
	}

	if len(out) == 0 {
		return nil
	}

	return out
}

// shareEdit is one validated change of an ACL entry.
type shareEdit struct {
	id string
	// want are the JMAP properties to apply; nil removes the entry.
	want map[string]bool
	// explicit are the rights a patch of single rights named, nil for
	// a whole rights object.
	explicit map[string]bool
}

// sharePatch is the shareWith part of a creation or an update.
type sharePatch struct {
	// replace is a whole new shareWith (or null): every principal that is
	// not in it loses its entry.
	replace bool
	edits   map[string]*shareEdit
}

// unescapePointer undoes the escaping of one JSON pointer segment (RFC
// 6901 4).
func unescapePointer(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(s, "~1", "/"), "~0", "~")
}

// readShare collects the shareWith keys of a patch: "shareWith",
// "shareWith/<principal>" and "shareWith/<principal>/<right>" (RFC 8620
// 5.3). nil when the patch has none.
func readShare(raw map[string]json.RawMessage) (*sharePatch, *setError) {
	var p *sharePatch

	for _, key := range sortedRawKeys(raw) {
		if key != propShareWith && !strings.HasPrefix(key, propShareWith+"/") {
			continue
		}

		if p == nil {
			p = &sharePatch{edits: map[string]*shareEdit{}}
		}

		if serr := p.read(key, raw[key]); serr != nil {
			return nil, serr
		}
	}

	return p, nil
}

// invalidPatch is a patch whose paths overlap (RFC 8620 5.3).
func invalidPatch(why string) *setError { return &setError{Type: "invalidPatch", Description: why} }

// read adds one key of a patch.
func (p *sharePatch) read(key string, raw json.RawMessage) *setError {
	if key == propShareWith {
		if len(p.edits) > 0 || p.replace {
			return invalidPatch("shareWith is patched and replaced at once")
		}

		p.replace = true

		return p.readAll(raw)
	}

	if p.replace {
		return invalidPatch("shareWith is patched and replaced at once")
	}

	pid, right, hasRight := strings.Cut(strings.TrimPrefix(key, propShareWith+"/"), "/")
	pid = unescapePointer(pid)

	if pid == "" {
		return invalidProperty(key, "no principal named")
	}

	e := p.edits[pid]

	if !hasRight {
		if e != nil {
			return invalidPatch("the rights of " + pid + " are patched and replaced at once")
		}

		want, serr := readRightsObject(key, raw, true)
		if serr != nil {
			return serr
		}

		p.edits[pid] = &shareEdit{id: pid, want: want}

		return nil
	}

	return p.readOne(key, pid, unescapePointer(right), raw)
}

// readOne adds a patch of one right of one principal.
func (p *sharePatch) readOne(key, pid, right string, raw json.RawMessage) *setError {
	e := p.edits[pid]
	if e != nil && e.explicit == nil {
		return invalidPatch("the rights of " + pid + " are patched and replaced at once")
	}

	if _, ok := shareRightOf(right); !ok {
		return invalidProperty(key, "a mailbox has no right "+right)
	}

	var v bool
	if err := json.Unmarshal(raw, &v); err != nil || string(raw) == jsonNull {
		return invalidProperty(key, "a right is true or false")
	}

	if right == rightSubmit && v {
		return invalidProperty(key, descNoSubmit)
	}

	if e == nil {
		e = &shareEdit{id: pid, want: map[string]bool{}, explicit: map[string]bool{}}
		p.edits[pid] = e
	}

	e.want[right], e.explicit[right] = v, true

	// mayRename and mayDelete are both the IMAP right x: patching one
	// patches the other, and patching the two apart is a contradiction.
	if other := map[string]string{rightRename: rightDelete, rightDelete: rightRename}[right]; other != "" {
		if e.explicit[other] && e.want[other] != v {
			return invalidProperty(key, descRenameDelete)
		}

		e.want[other] = v
	}

	return nil
}

// The descriptions of the rights a client cannot combine.
const (
	descNoSubmit     = "maySubmit has no IMAP right and cannot be granted"
	descRenameDelete = "mayRename and mayDelete are one IMAP right (x) and cannot differ"
)

// readAll reads a whole new shareWith.
func (p *sharePatch) readAll(raw json.RawMessage) *setError {
	if string(raw) == jsonNull {
		return nil
	}

	var all map[string]json.RawMessage
	if err := json.Unmarshal(raw, &all); err != nil {
		return invalidProperty(propShareWith, "shareWith is an object of principal ids or null")
	}

	for _, pid := range sortedRawKeys(all) {
		want, serr := readRightsObject(propShareWith+"/"+pid, all[pid], false)
		if serr != nil {
			return serr
		}

		p.edits[pid] = &shareEdit{id: pid, want: want}
	}

	return nil
}

// readRightsObject reads the rights of one principal: an object of
// MailboxRights properties, a property it leaves out being false. nil is
// allowed where it removes the principal.
func readRightsObject(key string, raw json.RawMessage, nullable bool) (map[string]bool, *setError) {
	if string(raw) == jsonNull {
		if nullable {
			return nil, nil
		}

		return nil, invalidProperty(key, "the rights of a principal are an object")
	}

	var given map[string]json.RawMessage
	if err := json.Unmarshal(raw, &given); err != nil {
		return nil, invalidProperty(key, "the rights of a principal are an object")
	}

	want := make(map[string]bool, len(shareRights))
	for _, r := range shareRights {
		want[r.key] = false
	}

	for k, v := range given {
		if _, ok := shareRightOf(k); !ok {
			return nil, invalidProperty(key, "a mailbox has no right "+k)
		}

		var b bool
		if err := json.Unmarshal(v, &b); err != nil || string(v) == jsonNull {
			return nil, invalidProperty(key, "a right is true or false")
		}

		want[k] = b
	}

	if want[rightSubmit] {
		return nil, invalidProperty(key, descNoSubmit)
	}

	if want[rightRename] != want[rightDelete] {
		return nil, invalidProperty(key, descRenameDelete)
	}

	return want, nil
}

// shareCheck validates a share patch against the mailbox as the call
// read it and returns the edits to make, in principal order. A principal
// must be one the user may see or that exists here (see principal.go);
// the owner's rights are implicit and it is never a key (RFC 9670 4).
func (s *Server) shareCheck(ctx context.Context, sess *session, a *account, acl map[string]string,
	p *sharePatch,
) ([]*shareEdit, *setError) {
	var out []*shareEdit

	if p.replace {
		for id := range acl {
			if id != a.owner && isPrincipalID(id) && p.edits[id] == nil {
				out = append(out, &shareEdit{id: id})
			}
		}
	}

	for _, pid := range sortedEditKeys(p.edits) {
		e := p.edits[pid]

		keep, serr := s.checkPrincipal(ctx, sess, a, acl, e)
		if serr != nil {
			return nil, serr
		}

		if keep {
			out = append(out, e)
		}
	}

	sort.SliceStable(out, func(i, j int) bool { return out[i].id < out[j].id })

	return out, nil
}

// checkPrincipal validates the principal of one edit and reports whether
// the edit changes anything: removing an entry that is not there does
// not.
func (s *Server) checkPrincipal(ctx context.Context, sess *session, a *account, acl map[string]string,
	e *shareEdit,
) (bool, *setError) {
	key := propShareWith + "/" + e.id

	switch {
	case e.id == a.owner:
		return false, invalidProperty(key, "the owner's rights are implicit and not part of shareWith")
	case !isPrincipalID(e.id):
		return false, invalidProperty(key, descNoPrincipal)
	}

	if _, has := acl[e.id]; has {
		return true, nil
	}

	if e.want == nil {
		return false, nil
	}

	known, err := s.knownPrincipal(ctx, sess, e.id)

	switch {
	case errors.Is(err, errLookupBudget), errors.Is(err, errMissBudget):
		return false, &setError{Type: "rateLimit", Properties: []string{key}, Description: err.Error()}
	case err != nil:
		sess.log.Warn("jmap: principal lookup failed", "err", err)

		return false, &setError{Type: setServerFail, Description: descDirectoryDown}
	case !known:
		return false, invalidProperty(key, descNoPrincipal)
	}

	return true, nil
}

// descNoPrincipal is a shareWith key that names no principal the user
// may share with.
const descNoPrincipal = "no such principal"

// sortedEditKeys are the principals of a patch in order.
func sortedEditKeys(m map[string]*shareEdit) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}

	sort.Strings(out)

	return out
}

// errNoACLModifier is a store that cannot change one ACL entry in a
// versioned step.
var errNoACLModifier = errors.New("the store cannot change ACLs")

// shareApply writes the edits to the ACL of a mailbox, one versioned
// change per identifier as the admin API makes it, and tells each
// grantee whose rights changed. A pointer that could not be written is
// logged: the ACL is stored and counts, as for IMAP SETACL.
func (s *Server) shareApply(sess *session, a *account, mb *model.Mailbox, edits []*shareEdit) error {
	if len(edits) == 0 {
		return nil
	}

	mod, ok := a.user.(model.ACLModifier)
	if !ok {
		return errNoACLModifier
	}

	anyone := mb.ACL["anyone"]

	for _, e := range edits {
		var old string

		rights, _, err := mod.ModifyACL(mb.Name, e.id, -1, func(cur string) string {
			old = cur

			if e.want == nil {
				return ""
			}

			return applyRights(cur, e.want)
		})

		switch {
		case errors.Is(err, model.ErrPointerSync):
			sess.log.Warn("jmap: shared pointer not updated", "mailbox", mb.Name, "identifier", e.id, "err", err)
		case err != nil:
			return err
		}

		s.o.Principals.Notices.Record(sharenotice.Change{
			Owner: a.owner, MailboxID: mb.ID, Mailbox: mb.Name, Grantee: e.id, By: sess.account,
			Old: sharenotice.Effective(old, anyone), New: sharenotice.Effective(rights, anyone),
		})
	}

	return nil
}
