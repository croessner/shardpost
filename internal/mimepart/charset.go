package mimepart

import (
	"fmt"
	"io"
	"mime"

	"golang.org/x/text/encoding/htmlindex"
)

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
