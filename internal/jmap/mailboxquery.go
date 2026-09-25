package jmap

import (
	"context"
	"encoding/json"
	"sort"
	"strings"

	"shardpost/internal/model"
)

// queryRequest is the argument shape of a /query (RFC 8620 5.5).
type queryRequest struct {
	Filter         json.RawMessage `json:"filter"`
	Anchor         *string         `json:"anchor"`
	Limit          *uint64         `json:"limit"`
	AccountID      string          `json:"accountId"`
	Sort           []comparator    `json:"sort"`
	Position       int64           `json:"position"`
	AnchorOffset   int64           `json:"anchorOffset"`
	CalculateTotal bool            `json:"calculateTotal"`
}

// comparator is one sort key of a /query.
type comparator struct {
	IsAscending *bool  `json:"isAscending"`
	Property    string `json:"property"`
	Collation   string `json:"collation"`
}

// ascending reports the direction, which defaults to ascending.
func (c comparator) ascending() bool { return c.IsAscending == nil || *c.IsAscending }

// queryResponse is what a /query answers.
type queryResponse struct {
	Total               *uint64  `json:"total,omitempty"`
	AccountID           string   `json:"accountId"`
	QueryState          string   `json:"queryState"`
	IDs                 []string `json:"ids"`
	Position            uint64   `json:"position"`
	Limit               uint64   `json:"limit,omitempty"`
	CanCalculateChanges bool     `json:"canCalculateChanges"`
}

// mailboxFilter is the filter of a Mailbox/query (RFC 8621 2.3). A node
// is either a condition or an operator over further nodes; which one it
// is shows in the "operator" property.
type mailboxFilter struct {
	// ParentID and Role are the two conditions whose value may be null,
	// which means the top level and "no role" — not the same thing as
	// leaving the condition out. They are therefore kept raw and decoded
	// after it is known whether the client sent them at all.
	ParentID     json.RawMessage `json:"parentId"`
	Role         json.RawMessage `json:"role"`
	Name         *string         `json:"name"`
	HasAnyRole   *bool           `json:"hasAnyRole"`
	IsSubscribed *bool           `json:"isSubscribed"`
	Operator     string          `json:"operator"`
	Conditions   []mailboxFilter `json:"conditions"`
}

// wanted decodes a condition whose value may be null: present reports
// that the client sent it, value is what it asked for ("" for null).
func wanted(raw json.RawMessage) (value string, present bool, ok bool) {
	if len(raw) == 0 {
		return "", false, true
	}

	if string(raw) == jsonNull {
		return "", true, true
	}

	if err := json.Unmarshal(raw, &value); err != nil {
		return "", true, false
	}

	return value, true, true
}

// maxQueryLimit bounds one page of a /query.
const maxQueryLimit = 4096

// jsonNull is the literal a client sends for a condition whose absence
// and whose null mean different things.
const jsonNull = "null"

// mailboxQuery implements Mailbox/query (RFC 8621 2.3). The mailbox list
// of an account is small enough to filter and sort in memory; the work
// is in saying exactly what the filter means.
func mailboxQuery(_ context.Context, sess *session, args json.RawMessage) (any, *methodError) {
	var req queryRequest
	if merr := forAccount(sess, args, &req); merr != nil {
		return nil, merr
	}

	filter, merr := readFilter(req.Filter)
	if merr != nil {
		return nil, merr
	}

	if len(req.Sort) > len(mailboxSortKeys) {
		return nil, &methodError{Type: errUnsupportedSort,
			Description: "a mailbox has fewer properties to sort by than that"}
	}

	state, t, merr := mailboxState(sess)
	if merr != nil {
		return nil, merr
	}

	matched, merr := matchMailboxes(t, filter)
	if merr != nil {
		return nil, merr
	}

	if merr := sortMailboxes(t, matched, req.Sort); merr != nil {
		return nil, merr
	}

	return page(accountID(sess.account), state, ids(matched), &req)
}

