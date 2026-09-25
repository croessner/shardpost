package jmap

import (
	"context"
	"encoding/json"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-message/textproto"

	"shardpost/internal/mailmeta"
	"shardpost/internal/model"
)

// The Email properties (RFC 8621 4.1), named once.
const (
	propBlobID        = "blobId"
	propThreadID      = "threadId"
	propMailboxIDs    = "mailboxIds"
	propKeywords      = "keywords"
	propSize          = "size"
	propReceivedAt    = "receivedAt"
	propMessageID     = "messageId"
	propInReplyTo     = "inReplyTo"
	propReferences    = "references"
	propSender        = "sender"
	propFrom          = "from"
	propTo            = "to"
	propCc            = "cc"
	propBcc           = "bcc"
	propReplyTo       = "replyTo"
	propSubject       = "subject"
	propSentAt        = "sentAt"
	propPreview       = "preview"
	propHasAttachment = "hasAttachment"
	propHeaders       = "headers"
	propBodyStructure = "bodyStructure"
	propBodyValues    = "bodyValues"
	propTextBody      = "textBody"
	propHTMLBody      = "htmlBody"
	propAttachments   = "attachments"
)

// emailDefaultProperties are what an Email/get without a property list
// returns (RFC 8621 4.2).
var emailDefaultProperties = []string{
	propID, propBlobID, propThreadID, propMailboxIDs, propKeywords, propSize,
	propReceivedAt, propMessageID, propInReplyTo, propReferences, propSender,
	propFrom, propTo, propCc, propBcc, propReplyTo, propSubject, propSentAt,
	propHasAttachment, propPreview, propBodyValues, propTextBody, propHTMLBody,
	propAttachments,
}

// emailProperties are the fixed properties an Email has; header:*
// properties are recognised by their prefix.
var emailProperties = func() map[string]bool {
	out := map[string]bool{propHeaders: true, propBodyStructure: true}
	for _, p := range emailDefaultProperties {
		out[p] = true
	}

	return out
}()

// headerReadProperties are the ones the index envelope cannot answer.
var headerReadProperties = map[string]bool{propSender: true, propReplyTo: true, propReferences: true}

// emailGetRequest is the argument shape of Email/get: a /get plus what
// RFC 8621 4.2 adds for the body.
type emailGetRequest struct {
	getRequest
	BodyProperties      *[]string `json:"bodyProperties"`
	bodyProps           []string  // BodyProperties, checked, or the default
	MaxBodyValueBytes   uint64    `json:"maxBodyValueBytes"`
	FetchTextBodyValues bool      `json:"fetchTextBodyValues"`
	FetchHTMLBodyValues bool      `json:"fetchHTMLBodyValues"`
	FetchAllBodyValues  bool      `json:"fetchAllBodyValues"`
}

// emailGet implements Email/get (RFC 8621 4.2).
func emailGet(_ context.Context, sess *session, args json.RawMessage) (any, *methodError) {
	var req emailGetRequest
	if merr := forAccount(sess, args, &req.getRequest); merr != nil {
		return nil, merr
	}

	if err := json.Unmarshal(args, &req); err != nil {
		return nil, &methodError{Type: errInvalidArguments, Description: err.Error()}
	}

	props, merr := emailPropertyList(req.Properties)
	if merr != nil {
		return nil, merr
	}

	if req.bodyProps, merr = bodyPropertyList(req.BodyProperties); merr != nil {
		return nil, merr
	}

	// Asking for every message of an account is not something a client
	// needs and not something this server can answer within its limits.
	if req.IDs == nil || len(*req.IDs) > sess.limits.MaxObjectsInGet {
		return nil, &methodError{Type: errRequestTooLarge,
			Description: "Email/get needs ids, at most maxObjectsInGet of them"}
	}

	state, loc, merr := emailState(sess)
	if merr != nil {
		return nil, merr
	}

	hits, err := loc.resolve(*req.IDs)
	if err != nil {
		return nil, &methodError{Type: errServerFail, Description: err.Error()}
	}

	out := &getResponse{AccountID: accountID(sess.account), State: state, List: []map[string]any{}, NotFound: []string{}}

	for _, id := range *req.IDs {
		f, ok := hits[id]
		if !ok {
			out.NotFound = append(out.NotFound, id)
			continue
		}

		obj, merr := emailObject(sess, id, f, props, &req)
		if merr != nil {
			return nil, merr
		}

		out.List = append(out.List, obj)
	}

	return out, nil
}

// emailState reads the account state and brings the locator up to it,
// in that order: the locator then covers at least what the state claims.
func emailState(sess *session) (string, *locator, *methodError) {
	seq, err := sess.user.AccountState()
	if err != nil {
		return "", nil, &methodError{Type: errServerFail, Description: err.Error()}
	}

	loc, err := newLocator(sess.user)
	if err != nil {
		return "", nil, &methodError{Type: errServerFail, Description: err.Error()}
	}

	if err := loc.catchUp(seq); err != nil {
		return "", nil, &methodError{Type: errServerFail, Description: err.Error()}
	}

	return stateString(seq), loc, nil
}

// emailPropertyList checks the properties a client asked for and fills
// in the default list for none.
func emailPropertyList(props *[]string) ([]string, *methodError) {
	if props == nil {
		return emailDefaultProperties, nil
	}

	for _, p := range *props {
		if emailProperties[p] {
			continue
		}

		if _, ok := parseHeaderProperty(p); ok {
			continue
		}

		return nil, &methodError{Type: errInvalidArguments, Arguments: []string{argProperties},
			Description: "no such property: " + p}
	}

	return *props, nil
}

