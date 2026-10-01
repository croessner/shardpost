// Package mimepart takes a message apart into its MIME parts and derives
// from them what RFC 8621 4.1.4 defines: the text and HTML bodies and the
// attachments. JMAP renders its body properties from it, and the store
// records hasAttachment in the index record with the same code, so the
// property Email/get returns and the one Email/query filters by cannot
// disagree.
//
// A message is taken apart here rather than through a MIME library's
// reader, for two reasons. A body part's blob is its bytes after the
// transfer encoding is undone and before any charset is touched — that is
// what a client downloads (RFC 8621 4.1.4) — and a body value is those
// bytes converted to UTF-8, where the caller has to know when the
// conversion failed (isEncodingProblem). A reader that converts on its
// own does neither. The headers are parsed by go-message, which keeps
// their order and raw form.
package mimepart

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"errors"
	"io"
	"mime"
	"mime/quotedprintable"
	"strconv"
	"strings"

	"github.com/emersion/go-message/textproto"
)

// The media types, header fields and encodings the MIME defaults are
// made of.
const (
	MediaTextPlain   = "text/plain"
	MediaTextHTML    = "text/html"
	MediaOctetStream = "application/octet-stream"
	CharsetASCII     = "us-ascii"

	subAlternative   = "alternative"
	multipartPrefix  = "multipart/"
	fieldContentType = "Content-Type"
	fieldCTE         = "Content-Transfer-Encoding"
	fieldDisposition = "Content-Disposition"
	paramName        = "name"
)

// The dispositions RFC 8621 4.1.4 looks at.
const (
	DispInline     = "inline"
	DispAttachment = "attachment"
)

// Limits of the walk: a message nested deeper than this, or with more
// parts, is one whose structure is not described in full.
const (
	MaxDepth = 32
	MaxParts = 1000
)

// Part is one part of a message. A multipart part has children and no
// body; every other part has a body and no children.
type Part struct {
	Header    textproto.Header
	MediaType string
	Params    map[string]string
	Path      string // IMAP section number: "1", "2.1"; "" for a multipart
	Body      []byte // the raw, still transfer-encoded body of a leaf
	Children  []*Part
}

// ErrTooManyParts ends a walk that has gone past MaxParts or MaxDepth.
var ErrTooManyParts = errors.New("the message has too many parts")

// Parse takes a message apart.
func Parse(raw []byte) (*Part, error) {
	count := 0

	return parsePart(raw, "", MediaTextPlain, 0, &count)
}

// ParseOrOpaque takes a message apart, and describes one beyond the
// limits of the walk as one opaque part.
func ParseOrOpaque(raw []byte) *Part {
	root, err := Parse(raw)
	if err != nil {
		return Opaque(raw)
	}

	return root
}

// parsePart parses one part: its header, and either its body or its
// children. path is the section number the part gets if it is a leaf;
// the root of a single-part message is section "1". defaultType is what
// a part without Content-Type is (RFC 2046 5.1.5: message/rfc822 inside
// multipart/digest, text/plain everywhere else).
func parsePart(raw []byte, path, defaultType string, depth int, count *int) (*Part, error) {
	*count++
	if *count > MaxParts || depth > MaxDepth {
		return nil, ErrTooManyParts
	}

	hdr, body := SplitHeader(raw)
	p := &Part{Header: hdr}
	p.MediaType, p.Params = contentType(hdr, defaultType)

	if strings.HasPrefix(p.MediaType, multipartPrefix) && p.Params["boundary"] == "" {
		// RFC 2046 5.1.1 makes a multipart without a boundary an error.
		// Its content is kept as one opaque part a client can download,
		// rather than lost from every list.
		p.MediaType = MediaOctetStream
	}

	if !strings.HasPrefix(p.MediaType, multipartPrefix) {
		p.Path, p.Body = leafPath(path), body

		return p, nil
	}

	childDefault := MediaTextPlain
	if p.MediaType == "multipart/digest" {
		childDefault = "message/rfc822"
	}

	for i, chunk := range SplitMultipart(body, p.Params["boundary"]) {
		child, err := parsePart(chunk, childPath(path, i+1), childDefault, depth+1, count)
		if err != nil {
			return nil, err
		}

		p.Children = append(p.Children, child)
	}

	return p, nil
}

