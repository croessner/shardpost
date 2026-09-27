package jmap

import (
	"context"
	"encoding/json"
	"sort"
	"strings"

	"shardpost/internal/mailmeta"
	"shardpost/internal/model"
)

// CapMail is the URN of the JMAP Mail capability (RFC 8621).
const CapMail = "urn:ietf:params:jmap:mail"

// roleOfUse maps the RFC 6154 special-use attribute of a mailbox to the
// JMAP role (RFC 8621 2, the IANA registry). INBOX has no attribute and
// is handled by its name.
var roleOfUse = map[string]string{
	`\Archive`:   "archive",
	`\Drafts`:    "drafts",
	`\Junk`:      "junk",
	`\Sent`:      "sent",
	`\Trash`:     "trash",
	`\All`:       "all",
	imapFlagged:  "flagged",
	`\Important`: "important",
}

// inboxName is the one mailbox name IMAP reserves.
const inboxName = "INBOX"

// The Mailbox properties, named once so that the object, the filter and
// the /set all spell them the same way.
const (
	propID            = "id"
	propName          = "name"
	propParentID      = "parentId"
	propRole          = "role"
	propSortOrder     = "sortOrder"
	propIsSubscribed  = "isSubscribed"
	propMyRights      = "myRights"
	propTotalEmails   = "totalEmails"
	propUnreadEmails  = "unreadEmails"
	propTotalThreads  = "totalThreads"
	propUnreadThreads = "unreadThreads"
)

// mailboxRights is what the authenticated account may do with a mailbox
// (RFC 8621 2). In the personal account the rights are the owner's, with
// the two exceptions IMAP imposes on INBOX; in a shared account they are
// mapped from the RFC 4314 rights (see account.myRights).
type mailboxRights struct {
	MayReadItems   bool `json:"mayReadItems"`
	MayAddItems    bool `json:"mayAddItems"`
	MayRemoveItems bool `json:"mayRemoveItems"`
	MaySetSeen     bool `json:"maySetSeen"`
	MaySetKeywords bool `json:"maySetKeywords"`
	MayCreateChild bool `json:"mayCreateChild"`
	MayRename      bool `json:"mayRename"`
	MayDelete      bool `json:"mayDelete"`
	MaySubmit      bool `json:"maySubmit"`
	// MayShare is right a (JMAP Mail Sharing); it is only shown to a
	// request that uses the principals or the mail:share capability.
	MayShare *bool `json:"mayShare,omitempty"`
}

// ownerRights are the rights of the account on its own mailbox. INBOX
// can be neither renamed nor deleted (RFC 9051 6.3.4, 6.3.6), and a
// client is told so rather than finding out from a failed /set.
func ownerRights(name string) mailboxRights {
	movable := name != inboxName

	return mailboxRights{
		MayReadItems: true, MayAddItems: true, MayRemoveItems: true,
		MaySetSeen: true, MaySetKeywords: true, MayCreateChild: true,
		MayRename: movable, MayDelete: movable, MaySubmit: true,
	}
}

// tree is the mailbox list of an account, indexed the way JMAP looks at
// it. IMAP names a mailbox by its path and JMAP by a leaf plus a pointer
// to its parent, and the two have to map onto each other exactly.
type tree struct {
	byID   map[string]model.Mailbox
	byName map[string]model.Mailbox
	all    []model.Mailbox
}

// readTree reads every mailbox of a namespace. Only the personal account
// reads its tree this way; a shared account builds its tree from the
// mailboxes the user may see (account.tree), and a handler asks the
// account for its tree rather than reading one itself.
func readTree(u model.User) (*tree, error) {
	all, err := u.ListMailboxes()
	if err != nil {
		return nil, err
	}

	return buildTree(all), nil
}

// buildTree indexes a mailbox list.
func buildTree(all []model.Mailbox) *tree {
	t := &tree{byID: map[string]model.Mailbox{}, byName: map[string]model.Mailbox{}, all: all}
	for _, mb := range all {
		t.byID[mb.ID] = mb
		t.byName[mb.Name] = mb
	}

	return t
}

