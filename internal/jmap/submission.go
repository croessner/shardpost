package jmap

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"strconv"
	"time"

	"shardpost/internal/model"
	"shardpost/internal/outbound"
)

// EmailSubmission (RFC 8621 7). ShardPost sends nothing itself: it hands
// the message to the configured relay, naming the user with XCLIENT (see
// outbound.Relay.Submit), and the relay's own rules decide what the user
// may send. On top of that ShardPost checks the envelope and the From
// against the account's identities, bounds the recipients and removes the
// Bcc field.
//
// A submission is final as soon as the relay has taken it: delayed
// sending (FUTURERELEASE) is not offered, so maxDelayedSend is 0, every
// submission is "final", and a cancellation is cannotUnsend. The record
// keeps sendAt and undoStatus all the same, so that a queue for delayed
// sending can be added without changing what is stored.
//
// The records of an account are one record in the admin store, bounded
// in number and age: RFC 8621 7 lets a server destroy a submission at any
// time after it was sent.

// Submitter hands a message to the relay (outbound.Relay).
type Submitter interface {
	Submit(ctx context.Context, as *outbound.XClient, from string, to []string, raw []byte) (*outbound.Report, error)
}

// The undo states (RFC 8621 7).
const (
	undoPending  = "pending"
	undoFinal    = "final"
	undoCanceled = "canceled"
)

// The set errors of EmailSubmission (RFC 8621 7.5).
const (
	setTooManyRecipients = "tooManyRecipients"
	setNoRecipients      = "noRecipients"
	setInvalidRecipients = "invalidRecipients"
	setForbiddenMailFrom = "forbiddenMailFrom"
	setForbiddenToSend   = "forbiddenToSend"
	setCannotUnsend      = "cannotUnsend"
)

// The properties of an EmailSubmission.
const (
	propIdentityID     = "identityId"
	propEmailID        = "emailId"
	propEmailIDs       = "emailIds"
	propEnvelope       = "envelope"
	propSendAt         = "sendAt"
	propUndoStatus     = "undoStatus"
	propDeliveryStatus = "deliveryStatus"
	propDSNBlobIDs     = "dsnBlobIds"
	propMDNBlobIDs     = "mdnBlobIds"
)

// submissionPrefix is the admin store prefix of the submission records.
const submissionPrefix = "jmap/submission/" // + account -> submissionBook

// submissionKey is the admin store key of an account's submissions.
func submissionKey(account string) string { return submissionPrefix + url.PathEscape(account) }

// envAddress is an Address of an Envelope.
type envAddress struct {
	Parameters map[string]*string `json:"parameters"`
	Email      string             `json:"email"`
}

// envelope is the SMTP envelope of a submission.
type envelope struct {
	MailFrom envAddress   `json:"mailFrom"`
	RcptTo   []envAddress `json:"rcptTo"`
}

// deliveryStatus is the delivery state of one recipient.
type deliveryStatus struct {
	SMTPReply string `json:"smtpReply"`
	Delivered string `json:"delivered"`
	Displayed string `json:"displayed"`
}

// submission is an EmailSubmission as it is kept.
type submission struct {
	SendAt         time.Time                 `json:"sendAt"`
	DeliveryStatus map[string]deliveryStatus `json:"deliveryStatus"`
	Envelope       envelope                  `json:"envelope"`
	ID             string                    `json:"id"`
	IdentityID     string                    `json:"identityId"`
	EmailID        string                    `json:"emailId"`
	ThreadID       string                    `json:"threadId"`
	UndoStatus     string                    `json:"undoStatus"`
}

// submissionChange is one entry of the change log of the submissions.
type submissionChange struct {
	Seq uint64 `json:"seq"`
	ID  string `json:"id"`
	Op  string `json:"op"`
}

// submissionBook is the record of an account's submissions: the
// submissions themselves, the state and a bounded change log.
type submissionBook struct {
	Account string             `json:"account"`
	Subs    []*submission      `json:"subs"`
	Log     []submissionChange `json:"log"`
	State   uint64             `json:"state"`
}

// submissionLog bounds the change log of the submissions.
const submissionLog = 256

// find is the submission with the id.
func (b *submissionBook) find(id string) *submission {
	for _, s := range b.Subs {
		if s.ID == id {
			return s
		}
	}

	return nil
}

