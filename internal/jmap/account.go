package jmap

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"sort"
	"strconv"
	"strings"

	"shardpost/internal/model"
)

// Sharing: the accounts of other namespaces (RFC 8620 1.6.2, RFC 8621 2).
//
// Besides the personal account a session lists one account per owner who
// shares a mailbox with the user, and one for the public namespace. The
// store view of such an account (model.User) does no permission checks at
// all: IMAP enforces RFC 4314 in its protocol layer, and JMAP does the
// same here. An account object carries the mailboxes the user may see in
// it (right l) with the rights the user holds on each, and everything a
// shared account answers is derived from that set:
//
//   - the mailbox tree holds the visible mailboxes only, so a mailbox the
//     user cannot see is not found, exactly as IMAP answers NONEXISTENT;
//   - the locator resolves messages only in the mailboxes the user may
//     read (right r), so a message is an Email of the account only where
//     the user can read it;
//   - every write checks the right it needs before anything is written.
//
// The personal account has all rights on everything, and every check
// below is a no-op for it.

// sharedBox is one mailbox of a shared account: its registry entry, with
// the subscription replaced by the user's own, and the user's rights.
type sharedBox struct {
	mb     model.Mailbox
	rights string
}

// account is the account one method call works on.
type account struct {
	// user is the store view of the account's namespace; own is the
	// view of the user's own namespace, which keeps the user's
	// subscriptions to shared and public mailboxes and the private
	// \Seen marks of public folders.
	user model.User
	own  model.User
	// boxes are the mailboxes the user may see (right l), by id; nil for
	// the personal account.
	boxes map[string]*sharedBox
	// id is the account id, owner the namespace in the store, login the
	// account the request authenticated as.
	id, owner, login string
	personal         bool
	public           bool
	// srv bounds and logs what a shared account examines; loc is the
	// locator the call read last, which blobReadable reuses.
	srv *Server
	loc *locator
}

// personalAccount is the account of the authenticated user.
func personalAccount(login string, u model.User) *account {
	return &account{id: accountID(login), owner: login, login: login, user: u, own: u, personal: true}
}

// load reads the mailboxes the user may see in a shared account, with
// the rights it holds on them. It is read once when a method call opens
// the account — so a grant or a revocation counts from the next call on
// — and again only after the call's own writes changed what the user
// sees (Mailbox/set).
func (a *account) load() error { return a.loadWith(nil) }

// loadWith is load with the user's pointers already read (nil reads
// them).
func (a *account) loadWith(refs []model.SharedRef) error {
	if a.personal {
		return nil
	}

	var (
		boxes map[string]*sharedBox
		err   error
	)

	switch {
	case a.public:
		boxes, err = publicBoxes(a.own, a.user, a.login, a.srv.maxBoxes())
	case refs == nil:
		if refs, err = a.own.ListShared(); err == nil {
			boxes, err = ownerBoxes(a.own, a.user, a.login, a.srv.capOwner(a.owner, refs))
		}
	default:
		boxes, err = ownerBoxes(a.own, a.user, a.login, a.srv.capOwner(a.owner, refs))
	}

	if err != nil {
		return err
	}

	a.boxes = boxes

	return nil
}

// maxBoxes is the bound on the mailboxes one shared account examines;
// the default for a server built without options (tests).
func (s *Server) maxBoxes() int {
	if s == nil || s.o.Sharing.MaxMailboxes <= 0 {
		return defaultMaxSharedMailboxes
	}

	return s.o.Sharing.MaxMailboxes
}

// capOwner is the user's pointers to one owner, in name order and at
// most maxBoxes of them. The ones beyond the bound are left out of the
// account — the same ones every time — and the first time an owner is
// cut, the process logs it.
func (s *Server) capOwner(owner string, refs []model.SharedRef) []model.SharedRef {
	var mine []model.SharedRef

	for _, r := range refs {
		if r.Owner == owner {
			mine = append(mine, r)
		}
	}

	sort.Slice(mine, func(i, j int) bool { return mine[i].Mailbox < mine[j].Mailbox })

	if limit := s.maxBoxes(); len(mine) > limit {
		if s != nil {
			if _, logged := s.capLogged.LoadOrStore(owner, true); !logged {
				s.o.Log.Warn("jmap: shared account cut at jmap.max_shared_mailboxes",
					"shared", owner, "pointers", len(mine), "limit", limit)
			}
		}

		mine = mine[:limit]
	}

	return mine
}

