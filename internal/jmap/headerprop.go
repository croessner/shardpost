package jmap

import (
	"bytes"
	"net/mail"
	"strings"
	"unicode/utf8"

	nettextproto "net/textproto"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-message/textproto"

	"shardpost/internal/mailmeta"
)

// The header:{name}[:as{Form}][:all] properties of RFC 8621 4.1.2: any
// header of the message, in one of seven parsed forms, either the last
// instance or all of them.

// The header forms.
const (
	formRaw       = "Raw"
	formText      = "Text"
	formAddresses = "Addresses"
	formGrouped   = "GroupedAddresses"
	formMsgIDs    = "MessageIds"
	formDate      = "Date"
	formURLs      = "URLs"
)

// knownHeaderForms are the forms RFC 8621 4.1.2 allows for the header
// fields RFC 5322 and RFC 2369 define (Raw is always allowed). A field
// that is not in the table allows every form: the server cannot know
// what it holds, and the client asked for a form, so it gets one.
var knownHeaderForms = map[string][]string{
	fieldSubject: {formText}, "Comments": {formText}, "Keywords": {formText}, "List-Id": {formText},
	fieldFrom: {formAddresses, formGrouped}, fieldSender: {formAddresses, formGrouped},
	fieldReplyTo: {formAddresses, formGrouped}, fieldTo: {formAddresses, formGrouped},
	fieldCc: {formAddresses, formGrouped}, fieldBcc: {formAddresses, formGrouped},
	"Resent-From": {formAddresses, formGrouped}, "Resent-Sender": {formAddresses, formGrouped},
	"Resent-Reply-To": {formAddresses, formGrouped}, "Resent-To": {formAddresses, formGrouped},
	"Resent-Cc": {formAddresses, formGrouped}, "Resent-Bcc": {formAddresses, formGrouped},
	fieldMessageID: {formMsgIDs}, fieldInReplyTo: {formMsgIDs}, fieldReferences: {formMsgIDs},
	"Resent-Message-Id": {formMsgIDs},
	fieldDate:           {formDate}, "Resent-Date": {formDate},
	"List-Help": {formURLs}, "List-Unsubscribe": {formURLs}, "List-Subscribe": {formURLs},
	"List-Post": {formURLs}, "List-Owner": {formURLs}, "List-Archive": {formURLs},
	"Return-Path": {}, "Received": {},
}

// allForms are the forms a header outside the table allows.
var allForms = []string{formText, formAddresses, formGrouped, formMsgIDs, formDate, formURLs}

// headerProp is a parsed header:* property.
type headerProp struct {
	name string // canonical field name
	form string
	all  bool
}

// parseHeaderProperty parses a header:* property name, and refuses a
// form the field does not allow.
func parseHeaderProperty(p string) (headerProp, bool) {
	rest, ok := strings.CutPrefix(p, "header:")
	if !ok || rest == "" {
		return headerProp{}, false
	}

	hp := headerProp{form: formRaw}

	if r, found := strings.CutSuffix(rest, ":all"); found {
		rest, hp.all = r, true
	}

	if i := strings.Index(rest, ":as"); i >= 0 {
		rest, hp.form = rest[:i], rest[i+len(":as"):]
	}

	if !validFieldName(rest) {
		return headerProp{}, false
	}

	hp.name = nettextproto.CanonicalMIMEHeaderKey(rest)

	return hp, formAllowed(hp.name, hp.form)
}

// validFieldName reports whether s can be a header field name: one or
// more printable ASCII characters other than the colon (RFC 5322 3.6.8).
// CanonicalMIMEHeaderKey returns anything else unchanged, so a name with
// a line break in it would otherwise reach a message being written.
func validFieldName(s string) bool {
	if s == "" {
		return false
	}

	for i := 0; i < len(s); i++ {
		if s[i] < '!' || s[i] > '~' || s[i] == ':' {
			return false
		}
	}

	return true
}

// formAllowed reports whether a field may be read in a form.
func formAllowed(name, form string) bool {
	if form == formRaw {
		return true
	}

	allowed, known := knownHeaderForms[name]
	if !known {
		allowed = allForms
	}

	for _, f := range allowed {
		if f == form {
			return true
		}
	}

	return false
}

// value is the property's value in a header. A header that could not be
// read gives what an absent field gives.
func (hp headerProp) value(h textproto.Header, ok bool) any {
	var raws [][]byte

	if ok {
		fields := h.FieldsByKey(hp.name)
		for fields.Next() {
			if raw, err := fields.Raw(); err == nil {
				raws = append(raws, raw)
			}
		}
	}

	if hp.all {
		out := make([]any, 0, len(raws))
		for _, raw := range raws {
			out = append(out, parseForm(hp.form, rawValue(raw)))
		}

		return out
	}

	if len(raws) == 0 {
		return nil
	}

	// RFC 8621 4.1.2: without :all, the last instance.
	return parseForm(hp.form, rawValue(raws[len(raws)-1]))
}