// note records a change.
func (b *submissionBook) note(id string, op model.AccountOp) {
	b.State++
	b.Log = append(b.Log, submissionChange{Seq: b.State, ID: id, Op: string(op)})

	if len(b.Log) > submissionLog {
		b.Log = b.Log[len(b.Log)-submissionLog:]
	}
}

// maxSubmissionBytes bounds the record of an account's submissions, well
// below what one admin store record may hold: a submission to many
// recipients carries a delivery status for each.
const maxSubmissionBytes = 1 << 20

// expire drops the submissions over the age, the number and the size an
// account keeps, oldest first; to a client it is a destruction.
func (b *submissionBook) expire(maxAge time.Duration, maxKept int) {
	cut := time.Now().Add(-maxAge)
	kept := b.Subs[:0]

	for i, s := range b.Subs {
		if s.SendAt.Before(cut) || len(b.Subs)-i > maxKept {
			b.note(s.ID, model.OpDestroyed)
			continue
		}

		kept = append(kept, s)
	}

	b.Subs = kept

	for len(b.Subs) > 1 {
		if raw, err := json.Marshal(b); err == nil && len(raw) <= maxSubmissionBytes {
			return
		}

		b.note(b.Subs[0].ID, model.OpDestroyed)
		b.Subs = b.Subs[1:]
	}
}

// loadSubmissions reads the submissions of an account.
func loadSubmissions(kv model.AdminKV, account string, opts *SubmissionOptions) (*submissionBook, error) {
	raw, err := kv.Get(submissionKey(account))

	switch {
	case errors.Is(err, model.ErrNotFound):
		return &submissionBook{Account: account}, nil
	case err != nil:
		return nil, err
	}

	b := &submissionBook{}
	if json.Unmarshal(raw, b) != nil || b.Account != account {
		return &submissionBook{Account: account}, nil
	}

	b.expire(opts.Retention, opts.MaxKept)

	return b, nil
}

// updateSubmissions changes the submissions of an account atomically.
func updateSubmissions(kv model.AdminKV, account string, opts *SubmissionOptions, fn func(*submissionBook) error) error {
	return kv.Update(submissionKey(account), func(cur []byte) ([]byte, error) {
		b := &submissionBook{Account: account}
		if cur != nil {
			if json.Unmarshal(cur, b) != nil || b.Account != account {
				b = &submissionBook{Account: account}
			}
		}

		b.expire(opts.Retention, opts.MaxKept)

		if err := fn(b); err != nil {
			return nil, err
		}

		return json.Marshal(b)
	})
}

// submissionStore is the configuration and the store of submissions.
func submissionStore(sess *session) (*SubmissionOptions, model.AdminKV, *methodError) {
	opts, merr := submissionOf(sess)
	if merr != nil {
		return nil, nil, merr
	}

	if opts.KV == nil || opts.Relay == nil {
		return nil, nil, &methodError{Type: errUnknownMethod, Description: "email submission is not configured"}
	}

	return opts, opts.KV, nil
}

// submissionProperties are the properties of an EmailSubmission.
var submissionProperties = []string{
	propID, propIdentityID, propEmailID, propThreadID, propEnvelope, propSendAt, propUndoStatus,
	propDeliveryStatus, propDSNBlobIDs, propMDNBlobIDs,
}

// submissionKnown is submissionProperties as a set.
var submissionKnown = func() map[string]bool {
	out := map[string]bool{}
	for _, p := range submissionProperties {
		out[p] = true
	}

	return out
}()

// render is a submission with the properties asked for.
func (s *submission) render(props []string) map[string]any {
	out := map[string]any{propID: s.ID}

	for _, p := range props {
		switch p {
		case propIdentityID:
			out[p] = s.IdentityID
		case propEmailID:
			out[p] = s.EmailID
		case propThreadID:
			out[p] = s.ThreadID
		case propEnvelope:
			out[p] = s.Envelope
		case propSendAt:
			out[p] = utcDate(s.SendAt)
		case propUndoStatus:
			out[p] = s.UndoStatus
		case propDeliveryStatus:
			out[p] = s.DeliveryStatus
		case propDSNBlobIDs, propMDNBlobIDs:
			out[p] = []string{}
		}
	}

	return out
}

