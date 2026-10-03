package jmap

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"unicode"
	"unicode/utf8"

	"shardpost/internal/metrics"
	"shardpost/internal/model"
)

// mailboxSetRequest is a /set of mailboxes: the common shape plus the
// one argument RFC 8621 2.5 adds to it.
type mailboxSetRequest struct {
	setRequest
	OnDestroyRemoveEmails bool `json:"onDestroyRemoveEmails"`
}

// serverSetProperties are the properties of a Mailbox the server
// computes; a client that tries to write one is told which.
var serverSetProperties = map[string]bool{
	propID: true, propTotalEmails: true, propUnreadEmails: true,
	propTotalThreads: true, propUnreadThreads: true, propMyRights: true,
}

// mailboxWritable are the properties a client may write.
var mailboxWritable = map[string]bool{
	propName: true, propParentID: true, propRole: true, propSortOrder: true, propIsSubscribed: true,
}

// maxSortOrder bounds Mailbox.sortOrder (RFC 8621 2).
const maxSortOrder = 1 << 31

// descNoSharedCreate is why a mailbox is not created in a shared
// account. IMAP CREATE allows it with right k on the parent, but the new
// mailbox carries no ACL — the store does not inherit one — so its
// creator could not see it afterwards: a JMAP client would hold a
// mailbox that answers notFound from then on. The owner creates the
// mailbox and shares it.
const descNoSharedCreate = "a mailbox created in another account would carry no rights for its creator; " +
	"the owner creates and shares it"

// danglingCreation is what a reference to a creation that never
// happened is answered with.
const danglingCreation = "the creation this one points at did not happen"

// mailboxSet implements Mailbox/set (RFC 8621 2.5): creations first,
// then updates, then destructions, each reported on its own, so that one
// rejected object does not undo the rest.
func mailboxSet(ctx context.Context, sess *session, args json.RawMessage) (any, *methodError) {
	var req mailboxSetRequest
	if err := json.Unmarshal(args, &req); err != nil {
		return nil, &methodError{Type: errInvalidArguments, Description: err.Error()}
	}

	if req.AccountID != "" && req.AccountID != sess.accountID() {
		return nil, &methodError{Type: errAccountNotFound}
	}

	if n := len(req.Create) + len(req.Update) + len(req.Destroy); n > sess.limits.MaxObjectsInSet {
		return nil, &methodError{Type: errRequestTooLarge, Description: descTooManyObjects}
	}

	state, _, merr := mailboxState(sess)
	if merr != nil {
		return nil, merr
	}

	if req.IfInState != nil && *req.IfInState != state {
		return nil, &methodError{Type: errStateMismatch}
	}

	out := newSetResponse(sess.accountID(), state)

	mailboxCreates(ctx, sess, &req, out)
	mailboxUpdates(ctx, sess, &req, out)
	mailboxDestroys(sess, &req, out)

	out.NewState = stateAfter(sess, state, out)

	return out, nil
}

// stateAfter is the state a client may adopt after a /set. The account
// counter alone will not do: a write by another session between the two
// reads would be swallowed by it, and a client that took it as its
// sinceState would never hear of that write. The log is therefore read
// back, and the new state is taken only when everything in it is this
// call's own work.
func stateAfter(sess *session, before string, out *setResponse) string {
	if len(out.Created) == 0 && len(out.Updated) == 0 && len(out.Destroyed) == 0 {
		return before
	}

	since, ok := sess.current().seqOf(before)
	if !ok {
		return before
	}

	entries, ok, err := sess.user.AccountChanges(since, maxMailboxChanges)
	if err != nil || !ok {
		return before
	}

	mine := touchedIDs(out)
	last := since
	a := sess.current()

	for i := range entries {
		switch {
		case mine[entries[i].Mailbox]:
			last = entries[i].Seq
		case hidden(a, &entries[i]):
			// Not in a shared account's state (sharedstate.go).
		default:
			return before
		}
	}

	if last == since {
		return before
	}

	// A shared account's token carries what the user sees now, which
	// this call's own writes may have changed (a subscription, a
	// destroyed mailbox); the client applied them already.
	if err := a.load(); err != nil {
		return before
	}

	return a.state(stateMailboxes, last)
}

