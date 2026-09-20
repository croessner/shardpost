// Package imapproto is the IMAP protocol layer: an own parser, command
// dispatcher and response encoder on top of the model.User storage API.
// It replaces go-imap's imapserver so that extensions the library does not
// parse (CONDSTORE, QRESYNC, QUOTA, SORT, THREAD, OBJECTID, PREVIEW,
// SAVEDATE, ID) are first-class. go-imap is still used as a pure library
// for MIME section extraction, body structures and envelopes.
package imapproto

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// --- reading ---------------------------------------------------------------

// tokenKind classifies a parsed argument.
type tokenKind int

const (
	tokAtom   tokenKind = iota
	tokString           // quoted string or literal
	tokList             // parenthesised list
	tokEOL
)

// token is one parsed IMAP argument.
type token struct {
	kind tokenKind
	str  string
	list []token
}

func (t token) String() string {
	switch t.kind {
	case tokList:
		parts := make([]string, len(t.list))
		for i, x := range t.list {
			parts[i] = x.String()
		}

		return "(" + strings.Join(parts, " ") + ")"
	case tokEOL:
		return "<eol>"
	}

	return t.str
}

// isAtomLike reports whether the token can be used where an atom or
// astring is expected.
func (t token) isString() bool { return t.kind == tokAtom || t.kind == tokString }

var errSyntax = errors.New("syntax error")

// reader parses one command line at a time.
type reader struct {
	br      *bufio.Reader
	line    []byte // current line without CRLF
	pos     int
	cont    func(n int64) error // called before reading a synchronising literal
	reserve func(n int64) error // memory accounting before a literal is read
	// refuseSync makes a synchronising literal a syntax error instead of
	// sending the continuation: used when draining a refused command
	// (the client only sends such data after a "+").
	refuseSync  bool
	maxLiteral  int64
	nonSyncMax  int64 // LITERAL- limit for {n+}
	literalPlus bool
	// onLine and onLiteral report what the parser consumes (protocol
	// trace): every line read and the size of every literal read.
	onLine    func(line []byte)
	onLiteral func(n int64)
}

func newReader(r io.Reader, maxLiteral int64) *reader {
	return &reader{br: bufio.NewReaderSize(r, maxLineLen+2), maxLiteral: maxLiteral, nonSyncMax: 4096}
}

// maxLineLen bounds one command line (literals are read separately and
// bounded by maxLiteral). Longer lines are a protocol error, not a
// memory allocation.
const maxLineLen = 64 << 10

var errLineTooLong = errors.New("line too long")

// readLine reads the next line (without CRLF).
func (r *reader) readLine() error {
	line, err := r.br.ReadSlice('\n')
	if errors.Is(err, bufio.ErrBufferFull) {
		// drain the rest of the oversized line, then fail this command
		for errors.Is(err, bufio.ErrBufferFull) {
			_, err = r.br.ReadSlice('\n')
		}

		r.line, r.pos = nil, 0

		return errLineTooLong
	}

	if err != nil && len(line) == 0 {
		return err
	}

	if len(line) > maxLineLen {
		r.line, r.pos = nil, 0
		return errLineTooLong
	}

	r.line = append(r.line[:0], bytes.TrimRight(line, "\r\n")...)
	r.pos = 0

	if r.onLine != nil {
		r.onLine(r.line)
	}

	return nil
}

// endOfLine reports a syntax error when a command carries more than its
// arguments. A name that the parser stopped reading early — an unquoted
// umlaut, for instance — would otherwise be acted on silently, and the
// client would be told OK for a mailbox it never named.
func (r *reader) endOfLine() error {
	if r.eol() {
		return nil
	}

	return fmt.Errorf("%w: unexpected data after the arguments", errSyntax)
}

func (r *reader) eol() bool {
	r.skipSpaces()
	return r.pos >= len(r.line)
}

func (r *reader) skipSpaces() {
	for r.pos < len(r.line) && r.line[r.pos] == ' ' {
		r.pos++
	}
}

func (r *reader) peek() byte {
	if r.pos < len(r.line) {
		return r.line[r.pos]
	}

	return 0
}

// rest returns the unparsed remainder of the line.
func (r *reader) rest() string {
	r.skipSpaces()
	s := string(r.line[r.pos:])
	r.pos = len(r.line)

	return s
}

func isAtomChar(c byte) bool {
	switch c {
	case '(', ')', '{', ' ', '%', '*', '"', '\\', ']', 0, '\r', '\n':
		return false
	}

	return c > 31 && c < 127
}

