package imapproto

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"

	"shardpost/internal/model"
	"shardpost/internal/mutf7"
)

// Mailbox names on the wire (RFC 3501 5.1.3, RFC 6855 3, RFC 9051 5.1
// and appendix A.1).
//
// The store keeps every mailbox name in UTF-8. A session speaks UTF-8
// names once it has enabled UTF8=ACCEPT or IMAP4rev2 (RFC 9051 5.1:
// mailbox names are Net-Unicode, and a server that advertises both
// revisions sends UTF-8 only after "ENABLE IMAP4rev2"). Every other
// session speaks modified UTF-7: names it sends are decoded, names it is
// sent are encoded, in every command and response that carries one.
//
// A modified UTF-7 name must be in the one form the encoder produces
// (RFC 3501 5.1.3 asks the server to check this at least for CREATE);
// anything else is answered NO [CANNOT], whatever the command, since the
// name cannot be stored in its exact form and listed back as such. A
// name with 8-bit octets is UTF-8 in either mode — modified UTF-7 never
// has them — and is taken as is when it is valid UTF-8. Octets that are
// not UTF-8 fit neither form and are answered BAD, as RFC 6855 3 asks
// for UTF-8 quoted strings that are not well-formed.
//
// A new name is put into Unicode Normalization Form C (RFC 9051 5.1);
// see target for how a name in another form is looked up.

// mailboxNameError is a mailbox argument the session's wire form does
// not allow; the command is answered NO [CANNOT] (bad: BAD) after the
// rest of its arguments has been consumed.
type mailboxNameError struct {
	bad bool
	msg string
}

func (e *mailboxNameError) Error() string { return e.msg }

// errNotUTF8 is a name with octets that are not UTF-8.
var errNotUTF8 = &mailboxNameError{bad: true, msg: "Mailbox name is not valid UTF-8"}

// utf8Names reports whether the session sends and receives mailbox names
// in UTF-8 rather than modified UTF-7.
func (c *conn) utf8Names() bool { return c.utf8 || c.rev2 }

// fromWire converts a mailbox name (or LIST reference or pattern) as the
// client sent it to its stored UTF-8 form.
func (c *conn) fromWire(s string) (string, error) {
	// 8-bit octets are UTF-8 even in a session that speaks modified
	// UTF-7, whose names are 7-bit: a client that sends "Grüße & Co"
	// without having enabled UTF-8 means exactly that, and its "&" is an
	// ampersand, not the start of an encoded sequence. Such a name is
	// not decoded.
	if hasNonASCII(s) {
		if !utf8.ValidString(s) {
			return "", errNotUTF8
		}

		return s, nil
	}

	if c.utf8Names() || !strings.Contains(s, "&") {
		return s, nil
	}

	name, err := mutf7.DecodeCanonical(s)
	if err != nil {
		return "", &mailboxNameError{msg: fmt.Sprintf("Mailbox name is not valid modified UTF-7 (%v)", err)}
	}

	return name, nil
}

// listWire converts a LIST/LSUB reference or pattern like fromWire, but
// takes one that is not canonical modified UTF-7 as it stands instead of
// refusing it: a pattern names no mailbox, it only selects, and a pattern
// nothing can match lists nothing (as RFC 3501 6.3.8 allows any string).
// Octets that are not UTF-8 stay an error.
func (c *conn) listWire(s string) (string, error) {
	name, err := c.fromWire(s)
	if err != nil && err != errNotUTF8 {
		return s, nil
	}

	return name, err
}

// mailboxArg reads a mailbox name argument and converts it to its stored
// form.
func (c *conn) mailboxArg() (string, error) {
	s, err := c.r.astring()
	if err != nil {
		return "", err
	}

	return c.fromWire(s)
}

// toWire converts a stored mailbox name to the session's wire form.
func (c *conn) toWire(name string) string {
	if c.utf8Names() {
		return name
	}

	return mutf7.Encode(name)
}

