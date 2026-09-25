package jmap

import (
	"encoding/json"
	"errors"
	"net/mail"
	"os"
	"strings"
	"time"

	nettextproto "net/textproto"

	"shardpost/internal/mailmeta"
	"shardpost/internal/model"
)

// Creating an Email (RFC 8621 4.6): the client describes the message in
// the Email object's own terms — header properties, a body structure or
// the three body lists, body values and blobs — and the server writes the
// message, stores it in the first mailbox and copies it to the others.

// emailCreatable are the properties a creation may carry besides the
// header:* ones; emailServerSet are the ones only the server sets.
var (
	emailCreatable = map[string]bool{
		propMailboxIDs: true, propKeywords: true, propReceivedAt: true,
		propMessageID: true, propInReplyTo: true, propReferences: true, propSender: true,
		propFrom: true, propTo: true, propCc: true, propBcc: true, propReplyTo: true,
		propSubject: true, propSentAt: true,
		propBodyStructure: true, propBodyValues: true, propTextBody: true, propHTMLBody: true, propAttachments: true,
	}
	emailServerSet = map[string]bool{
		propID: true, propBlobID: true, propThreadID: true, propSize: true,
		propPreview: true, propHasAttachment: true, propHeaders: true,
	}
)

// convenienceFields are the header fields the Email object has a
// property of its own for, and the form that property carries.
var convenienceFields = []struct{ prop, field, form string }{
	{propMessageID, fieldMessageID, formMsgIDs},
	{propInReplyTo, fieldInReplyTo, formMsgIDs},
	{propReferences, fieldReferences, formMsgIDs},
	{propSender, fieldSender, formAddresses},
	{propFrom, fieldFrom, formAddresses},
	{propTo, fieldTo, formAddresses},
	{propCc, fieldCc, formAddresses},
	{propBcc, fieldBcc, formAddresses},
	{propReplyTo, fieldReplyTo, formAddresses},
	{propSubject, fieldSubject, formText},
	{propSentAt, fieldDate, formDate},
}

// emailCreates runs the creations of a /set.
func emailCreates(sess *session, create map[string]json.RawMessage, out *setResponse, expect map[string]expectation) {
	for cid, raw := range create {
		created, exp, serr := createEmail(sess, raw)
		if serr != nil {
			out.NotCreated[cid] = serr
			continue
		}

		id, _ := created[propID].(string)
		out.Created[cid] = created
		sess.created[cid] = id
		expect[id] = exp
	}
}

// draft is a creation, decoded and checked.
type draft struct {
	boxes      map[string]bool
	keywords   map[string]bool
	receivedAt time.Time
	top        []hfield
	root       *outPart
}

// createEmail validates one creation, writes the message and stores it.
func createEmail(sess *session, raw json.RawMessage) (map[string]any, expectation, *setError) {
	t, err := newTree(sess.user)
	if err != nil {
		return nil, expectation{}, &setError{Type: setServerFail, Description: err.Error()}
	}

	d, serr := readDraft(sess, t, raw)
	if serr != nil {
		return nil, expectation{}, serr
	}

	msg := writeMessage(d.top, d.root)

	if int64(len(msg)) > sess.limits.MaxSizeUpload {
		return nil, expectation{}, &setError{Type: setTooLarge, Description: descTooLarge}
	}

	created, serr := storeEmail(sess, t, msg, d.boxes, d.keywords, d.receivedAt)

	return created, expectation{keywords: d.keywords, boxes: d.boxes}, serr
}