// Opaque is a message described as one part: its header, and everything
// after it as a body of unknown type. It is what a message gets whose
// structure is beyond the limits of the walk.
func Opaque(raw []byte) *Part {
	hdr, body := SplitHeader(raw)

	return &Part{Header: hdr, MediaType: MediaOctetStream, Params: map[string]string{}, Path: "1", Body: body}
}

// leafPath is the section number of a leaf: the root leaf of a single-part
// message is "1".
func leafPath(path string) string {
	if path == "" {
		return "1"
	}

	return path
}

// childPath is the section number of the n-th child of a multipart.
func childPath(parent string, n int) string {
	if parent == "" {
		return strconv.Itoa(n)
	}

	return parent + "." + strconv.Itoa(n)
}

// HeaderEnd is the offset just past the empty line that ends a header,
// or -1 when raw does not contain it yet. Both CRLF and bare LF occur,
// and the first empty line is the end whichever it is: a header in bare
// LF followed by a body that contains a CRLF blank line ends at the LF
// one.
func HeaderEnd(raw []byte) int {
	crlf := bytes.Index(raw, []byte("\r\n\r\n"))
	lf := bytes.Index(raw, []byte("\n\n"))

	switch {
	case crlf >= 0 && (lf < 0 || crlf < lf):
		return crlf + 4
	case lf >= 0:
		return lf + 2
	}

	return -1
}

// SplitHeader separates a part's header from its body. A part that starts
// with an empty line has an empty header.
func SplitHeader(raw []byte) (textproto.Header, []byte) {
	end := HeaderEnd(raw)
	if end < 0 {
		// No empty line at all: a part that is only a header, or one that
		// is only a body. A first line without a colon is not a header.
		first, _, _ := bytes.Cut(raw, []byte("\n"))
		if !bytes.Contains(first, []byte(":")) {
			return textproto.Header{}, raw
		}

		hdr, _ := textproto.ReadHeader(bufio.NewReader(bytes.NewReader(raw)))

		return hdr, nil
	}

	if bytes.HasPrefix(raw, []byte("\r\n")) || bytes.HasPrefix(raw, []byte("\n")) {
		return textproto.Header{}, raw[bytes.IndexByte(raw, '\n')+1:]
	}

	hdr, _ := textproto.ReadHeader(bufio.NewReader(bytes.NewReader(raw[:end])))

	return hdr, raw[end:]
}

// contentType is the lower-cased media type and the parameters of a part,
// with the default for a part that has no usable Content-Type. RFC 2045
// 5.2 makes an unusable type text/plain; a type that parses but carries a
// parameter the strict parser refuses — an unquoted boundary with "=" in
// it, an empty charset, a quote that is never closed, all common — keeps
// its type, and its parameters are recovered one by one, because losing
// the boundary turns a message with attachments into one text part.
func contentType(hdr textproto.Header, defaultType string) (string, map[string]string) {
	v := hdr.Get(fieldContentType)
	if v == "" {
		return defaultType, map[string]string{}
	}

	mt, params, err := mime.ParseMediaType(v)

	switch {
	case !strings.Contains(mt, "/"):
		return MediaTextPlain, map[string]string{}
	case errors.Is(err, mime.ErrInvalidMediaParameter):
		return strings.ToLower(mt), LooseParams(v)
	case err != nil:
		return MediaTextPlain, map[string]string{}
	}

	return strings.ToLower(mt), params
}

// LooseParams recovers the parameters of a header value the strict
// parser refused: one bad parameter must not cost the others. A quote
// that is never closed runs to the end of the value.
func LooseParams(v string) map[string]string {
	out := map[string]string{}

	_, rest, found := strings.Cut(v, ";")
	if !found {
		return out
	}

	for _, piece := range splitParams(rest) {
		k, val, ok := strings.Cut(piece, "=")
		if !ok {
			continue
		}

		k = strings.ToLower(strings.TrimSpace(k))
		val = strings.TrimSuffix(strings.TrimPrefix(strings.TrimSpace(val), `"`), `"`)

		if _, dup := out[k]; k != "" && val != "" && !dup {
			out[k] = val
		}
	}

	return out
}

// splitParams splits parameters at the semicolons outside quotes.
func splitParams(s string) []string {
	var out []string

	inQuote, start := false, 0

	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '"':
			inQuote = !inQuote
		case ';':
			if !inQuote {
				out = append(out, s[start:i])
				start = i + 1
			}
		}
	}

	return append(out, s[start:])
}

