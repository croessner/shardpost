package jmap

import (
	"encoding/json"
	"errors"
	"mime"
	"strings"

	"shardpost/internal/model"
)

// The body of a created Email: either a bodyStructure, which says
// exactly what the tree is, or textBody, htmlBody and attachments, from
// which the server builds the usual one — the text and the HTML as
// alternatives, inline images related to the HTML, and the attachments
// mixed in after.

// partCreatable are the properties a body part of a creation may carry
// besides header:*.
var partCreatable = map[string]bool{
	"partId": true, propBlobID: true, propSize: true, partName: true, "type": true, partCharset: true,
	"disposition": true, "cid": true, "language": true, "location": true, "subParts": true,
}

// bodyValueIn is a body value a creation supplies.
type bodyValueIn struct {
	Value             string `json:"value"`
	IsEncodingProblem bool   `json:"isEncodingProblem"`
	IsTruncated       bool   `json:"isTruncated"`
}

// partSpec is a body part of a creation, decoded.
type partSpec struct {
	PartID      *string           `json:"partId"`
	BlobID      *string           `json:"blobId"`
	Size        *int64            `json:"size"`
	Name        *string           `json:"name"`
	Type        *string           `json:"type"`
	Charset     *string           `json:"charset"`
	Disposition *string           `json:"disposition"`
	Cid         *string           `json:"cid"`
	Location    *string           `json:"location"`
	Language    []string          `json:"language"`
	SubParts    []json.RawMessage `json:"subParts"`
}

// bodyReader reads the parts of one creation.
type bodyReader struct {
	sess    *session
	values  map[string]bodyValueIn
	missing []string          // blob ids that are not there
	cache   map[string][]byte // blob contents read so far, by id
	budget  int64             // bytes of content still allowed
	count   int
}

// readBody builds the body of a creation.
func readBody(sess *session, given map[string]json.RawMessage) (*outPart, *setError) {
	br := &bodyReader{sess: sess, values: map[string]bodyValueIn{}, cache: map[string][]byte{},
		budget: sess.limits.MaxSizeUpload}

	if v, ok := given[propBodyValues]; ok && string(v) != jsonNull {
		if err := json.Unmarshal(v, &br.values); err != nil {
			return nil, invalidProperty(propBodyValues, err.Error())
		}

		for id, bv := range br.values {
			if bv.IsEncodingProblem || bv.IsTruncated {
				return nil, invalidProperty(propBodyValues, "body value "+id+" is marked truncated or broken")
			}
		}
	}

	var (
		root *outPart
		serr *setError
	)

	if v, ok := given[propBodyStructure]; ok && string(v) != jsonNull {
		for _, p := range []string{propTextBody, propHTMLBody, propAttachments} {
			if _, both := given[p]; both {
				return nil, invalidProperty(p, "bodyStructure and "+p+" in one creation")
			}
		}

		root, serr = br.part(v, "", 0)
	} else {
		root, serr = br.lists(given)
	}

	if len(br.missing) > 0 {
		return nil, &setError{Type: setBlobNotFound, NotFound: br.missing}
	}

	return root, serr
}

// lists builds the body from textBody, htmlBody and attachments.
func (br *bodyReader) lists(given map[string]json.RawMessage) (*outPart, *setError) {
	text, serr := br.single(given, propTextBody, mediaTextPlain)
	if serr != nil {
		return nil, serr
	}

	html, serr := br.single(given, propHTMLBody, mediaTextHTML)
	if serr != nil {
		return nil, serr
	}

	var raws []json.RawMessage

	if v, ok := given[propAttachments]; ok && string(v) != jsonNull {
		if err := json.Unmarshal(v, &raws); err != nil {
			return nil, invalidProperty(propAttachments, err.Error())
		}
	}

	var inline, attached []*outPart

	for _, raw := range raws {
		p, serr := br.part(raw, dispAttachment, 1)
		if serr != nil {
			return nil, serr
		}

		if html != nil && createdDisposition(p) == dispInline && hasField(p.header, "Content-Id") {
			inline = append(inline, p)
		} else {
			attached = append(attached, p)
		}
	}

	return assemble(text, html, inline, attached), nil
}

// single reads textBody or htmlBody: absent, or exactly one part of the
// list's type.
func (br *bodyReader) single(given map[string]json.RawMessage, prop, mediaType string) (*outPart, *setError) {
	v, ok := given[prop]
	if !ok || string(v) == jsonNull {
		return nil, nil
	}

	var raws []json.RawMessage
	if err := json.Unmarshal(v, &raws); err != nil || len(raws) != 1 {
		return nil, invalidProperty(prop, prop+" is a list of exactly one part")
	}

	p, serr := br.leafOfType(raws[0], mediaType)
	if serr != nil {
		return nil, serr
	}

	if p != nil && p.mediaType != mediaType {
		return nil, invalidProperty(prop, prop+" is of type "+mediaType)
	}

	return p, nil
}

