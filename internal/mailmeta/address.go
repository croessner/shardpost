package mailmeta

import (
	"strings"

	"github.com/emersion/go-imap/v2"
)

// ParseAddressList parses an RFC 5322 address-list (with the obsolete
// syntax that still shows up: comments inside addr-specs, a comment after
// a bare address as display name, source routes, empty groups) into the
// ENVELOPE address form: a group is a start entry (mailbox = group name,
// no host) followed by its members and an all-empty end entry (RFC 3501
// 7.4.2). Encoded words in names are decoded; source routes are dropped
// here, ParseAddressListRoutes returns them.
func ParseAddressList(v string) []imap.Address {
	out, _ := ParseAddressListRoutes(v)
	return out
}

// ParseAddressListRoutes is ParseAddressList with the obsolete source
// routes (RFC 5322 4.4 obs-route, "<@a,@b:user@host>"), which the ENVELOPE
// reports as the at-domain-list of an address (RFC 3501 7.4.2). The
// routes run parallel to the addresses, "" for an address without one;
// they are nil when no address has a route.
func ParseAddressListRoutes(v string) ([]imap.Address, []string) {
	p := &addrParser{s: v}

	var out []imap.Address

	for {
		p.cfws()

		if p.eof() {
			break
		}

		if p.peek() == ',' || p.peek() == ';' {
			p.i++
			continue
		}

		out = p.address(out, false)
	}

	if len(p.routes) == 0 {
		return out, nil
	}

	routes := make([]string, len(out))
	for i, r := range p.routes {
		if i < len(routes) {
			routes[i] = r
		}
	}

	return out, routes
}

type addrParser struct {
	s string
	i int
	// routes are the source routes found, by index of the address.
	routes map[int]string
}

func (p *addrParser) eof() bool  { return p.i >= len(p.s) }
func (p *addrParser) peek() byte { return p.s[p.i] }

// cfws skips whitespace and comments; the last comment text is returned.
func (p *addrParser) cfws() string {
	last := ""

	for !p.eof() {
		switch p.peek() {
		case ' ', '\t', '\r', '\n':
			p.i++
		case '(':
			last = p.comment()
		default:
			return last
		}
	}

	return last
}

func (p *addrParser) comment() string {
	depth := 0

	var b strings.Builder

	for !p.eof() {
		c := p.peek()

		p.i++
		switch {
		case c == '\\' && !p.eof():
			b.WriteByte(p.peek())
			p.i++
		case c == '(':
			depth++
			if depth > 1 {
				b.WriteByte(c)
			}
		case c == ')':
			depth--
			if depth == 0 {
				return strings.TrimSpace(b.String())
			}

			b.WriteByte(c)
		default:
			b.WriteByte(c)
		}
	}

	return strings.TrimSpace(b.String())
}

// word reads a quoted string or an atom (anything up to a special).
func (p *addrParser) word() (string, bool) {
	if p.eof() {
		return "", false
	}

	if p.peek() == '"' {
		p.i++

		var b strings.Builder

		for !p.eof() && p.peek() != '"' {
			if p.peek() == '\\' && p.i+1 < len(p.s) {
				p.i++
			}

			b.WriteByte(p.peek())
			p.i++
		}

		if !p.eof() {
			p.i++
		}

		return b.String(), true
	}

	start := p.i
	for !p.eof() && !strings.ContainsRune("()<>@,:;\"\\ \t\r\n", rune(p.peek())) {
		p.i++
	}

	return p.s[start:p.i], p.i > start
}

// phrase collects words (comments dropped) until a special that decides
// what the address is. It returns the words and the deciding character
// (0 at end of input). As a display name the words are joined by spaces;
// as a local part they are concatenated (a quoted word keeps its spaces).
func (p *addrParser) phrase() ([]string, byte) {
	var words []string

	for {
		p.cfws()

		if p.eof() {
			return words, 0
		}

		c := p.peek()
		if w, ok := p.word(); ok {
			words = append(words, w)
			continue
		}

		if c == '.' { // dots inside a local part: handled by word() as atext
			p.i++
			continue
		}

		return words, c
	}
}

