package jmap

import (
	"mime"
	"strings"
	"unicode/utf8"

	"golang.org/x/text/encoding/htmlindex"

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
		if v, ok := r.property(p, req); ok {
			out[p] = v
		}
	}

	return nil
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
	leaf := p.children == nil

	switch prop {
	case partPartID:
		return ifLeaf(leaf, p.path), true
	case partBlobID:
		return ifLeaf(leaf, partBlob(r.guid, p.path)), true
	case partSize:
		if !leaf {
			return 0, true
		}

		return len(r.body(p)), true
	case partHeaders:
		return headerList(p.header, true), true
	case partSubParts:
		if leaf {
			return nil, true
		}

		return r.parts(p.children, props), true
	}

	if v, ok := partHeaderProperty(p, prop); ok {
		return v, true
	}

	if hp, ok := parseHeaderProperty(prop); ok {
		return hp.value(p.header, true), true
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
		return p.mediaType, true
	case partCharset:
		return partCharsetOf(p), true
	case partDisposition:
		return nilIfEmpty(dispositionOf(p)), true
	case partCID:
		cid := strings.Trim(strings.TrimSpace(p.header.Get("Content-Id")), "<>")

		return nilIfEmpty(cid), true
	case partLanguage:
		return languagesOf(p), true
	case partLocation:
		return nilIfEmpty(strings.TrimSpace(p.header.Get("Content-Location"))), true
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

	b, _ := p.decodedBody()
	r.decoded[p] = b

	return b
}

// partFileName is the file name of a part: the filename parameter of its
// Content-Disposition, or the name parameter of its Content-Type (RFC
// 2231 continuations are joined by the MIME parser, RFC 2047 words are
// decoded here).
func partFileName(p *mimePart) string {
	name := ""

	if _, params, err := parseDisposition(p); err == nil {
		name = params["filename"]
	}

	if name == "" {
		name = p.params[partName]
	}

	if dec, err := wordDecoder.DecodeHeader(name); err == nil {
		name = dec
	}

	return name
}

// dispositionOf is the lower-cased disposition type of a part.
func dispositionOf(p *mimePart) string {
	d, _, err := parseDisposition(p)
	if err != nil {
		return ""
	}

	return d
}

// parseDisposition parses a part's Content-Disposition.
func parseDisposition(p *mimePart) (string, map[string]string, error) {
	v := p.header.Get(fieldDisposition)
	if v == "" {
		return "", map[string]string{}, nil
	}

	d, params, err := parseMediaTypeLoose(v)

	return strings.ToLower(d), params, err
}

// partCharsetOf is the charset of a part as RFC 8621 4.1.4 defines it:
// the parameter of a text part, us-ascii for a text part without one or
// a part without Content-Type, and null for anything else.
func partCharsetOf(p *mimePart) any {
	if !strings.HasPrefix(p.mediaType, "text/") {
		if p.header.Get(fieldContentType) == "" {
			return charsetASCII
		}

		return nil
	}

	if cs := p.params[partCharset]; cs != "" {
		return cs
	}

	return charsetASCII
}

