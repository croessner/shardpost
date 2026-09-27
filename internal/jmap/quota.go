package jmap

import (
	"context"
	"encoding/json"
	"math/bits"
	"sort"
	"strconv"
	"strings"
)

// Quota (RFC 9425). An account has the limits IMAP GETQUOTA reports: a
// storage limit and a message limit, each one Quota object while it is
// set. A limit of zero means unlimited, and an unlimited resource has no
// object, as it has no resource in the IMAP answer.
//
// The account change log does not record quota changes — the usage moves
// with every delivery, and the limit comes from the user database — so
// the state is not the account counter. It is the rendered values
// themselves, written into the token, which lets Quota/changes compare
// what the client saw with what is there now.

// CapQuota is the capability of the Quota data type (RFC 9425 3).
const CapQuota = "urn:ietf:params:jmap:quota"

// QuotaOptions configure the Quota objects.
type QuotaOptions struct {
	// StorageWarnPercent and MessagesWarnPercent are the lowest
	// configured quota warnings that fire on the way up (quota.warnings
	// with direction "over"), in percent of the limit; they become the
	// warnLimit of the object. Zero: no warnLimit.
	StorageWarnPercent  int64
	MessagesWarnPercent int64
}

// The ids of the two Quota objects, which are also their resource types.
const (
	quotaOctets = "octets"
	quotaCount  = "count"
)

// The properties of a Quota.
const (
	propResourceType = "resourceType"
	propUsed         = "used"
	propHardLimit    = "hardLimit"
	propWarnLimit    = "warnLimit"
	propSoftLimit    = "softLimit"
	propScope        = "scope"
	propDescription  = "description"
)

// quotaProperties are the properties of a Quota. softLimit and
// description may be asked for; this server has neither and leaves them
// out, which RFC 9425 4.1 allows for both.
var quotaProperties = map[string]bool{
	propID: true, propResourceType: true, propUsed: true, propHardLimit: true, propWarnLimit: true,
	propSoftLimit: true, propScope: true, propName: true, propTypes: true, propDescription: true,
}

// quotaScope is the scope of every Quota here: the limits are the
// account's own.
const quotaScope = "account"

// quotaTypes are the data types a Quota counts: the messages of the
// account. RFC 9425 4.1 asks for names of the JMAP data types registry,
// which calls the type Email.
var quotaTypes = []string{"Email"}

// quotaObject is one Quota: the values that move, since everything else
// follows from the id.
type quotaObject struct {
	ID        string
	Used      uint64
	HardLimit uint64
	// WarnLimit is zero when no warning is configured for the resource.
	WarnLimit uint64
}

// name is the name a client shows.
func (q *quotaObject) name() string {
	if q.ID == quotaOctets {
		return "Mail storage"
	}

	return "Mail messages"
}

// render is the object with the properties asked for.
func (q *quotaObject) render(props *[]string) map[string]any {
	all := map[string]any{
		propID: q.ID, propResourceType: q.ID, propUsed: q.Used, propHardLimit: q.HardLimit,
		propScope: quotaScope, propName: q.name(), propTypes: quotaTypes,
	}

	if q.WarnLimit > 0 {
		all[propWarnLimit] = q.WarnLimit
	}

	out := map[string]any{propID: q.ID}

	for p, v := range all {
		if selected(props, p) {
			out[p] = v
		}
	}

	return out
}

// quotaOf is the options of the session's server.
func quotaOf(sess *session) QuotaOptions {
	if sess.srv == nil {
		return QuotaOptions{}
	}

	return sess.srv.o.Quota
}

// loadQuotas reads the account's limits and usage, in id order.
func loadQuotas(sess *session) ([]*quotaObject, *methodError) {
	usage, err := sess.user.QuotaUsage()
	if err != nil {
		return nil, &methodError{Type: errServerFail, Description: err.Error()}
	}

	limit, err := sess.user.QuotaLimit()
	if err != nil {
		return nil, &methodError{Type: errServerFail, Description: err.Error()}
	}

	o := quotaOf(sess)

	var out []*quotaObject

	if limit.Msgs > 0 {
		out = append(out, &quotaObject{ID: quotaCount, Used: unsigned(usage.Msgs), HardLimit: uint64(limit.Msgs),
			WarnLimit: warnLimit(uint64(limit.Msgs), o.MessagesWarnPercent)})
	}

	if limit.Bytes > 0 {
		out = append(out, &quotaObject{ID: quotaOctets, Used: unsigned(usage.Bytes), HardLimit: uint64(limit.Bytes),
			WarnLimit: warnLimit(uint64(limit.Bytes), o.StorageWarnPercent)})
	}

	return out, nil
}