// touchedIDs are the mailboxes this /set changed.
func touchedIDs(out *setResponse) map[string]bool {
	mine := map[string]bool{}

	for _, obj := range out.Created {
		if id, ok := obj[propID].(string); ok {
			mine[id] = true
		}
	}

	for id := range out.Updated {
		mine[id] = true
	}

	for _, id := range out.Destroyed {
		mine[id] = true
	}

	return mine
}

// mailboxCreate is what a client may give a new mailbox.
type mailboxCreate struct {
	ParentID     *string `json:"parentId"`
	Role         *string `json:"role"`
	SortOrder    *uint64 `json:"sortOrder"`
	IsSubscribed *bool   `json:"isSubscribed"`
	Name         string  `json:"name"`
}

// mailboxCreates runs the creations of a /set. A creation may name its
// parent by the creation id of another one in the same call (RFC 8620
// 5.3), and a JSON object has no order, so the ones whose parent is
// still missing are retried until a round creates nothing new.
func mailboxCreates(ctx context.Context, sess *session, req *mailboxSetRequest, out *setResponse) {
	pending := map[string]json.RawMessage{}
	for cid, raw := range req.Create {
		pending[cid] = raw
	}

	for len(pending) > 0 && createRound(ctx, sess, pending, out) {
		continue
	}

	for cid := range pending {
		out.NotCreated[cid] = &setError{Type: setNotFound, Properties: []string{propParentID},
			Description: danglingCreation}
	}
}

// createRound carries out every creation whose parent is there by now
// and reports whether it got anything done.
func createRound(ctx context.Context, sess *session, pending map[string]json.RawMessage, out *setResponse) bool {
	progress := false

	for cid, raw := range pending {
		t, err := sess.current().tree()
		if err != nil {
			out.NotCreated[cid] = &setError{Type: setServerFail, Description: err.Error()}

			delete(pending, cid)

			progress = true

			continue
		}

		created, serr, wait := createMailbox(ctx, sess, t, raw)
		if wait {
			continue
		}

		delete(pending, cid)

		progress = true

		if serr != nil {
			out.NotCreated[cid] = serr
			continue
		}

		out.Created[cid] = created
		sess.created[cid], _ = created[propID].(string)
	}

	return progress
}

// createMailbox validates one creation and carries it out. wait reports
// that it names a creation id of this call that has not happened yet.
func createMailbox(ctx context.Context, sess *session, t *tree, raw json.RawMessage) (map[string]any, *setError, bool) {
	var given map[string]json.RawMessage
	if err := json.Unmarshal(raw, &given); err != nil {
		return nil, &setError{Type: setInvalidProperties, Description: err.Error()}, false
	}

	if serr := checkProperties(sess, given); serr != nil {
		return nil, serr, false
	}

	var spec mailboxCreate
	if err := json.Unmarshal(raw, &spec); err != nil {
		return nil, &setError{Type: setInvalidProperties, Description: err.Error()}, false
	}

	if !sess.current().personal {
		return nil, &setError{Type: setForbidden, Description: descNoSharedCreate}, false
	}

	parent, wait, serr := parentOf(sess, spec.ParentID)
	if wait || serr != nil {
		return nil, serr, wait
	}

	path, use, serr := placeOf(t, &spec, parent)
	if serr != nil {
		return nil, serr, false
	}

	edits, serr := createShare(ctx, sess, given)
	if serr != nil {
		return nil, serr, false
	}

	if err := sess.user.CreateMailbox(path, use); err != nil {
		return nil, createFailed(err), false
	}

	created, serr := finishCreate(sess, path, &spec)
	if serr != nil || len(edits) == 0 {
		return created, serr, false
	}

	return created, finishShare(sess, path, edits, created), false
}

// createShare validates the shareWith of a creation — a new mailbox has
// no ACL, so every principal in it is a new entry — and returns the
// edits to make once it exists.
func createShare(ctx context.Context, sess *session, given map[string]json.RawMessage) ([]*shareEdit, *setError) {
	raw, ok := given[propShareWith]
	if !ok {
		return nil, nil
	}

	p := &sharePatch{edits: map[string]*shareEdit{}}
	if serr := p.read(propShareWith, raw); serr != nil {
		return nil, serr
	}

	return sess.srv.shareCheck(ctx, sess, sess.current(), nil, p)
}

