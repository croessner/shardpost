package jmap

import (
	"context"
	"encoding/json"
	"sort"
	"time"

	"shardpost/internal/model"
	"shardpost/internal/sharenotice"
)

// ShareNotification (RFC 9670 3). The notices of an account are written
// by whoever changed a mailbox ACL — IMAP SETACL, the admin API, a
// shareWith here — into the account's own namespace (package
// sharenotice); this is the JMAP view of them, in the personal account.
// The server creates them; a client reads them and dismisses them by
// destroying them.

// The ShareNotification properties.
const (
	propCreated         = "created"
	propChangedBy       = "changedBy"
	propObjectType      = "objectType"
	propObjectAccountID = "objectAccountId"
	propObjectID        = "objectId"
	propOldRights       = "oldRights"
	propNewRights       = "newRights"
	propPrincipalID     = "principalId"
	filterAfter         = "after"
	filterBefore        = "before"
	// adminName is the changedBy name of a change made through the admin
	// API, which no principal made.
	adminName = "Administrator"
)

// shareNotificationProperties are the properties of a ShareNotification.
var shareNotificationProperties = map[string]bool{
	propID: true, propCreated: true, propChangedBy: true, propObjectType: true, propObjectAccountID: true,
	propObjectID: true, propOldRights: true, propNewRights: true, propName: true,
}

// noticeLimits are the bounds of what an account keeps.
func (s *Server) noticeLimits() sharenotice.Limits {
	if n := s.o.Principals.Notices; n != nil {
		return n.Limits
	}

	return sharenotice.Limits{}
}

// loadNotices reads the notices of the user.
func loadNotices(sess *session) (*sharenotice.Log, *methodError) {
	l, err := sharenotice.Load(sess.ownView(), sess.account, sess.srv.noticeLimits(), time.Now())
	if err != nil {
		return nil, &methodError{Type: errServerFail, Description: err.Error()}
	}

	return l, nil
}

// noticeRights is oldRights or newRights: the MailboxRights the user had
// through RFC 4314 rights on a mailbox of another namespace, as myRights
// showed them, or null for none.
func noticeRights(rights, name string) any {
	if rights == "" {
		return nil
	}

	r := sharedRights(rights, name)
	share := model.HasRight(rights, 'a')
	r.MayShare = &share

	return r
}

// changedBy is the entity that made a change (RFC 9670 3).
func changedBy(by string) map[string]any {
	if by == "" {
		return map[string]any{propName: adminName, propEmail: nil, propPrincipalID: nil}
	}

	return map[string]any{propName: by, propEmail: emailOf(by), propPrincipalID: by}
}

// noticeObject renders a notice with the properties asked for. The name
// is the mailbox's name in the owner's namespace at the time, the full
// path, since a notice about a revocation is all the user has left to
// tell which mailbox it was.
func noticeObject(n *sharenotice.Notice, props *[]string) map[string]any {
	out := map[string]any{propID: n.ID}

	put := func(key string, value any) {
		if selected(props, key) {
			out[key] = value
		}
	}

	put(propCreated, utcDate(time.Unix(n.Created, 0)))
	put(propChangedBy, changedBy(n.By))
	put(propObjectType, typeMailbox)
	put(propObjectAccountID, accountID(n.Owner))
	put(propObjectID, n.MailboxID)
	put(propOldRights, noticeRights(n.Old, n.Name))
	put(propNewRights, noticeRights(n.New, n.Name))
	put(propName, n.Name)

	return out
}

// shareNotificationGet implements ShareNotification/get (RFC 9670 3.1).
func shareNotificationGet(_ context.Context, sess *session, args json.RawMessage) (any, *methodError) {
	var req getRequest
	if merr := forAccount(sess, args, &req); merr != nil {
		return nil, merr
	}

	if merr := checkRequested(req.Properties, shareNotificationProperties); merr != nil {
		return nil, merr
	}

	if req.IDs != nil && len(*req.IDs) > sess.limits.MaxObjectsInGet {
		return nil, &methodError{Type: errRequestTooLarge, Description: descTooManyIDs}
	}

	l, merr := loadNotices(sess)
	if merr != nil {
		return nil, merr
	}

	out := &getResponse{AccountID: sess.accountID(), State: stateString(l.Seq),
		List: []map[string]any{}, NotFound: []string{}}

	byID := make(map[string]*sharenotice.Notice, len(l.Notices))
	for i := range l.Notices {
		byID[l.Notices[i].ID] = &l.Notices[i]
	}

	if req.IDs == nil {
		for i := range l.Notices {
			out.List = append(out.List, noticeObject(&l.Notices[i], req.Properties))
		}

		return out, nil
	}

	for _, id := range *req.IDs {
		if n := byID[id]; n != nil {
			out.List = append(out.List, noticeObject(n, req.Properties))
		} else {
			out.NotFound = append(out.NotFound, id)
		}
	}

	return out, nil
}