// SplitMultipart cuts a multipart body into the raw parts between its
// boundary lines (RFC 2046 5.1.1). The line break before a boundary line
// belongs to the boundary, not to the part before it. The preamble and
// the epilogue are dropped.
func SplitMultipart(body []byte, boundary string) [][]byte {
	if boundary == "" {
		return nil
	}

	delim := []byte("--" + boundary)
	closeDelim := []byte("--" + boundary + "--")

	var (
		parts [][]byte
		start = -1
	)

	for pos := 0; pos < len(body); {
		lineEnd := bytes.IndexByte(body[pos:], '\n')
		next := len(body)

		if lineEnd >= 0 {
			next = pos + lineEnd + 1
		}

		line := bytes.TrimRight(body[pos:next], " \t\r\n")

		if bytes.Equal(line, delim) || bytes.Equal(line, closeDelim) {
			if start >= 0 {
				parts = append(parts, trimBreak(body[start:pos]))
			}

			if bytes.Equal(line, closeDelim) {
				return parts
			}

			start = next
		}

		pos = next
	}

	if start >= 0 && start < len(body) {
		parts = append(parts, body[start:])
	}

	return parts
}

// trimBreak drops the one line break that belongs to the boundary after
// a part.
func trimBreak(b []byte) []byte {
	b = bytes.TrimSuffix(b, []byte("\n"))

	return bytes.TrimSuffix(b, []byte("\r"))
}

// Decoded undoes the transfer encoding of a leaf. ok is false when the
// encoding is one this package does not know or the body does not
// decode; the bytes returned are then as much as could be recovered, or
// the raw body.
func (p *Part) Decoded() ([]byte, bool) {
	enc := strings.ToLower(strings.TrimSpace(p.Header.Get(fieldCTE)))

	switch enc {
	case "", "7bit", "8bit", "binary":
		return p.Body, true
	case "base64":
		return decodeBase64(p.Body)
	case "quoted-printable":
		out, err := io.ReadAll(quotedprintable.NewReader(bytes.NewReader(p.Body)))

		return out, err == nil
	}

	return p.Body, false
}

// decodeBase64 decodes a base64 body, ignoring the line breaks and any
// other character outside the alphabet, as RFC 2045 6.8 asks of a
// decoder.
func decodeBase64(body []byte) ([]byte, bool) {
	clean := make([]byte, 0, len(body))

	for _, c := range body {
		if c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '+' || c == '/' || c == '=' {
			clean = append(clean, c)
		}
	}

	out, err := base64.StdEncoding.DecodeString(string(clean))
	if err != nil {
		// A body whose padding is missing still carries its data.
		out, err = base64.RawStdEncoding.DecodeString(strings.TrimRight(string(clean), "="))
	}

	return out, err == nil
}

// Find returns the leaf with the given section number.
func (p *Part) Find(path string) *Part {
	if p.Path == path && p.Children == nil {
		return p
	}

	for _, c := range p.Children {
		if found := c.Find(path); found != nil {
			return found
		}
	}

	return nil
}

// Leaves are the parts of a message that are not multiparts, in order.
func Leaves(p *Part) []*Part {
	if p.Children == nil {
		return []*Part{p}
	}

	var out []*Part
	for _, c := range p.Children {
		out = append(out, Leaves(c)...)
	}

	return out
}

// FileName is the file name of a part: the filename parameter of its
// Content-Disposition, or the name parameter of its Content-Type (RFC
// 2231 continuations are joined by the MIME parser, RFC 2047 words are
// decoded here).
func FileName(p *Part) string {
	name := ""

	if _, params, err := ParseDisposition(p); err == nil {
		name = params["filename"]
	}

	if name == "" {
		name = p.Params[paramName]
	}

	if dec, err := WordDecoder.DecodeHeader(name); err == nil {
		name = dec
	}

	return name
}

// Disposition is the lower-cased disposition type of a part.
func Disposition(p *Part) string {
	d, _, err := ParseDisposition(p)
	if err != nil {
		return ""
	}

	return d
}

// ParseDisposition parses a part's Content-Disposition.
func ParseDisposition(p *Part) (string, map[string]string, error) {
	v := p.Header.Get(fieldDisposition)
	if v == "" {
		return "", map[string]string{}, nil
	}

	d, params, err := parseMediaTypeLoose(v)

	return strings.ToLower(d), params, err
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

	return strings.TrimSpace(token), LooseParams(v), nil
}