// unsigned is a usage counter as an UnsignedInt; a counter that went
// below zero (a repair pending) reads as nothing used.
func unsigned(n int64) uint64 {
	if n < 0 {
		return 0
	}

	return uint64(n)
}

// warnLimit is the percentage of a limit, or zero when there is no
// warning or it would not come before the limit itself.
func warnLimit(limit uint64, percent int64) uint64 {
	if percent <= 0 || percent >= 100 {
		return 0
	}

	hi, lo := bits.Mul64(limit, uint64(percent))
	w, _ := bits.Div64(hi, lo, 100)

	return w
}

// quotaStateVersion starts every quota state this server writes, so that
// a token of another shape is recognised as unknown rather than misread.
const quotaStateVersion = "q1"

// quotaState writes the values of the objects into a state token:
// "q1", then per object "-", the first letter of its id and
// used.hardLimit.warnLimit. It is opaque to a client and short.
func quotaState(list []*quotaObject) string {
	var b strings.Builder

	b.WriteString(quotaStateVersion)

	for _, q := range list {
		b.WriteString("-")
		b.WriteByte(q.ID[0])
		b.WriteString(strconv.FormatUint(q.Used, 10))
		b.WriteString(".")
		b.WriteString(strconv.FormatUint(q.HardLimit, 10))
		b.WriteString(".")
		b.WriteString(strconv.FormatUint(q.WarnLimit, 10))
	}

	return b.String()
}

// quotaIDByLetter maps the letter of a state entry back to its id.
var quotaIDByLetter = map[byte]string{'c': quotaCount, 'o': quotaOctets}

// parseQuotaState reads a state token back into the objects it
// describes. Anything this server did not write — another version, an
// unknown id, one id twice, a number that is not one — is refused.
func parseQuotaState(s string) (map[string]*quotaObject, bool) {
	rest, ok := strings.CutPrefix(s, quotaStateVersion)
	if !ok {
		return nil, false
	}

	out := map[string]*quotaObject{}

	if rest == "" {
		return out, true
	}

	if !strings.HasPrefix(rest, "-") {
		return nil, false
	}

	for _, entry := range strings.Split(rest[1:], "-") {
		if entry == "" {
			return nil, false
		}

		id, known := quotaIDByLetter[entry[0]]
		if !known || out[id] != nil {
			return nil, false
		}

		fields := strings.Split(entry[1:], ".")
		if len(fields) != 3 {
			return nil, false
		}

		var n [3]uint64

		for i, f := range fields {
			v, err := strconv.ParseUint(f, 10, 64)
			if err != nil {
				return nil, false
			}

			n[i] = v
		}

		out[id] = &quotaObject{ID: id, Used: n[0], HardLimit: n[1], WarnLimit: n[2]}
	}

	return out, true
}

// quotaGet implements Quota/get (RFC 9425 4.2).
func quotaGet(_ context.Context, sess *session, args json.RawMessage) (any, *methodError) {
	var req getRequest
	if merr := forAccount(sess, args, &req); merr != nil {
		return nil, merr
	}

	if req.IDs != nil && len(*req.IDs) > sess.limits.MaxObjectsInGet {
		return nil, &methodError{Type: errRequestTooLarge, Description: descTooManyIDs}
	}

	if merr := checkRequested(req.Properties, quotaProperties); merr != nil {
		return nil, merr
	}

	list, merr := loadQuotas(sess)
	if merr != nil {
		return nil, merr
	}

	out := &getResponse{AccountID: accountID(sess.account), State: quotaState(list), List: []map[string]any{}, NotFound: []string{}}

	byID := map[string]*quotaObject{}

	ids := make([]string, 0, len(list))
	for _, q := range list {
		byID[q.ID] = q
		ids = append(ids, q.ID)
	}

	if req.IDs != nil {
		ids = *req.IDs
	}

	for _, id := range ids {
		q, ok := byID[id]
		if !ok {
			out.NotFound = append(out.NotFound, id)
			continue
		}

		out.List = append(out.List, q.render(req.Properties))
	}

	return out, nil
}

