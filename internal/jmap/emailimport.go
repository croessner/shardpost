package jmap

import (
	"context"
	"encoding/json"
	"errors"
	"net/mail"
	"strings"
	"time"

	"shardpost/internal/mailmeta"
	"shardpost/internal/model"
)

// Email/import, Email/copy and Email/parse (RFC 8621 4.7-4.9): messages a
// client has as blobs, stored or only looked at, and the copy between
// accounts that this server, with one account per session, cannot do.

// setInvalidEmail is the set error of a blob that is not a message.
const setInvalidEmail = "invalidEmail"

// emailImportRequest is the argument shape of Email/import.
type emailImportRequest struct {
	Emails    map[string]json.RawMessage `json:"emails"`
	IfInState *string                    `json:"ifInState"`
	AccountID string                     `json:"accountId"`
}

// importResponse is what Email/import answers.
type importResponse struct {
	Created    map[string]map[string]any `json:"created"`
	NotCreated map[string]*setError      `json:"notCreated"`
	AccountID  string                    `json:"accountId"`
	OldState   string                    `json:"oldState"`
	NewState   string                    `json:"newState"`
}

// importable are the properties of an EmailImport.
var importable = map[string]bool{propBlobID: true, propMailboxIDs: true, propKeywords: true, propReceivedAt: true}

// emailImport implements Email/import: each blob is stored as a message
// in the first of its mailboxes and copied to the others.
func emailImport(_ context.Context, sess *session, args json.RawMessage) (any, *methodError) {
	var req emailImportRequest
	if merr := forAccount(sess, args, &req); merr != nil {
		return nil, merr
	}

	if len(req.Emails) > sess.limits.MaxObjectsInSet {
		return nil, &methodError{Type: errRequestTooLarge, Description: "more emails than maxObjectsInSet"}
	}

	state, _, merr := emailState(sess)
	if merr != nil {
		return nil, merr
	}

	if req.IfInState != nil && *req.IfInState != state {
		return nil, &methodError{Type: errStateMismatch}
	}

	out := &importResponse{AccountID: accountID(sess.account), OldState: state, NewState: state,
		Created: map[string]map[string]any{}, NotCreated: map[string]*setError{}}

	mine := map[string]bool{}
	expect := map[string]expectation{}

	for cid, raw := range req.Emails {
		created, exp, serr := importOne(sess, raw)
		if serr != nil {
			out.NotCreated[cid] = serr
			continue
		}

		out.Created[cid] = created
		id, _ := created[propID].(string)
		sess.created[cid] = id
		mine[id] = true
		expect[id] = exp
	}

	out.NewState = reconcile(sess, state, mine, out.Created, nil, nil, expect)

	return out, nil
}

// importOne checks and stores one EmailImport.
func importOne(sess *session, raw json.RawMessage) (map[string]any, expectation, *setError) {
	created, d, serr := importDraft(sess, raw)
	if serr != nil {
		return nil, expectation{}, serr
	}

	return created, expectation{keywords: d.keywords, boxes: d.boxes}, nil
}

// importDraft checks one EmailImport and stores it.
func importDraft(sess *session, raw json.RawMessage) (map[string]any, *draft, *setError) {
	var given map[string]json.RawMessage
	if err := json.Unmarshal(raw, &given); err != nil {
		return nil, nil, &setError{Type: setInvalidProperties, Description: err.Error()}
	}

	for key := range given {
		if !importable[key] {
			return nil, nil, invalidProperty(key, "an EmailImport has no property "+key)
		}
	}

	var blobID string
	if err := json.Unmarshal(given[propBlobID], &blobID); err != nil || blobID == "" {
		return nil, nil, invalidProperty(propBlobID, "an EmailImport names a blob")
	}

	t, err := readTree(sess.user)
	if err != nil {
		return nil, nil, &setError{Type: setServerFail, Description: err.Error()}
	}

	d := &draft{}
	if serr := readPlacement(sess, t, given, d); serr != nil {
		return nil, nil, serr
	}

	msg, err := blobContent(sess, blobID, 0)

	switch {
	case errors.Is(err, model.ErrNoBlob):
		return nil, nil, invalidProperty(propBlobID, "no blob "+blobID)
	case err != nil:
		return nil, nil, &setError{Type: setServerFail, Description: err.Error()}
	case !isMessage(msg):
		return nil, nil, &setError{Type: setInvalidEmail, Description: "the blob is not a message"}
	}

	if _, given := given[propReceivedAt]; !given {
		d.receivedAt = receivedTime(msg)
	}

	created, serr := storeEmail(sess, t, msg, d.boxes, d.keywords, d.receivedAt)

	return created, d, serr
}

// isMessage reports whether raw starts with a header: a message with at
// least one field.
func isMessage(raw []byte) bool {
	hdr, _ := splitHeader(raw)

	return hdr.Len() > 0
}

// receivedTime is the default receivedAt of an import (RFC 8621 4.8): the
// date of the most recent Received field, which is the topmost, or now.
func receivedTime(raw []byte) time.Time {
	hdr, _ := splitHeader(raw)

	if v := hdr.Get("Received"); v != "" {
		if i := strings.LastIndex(v, ";"); i >= 0 {
			if t, err := mail.ParseDate(strings.TrimSpace(unfold(v[i+1:]))); err == nil {
				return t
			}
		}
	}

	return time.Now()
}

