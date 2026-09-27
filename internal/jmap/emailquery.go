package jmap

import (
	"context"
	"encoding/json"
	"sort"
	"strings"
	"time"

	"shardpost/internal/fts"
	"shardpost/internal/mailmeta"
	"shardpost/internal/model"
)

// Email/query (RFC 8621 4.4). A query is over messages — GUIDs — not over
// placements, so a message in two mailboxes is one result, and its
// keywords and mailboxes are those of all its copies, as Email/get
// reports them.
//
// Where the candidates come from decides the cost. A query limited to
// one mailbox (inMailbox at the top) reads that mailbox. A text search
// across the account takes its candidates from the full-text index, which
// is kept per account and per GUID, and finds them through the locator.
// Anything else reads every mailbox of the account.

// emailFilter is one node of an Email/query filter: an operator over
// further nodes, or a condition whose properties must all hold.
type emailFilter struct {
	Operator                string        `json:"operator"`
	Conditions              []emailFilter `json:"conditions"`
	InMailbox               *string       `json:"inMailbox"`
	InMailboxOtherThan      []string      `json:"inMailboxOtherThan"`
	Before                  *string       `json:"before"`
	After                   *string       `json:"after"`
	MinSize                 *int64        `json:"minSize"`
	MaxSize                 *int64        `json:"maxSize"`
	AllInThreadHaveKeyword  *string       `json:"allInThreadHaveKeyword"`
	SomeInThreadHaveKeyword *string       `json:"someInThreadHaveKeyword"`
	NoneInThreadHaveKeyword *string       `json:"noneInThreadHaveKeyword"`
	HasKeyword              *string       `json:"hasKeyword"`
	NotKeyword              *string       `json:"notKeyword"`
	HasAttachment           *bool         `json:"hasAttachment"`
	Text                    *string       `json:"text"`
	From                    *string       `json:"from"`
	To                      *string       `json:"to"`
	Cc                      *string       `json:"cc"`
	Bcc                     *string       `json:"bcc"`
	Subject                 *string       `json:"subject"`
	Body                    *string       `json:"body"`
	Header                  []string      `json:"header"`
}

// emailComparator is one sort key of an Email/query (RFC 8621 4.4.2).
type emailComparator struct {
	IsAscending *bool  `json:"isAscending"`
	Property    string `json:"property"`
	Keyword     string `json:"keyword"`
	Collation   string `json:"collation"`
}

// ascending reports the direction, which defaults to ascending.
func (c emailComparator) ascending() bool { return c.IsAscending == nil || *c.IsAscending }

// emailSortOptions are the sort properties this server supports, and what
// the mail capability advertises as emailQuerySortOptions.
var emailSortOptions = []string{
	propReceivedAt, propSize, propFrom, propTo, propSubject, propSentAt,
	"hasKeyword", propAllInThreadHave, propSomeInThreadHave,
}

// emailQueryRequest is the argument shape of Email/query.
type emailQueryRequest struct {
	paging
	Filter          json.RawMessage   `json:"filter"`
	AccountID       string            `json:"accountId"`
	Sort            []emailComparator `json:"sort"`
	CollapseThreads bool              `json:"collapseThreads"`
}

// emailItem is one message of a query: a GUID with every copy of it.
type emailItem struct {
	meta     *model.Meta
	boxes    map[string]bool
	keywords map[string]bool
	guid     string
	thread   string
	flags    [][]string
}

// newItem starts an item from one copy.
func newItem(m *model.Meta, mailbox string) *emailItem {
	return &emailItem{guid: m.GUID, meta: m, boxes: map[string]bool{mailbox: true},
		flags: [][]string{m.Flags}, thread: mailmeta.ThreadID(m)}
}

// add merges another copy into the item.
func (it *emailItem) add(m *model.Meta, mailbox string) {
	it.boxes[mailbox] = true
	it.flags = append(it.flags, m.Flags)
	it.keywords = nil
}