// leafOfType reads a part whose type defaults to the given one.
func (br *bodyReader) leafOfType(raw json.RawMessage, mediaType string) (*outPart, *setError) {
	var given map[string]json.RawMessage
	if err := json.Unmarshal(raw, &given); err != nil {
		return nil, &setError{Type: setInvalidProperties, Description: err.Error()}
	}

	if _, ok := given["type"]; !ok {
		given["type"] = mustJSON(mediaType)
	}

	b, _ := json.Marshal(given)

	return br.part(b, "", 1)
}

// assemble puts the parts together into the usual tree.
func assemble(text, html *outPart, inline, attached []*outPart) *outPart {
	if html != nil && len(inline) > 0 {
		html = &outPart{mediaType: "multipart/related", children: append([]*outPart{html}, inline...)}
	}

	var body *outPart

	switch {
	case text != nil && html != nil:
		body = &outPart{mediaType: "multipart/alternative", children: []*outPart{text, html}}
	case text != nil:
		body = text
	case html != nil:
		body = html
	}

	switch {
	case len(attached) == 0 && body != nil:
		return body
	case len(attached) == 0:
		return &outPart{mediaType: mediaTextPlain, params: map[string]string{partCharset: charsetUTF8}}
	case body != nil:
		attached = append([]*outPart{body}, attached...)
	}

	return &outPart{mediaType: mediaMixed, children: attached}
}

// createdDisposition is the disposition a created part was given.
func createdDisposition(p *outPart) string {
	for _, f := range p.header {
		if f.name == fieldDisposition {
			d, _, _ := mime.ParseMediaType(f.value)

			return d
		}
	}

	return ""
}

// part reads one body part of a creation. defaultDisposition is what a
// part with a name and no disposition gets.
func (br *bodyReader) part(raw json.RawMessage, defaultDisposition string, depth int) (*outPart, *setError) {
	br.count++
	if br.count > maxParts || depth > maxPartDepth {
		return nil, invalidProperty(propBodyStructure, "too many parts, or nested too deep")
	}

	given, spec, serr := decodePart(raw)
	if serr != nil {
		return nil, serr
	}

	fields, serr := readFields(given, false)
	if serr != nil {
		return nil, serr
	}

	p := &outPart{mediaType: mediaTextPlain, params: map[string]string{}}

	if spec.Type != nil {
		p.mediaType = strings.ToLower(*spec.Type)
	} else if spec.SubParts != nil {
		p.mediaType = mediaMixed
	}

	if !validPartType(p.mediaType) {
		return nil, invalidProperty("type", "not a media type without parameters: "+p.mediaType)
	}

	if serr := br.content(p, spec, depth); serr != nil {
		return nil, serr
	}

	if spec.Name != nil && p.children == nil {
		p.params[partName] = *spec.Name
	}

	p.header, serr = partFields(fields, spec, defaultDisposition)

	return p, serr
}

// validPartType accepts a body part's type: RFC 8621 4.1.4 defines it as
// the media type alone, so "type/subtype" and nothing else. Parameters
// belong to their own properties (charset, name); one given here would
// otherwise leave a type mime.FormatMediaType cannot write.
func validPartType(s string) bool {
	if !validMediaType(s) {
		return false
	}

	mt, params, err := mime.ParseMediaType(s)

	return err == nil && len(params) == 0 && mt == s
}

// decodePart decodes a body part of a creation and refuses the
// properties it may not carry.
func decodePart(raw json.RawMessage) (map[string]json.RawMessage, *partSpec, *setError) {
	var given map[string]json.RawMessage
	if err := json.Unmarshal(raw, &given); err != nil {
		return nil, nil, &setError{Type: setInvalidProperties, Description: err.Error()}
	}

	for key := range given {
		if !partCreatable[key] && !strings.HasPrefix(key, "header:") {
			return nil, nil, invalidProperty(key, "a body part of a new email has no property "+key)
		}
	}

	spec := &partSpec{}
	if err := json.Unmarshal(raw, spec); err != nil {
		return nil, nil, &setError{Type: setInvalidProperties, Description: err.Error()}
	}

	return given, spec, nil
}

// content fills in a part's content: its children, a body value, or a
// blob.
func (br *bodyReader) content(p *outPart, spec *partSpec, depth int) *setError {
	if strings.HasPrefix(p.mediaType, "multipart/") {
		return br.children(p, spec, depth)
	}

	switch {
	case spec.SubParts != nil:
		return invalidProperty("subParts", "only a multipart has subParts")
	case (spec.PartID == nil) == (spec.BlobID == nil):
		return invalidProperty("partId", "a part has a partId or a blobId")
	case spec.PartID != nil:
		return br.valueContent(p, spec)
	}

	return br.blobPart(p, spec)
}