// ownerBoxes resolves the user's pointers to the mailboxes of one owner,
// as IMAP LIST does: a pointer whose mailbox is gone is pruned, and one
// without right l is not a mailbox of the account. Mailboxes shared only
// with "anyone" have no pointer and are not found this way, as in IMAP.
func ownerBoxes(own, owner model.User, login string, refs []model.SharedRef) (map[string]*sharedBox, error) {
	out := map[string]*sharedBox{}

	for _, r := range refs {
		mb, err := owner.Lookup(r.Mailbox)
		if errors.Is(err, model.ErrNoMailbox) {
			// The mailbox is gone and the pointer stale (unless it was
			// re-created and shared again, which the generation covers).
			_ = own.RemoveSharedRef(r)

			continue
		}

		if err != nil {
			return nil, err
		}

		rights := model.EffectiveRights(mb.ACL, login)
		if !model.HasRight(rights, 'l') {
			continue
		}

		box := *mb
		box.Subscribed = r.Subscribed
		out[box.ID] = &sharedBox{mb: box, rights: rights}
	}

	return out, nil
}

// publicBoxes reads the public folders the user may see (right l), with
// its own subscriptions, as IMAP LIST reads the public namespace: the
// first limit of them by name.
func publicBoxes(own, public model.User, login string, limit int) (map[string]*sharedBox, error) {
	list, err := public.ListMailboxes()
	if err != nil {
		return nil, err
	}

	subs, err := own.PublicSubscriptions()
	if err != nil {
		return nil, err
	}

	sort.Slice(list, func(i, j int) bool { return list[i].Name < list[j].Name })

	out := map[string]*sharedBox{}

	for i := range list {
		rights := model.EffectiveRights(list[i].ACL, login)
		if !model.HasRight(rights, 'l') {
			continue
		}

		if len(out) >= limit {
			break
		}

		box := list[i]
		box.Subscribed = subs[box.ID]
		out[box.ID] = &sharedBox{mb: box, rights: rights}
	}

	return out, nil
}

// rights are the user's rights on a mailbox of the account: all of them
// on its own, the ACL's on a visible shared one, none otherwise.
func (a *account) rights(mailboxID string) string {
	if a.personal {
		return model.AllRights
	}

	if b := a.boxes[mailboxID]; b != nil {
		return b.rights
	}

	return ""
}

// may reports whether the user holds a right on a mailbox of the account.
func (a *account) may(mailboxID string, right byte) bool {
	return model.HasRight(a.rights(mailboxID), right)
}

// readable are the mailboxes whose messages the user may read (rights l
// and r); nil means all of them, for the personal account.
func (a *account) readable() map[string]bool {
	if a.personal {
		return nil
	}

	out := make(map[string]bool, len(a.boxes))

	for id, b := range a.boxes {
		if model.HasRight(b.rights, 'r') {
			out[id] = true
		}
	}

	return out
}

// tree reads the mailbox tree of the account: every mailbox of the
// personal account, the visible ones of a shared account. A visible
// mailbox below one the user cannot see keeps the invisible levels in its
// name and has no parent, the way IMAP LIST shows it below a \Noselect
// level.
func (a *account) tree() (*tree, error) {
	if a.personal {
		return readTree(a.user)
	}

	all := make([]model.Mailbox, 0, len(a.boxes))
	for _, b := range a.boxes {
		all = append(all, b.mb)
	}

	sort.Slice(all, func(i, j int) bool { return all[i].Name < all[j].Name })

	return buildTree(all), nil
}