// storeEmail stores a message in the first of its mailboxes, copies it to
// the others, and returns what a creation reports. A creation is one unit
// (RFC 8620 5.3): when a copy fails, what was stored already is removed
// again, so that a client retrying the creation does not end up with the
// message twice.
func storeEmail(sess *session, t *tree, msg []byte, boxes, keywords map[string]bool,
	receivedAt time.Time,
) (map[string]any, *setError) {
	ids := sortedKeys(boxes)

	flags := make([]string, 0, len(keywords))
	for _, k := range sortedKeys(keywords) {
		flags = append(flags, flagOf(k))
	}

	m, err := sess.user.Deliver(t.byID[ids[0]].Name, msg, flags, receivedAt)
	if err != nil {
		return nil, storeFailed(err)
	}

	stored := []placement{{mailbox: t.byID[ids[0]].Name, meta: *m}}

	for _, id := range ids[1:] {
		copied, err := sess.user.Copy([]model.Meta{*m}, t.byID[id].Name)
		if err != nil {
			for _, pl := range stored {
				if xerr := sess.user.Expunge(pl.mailbox, []model.Meta{pl.meta}); xerr != nil {
					sess.log.Warn("jmap: undoing a failed creation", "guid", m.GUID, "mailbox", pl.mailbox, "err", xerr)
				}
			}

			return nil, storeFailed(err)
		}

		stored = append(stored, placed(t.byID[id].Name, copied)...)
	}

	return map[string]any{propID: m.GUID, propBlobID: m.GUID, propThreadID: mailmeta.ThreadID(m), propSize: m.Size}, nil
}

// readDraft decodes and checks a creation.
func readDraft(sess *session, t *tree, raw json.RawMessage) (*draft, *setError) {
	var given map[string]json.RawMessage
	if err := json.Unmarshal(raw, &given); err != nil {
		return nil, &setError{Type: setInvalidProperties, Description: err.Error()}
	}

	d := &draft{receivedAt: time.Now()}

	if serr := checkCreatable(given); serr != nil {
		return nil, serr
	}

	serr := readPlacement(sess, t, given, d)
	if serr != nil {
		return nil, serr
	}

	if d.root, serr = readBody(sess, given); serr != nil {
		return nil, serr
	}

	if d.top, serr = readTopFields(sess, given, d.root.header); serr != nil {
		return nil, serr
	}

	return d, nil
}

// checkCreatable refuses the properties a creation may not carry.
func checkCreatable(given map[string]json.RawMessage) *setError {
	for key := range given {
		switch {
		case emailServerSet[key]:
			return invalidProperty(key, "the server sets this property")
		case emailCreatable[key]:
		case strings.HasPrefix(key, "header:"):
		default:
			return invalidProperty(key, "an email has no property "+key)
		}
	}

	return nil
}

// readPlacement reads the mailboxes, keywords and arrival of a creation.
func readPlacement(sess *session, t *tree, given map[string]json.RawMessage, d *draft) *setError {
	v, ok := given[propMailboxIDs]
	if !ok {
		return invalidProperty(propMailboxIDs, "an email is created in at least one mailbox")
	}

	var serr *setError
	if d.boxes, serr = readMailboxSet(sess, t, v); serr != nil {
		return serr
	}

	if len(d.boxes) == 0 {
		return invalidProperty(propMailboxIDs, "an email is created in at least one mailbox")
	}

	d.keywords = map[string]bool{}

	if v, ok := given[propKeywords]; ok {
		if d.keywords, serr = readKeywordSet(v); serr != nil {
			return serr
		}
	}

	if v, ok := given[propReceivedAt]; ok {
		var s string
		if err := json.Unmarshal(v, &s); err != nil {
			return invalidProperty(propReceivedAt, err.Error())
		}

		when, ok := parseUTCDate(s)
		if !ok {
			return invalidProperty(propReceivedAt, "not a UTCDate")
		}

		d.receivedAt = when
	}

	return nil
}

// readTopFields reads the header fields of the message itself: the
// convenience properties and the header:* ones, with the Date and the
// Message-ID the server has to add when the client did not. The fields of
// the root body part end up in the same header block, so a field may be
// in only one of the two (RFC 8621 4.6), and the root part counts when
// the server looks for a Date or a Message-ID.
func readTopFields(sess *session, given map[string]json.RawMessage, rootFields []hfield) ([]hfield, *setError) {
	fields, serr := readFields(given, true)
	if serr != nil {
		return nil, serr
	}

	for _, f := range rootFields {
		if hasField(fields, f.name) {
			return nil, invalidProperty("header:"+f.name, "the field is given on the email and on its body part")
		}
	}

	both := append(append([]hfield(nil), fields...), rootFields...)

	if !hasField(both, fieldDate) {
		fields = append(fields, hfield{fieldDate, time.Now().Format(time.RFC1123Z)})
	}

	if !hasField(both, fieldMessageID) {
		fields = append(fields, hfield{"Message-ID", "<" + newBoundary()[2:] + "@" + messageIDHost(sess, fields) + ">"})
	}

	return fields, nil
}

