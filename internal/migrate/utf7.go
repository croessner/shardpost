package migrate

import (
	"encoding/base64"
	"errors"
	"strings"
	"unicode/utf16"
	"unicode/utf8"
)

// imapBase64 is the modified base64 of RFC 3501 5.1.3 ("," for "/", no
// padding).
var imapBase64 = base64.NewEncoding("ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+,").WithPadding(base64.NoPadding)

// decodeMUTF7 decodes a mailbox name in modified UTF-7 (RFC 3501 5.1.3).
// A name that is already UTF-8 (UTF8=ACCEPT) is returned unchanged.
func decodeMUTF7(s string) (string, error) {
	if !isASCII(s) {
		if !utf8.ValidString(s) {
			return "", errors.New("mailbox name is neither modified UTF-7 nor UTF-8")
		}

		return s, nil
	}

	var b strings.Builder

	for i := 0; i < len(s); i++ {
		if s[i] != '&' {
			b.WriteByte(s[i])
			continue
		}

		end := strings.IndexByte(s[i+1:], '-')
		if end < 0 {
			return "", errors.New("unterminated modified UTF-7 sequence")
		}

		enc := s[i+1 : i+1+end]
		i += end + 1

		if enc == "" {
			b.WriteByte('&')
			continue
		}

		raw, err := imapBase64.DecodeString(enc)
		if err != nil || len(raw)%2 != 0 {
			return "", errors.New("invalid modified UTF-7 sequence")
		}

		units := make([]uint16, len(raw)/2)
		for j := range units {
			units[j] = uint16(raw[2*j])<<8 | uint16(raw[2*j+1])
		}

		b.WriteString(string(utf16.Decode(units)))
	}

	return b.String(), nil
}