// atom reads an atom; '*' and '%' and ']' are accepted as they occur in
// sequence sets, list patterns and section specs.
func (r *reader) atom() (string, error) {
	r.skipSpaces()

	start := r.pos
	for r.pos < len(r.line) {
		c := r.line[r.pos]
		if isAtomChar(c) || c == '*' || c == '%' || c == ']' || c == '\\' || c == '$' {
			r.pos++
			continue
		}

		break
	}

	if r.pos == start {
		return "", fmt.Errorf("%w: expected atom at %q", errSyntax, r.line[start:])
	}

	return string(r.line[start:r.pos]), nil
}

// maxNesting bounds parenthesised list depth.
const maxNesting = 16

// next reads the next argument: atom, quoted string, literal or list.
func (r *reader) next() (token, error) {
	return r.nextDepth(0)
}

func (r *reader) nextDepth(depth int) (token, error) {
	r.skipSpaces()

	if r.pos >= len(r.line) {
		return token{kind: tokEOL}, nil
	}

	switch r.line[r.pos] {
	case '"':
		s, err := r.quoted()
		return token{kind: tokString, str: s}, err
	case '{':
		s, err := r.literal()
		return token{kind: tokString, str: s}, err
	case '~':
		if r.pos+1 < len(r.line) && r.line[r.pos+1] == '{' {
			s, err := r.literal()
			return token{kind: tokString, str: s}, err
		}
	case '(':
		if depth >= maxNesting {
			return token{}, fmt.Errorf("%w: list nesting too deep", errSyntax)
		}

		return r.list(depth)
	case ')':
		return token{}, fmt.Errorf("%w: unexpected )", errSyntax)
	}

	a, err := r.atom()

	return token{kind: tokAtom, str: a}, err
}

// list reads the elements of a parenthesised list whose opening
// parenthesis is at the current position, up to the closing one.
func (r *reader) list(depth int) (token, error) {
	r.pos++

	var list []token

	for {
		r.skipSpaces()

		if r.pos < len(r.line) && r.line[r.pos] == ')' {
			r.pos++
			return token{kind: tokList, list: list}, nil
		}

		t, err := r.nextDepth(depth + 1)
		if err != nil {
			return token{}, err
		}

		if t.kind == tokEOL {
			return token{}, fmt.Errorf("%w: unterminated list", errSyntax)
		}

		list = append(list, t)
	}
}

// astring reads an atom, quoted string or literal and returns its value.
func (r *reader) astring() (string, error) {
	t, err := r.next()
	if err != nil {
		return "", err
	}

	if !t.isString() {
		return "", fmt.Errorf("%w: expected string, got %s", errSyntax, t)
	}

	return t.str, nil
}

func (r *reader) quoted() (string, error) {
	r.pos++ // opening quote

	var b strings.Builder

	for r.pos < len(r.line) {
		c := r.line[r.pos]
		r.pos++

		switch c {
		case '"':
			return b.String(), nil
		case '\\':
			if r.pos >= len(r.line) {
				return "", fmt.Errorf("%w: bad escape", errSyntax)
			}

			b.WriteByte(r.line[r.pos])
			r.pos++
		default:
			b.WriteByte(c)
		}
	}

	return "", fmt.Errorf("%w: unterminated quoted string", errSyntax)
}

// literal reads {n}, {n+} (LITERAL+/LITERAL-) or ~{n} (BINARY). The literal
// bytes follow the current line; after them the command line continues.
func (r *reader) literal() (string, error) {
	n, nonSync, err := r.literalSpec()
	if err != nil {
		return "", err
	}

	if err := r.admitLiteral(n, nonSync); err != nil {
		if nonSync {
			// The client sent the data without waiting: skip it, or it
			// would be read as command lines.
			if derr := r.discardLiteral(n); derr != nil {
				return "", derr
			}
		}

		return "", err
	}

	buf := make([]byte, n)
	if _, err := io.ReadFull(r.br, buf); err != nil {
		return "", err
	}

	if r.onLiteral != nil {
		r.onLiteral(n)
	}
	// The rest of the command continues on the following line.
	if err := r.readLine(); err != nil {
		return "", err
	}

	return string(buf), nil
}

// discardFactor bounds the refused non-synchronising literal that is
// read and discarded, as a multiple of maxLiteral; a larger one closes
// the connection (RFC 7888 section 4).
const discardFactor = 2