// emailSubmissionGet implements EmailSubmission/get.
func emailSubmissionGet(_ context.Context, sess *session, args json.RawMessage) (any, *methodError) {
	opts, kv, merr := submissionStore(sess)
	if merr != nil {
		return nil, merr
	}

	var req getRequest
	if merr := forAccount(sess, args, &req); merr != nil {
		return nil, merr
	}

	if merr := checkRequested(req.Properties, submissionKnown); merr != nil {
		return nil, merr
	}

	props := submissionProperties
	if req.Properties != nil {
		props = *req.Properties
	}

	if req.IDs != nil && len(*req.IDs) > sess.limits.MaxObjectsInGet {
		return nil, &methodError{Type: errRequestTooLarge, Description: descTooManyIDs}
	}

	b, err := loadSubmissions(kv, sess.account, opts)
	if err != nil {
		return nil, &methodError{Type: errServerFail, Description: err.Error()}
	}

	out := &getResponse{AccountID: accountID(sess.account), State: stateString(b.State),
		List: []map[string]any{}, NotFound: []string{}}

	var ids []string
	if req.IDs != nil {
		ids = *req.IDs
	} else {
		for _, s := range b.Subs {
			ids = append(ids, s.ID)
		}
	}

	for _, id := range ids {
		if s := b.find(id); s != nil {
			out.List = append(out.List, s.render(props))
		} else {
			out.NotFound = append(out.NotFound, id)
		}
	}

	return out, nil
}

// emailSubmissionChanges implements EmailSubmission/changes from the
// bounded change log.
func emailSubmissionChanges(_ context.Context, sess *session, args json.RawMessage) (any, *methodError) {
	opts, kv, merr := submissionStore(sess)
	if merr != nil {
		return nil, merr
	}

	var req changesRequest
	if merr := forAccount(sess, args, &req); merr != nil {
		return nil, merr
	}

	b, err := loadSubmissions(kv, sess.account, opts)
	if err != nil {
		return nil, &methodError{Type: errServerFail, Description: err.Error()}
	}

	since, ok := parseState(req.SinceState)

	switch {
	case !ok || since > b.State:
		return nil, &methodError{Type: errCannotCalculateChanges, Description: descUnknownState}
	case since < b.State && (len(b.Log) == 0 || b.Log[0].Seq > since+1):
		return nil, &methodError{Type: errCannotCalculateChanges, Description: descLogTrimmed}
	}

	return submissionDelta(sess, b, since, req.MaxChanges), nil
}

// submissionDelta folds the log entries after since into created,
// updated and destroyed.
func submissionDelta(sess *session, b *submissionBook, since uint64, maxChanges *uint64) *changesResponse {
	out := &changesResponse{AccountID: accountID(sess.account), OldState: stateString(since), NewState: stateString(since),
		Created: []string{}, Updated: []string{}, Destroyed: []string{}}

	kind := map[string]string{}
	order := []string{}
	last := since

	for _, c := range b.Log {
		if c.Seq <= since {
			continue
		}

		if _, seen := kind[c.ID]; !seen {
			if maxChanges != nil && uint64(len(order)) >= *maxChanges {
				out.HasMoreChanges = true

				break
			}

			order = append(order, c.ID)
		}

		kind[c.ID] = classify(kind[c.ID], model.AccountOp(c.Op))
		last = c.Seq
	}

	for _, id := range order {
		switch kind[id] {
		case string(model.OpCreated):
			out.Created = append(out.Created, id)
		case string(model.OpDestroyed):
			out.Destroyed = append(out.Destroyed, id)
		case "":
		default:
			out.Updated = append(out.Updated, id)
		}
	}

	out.NewState = stateString(last)

	return out
}

// submissionFilter is the filter of EmailSubmission/query (RFC 8621
// 7.3): a condition, or an operator over further filters.
type submissionFilter struct {
	IdentityIDs   []string           `json:"identityIds"`
	EmailIDs      []string           `json:"emailIds"`
	ThreadIDs     []string           `json:"threadIds"`
	UndoStatus    *string            `json:"undoStatus"`
	Before        *string            `json:"before"`
	After         *string            `json:"after"`
	Operator      string             `json:"operator"`
	Conditions    []submissionFilter `json:"conditions"`
	before, after time.Time
}

// submissionFilterKeys are the properties a submission filter may have.
var submissionFilterKeys = map[string]bool{
	"identityIds": true, propEmailIDs: true, "threadIds": true, propUndoStatus: true,
	filterBefore: true, filterAfter: true, filterOperator: true, filterConditions: true,
}

// operator implements opFilter.
func (f *submissionFilter) operator() string { return f.Operator }

