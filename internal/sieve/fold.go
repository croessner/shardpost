package sieve

import (
	"strings"
	"unicode/utf8"
)

// Line lengths of RFC 5322 2.1.1: a line should stay within 78 octets
// and must stay within 998, without the CRLF.
const (
	foldLine = 78
	maxLine  = 998
)

// foldField returns the header field "name: value" with its CRLF,
// folded at spaces so that a line stays within 78 octets where a space
// allows it (RFC 5322 2.2.3). A word longer than that stays whole; one
// beyond 998 octets cannot occur in the values folded here (message
// IDs).
func foldField(name, value string) string {
	var b strings.Builder

	b.WriteString(name)
	b.WriteString(":")

	n := len(name) + 1

	for i, w := range strings.Fields(value) {
		if i > 0 && n+1+len(w) > foldLine {
			b.WriteString("\r\n")

			n = 0
		}

		b.WriteString(" ")
		b.WriteString(w)

		n += 1 + len(w)
	}

	b.WriteString("\r\n")

	return b.String()
}

// wrapLongLines breaks the lines of a CRLF text that exceed 998 octets,
// at the last space before the limit, or at the limit where there is
// none, so that no relay has to refuse or break the message. Shorter
// lines stay as they are.
func wrapLongLines(text string) string {
	lines := strings.Split(text, "\r\n")
	for i, l := range lines {
		lines[i] = wrapLine(l)
	}

	return strings.Join(lines, "\r\n")
}

// wrapLine breaks one line into parts of at most maxLine octets.
func wrapLine(l string) string {
	var parts []string

	for len(l) > maxLine {
		cut := strings.LastIndexByte(l[:maxLine+1], ' ')
		if cut <= 0 {
			// No space: cut at the limit, but not inside a character.
			cut = maxLine
			for cut > 1 && !utf8.RuneStart(l[cut]) {
				cut--
			}
		}

		parts = append(parts, l[:cut])
		l = strings.TrimPrefix(l[cut:], " ")
	}

	// A fold at the very end leaves nothing for another line.
	if l == "" && len(parts) > 0 {
		return strings.Join(parts, "\r\n")
	}

	return strings.Join(append(parts, l), "\r\n")
}
