package cephstore

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"syscall"

	"shardpost/internal/model"
)

// Access control (RFC 4314) and shared folders.
//
// The ACL of a mailbox lives in its registry record in the owner's
// namespace (MailboxRec.ACL). A user who was granted rights finds the
// mailbox through a pointer in their own registry, "sh:<owner>/<name>",
// written by SetACL on the owner's side and removed with the last right,
// on DELETE and moved on RENAME. The pointer is a hint for LIST; the
// owner's record is authoritative and checked on every access, so a
// stale pointer (owner deleted the mailbox, pointer write failed) shows
// nothing the user is not entitled to.

const regSharedPrefix = "sh:"

// identAnyone is the RFC 4314 identifier that grants rights to everyone.
const identAnyone = "anyone"

type sharedRec struct {
	Subscribed bool   `json:"subscribed,omitempty"`
	Gen        uint64 `json:"gen,omitempty"`     // ACL generation of the owner's record
	Deleted    bool   `json:"deleted,omitempty"` // tombstone: revoked at Gen
}

func sharedKey(owner, mailbox string) string { return regSharedPrefix + owner + "/" + mailbox }

// GetACL returns the explicit ACL entries of a mailbox.
func (u *User) GetACL(name string) (map[string]string, error) {
	mb, err := u.Lookup(name)
	if err != nil {
		return nil, err
	}

	out := map[string]string{}
	for k, v := range mb.ACL {
		out[k] = v
	}

	return out, nil
}

// SetACL replaces the rights of identifier on the mailbox; empty rights
// remove the entry. The grantee's pointer is written afterwards (best
// effort: the ACL is what counts).
func (u *User) SetACL(name, identifier, rights string) error {
	name = canonical(name)

	identifier = canonicalIdentifier(identifier)
	if identifier == "" || identifier == u.name {
		return fmt.Errorf("invalid identifier")
	}

	rights, err := normalizeACLRights(rights)
	if err != nil {
		return err
	}

	var missing bool

	err = casOmap(u, u.idx, oidRegistry, []string{regKey(name)}, func(cur map[string][]byte) (map[string][]byte, []string) {
		missing = false

		v, ok := cur[regKey(name)]
		if !ok {
			missing = true
			return nil, nil
		}

		var rec MailboxRec
		if json.Unmarshal(v, &rec) != nil {
			missing = true
			return nil, nil
		}

		if !applyACLChange(&rec, identifier, rights) {
			return nil, nil
		}

		rec.ACLGen++
		b, _ := json.Marshal(&rec)

		return map[string][]byte{regKey(name): b}, nil
	})
	u.invalidate()

	if err != nil {
		return fmt.Errorf("set acl %q: %w", name, err)
	}

	if missing {
		return ErrNoMailbox
	}

	if identifier == identAnyone {
		return nil
	}

	return u.syncSharedRef(name, identifier)
}

var _ model.ACLModifier = (*User)(nil)

// ModifyACL implements model.ACLModifier: the change is computed from the
// record read in the same CAS round, so concurrent relative changes do
// not lose each other.
func (u *User) ModifyACL(name, identifier string, expectGen int64, change func(old string) string) (string, uint64, error) {
	name = canonical(name)

	identifier = canonicalIdentifier(identifier)
	if identifier == "" || identifier == u.name {
		return "", 0, fmt.Errorf("invalid identifier")
	}

	var out aclOutcome

	err := casOmap(u, u.idx, oidRegistry, []string{regKey(name)}, func(cur map[string][]byte) (map[string][]byte, []string) {
		out = aclOutcome{}
		return out.apply(cur, regKey(name), identifier, expectGen, change)
	})
	u.invalidate()

	switch {
	case err != nil:
		return "", 0, fmt.Errorf("modify acl %q: %w", name, err)
	case out.missing:
		return "", 0, ErrNoMailbox
	case out.conflict:
		return "", 0, model.ErrVersionConflict
	case out.bad != nil:
		return "", 0, out.bad
	}

	// A pointer is written whenever rights remain (idempotent; this also
	// repairs a missing one). An unchanged removal writes nothing: its
	// tombstone could even create the registry of an account without store.
	if identifier != identAnyone && (out.changed || out.rights != "") {
		if err := u.syncSharedRef(name, identifier); err != nil {
			return out.rights, out.gen, fmt.Errorf("%w: %v", model.ErrPointerSync, err)
		}
	}

	return out.rights, out.gen, nil
}