// kw is the keyword set of the item across its copies.
func (it *emailItem) kw() map[string]bool {
	if it.keywords == nil {
		it.keywords = keywordsOf(it.flags...)
	}

	return it.keywords
}

// emailQuery is one query being run.
type emailQuery struct {
	sess    *session
	loc     *locator
	threads map[string][]map[string]bool // thread -> keyword sets of its messages
	filter  *cnode
	sorts   []emailComparator
	// threadDep is set when the filter looks at other messages of a
	// thread: a message's match can then change without the message.
	threadDep bool
}

// emailQueryRun implements Email/query.
func emailQueryRun(_ context.Context, sess *session, args json.RawMessage) (any, *methodError) {
	var req emailQueryRequest
	if merr := forAccount(sess, args, &req); merr != nil {
		return nil, merr
	}

	state, q, merr := prepareQuery(sess, req.Filter, req.Sort)
	if merr != nil {
		return nil, merr
	}

	ids, merr := q.run(req.CollapseThreads)
	if merr != nil {
		return nil, merr
	}

	out, merr := page(sess.accountID(), state, ids, &req.paging)
	if merr != nil {
		return nil, merr
	}

	// The position of a message in the default order is its arrival,
	// which never changes, so the delta of such a query can be computed
	// against the log. Any other order, or a collapsed one, cannot.
	out.CanCalculateChanges = defaultSort(req.Sort) && !req.CollapseThreads && !q.threadDep

	return out, nil
}

// prepareQuery reads the state, brings the locator up to it and compiles
// the filter and the sort.
func prepareQuery(sess *session, rawFilter json.RawMessage, sorts []emailComparator) (string, *emailQuery, *methodError) {
	f, merr := readEmailFilter(rawFilter)
	if merr != nil {
		return "", nil, merr
	}

	if merr := checkSorts(sorts); merr != nil {
		return "", nil, merr
	}

	state, loc, merr := emailState(sess)
	if merr != nil {
		return "", nil, merr
	}

	q := &emailQuery{sess: sess, loc: loc, sorts: sorts, threads: map[string][]map[string]bool{}}

	if q.filter, merr = q.compile(f); merr != nil {
		return "", nil, merr
	}

	return state, q, nil
}

// run collects, filters, sorts and collapses, and returns the ids in
// order.
func (q *emailQuery) run(collapse bool) ([]string, *methodError) {
	items, err := q.candidates()
	if err != nil {
		return nil, &methodError{Type: errServerFail, Description: err.Error()}
	}

	matched := make([]*emailItem, 0, len(items))

	for _, it := range items {
		if q.filter.eval(it) {
			matched = append(matched, it)
		}
	}

	q.sortItems(matched)

	ids := make([]string, 0, len(matched))
	seenThread := map[string]bool{}

	for _, it := range matched {
		if collapse {
			if seenThread[it.thread] {
				continue
			}

			seenThread[it.thread] = true
		}

		ids = append(ids, it.guid)
	}

	return ids, nil
}

// readEmailFilter decodes a filter strictly: an unknown condition would
// otherwise be dropped and match everything.
func readEmailFilter(raw json.RawMessage) (*emailFilter, *methodError) {
	if len(raw) == 0 || string(raw) == jsonNull {
		return nil, nil
	}

	if raw[0] != '{' {
		return nil, &methodError{Type: errInvalidArguments, Arguments: []string{argFilter}, Description: descFilterObject}
	}

	f := &emailFilter{}

	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()

	if err := dec.Decode(f); err != nil {
		return nil, &methodError{Type: errUnsupportedFilter, Description: err.Error()}
	}

	return f, nil
}

// defaultSort reports whether a sort is the server's default: arrival,
// newest first.
func defaultSort(sorts []emailComparator) bool {
	return len(sorts) == 0 || (len(sorts) == 1 && sorts[0].Property == propReceivedAt && !sorts[0].ascending())
}

