package jmap

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"unicode"
	"unicode/utf8"

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

// danglingCreation is what a reference to a creation that never
// happened is answered with.
const danglingCreation = "the creation this one points at did not happen"

// mailboxSet implements Mailbox/set (RFC 8621 2.5): creations first,
// then updates, then destructions, each reported on its own, so that one
// rejected object does not undo the rest.
func mailboxSet(_ context.Context, sess *session, args json.RawMessage) (any, *methodError) {
	var req mailboxSetRequest
	if err := json.Unmarshal(args, &req); err != nil {
		return nil, &methodError{Type: errInvalidArguments, Description: err.Error()}
	}

	if req.AccountID != "" && req.AccountID != accountID(sess.account) {
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

	out := newSetResponse(accountID(sess.account), state)

	mailboxCreates(sess, &req, out)
	mailboxUpdates(sess, &req, out)
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

	since, ok := parseState(before)
	if !ok {
		return before
	}

	entries, ok, err := sess.user.AccountChanges(since, maxMailboxChanges)
	if err != nil || !ok {
		return before
	}

	mine := touchedIDs(out)
	last := since

	for i := range entries {
		if !mine[entries[i].Mailbox] {
			return before
		}

		last = entries[i].Seq
	}

	return stateString(last)
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
func mailboxCreates(sess *session, req *mailboxSetRequest, out *setResponse) {
	pending := map[string]json.RawMessage{}
	for cid, raw := range req.Create {
		pending[cid] = raw
	}

	for len(pending) > 0 && createRound(sess, pending, out) {
		continue
	}

	for cid := range pending {
		out.NotCreated[cid] = &setError{Type: setNotFound, Properties: []string{propParentID},
			Description: danglingCreation}
	}
}

// createRound carries out every creation whose parent is there by now
// and reports whether it got anything done.
func createRound(sess *session, pending map[string]json.RawMessage, out *setResponse) bool {
	progress := false

	for cid, raw := range pending {
		t, err := newTree(sess.user)
		if err != nil {
			out.NotCreated[cid] = &setError{Type: setServerFail, Description: err.Error()}

			delete(pending, cid)

			progress = true

			continue
		}

		created, serr, wait := createMailbox(sess, t, raw)
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
func createMailbox(sess *session, t *tree, raw json.RawMessage) (map[string]any, *setError, bool) {
	var given map[string]json.RawMessage
	if err := json.Unmarshal(raw, &given); err != nil {
		return nil, &setError{Type: setInvalidProperties, Description: err.Error()}, false
	}

	if serr := checkProperties(given); serr != nil {
		return nil, serr, false
	}

	var spec mailboxCreate
	if err := json.Unmarshal(raw, &spec); err != nil {
		return nil, &setError{Type: setInvalidProperties, Description: err.Error()}, false
	}

	parent, wait, serr := parentOf(sess, spec.ParentID)
	if wait || serr != nil {
		return nil, serr, wait
	}

	path, use, serr := placeOf(t, &spec, parent)
	if serr != nil {
		return nil, serr, false
	}

	if err := sess.user.CreateMailbox(path, use); err != nil {
		return nil, createFailed(err), false
	}

	created, serr := finishCreate(sess, path, &spec)

	return created, serr, false
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

	created := map[string]any{propID: mb.ID, propMyRights: ownerRights(path),
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
func checkProperties(given map[string]json.RawMessage) *setError {
	for key := range given {
		switch {
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
	path         string
	moves        bool
}

// mailboxUpdates runs the updates of a /set.
func mailboxUpdates(sess *session, req *mailboxSetRequest, out *setResponse) {
	for id, patch := range req.Update {
		t, err := newTree(sess.user)
		if err != nil {
			out.NotUpdated[id] = &setError{Type: setServerFail, Description: err.Error()}
			continue
		}

		mb, ok := t.byID[id]
		if !ok {
			out.NotUpdated[id] = &setError{Type: setNotFound}
			continue
		}

		if serr := updateMailbox(sess, t, &mb, patch); serr != nil {
			out.NotUpdated[id] = serr
			continue
		}

		out.Updated[id] = nil
	}
}

// updateMailbox applies one patch. Everything is decoded and checked
// first and written afterwards: RFC 8620 5.3 forbids committing half of
// an update to one record, and a rename that failed after a sort order
// had already been written would be exactly that. The rename goes first
// because it is the step that can still fail on the state of the store.
func updateMailbox(sess *session, t *tree, mb *model.Mailbox, raw map[string]json.RawMessage) *setError {
	patch, serr := readPatch(sess, t, mb, raw)
	if serr != nil {
		return serr
	}

	if patch.moves {
		if err := sess.user.RenameMailbox(mb.Name, patch.path); err != nil {
			return renameFailed(err)
		}
	}

	if patch.sortOrder != nil {
		if err := sess.user.SetSortOrder(patch.path, *patch.sortOrder); err != nil {
			return &setError{Type: setServerFail, Description: err.Error()}
		}
	}

	if patch.isSubscribed != nil {
		if err := sess.user.SetSubscribed(patch.path, *patch.isSubscribed); err != nil {
			return &setError{Type: setServerFail, Description: err.Error()}
		}
	}

	return nil
}

// renameFailed maps a failed rename onto the /set error a client sees.
func renameFailed(err error) *setError {
	if errors.Is(err, model.ErrMailboxExists) {
		return invalidProperty(propName, "a mailbox of that name is already there")
	}

	return &setError{Type: setServerFail, Description: err.Error()}
}

// readPatch decodes and validates an update without writing anything.
func readPatch(sess *session, t *tree, mb *model.Mailbox, raw map[string]json.RawMessage) (*mailboxPatch, *setError) {
	if serr := checkPatch(raw); serr != nil {
		return nil, serr
	}

	patch := &mailboxPatch{path: mb.Name}

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

// checkPatch refuses the properties an update may not carry.
func checkPatch(patch map[string]json.RawMessage) *setError {
	for key := range patch {
		switch {
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
		t, err := newTree(sess.user)
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
	}
}

// destroyMailbox deletes one mailbox, after the three checks RFC 8621
// 2.5 asks for.
func destroyMailbox(sess *session, t *tree, mb *model.Mailbox, removeEmails bool) *setError {
	if mb.Name == inboxName {
		return &setError{Type: setForbidden, Description: "INBOX cannot be deleted"}
	}

	if t.children(mb.Name) {
		return &setError{Type: setMailboxHasChild}
	}

	if !removeEmails {
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

		return &setError{Type: setServerFail, Description: err.Error()}
	}

	return nil
}