// readFilter decodes and checks the filter of a query. A filter that is
// not an object at all is a malformed argument; one this server cannot
// evaluate is unsupportedFilter (RFC 8620 5.5).
func readFilter(raw json.RawMessage) (*mailboxFilter, *methodError) {
	if len(raw) == 0 || string(raw) == jsonNull {
		return nil, nil
	}

	if raw[0] != '{' {
		return nil, &methodError{Type: errInvalidArguments, Arguments: []string{"filter"},
			Description: "a filter is an object"}
	}

	filter := &mailboxFilter{}

	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()

	if err := dec.Decode(filter); err != nil {
		// An unknown condition would otherwise be dropped and match
		// everything, which is the worst possible answer to a search.
		return nil, &methodError{Type: errUnsupportedFilter, Description: err.Error()}
	}

	return filter, checkFilter(filter)
}

// matchMailboxes returns the mailboxes the filter selects, in name
// order.
func matchMailboxes(t *tree, f *mailboxFilter) ([]model.Mailbox, *methodError) {
	out := make([]model.Mailbox, 0, len(t.all))

	for _, mb := range t.all {
		ok, merr := matches(t, &mb, f)
		if merr != nil {
			return nil, merr
		}

		if ok {
			out = append(out, mb)
		}
	}

	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })

	return out, nil
}

// matches evaluates one filter node against one mailbox.
func matches(t *tree, mb *model.Mailbox, f *mailboxFilter) (bool, *methodError) {
	switch {
	case f == nil:
		return true, nil
	case f.Operator != "":
		return matchesOperator(t, mb, f)
	}

	return matchesCondition(t, mb, f), nil
}

// matchesCondition evaluates the conditions of one filter node; all of
// the ones a client gave have to hold (RFC 8620 5.5).
func matchesCondition(t *tree, mb *model.Mailbox, f *mailboxFilter) bool {
	name, parent := t.split(mb.Name)

	if want, present, _ := wanted(f.ParentID); present && !sameParent(parent, want) {
		return false
	}

	if want, present, _ := wanted(f.Role); present && roleOf(mb) != want {
		return false
	}

	switch {
	case f.Name != nil && !strings.Contains(strings.ToLower(name), strings.ToLower(*f.Name)):
		return false
	case f.HasAnyRole != nil && (roleOf(mb) != "") != *f.HasAnyRole:
		return false
	case f.IsSubscribed != nil && mb.Subscribed != *f.IsSubscribed:
		return false
	}

	return true
}

// checkFilter refuses a filter this server cannot evaluate: a value of
// the wrong type in a condition whose null has a meaning of its own.
func checkFilter(f *mailboxFilter) *methodError {
	if f == nil {
		return nil
	}

	if _, _, ok := wanted(f.ParentID); !ok {
		return &methodError{Type: errUnsupportedFilter, Description: "parentId must be an id or null"}
	}

	if _, _, ok := wanted(f.Role); !ok {
		return &methodError{Type: errUnsupportedFilter, Description: "role must be a string or null"}
	}

	for i := range f.Conditions {
		if merr := checkFilter(&f.Conditions[i]); merr != nil {
			return merr
		}
	}

	return nil
}

// matchesOperator evaluates an AND, OR or NOT node.
func matchesOperator(t *tree, mb *model.Mailbox, f *mailboxFilter) (bool, *methodError) {
	results := make([]bool, 0, len(f.Conditions))

	for i := range f.Conditions {
		ok, merr := matches(t, mb, &f.Conditions[i])
		if merr != nil {
			return false, merr
		}

		results = append(results, ok)
	}

	switch f.Operator {
	case "AND":
		return !slicesContains(results, false), nil
	case "OR":
		return slicesContains(results, true), nil
	case "NOT":
		return !slicesContains(results, true), nil
	}

	return false, &methodError{Type: errUnsupportedFilter, Description: "unknown operator " + f.Operator}
}

// slicesContains reports whether the list holds v.
func slicesContains(list []bool, v bool) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}

	return false
}

// sameParent compares a mailbox's parent with the one a filter names.
// An empty want is the null the client sent, which is the top level.
func sameParent(have *string, want string) bool {
	if want == "" {
		return have == nil
	}

	return have != nil && *have == want
}

// mailboxSortKeys are the properties RFC 8621 2.3 allows a Mailbox/query
// to sort by.
var mailboxSortKeys = map[string]bool{propName: true, propSortOrder: true, propParentID: true}