// checkSorts refuses a sort this server cannot do.
func checkSorts(sorts []emailComparator) *methodError {
	for _, c := range sorts {
		if !containsString(emailSortOptions, c.Property) {
			return &methodError{Type: errUnsupportedSort, Description: "cannot sort by " + c.Property}
		}

		if !validCollation(c.Collation) {
			return &methodError{Type: errUnsupportedSort, Description: "cannot sort with " + c.Collation}
		}

		if strings.HasSuffix(c.Property, "eyword") && !validKeyword(strings.ToLower(c.Keyword)) {
			return &methodError{Type: errUnsupportedSort, Description: c.Property + " needs a keyword"}
		}
	}

	return nil
}

// candidates are the messages a query has to look at.
func (q *emailQuery) candidates() ([]*emailItem, error) {
	if box, ok := q.filter.mailbox(); ok {
		return q.mailboxItems(box)
	}

	if guids, ok := q.filter.textCandidates(); ok {
		return q.guidItems(guids)
	}

	return q.accountItems()
}

// mailboxItems reads one mailbox and completes the messages that have
// copies elsewhere, when the query needs to know about them. A mailbox
// the user may not read has no messages, as one that does not exist.
func (q *emailQuery) mailboxItems(mailbox string) ([]*emailItem, error) {
	metas, err := q.sess.current().listMessages(mailbox)
	if err != nil && err != model.ErrNoMailbox {
		return nil, err
	}

	byGUID := map[string]*emailItem{}
	order := []*emailItem{}

	for i := range metas {
		m := &metas[i]
		if it, ok := byGUID[m.GUID]; ok {
			it.add(m, mailbox)
			continue
		}

		it := newItem(m, mailbox)
		byGUID[m.GUID] = it
		order = append(order, it)
	}

	return order, q.complete(byGUID, mailbox)
}

// complete adds the copies in other mailboxes to the items read from one.
// Most messages have one copy; the locator says which have more, and only
// those are looked up.
func (q *emailQuery) complete(items map[string]*emailItem, mailbox string) error {
	guids := make([]string, 0, len(items))
	for g := range items {
		guids = append(guids, g)
	}

	recs, err := q.loc.emailRecs(guids)
	if err != nil {
		return err
	}

	var multi []string

	for g, rec := range recs {
		if len(rec.Places) > 1 {
			multi = append(multi, g)
		}
	}

	hits, err := q.loc.resolve(multi)
	if err != nil {
		return err
	}

	for g, f := range hits {
		for i := range f.metas {
			if box := boxOf(f, i); box != mailbox {
				items[g].add(&f.metas[i], box)
			}
		}
	}

	return nil
}

// boxOf is the mailbox the i-th copy of a found message lives in.
func boxOf(f *found, i int) string {
	return f.places[i].Mailbox
}

// guidItems finds messages by GUID through the locator.
func (q *emailQuery) guidItems(guids map[string]bool) ([]*emailItem, error) {
	list := make([]string, 0, len(guids))
	for g := range guids {
		list = append(list, g)
	}

	hits, err := q.loc.resolve(list)
	if err != nil {
		return nil, err
	}

	out := make([]*emailItem, 0, len(hits))

	for _, f := range hits {
		it := newItem(&f.metas[0], boxOf(f, 0))
		for i := 1; i < len(f.metas); i++ {
			it.add(&f.metas[i], boxOf(f, i))
		}

		out = append(out, it)
	}

	return out, nil
}