// aclOutcome is the result of one ModifyACL CAS round.
type aclOutcome struct {
	missing, conflict, changed bool
	bad                        error
	rights                     string
	gen                        uint64
}

// apply computes the new registry record for one CAS round.
func (o *aclOutcome) apply(cur map[string][]byte, key, identifier string, expectGen int64, change func(string) string) (map[string][]byte, []string) {
	var rec MailboxRec
	if v, ok := cur[key]; !ok || json.Unmarshal(v, &rec) != nil {
		o.missing = true
		return nil, nil
	}

	if expectGen >= 0 && rec.ACLGen != uint64(expectGen) {
		o.conflict = true
		return nil, nil
	}

	if o.rights, o.bad = normalizeACLRights(change(rec.ACL[identifier])); o.bad != nil {
		return nil, nil
	}

	o.gen = rec.ACLGen

	if !applyACLChange(&rec, identifier, o.rights) {
		return nil, nil
	}

	rec.ACLGen++
	o.gen, o.changed = rec.ACLGen, true
	b, _ := json.Marshal(&rec)

	return map[string][]byte{key: b}, nil
}

// syncSharedRef writes or removes the grantee's pointer to the mailbox.
// The pointer follows the ACL as it is now, not as the calling SetACL
// wrote it, and carries the record's generation: of two concurrent
// pointer writes the one derived from the newer ACL wins regardless of
// the order they land in (an older revoke leaves a tombstone that an
// older grant cannot override, and vice versa).
func (u *User) syncSharedRef(name, identifier string) error {
	// Public folders carry no pointers: they are found through the
	// public namespace itself, so a grant must not write into the
	// grantee's registry (a folder open to a thousand accounts would
	// otherwise cost a thousand writes, and the folder would show up
	// twice in LIST).
	if model.IsPublic(u.name) {
		return nil
	}

	mb, err := u.Lookup(name)
	if err != nil {
		return nil // mailbox gone meanwhile: DELETE removed the pointers
	}

	gen := mb.ACLGen
	if mb.ACL[identifier] == "" {
		return u.withGrantee(identifier, func(g *User) error { return g.removeSharedRef(u.name, name, gen) })
	}

	return u.withGrantee(identifier, func(g *User) error { return g.addSharedRef(u.name, name, gen) })
}

// normalizeACLRights normalises a non-empty rights string; empty rights
// (remove the entry) stay empty.
func normalizeACLRights(rights string) (string, error) {
	if rights != "" {
		r, ok := model.NormalizeRights(rights)
		if !ok {
			return "", fmt.Errorf("invalid rights %q", rights)
		}

		rights = r
	}

	return rights, nil
}

// applyACLChange sets (or, with empty rights, removes) the rights of
// identifier in rec. It returns false if the ACL already was as asked.
func applyACLChange(rec *MailboxRec, identifier, rights string) bool {
	if rights == "" {
		if _, had := rec.ACL[identifier]; !had {
			return false
		}

		delete(rec.ACL, identifier)

		if len(rec.ACL) == 0 {
			rec.ACL = nil
		}
	} else {
		if rec.ACL == nil {
			rec.ACL = map[string]string{}
		}

		if rec.ACL[identifier] == rights {
			return false
		}

		rec.ACL[identifier] = rights
	}

	return true
}

// withGrantee runs fn against the namespace of another user.
func (u *User) withGrantee(identifier string, fn func(g *User) error) error {
	if u.store == nil {
		return nil
	}

	g, err := u.store.User(identifier)
	if err != nil {
		return fmt.Errorf("open grantee %q: %w", identifier, err)
	}
	defer g.Close()

	return fn(g)
}

func (u *User) addSharedRef(owner, mailbox string, gen uint64) error {
	key := sharedKey(owner, mailbox)

	return casOmap(u, u.idx, oidRegistry, []string{key}, func(cur map[string][]byte) (map[string][]byte, []string) {
		var rec sharedRec
		if v, ok := cur[key]; ok {
			_ = json.Unmarshal(v, &rec)
			if rec.Gen > gen || (rec.Gen == gen && !rec.Deleted) {
				return nil, nil
			}
		}

		rec.Gen, rec.Deleted = gen, false
		b, _ := json.Marshal(&rec)

		return map[string][]byte{key: b}, nil
	})
}