// conditionCount implements opFilter.
func (f *submissionFilter) conditionCount() int { return len(f.Conditions) }

// submissionChild is the i-th filter below an operator.
func submissionChild(f *submissionFilter, i int) *submissionFilter { return &f.Conditions[i] }

// readSubmissionFilter decodes the filter of a query; nil when there is
// none. A property, operator or date this server cannot evaluate is
// unsupportedFilter rather than a condition silently left out.
func readSubmissionFilter(raw json.RawMessage) (*submissionFilter, *methodError) {
	if len(raw) == 0 || string(raw) == jsonNull {
		return nil, nil
	}

	f := &submissionFilter{}
	if merr := decodeOpFilter(raw, f, submissionFilterKeys, "a submission", submissionChild); merr != nil {
		return nil, merr
	}

	if merr := f.parseDates(); merr != nil {
		return nil, merr
	}

	return f, nil
}

// parseDates reads the dates of a filter and the ones below it.
func (f *submissionFilter) parseDates() *methodError {
	if merr := parseFilterDates(filterDate{f.Before, &f.before, filterBefore}, filterDate{f.After, &f.after, filterAfter}); merr != nil {
		return merr
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
func (f *submissionFilter) condition(s *submission) bool {
	switch {
	case f.IdentityIDs != nil && !containsString(f.IdentityIDs, s.IdentityID),
		f.EmailIDs != nil && !containsString(f.EmailIDs, s.EmailID),
		f.ThreadIDs != nil && !containsString(f.ThreadIDs, s.ThreadID),
		f.UndoStatus != nil && *f.UndoStatus != s.UndoStatus,
		f.Before != nil && !s.SendAt.Before(f.before),
		f.After != nil && s.SendAt.Before(f.after):
		return false
	}

	return true
}

// matches reports whether a submission matches the filter.
func (f *submissionFilter) matches(s *submission) bool {
	if f == nil {
		return true
	}

	return matchOpFilter(f, func(c *submissionFilter) bool { return c.condition(s) }, submissionChild)
}

// submissionQueryRequest is the argument shape of EmailSubmission/query.
type submissionQueryRequest struct {
	paging
	Filter    json.RawMessage `json:"filter"`
	AccountID string          `json:"accountId"`
	Sort      []comparator    `json:"sort"`
}

// submissionSortKeys are what EmailSubmission/query sorts by (RFC 8621
// 7.3 names sentAt, which is the sendAt property).
var submissionSortKeys = map[string]bool{propEmailID: true, propThreadID: true, "sentAt": true}

// emailSubmissionQuery implements EmailSubmission/query.
func emailSubmissionQuery(_ context.Context, sess *session, args json.RawMessage) (any, *methodError) {
	opts, kv, merr := submissionStore(sess)
	if merr != nil {
		return nil, merr
	}

	var req submissionQueryRequest
	if merr := forAccount(sess, args, &req); merr != nil {
		return nil, merr
	}

	filter, merr := readSubmissionFilter(req.Filter)
	if merr != nil {
		return nil, merr
	}

	if merr := checkSort(req.Sort, submissionSortKeys); merr != nil {
		return nil, merr
	}

	b, err := loadSubmissions(kv, sess.account, opts)
	if err != nil {
		return nil, &methodError{Type: errServerFail, Description: err.Error()}
	}

	var matched []*submission

	for _, s := range b.Subs {
		if filter.matches(s) {
			matched = append(matched, s)
		}
	}

	sortSubmissions(matched, req.Sort)

	ids := make([]string, 0, len(matched))
	for _, s := range matched {
		ids = append(ids, s.ID)
	}

	return page(accountID(sess.account), stateString(b.State), ids, &req.paging)
}

// emailSubmissionQueryChanges implements EmailSubmission/queryChanges:
// the list is short and kept in one record, so a client runs the query
// again.
func emailSubmissionQueryChanges(_ context.Context, sess *session, _ json.RawMessage) (any, *methodError) {
	if _, _, merr := submissionStore(sess); merr != nil {
		return nil, merr
	}

	return nil, &methodError{Type: errCannotCalculateChanges, Description: "run the query again"}
}

// sortKeyOf is the value a submission sorts by.
func (s *submission) sortKeyOf(prop string) string {
	switch prop {
	case propEmailID:
		return s.EmailID
	case propThreadID:
		return s.ThreadID
	}

	return strconv.FormatInt(s.SendAt.UnixNano(), 10)
}