// myRights is Mailbox.myRights: the owner's for the personal account,
// the mapping of the RFC 4314 rights otherwise. Reading needs l and r,
// removing t and e (a message is flagged \Deleted, then expunged), and a
// rename or a deletion x; INBOX can be neither, whoever owns it. Nobody
// submits from somebody else's mailbox. mayCreateChild is false even with
// k: a mailbox created in another namespace carries no rights for its
// creator (see descNoSharedCreate), so Mailbox/set does not create one.
func (a *account) myRights(mb *model.Mailbox) mailboxRights {
	if a.personal {
		return ownerRights(mb.Name)
	}

	r := a.rights(mb.ID)
	has := func(c byte) bool { return model.HasRight(r, c) }
	movable := mb.Name != inboxName

	return mailboxRights{
		MayReadItems:   has('l') && has('r'),
		MayAddItems:    has('i'),
		MayRemoveItems: has('t') && has('e'),
		MaySetSeen:     has('s'),
		MaySetKeywords: has('w'),
		MayCreateChild: false,
		MayRename:      has('x') && movable,
		MayDelete:      has('x') && movable,
		MaySubmit:      false,
	}
}

// writeRights are the rights that let a user change something in a
// shared mailbox; an account without any of them on any mailbox is
// read-only. k is not among them: a shared account creates no mailbox
// (see descNoSharedCreate), so k alone lets the user do nothing there.
const writeRights = "iwstex"

// readOnly reports whether the user can change nothing in the account.
func (a *account) readOnly() bool {
	if a.personal {
		return false
	}

	for _, b := range a.boxes {
		if strings.ContainsAny(b.rights, writeRights) {
			return false
		}
	}

	return true
}

// The two kinds of state token of an account: the mailboxes, and the
// messages and threads.
const (
	stateMailboxes = false
	stateEmails    = true
)

// stateSep separates the log sequence from the fingerprint in the state
// token of a shared account.
const stateSep = "-"

// fingerprint summarises what the user may see of a shared account. The
// owner's change log records what happens to the owner's mailboxes, but
// not what the user may see of them: a grant, a revocation, a pointer
// that appears or goes, or a subscription the user changed would slip
// past a delta computed from the log alone. So the state token of a
// shared account carries a fingerprint of the visible set, and a token
// whose fingerprint is not the current one is answered with
// cannotCalculateChanges — the client resynchronises rather than being
// handed a wrong delta. The mailbox fingerprint covers every visible
// mailbox with its rights and subscription, the message fingerprint the
// readable mailboxes.
func (a *account) fingerprint(emails bool) string {
	ids := make([]string, 0, len(a.boxes))
	for id := range a.boxes {
		ids = append(ids, id)
	}

	sort.Strings(ids)

	h := sha256.New()

	for _, id := range ids {
		b := a.boxes[id]

		switch {
		case emails && model.HasRight(b.rights, 'r'):
			_, _ = h.Write([]byte(id + "\n"))
		case !emails:
			_, _ = h.Write([]byte(id + ":" + b.rights + ":" + strconv.FormatBool(b.mb.Subscribed) + "\n"))
		}
	}

	return hex.EncodeToString(h.Sum(nil)[:6])
}

// state writes a state token of the account at a log sequence.
func (a *account) state(emails bool, seq uint64) string {
	if a.personal {
		return stateString(seq)
	}

	return stateString(seq) + stateSep + a.fingerprint(emails)
}

// parseState reads a state token of the account back. A token of a
// shared account whose fingerprint is not the current one is not a
// state this account can compute a delta from.
func (a *account) parseState(emails bool, s string) (uint64, bool) {
	if a.personal {
		return parseState(s)
	}

	seq, fp, ok := strings.Cut(s, stateSep)
	if !ok || fp != a.fingerprint(emails) {
		return 0, false
	}

	return parseState(seq)
}

// listMessages reads the messages of a mailbox the user may read, as the
// user sees them. A mailbox it may not read has none.
func (a *account) listMessages(mailboxID string) ([]model.Meta, error) {
	if !a.personal && !a.may(mailboxID, 'r') {
		return nil, model.ErrNoMailbox
	}

	metas, err := a.user.ListMessages(mailboxID)
	if err != nil {
		return nil, err
	}

	return metas, a.overlayAll(mailboxID, metas)
}

// overlayAll puts the user's private \Seen marks over the messages of a
// whole public folder (see docs/development/public-folders.md): a mark
// decides where there is one, the shared flag where there is none. It
// does nothing for any other account.
func (a *account) overlayAll(mailboxID string, metas []model.Meta) error {
	b := a.publicBox(mailboxID)
	if b == nil || len(metas) == 0 {
		return nil
	}

	marks, err := a.own.PrivateSeen(mailboxID, b.mb.UIDValidity)
	if err != nil {
		return err
	}

	applyMarks(metas, marks)

	return nil
}