// removeSharedRef revokes the pointer at generation gen: a tombstone
// keeps the generation so that a late grant of an older generation
// cannot bring the pointer back. gen 0 removes unconditionally.
func (u *User) removeSharedRef(owner, mailbox string, gen uint64) error {
	key := sharedKey(owner, mailbox)

	return casOmap(u, u.idx, oidRegistry, []string{key}, func(cur map[string][]byte) (map[string][]byte, []string) {
		v, ok := cur[key]
		if !ok {
			if gen == 0 {
				return nil, nil
			}

			b, _ := json.Marshal(&sharedRec{Gen: gen, Deleted: true})

			return map[string][]byte{key: b}, nil
		}

		if gen == 0 {
			return nil, []string{key}
		}

		var rec sharedRec

		_ = json.Unmarshal(v, &rec)
		if rec.Gen > gen || (rec.Gen == gen && rec.Deleted) {
			return nil, nil
		}

		b, _ := json.Marshal(&sharedRec{Gen: gen, Deleted: true})

		return map[string][]byte{key: b}, nil
	})
}

// RemoveSharedRef implements model.User: drops the pointer ref unless a
// newer generation replaced it (a mailbox re-created and shared again
// after a LIST found it gone).
func (u *User) RemoveSharedRef(ref model.SharedRef) error {
	key := sharedKey(ref.Owner, ref.Mailbox)

	return casOmap(u, u.idx, oidRegistry, []string{key}, func(cur map[string][]byte) (map[string][]byte, []string) {
		v, ok := cur[key]
		if !ok {
			return nil, nil
		}

		var rec sharedRec

		_ = json.Unmarshal(v, &rec)
		if ref.Gen != 0 && rec.Gen > ref.Gen {
			return nil, nil
		}

		return nil, []string{key}
	})
}

// ListShared returns the mailboxes of other users this user has pointers
// for (LIST verifies the rights against the owner's ACL).
func (u *User) ListShared() ([]model.SharedRef, error) {
	vals, err := u.idx.GetAllOmapValues(oidRegistry, "", regSharedPrefix, omapPageSize)
	if err != nil {
		if errCode(err) == syscall.ENOENT {
			return nil, nil
		}

		return nil, fmt.Errorf("list shared: %w", err)
	}

	out := make([]model.SharedRef, 0, len(vals))
	for k, v := range vals {
		rest := strings.TrimPrefix(k, regSharedPrefix)

		owner, mailbox, ok := strings.Cut(rest, "/")
		if !ok {
			continue
		}

		var rec sharedRec

		_ = json.Unmarshal(v, &rec)
		if rec.Deleted {
			continue
		}

		out = append(out, model.SharedRef{Owner: owner, Mailbox: mailbox, Subscribed: rec.Subscribed, Gen: rec.Gen})
	}

	sort.Slice(out, func(i, j int) bool {
		if out[i].Owner != out[j].Owner {
			return out[i].Owner < out[j].Owner
		}

		return out[i].Mailbox < out[j].Mailbox
	})

	return out, nil
}

// SetSharedSubscribed changes the subscription flag of a shared folder.
func (u *User) SetSharedSubscribed(owner, mailbox string, subscribed bool) error {
	key := sharedKey(owner, mailbox)

	var missing bool

	err := casOmap(u, u.idx, oidRegistry, []string{key}, func(cur map[string][]byte) (map[string][]byte, []string) {
		missing = false

		v, ok := cur[key]
		if !ok {
			missing = true
			return nil, nil
		}

		var rec sharedRec

		_ = json.Unmarshal(v, &rec)
		if rec.Subscribed == subscribed {
			return nil, nil
		}

		rec.Subscribed = subscribed
		b, _ := json.Marshal(&rec)

		return map[string][]byte{key: b}, nil
	})
	if err != nil {
		return err
	}

	if missing {
		return ErrNoMailbox
	}

	return nil
}

// dropSharedRefs removes the grantees' pointers after a mailbox was
// deleted (best effort; a leftover pointer is pruned by LIST).
func (u *User) dropSharedRefs(acl map[string]string, mailbox string) {
	if model.IsPublic(u.name) {
		return // no pointers for public folders, see syncSharedRef
	}

	for id := range acl {
		if id == identAnyone {
			continue
		}

		if err := u.withGrantee(id, func(g *User) error { return g.removeSharedRef(u.name, mailbox, 0) }); err != nil {
			u.log.Warn("shared pointer not removed", "grantee", id, "mailbox", mailbox, "err", err)
		}
	}
}