// split returns the JMAP name of a mailbox and the id of its parent.
// IMAP allows a path whose intermediate levels have no mailbox of their
// own, and JMAP has no object for such a level; those levels therefore
// stay in the name. The invariant either way is that the path is the
// parent's path, the delimiter and the name.
func (t *tree) split(path string) (name string, parent *string) {
	for i := strings.LastIndex(path, model.Delim); i > 0; i = strings.LastIndex(path[:i], model.Delim) {
		if mb, ok := t.byName[path[:i]]; ok {
			id := mb.ID

			return path[i+len(model.Delim):], &id
		}
	}

	return path, nil
}

// path composes the IMAP path of a mailbox from its JMAP parent and
// name. It is the inverse of split.
func (t *tree) path(parent *string, name string) (string, *setError) {
	if parent == nil || *parent == "" {
		return name, nil
	}

	mb, ok := t.byID[*parent]
	if !ok {
		return "", &setError{Type: setNotFound, Properties: []string{propParentID},
			Description: "no mailbox " + *parent}
	}

	return mb.Name + model.Delim + name, nil
}

// children reports whether any mailbox lies below this one.
func (t *tree) children(path string) bool {
	for _, mb := range t.all {
		if strings.HasPrefix(mb.Name, path+model.Delim) {
			return true
		}
	}

	return false
}

// roleOf is the JMAP role of a mailbox, or "" for none.
func roleOf(mb *model.Mailbox) string {
	if mb.Name == inboxName {
		return "inbox"
	}

	for _, use := range mb.SpecialUse {
		if role, ok := roleOfUse[use]; ok {
			return role
		}
	}

	return ""
}

// useOfRole is the special-use attribute a role asks for, and whether
// the role is one this server can give a new mailbox. "inbox" is not:
// the account has exactly one INBOX and it is made for it.
func useOfRole(role string) (string, bool) {
	for use, r := range roleOfUse {
		if r == role {
			return use, true
		}
	}

	return "", false
}

// counts are the message and thread totals of a mailbox. They cost a
// scan of the mailbox index, the same one an IMAP STATUS costs, so they
// are computed only for the properties a client actually asked for.
type counts struct {
	emails, unread, threads, unreadThreads uint64
}

// countMessages reads the totals of one mailbox as the user sees them:
// in a public folder with its own \Seen marks. A mailbox the user may not
// read counts nothing.
func countMessages(a *account, id string) (counts, error) {
	if !a.personal && !a.may(id, 'r') {
		return counts{}, nil
	}

	msgs, err := a.listMessages(id)
	if err != nil {
		return counts{}, err
	}

	var c counts

	threads := map[string]bool{}  // thread -> any unread message in it
	seen := map[string]struct{}{} // threads encountered

	for i := range msgs {
		c.emails++

		// RFC 8621 2: unread is neither $seen nor $draft. A draft the
		// account wrote itself is not something it has to read.
		unread := !model.HasFlag(msgs[i].Flags, `\Seen`) && !model.HasFlag(msgs[i].Flags, `\Draft`)
		if unread {
			c.unread++
		}

		id := mailmeta.ThreadID(&msgs[i])
		seen[id] = struct{}{}
		threads[id] = threads[id] || unread
	}

	c.threads = uint64(len(seen))

	for _, unread := range threads {
		if unread {
			c.unreadThreads++
		}
	}

	return c, nil
}