// finishShare writes the shareWith of a new mailbox and puts what it is
// now into the creation's answer. A creation is all or nothing (RFC 8620
// 5.3): when the ACL cannot be written, the new mailbox is deleted again
// and the creation reported as failed. Only if that deletion fails too is
// the mailbox reported created, with the shareWith actually in effect,
// so that the answer is true either way. Notifications already written
// for a grant that was rolled back stay.
func finishShare(sess *session, path string, edits []*shareEdit, created map[string]any) *setError {
	mb, err := sess.user.Lookup(path)
	if err == nil {
		err = sess.srv.shareApply(sess, sess.current(), mb, edits)
	}

	if err == nil {
		if mb, err = sess.user.Lookup(path); err == nil {
			created[propShareWith] = shareWithOf(sess.current(), mb)

			return nil
		}
	}

	sess.log.Warn("jmap: shareWith of a new mailbox not written; deleting it again", "mailbox", path, "err", err)

	failed := &setError{Type: setServerFail, Description: "the mailbox could not be shared: " + err.Error()}
	if errors.Is(err, model.ErrLimit) {
		failed = limitFailed(err)
	}

	if derr := sess.user.DeleteMailbox(path); derr != nil {
		sess.log.Error("jmap: a new mailbox whose shareWith failed could not be deleted", "mailbox", path, "err", derr)

		if mb, lerr := sess.user.Lookup(path); lerr == nil {
			created[propShareWith] = shareWithOf(sess.current(), mb)

			return nil
		}
	}

	return failed
}

// placeOf validates the name, the sort order and the role of a creation
// and returns the path and the special-use it asks for.
func placeOf(t *tree, spec *mailboxCreate, parent *string) (string, []string, *setError) {
	if serr := checkName(spec.Name); serr != nil {
		return "", nil, serr
	}

	if serr := checkSortOrder(spec.SortOrder); serr != nil {
		return "", nil, serr
	}

	path, serr := t.path(parent, spec.Name)
	if serr != nil {
		return "", nil, serr
	}

	use, serr := useOf(t, spec.Role)
	if serr != nil {
		return "", nil, serr
	}

	return path, use, nil
}

// parentOf resolves the parentId of a creation or an update, which may
// name another creation of the same call.
func parentOf(sess *session, ref *string) (parent *string, wait bool, serr *setError) {
	if ref == nil {
		return nil, false, nil
	}

	if *ref == "" {
		return nil, false, invalidProperty(propParentID, "an empty string is not an id; use null for the top level")
	}

	id, pending := resolveCreationID(sess, *ref)
	if pending {
		return nil, true, nil
	}

	return &id, false, nil
}

// checkSortOrder bounds the sort order, which RFC 8621 2 keeps below
// 2^31 although JSON would carry more.
func checkSortOrder(order *uint64) *setError {
	if order != nil && *order >= maxSortOrder {
		return invalidProperty(propSortOrder, "sortOrder must be below 2^31")
	}

	return nil
}

// useOf turns a requested role into the special-use attribute of a new
// mailbox. An account has at most one mailbox per role (RFC 8621 2), so
// a role another mailbox already holds is refused.
func useOf(t *tree, role *string) ([]string, *setError) {
	if role == nil || *role == "" {
		return nil, nil
	}

	use, ok := useOfRole(*role)
	if !ok {
		return nil, invalidProperty(propRole, "this server cannot give a mailbox the role "+*role)
	}

	for i := range t.all {
		if roleOf(&t.all[i]) == *role {
			return nil, invalidProperty(propRole, "another mailbox already has the role "+*role)
		}
	}

	return []string{use}, nil
}

// createFailed maps a store error onto the /set error a client sees. A
// name a sibling already has violates the uniqueness RFC 8621 2 asks of
// names, which is an invalid property rather than a /set error type of
// its own: alreadyExists belongs to /copy.
func createFailed(err error) *setError {
	if errors.Is(err, model.ErrMailboxExists) {
		return invalidProperty(propName, "a mailbox of that name is already there")
	}

	if errors.Is(err, model.ErrLimit) {
		return limitFailed(err)
	}

	return &setError{Type: setServerFail, Description: err.Error()}
}

