package jmap

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"mime"
	"mime/quotedprintable"
	"strings"
	"unicode/utf8"
)

// Writing a message: the other direction of mime.go. A part is described
// by its headers, its content before any transfer encoding, and its
// children, and the writer picks the transfer encoding, the boundaries
// and the line breaks.

// hfield is one header field, its value ready to be written (encoded,
// not yet folded).
type hfield struct{ name, value string }

// outPart is one part of a message being written.
type outPart struct {
	header    []hfield // the fields other than Content-Type and Content-Transfer-Encoding
	mediaType string
	params    map[string]string
	content   []byte
	children  []*outPart
}

// maxLineOctets is the longest line RFC 5322 2.1.1 allows, without the
// line break; foldAt is where a header line is folded when it can be.
const (
	maxLineOctets = 998
	foldAt        = 76
)

// writeMessage writes a message: its top-level header fields, then the
// root part's own fields, then the content. It fails rather than write a
// part whose Content-Type cannot be formatted.
func writeMessage(top []hfield, root *outPart) ([]byte, error) {
	var b bytes.Buffer

	for _, f := range top {
		writeField(&b, f)
	}

	writeField(&b, hfield{"MIME-Version", "1.0"})

	if err := writePart(&b, root); err != nil {
		return nil, err
	}

	return b.Bytes(), nil
}

// writePart writes a part's MIME fields, the empty line and its content.
func writePart(b *bytes.Buffer, p *outPart) error {
	params := map[string]string{}
	for k, v := range p.params {
		params[k] = v
	}

	var boundary string
	if len(p.children) > 0 || strings.HasPrefix(p.mediaType, "multipart/") {
		boundary = newBoundary()
		params["boundary"] = boundary
	}

	ct := mime.FormatMediaType(p.mediaType, params)
	if ct == "" {
		return fmt.Errorf("jmap: cannot write Content-Type %q", p.mediaType)
	}

	writeField(b, hfield{fieldContentType, ct})

	enc := ""
	if boundary == "" {
		enc = transferEncoding(p)
		writeField(b, hfield{fieldCTE, enc})
	}

	for _, f := range p.header {
		writeField(b, f)
	}

	b.WriteString("\r\n")

	if boundary == "" {
		writeContent(b, p.content, enc)

		return nil
	}

	for _, c := range p.children {
		b.WriteString("--" + boundary + "\r\n")

		if err := writePart(b, c); err != nil {
			return err
		}

		b.WriteString("\r\n")
	}

	b.WriteString("--" + boundary + "--\r\n")

	return nil
}

// newBoundary is a boundary no content will contain: random, and with
// characters quoted-printable and base64 never produce in a row.
func newBoundary() string {
	var r [12]byte

	_, _ = rand.Read(r[:])

	return "=_" + hex.EncodeToString(r[:])
}

// transferEncoding picks the encoding of a leaf. Text that is ASCII in
// lines of legal length goes as it is; other text is quoted-printable,
// which keeps it readable; a message goes as it is, because RFC 2046
// 5.2.1 allows it no other encoding; everything else is base64.
func transferEncoding(p *outPart) string {
	switch {
	case strings.HasPrefix(p.mediaType, "message/"):
		if isASCII(p.content) {
			return enc7bit
		}

		return enc8bit
	case strings.HasPrefix(p.mediaType, "text/"):
		if isASCII(p.content) && linesFit(p.content) {
			return enc7bit
		}

		return encQP
	}

	return encBase64
}

// writeContent writes a leaf's content in its transfer encoding. The
// line break after it, if any, belongs to the boundary that follows.
func writeContent(b *bytes.Buffer, content []byte, enc string) {
	switch enc {
	case encBase64:
		s := base64.StdEncoding.EncodeToString(content)
		for len(s) > foldAt {
			b.WriteString(s[:foldAt] + "\r\n")
			s = s[foldAt:]
		}

		b.WriteString(s)
	case encQP:
		w := quotedprintable.NewWriter(b)
		_, _ = w.Write(content)
		_ = w.Close()
	default:
		b.Write(crlf(content))
	}
}

// crlf turns every line break into CRLF.
func crlf(in []byte) []byte {
	out := bytes.ReplaceAll(in, []byte("\r\n"), []byte("\n"))

	return bytes.ReplaceAll(out, []byte("\n"), []byte("\r\n"))
}

// isASCII reports whether b is 7-bit without NUL or a bare CR.
func isASCII(b []byte) bool {
	for i, c := range b {
		if c == 0 || c >= 0x80 || (c == '\r' && (i+1 >= len(b) || b[i+1] != '\n')) {
			return false
		}
	}

	return true
}

// linesFit reports whether every line of b is within the length RFC 5322
// allows.
func linesFit(b []byte) bool {
	for _, line := range bytes.Split(b, []byte("\n")) {
		if len(bytes.TrimSuffix(line, []byte("\r"))) > maxLineOctets {
			return false
		}
	}

	return true
}

// writeField writes one header field, folded at spaces where the line
// would otherwise grow past foldAt, never before an empty word, which
// would leave a line of nothing but white space. A value without spaces
// stays one line: there is nowhere to fold it, which is why values are
// checked with foldable before they get here.
func writeField(b *bytes.Buffer, f hfield) {
	line := f.name + ":"

	for i, word := range strings.Split(f.value, " ") {
		switch {
		case i == 0:
			line += " " + word
		case word != "" && len(line)+1+len(word) > foldAt && strings.TrimSpace(line) != f.name+":":
			b.WriteString(line + "\r\n")
			line = " " + word
		default:
			line += " " + word
		}
	}

	b.WriteString(line + "\r\n")
}

// encodeText is a header text in RFC 2047 encoded words where it is not
// ASCII.
func encodeText(s string) string {
	return mime.QEncoding.Encode(charsetUTF8, s)
}

// needsNoEncoding reports whether a header text can be written as it is.
func needsNoEncoding(s string) bool {
	for _, r := range s {
		if r >= utf8.RuneSelf || r < 0x20 && r != '\t' {
			return false
		}
	}

	return true
}