// moveSharedRefs re-points grantees after a RENAME.
func (u *User) moveSharedRefs(rec model.MailboxRec, oldName, newName string) {
	if model.IsPublic(u.name) {
		return // no pointers for public folders, see syncSharedRef
	}

	for id := range rec.ACL {
		if id == identAnyone {
			continue
		}

		err := u.withGrantee(id, func(g *User) error {
			if err := g.addSharedRef(u.name, newName, rec.ACLGen); err != nil {
				return err
			}

			return g.removeSharedRef(u.name, oldName, 0)
		})
		if err != nil {
			u.log.Warn("shared pointer not moved", "grantee", id, "mailbox", oldName, "err", err)
		}
	}
}

func canonicalIdentifier(id string) string {
	id = strings.TrimSpace(id)
	if strings.EqualFold(id, identAnyone) {
		return identAnyone
	}

	return strings.ToLower(id)
}

// Public folder subscriptions live in the account's own registry, keyed
// by the mailbox id of the public folder: the folder belongs to no one,
// but the decision to see it belongs to each account.
const regPublicPrefix = "pub:"

// PublicSubscriptions implements model.User.
func (u *User) PublicSubscriptions() (map[string]bool, error) {
	vals, err := u.idx.GetAllOmapValues(oidRegistry, "", regPublicPrefix, omapPageSize)
	if err != nil {
		if errCode(err) == syscall.ENOENT {
			return map[string]bool{}, nil
		}

		return nil, err
	}

	out := make(map[string]bool, len(vals))
	for k := range vals {
		if id, ok := strings.CutPrefix(k, regPublicPrefix); ok && id != "" {
			out[id] = true
		}
	}

	return out, nil
}

// SetPublicSubscribed implements model.User.
func (u *User) SetPublicSubscribed(mailboxID string, subscribed bool) error {
	key := regPublicPrefix + mailboxID

	return casOmap(u, u.idx, oidRegistry, []string{key}, func(cur map[string][]byte) (map[string][]byte, []string) {
		_, have := cur[key]

		switch {
		case subscribed && !have:
			return map[string][]byte{key: []byte("1")}, nil
		case !subscribed && have:
			return nil, []string{key}
		default:
			return nil, nil
		}
	})
}

// Private \Seen marks of public folders live in the reading account's
// own namespace, one object per public mailbox:
//
//	pubseen/<mailbox id>   omap: "s:<uidvalidity>:<uid>" -> "<modseq>:<0|1>"
//
// The UIDVALIDITY is part of the key, so a folder that is deleted and
// recreated starts without marks. The modseq is not a number space of
// its own: the caller allocates it from the public mailbox's counter
// with TouchModSeq, so a session reports one MODSEQ per mailbox and
// CONDSTORE and QRESYNC stay correct for shared and private changes
// alike.
const (
	oidPrivateSeen = "pubseen/"
	pseenPrefix    = "s:"
)

func pseenKey(uidValidity uint32, uid uint32) string {
	return pseenPrefix + strconv.FormatUint(uint64(uidValidity), 10) + ":" + strconv.FormatUint(uint64(uid), 10)
}

// PrivateSeen implements model.User.
func (u *User) PrivateSeen(mailboxID string, uidValidity uint32) (map[uint32]model.PrivateMark, error) {
	prefix := pseenPrefix + strconv.FormatUint(uint64(uidValidity), 10) + ":"

	vals, err := u.idx.GetAllOmapValues(oidPrivateSeen+mailboxID, "", prefix, omapPageSize)
	if err != nil {
		if errCode(err) == syscall.ENOENT {
			return map[uint32]model.PrivateMark{}, nil
		}

		return nil, err
	}

	marks := make(map[uint32]model.PrivateMark, len(vals))

	for k, v := range vals {
		rest, ok := strings.CutPrefix(k, prefix)
		if !ok {
			continue
		}

		uid, err := strconv.ParseUint(rest, 10, 32)
		if err != nil {
			continue
		}

		marks[uint32(uid)] = decodeMark(v)
	}

	return marks, nil
}

// PrivateSeenOf implements model.User.
func (u *User) PrivateSeenOf(mailboxID string, uidValidity uint32, uids []uint32) (map[uint32]model.PrivateMark, error) {
	marks := make(map[uint32]model.PrivateMark, len(uids))

	if len(uids) == 0 {
		return marks, nil
	}

	keys := make([]string, 0, len(uids))
	for _, uid := range uids {
		keys = append(keys, pseenKey(uidValidity, uid))
	}

	vals, _, err := u.getKeysVer(u.idx, oidPrivateSeen+mailboxID, keys)
	if err != nil {
		if errCode(err) == syscall.ENOENT {
			return marks, nil
		}

		return nil, err
	}

	for i, key := range keys {
		if v, ok := vals[key]; ok {
			marks[uids[i]] = decodeMark(v)
		}
	}

	return marks, nil
}

