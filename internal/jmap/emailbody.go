package jmap

import (
	"strings"
	"unicode/utf8"

	"golang.org/x/text/encoding/htmlindex"

	"shardpost/internal/attcache"
	"shardpost/internal/model"
)

// The EmailBodyPart properties (RFC 8621 4.1.4), named once.
const (
	partPartID      = "partId"
	partBlobID      = "blobId"
	partSize        = "size"
	partHeaders     = "headers"
	partName        = "name"
	partType        = "type"
	partCharset     = "charset"
	partDisposition = "disposition"
	partCID         = "cid"
	partLanguage    = "language"
	partLocation    = "location"
	partSubParts    = "subParts"
)

// bodyPartDefault are the body part properties an Email/get without
// bodyProperties returns (RFC 8621 4.2).
var bodyPartDefault = []string{
	partPartID, partBlobID, partSize, partName, partType, partCharset,
	partDisposition, partCID, partLanguage, partLocation,
}

// bodyPartKnown are the properties a body part has.
var bodyPartKnown = func() map[string]bool {
	out := map[string]bool{partHeaders: true, partSubParts: true}
	for _, p := range bodyPartDefault {
		out[p] = true
	}

	return out
}()

// contentProperties are the Email properties that need the message
// itself, not only the index record and the header.
var contentProperties = map[string]bool{
	propBodyStructure: true, propBodyValues: true, propTextBody: true,
	propHTMLBody: true, propAttachments: true, propHasAttachment: true,
}

// bodyPropertyList checks the body part properties a client asked for.
func bodyPropertyList(props *[]string) ([]string, *methodError) {
	if props == nil {
		return bodyPartDefault, nil
	}

	for _, p := range *props {
		if bodyPartKnown[p] {
			continue
		}

		if _, ok := parseHeaderProperty(p); ok {
			continue
		}

		return nil, &methodError{Type: errInvalidArguments, Arguments: []string{"bodyProperties"},
			Description: "no such body property: " + p}
	}

	return *props, nil
}

// rendering is what one Email/get needs to render the body of one
// message: the parsed message, the three lists RFC 8621 4.1.4 derives
// from it, and the decoded bodies, decoded once however often they are
// asked for.
type rendering struct {
	root                         *mimePart
	textBody, htmlBody, attached []*mimePart
	decoded                      map[*mimePart][]byte
	guid                         string
}

// bodyProperties fills in the properties that need the message itself.
// The message is read and parsed only when one of them was asked for,
// because that is the one expensive step of an Email/get.
func bodyProperties(sess *session, m *model.Meta, raw []byte, props []string, req *emailGetRequest, out map[string]any) *methodError {
	need := false

	for _, p := range props {
		// hasAttachment is kept in the index record of every message
		// stored since the record has the field, and remembered in the
		// locator for many older ones; only the rest need the message.
		if p == propHasAttachment {
			if has, ok := knownHasAttachment(m, req); ok {
				out[p] = has

				continue
			}
		}

		need = need || contentProperties[p]
	}

	if !need {
		return nil
	}

	raw, err := messageBytes(sess, m, raw)
	if err != nil {
		// A message whose data cannot be read has no body to describe.
		// Its other properties stand, and so do the other messages of the
		// call: one broken message must not keep a client from the rest.
		sess.log.Warn("jmap: message body unreadable", "guid", m.GUID, "err", err)

		return nil
	}

	root, err := parseMessage(raw)
	if err != nil {
		// Too deep or too many parts to describe: the message is offered
		// as one opaque part instead, which a client can still download.
		root = opaqueMessage(raw)
	}

	r := &rendering{root: root, guid: m.GUID, decoded: map[*mimePart][]byte{}}
	r.textBody, r.htmlBody, r.attached = flatten(root)

	for _, p := range props {
		if _, done := out[p]; done && p == propHasAttachment {
			continue // answered from the record: the record is the answer
		}

		if v, ok := r.property(p, req); ok {
			out[p] = v
		}
	}

	return nil
}