// accountItems reads every mailbox of the account the user may read.
func (q *emailQuery) accountItems() ([]*emailItem, error) {
	a := q.sess.current()

	t, err := a.tree()
	if err != nil {
		return nil, err
	}

	byGUID := map[string]*emailItem{}
	order := []*emailItem{}

	for _, mb := range t.all {
		if !a.personal && !a.may(mb.ID, 'r') {
			continue
		}

		metas, err := a.listMessages(mb.ID)
		if err != nil && err != model.ErrNoMailbox {
			return nil, err
		}

		for i := range metas {
			m := &metas[i]
			if it, ok := byGUID[m.GUID]; ok {
				it.add(m, mb.ID)
				continue
			}

			it := newItem(m, mb.ID)
			byGUID[m.GUID] = it
			order = append(order, it)
		}
	}

	return order, nil
}

// threadKeywords are the keyword sets of the messages of a thread, read
// once per thread and query.
func (q *emailQuery) threadKeywords(thread string) []map[string]bool {
	if sets, ok := q.threads[thread]; ok {
		return sets
	}

	var sets []map[string]bool

	members, err := q.loc.threadMembers([]string{thread})
	if err == nil {
		guids := make([]string, 0, len(members[thread]))
		for _, m := range members[thread] {
			guids = append(guids, m.guid)
		}

		if hits, err := q.loc.resolve(guids); err == nil {
			for _, f := range hits {
				sets = append(sets, keywordsOfFound(f))
			}
		}
	}

	q.threads[thread] = sets

	return sets
}

// sortItems orders the matches: the client's comparators, then arrival,
// newest first, then the id, so that equal keys have a stable order.
func (q *emailQuery) sortItems(items []*emailItem) {
	sort.SliceStable(items, func(i, j int) bool {
		for _, c := range q.sorts {
			if cmp := q.compare(items[i], items[j], c); cmp != 0 {
				return cmp < 0
			}
		}

		a, b := items[i].meta.InternalDate, items[j].meta.InternalDate
		if !a.Equal(b) {
			return a.After(b)
		}

		return items[i].guid < items[j].guid
	})
}

// compare compares two messages by one comparator.
func (q *emailQuery) compare(a, b *emailItem, c emailComparator) int {
	cmp := q.key(a, c).compare(q.key(b, c))
	if !c.ascending() {
		cmp = -cmp
	}

	return cmp
}

// emailKey is the value one comparator sorts a message by.
type emailKey struct {
	s string
	n int64
}

// compare orders two keys.
func (k emailKey) compare(o emailKey) int {
	switch {
	case k.n != o.n:
		if k.n < o.n {
			return -1
		}

		return 1
	case k.s != o.s:
		return strings.Compare(k.s, o.s)
	}

	return 0
}

// key is the sort key of a message under one comparator.
func (q *emailQuery) key(it *emailItem, c emailComparator) emailKey {
	env := it.meta.Envelope

	switch c.Property {
	case propReceivedAt:
		return emailKey{n: it.meta.InternalDate.UnixNano()}
	case propSize:
		return emailKey{n: it.meta.Size}
	case propSentAt:
		if env == nil {
			return emailKey{}
		}

		return emailKey{n: env.Date.UnixNano()}
	case propSubject:
		if env == nil {
			return emailKey{}
		}

		return emailKey{s: collate(c.Collation, mailmeta.BaseSubject(env.Subject))}
	case propFrom, propTo:
		return emailKey{s: collate(c.Collation, addressSortKey(it.meta, c.Property))}
	}

	return emailKey{n: boolKey(q.keywordKey(it, c))}
}

// keywordKey is the value of a keyword sort: true sorts after false in
// ascending order.
func (q *emailQuery) keywordKey(it *emailItem, c emailComparator) bool {
	kw := strings.ToLower(c.Keyword)

	switch c.Property {
	case propAllInThreadHave:
		return allHave(q.threadKeywords(it.thread), kw)
	case propSomeInThreadHave:
		return someHave(q.threadKeywords(it.thread), kw)
	}

	return it.kw()[kw]
}

// boolKey is a boolean as a sort number.
func boolKey(b bool) int64 {
	if b {
		return 1
	}

	return 0
}