// quotaChanges implements Quota/changes (RFC 9425 4.3). The client's
// state holds the values it saw, so the delta is a comparison: an object
// that appeared is created, one that went (its limit was removed) is
// destroyed, one whose values differ is updated. More changes than
// maxChanges are handed out in steps, each with a state of its own that
// has the reported objects at their new values and the others at the
// old ones.
func quotaChanges(_ context.Context, sess *session, args json.RawMessage) (any, *methodError) {
	var req changesRequest
	if merr := forAccount(sess, args, &req); merr != nil {
		return nil, merr
	}

	limit, merr := changeLimit(req.MaxChanges)
	if merr != nil {
		return nil, merr
	}

	old, ok := parseQuotaState(req.SinceState)
	if !ok {
		return nil, &methodError{Type: errCannotCalculateChanges, Description: descUnknownState}
	}

	list, merr := loadQuotas(sess)
	if merr != nil {
		return nil, merr
	}

	now := map[string]*quotaObject{}
	for _, q := range list {
		now[q.ID] = q
	}

	out := &changesResponse{AccountID: accountID(sess.account), OldState: req.SinceState,
		Created: []string{}, Updated: []string{}, Destroyed: []string{}}

	step, usedOnly := quotaDelta(out, old, now, limit)

	out.NewState = quotaState(inOrder(step))

	// RFC 9425 4.3: ["used"] when that is all that changed of the updated
	// objects, null otherwise — and null too when none was updated, where
	// there is nothing the list would describe.
	var props []string
	if len(out.Updated) > 0 && usedOnly {
		props = []string{propUsed}
	}

	out.UpdatedProperties = &props

	return out, nil
}

// quotaDelta fills in the three lists, at most limit ids, and returns
// the objects as they are after the ids reported, and whether nothing but
// used changed of the updated ones.
func quotaDelta(out *changesResponse, old, now map[string]*quotaObject, limit int) (map[string]*quotaObject, bool) {
	usedOnly := true
	step := map[string]*quotaObject{}

	for id, q := range old {
		step[id] = q
	}

	for _, id := range changedQuotas(old, now) {
		if len(out.Created)+len(out.Updated)+len(out.Destroyed) == limit {
			out.HasMoreChanges = true

			break
		}

		was, is := old[id], now[id]

		switch {
		case was == nil:
			out.Created = append(out.Created, id)
			step[id] = is
		case is == nil:
			out.Destroyed = append(out.Destroyed, id)
			delete(step, id)
		default:
			out.Updated = append(out.Updated, id)
			usedOnly = usedOnly && was.HardLimit == is.HardLimit && was.WarnLimit == is.WarnLimit
			step[id] = is
		}
	}

	return step, usedOnly
}

// changedQuotas are the ids whose object differs between two states, in
// order.
func changedQuotas(old, now map[string]*quotaObject) []string {
	var out []string

	for _, id := range []string{quotaCount, quotaOctets} {
		was, is := old[id], now[id]

		switch {
		case was == nil && is == nil:
		case was == nil || is == nil || *was != *is:
			out = append(out, id)
		}
	}

	return out
}

// inOrder is a set of objects as the list a state is written from.
func inOrder(m map[string]*quotaObject) []*quotaObject {
	var out []*quotaObject

	for _, id := range []string{quotaCount, quotaOctets} {
		if q, ok := m[id]; ok {
			out = append(out, q)
		}
	}

	return out
}

// quotaFilter is the filter of a Quota/query (RFC 9425 4.4): a condition
// or an operator over further filters.
type quotaFilter struct {
	Name         *string       `json:"name"`
	Scope        *string       `json:"scope"`
	ResourceType *string       `json:"resourceType"`
	Type         *string       `json:"type"`
	Operator     string        `json:"operator"`
	Conditions   []quotaFilter `json:"conditions"`
}

// quotaCondType is the filter condition on the data types of a Quota.
const quotaCondType = "type"

// quotaFilterKeys are the properties a quota filter may have.
var quotaFilterKeys = map[string]bool{
	propName: true, propScope: true, propResourceType: true, quotaCondType: true, "operator": true, "conditions": true,
}

// quotaSortKeys are the properties RFC 9425 4.4 allows a Quota/query to
// sort by.
var quotaSortKeys = map[string]bool{propName: true, propUsed: true}

// quotaQuery implements Quota/query (RFC 9425 4.4). An account has two
// objects at most; they are filtered and sorted in memory.
func quotaQuery(_ context.Context, sess *session, args json.RawMessage) (any, *methodError) {
	var req queryRequest
	if merr := forAccount(sess, args, &req); merr != nil {
		return nil, merr
	}

	filter, merr := readQuotaFilter(req.Filter)
	if merr != nil {
		return nil, merr
	}

	for _, c := range req.Sort {
		if !quotaSortKeys[c.Property] {
			return nil, &methodError{Type: errUnsupportedSort, Description: "cannot sort by " + c.Property}
		}

		if !validCollation(c.Collation) {
			return nil, &methodError{Type: errUnsupportedSort, Description: "cannot sort with " + c.Collation}
		}
	}

	list, merr := loadQuotas(sess)
	if merr != nil {
		return nil, merr
	}

	var matched []*quotaObject

	for _, q := range list {
		if filter == nil || filter.matches(q) {
			matched = append(matched, q)
		}
	}

	sortQuotas(matched, req.Sort)

	ids := make([]string, 0, len(matched))
	for _, q := range matched {
		ids = append(ids, q.ID)
	}

	return page(accountID(sess.account), quotaState(list), ids, &req.paging)
}

