package migrate

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// Wire format shared by the IMAP and ManageSieve source readers: a
// response line whose literals ({n} or {n+} at a line end) are read and
// kept aside, and a parser for the IMAP value syntax.

// maxLine bounds one response line without literals; maxLiteral one
// literal (a message is at most this large).
const (
	maxLine    = 1 << 20
	maxLiteral = 512 << 20
)

// lineReader reads responses.
type lineReader struct {
	br *bufio.Reader
	// litLimit bounds a literal (maxLiteral when 0).
	litLimit int64
}

// response is one response line with its literals; the text holds a
// placeholder "\x00<i>\x00" where literal i was.
type response struct {
	text string
	lits [][]byte
}

// readResponse reads one line, following literals.
func (lr *lineReader) readResponse() (*response, error) {
	var (
		b    strings.Builder
		lits [][]byte
	)

	for {
		line, err := lr.readLine()
		if err != nil {
			return nil, err
		}

		n, prefix, ok := literalSuffix(line)
		if !ok {
			b.WriteString(line)
			return &response{text: b.String(), lits: lits}, nil
		}

		limit := lr.litLimit
		if limit == 0 {
			limit = maxLiteral
		}

		if n > limit {
			return nil, fmt.Errorf("literal of %d bytes exceeds the limit", n)
		}

		lit := make([]byte, n)
		if _, err := io.ReadFull(lr.br, lit); err != nil {
			return nil, err
		}

		b.WriteString(prefix)
		fmt.Fprintf(&b, "\x00%d\x00", len(lits))
		lits = append(lits, lit)
	}
}

// readLine reads a line without its CRLF.
func (lr *lineReader) readLine() (string, error) {
	var b []byte

	for {
		chunk, isPrefix, err := lr.br.ReadLine()
		if err != nil {
			return "", err
		}

		b = append(b, chunk...)
		if len(b) > maxLine {
			return "", errors.New("response line too long")
		}

		if !isPrefix {
			return string(b), nil
		}
	}
}

// literalSuffix recognises "{n}" or "{n+}" at the end of line.
func literalSuffix(line string) (int64, string, bool) {
	if !strings.HasSuffix(line, "}") {
		return 0, "", false
	}

	i := strings.LastIndexByte(line, '{')
	if i < 0 {
		return 0, "", false
	}

	num := strings.TrimSuffix(line[i+1:len(line)-1], "+")

	n, err := strconv.ParseInt(num, 10, 64)
	if err != nil || n < 0 {
		return 0, "", false
	}

	return n, line[:i], true
}

// value is a parsed IMAP value: atom (string), quoted string (qstring),
// literal ([]byte), list ([]value) or nil (NIL).
type value any

// qstring is a quoted string (distinguished from an atom).
type qstring string

// parser parses the values of a response.
type parser struct {
	s    string
	i    int
	lits [][]byte
}

func newParser(r *response, from int) *parser { return &parser{s: r.text, i: from, lits: r.lits} }

// values parses all remaining values.
func (p *parser) values() ([]value, error) {
	var out []value

	for {
		p.skipSpace()

		if p.i >= len(p.s) {
			return out, nil
		}

		v, err := p.value()
		if err != nil {
			return nil, err
		}

		out = append(out, v)
	}
}

func (p *parser) skipSpace() {
	for p.i < len(p.s) && p.s[p.i] == ' ' {
		p.i++
	}
}

// value parses one value.
func (p *parser) value() (value, error) {
	if p.i >= len(p.s) {
		return nil, errors.New("unexpected end of response")
	}

	switch c := p.s[p.i]; c {
	case '(':
		p.i++
		return p.list()
	case '"':
		return p.quoted()
	case 0:
		return p.literal()
	default:
		return p.atom()
	}
}

func (p *parser) list() (value, error) {
	out := []value{}

	for {
		p.skipSpace()

		if p.i >= len(p.s) {
			return nil, errors.New("unterminated list")
		}

		if p.s[p.i] == ')' {
			p.i++
			return out, nil
		}

		v, err := p.value()
		if err != nil {
			return nil, err
		}

		out = append(out, v)
	}
}

func (p *parser) quoted() (value, error) {
	var b strings.Builder

	for p.i++; p.i < len(p.s); p.i++ {
		switch c := p.s[p.i]; c {
		case '\\':
			p.i++
			if p.i < len(p.s) {
				b.WriteByte(p.s[p.i])
			}
		case '"':
			p.i++
			return qstring(b.String()), nil
		default:
			b.WriteByte(c)
		}
	}

	return nil, errors.New("unterminated quoted string")
}

func (p *parser) literal() (value, error) {
	end := strings.IndexByte(p.s[p.i+1:], 0)
	if end < 0 {
		return nil, errors.New("broken literal marker")
	}

	n, err := strconv.Atoi(p.s[p.i+1 : p.i+1+end])
	if err != nil || n >= len(p.lits) {
		return nil, errors.New("broken literal marker")
	}

	p.i += end + 2

	return p.lits[n], nil
}

// atom reads up to a space or parenthesis; a bracketed part (BODY[...],
// response codes) is kept whole.
func (p *parser) atom() (value, error) {
	start := p.i

	for p.i < len(p.s) {
		c := p.s[p.i]
		if c == '[' {
			end := strings.IndexByte(p.s[p.i:], ']')
			if end < 0 {
				return nil, errors.New("unterminated bracket")
			}

			p.i += end + 1

			continue
		}

		if c == ' ' || c == '(' || c == ')' {
			break
		}

		p.i++
	}

	a := p.s[start:p.i]
	if a == "" {
		return nil, fmt.Errorf("unexpected %q", p.s[p.i])
	}

	if strings.EqualFold(a, "NIL") {
		return nil, nil
	}

	return a, nil
}

// str returns the string of an atom, quoted string or literal.
func str(v value) (string, bool) {
	switch x := v.(type) {
	case string:
		return x, true
	case qstring:
		return string(x), true
	case []byte:
		return string(x), true
	default:
		return "", false
	}
}

// num returns the number of an atom.
func num(v value) (uint64, bool) {
	s, ok := v.(string)
	if !ok {
		return 0, false
	}

	n, err := strconv.ParseUint(s, 10, 64)

	return n, err == nil
}

// quote renders s as an IMAP/ManageSieve quoted string, or as a
// synchronising-free literal (LITERAL+) when it cannot be quoted.
func quote(s string) string {
	if strings.ContainsAny(s, "\r\n\x00") || !isASCII(s) {
		return fmt.Sprintf("{%d+}\r\n%s", len(s), s)
	}

	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"`
}

func isASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= 0x80 {
			return false
		}
	}

	return true
}