// addressSortKey is the from or to sort key of RFC 8621 4.4.2: the name
// of the first address, or its email when it has no name.
func addressSortKey(m *model.Meta, prop string) string {
	if m.Envelope == nil {
		return ""
	}

	list := m.Envelope.From
	if prop == propTo {
		list = m.Envelope.To
	}

	for _, a := range list {
		if a.Host == "" {
			continue
		}

		if a.Name != "" {
			return a.Name
		}

		return a.Mailbox + "@" + a.Host
	}

	return ""
}

// allHave reports whether every set has the keyword (and there is at
// least one set).
func allHave(sets []map[string]bool, kw string) bool {
	if len(sets) == 0 {
		return false
	}

	for _, s := range sets {
		if !s[kw] {
			return false
		}
	}

	return true
}

// someHave reports whether any set has the keyword.
func someHave(sets []map[string]bool, kw string) bool {
	for _, s := range sets {
		if s[kw] {
			return true
		}
	}

	return false
}

// parseUTCDate reads a JMAP UTCDate.
func parseUTCDate(s string) (time.Time, bool) {
	t, err := time.Parse(time.RFC3339, s)

	return t, err == nil
}

// ftsSearch runs one full-text query. The index is the namespace's, so
// in a shared account it finds messages the user may not read; every hit
// is resolved through the locator, which finds a message only where the
// user may read it, so such a hit never becomes a result.
func (q *emailQuery) ftsSearch(fields []string, value string) (map[string]bool, error) {
	return q.sess.user.SearchFields(fields, value)
}

// tokenless reports whether a search value has nothing the full-text
// index can look for.
func tokenless(v string) bool { return len(fts.Tokenize(v)) == 0 }

// emailQueryChangesRequest is the argument shape of Email/queryChanges.
type emailQueryChangesRequest struct {
	MaxChanges      *uint64           `json:"maxChanges"`
	UpToID          *string           `json:"upToId"`
	Filter          json.RawMessage   `json:"filter"`
	AccountID       string            `json:"accountId"`
	SinceQueryState string            `json:"sinceQueryState"`
	Sort            []emailComparator `json:"sort"`
	CalculateTotal  bool              `json:"calculateTotal"`
	CollapseThreads bool              `json:"collapseThreads"`
}

// addedItem is one entry of the added list of a /queryChanges.
type addedItem struct {
	ID    string `json:"id"`
	Index uint64 `json:"index"`
}

// queryChangesResponse is what a /queryChanges answers (RFC 8620 5.6).
type queryChangesResponse struct {
	Total         *uint64     `json:"total,omitempty"`
	AccountID     string      `json:"accountId"`
	OldQueryState string      `json:"oldQueryState"`
	NewQueryState string      `json:"newQueryState"`
	Removed       []string    `json:"removed"`
	Added         []addedItem `json:"added"`
}

// emailQueryChanges implements Email/queryChanges (RFC 8621 4.5) for the
// default order, as decided: arrival, newest first. A message's arrival
// never changes, so its position among the others depends only on which
// messages match — and that can change only for the messages the log
// says changed. The query is run again as it stands now; every message
// that changed since the client's state is reported removed, and those
// that match now are added back at their position. RFC 8620 5.6 allows a
// server to report removed ids that were not in the old result, and this
// is where it does. Any other order, or a collapsed one, is
// cannotCalculateChanges, and the client runs the query again.
func emailQueryChanges(_ context.Context, sess *session, args json.RawMessage) (any, *methodError) {
	var req emailQueryChangesRequest
	if merr := forAccount(sess, args, &req); merr != nil {
		return nil, merr
	}

	if !defaultSort(req.Sort) || req.CollapseThreads {
		return nil, &methodError{Type: errCannotCalculateChanges,
			Description: "query deltas are computed for the default order only; run the query again"}
	}

	state, q, merr := prepareQuery(sess, req.Filter, req.Sort)
	if merr != nil {
		return nil, merr
	}

	// Checked after the query read the rights: a shared account's token
	// has to name what the user may read now.
	since, ok := sess.current().parseState(stateEmails, req.SinceQueryState)
	if !ok {
		return nil, &methodError{Type: errCannotCalculateChanges, Description: descUnknownState}
	}

	now, _ := sess.current().seqOf(state)
	if since > now {
		return nil, &methodError{Type: errCannotCalculateChanges, Description: descUnknownState}
	}

	if q.threadDep {
		return nil, &methodError{Type: errCannotCalculateChanges,
			Description: "a thread condition can change with other messages; run the query again"}
	}

	ids, merr := q.run(false)
	if merr != nil {
		return nil, merr
	}

	// The log is read after the query ran, and to its end: every message
	// the query saw had its change announced before, so it is in what is
	// read — unless its change is still being made, and then no delta can
	// be trusted.
	changed, merr := changedEmails(sess, since, now)
	if merr != nil {
		return nil, merr
	}

	return queryDelta(sess, &req, state, changed, ids)
}