// shareNotificationChanges implements ShareNotification/changes (RFC 9670
// 3.2). The record counts every creation and destruction and remembers
// the destructions as far back as its bounds allow; a state older than
// that is cannotCalculateChanges. A notice never changes, so nothing is
// ever updated.
func shareNotificationChanges(_ context.Context, sess *session, args json.RawMessage) (any, *methodError) {
	var req changesRequest
	if merr := forAccount(sess, args, &req); merr != nil {
		return nil, merr
	}

	limit, merr := changeLimit(req.MaxChanges)
	if merr != nil {
		return nil, merr
	}

	since, ok := parseState(req.SinceState)
	if !ok {
		return nil, &methodError{Type: errCannotCalculateChanges, Description: descUnknownState}
	}

	l, merr := loadNotices(sess)
	if merr != nil {
		return nil, merr
	}

	created, destroyed, state, more, ok := l.Changes(since, limit)
	if !ok {
		return nil, &methodError{Type: errCannotCalculateChanges, Description: descLogTrimmed}
	}

	return &changesResponse{AccountID: sess.accountID(), OldState: req.SinceState, NewState: stateString(state),
		Created: created, Updated: []string{}, Destroyed: destroyed, HasMoreChanges: more}, nil
}

// shareNotificationSet implements ShareNotification/set (RFC 9670 3.3):
// a client may destroy a notice, nothing else.
func shareNotificationSet(_ context.Context, sess *session, args json.RawMessage) (any, *methodError) {
	var req setRequest
	if merr := forAccount(sess, args, &req); merr != nil {
		return nil, merr
	}

	if n := len(req.Create) + len(req.Update) + len(req.Destroy); n > sess.limits.MaxObjectsInSet {
		return nil, &methodError{Type: errRequestTooLarge, Description: descTooManyObjects}
	}

	l, merr := loadNotices(sess)
	if merr != nil {
		return nil, merr
	}

	state := stateString(l.Seq)
	if req.IfInState != nil && *req.IfInState != state {
		return nil, &methodError{Type: errStateMismatch}
	}

	out := newSetResponse(sess.accountID(), state)
	refuse := &setError{Type: setForbidden, Description: "share notifications are created by the server and only destroyed"}

	for cid := range req.Create {
		out.NotCreated[cid] = refuse
	}

	for id := range req.Update {
		out.NotUpdated[id] = refuse
	}

	if len(req.Destroy) == 0 {
		return out, nil
	}

	if merr := destroyNotices(sess, req.Destroy, out); merr != nil {
		return nil, merr
	}

	return out, nil
}

// destroyNotices dismisses notices and reports each id.
func destroyNotices(sess *session, ids []string, out *setResponse) *methodError {
	after, destroyed, err := sharenotice.Destroy(sess.ownView(), sess.account, ids, sess.srv.noticeLimits(), time.Now())
	if err != nil {
		return &methodError{Type: errServerFail, Description: err.Error()}
	}

	gone := map[string]bool{}
	for _, id := range destroyed {
		gone[id] = true
	}

	for _, id := range ids {
		switch {
		case gone[id]:
			if !contains(out.Destroyed, id) {
				out.Destroyed = append(out.Destroyed, id)
			}
		case !contains(out.Destroyed, id):
			out.NotDestroyed[id] = &setError{Type: setNotFound}
		}
	}

	out.NewState = stateString(after.Seq)

	return nil
}

// noticeFilter is the filter of a ShareNotification/query (RFC 9670
// 3.4.1).
type noticeFilter struct {
	After           *string        `json:"after"`
	Before          *string        `json:"before"`
	ObjectType      *string        `json:"objectType"`
	ObjectAccountID *string        `json:"objectAccountId"`
	Operator        string         `json:"operator"`
	Conditions      []noticeFilter `json:"conditions"`
	after, before   time.Time
}

// noticeFilterKeys are the properties a notice filter may have.
var noticeFilterKeys = map[string]bool{
	filterAfter: true, filterBefore: true, propObjectType: true, propObjectAccountID: true,
	filterOperator: true, filterConditions: true,
}