// rawValue is the Raw form of a field: everything after the colon up to
// the final line break, folding included. RFC 8621 4.1.2 asks for bytes
// that are not UTF-8 to become U+FFFD and for NULs to be dropped.
func rawValue(field []byte) string {
	if i := bytes.IndexByte(field, ':'); i >= 0 {
		field = field[i+1:]
	}

	field = bytes.TrimRight(field, "\r\n")
	field = bytes.ReplaceAll(field, []byte{0}, nil)

	return strings.ToValidUTF8(string(field), string(utf8.RuneError))
}

// parseForm turns a Raw value into the requested form.
func parseForm(form, raw string) any {
	switch form {
	case formText:
		return textForm(raw)
	case formAddresses:
		return addressesForm(raw)
	case formGrouped:
		return groupedForm(raw)
	case formMsgIDs:
		return messageIDs(unfold(raw))
	case formDate:
		return dateForm(raw)
	case formURLs:
		return urlsForm(raw)
	}

	return raw
}

// unfold removes the folding of a header value and trims it.
func unfold(raw string) string {
	return strings.TrimSpace(strings.NewReplacer("\r\n", "", "\n", "").Replace(raw))
}

// wordDecoder decodes RFC 2047 encoded words in any charset Go knows.
var wordDecoder = mailmeta.WordDecoder

// textForm is the Text form: unfolded, encoded words decoded, trimmed.
func textForm(raw string) string {
	v := unfold(raw)

	if dec, err := wordDecoder.DecodeHeader(v); err == nil {
		v = dec
	}

	return strings.TrimSpace(v)
}

// addressesForm is the Addresses form: the address list, groups
// flattened.
func addressesForm(raw string) any {
	list := addressList(mailmeta.ParseAddressList(unfold(raw)))
	if list == nil {
		return []emailAddress{}
	}

	return list
}

// emailAddressGroup is a JMAP EmailAddressGroup (RFC 8621 4.1.2.4).
type emailAddressGroup struct {
	Name      *string        `json:"name"`
	Addresses []emailAddress `json:"addresses"`
}

// groupedForm is the GroupedAddresses form. The parser encodes a group
// the RFC 3501 way — a start entry named after the group, the members,
// and an empty end entry — and addresses outside any group go into a
// group without a name, one per run.
func groupedForm(raw string) []emailAddressGroup {
	out := []emailAddressGroup{}

	var cur *emailAddressGroup

	for _, a := range mailmeta.ParseAddressList(unfold(raw)) {
		switch {
		case a.Host == "" && a.Mailbox != "":
			name := a.Mailbox
			out = append(out, emailAddressGroup{Name: &name, Addresses: []emailAddress{}})
			cur = &out[len(out)-1]
		case a.Host == "" && a.Mailbox == "":
			cur = nil
		default:
			if cur == nil {
				out = append(out, emailAddressGroup{Addresses: []emailAddress{}})
				cur = &out[len(out)-1]
			}

			cur.Addresses = append(cur.Addresses, oneAddress(a))
		}
	}

	return out
}

// oneAddress is one envelope address as a JMAP address.
func oneAddress(a imap.Address) emailAddress {
	addr := emailAddress{Email: a.Mailbox + "@" + a.Host}
	if a.Name != "" {
		name := a.Name
		addr.Name = &name
	}

	return addr
}

// dateForm is the Date form, or null for a value that is not a date.
func dateForm(raw string) any {
	t, err := mail.ParseDate(unfold(raw))
	if err != nil {
		return nil
	}

	return localDate(t)
}

// urlsForm is the URLs form: the URLs in angle brackets of an RFC 2369
// list, or null when there are none.
func urlsForm(raw string) any {
	var out []string

	rest := unfold(raw)

	for {
		start := strings.IndexByte(rest, '<')
		if start < 0 {
			break
		}

		end := strings.IndexByte(rest[start:], '>')
		if end < 0 {
			break
		}

		if u := strings.TrimSpace(rest[start+1 : start+end]); u != "" {
			out = append(out, u)
		}

		rest = rest[start+end+1:]
	}

	if len(out) == 0 {
		return nil
	}

	return out
}

// headerList is the headers property: every field of the header in
// order, by the name the message gave it, with its Raw value.
func headerList(h textproto.Header, ok bool) []map[string]string {
	out := []map[string]string{}

	if !ok {
		return out
	}

	fields := h.Fields()
	for fields.Next() {
		raw, err := fields.Raw()
		if err != nil {
			continue
		}

		name := fields.Key()
		if i := bytes.IndexByte(raw, ':'); i >= 0 {
			name = strings.TrimSpace(string(raw[:i]))
		}

		out = append(out, map[string]string{"name": name, "value": rawValue(raw)})
	}

	return out
}