// overlay is overlayAll for some messages of a folder, which reads only
// their marks.
func (a *account) overlay(mailboxID string, metas []model.Meta) error {
	b := a.publicBox(mailboxID)
	if b == nil || len(metas) == 0 {
		return nil
	}

	uids := make([]uint32, len(metas))
	for i := range metas {
		uids[i] = metas[i].UID
	}

	marks, err := a.own.PrivateSeenOf(mailboxID, b.mb.UIDValidity, uids)
	if err != nil {
		return err
	}

	applyMarks(metas, marks)

	return nil
}

// publicBox is the public folder of that id, or nil for anything else.
func (a *account) publicBox(mailboxID string) *sharedBox {
	if !a.public {
		return nil
	}

	return a.boxes[mailboxID]
}

// applyMarks replaces the shared \Seen by the account's own mark and
// lifts the MODSEQ to the one the mark was written under, as the IMAP
// overlay does.
func applyMarks(metas []model.Meta, marks map[uint32]model.PrivateMark) {
	for i := range metas {
		mark, ok := marks[metas[i].UID]
		if !ok {
			continue
		}

		metas[i].Flags = withSeen(metas[i].Flags, mark.Seen)
		metas[i].ModSeq = max(metas[i].ModSeq, mark.ModSeq)
	}
}

// boxNamed is the visible mailbox of that name, or nil.
func (a *account) boxNamed(name string) *sharedBox {
	for _, b := range a.boxes {
		if b.mb.Name == name {
			return b
		}
	}

	return nil
}

// setPrivateSeen writes the user's own \Seen marks on messages of a
// public folder, as an IMAP STORE does (see imapproto/private.go): the
// next MODSEQ of the folder is allocated first and the marks are written
// under it while the change is still pending, so every session learns of
// them through the folder's change log, and the account log of the
// public namespace records the messages as updated.
func (a *account) setPrivateSeen(name string, uids []uint32, seen bool) error {
	b := a.boxNamed(name)
	if b == nil || !a.public {
		return model.ErrNoMailbox
	}

	_, err := a.user.TouchModSeq(name, uids, func(modseq uint64) error {
		return a.own.SetPrivateSeen(b.mb.ID, b.mb.UIDValidity, uids, seen, modseq)
	})

	return err
}

// keywordSeen is the keyword of \Seen, which a public folder keeps per
// reader.
const keywordSeen = "$seen"

// keywordRight is the right a keyword needs in a mailbox (RFC 4314 4):
// $seen needs s, every other keyword w.
func keywordRight(k string) byte {
	if k == keywordSeen {
		return 's'
	}

	return 'w'
}

// missingKeywordRight is the first keyword of the set the rights do not
// let the user write, or "" when they all may be written.
func missingKeywordRight(rights string, keywords map[string]bool) string {
	for _, k := range sortedKeys(keywords) {
		if !model.HasRight(rights, keywordRight(k)) {
			return k
		}
	}

	return ""
}

// allowedFlags keeps the flags the rights let the user set on a new
// copy, as IMAP COPY does (RFC 4314 4): \Seen needs s, \Deleted t,
// everything else w.
func allowedFlags(rights string, flags []string) []string {
	if rights == model.AllRights {
		return flags
	}

	out := flags[:0:0]

	for _, f := range flags {
		need := byte('w')

		switch strings.ToLower(f) {
		case `\seen`:
			need = 's'
		case `\deleted`:
			need = 't'
		}

		if model.HasRight(rights, need) {
			out = append(out, f)
		}
	}

	return out
}

// flagSeen is the IMAP flag behind $seen.
const flagSeen = `\Seen`

// withSeen returns flags with \Seen set or removed and everything else
// as it is.
func withSeen(flags []string, seen bool) []string {
	if model.HasFlag(flags, flagSeen) == seen {
		return flags
	}

	out := make([]string, 0, len(flags)+1)

	for _, f := range flags {
		if !strings.EqualFold(f, flagSeen) {
			out = append(out, f)
		}
	}

	if seen {
		out = append(out, flagSeen)
	}

	return out
}