// finishCreate applies the properties that are not part of CREATE and
// returns what the server set beyond what the client asked for.
func finishCreate(sess *session, path string, spec *mailboxCreate) (map[string]any, *setError) {
	if spec.SortOrder != nil && *spec.SortOrder != 0 {
		if err := sess.user.SetSortOrder(path, uint32(*spec.SortOrder)); err != nil {
			return nil, &setError{Type: setServerFail, Description: err.Error()}
		}
	}

	if spec.IsSubscribed != nil {
		if err := sess.user.SetSubscribed(path, *spec.IsSubscribed); err != nil {
			return nil, &setError{Type: setServerFail, Description: err.Error()}
		}
	}

	mb, err := sess.user.Lookup(path)
	if err != nil {
		return nil, &setError{Type: setServerFail, Description: err.Error()}
	}

	rights := ownerRights(path)

	if sess.shareProps() {
		owner := true
		rights.MayShare = &owner
	}

	created := map[string]any{propID: mb.ID, propMyRights: rights,
		propTotalEmails: 0, propUnreadEmails: 0, propTotalThreads: 0, propUnreadThreads: 0}

	if spec.SortOrder == nil {
		created[propSortOrder] = uint32(0)
	}

	if spec.IsSubscribed == nil {
		created[propIsSubscribed] = mb.Subscribed
	}

	if spec.Role == nil {
		created[propRole] = nilIfEmpty(roleOf(mb))
	}

	return created, nil
}

// checkProperties refuses the properties a client may not write.
// shareWith is one with the capabilities that define it.
func checkProperties(sess *session, given map[string]json.RawMessage) *setError {
	for key := range given {
		switch {
		case key == propShareWith && sess.shareProps():
		case serverSetProperties[key]:
			return invalidProperty(key, "the server sets this property")
		case !mailboxWritable[key]:
			return invalidProperty(key, "a mailbox has no property "+key)
		}
	}

	return nil
}

// maxMailboxNameBytes is the longest mailbox name this server accepts,
// and what the mail capability advertises as maxSizeMailboxName.
const maxMailboxNameBytes = 255

// checkName refuses a name that is not one level of the hierarchy. JMAP
// names a mailbox by its leaf and points at a parent, so a name that
// carries the delimiter would claim a place the parent does not have.
// RFC 8621 2 asks for Net-Unicode, so a control character or a byte
// sequence that is not UTF-8 is refused as well.
func checkName(name string) *setError {
	switch {
	case strings.TrimSpace(name) == "":
		return invalidProperty(propName, "a mailbox needs a name")
	case len(name) > maxMailboxNameBytes:
		return invalidProperty(propName, "a mailbox name is at most maxSizeMailboxName bytes")
	case strings.Contains(name, model.Delim):
		return invalidProperty(propName, "a name is one level; use parentId for the place in the tree")
	case !utf8.ValidString(name):
		return invalidProperty(propName, "a mailbox name is UTF-8")
	case strings.IndexFunc(name, unicode.IsControl) >= 0:
		return invalidProperty(propName, "a mailbox name has no control characters")
	}

	return nil
}

// mailboxPatch is a validated update: everything a client asked for,
// decoded and checked, before any of it is written.
type mailboxPatch struct {
	sortOrder    *uint32
	isSubscribed *bool
	// share is the shareWith part of the patch, edits what it comes to
	// against the mailbox's ACL.
	share *sharePatch
	edits []*shareEdit
	path  string
	moves bool
}

// mailboxUpdates runs the updates of a /set.
func mailboxUpdates(ctx context.Context, sess *session, req *mailboxSetRequest, out *setResponse) {
	for id, patch := range req.Update {
		t, err := sess.current().tree()
		if err != nil {
			out.NotUpdated[id] = &setError{Type: setServerFail, Description: err.Error()}
			continue
		}

		mb, ok := t.byID[id]
		if !ok {
			out.NotUpdated[id] = &setError{Type: setNotFound}
			continue
		}

		changed, serr := updateMailbox(ctx, sess, t, &mb, patch)
		if serr != nil {
			out.NotUpdated[id] = serr
			continue
		}

		out.Updated[id] = changed

		reloadAfterWrite(sess)
	}
}