// knownHasAttachment answers hasAttachment without the message: from the
// index record, or from the locator for an older record. A message the
// locator knows to have no data has no body properties at all.
func knownHasAttachment(m *model.Meta, req *emailGetRequest) (has, ok bool) {
	if m.HasAttachment != nil {
		return *m.HasAttachment, true
	}

	if v, found := req.attach[m.GUID]; found && v != attcache.Unreadable {
		return v.Has(), true
	}

	return false, false
}

// messageBytes is the message a rendering works on: the one given, or
// the stored one.
func messageBytes(sess *session, m *model.Meta, raw []byte) ([]byte, error) {
	if raw != nil {
		return raw, nil
	}

	return sess.user.Read(m)
}

// property renders one content property.
func (r *rendering) property(p string, req *emailGetRequest) (any, bool) {
	switch p {
	case propBodyStructure:
		return r.part(r.root, req.bodyProps), true
	case propTextBody:
		return r.parts(r.textBody, req.bodyProps), true
	case propHTMLBody:
		return r.parts(r.htmlBody, req.bodyProps), true
	case propAttachments:
		return r.parts(r.attached, req.bodyProps), true
	case propHasAttachment:
		return hasAttachment(r.attached), true
	case propBodyValues:
		return r.values(req), true
	}

	return nil, false
}

// parts renders a list of body parts.
func (r *rendering) parts(list []*mimePart, props []string) []map[string]any {
	out := make([]map[string]any, 0, len(list))
	for _, p := range list {
		out = append(out, r.part(p, props))
	}

	return out
}

// part renders one body part with the properties asked for.
func (r *rendering) part(p *mimePart, props []string) map[string]any {
	out := map[string]any{}

	for _, prop := range props {
		if v, ok := r.partProperty(p, prop, props); ok {
			out[prop] = v
		}
	}

	return out
}

// partProperty renders one property of a body part.
func (r *rendering) partProperty(p *mimePart, prop string, props []string) (any, bool) {
	leaf := p.Children == nil

	switch prop {
	case partPartID:
		return ifLeaf(leaf, p.Path), true
	case partBlobID:
		return ifLeaf(leaf, partBlob(r.guid, p.Path)), true
	case partSize:
		if !leaf {
			return 0, true
		}

		return len(r.body(p)), true
	case partHeaders:
		return headerList(p.Header, true), true
	case partSubParts:
		if leaf {
			return nil, true
		}

		return r.parts(p.Children, props), true
	}

	if v, ok := partHeaderProperty(p, prop); ok {
		return v, true
	}

	if hp, ok := parseHeaderProperty(prop); ok {
		return hp.value(p.Header, true), true
	}

	return nil, false
}

// partHeaderProperty answers the body part properties that come from
// its header.
func partHeaderProperty(p *mimePart, prop string) (any, bool) {
	switch prop {
	case partName:
		return nilIfEmpty(partFileName(p)), true
	case partType:
		return p.MediaType, true
	case partCharset:
		return partCharsetOf(p), true
	case partDisposition:
		return nilIfEmpty(dispositionOf(p)), true
	case partCID:
		cid := strings.Trim(strings.TrimSpace(p.Header.Get("Content-Id")), "<>")

		return nilIfEmpty(cid), true
	case partLanguage:
		return languagesOf(p), true
	case partLocation:
		return nilIfEmpty(strings.TrimSpace(p.Header.Get("Content-Location"))), true
	}

	return nil, false
}

// ifLeaf is v for a leaf part and null for a multipart one.
func ifLeaf(leaf bool, v string) any {
	if !leaf {
		return nil
	}

	return v
}

// body is the decoded body of a leaf, decoded once.
func (r *rendering) body(p *mimePart) []byte {
	if b, ok := r.decoded[p]; ok {
		return b
	}

	b, _ := p.Decoded()
	r.decoded[p] = b

	return b
}