// mailboxObject renders one mailbox with the properties the client
// asked for. id is always present, as RFC 8620 5.1 requires.
func mailboxObject(a *account, t *tree, mb *model.Mailbox, props *[]string, share bool) (map[string]any, error) {
	name, parent := t.split(mb.Name)

	out := map[string]any{propID: mb.ID}

	put := func(key string, value any) {
		if selected(props, key) {
			out[key] = value
		}
	}

	put(propName, name)
	put(propParentID, parent)
	put(propSortOrder, mb.SortOrder)
	put(propIsSubscribed, mb.Subscribed)

	if share {
		rights := a.myRights(mb)
		may := a.mayShare(mb.ID)
		rights.MayShare = &may
		put(propMyRights, rights)
		put(propShareWith, shareWithOf(a, mb))
	} else {
		put(propMyRights, a.myRights(mb))
	}

	if selected(props, propRole) {
		out[propRole] = nilIfEmpty(roleOf(mb))
	}

	if !selected(props, propTotalEmails) && !selected(props, propUnreadEmails) &&
		!selected(props, propTotalThreads) && !selected(props, propUnreadThreads) {
		return out, nil
	}

	c, err := countMessages(a, mb.ID)
	if err != nil {
		return nil, err
	}

	put(propTotalEmails, c.emails)
	put(propUnreadEmails, c.unread)
	put(propTotalThreads, c.threads)
	put(propUnreadThreads, c.unreadThreads)

	return out, nil
}

// nilIfEmpty renders an absent string as JSON null.
func nilIfEmpty(s string) any {
	if s == "" {
		return nil
	}

	return s
}

// mailboxProperties are the properties a Mailbox has; a client that
// asks for another one is told so rather than handed a shorter object.
var mailboxProperties = map[string]bool{
	propID: true, propName: true, propParentID: true, propRole: true,
	propSortOrder: true, propIsSubscribed: true, propMyRights: true,
	propTotalEmails: true, propUnreadEmails: true,
	propTotalThreads: true, propUnreadThreads: true,
}

// mailboxPropertiesOf are the properties a Mailbox has for a request:
// shareWith with the capabilities that define it.
func mailboxPropertiesOf(sess *session) map[string]bool {
	if !sess.shareProps() {
		return mailboxProperties
	}

	out := make(map[string]bool, len(mailboxProperties)+1)
	for k := range mailboxProperties {
		out[k] = true
	}

	out[propShareWith] = true

	return out
}

// mailboxGet implements Mailbox/get (RFC 8621 2.1).
func mailboxGet(_ context.Context, sess *session, args json.RawMessage) (any, *methodError) {
	var req getRequest
	if merr := forAccount(sess, args, &req); merr != nil {
		return nil, merr
	}

	if merr := checkRequested(req.Properties, mailboxPropertiesOf(sess)); merr != nil {
		return nil, merr
	}

	if req.IDs != nil && len(*req.IDs) > sess.limits.MaxObjectsInGet {
		return nil, &methodError{Type: errRequestTooLarge, Description: descTooManyIDs}
	}

	state, t, merr := mailboxState(sess)
	if merr != nil {
		return nil, merr
	}

	if req.IDs == nil && len(t.all) > sess.limits.MaxObjectsInGet {
		return nil, &methodError{Type: errRequestTooLarge,
			Description: "the account has more mailboxes than maxObjectsInGet"}
	}

	list, err := mailboxList(sess, t, &req)
	if err != nil {
		return nil, &methodError{Type: errServerFail, Description: err.Error()}
	}

	return &getResponse{
		AccountID: sess.accountID(), State: state,
		List: list, NotFound: missingIDs(t, req.IDs),
	}, nil
}

// mailboxList renders the mailboxes a /get asked for, by id order.
func mailboxList(sess *session, t *tree, req *getRequest) ([]map[string]any, error) {
	out := make([]map[string]any, 0, len(t.all))

	for i := range t.all {
		mb := &t.all[i]
		if req.IDs != nil && !contains(*req.IDs, mb.ID) {
			continue
		}

		obj, err := mailboxObject(sess.current(), t, mb, req.Properties, sess.shareProps())
		if err != nil {
			return nil, err
		}

		out = append(out, obj)
	}

	sort.Slice(out, func(i, j int) bool {
		a, _ := out[i][propID].(string)
		b, _ := out[j][propID].(string)

		return a < b
	})

	return out, nil
}

// missingIDs are the ids a /get asked for that the account does not
// have.
func missingIDs(t *tree, ids *[]string) []string {
	out := []string{}

	if ids == nil {
		return out
	}

	for _, id := range *ids {
		if _, ok := t.byID[id]; !ok {
			out = append(out, id)
		}
	}

	return out
}