// updateMailbox applies one patch. Everything is decoded and checked
// first and written afterwards: RFC 8620 5.3 forbids committing half of
// an update to one record, and a rename that failed after a sort order
// had already been written would be exactly that. The rename goes first
// because it is the step that can still fail on the state of the store.
//
// It returns what the server changed beyond the patch: after a shareWith
// change, the shareWith as it is now, since a patch of one right can move
// another (mayRename and mayDelete are one IMAP right).
func updateMailbox(ctx context.Context, sess *session, t *tree, mb *model.Mailbox, raw map[string]json.RawMessage) (any, *setError) {
	patch, serr := readPatch(sess, t, mb, raw)
	if serr != nil {
		return nil, serr
	}

	a := sess.current()
	if serr := mayUpdate(a, mb, patch); serr != nil {
		return nil, serr
	}

	if patch.share != nil {
		if patch.edits, serr = sess.srv.shareCheck(ctx, sess, a, mb.ACL, patch.share); serr != nil {
			return nil, serr
		}
	}

	if patch.moves {
		if err := sess.user.RenameMailbox(mb.Name, patch.path); err != nil {
			return nil, renameFailed(err)
		}
	}

	if patch.sortOrder != nil {
		if err := sess.user.SetSortOrder(patch.path, *patch.sortOrder); err != nil {
			return nil, &setError{Type: setServerFail, Description: err.Error()}
		}
	}

	now := *mb
	now.Name = patch.path

	if patch.isSubscribed != nil {
		if err := a.setSubscribed(&now, *patch.isSubscribed); err != nil {
			return nil, &setError{Type: setServerFail, Description: err.Error()}
		}
	}

	if patch.share == nil {
		return nil, nil
	}

	return updateShare(sess, a, &now, patch.edits)
}

// updateShare writes the shareWith of an update and answers with it as it
// is now.
func updateShare(sess *session, a *account, mb *model.Mailbox, edits []*shareEdit) (any, *setError) {
	if err := sess.srv.shareApply(sess, a, mb, edits); err != nil {
		if errors.Is(err, model.ErrNoMailbox) {
			return nil, &setError{Type: setNotFound}
		}

		if errors.Is(err, model.ErrLimit) {
			return nil, limitFailed(err)
		}

		return nil, &setError{Type: setServerFail, Description: err.Error()}
	}

	after, err := sess.user.Lookup(mb.Name)
	if err != nil {
		return nil, &setError{Type: setServerFail, Description: err.Error()}
	}

	return map[string]any{propShareWith: shareWithOf(a, after)}, nil
}

// reloadAfterWrite reads a shared account's mailboxes again after a
// write of this call changed them — a rename moves names, a destruction
// takes a mailbox away, a subscription changes the user's view — so the
// next object of the same call sees the account as it is now. Once per
// write, not per object: an object that fails reads nothing again. A
// failed read keeps the view the call had; the next write or call reads
// it again.
func reloadAfterWrite(sess *session) {
	if a := sess.current(); !a.personal {
		if err := a.load(); err != nil {
			sess.log.Warn("jmap: shared account not read again after a write", "err", err)
		}
	}
}

// mayUpdate checks the rights an update of a shared mailbox needs before
// anything is written, as IMAP checks them: a rename or a move needs x on
// the mailbox and k on the nearest existing mailbox above its new place
// (a place without one is the top level of another namespace, which
// nobody holds k on); the sort order needs x, since it is a property of
// the owner's mailbox that every other user sees. A subscription is the
// user's own and needs nothing beyond seeing the mailbox (l).
func mayUpdate(a *account, mb *model.Mailbox, patch *mailboxPatch) *setError {
	if patch.share != nil && !a.mayShare(mb.ID) {
		return &setError{Type: setForbidden, Properties: []string{propShareWith},
			Description: "changing who a mailbox is shared with needs the right to administer it (a)"}
	}

	if a.personal {
		return nil
	}

	if patch.sortOrder != nil && *patch.sortOrder != mb.SortOrder && !a.may(mb.ID, 'x') {
		return &setError{Type: setForbidden, Properties: []string{propSortOrder},
			Description: "changing the sort order needs the right to rename the mailbox"}
	}

	if !patch.moves {
		return nil
	}

	if !a.may(mb.ID, 'x') {
		return &setError{Type: setForbidden, Description: "no right to rename this mailbox"}
	}

	rights, err := a.parentRights(patch.path)
	if err != nil {
		return &setError{Type: setServerFail, Description: err.Error()}
	}

	if !model.HasRight(rights, 'k') {
		return &setError{Type: setForbidden, Properties: []string{propParentID},
			Description: "no right to create a mailbox there"}
	}

	return nil
}