// readQuotaFilter decodes and checks a Quota/query filter; null is none.
func readQuotaFilter(raw json.RawMessage) (*quotaFilter, *methodError) {
	if len(raw) == 0 || string(raw) == jsonNull {
		return nil, nil
	}

	var f quotaFilter
	if merr := decodeQuotaFilter(raw, &f); merr != nil {
		return nil, merr
	}

	return &f, nil
}

// decodeQuotaFilter decodes one filter and the ones below it. A property
// the filter does not have, or an operator that does not exist, is
// unsupportedFilter: a condition this server ignored would widen the
// result behind the client's back.
func decodeQuotaFilter(raw json.RawMessage, f *quotaFilter) *methodError {
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(raw, &keys); err != nil {
		return &methodError{Type: errInvalidArguments, Arguments: []string{argFilter}, Description: err.Error()}
	}

	for k := range keys {
		if !quotaFilterKeys[k] {
			return &methodError{Type: errUnsupportedFilter, Description: "a quota cannot be filtered by " + k}
		}
	}

	if err := json.Unmarshal(raw, f); err != nil {
		return &methodError{Type: errInvalidArguments, Arguments: []string{argFilter}, Description: err.Error()}
	}

	if f.Operator == "" {
		if len(f.Conditions) > 0 {
			return &methodError{Type: errUnsupportedFilter, Description: "conditions need an operator"}
		}

		return nil
	}

	switch f.Operator {
	case opAND, opOR, opNOT:
	default:
		return &methodError{Type: errUnsupportedFilter, Description: "unknown operator " + f.Operator}
	}

	var subs []json.RawMessage
	if err := json.Unmarshal(keys["conditions"], &subs); err != nil {
		return &methodError{Type: errInvalidArguments, Arguments: []string{argFilter}, Description: "an operator needs conditions"}
	}

	for i, sub := range subs {
		if merr := decodeQuotaFilter(sub, &f.Conditions[i]); merr != nil {
			return merr
		}
	}

	return nil
}

// matches evaluates the filter over one object.
func (f *quotaFilter) matches(q *quotaObject) bool {
	if f.Operator == "" {
		return f.condition(q)
	}

	// AND is decided by the first condition that fails, OR and NOT by
	// the first that holds.
	decides := f.Operator != opAND

	for i := range f.Conditions {
		if f.Conditions[i].matches(q) == decides {
			return f.Operator == opOR
		}
	}

	return f.Operator != opOR
}

// condition evaluates a filter condition: every property given has to
// hold.
func (f *quotaFilter) condition(q *quotaObject) bool {
	switch {
	case f.Name != nil && !strings.Contains(strings.ToLower(q.name()), strings.ToLower(*f.Name)):
		return false
	case f.Scope != nil && *f.Scope != quotaScope:
		return false
	case f.ResourceType != nil && *f.ResourceType != q.ID:
		return false
	case f.Type != nil && !hasQuotaType(*f.Type):
		return false
	}

	return true
}

// hasQuotaType reports whether a Quota counts the data type. "Mail" is
// the name the examples of RFC 9425 use, and a client written from them
// asks for it; it means the same here.
func hasQuotaType(t string) bool {
	if t == "Mail" {
		return true
	}

	for _, have := range quotaTypes {
		if have == t {
			return true
		}
	}

	return false
}

// sortQuotas applies the comparators, last key first, over the list in
// id order.
func sortQuotas(list []*quotaObject, by []comparator) {
	for i := len(by) - 1; i >= 0; i-- {
		c := by[i]

		less := func(a, b *quotaObject) bool {
			if c.Property == propUsed {
				return a.Used < b.Used
			}

			return collate(c.Collation, a.name()) < collate(c.Collation, b.name())
		}

		sort.SliceStable(list, func(a, b int) bool {
			if !c.ascending() {
				return less(list[b], list[a])
			}

			return less(list[a], list[b])
		})
	}
}

// quotaQueryChanges implements Quota/queryChanges (RFC 9425 4.5). As for
// mailboxes, the result of a query is not a list this server can compute
// a delta against; the client runs the query again, which its /query
// announced with canCalculateChanges false.
func quotaQueryChanges(_ context.Context, sess *session, args json.RawMessage) (any, *methodError) {
	var req queryRequest
	if merr := forAccount(sess, args, &req); merr != nil {
		return nil, merr
	}

	return nil, &methodError{Type: errCannotCalculateChanges,
		Description: "this server does not compute quota query deltas; run the query again"}
}