// emailObject renders one message with the properties asked for.
func emailObject(sess *session, id string, f *found, props []string, req *emailGetRequest) (map[string]any, *methodError) {
	m := &f.metas[0]
	out := map[string]any{propID: id}

	var (
		hdr    textproto.Header
		hdrErr error
		hdrOK  bool
	)

	header := func() (textproto.Header, bool) {
		switch {
		case hdrOK || hdrErr != nil:
		case f.raw != nil:
			hdr, _ = splitHeader(f.raw)
			hdrOK = true
		default:
			hdr, hdrErr = readMessageHeader(sess, m)
			hdrOK = hdrErr == nil
		}

		return hdr, hdrOK
	}

	for _, p := range props {
		if v, ok := metaProperty(p, id, m, f); ok {
			out[p] = v
			continue
		}

		if headerReadProperties[p] {
			h, ok := header()
			out[p] = headerProperty(p, h, ok)

			continue
		}

		if p == propHeaders {
			h, ok := header()
			out[p] = headerList(h, ok)

			continue
		}

		if hp, ok := parseHeaderProperty(p); ok {
			h, hok := header()
			out[p] = hp.value(h, hok)
		}
	}

	if merr := bodyProperties(sess, m, f.raw, props, req, out); merr != nil {
		return nil, merr
	}

	return out, nil
}

// readMessageHeader reads the header block of the message's blob.
func readMessageHeader(sess *session, m *model.Meta) (textproto.Header, error) {
	blobs, ok := sess.user.(model.Blobs)
	if !ok {
		return textproto.Header{}, errNoIndex
	}

	return readHeader(blobs, m.GUID, m.Size)
}

// metaProperty answers a property from the index record and the
// locator, and reports whether it could.
func metaProperty(p, id string, m *model.Meta, f *found) (any, bool) {
	switch p {
	case propID:
		return id, true
	case propBlobID:
		return m.GUID, true
	case propThreadID:
		return mailmeta.ThreadID(m), true
	case propMailboxIDs:
		return f.boxes, true
	case propKeywords:
		return keywordsOfFound(f), true
	case propSize:
		return m.Size, true
	case propReceivedAt:
		return utcDate(m.InternalDate), true
	case propPreview:
		return m.Preview, true
	}

	return envelopeProperty(p, m.Envelope)
}

// envelopeProperty answers a header property from the envelope, for the
// properties where the envelope is exact.
func envelopeProperty(p string, env *imap.Envelope) (any, bool) {
	if env == nil {
		env = &imap.Envelope{}
	}

	switch p {
	case propSubject:
		return nilIfEmpty(env.Subject), true
	case propFrom:
		return addressList(env.From), true
	case propTo:
		return addressList(env.To), true
	case propCc:
		return addressList(env.Cc), true
	case propBcc:
		return addressList(env.Bcc), true
	case propSentAt:
		return localDate(env.Date), true
	case propMessageID:
		return messageIDs(env.MessageID), true
	case propInReplyTo:
		if len(env.InReplyTo) == 0 {
			return nil, true
		}

		return messageIDs(env.InReplyTo[0]), true
	}

	return nil, false
}

// headerProperty answers the three properties the envelope gets wrong,
// from the real header. A message whose header could not be read answers
// null, which is also what an absent header gives.
func headerProperty(p string, h textproto.Header, ok bool) any {
	if !ok {
		return nil
	}

	switch p {
	case propSender:
		return addressHeader(h, fieldSender)
	case propReplyTo:
		return addressHeader(h, fieldReplyTo)
	case propReferences:
		if !h.Has(fieldReferences) {
			return nil
		}

		return messageIDs(h.Get(fieldReferences))
	}

	return nil
}

// addressHeader is the address list of a header, or null when the
// message does not have it.
func addressHeader(h textproto.Header, name string) any {
	if !h.Has(name) {
		return nil
	}

	return addressList(mailmeta.ParseAddressList(h.Get(name)))
}

// keywordsOfFound is the keyword set of a message across its copies.
func keywordsOfFound(f *found) map[string]bool {
	sets := make([][]string, len(f.metas))
	for i := range f.metas {
		sets[i] = f.metas[i].Flags
	}

	return keywordsOf(sets...)
}

// emailAddress is a JMAP EmailAddress (RFC 8621 4.1.2.3).
type emailAddress struct {
	Name  *string `json:"name"`
	Email string  `json:"email"`
}

// addressList renders an envelope address list as JMAP addresses. Group
// markers — the RFC 3501 encoding of a group as a start and an end entry
// without a host — carry no address and are left out, as the flat JMAP
// form asks (RFC 8621 4.1.2.3). An absent list is null.
func addressList(list []imap.Address) any {
	if len(list) == 0 {
		return nil
	}

	out := make([]emailAddress, 0, len(list))

	for _, a := range list {
		if a.Host == "" {
			continue
		}

		addr := emailAddress{Email: a.Mailbox + "@" + a.Host}
		if a.Name != "" {
			name := a.Name
			addr.Name = &name
		}

		out = append(out, addr)
	}

	return out
}

// messageIDs parses a Message-ID style header into the ids without angle
// brackets, or null when it holds none.
func messageIDs(v string) any {
	ids := mailmeta.MessageIDs(v)
	if len(ids) == 0 {
		return nil
	}

	return ids
}

// utcDate writes a time as a JMAP UTCDate.
func utcDate(t time.Time) string { return t.UTC().Format(time.RFC3339) }

// localDate writes a time as a JMAP Date, keeping the offset the message
// gave it; a message without a usable Date header has none.
func localDate(t time.Time) any {
	if t.IsZero() {
		return nil
	}

	return t.Format(time.RFC3339)
}