// renameFailed maps a failed rename onto the /set error a client sees.
func renameFailed(err error) *setError {
	if errors.Is(err, model.ErrMailboxExists) {
		return invalidProperty(propName, "a mailbox of that name is already there")
	}

	if errors.Is(err, model.ErrMailboxLocked) {
		metrics.MailboxLockRefusals.WithLabelValues("jmap", "mailbox_set").Inc()

		return &setError{Type: setForbidden, Description: descMailboxDeleting}
	}

	return &setError{Type: setServerFail, Description: err.Error()}
}

// readPatch decodes and validates an update without writing anything.
func readPatch(sess *session, t *tree, mb *model.Mailbox, raw map[string]json.RawMessage) (*mailboxPatch, *setError) {
	if serr := checkPatch(sess, raw); serr != nil {
		return nil, serr
	}

	patch := &mailboxPatch{path: mb.Name}

	if sess.shareProps() {
		share, serr := readShare(raw)
		if serr != nil {
			return nil, serr
		}

		patch.share = share
	}

	if v, ok := raw[propSortOrder]; ok {
		var order uint64
		if err := json.Unmarshal(v, &order); err != nil {
			return nil, invalidProperty(propSortOrder, err.Error())
		}

		if serr := checkSortOrder(&order); serr != nil {
			return nil, serr
		}

		small := uint32(order)
		patch.sortOrder = &small
	}

	if v, ok := raw[propIsSubscribed]; ok {
		var sub bool
		if err := json.Unmarshal(v, &sub); err != nil {
			return nil, invalidProperty(propIsSubscribed, err.Error())
		}

		patch.isSubscribed = &sub
	}

	return readMove(sess, t, mb, raw, patch)
}

// readMove works out where an update puts the mailbox, and checks that
// it may go there.
func readMove(sess *session, t *tree, mb *model.Mailbox,
	raw map[string]json.RawMessage, patch *mailboxPatch,
) (*mailboxPatch, *setError) {
	rawName, hasName := raw[propName]
	rawParent, hasParent := raw[propParentID]

	if !hasName && !hasParent {
		return patch, nil
	}

	name, parent := t.split(mb.Name)

	if hasName {
		serr := readName(rawName, name, &name)
		if serr != nil {
			return nil, serr
		}
	}

	if hasParent {
		got, serr := readParent(sess, rawParent)
		if serr != nil {
			return nil, serr
		}

		parent = got
	}

	return checkMove(t, mb, name, parent, patch)
}

// readName decodes the name of an update. A mailbox whose name carries
// the delimiter is one IMAP made, not one a client could create, so
// sending it back unchanged while moving the mailbox is not refused for
// its shape.
func readName(raw json.RawMessage, current string, into *string) *setError {
	if err := json.Unmarshal(raw, into); err != nil {
		return invalidProperty(propName, err.Error())
	}

	if *into == current {
		return nil
	}

	return checkName(*into)
}

// readParent decodes the parent of an update.
func readParent(sess *session, raw json.RawMessage) (*string, *setError) {
	var ref *string
	if err := json.Unmarshal(raw, &ref); err != nil {
		return nil, invalidProperty(propParentID, err.Error())
	}

	got, wait, serr := parentOf(sess, ref)
	if serr != nil {
		return nil, serr
	}

	if wait {
		return nil, &setError{Type: setNotFound, Properties: []string{propParentID},
			Description: danglingCreation}
	}

	return got, nil
}