// mailboxState reads the account state and the mailbox list together,
// state first: a mailbox that appears in between is then missing from a
// result that claims an older state, which a client repairs on its next
// /changes. The other order would claim a state that does not cover
// what the result already shows.
func mailboxState(sess *session) (string, *tree, *methodError) {
	seq, err := sess.user.AccountState()
	if err != nil {
		return "", nil, &methodError{Type: errServerFail, Description: err.Error()}
	}

	a := sess.current()

	t, err := a.tree()
	if err != nil {
		return "", nil, &methodError{Type: errServerFail, Description: err.Error()}
	}

	return a.state(stateMailboxes, seq), t, nil
}

// contains reports whether the list holds s.
func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}

	return false
}

// maxMailboxChanges bounds one Mailbox/changes page when the client
// names no limit of its own.
const maxMailboxChanges = 256

// mailboxChanges implements Mailbox/changes (RFC 8621 2.2) against the
// account change log.
func mailboxChanges(_ context.Context, sess *session, args json.RawMessage) (any, *methodError) {
	var req changesRequest
	if merr := forAccount(sess, args, &req); merr != nil {
		return nil, merr
	}

	// The tree is read first: in a shared account it decides what the
	// user sees now, and the state token is checked against that.
	a := sess.current()

	t, terr := a.tree()
	if terr != nil {
		return nil, &methodError{Type: errServerFail, Description: terr.Error()}
	}

	since, ok := a.parseState(stateMailboxes, req.SinceState)
	if !ok {
		return nil, &methodError{Type: errCannotCalculateChanges, Description: descUnknownState}
	}

	limit, merr := changeLimit(req.MaxChanges)
	if merr != nil {
		return nil, merr
	}

	entries, ok, err := sess.user.AccountChanges(since, limit)
	if err != nil {
		return nil, &methodError{Type: errServerFail, Description: err.Error()}
	}

	if !ok {
		return nil, &methodError{Type: errCannotCalculateChanges, Description: descLogTrimmed}
	}

	return collectMailboxChanges(sess, req.SinceState, since, entries, len(entries) == limit, t)
}

// changeLimit is the page size of a /changes. RFC 8620 5.2 asks for
// invalidArguments on a maxChanges outside the allowed range, and zero
// is outside it.
func changeLimit(maxChanges *uint64) (int, *methodError) {
	if maxChanges == nil {
		return maxMailboxChanges, nil
	}

	if *maxChanges == 0 {
		return 0, &methodError{Type: errInvalidArguments, Arguments: []string{"maxChanges"},
			Description: "maxChanges must be greater than zero"}
	}

	if *maxChanges < uint64(maxMailboxChanges) {
		return int(*maxChanges), nil
	}

	return maxMailboxChanges, nil
}

// countProperties are the Mailbox properties that change when messages
// come and go, and nothing else does.
var countProperties = []string{propTotalEmails, propUnreadEmails, propTotalThreads, propUnreadThreads}

// changeSet folds one window of the account log into the three lists of
// a /changes response.
type changeSet struct {
	kind   map[string]string // mailbox id -> the folded operation
	counts map[string]bool   // mailbox id -> nothing but its message counts changed
}

// collectMailboxChanges turns a page of account log entries into the
// three lists of a /changes response. A change to the messages of a
// mailbox is a change to the mailbox: its counts are properties of it,
// and a client that is not told stays wrong until it resynchronises.
func collectMailboxChanges(sess *session, sinceState string, since uint64,
	entries []model.AccountChange, full bool, t *tree,
) (any, *methodError) {
	a := sess.current()
	out := &changesResponse{
		AccountID: a.id, OldState: sinceState, NewState: sinceState,
		HasMoreChanges: full,
		Created:        []string{}, Updated: []string{}, Destroyed: []string{},
	}

	set := &changeSet{kind: map[string]string{}, counts: map[string]bool{}}
	last := since

	for i := range entries {
		e := &entries[i]
		last = e.Seq

		if hidden(a, e) {
			continue
		}

		if merr := set.add(e, t); merr != nil {
			return nil, merr
		}
	}

	out.NewState = a.state(stateMailboxes, last)
	props := set.updatedProperties()
	out.UpdatedProperties = &props
	fill(out, set.kind, t)

	return out, nil
}