// sortMailboxes applies the client's comparators, last key first, over a
// list that is already in name order, so that equal keys keep a stable
// and meaningful order rather than the registry's.
func sortMailboxes(t *tree, list []model.Mailbox, by []comparator) *methodError {
	for i := len(by) - 1; i >= 0; i-- {
		c := by[i]
		if !mailboxSortKeys[c.Property] {
			return &methodError{Type: errUnsupportedSort, Description: "cannot sort by " + c.Property}
		}

		if c.Collation != "" && c.Collation != collationOctet {
			return &methodError{Type: errUnsupportedSort, Description: "cannot sort with " + c.Collation}
		}

		sort.SliceStable(list, func(a, b int) bool {
			less := sortKey(t, &list[a], c.Property) < sortKey(t, &list[b], c.Property)
			if !c.ascending() {
				return sortKey(t, &list[b], c.Property) < sortKey(t, &list[a], c.Property)
			}

			return less
		})
	}

	return nil
}

// sortKey renders the property a comparator sorts by. sortOrder is
// zero-padded so that the string order is the numeric one.
func sortKey(t *tree, mb *model.Mailbox, property string) string {
	name, parent := t.split(mb.Name)

	switch property {
	case propName:
		return name
	case propParentID:
		if parent == nil {
			return ""
		}

		return *parent
	}

	return pad10(mb.SortOrder)
}

// pad10 writes a sort order so that it compares as a number.
func pad10(n uint32) string {
	out := []byte("0000000000")
	for i := len(out) - 1; n > 0; i-- {
		out[i] = byte('0' + n%10)
		n /= 10
	}

	return string(out)
}

// ids is the id list of a result.
func ids(list []model.Mailbox) []string {
	out := make([]string, 0, len(list))
	for _, mb := range list {
		out = append(out, mb.ID)
	}

	return out
}

// page cuts the window the client asked for out of a result: a position,
// or an anchor with an offset, plus a limit (RFC 8620 5.5).
func page(account, state string, all []string, req *queryRequest) (*queryResponse, *methodError) {
	from, merr := startOf(all, req)
	if merr != nil {
		return nil, merr
	}

	limit := uint64(maxQueryLimit)
	if req.Limit != nil && *req.Limit < limit {
		limit = *req.Limit
	}

	window := all[from:]
	if uint64(len(window)) > limit {
		window = window[:limit]
	}

	out := &queryResponse{
		AccountID: account, QueryState: state,
		// The result of a mailbox query is not a stable list this server
		// can compute a delta against; a client re-runs the query.
		CanCalculateChanges: false,
		Position:            uint64(from),
		IDs:                 window,
	}

	// RFC 8620 5.5: the limit is reported only when the server chose a
	// smaller one than the client asked for.
	if req.Limit == nil || *req.Limit > limit {
		out.Limit = limit
	}

	if req.CalculateTotal {
		total := uint64(len(all))
		out.Total = &total
	}

	return out, nil
}

// startOf resolves the position or the anchor of a query window.
func startOf(all []string, req *queryRequest) (int, *methodError) {
	if req.Anchor == nil {
		pos := req.Position
		if pos < 0 {
			pos = int64(len(all)) + pos
		}

		return clampIndex(pos, len(all)), nil
	}

	for i, id := range all {
		if id == *req.Anchor {
			return clampIndex(int64(i)+req.AnchorOffset, len(all)), nil
		}
	}

	return 0, &methodError{Type: errAnchorNotFound}
}

// clampIndex keeps a window start inside the result.
func clampIndex(pos int64, n int) int {
	switch {
	case pos < 0:
		return 0
	case pos > int64(n):
		return n
	}

	return int(pos)
}

// mailboxQueryChanges implements Mailbox/queryChanges (RFC 8621 2.4).
// The query result of this server is not a list it can compute a delta
// against — that would need the result the client last saw — so it says
// so, as its /query already announced with canCalculateChanges false.
func mailboxQueryChanges(_ context.Context, sess *session, args json.RawMessage) (any, *methodError) {
	var req queryRequest
	if merr := forAccount(sess, args, &req); merr != nil {
		return nil, merr
	}

	return nil, &methodError{Type: errCannotCalculateChanges,
		Description: "this server does not compute mailbox query deltas; run the query again"}
}