// encodeMark and decodeMark store a mark as "<modseq>:<0|1>".
func encodeMark(m model.PrivateMark) []byte {
	seen := "0"
	if m.Seen {
		seen = "1"
	}

	return []byte(strconv.FormatUint(m.ModSeq, 10) + ":" + seen)
}

func decodeMark(v []byte) model.PrivateMark {
	modseq, seen, _ := strings.Cut(string(v), ":")
	n, _ := strconv.ParseUint(modseq, 10, 64)

	return model.PrivateMark{Seen: seen == "1", ModSeq: n}
}

// SetPrivateSeen implements model.User.
func (u *User) SetPrivateSeen(mailboxID string, uidValidity uint32, uids []uint32, seen bool, modseq uint64) error {
	if len(uids) == 0 {
		return nil
	}

	keys := make([]string, 0, len(uids))
	for _, uid := range uids {
		keys = append(keys, pseenKey(uidValidity, uid))
	}

	mark := encodeMark(model.PrivateMark{Seen: seen, ModSeq: modseq})

	return casOmap(u, u.idx, oidPrivateSeen+mailboxID, keys, func(cur map[string][]byte) (map[string][]byte, []string) {
		set := map[string][]byte{}

		for _, key := range keys {
			// A mark written under a higher modseq is newer than this
			// one: another session of the same account got there first.
			if have, ok := cur[key]; ok && decodeMark(have).ModSeq > modseq {
				continue
			}

			set[key] = mark
		}

		return set, nil
	})
}

// PrunePrivateSeen implements model.User.
func (u *User) PrunePrivateSeen(mailboxID string, uidValidity uint32, keep map[uint32]bool, limit int) error {
	if limit <= 0 {
		return nil
	}

	vals, err := u.idx.GetAllOmapValues(oidPrivateSeen+mailboxID, "", pseenPrefix, omapPageSize)
	if err != nil {
		if errCode(err) == syscall.ENOENT {
			return nil
		}

		return err
	}

	rm := staleMarks(vals, uidValidity, keep, limit)
	if len(rm) == 0 {
		return nil
	}

	return casOmap(u, u.idx, oidPrivateSeen+mailboxID, rm, func(_ map[string][]byte) (map[string][]byte, []string) {
		return nil, rm
	})
}

// staleMarks returns the keys of marks whose message is gone: everything
// of another UIDVALIDITY and every UID that keep does not name, at most
// limit keys.
func staleMarks(vals map[string][]byte, uidValidity uint32, keep map[uint32]bool, limit int) []string {
	live := pseenPrefix + strconv.FormatUint(uint64(uidValidity), 10) + ":"
	rm := make([]string, 0, limit)

	for k := range vals {
		rest, ok := strings.CutPrefix(k, live)
		if !ok {
			rm = append(rm, k) // another incarnation of the folder
		} else if uid, err := strconv.ParseUint(rest, 10, 32); err == nil && !keep[uint32(uid)] {
			rm = append(rm, k)
		}

		if len(rm) == limit {
			break
		}
	}

	return rm
}

// TouchModSeq implements model.User. The change is announced as pending
// first, so a reader either has not seen the modseq yet or finds what
// write has written; that is the same two-phase scheme SetFlags uses for
// entries on higher shards.
func (u *User) TouchModSeq(name string, uids []uint32, write func(modseq uint64) error) (modseq uint64, err error) {
	end := u.trace(spanTouchModSeq, attrCount(len(uids)))
	defer func() { end(err) }()

	mb, err := u.Lookup(name)
	if err != nil {
		return 0, err
	}

	chg := &changeRec{UIDs: uids, Pending: write != nil}

	if _, modseq, err = u.casHeader(mb, func(_ *MailboxInfo, _ uint64) (*headerChange, error) {
		return &headerChange{chg: chg}, nil
	}); err != nil {
		return 0, err
	}

	if write == nil {
		return modseq, nil
	}

	if err := write(modseq); err != nil {
		// Nothing was written, so nothing is to be picked up: commit the
		// change empty instead of leaving readers waiting for it.
		if cerr := u.commitPending(mb, modseq, &changeRec{}, nil); cerr != nil {
			u.log.Warn("commit of an empty change failed", "mailbox", mb.Name, "modseq", modseq, "err", cerr)
		}

		return 0, err
	}

	if err := u.commitPending(mb, modseq, chg, nil); err != nil {
		return 0, err
	}

	return modseq, nil
}