// hidden reports whether a log entry of a shared account is about
// something the user cannot see: a mailbox without right l, or the
// messages of one without right r. The personal account sees
// everything. The visible set itself cannot have changed since the
// client's state, because the state token carries its fingerprint.
func hidden(a *account, e *model.AccountChange) bool {
	switch {
	case a.personal || e.Mailbox == "":
		return false
	case e.Kind == model.KindEmail:
		return !a.may(e.Mailbox, 'r')
	}

	return a.boxes[e.Mailbox] == nil
}

// add folds one log entry into the set.
func (s *changeSet) add(e *model.AccountChange, t *tree) *methodError {
	if e.Mailbox == "" {
		// A change that named no mailbox: something happened and the log
		// does not say where. Comparing it against what the client holds
		// is the client's own work.
		return &methodError{Type: errCannotCalculateChanges,
			Description: "a change was recorded without its mailbox"}
	}

	if e.Kind == model.KindEmail {
		if _, seen := s.kind[e.Mailbox]; !seen {
			s.kind[e.Mailbox] = string(model.OpUpdated)
			s.counts[e.Mailbox] = true
		}

		return nil
	}

	s.kind[e.Mailbox] = classify(s.kind[e.Mailbox], e.Op)
	s.counts[e.Mailbox] = false

	return s.remapped(e, t)
}

// remapped marks the mailboxes below a created or destroyed one. Their
// JMAP name and parent are read off the tree around them, so a level
// that appears or disappears renames every mailbox under it without any
// of them being written.
func (s *changeSet) remapped(e *model.AccountChange, t *tree) *methodError {
	if e.Op == model.OpUpdated {
		return nil
	}

	if e.Name == "" {
		return &methodError{Type: errCannotCalculateChanges,
			Description: "a mailbox change was recorded without its name"}
	}

	for _, mb := range t.all {
		if !strings.HasPrefix(mb.Name, e.Name+model.Delim) {
			continue
		}

		if _, seen := s.kind[mb.ID]; !seen {
			s.kind[mb.ID] = string(model.OpUpdated)
		}

		s.counts[mb.ID] = false
	}

	return nil
}

// updatedProperties names the properties that changed when the only
// thing that happened to every updated mailbox was its messages coming
// and going. Anything else makes it nil, which RFC 8620 5.2 reads as
// "the server cannot tell".
func (s *changeSet) updatedProperties() []string {
	for id, k := range s.kind {
		if k == string(model.OpUpdated) && !s.counts[id] {
			return nil
		}
	}

	return countProperties
}

// opGone is the fold of an object that came and went inside one window:
// a client that never saw it needs to hear about neither.
const opGone = "gone"

// classify folds the operations an object saw in one window into the one
// list it belongs in: created then destroyed is neither, created then
// changed is still created, and anything else is an update.
func classify(sofar string, op model.AccountOp) string {
	switch {
	case sofar == "":
		return string(op)
	case op == model.OpDestroyed:
		if sofar == string(model.OpCreated) {
			return opGone
		}

		return string(model.OpDestroyed)
	case sofar == string(model.OpCreated) || sofar == opGone:
		return sofar
	}

	return string(model.OpUpdated)
}

// fill puts each id in the list its folded operation asks for, checked
// against what exists now: the log says what happened, the registry says
// what is there, and a client is told the two together.
func fill(out *changesResponse, kind map[string]string, t *tree) {
	for id, k := range kind {
		_, exists := t.byID[id]

		switch {
		case k == opGone:
			continue
		case !exists:
			out.Destroyed = append(out.Destroyed, id)
		case k == string(model.OpCreated):
			out.Created = append(out.Created, id)
		default:
			out.Updated = append(out.Updated, id)
		}
	}

	sort.Strings(out.Created)
	sort.Strings(out.Updated)
	sort.Strings(out.Destroyed)
}