// children reads the parts of a multipart.
func (br *bodyReader) children(p *outPart, spec *partSpec, depth int) *setError {
	if spec.PartID != nil || spec.BlobID != nil || len(spec.SubParts) == 0 {
		return invalidProperty("subParts", "a multipart has at least one of subParts and no content of its own")
	}

	for _, raw := range spec.SubParts {
		c, serr := br.part(raw, "", depth+1)
		if serr != nil {
			return serr
		}

		p.children = append(p.children, c)
	}

	return nil
}

// blobPart fills in a part from a blob. A blob that is not there is
// noted, and the creation fails once every part has been looked at, so
// that the client learns of all of them at once.
func (br *bodyReader) blobPart(p *outPart, spec *partSpec) *setError {
	content, ok := br.cache[*spec.BlobID]
	if !ok {
		var err error

		content, err = blobContent(br.sess, *spec.BlobID, br.sess.limits.MaxSizeUpload)

		switch {
		case errors.Is(err, model.ErrNoBlob):
			br.missing = append(br.missing, *spec.BlobID)
		case errors.Is(err, errBlobTooLarge):
			return &setError{Type: setTooLarge, Description: descTooLarge}
		case err != nil:
			return &setError{Type: setServerFail, Description: err.Error()}
		}

		br.cache[*spec.BlobID] = content
	}

	if serr := br.charge(len(content)); serr != nil {
		return serr
	}

	p.content = content

	if spec.Charset != nil && strings.HasPrefix(p.mediaType, "text/") {
		p.params[partCharset] = *spec.Charset
	}

	return nil
}

// charge counts content against the size a message may have, so that a
// creation that references large blobs, or one blob many times, is
// refused before it is put together rather than after.
func (br *bodyReader) charge(n int) *setError {
	br.budget -= int64(n)
	if br.budget < 0 {
		return &setError{Type: setTooLarge, Description: descTooLarge}
	}

	return nil
}

// valueContent fills in a part from a body value.
func (br *bodyReader) valueContent(p *outPart, spec *partSpec) *setError {
	if spec.Charset != nil || spec.Size != nil {
		return invalidProperty(partCharset, "a part given by partId has no charset or size")
	}

	bv, ok := br.values[*spec.PartID]
	if !ok {
		return invalidProperty("partId", "no body value "+*spec.PartID)
	}

	if serr := br.charge(len(bv.Value)); serr != nil {
		return serr
	}

	p.content = []byte(bv.Value)

	if strings.HasPrefix(p.mediaType, "text/") {
		p.params[partCharset] = charsetUTF8
	}

	return nil
}

// partFields are the header fields of a part: the ones its properties
// stand for, then its header:* ones. A field given both ways is refused.
func partFields(fields []hfield, spec *partSpec, defaultDisposition string) ([]hfield, *setError) {
	out, serr := dispositionField(spec, defaultDisposition)
	if serr != nil {
		return nil, serr
	}

	if spec.Cid != nil && (*spec.Cid == "" || strings.ContainsAny(*spec.Cid, "<> \t")) {
		return nil, invalidProperty("cid", "a content id without angle brackets or spaces")
	}

	for _, e := range []struct {
		field, value string
		given        bool
	}{
		{"Content-Id", "<" + deref(spec.Cid) + ">", spec.Cid != nil},
		{"Content-Language", strings.Join(spec.Language, ", "), spec.Language != nil},
		{"Content-Location", deref(spec.Location), spec.Location != nil},
	} {
		if !e.given {
			continue
		}

		if hasField(fields, e.field) || strings.ContainsAny(e.value, "\r\n\x00") || !foldable(e.value) {
			return nil, invalidProperty(e.field, "given twice, or not a value")
		}

		out = append(out, hfield{e.field, e.value})
	}

	return append(out, fields...), nil
}

// dispositionField is the Content-Disposition of a part, if it has one:
// when it was given, when the part has a name, and for every part of the
// attachments list.
func dispositionField(spec *partSpec, defaultDisposition string) ([]hfield, *setError) {
	if spec.Name == nil && spec.Disposition == nil && defaultDisposition == "" {
		return nil, nil
	}

	disposition := defaultDisposition
	if spec.Disposition != nil {
		disposition = strings.ToLower(*spec.Disposition)
	}

	if disposition == "" {
		disposition = dispAttachment
	}

	params := map[string]string{}
	if spec.Name != nil {
		params["filename"] = *spec.Name
	}

	v := mime.FormatMediaType(disposition, params)
	if v == "" {
		return nil, invalidProperty("disposition", "not a disposition, or a name that cannot be written")
	}

	return []hfield{{fieldDisposition, v}}, nil
}

// deref is a string pointer's value, or "".
func deref(s *string) string {
	if s == nil {
		return ""
	}

	return *s
}