// readFields reads the header fields of an Email or a body part: the
// convenience properties (top only) and header:*. Two properties for one
// field, a Content-* field on the Email, and a
// Content-Transfer-Encoding anywhere are refused.
func readFields(given map[string]json.RawMessage, top bool) ([]hfield, *setError) {
	var fields []hfield

	seen := map[string]string{} // canonical field -> the property that set it

	claim := func(field, prop string) *setError {
		if other, dup := seen[field]; dup {
			return invalidProperty(prop, "the field is also given as "+other)
		}

		seen[field] = prop

		return nil
	}

	if top {
		for _, c := range convenienceFields {
			v, ok := given[c.prop]
			if !ok || string(v) == jsonNull {
				continue
			}

			if serr := claim(c.field, c.prop); serr != nil {
				return nil, serr
			}

			vals, serr := encodeForm(c.form, v, false, c.prop)
			if serr != nil {
				return nil, serr
			}

			for _, s := range vals {
				fields = append(fields, hfield{c.field, s})
			}
		}
	}

	more, serr := readHeaderProps(given, top, claim)
	if serr != nil {
		return nil, serr
	}

	return append(fields, more...), nil
}

// readHeaderProps reads the header:* properties, sorted so that the
// message comes out the same for the same request.
func readHeaderProps(given map[string]json.RawMessage, top bool, claim func(string, string) *setError) ([]hfield, *setError) {
	var fields []hfield

	for _, key := range sortedRawKeys(given) {
		if !strings.HasPrefix(key, "header:") {
			continue
		}

		hp, ok := parseHeaderProperty(key)
		if !ok {
			return nil, invalidProperty(key, "not a header property, or a form the field does not allow")
		}

		if serr := checkHeaderProp(hp, key, top); serr != nil {
			return nil, serr
		}

		if serr := claim(hp.name, key); serr != nil {
			return nil, serr
		}

		vals, serr := encodeForm(hp.form, given[key], hp.all, key)
		if serr != nil {
			return nil, serr
		}

		for _, s := range vals {
			fields = append(fields, hfield{hp.name, s})
		}
	}

	return fields, nil
}

// checkHeaderProp refuses a header:* property for a field the server
// writes itself, or one that belongs elsewhere.
func checkHeaderProp(hp headerProp, key string, top bool) *setError {
	switch {
	case hp.name == fieldCTE || hp.name == fieldMIMEVersion:
		return invalidProperty(key, "the server writes this field")
	case top && strings.HasPrefix(hp.name, "Content-"):
		return invalidProperty(key, "Content-* fields belong to a body part")
	case !top && (hp.name == fieldContentType || hp.name == fieldDisposition):
		return invalidProperty(key, "use the type, name and disposition properties")
	}

	return nil
}

// hasField reports whether a field is among the fields.
func hasField(fields []hfield, name string) bool {
	for _, f := range fields {
		if nettextproto.CanonicalMIMEHeaderKey(f.name) == nettextproto.CanonicalMIMEHeaderKey(name) {
			return true
		}
	}

	return false
}

// messageIDHost is the right-hand side of a generated Message-ID: the
// sender's domain, or the account's, or the host's name.
func messageIDHost(sess *session, fields []hfield) string {
	for _, f := range fields {
		if f.name != fieldFrom {
			continue
		}

		if list, err := mail.ParseAddressList(f.value); err == nil && len(list) > 0 {
			if _, host, ok := strings.Cut(list[0].Address, "@"); ok && host != "" {
				return host
			}
		}
	}

	if _, host, ok := strings.Cut(sess.account, "@"); ok && host != "" {
		return host
	}

	if h, err := os.Hostname(); err == nil && h != "" {
		return h
	}

	return "localhost"
}

// errNotAValue is what a header value of the wrong JSON shape gets.
var errNotAValue = errors.New("not a value of this form")
