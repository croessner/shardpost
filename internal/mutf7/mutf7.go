// Package mutf7 converts mailbox names between UTF-8 and the modified
// UTF-7 of IMAP (RFC 3501 5.1.3, RFC 9051 appendix A.1).
//
// The store keeps mailbox names in UTF-8. Modified UTF-7 is the wire form
// for IMAP sessions that have enabled neither UTF8=ACCEPT nor IMAP4rev2,
// and the form an IMAP source server may send during an account import.
package mutf7

import (
	"encoding/base64"
	"errors"
	"strings"
	"unicode/utf16"
)

// imapBase64 is the modified base64 of RFC 3501 5.1.3 ("," for "/", no
// padding).
var imapBase64 = base64.NewEncoding("ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+,").WithPadding(base64.NoPadding)

// Errors of Decode and DecodeCanonical.
var (
	ErrNotASCII     = errors.New("modified UTF-7 is 7-bit")
	ErrUnterminated = errors.New("unterminated modified UTF-7 sequence")
	ErrInvalid      = errors.New("invalid modified UTF-7 sequence")
	ErrNotCanonical = errors.New("modified UTF-7 name is not in its only valid form")
)

// Encode converts a UTF-8 name to modified UTF-7: printable US-ASCII
// stands for itself, "&" becomes "&-", and every run of other characters
// is the modified base64 of its UTF-16 form between "&" and "-". An
// invalid UTF-8 byte is encoded as U+FFFD.
func Encode(s string) string {
	var (
		b   strings.Builder
		run []uint16
	)

	flush := func() {
		if len(run) == 0 {
			return
		}

		raw := make([]byte, 0, 2*len(run))
		for _, u := range run {
			raw = append(raw, byte(u>>8), byte(u))
		}

		b.WriteByte('&')
		b.WriteString(imapBase64.EncodeToString(raw))
		b.WriteByte('-')

		run = run[:0]
	}

	for _, r := range s {
		if r < 0x20 || r > 0x7e {
			run = utf16.AppendRune(run, r)
			continue
		}

		flush()

		if r == '&' {
			b.WriteString("&-")
		} else {
			b.WriteRune(r)
		}
	}

	flush()

	return b.String()
}

// Decode converts a modified UTF-7 name to UTF-8. It accepts every
// sequence that decodes, including forms Encode would not produce
// (superfluous shifts, encoded printable ASCII); DecodeCanonical does
// not.
func Decode(s string) (string, error) {
	var b strings.Builder

	for i := 0; i < len(s); i++ {
		if s[i] >= 0x80 {
			return "", ErrNotASCII
		}

		if s[i] != '&' {
			b.WriteByte(s[i])
			continue
		}

		end := strings.IndexByte(s[i+1:], '-')
		if end < 0 {
			return "", ErrUnterminated
		}

		enc := s[i+1 : i+1+end]
		i += end + 1

		if enc == "" {
			b.WriteByte('&')
			continue
		}

		raw, err := imapBase64.DecodeString(enc)
		if err != nil || len(raw)%2 != 0 {
			return "", ErrInvalid
		}

		units := make([]uint16, len(raw)/2)
		for j := range units {
			units[j] = uint16(raw[2*j])<<8 | uint16(raw[2*j+1])
		}

		b.WriteString(string(utf16.Decode(units)))
	}

	return b.String(), nil
}

// DecodeCanonical is Decode for names whose form matters: it accepts only
// the one form Encode produces for the decoded name. That form is unique,
// so a name a client sends and the name the server lists for it are the
// same octets (RFC 3501 5.1.3: no superfluous shifts, no encoded
// printable US-ASCII, nothing outside the modified base64 alphabet; an
// unpaired surrogate or stray bits at the end fail as well).
func DecodeCanonical(s string) (string, error) {
	out, err := Decode(s)
	if err != nil {
		return "", err
	}

	if Encode(out) != s {
		return "", ErrNotCanonical
	}

	return out, nil
}