// checkMove resolves the target path and refuses the moves that cannot
// be made.
func checkMove(t *tree, mb *model.Mailbox, name string, parent *string, patch *mailboxPatch) (*mailboxPatch, *setError) {
	path, serr := t.path(parent, name)
	if serr != nil {
		return nil, serr
	}

	if path == mb.Name {
		return patch, nil
	}

	switch {
	case mb.Name == inboxName:
		return nil, &setError{Type: setForbidden, Description: "INBOX cannot be renamed or moved"}
	case parent != nil && *parent == mb.ID:
		return nil, invalidProperty(propParentID, "a mailbox cannot be its own parent")
	case strings.HasPrefix(path, mb.Name+model.Delim):
		return nil, invalidProperty(propParentID, "a mailbox cannot be moved below itself")
	}

	patch.path, patch.moves = path, true

	return patch, nil
}

// checkPatch refuses the properties an update may not carry. shareWith,
// and a path into it, is one with the capabilities that define it.
func checkPatch(sess *session, patch map[string]json.RawMessage) *setError {
	for key := range patch {
		switch {
		case sess.shareProps() && (key == propShareWith || strings.HasPrefix(key, propShareWith+"/")):
		case serverSetProperties[key]:
			return invalidProperty(key, "the server sets this property")
		case key == propRole:
			return invalidProperty(propRole,
				"the role of a mailbox is fixed when it is created; create one with the role and move the messages")
		case !mailboxWritable[key]:
			return invalidProperty(key, "a mailbox has no property "+key)
		}
	}

	return nil
}

// mailboxDestroys runs the destructions of a /set.
func mailboxDestroys(sess *session, req *mailboxSetRequest, out *setResponse) {
	for _, id := range req.Destroy {
		t, err := sess.current().tree()
		if err != nil {
			out.NotDestroyed[id] = &setError{Type: setServerFail, Description: err.Error()}
			continue
		}

		mb, ok := t.byID[id]
		if !ok {
			out.NotDestroyed[id] = &setError{Type: setNotFound}
			continue
		}

		if serr := destroyMailbox(sess, t, &mb, req.OnDestroyRemoveEmails); serr != nil {
			out.NotDestroyed[id] = serr
			continue
		}

		out.Destroyed = append(out.Destroyed, id)

		reloadAfterWrite(sess)
	}
}

// destroyMailbox deletes one mailbox, after the three checks RFC 8621
// 2.5 asks for.
func destroyMailbox(sess *session, t *tree, mb *model.Mailbox, removeEmails bool) *setError {
	if mb.Name == inboxName {
		return &setError{Type: setForbidden, Description: "INBOX cannot be deleted"}
	}

	// IMAP DELETE needs x, and removes the messages with the mailbox.
	if !sess.current().may(mb.ID, 'x') {
		return &setError{Type: setForbidden, Description: "no right to delete this mailbox"}
	}

	if t.children(mb.Name) {
		return &setError{Type: setMailboxHasChild}
	}

	if !removeEmails {
		// Whether the mailbox is empty is something only a reader may
		// learn. Without r the destruction is refused unless the client
		// says the messages may go — IMAP DELETE removes them anyway, and
		// that is what onDestroyRemoveEmails asks for.
		if !sess.current().may(mb.ID, 'r') {
			return &setError{Type: setForbidden,
				Description: "the messages of this mailbox cannot be read; destroy it with onDestroyRemoveEmails"}
		}

		msgs, err := sess.user.ListMessages(mb.ID)
		if err != nil {
			return &setError{Type: setServerFail, Description: err.Error()}
		}

		if len(msgs) > 0 {
			return &setError{Type: setMailboxHasEmail}
		}
	}

	if err := sess.user.DeleteMailbox(mb.Name); err != nil {
		if errors.Is(err, model.ErrNoMailbox) {
			return &setError{Type: setNotFound}
		}

		if errors.Is(err, model.ErrMailboxLocked) {
			metrics.MailboxLockRefusals.WithLabelValues("jmap", "mailbox_set").Inc()

			return &setError{Type: setForbidden, Description: descMailboxDeleting}
		}

		return &setError{Type: setServerFail, Description: err.Error()}
	}

	return nil
}