func (p *addrParser) address(out []imap.Address, inGroup bool) []imap.Address {
	start := p.i
	words, c := p.phrase()
	text := strings.Join(words, " ")
	local := strings.Join(words, "")

	switch c {
	case ':':
		if inGroup { // a group inside a group is not allowed: treat as junk
			p.i++
			return out
		}

		p.i++

		return p.group(out, text)
	case '<':
		p.i++
		mbox, host, route := p.angle()

		if route != "" {
			if p.routes == nil {
				p.routes = map[int]string{}
			}

			p.routes[len(out)] = route
		}

		return append(out, imap.Address{Name: decodeName(text), Mailbox: mbox, Host: host})
	case '@':
		// bare addr-spec: the phrase so far is the local part (with the
		// dots and comments of the obsolete syntax already removed)
		p.i++

		host, name := p.domain()
		if !p.eof() && p.peek() != ',' && p.peek() != ';' {
			p.skipJunk()
		}

		return append(out, imap.Address{Name: decodeName(name), Mailbox: local, Host: host})
	default:
		if local != "" { // a local part without domain
			return append(out, imap.Address{Mailbox: local})
		}

		if p.i == start {
			p.i++ // junk character: never stall
		}

		return out
	}
}

// group parses the members of a group after "name:" up to and including
// the terminating ";" and appends the group start, members and end marker.
func (p *addrParser) group(out []imap.Address, name string) []imap.Address {
	out = append(out, imap.Address{Mailbox: decodeName(name)})

	for {
		p.cfws()

		if p.eof() {
			break
		}

		if p.peek() == ';' {
			p.i++
			break
		}

		if p.peek() == ',' {
			p.i++
			continue
		}

		out = p.address(out, true)
	}

	return append(out, imap.Address{})
}

// angle parses the inside of <...> after the "<".
func (p *addrParser) angle() (mbox, host, route string) {
	p.cfws()

	if !p.eof() && p.peek() == '@' { // obs-route "@a,@b:"
		route = p.route()
	}

	mbox = p.angleLocal()

	if !p.eof() && p.peek() == '@' {
		p.i++
		host, _ = p.domain()
	}

	for !p.eof() && p.peek() != '>' {
		p.i++
	}

	if !p.eof() {
		p.i++
	}

	return mbox, host, route
}

// route reads an obsolete source route "@a,@b:" and returns it without
// the colon, whitespace and comments ("@a,@b").
func (p *addrParser) route() string {
	var b strings.Builder

	for !p.eof() && p.peek() != ':' && p.peek() != '>' {
		switch c := p.peek(); c {
		case '(':
			p.comment()
		case ' ', '\t', '\r', '\n':
			p.i++
		default:
			b.WriteByte(c)

			p.i++
		}
	}

	if !p.eof() && p.peek() == ':' {
		p.i++
	}

	return b.String()
}

// angleLocal reads the local part inside <...> up to "@" or ">".
func (p *addrParser) angleLocal() string {
	var local []string

	for {
		p.cfws()

		if p.eof() || p.peek() == '>' || p.peek() == '@' {
			break
		}

		if p.peek() == '.' {
			local = append(local, ".")
			p.i++

			continue
		}

		w, ok := p.word()
		if !ok {
			p.i++
			continue
		}

		local = append(local, w)
	}

	return strings.Join(local, "")
}

// domain reads a (possibly comment-interrupted) domain or domain literal;
// a comment after it (the obsolete display name) is returned too.
func (p *addrParser) domain() (string, string) {
	var (
		b        strings.Builder
		trailing string
	)
	for {
		trailing = p.cfws()
		if p.eof() {
			break
		}

		c := p.peek()
		if c == '[' {
			for !p.eof() && p.peek() != ']' {
				b.WriteByte(p.peek())
				p.i++
			}

			if !p.eof() {
				b.WriteByte(']')

				p.i++
			}

			continue
		}

		if c == '.' {
			b.WriteByte('.')

			p.i++

			continue
		}

		w, ok := p.word()
		if !ok {
			break
		}

		b.WriteString(w)
	}

	return b.String(), trailing
}

func (p *addrParser) skipJunk() {
	for !p.eof() && p.peek() != ',' && p.peek() != ';' {
		p.i++
	}
}

func decodeName(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}

	if strings.Contains(s, "=?") {
		if d, err := WordDecoder.DecodeHeader(s); err == nil {
			return d
		}
	}

	return s
}