// languagesOf is the Content-Language of a part as a list of tags.
func languagesOf(p *mimePart) any {
	v := p.header.Get("Content-Language")
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

// hasAttachment is what RFC 8621 4.1.4 suggests: an attachment that is
// not marked inline is one a client should offer for download.
func hasAttachment(attached []*mimePart) bool {
	for _, p := range attached {
		if dispositionOf(p) != dispInline {
			return true
		}
	}

	return false
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
			if p.children == nil && strings.HasPrefix(p.mediaType, "text/") {
				out[p.path] = r.value(p, req.MaxBodyValueBytes)
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
	raw, decodedOK := p.decodedBody()
	text, charsetOK := toUTF8(raw, p.params[partCharset])

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

// leaves are the parts of a message that are not multiparts, in order.
func leaves(p *mimePart) []*mimePart {
	if p.children == nil {
		return []*mimePart{p}
	}

	var out []*mimePart
	for _, c := range p.children {
		out = append(out, leaves(c)...)
	}

	return out
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

// flatten derives textBody, htmlBody and attachments from the part tree,
// with the algorithm RFC 8621 4.1.4 gives. It is ported as written,
// including the one thing that is easy to get wrong in a port: the
// reference algorithm stops collecting into a list by setting its own
// variable to null, which affects the rest of that call and the calls it
// makes, but not its caller. A nil pointer here is that null.
func flatten(root *mimePart) (text, html, attached []*mimePart) {
	var t, h, a []*mimePart

	walkStructure([]*mimePart{root}, "mixed", false, &h, &t, &a)

	return t, h, a
}

// walkStructure is parseStructure of RFC 8621 4.1.4.
func walkStructure(parts []*mimePart, multipartType string, inAlternative bool, htmlBody, textBody, attachments *[]*mimePart) {
	textLength, htmlLength := sliceLen(textBody), sliceLen(htmlBody)

	for i, part := range parts {
		switch {
		case strings.HasPrefix(part.mediaType, "multipart/"):
			sub := strings.TrimPrefix(part.mediaType, "multipart/")
			walkStructure(part.children, sub, inAlternative || sub == subAlternative, htmlBody, textBody, attachments)
		case !isInlinePart(part, i, multipartType):
			push(attachments, part)
		case multipartType == subAlternative:
			placeAlternative(part, htmlBody, textBody, attachments)
		default:
			htmlBody, textBody = placeInline(part, inAlternative, htmlBody, textBody, attachments)
		}
	}

	if multipartType == subAlternative && textBody != nil && htmlBody != nil {
		balanceAlternative(textBody, htmlBody, textLength, htmlLength)
	}
}

// placeInline puts an inline part outside a multipart/alternative into
// the lists, and returns them as the rest of the walk is to see them:
// inside an alternative a plain part ends the collection of HTML and an
// HTML part ends the collection of plain text, for this call and the
// ones after it, as the reference algorithm's nulling does.
func placeInline(part *mimePart, inAlternative bool, htmlBody, textBody, attachments *[]*mimePart) (html, text *[]*mimePart) {
	if inAlternative {
		if part.mediaType == mediaTextPlain {
			htmlBody = nil
		}

		if part.mediaType == mediaTextHTML {
			textBody = nil
		}
	}

	push(textBody, part)
	push(htmlBody, part)

	if (textBody == nil || htmlBody == nil) && isInlineMedia(part.mediaType) {
		push(attachments, part)
	}

	return htmlBody, textBody
}

// isInlinePart is the isInline test of the reference algorithm: a body
// part rather than an attachment.
func isInlinePart(part *mimePart, i int, multipartType string) bool {
	if dispositionOf(part) == dispAttachment {
		return false
	}

	if part.mediaType != mediaTextPlain && part.mediaType != mediaTextHTML && !isInlineMedia(part.mediaType) {
		return false
	}

	// In multipart/related only the first part can be inline; a text part
	// with a file name that is not first is taken for an attachment.
	return i == 0 || (multipartType != "related" && (isInlineMedia(part.mediaType) || partFileName(part) == ""))
}

// placeAlternative puts one inline part of a multipart/alternative into
// the list of its kind.
func placeAlternative(part *mimePart, htmlBody, textBody, attachments *[]*mimePart) {
	switch part.mediaType {
	case mediaTextPlain:
		push(textBody, part)
	case mediaTextHTML:
		push(htmlBody, part)
	default:
		push(attachments, part)
	}
}

// balanceAlternative fills in the kind an alternative lacks: an
// alternative with only an HTML part gives it to textBody as well, and
// one with only a text part gives it to htmlBody.
func balanceAlternative(textBody, htmlBody *[]*mimePart, textLength, htmlLength int) {
	if textLength == len(*textBody) && htmlLength != len(*htmlBody) {
		*textBody = append(*textBody, (*htmlBody)[htmlLength:]...)
	}

	if htmlLength == len(*htmlBody) && textLength != len(*textBody) {
		*htmlBody = append(*htmlBody, (*textBody)[textLength:]...)
	}
}

// push appends to a list that is still being collected.
func push(list *[]*mimePart, part *mimePart) {
	if list != nil {
		*list = append(*list, part)
	}
}

// sliceLen is the length of a list, or -1 for one that is not collected.
func sliceLen(list *[]*mimePart) int {
	if list == nil {
		return -1
	}

	return len(*list)
}

// isInlineMedia is the isInlineMediaType of the reference algorithm.
func isInlineMedia(mt string) bool {
	return strings.HasPrefix(mt, "image/") || strings.HasPrefix(mt, "audio/") || strings.HasPrefix(mt, "video/")
}

// parseMediaTypeLoose parses a Content-Disposition, and recovers what it
// can when the parameters do not parse: a broken filename must not turn
// an attachment into an inline part, nor lose the name of one.
func parseMediaTypeLoose(v string) (string, map[string]string, error) {
	d, params, err := mime.ParseMediaType(v)
	if err == nil {
		return d, params, nil
	}

	token, _, _ := strings.Cut(v, ";")

	return strings.TrimSpace(token), looseParams(v), nil
}