var errLiteralNotDiscarded = errors.New("refused non-synchronising literal too large to skip")

// discardLiteral skips a refused non-synchronising literal and reads the
// line the command continues on.
func (r *reader) discardLiteral(n int64) error {
	if n > discardFactor*r.maxLiteral {
		return errLiteralNotDiscarded
	}

	if _, err := io.CopyN(io.Discard, r.br, n); err != nil {
		return err
	}

	if r.onLiteral != nil {
		r.onLiteral(n)
	}

	return r.readLine()
}

// literalSpec parses the literal header {n}, {n+} or ~{n} at the end of the
// current line and returns the size and whether it is non-synchronising.
func (r *reader) literalSpec() (int64, bool, error) {
	if r.line[r.pos] == '~' {
		r.pos++
	}

	if r.pos >= len(r.line) || r.line[r.pos] != '{' {
		return 0, false, fmt.Errorf("%w: expected literal", errSyntax)
	}

	end := bytes.IndexByte(r.line[r.pos:], '}')
	if end < 0 || end != len(r.line)-r.pos-1 {
		return 0, false, fmt.Errorf("%w: bad literal", errSyntax)
	}

	spec := string(r.line[r.pos+1 : r.pos+end])
	nonSync := strings.HasSuffix(spec, "+")
	spec = strings.TrimSuffix(spec, "+")

	n, err := strconv.ParseInt(spec, 10, 64)
	if err != nil || n < 0 {
		return 0, false, fmt.Errorf("%w: bad literal size", errSyntax)
	}

	return n, nonSync, nil
}

// admitLiteral enforces the literal limits, reserves the memory and, for a
// synchronising literal, sends the continuation request.
func (r *reader) admitLiteral(n int64, nonSync bool) error {
	if n > r.maxLiteral {
		return &literalTooLarge{n}
	}

	if nonSync && n > r.nonSyncMax && !r.literalPlus {
		return &literalTooLarge{n}
	}

	if !nonSync && r.refuseSync {
		return fmt.Errorf("%w: literal refused", errSyntax)
	}

	if r.reserve != nil {
		if err := r.reserve(n); err != nil {
			return err
		}
	}

	if !nonSync && r.cont != nil {
		if err := r.cont(n); err != nil {
			return err
		}
	}

	return nil
}

type literalTooLarge struct{ n int64 }

func (e *literalTooLarge) Error() string { return fmt.Sprintf("literal too large (%d bytes)", e.n) }

// --- writing ---------------------------------------------------------------

// writer encodes responses. All methods buffer; flush sends.
type writer struct {
	bw          *bufio.Writer
	beforeFlush func() // e.g. arm the write deadline
}

func newWriter(w io.Writer) *writer { return &writer{bw: bufio.NewWriterSize(w, 64<<10)} }

func (w *writer) flush() error {
	if w.beforeFlush != nil {
		w.beforeFlush()
	}

	return w.bw.Flush()
}

// line buffers one formatted response line with CRLF. Write errors are
// sticky in bufio.Writer and surface on the next flush, so they are not
// checked here.
func (w *writer) line(format string, args ...any) {
	_, _ = fmt.Fprintf(w.bw, format, args...)
	_, _ = w.bw.WriteString("\r\n")
}

// untagged writes "* <text>".
func (w *writer) untagged(format string, args ...any) {
	_, _ = w.bw.WriteString("* ")
	w.line(format, args...)
}

// quote encodes a quoted string; falls back to a literal if needed.
func quote(s string) string {
	if strings.ContainsAny(s, "\r\n") || hasNonASCII(s) {
		return fmt.Sprintf("{%d}\r\n%s", len(s), s)
	}

	var b strings.Builder
	b.WriteByte('"')

	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == '"' || c == '\\' {
			b.WriteByte('\\')
		}

		b.WriteByte(c)
	}

	b.WriteByte('"')

	return b.String()
}

func hasNonASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= 128 || s[i] == 0 {
			return true
		}
	}

	return false
}

// nstring encodes NIL or a quoted string.
func nstring(s string) string {
	if s == "" {
		return kwNIL
	}

	return quote(s)
}

// astringOut encodes a mailbox name or similar: atom if possible, else
// quoted (INBOX stays bare, names with spaces get quotes).
func astringOut(s string) string {
	if s == "" {
		return `""`
	}

	for i := 0; i < len(s); i++ {
		if !isAtomChar(s[i]) {
			return quote(s)
		}
	}

	return s
}
