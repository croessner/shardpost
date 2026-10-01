// Package charset converts message text to UTF-8: MIME body parts by
// their charset parameter, header values with RFC 2047 encoded words,
// and unlabelled 8-bit text that is not UTF-8. It knows every charset
// of the WHATWG encoding list (golang.org/x/text/encoding/htmlindex).
package charset

import (
	"fmt"
	"io"
	"mime"
	"strings"
	"sync/atomic"
	"unicode/utf8"

	"golang.org/x/text/encoding"
	"golang.org/x/text/encoding/charmap"
	"golang.org/x/text/encoding/htmlindex"
)

// DefaultFallback is the charset unlabelled 8-bit text is read in when
// it is not UTF-8: Windows-1252, the superset of ISO-8859-1 that mail
// software of the Western European languages writes most often.
const DefaultFallback = "windows-1252"

const utf8Name = "utf-8"

var fallback atomic.Pointer[encoding.Encoding]

func init() {
	if err := SetFallback(DefaultFallback); err != nil {
		panic(err)
	}
}

// SetFallback sets the charset of unlabelled text that is not valid
// UTF-8. It has to be an ASCII-compatible charset of the WHATWG list;
// UTF-8, UTF-16 and the replacement encoding are refused, because they
// would not decode the bytes that made the text invalid.
func SetFallback(label string) error {
	enc, err := Fallback(label)
	if err != nil {
		return err
	}

	fallback.Store(&enc)

	return nil
}

// Fallback resolves and checks a fallback charset label without setting
// it (the configuration validates with it).
func Fallback(label string) (encoding.Encoding, error) {
	enc, err := htmlindex.Get(strings.TrimSpace(label))
	if err != nil {
		return nil, fmt.Errorf("unknown charset %q", label)
	}

	name, _ := htmlindex.Name(enc)
	switch name {
	case utf8Name, "utf-16be", "utf-16le", "replacement":
		return nil, fmt.Errorf("charset %q cannot be a fallback", label)
	}

	return enc, nil
}

// unlabelled reports whether a charset label says nothing beyond ASCII:
// none at all, or US-ASCII, which mail software writes by default and
// often for 8-bit text as well.
func unlabelled(label string) bool {
	switch label {
	case "", "us-ascii", "ascii", "us", "ansi_x3.4-1968", "iso646-us":
		return true
	}

	return false
}

func isUTF8(label string) bool {
	return label == utf8Name || label == "utf8" || label == "unicode-1-1-utf-8"
}

func normalize(label string) string {
	return strings.ToLower(strings.Trim(strings.TrimSpace(label), `"'`))
}

// Declared reports whether a charset label names a charset that is
// known and says more than ASCII: not empty, not US-ASCII, not unknown.
// An HTML part whose label is not declared is read in the charset its
// <meta> element names, if it names one.
func Declared(label string) bool {
	label = normalize(label)
	if unlabelled(label) {
		return false
	}

	_, err := htmlindex.Get(label)

	return err == nil
}

// ToUTF8 converts text in the named charset to UTF-8. Text without a
// charset (or labelled US-ASCII) that is not valid UTF-8 is read in the
// fallback charset; so is text in a charset that is not known. Invalid
// sequences in text labelled UTF-8 become U+FFFD. Text labelled with a
// single-byte charset (ISO-8859-x, Windows-125x, KOI8, …) that is valid
// UTF-8 with at least one multi-byte sequence is kept as UTF-8: it is
// UTF-8 under a wrong label far more often than it is the rare byte
// sequence of a single-byte charset that happens to form valid UTF-8.
func ToUTF8(b []byte, label string) string {
	label = normalize(label)

	switch {
	case isUTF8(label):
		if utf8.Valid(b) {
			return string(b)
		}

		return strings.ToValidUTF8(string(b), string(utf8.RuneError))
	case unlabelled(label):
		return decodeFallback(b)
	}

	enc, err := htmlindex.Get(label)
	if err != nil {
		return decodeFallback(b)
	}

	if _, single := enc.(*charmap.Charmap); single && multiByteUTF8(b) {
		return string(b)
	}

	out, err := enc.NewDecoder().Bytes(b)
	if err != nil {
		return strings.ToValidUTF8(string(b), string(utf8.RuneError))
	}

	return string(out)
}

// multiByteUTF8 reports whether b is valid UTF-8 with at least one
// character outside ASCII.
func multiByteUTF8(b []byte) bool {
	for _, c := range b {
		if c >= utf8.RuneSelf {
			return utf8.Valid(b)
		}
	}

	return false
}

// decodeFallback returns valid UTF-8 as it is and reads anything else in
// the fallback charset.
func decodeFallback(b []byte) string {
	if utf8.Valid(b) {
		return string(b)
	}

	out, err := (*fallback.Load()).NewDecoder().Bytes(b)
	if err != nil {
		return strings.ToValidUTF8(string(b), string(utf8.RuneError))
	}

	return string(out)
}

// WordDecoder decodes RFC 2047 encoded words in every charset Go's text
// encodings know (the WHATWG list: windows-1252, ISO-8859-15, KOI8-R,
// Shift_JIS, GB18030 and the others), not just UTF-8 and ISO-8859-1,
// which is all mime.WordDecoder decodes on its own. A word in a charset
// that is not known stays as it was written.
var WordDecoder = mime.WordDecoder{CharsetReader: func(charset string, r io.Reader) (io.Reader, error) {
	enc, err := htmlindex.Get(charset)
	if err != nil {
		return nil, fmt.Errorf("charset %q: %w", charset, err)
	}

	return enc.NewDecoder().Reader(r), nil
}}

// DecodeHeader returns a header value as UTF-8 text: raw 8-bit bytes
// that are not UTF-8 are read in the fallback charset, then encoded
// words are decoded. A value whose encoded words cannot be decoded keeps
// them as they were written.
func DecodeHeader(v string) string {
	if !utf8.ValidString(v) {
		v = decodeFallback([]byte(v))
	}

	if !strings.Contains(v, "=?") {
		return v
	}

	if d, err := WordDecoder.DecodeHeader(v); err == nil {
		return d
	}

	return v
}
