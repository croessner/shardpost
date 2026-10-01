package jmap

import (
	"encoding/json"
	"errors"
	"net/mail"
	"sort"
	"strings"

	"shardpost/internal/model"
)

// Writing a header field from one of the parsed forms of RFC 8621 4.1.2:
// the inverse of headerprop.go, for a message a client composes.

// encodeForm turns a property value into the values of header fields: one
// for a single form, one per element for an :all property, none for null.
func encodeForm(form string, v json.RawMessage, all bool, prop string) ([]string, *setError) {
	if string(v) == jsonNull {
		return nil, nil
	}

	if !all {
		s, ok, serr := encodeOne(form, v, prop)
		if serr != nil || !ok {
			return nil, serr
		}

		return []string{s}, nil
	}

	var list []json.RawMessage
	if err := json.Unmarshal(v, &list); err != nil {
		return nil, invalidProperty(prop, "an :all property is a list")
	}

	out := make([]string, 0, len(list))

	for _, item := range list {
		s, ok, serr := encodeOne(form, item, prop)
		if serr != nil {
			return nil, serr
		}

		if ok {
			out = append(out, s)
		}
	}

	return out, nil
}

// encodeOne turns one value into a header field value. ok is false for a
// value that makes no field, such as an empty address list.
func encodeOne(form string, v json.RawMessage, prop string) (string, bool, *setError) {
	var (
		s   string
		err error
	)

	switch form {
	case formRaw:
		s, err = rawFieldValue(v)
	case formText:
		s, err = textFieldValue(v)
	case formAddresses:
		s, err = addressFieldValue(v)
	case formGrouped:
		s, err = groupedFieldValue(v)
	case formMsgIDs:
		s, err = msgIDFieldValue(v)
	case formDate:
		s, err = dateFieldValue(v)
	case formURLs:
		s, err = urlFieldValue(v)
	default:
		return "", false, invalidProperty(prop, "no such form")
	}

	if err != nil {
		return "", false, invalidProperty(prop, err.Error())
	}

	if !foldable(s) {
		return "", false, invalidProperty(prop, "a word too long for a header line")
	}

	return s, s != "", nil
}

// maxWord is the longest run without a space a header value may have:
// the writer folds at spaces only, and a line may not pass maxLineOctets
// with the field name in front of it.
const maxWord = maxLineOctets - 100

// foldable reports whether every word of a value fits on a header line.
func foldable(s string) bool {
	for _, w := range strings.Split(s, " ") {
		if len(w) > maxWord {
			return false
		}
	}

	return true
}

// rawFieldValue is a Raw value: taken as it is, unfolded, since the
// writer folds.
func rawFieldValue(v json.RawMessage) (string, error) {
	var s string
	if err := json.Unmarshal(v, &s); err != nil {
		return "", err
	}

	s = strings.TrimLeft(unfold(s), " \t")
	if strings.ContainsAny(s, "\r\n\x00") {
		return "", errNotAValue
	}

	return s, nil
}

// textFieldValue is a Text value, in encoded words where it needs them.
func textFieldValue(v json.RawMessage) (string, error) {
	var s string
	if err := json.Unmarshal(v, &s); err != nil {
		return "", err
	}

	if strings.ContainsAny(s, "\r\n\x00") {
		return "", errNotAValue
	}

	if needsNoEncoding(s) {
		return s, nil
	}

	return encodeText(s), nil
}

// addressFieldValue is an Addresses value.
func addressFieldValue(v json.RawMessage) (string, error) {
	var list []emailAddress
	if err := json.Unmarshal(v, &list); err != nil {
		return "", err
	}

	return formatAddresses(list)
}

// formatAddresses writes addresses, a name in encoded words where it is
// not ASCII.
func formatAddresses(list []emailAddress) (string, error) {
	out := make([]string, 0, len(list))

	for _, a := range list {
		name := ""
		if a.Name != nil {
			name = *a.Name
		}

		if a.Email == "" || strings.ContainsAny(a.Email+name, "\r\n\x00") {
			return "", errNotAValue
		}

		out = append(out, (&mail.Address{Name: name, Address: a.Email}).String())
	}

	return strings.Join(out, ", "), nil
}

// groupedFieldValue is a GroupedAddresses value: groups with a name are
// written as groups, the others as plain addresses.
func groupedFieldValue(v json.RawMessage) (string, error) {
	var groups []struct {
		Name      *string        `json:"name"`
		Addresses []emailAddress `json:"addresses"`
	}

	if err := json.Unmarshal(v, &groups); err != nil {
		return "", err
	}

	out := make([]string, 0, len(groups))

	for _, g := range groups {
		list, err := formatAddresses(g.Addresses)
		if err != nil {
			return "", err
		}

		if g.Name == nil {
			if list != "" {
				out = append(out, list)
			}

			continue
		}

		name, err := phrase(*g.Name)
		if err != nil {
			return "", err
		}

		out = append(out, name+": "+list+";")
	}

	return strings.Join(out, ", "), nil
}

// phrase is a display name as RFC 5322 3.2.5 wants it: encoded words
// where it is not ASCII, a quoted string where it has specials.
func phrase(s string) (string, error) {
	if strings.ContainsAny(s, "\r\n\x00") {
		return "", errNotAValue
	}

	if !needsNoEncoding(s) {
		return encodeText(s), nil
	}

	if !strings.ContainsAny(s, `()<>[]:;@\,."`) {
		return s, nil
	}

	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"`, nil
}

// msgIDFieldValue is a MessageIds value.
func msgIDFieldValue(v json.RawMessage) (string, error) { return bracketList(v, " ") }