// mailboxOut renders a mailbox name as an astring in the session's wire
// form: an atom where possible, else a quoted string (with UTF-8 only
// for a session that enabled it), else a literal.
func (c *conn) mailboxOut(name string) string {
	s := c.toWire(name)
	if s == "" {
		return c.mailboxString(name)
	}

	for i := 0; i < len(s); i++ {
		if !isAtomChar(s[i]) {
			return c.mailboxString(name)
		}
	}

	return s
}

// mailboxString renders a mailbox name, or a namespace prefix, as a
// string (never an atom) in the session's wire form.
func (c *conn) mailboxString(name string) string {
	s := c.toWire(name)
	if c.utf8Names() && utf8.ValidString(s) && !strings.ContainsAny(s, "\r\n\x00") {
		return quoteUTF8(s)
	}

	return quote(s)
}

// quoteUTF8 is quote for a session that accepts UTF-8 in quoted strings
// (RFC 6855 3, RFC 9051 4.3.1); s holds no CR, LF or NUL.
func quoteUTF8(s string) string {
	var b strings.Builder

	b.WriteByte('"')

	for i := 0; i < len(s); i++ {
		if s[i] == '"' || s[i] == '\\' {
			b.WriteByte('\\')
		}

		b.WriteByte(s[i])
	}

	b.WriteByte('"')

	return b.String()
}

// errNewName is a name CREATE and RENAME refuse to give a mailbox.
var errNewName = errors.New("mailbox name is not Net-Unicode")

// checkNewName refuses a name a new mailbox must not get: RFC 6855 3 and
// RFC 9051 5.1 want Net-Unicode (RFC 5198) without control characters,
// line or paragraph separators. Existing names are not checked: whatever
// the store holds stays reachable.
func checkNewName(name string) error {
	for _, r := range name {
		if unicode.IsControl(r) || r == '\u2028' || r == '\u2029' {
			return errNewName
		}
	}

	return nil
}

// nfcListArgs lets LIST find NFC names with a reference or patterns in
// another normalization form, as target does for a single name: a
// pattern that is not in NFC is kept and joined by its NFC form, so a
// mailbox stored under either form matches. The reference is a literal
// prefix; one that is not in NFC is folded into the patterns first, so
// that both forms of it are tried too. A reference with wildcard
// characters, or a LIST with an empty pattern (which asks for the
// hierarchy delimiter), is left as it is.
func nfcListArgs(ref string, patterns []string) (string, []string) {
	if _, changed := nfcName(ref); changed && !strings.ContainsAny(ref, "*%") && !slices.Contains(patterns, "") {
		for i, p := range patterns {
			patterns[i] = foldReference(ref, p)
		}

		ref = ""
	}

	out := patterns
	for _, p := range patterns {
		if nfc := nfcPattern(p); nfc != p && !slices.Contains(out, nfc) {
			out = append(out, nfc)
		}
	}

	return ref, out
}

// foldReference joins a LIST reference and a pattern into one pattern,
// the way the reference is applied when matching (a pattern that starts
// with the delimiter ignores the reference).
func foldReference(ref, pattern string) string {
	if strings.HasPrefix(pattern, model.Delim) {
		return strings.TrimPrefix(pattern, model.Delim)
	}

	if ref != "" && !strings.HasSuffix(ref, model.Delim) {
		ref += model.Delim
	}

	return ref + pattern
}

// nfcPattern normalizes the literal parts of a LIST pattern to NFC; the
// wildcards stay where they are.
func nfcPattern(p string) string {
	var b strings.Builder

	for {
		i := strings.IndexAny(p, "*%")
		if i < 0 {
			b.WriteString(norm.NFC.String(p))
			return b.String()
		}

		b.WriteString(norm.NFC.String(p[:i]))
		b.WriteByte(p[i])
		p = p[i+1:]
	}
}

// nfcName returns name in Normalization Form C and whether that differs.
func nfcName(name string) (string, bool) {
	if norm.NFC.IsNormalString(name) {
		return name, false
	}

	return norm.NFC.String(name), true
}