// parentRights are the user's rights on the nearest existing ancestor of
// a path in the account, which decide whether a mailbox may be created or
// renamed there (right k), as IMAP CREATE and RENAME decide it. A path
// without an existing ancestor — the top level of somebody else's
// namespace — has none. The personal account has all of them.
func (a *account) parentRights(path string) (string, error) {
	if a.personal {
		return model.AllRights, nil
	}

	for n := path; ; {
		i := strings.LastIndex(n, model.Delim)
		if i < 0 {
			return "", nil
		}

		n = n[:i]

		acl, err := a.user.GetACL(n)
		if err == nil {
			return model.EffectiveRights(acl, a.login), nil
		}

		if !errors.Is(err, model.ErrNoMailbox) {
			return "", err
		}
	}
}

// setSubscribed records whether the user subscribes to a mailbox of the
// account. The subscription to a shared or a public mailbox is the
// user's, kept in its own namespace, never the owner's.
func (a *account) setSubscribed(mb *model.Mailbox, on bool) error {
	switch {
	case a.personal:
		return a.user.SetSubscribed(mb.Name, on)
	case a.public:
		return a.own.SetPublicSubscribed(mb.ID, on)
	}

	return a.own.SetSharedSubscribed(a.owner, mb.Name, on)
}

// views are the store views of other namespaces one request opened, by
// namespace; they are closed when the request ends, as an IMAP session
// closes its views at logout.
type views struct {
	store Store
	open  map[string]model.User
}

// get opens a namespace once per request.
func (v *views) get(name string) (model.User, error) {
	if u, ok := v.open[name]; ok {
		return u, nil
	}

	u, err := v.store.User(name)
	if err != nil {
		return nil, err
	}

	if v.open == nil {
		v.open = map[string]model.User{}
	}

	v.open[name] = u

	return u, nil
}

// close closes every view the request opened.
func (v *views) close() {
	for _, u := range v.open {
		u.Close()
	}

	v.open = nil
}

// SharingOptions configure the accounts of other namespaces a session
// lists: the mailboxes other accounts share with the user, and the
// public folders. They follow the IMAP namespaces: a prefix that is
// empty switches that namespace off for JMAP as well.
type SharingOptions struct {
	// SharedPrefix is server.shared_prefix; "" lists no shared accounts.
	SharedPrefix string
	// PublicPrefix is server.public_prefix; "" lists no public account.
	// Its name without the trailing delimiter names the account.
	PublicPrefix string
	// MaxAccounts bounds how many shared accounts (owners) the session
	// resource lists, so that building it stays cheap; an account beyond
	// the bound is still reachable by its id.
	MaxAccounts int
	// MaxMailboxes bounds the mailboxes one shared account examines —
	// the user's pointers to one owner, in name order, or the visible
	// public folders by name — and the pointers the session resource
	// examines in total. What lies beyond is left out, and logged.
	MaxMailboxes int
}

// The bounds a configuration that says nothing gets.
const (
	defaultMaxSharedAccounts  = 64
	defaultMaxSharedMailboxes = 1000
)

// sharingDefaults fills in the bounds left at zero.
func sharingDefaults(o SharingOptions) SharingOptions {
	if o.MaxAccounts <= 0 {
		o.MaxAccounts = defaultMaxSharedAccounts
	}

	if o.MaxMailboxes <= 0 {
		o.MaxMailboxes = defaultMaxSharedMailboxes
	}

	return o
}

// resolveAccount returns the account an accountId names for this
// session. An empty id, or the user's own, is the personal account.
// Anything else is a shared account only when the user can see at least
// one mailbox in it; an unknown id, the id of an account that shares
// nothing with the user and one the user has no rights in are all
// accountNotFound, so that whether an account exists is never revealed.
func (s *Server) resolveAccount(sess *session, id string) (*account, *methodError) {
	if id == "" || id == accountID(sess.account) {
		return sess.home(), nil
	}

	a, err := s.sharedAccount(sess, id)
	if err != nil {
		sess.log.Error("jmap: open shared account", "shared", id, "err", err)

		return nil, &methodError{Type: errServerFail, Description: "the account could not be read"}
	}

	if a == nil {
		return nil, &methodError{Type: errAccountNotFound}
	}

	return a, nil
}