// partCharsetOf is the charset of a part as RFC 8621 4.1.4 defines it:
// the parameter of a text part, us-ascii for a text part without one or
// a part without Content-Type, and null for anything else.
func partCharsetOf(p *mimePart) any {
	if !strings.HasPrefix(p.MediaType, "text/") {
		if p.Header.Get(fieldContentType) == "" {
			return charsetASCII
		}

		return nil
	}

	if cs := p.Params[partCharset]; cs != "" {
		return cs
	}

	return charsetASCII
}

// languagesOf is the Content-Language of a part as a list of tags.
func languagesOf(p *mimePart) any {
	v := p.Header.Get("Content-Language")
	if strings.TrimSpace(v) == "" {
		return nil
	}

	var out []string

	for _, tag := range strings.Split(v, ",") {
		if tag = strings.TrimSpace(tag); tag != "" {
			out = append(out, tag)
		}
	}

	return out
}

// bodyValue is a JMAP EmailBodyValue (RFC 8621 4.1.4).
type bodyValue struct {
	Value             string `json:"value"`
	IsEncodingProblem bool   `json:"isEncodingProblem"`
	IsTruncated       bool   `json:"isTruncated"`
}

// values renders the bodyValues a request asked for: the text parts of
// textBody, of htmlBody, or of the whole message.
func (r *rendering) values(req *emailGetRequest) map[string]bodyValue {
	out := map[string]bodyValue{}

	add := func(list []*mimePart) {
		for _, p := range list {
			if p.Children == nil && strings.HasPrefix(p.MediaType, "text/") {
				out[p.Path] = r.value(p, req.MaxBodyValueBytes)
			}
		}
	}

	switch {
	case req.FetchAllBodyValues:
		add(leaves(r.root))
	default:
		if req.FetchTextBodyValues {
			add(r.textBody)
		}

		if req.FetchHTMLBodyValues {
			add(r.htmlBody)
		}
	}

	return out
}

// value decodes one text part to UTF-8 and cuts it to the requested
// size, on a character boundary.
func (r *rendering) value(p *mimePart, limit uint64) bodyValue {
	raw, decodedOK := p.Decoded()
	text, charsetOK := toUTF8(raw, p.Params[partCharset])

	v := bodyValue{Value: text, IsEncodingProblem: !decodedOK || !charsetOK}

	if limit > 0 && uint64(len(v.Value)) > limit {
		cut := int(limit)
		for cut > 0 && !utf8.RuneStart(v.Value[cut]) {
			cut--
		}

		v.Value, v.IsTruncated = v.Value[:cut], true
	}

	return v
}

// toUTF8 converts bytes in a charset to UTF-8. ok is false when the
// charset is unknown or the bytes are not valid in it; what comes back is
// then the best reading there is, with U+FFFD where it had to guess.
func toUTF8(b []byte, charset string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(charset)) {
	case "", charsetUTF8, "utf8", "us-ascii", "ascii":
		if utf8.Valid(b) {
			return string(b), true
		}

		return strings.ToValidUTF8(string(b), string(utf8.RuneError)), false
	}

	enc, err := htmlindex.Get(charset)
	if err != nil {
		return strings.ToValidUTF8(string(b), string(utf8.RuneError)), false
	}

	out, err := enc.NewDecoder().Bytes(b)
	if err != nil {
		return strings.ToValidUTF8(string(b), string(utf8.RuneError)), false
	}

	return string(out), true
}

// partBlob is the blob id of a body part: the message's GUID and the
// section number, with the dots of the number replaced, because a JMAP
// id is limited to letters, digits, "-" and "_" (RFC 8620 1.2). A part of
// a message that is itself a part (message/rfc822, parsed by Email/parse)
// adds its section number after another "_".
func partBlob(guid, path string) string {
	return guid + "_" + strings.ReplaceAll(path, ".", "-")
}

// parsePartBlob reads a body part blob id back. The path keeps the "_"
// between the levels of nesting.
func parsePartBlob(id string) (guid, path string, ok bool) {
	guid, rest, found := strings.Cut(id, "_")
	if !found || !model.ValidGUID(guid) || rest == "" {
		return "", "", false
	}

	return guid, strings.ReplaceAll(rest, "-", "."), true
}
