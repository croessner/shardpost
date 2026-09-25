package jmap

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

// A message is taken apart here rather than through a MIME library's
// reader, for two reasons. A body part's blob is its bytes after the
// transfer encoding is undone and before any charset is touched — that is
// what a client downloads (RFC 8621 4.1.4) — and a body value is those
// bytes converted to UTF-8 by this server, which has to know when the
// conversion failed to say so (isEncodingProblem). A reader that converts
// on its own does neither. The headers are parsed by go-message, which
// keeps their order and raw form, as the headers property needs.

// The media types and the charset the MIME defaults are made of.
const (
	mediaTextPlain   = "text/plain"
	mediaTextHTML    = "text/html"
	mediaOctetStream = "application/octet-stream"
	charsetASCII     = "us-ascii"
	subAlternative   = "alternative"
)

// Limits of the walk: a message nested deeper than this, or with more
// parts, is one whose structure this server does not describe in full.
const (
	maxPartDepth = 32
	maxParts     = 1000
)

// mimePart is one part of a message. A multipart part has children and
// no body; every other part has a body and no children.
type mimePart struct {
	header    textproto.Header
	mediaType string
	params    map[string]string
	path      string // IMAP section number: "1", "2.1"; "" for a multipart
	body      []byte // the raw, still transfer-encoded body of a leaf
	children  []*mimePart
}

// errTooManyParts ends a walk that has gone past maxParts.
var errTooManyParts = errors.New("the message has too many parts")

// parseMessage takes a message apart.
func parseMessage(raw []byte) (*mimePart, error) {
	count := 0

	return parsePart(raw, "", mediaTextPlain, 0, &count)
}

// parsePart parses one part: its header, and either its body or its
// children. path is the section number the part gets if it is a leaf;
// the root of a single-part message is section "1". defaultType is what
// a part without Content-Type is (RFC 2046 5.1.5: message/rfc822 inside
// multipart/digest, text/plain everywhere else).
func parsePart(raw []byte, path, defaultType string, depth int, count *int) (*mimePart, error) {
	*count++
	if *count > maxParts || depth > maxPartDepth {
		return nil, errTooManyParts
	}

	hdr, body := splitHeader(raw)
	p := &mimePart{header: hdr}
	p.mediaType, p.params = contentType(hdr, defaultType)

	if strings.HasPrefix(p.mediaType, "multipart/") && p.params["boundary"] == "" {
		// RFC 2046 5.1.1 makes a multipart without a boundary an error.
		// Its content is kept as one opaque part a client can download,
		// rather than lost from every list.
		p.mediaType = mediaOctetStream
	}

	if !strings.HasPrefix(p.mediaType, "multipart/") {
		p.path, p.body = leafPath(path), body

		return p, nil
	}

	childDefault := mediaTextPlain
	if p.mediaType == "multipart/digest" {
		childDefault = "message/rfc822"
	}

	for i, chunk := range splitMultipart(body, p.params["boundary"]) {
		child, err := parsePart(chunk, childPath(path, i+1), childDefault, depth+1, count)
		if err != nil {
			return nil, err
		}

		p.children = append(p.children, child)
	}

	return p, nil
}

// opaqueMessage is a message described as one part: its header, and
// everything after it as a body of unknown type. It is what a message
// gets whose structure is beyond the limits of the walk.
func opaqueMessage(raw []byte) *mimePart {
	hdr, body := splitHeader(raw)

	return &mimePart{header: hdr, mediaType: mediaOctetStream, params: map[string]string{}, path: "1", body: body}
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

// splitHeader separates a part's header from its body. A part that starts
// with an empty line has an empty header.
func splitHeader(raw []byte) (textproto.Header, []byte) {
	end := headerEnd(raw)
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
	v := hdr.Get("Content-Type")
	if v == "" {
		return defaultType, map[string]string{}
	}

	mt, params, err := mime.ParseMediaType(v)

	switch {
	case !strings.Contains(mt, "/"):
		return mediaTextPlain, map[string]string{}
	case errors.Is(err, mime.ErrInvalidMediaParameter):
		return strings.ToLower(mt), looseParams(v)
	case err != nil:
		return mediaTextPlain, map[string]string{}
	}

	return strings.ToLower(mt), params
}

// looseParams recovers the parameters of a header value the strict
// parser refused: one bad parameter must not cost the others. A quote
// that is never closed runs to the end of the value.
func looseParams(v string) map[string]string {
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

// splitMultipart cuts a multipart body into the raw parts between its
// boundary lines (RFC 2046 5.1.1). The line break before a boundary line
// belongs to the boundary, not to the part before it. The preamble and
// the epilogue are dropped.
func splitMultipart(body []byte, boundary string) [][]byte {
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

// decodedBody undoes the transfer encoding of a leaf. ok is false when
// the encoding is one this server does not know or the body does not
// decode; the bytes returned are then as much as could be recovered, or
// the raw body.
func (p *mimePart) decodedBody() ([]byte, bool) {
	enc := strings.ToLower(strings.TrimSpace(p.header.Get("Content-Transfer-Encoding")))

	switch enc {
	case "", "7bit", "8bit", "binary":
		return p.body, true
	case "base64":
		return decodeBase64(p.body)
	case "quoted-printable":
		out, err := io.ReadAll(quotedprintable.NewReader(bytes.NewReader(p.body)))

		return out, err == nil
	}

	return p.body, false
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

// find returns the leaf with the given section number.
func (p *mimePart) find(path string) *mimePart {
	if p.path == path && p.children == nil {
		return p
	}

	for _, c := range p.children {
		if found := c.find(path); found != nil {
			return found
		}
	}

	return nil
}