// sharedAccount opens a shared account by id, or returns nil when the
// user has none of that id. The owner's namespace is opened only after
// the user's own pointers named it, so an id a client makes up opens
// nothing.
func (s *Server) sharedAccount(sess *session, id string) (*account, error) {
	switch {
	case id == model.PublicNamespace:
		return s.openShared(sess, id, nil)
	case s.o.Sharing.SharedPrefix == "" || id == "" || strings.HasPrefix(id, "~") || id == sess.account:
		return nil, nil
	}

	refs, err := sess.own.ListShared()
	if err != nil {
		return nil, err
	}

	return s.openShared(sess, id, refs)
}

// openShared opens a shared account with the user's pointers already
// read (nil for the public account), or returns nil when the user sees
// no mailbox in it.
func (s *Server) openShared(sess *session, id string, refs []model.SharedRef) (*account, error) {
	a := &account{id: id, owner: id, login: sess.account, own: sess.own, srv: s}

	switch {
	case id == model.PublicNamespace:
		if s.o.Sharing.PublicPrefix == "" {
			return nil, nil
		}

		a.public = true
	case !namesOwner(refs, id):
		return nil, nil
	}

	u, err := sess.views.get(a.owner)
	if err != nil {
		return nil, err
	}

	a.user = u

	if err := a.loadWith(refs); err != nil {
		return nil, err
	}

	if len(a.boxes) == 0 {
		return nil, nil
	}

	return a, nil
}

// namesOwner reports whether a pointer names the owner.
func namesOwner(refs []model.SharedRef, owner string) bool {
	for _, r := range refs {
		if r.Owner == owner {
			return true
		}
	}

	return false
}

// sharedAccounts are the accounts of other namespaces the session lists:
// one per owner who shares a visible mailbox with the user, in the order
// of their names and at most SharingOptions.MaxAccounts of them, and the
// public namespace when the user sees a folder in it. An owner whose
// namespace cannot be read now is left out and logged; it is listed
// again once it can be.
//
// Only the session resource builds this list — it opens the owners'
// namespaces and reads one registry entry per pointer. So it examines at
// most MaxMailboxes pointers in all; the owners beyond are left out of
// the resource (and logged), and stay reachable by their ids.
func (s *Server) sharedAccounts(sess *session) []*account {
	var out []*account

	owners, refs := s.sharingOwners(sess)
	budget := s.maxBoxes()

	for _, owner := range owners {
		if len(out) >= s.o.Sharing.MaxAccounts {
			break
		}

		n := len(s.capOwner(owner, refs))
		if n > budget {
			sess.log.Warn("jmap: session resource cut at jmap.max_shared_mailboxes", "shared", owner)
			break
		}

		budget -= n

		a, err := s.openShared(sess, owner, refs)
		if err != nil {
			sess.log.Warn("jmap: shared account left out of the session", "shared", owner, "err", err)
			continue
		}

		if a != nil {
			out = append(out, a)
		}
	}

	a, err := s.openShared(sess, model.PublicNamespace, nil)
	if err != nil {
		sess.log.Warn("jmap: public account left out of the session", "err", err)
	} else if a != nil {
		out = append(out, a)
	}

	return out
}

// sharingOwners are the owners the user's pointers name, sorted, and
// the pointers. A pointer into the public namespace is a leftover (public
// folders are found through their namespace) and is dropped, as IMAP
// LIST drops it.
func (s *Server) sharingOwners(sess *session) ([]string, []model.SharedRef) {
	if s.o.Sharing.SharedPrefix == "" {
		return nil, nil
	}

	refs, err := sess.own.ListShared()
	if err != nil {
		sess.log.Warn("jmap: shared mailboxes not read", "err", err)

		return nil, nil
	}

	seen := map[string]bool{}

	var owners []string

	for _, r := range refs {
		switch {
		case model.IsPublic(r.Owner):
			_ = sess.own.RemoveSharedRef(r)
		case r.Owner == sess.account || seen[r.Owner] || strings.HasPrefix(r.Owner, "~"):
		default:
			seen[r.Owner] = true
			owners = append(owners, r.Owner)
		}
	}

	sort.Strings(owners)

	return owners, refs
}