// emailCopyRequest is the part of a /copy this server looks at.
type emailCopyRequest struct {
	FromAccountID string `json:"fromAccountId"`
	AccountID     string `json:"accountId"`
}

// emailCopy implements Email/copy, which copies between two accounts. A
// session here has one account, so the source is never there; a copy
// within the account is Email/set on mailboxIds, and RFC 8620 5.4 makes
// the same account on both sides an invalid call.
func emailCopy(_ context.Context, sess *session, args json.RawMessage) (any, *methodError) {
	var req emailCopyRequest
	if merr := forAccount(sess, args, &req); merr != nil {
		return nil, merr
	}

	if req.FromAccountID == accountID(sess.account) {
		return nil, &methodError{Type: errInvalidArguments, Arguments: []string{"fromAccountId"},
			Description: "fromAccountId is the account copied to; within one account, change mailboxIds with Email/set"}
	}

	return nil, &methodError{Type: errFromAccountNotFound}
}

// emailParseRequest is the argument shape of Email/parse: an Email/get
// with blob ids for ids.
type emailParseRequest struct {
	emailGetRequest
	BlobIDs []string `json:"blobIds"`
}

// parseResponse is what Email/parse answers.
type parseResponse struct {
	Parsed      map[string]map[string]any `json:"parsed"`
	AccountID   string                    `json:"accountId"`
	NotParsable []string                  `json:"notParsable"`
	NotFound    []string                  `json:"notFound"`
}

// parseDefaultProperties are the properties Email/parse returns when the
// client names none (RFC 8621 4.9).
var parseDefaultProperties = []string{
	propMessageID, propInReplyTo, propReferences, propSender, propFrom, propTo, propCc, propBcc,
	propReplyTo, propSubject, propSentAt, propHasAttachment, propPreview, propBodyValues,
	propTextBody, propHTMLBody, propAttachments,
}

// parseNull are the properties an Email that is not stored does not have.
var parseNull = map[string]bool{propID: true, propMailboxIDs: true, propKeywords: true, propReceivedAt: true}

// emailParse implements Email/parse: a blob rendered as the Email it
// would be, without storing it.
func emailParse(_ context.Context, sess *session, args json.RawMessage) (any, *methodError) {
	var req emailParseRequest
	if merr := forAccount(sess, args, &req); merr != nil {
		return nil, merr
	}

	if req.Properties == nil {
		req.Properties = &parseDefaultProperties
	}

	props, merr := emailPropertyList(req.Properties)
	if merr != nil {
		return nil, merr
	}

	if req.bodyProps, merr = bodyPropertyList(req.BodyProperties); merr != nil {
		return nil, merr
	}

	if len(req.BlobIDs) > sess.limits.MaxObjectsInGet {
		return nil, &methodError{Type: errRequestTooLarge, Description: "more blobs than maxObjectsInGet"}
	}

	out := &parseResponse{AccountID: accountID(sess.account), Parsed: map[string]map[string]any{},
		NotParsable: []string{}, NotFound: []string{}}

	for _, id := range req.BlobIDs {
		raw, err := blobContent(sess, id, 0)

		switch {
		case errors.Is(err, model.ErrNoBlob):
			out.NotFound = append(out.NotFound, id)
			continue
		case err != nil:
			return nil, &methodError{Type: errServerFail, Description: err.Error()}
		case !isMessage(raw):
			out.NotParsable = append(out.NotParsable, id)
			continue
		}

		obj, merr := parsedEmail(sess, id, raw, props, &req.emailGetRequest)
		if merr != nil {
			return nil, merr
		}

		out.Parsed[id] = obj
	}

	return out, nil
}

// parsedEmail renders a blob as an Email. The index record is made up
// from the blob the way a delivery makes one; the blob id stands in for
// the GUID, so that the parts get blob ids under it.
func parsedEmail(sess *session, id string, raw []byte, props []string, req *emailGetRequest) (map[string]any, *methodError) {
	env, refs, preview := mailmeta.Extract(raw)
	m := model.Meta{GUID: id, Size: int64(len(raw)), Envelope: env, References: refs, Preview: preview}

	obj, merr := emailObject(sess, id, &found{metas: []model.Meta{m}, raw: raw}, props, req)
	if merr != nil {
		return nil, merr
	}

	for p := range obj {
		if parseNull[p] {
			obj[p] = nil
		}
	}

	if _, ok := obj[propThreadID]; ok {
		// The thread follows from References or the Message-ID; a message
		// with neither is a thread of its own, named after an id it does
		// not have yet.
		obj[propThreadID] = nil
		if len(refs) > 0 || (env != nil && len(mailmeta.MessageIDs(env.MessageID)) > 0) {
			obj[propThreadID] = mailmeta.ThreadID(&m)
		}
	}

	if _, ok := obj[propBlobID]; ok {
		obj[propBlobID] = id
	}

	return obj, nil
}