// noticeSortKeys are the properties a ShareNotification/query sorts by;
// RFC 9670 3.4.2 asks for created.
var noticeSortKeys = map[string]bool{propCreated: true}

// operator implements opFilter.
func (f *noticeFilter) operator() string { return f.Operator }

// conditionCount implements opFilter.
func (f *noticeFilter) conditionCount() int { return len(f.Conditions) }

// noticeChild is the i-th filter below an operator.
func noticeChild(f *noticeFilter, i int) *noticeFilter { return &f.Conditions[i] }

// parseDates reads the dates of a filter and the ones below it.
func (f *noticeFilter) parseDates() *methodError {
	for _, d := range []struct {
		s    *string
		into *time.Time
		name string
	}{{f.After, &f.after, filterAfter}, {f.Before, &f.before, filterBefore}} {
		if d.s == nil {
			continue
		}

		t, ok := parseUTCDate(*d.s)
		if !ok {
			return &methodError{Type: errUnsupportedFilter, Description: d.name + " is not a UTCDate"}
		}

		*d.into = t
	}

	for i := range f.Conditions {
		if merr := f.Conditions[i].parseDates(); merr != nil {
			return merr
		}
	}

	return nil
}

// condition evaluates a filter condition: every property given has to
// hold.
func (f *noticeFilter) condition(n *sharenotice.Notice) bool {
	created := time.Unix(n.Created, 0)

	switch {
	case f.After != nil && created.Before(f.after):
		return false
	case f.Before != nil && !created.Before(f.before):
		return false
	case f.ObjectType != nil && *f.ObjectType != typeMailbox:
		return false
	case f.ObjectAccountID != nil && *f.ObjectAccountID != accountID(n.Owner):
		return false
	}

	return true
}

// shareNotificationQuery implements ShareNotification/query (RFC 9670
// 3.4) over the notices of the account, which are few and bounded.
func shareNotificationQuery(_ context.Context, sess *session, args json.RawMessage) (any, *methodError) {
	var req queryRequest
	if merr := forAccount(sess, args, &req); merr != nil {
		return nil, merr
	}

	filter, merr := readNoticeFilter(req.Filter)
	if merr != nil {
		return nil, merr
	}

	if merr := checkSort(req.Sort, noticeSortKeys); merr != nil {
		return nil, merr
	}

	l, merr := loadNotices(sess)
	if merr != nil {
		return nil, merr
	}

	desc := len(req.Sort) > 0 && !req.Sort[0].ascending()

	return page(sess.accountID(), stateString(l.Seq), matchNotices(l, filter, desc), &req.paging)
}

// readNoticeFilter decodes and checks a ShareNotification/query filter;
// null is none.
func readNoticeFilter(raw json.RawMessage) (*noticeFilter, *methodError) {
	if len(raw) == 0 || string(raw) == jsonNull {
		return nil, nil
	}

	f := &noticeFilter{}
	if merr := decodeOpFilter(raw, f, noticeFilterKeys, "a share notification", noticeChild); merr != nil {
		return nil, merr
	}

	if merr := f.parseDates(); merr != nil {
		return nil, merr
	}

	return f, nil
}

// matchNotices are the ids of the notices the filter matches, oldest
// first (or newest, desc): by created, then in the order they were made.
func matchNotices(l *sharenotice.Log, filter *noticeFilter, desc bool) []string {
	matched := make([]*sharenotice.Notice, 0, len(l.Notices))

	for i := range l.Notices {
		n := &l.Notices[i]
		if filter == nil || matchOpFilter(filter, func(c *noticeFilter) bool { return c.condition(n) }, noticeChild) {
			matched = append(matched, n)
		}
	}

	sort.SliceStable(matched, func(i, j int) bool {
		a, b := matched[i], matched[j]
		if desc {
			a, b = b, a
		}

		if a.Created != b.Created {
			return a.Created < b.Created
		}

		return a.Seq < b.Seq
	})

	ids := make([]string, 0, len(matched))
	for _, n := range matched {
		ids = append(ids, n.ID)
	}

	return ids
}

// shareNotificationQueryChanges implements ShareNotification/queryChanges
// (RFC 9670 3.5): the query is cheap to run again, which its /query
// announced with canCalculateChanges false.
func shareNotificationQueryChanges(_ context.Context, sess *session, args json.RawMessage) (any, *methodError) {
	var req queryRequest
	if merr := forAccount(sess, args, &req); merr != nil {
		return nil, merr
	}

	return nil, &methodError{Type: errCannotCalculateChanges,
		Description: "share notification queries are not logged; run the query again"}
}