// dateFieldValue is a Date value, written with the offset it was given
// in.
func dateFieldValue(v json.RawMessage) (string, error) {
	var s string
	if err := json.Unmarshal(v, &s); err != nil {
		return "", err
	}

	t, ok := parseUTCDate(s)
	if !ok {
		return "", errNotAValue
	}

	return t.Format(rfc5322Date), nil
}

// rfc5322Date is the date format of RFC 5322 3.3.
const rfc5322Date = "Mon, 02 Jan 2006 15:04:05 -0700"

// urlFieldValue is a URLs value.
func urlFieldValue(v json.RawMessage) (string, error) { return bracketList(v, ", ") }

// bracketList writes a list of message ids or URLs, each in angle
// brackets.
func bracketList(v json.RawMessage, sep string) (string, error) {
	var list []string
	if err := json.Unmarshal(v, &list); err != nil {
		return "", err
	}

	out := make([]string, 0, len(list))

	for _, item := range list {
		if item == "" || strings.ContainsAny(item, "<> \t\r\n\x00") {
			return "", errNotAValue
		}

		out = append(out, "<"+item+">")
	}

	return strings.Join(out, sep), nil
}

// mustJSON encodes a string as JSON.
func mustJSON(s string) json.RawMessage {
	b, _ := json.Marshal(s)

	return b
}

// sortedKeys are the members of a set in order.
func sortedKeys(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}

	sort.Strings(out)

	return out
}

// sortedRawKeys are the keys of a JSON object in order.
func sortedRawKeys(m map[string]json.RawMessage) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}

	sort.Strings(out)

	return out
}

// blobReadable reports whether a blob id names something the user may
// read in the call's account: anything in the personal account, and in a
// shared account a message the user can read (right r where it is) or a
// part of one. Everything else in the owner's blob store is not found.
func blobReadable(sess *session, id string) (bool, error) {
	if sess.current().personal {
		return true, nil
	}

	guid := id
	if g, _, ok := parsePartBlob(id); ok {
		guid = g
	} else if !model.ValidGUID(id) {
		return false, nil
	}

	a := sess.current()
	if a.loc == nil {
		if _, _, merr := emailState(sess); merr != nil {
			return false, errors.New(merr.Description)
		}
	}

	hits, err := a.loc.resolve([]string{guid})
	if err != nil || hits[guid] != nil {
		return hits[guid] != nil, err
	}

	// A message this call stored after the locator was read is not in
	// it yet: bring it up to now, once, and look again.
	seq, err := sess.user.AccountState()
	if err != nil {
		return false, err
	}

	if err := a.loc.catchUp(seq); err != nil {
		return false, err
	}

	hits, err = a.loc.resolve([]string{guid})

	return hits[guid] != nil, err
}

// mayReadBlob is blobReadable as an error: ErrNoBlob for a blob the user
// may not read.
func mayReadBlob(sess *session, id string) error {
	readable, err := blobReadable(sess, id)
	if err == nil && !readable {
		err = model.ErrNoBlob
	}

	return err
}

// errBlobTooLarge is what blobContent answers for a blob beyond the
// bound it was given.
var errBlobTooLarge = errors.New("the blob is larger than allowed here")

// blobContent is the content of a blob a client names: an upload, a
// stored message, a body part of one, or a Sieve script's content, by its id or by the creation id
// of a Blob/upload earlier in the request. A blob, or the message a part
// is taken from, larger than limit is not read; 0 means no bound.
func blobContent(sess *session, ref string, limit int64) ([]byte, error) {
	blobs, ok := sess.user.(model.Blobs)
	if !ok {
		return nil, errNoIndex
	}

	id, pending := resolveCreationID(sess, ref)
	if pending {
		return nil, model.ErrNoBlob
	}

	// In a shared account the blob store is the owner's, and it holds
	// more than the user may read: messages in mailboxes without r,
	// uploads, the owner's scripts. Only a message the user can read
	// there, or a part of one, is a blob of the account.
	if err := mayReadBlob(sess, id); err != nil {
		return nil, err
	}

	if isScriptBlob(id) {
		return scriptContent(sess, id, limit)
	}

	if guid, path, ok := parsePartBlob(id); ok {
		body, found, err := readPart(blobs, guid, path, limit)
		if err == nil && !found {
			err = model.ErrNoBlob
		}

		return body, err
	}

	if !model.ValidGUID(id) {
		return nil, model.ErrNoBlob
	}

	size, err := blobs.BlobSize(id)
	if err != nil {
		return nil, err
	}

	if limit > 0 && size > limit {
		return nil, errBlobTooLarge
	}

	return blobs.ReadBlob(id, size, 0, -1)
}

// readPart is the decoded content of one body part of a stored blob.
// Each "_" in the path steps into a part that is a message of its own.
// A message larger than limit is not read; 0 means no bound.
func readPart(blobs model.Blobs, guid, path string, limit int64) ([]byte, bool, error) {
	size, err := blobs.BlobSize(guid)
	if err != nil {
		return nil, false, err
	}

	if limit > 0 && size > limit {
		return nil, false, errBlobTooLarge
	}

	body, err := blobs.ReadBlob(guid, size, 0, -1)
	if err != nil {
		return nil, false, err
	}

	for _, section := range strings.Split(path, "_") {
		root, err := parseMessage(body)
		if err != nil {
			return nil, false, err
		}

		part := root.Find(section)
		if part == nil {
			return nil, false, nil
		}

		body, _ = part.Decoded()
	}

	return body, true, nil
}