// sharingFingerprint summarises what the shared accounts of a session
// are made of, from what the user's own registry and the public registry
// hold, without opening any owner's namespace: the user's pointers with
// their generations — a grant or a revocation of the user's rights
// rewrites the pointer with the mailbox's new ACL generation, and a
// deletion or a rename removes or moves it — and the public folders the
// user sees, with its rights. It goes into the session state that every
// API response carries, so it has to be cheap: two registry reads, and
// none when both namespaces are off. A change of the "anyone" entry of a
// shared mailbox alone does not touch the pointers and is not seen here;
// the session resource shows it when a client next fetches it.
func (s *Server) sharingFingerprint(sess *session) string {
	sh := s.o.Sharing
	if sess.own == nil || (sh.SharedPrefix == "" && sh.PublicPrefix == "") {
		return ""
	}

	h := sha256.New()

	if sh.SharedPrefix != "" {
		refs, err := sess.own.ListShared()
		if err != nil {
			// An error is a state of its own; the next good read changes
			// it back, and a client fetches the session again.
			sess.log.Warn("jmap: shared mailboxes not read", "err", err)

			_, _ = h.Write([]byte("shared:unreadable\n"))
		}

		for _, r := range refs {
			_, _ = h.Write([]byte(r.Owner + "/" + r.Mailbox + ":" + strconv.FormatUint(r.Gen, 10) + "\n"))
		}
	}

	if sh.PublicPrefix != "" {
		_, _ = h.Write([]byte("public\n"))

		if err := s.publicFingerprint(sess, h); err != nil {
			sess.log.Warn("jmap: public folders not read", "err", err)

			_, _ = h.Write([]byte("public:unreadable\n"))
		}
	}

	return hex.EncodeToString(h.Sum(nil)[:8])
}

// publicFingerprint adds the public folders the user sees, with its
// rights, to a fingerprint: the registry alone, as IMAP LIST reads it.
func (s *Server) publicFingerprint(sess *session, h io.Writer) error {
	u, err := sess.views.get(model.PublicNamespace)
	if err != nil {
		return err
	}

	list, err := u.ListMailboxes()
	if err != nil {
		return err
	}

	sort.Slice(list, func(i, j int) bool { return list[i].Name < list[j].Name })

	seen := 0

	for i := range list {
		rights := model.EffectiveRights(list[i].ACL, sess.account)
		if !model.HasRight(rights, 'l') || seen >= s.maxBoxes() {
			continue
		}

		seen++
		_, _ = h.Write([]byte(list[i].ID + ":" + rights + "\n"))
	}

	return nil
}

// sharedAccountObject describes a shared account in the session: named
// after its owner (the public one after its prefix), never personal,
// read-only when the user can change nothing in it, and with the mail
// capability alone. Submission, vacation, Sieve, quota and the blob
// methods belong to the owner's own account; a shared account does not
// offer them, and a call of one of them with its id is accountNotFound.
func (s *Server) sharedAccountObject(a *account) Account {
	name := a.owner
	if a.public {
		if p := strings.TrimSuffix(s.o.Sharing.PublicPrefix, model.Delim); p != "" {
			name = p
		}
	}

	mail := s.mail()
	// Only below a mailbox the user holds k on; the top level of another
	// namespace has no mailbox to hold it on.
	mail.MayCreateTopLevelMailbox = false

	return Account{
		Name:       name,
		IsPersonal: false,
		IsReadOnly: a.readOnly(),
		AccountCapabilities: map[string]json.RawMessage{
			CapCore: json.RawMessage(`{}`),
			CapMail: mustRaw(mail),
		},
	}
}

// callSession is the session a method call of a shared-capable method
// runs with: the request's own session for the personal account, a copy
// whose store view is the shared account's otherwise. The copy shares
// the creation ids and the opened views of the request.
func (s *Server) callSession(sess *session, args json.RawMessage) (*session, *methodError) {
	var named struct {
		AccountID string `json:"accountId"`
	}

	if err := json.Unmarshal(args, &named); err != nil {
		return nil, &methodError{Type: errInvalidArguments, Description: err.Error()}
	}

	a, merr := s.resolveAccount(sess, named.AccountID)
	if merr != nil {
		return nil, merr
	}

	return sess.with(a), nil
}