// queryDelta builds the response from the changed messages and the
// query result of now.
func queryDelta(sess *session, req *emailQueryChangesRequest, state string,
	changed map[string]bool, ids []string,
) (any, *methodError) {
	out := &queryChangesResponse{AccountID: sess.accountID(), OldQueryState: req.SinceQueryState,
		NewQueryState: state, Removed: []string{}, Added: []addedItem{}}

	for g := range changed {
		out.Removed = append(out.Removed, g)
	}

	sort.Strings(out.Removed)

	for i, id := range ids {
		if changed[id] {
			out.Added = append(out.Added, addedItem{ID: id, Index: uint64(i)})
		}
	}

	if req.MaxChanges != nil && uint64(len(out.Removed)+len(out.Added)) > *req.MaxChanges {
		return nil, &methodError{Type: errTooManyChanges}
	}

	if req.CalculateTotal {
		total := uint64(len(ids))
		out.Total = &total
	}

	return out, nil
}

// changedEmails are the messages the log records a change of since a
// state, up to the state the query was answered at (until): the
// account's head, or a shared account's visible watermark, beyond which
// nothing the user can see lies. A change recorded without its
// messages cannot be turned into a delta, and neither can a state the log
// no longer covers, nor a change that is announced and not yet recorded.
func changedEmails(sess *session, since, until uint64) (map[string]bool, *methodError) {
	out := map[string]bool{}

	if sess.current().personal {
		now, err := sess.user.AccountState()
		if err != nil {
			return nil, &methodError{Type: errServerFail, Description: err.Error()}
		}

		until = max(until, now)
	}

	for since < until {
		entries, ok, err := sess.user.AccountChanges(since, locPage)
		if err != nil {
			return nil, &methodError{Type: errServerFail, Description: err.Error()}
		}

		if !ok {
			return nil, &methodError{Type: errCannotCalculateChanges, Description: descLogTrimmed}
		}

		if len(entries) == 0 {
			return nil, &methodError{Type: errCannotCalculateChanges,
				Description: "a change is being made; run the query again"}
		}

		if merr := collectChanged(sess.current(), entries, out); merr != nil {
			return nil, merr
		}

		since = entries[len(entries)-1].Seq
	}

	return out, nil
}

// collectChanged adds the messages of a page of log entries; in a shared
// account only those in mailboxes the user may read.
func collectChanged(a *account, entries []model.AccountChange, out map[string]bool) *methodError {
	for i := range entries {
		e := &entries[i]
		if e.Kind != model.KindEmail || hidden(a, e) {
			continue
		}

		if e.Partial && len(e.Msgs) == 0 {
			return &methodError{Type: errCannotCalculateChanges,
				Description: descPartialNoMsgs}
		}

		for _, m := range e.Msgs {
			out[m.GUID] = true
		}
	}

	return nil
}
