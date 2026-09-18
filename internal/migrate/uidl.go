package migrate

import (
	"fmt"
	"strconv"
	"strings"
)

// uidlFormat renders Dovecot's pop3_uidl_format for a message, so POP3
// clients keep their "already downloaded" state after a migration. Both
// syntaxes are understood:
//
//   - Dovecot 2.3: %u (UID), %v (UIDVALIDITY), %g (GUID), with optional
//     zero padding and hex, e.g. "%08Xu%08Xv" (the old default);
//     %{uid}, %{uidvalidity}, %{guid} as long names.
//   - Dovecot 2.4: %{uid | hex(8)}, %{uidvalidity | hex(8) | upper}, with
//     the filters hex, hex(n), upper and lower.
//
// Header-hash variables (%m, %{md5}) are not supported: the message
// would have to be hashed like Dovecot does, which is not verifiable here.
type uidlFormat []uidlPart

// Variables of a UIDL format.
const (
	varUID         = "uid"
	varUIDValidity = "uidvalidity"
	varGUID        = "guid"
)

// uidlPart is literal text or one variable.
type uidlPart struct {
	text  string
	name  string // "uid", "uidvalidity", "guid"
	hex   bool
	upper bool
	width int
}

// parseUIDLFormat parses a format ("" = no UIDL is stored).
func parseUIDLFormat(f string) (uidlFormat, error) {
	if f == "" {
		return nil, nil
	}

	var out uidlFormat

	for i := 0; i < len(f); {
		if f[i] != '%' {
			j := strings.IndexByte(f[i:], '%')
			if j < 0 {
				j = len(f) - i
			}

			out = append(out, uidlPart{text: f[i : i+j]})
			i += j

			continue
		}

		p, n, err := parseUIDLVar(f[i+1:])
		if err != nil {
			return nil, fmt.Errorf("pop3 uidl format %q: %w", f, err)
		}

		out = append(out, p)
		i += 1 + n
	}

	return out, nil
}

// parseUIDLVar parses one variable after "%"; n is its length.
func parseUIDLVar(s string) (uidlPart, int, error) {
	if strings.HasPrefix(s, "%") {
		return uidlPart{text: "%"}, 1, nil
	}

	if strings.HasPrefix(s, "{") {
		end := strings.IndexByte(s, '}')
		if end < 0 {
			return uidlPart{}, 0, fmt.Errorf("unterminated %%{")
		}

		p, err := parseFilters(s[1:end])

		return p, end + 1, err
	}

	return parseShortVar(s)
}

// parseShortVar parses the 2.3 form: [width][X|x]{u,v,g}.
func parseShortVar(s string) (uidlPart, int, error) {
	var p uidlPart

	i := 0
	for i < len(s) && s[i] >= '0' && s[i] <= '9' {
		i++
	}

	p.width, _ = strconv.Atoi(s[:i])

	if i < len(s) && (s[i] == 'X' || s[i] == 'x') {
		p.hex, p.upper = true, s[i] == 'X'
		i++
	}

	if i >= len(s) {
		return uidlPart{}, 0, fmt.Errorf("incomplete variable")
	}

	names := map[byte]string{'u': varUID, 'v': varUIDValidity, 'g': varGUID}

	p.name = names[s[i]]
	if p.name == "" {
		return uidlPart{}, 0, fmt.Errorf("unsupported variable %%%c", s[i])
	}

	return p, i + 1, nil
}

// parseFilters parses "name | filter | ..." of the 2.4 syntax.
func parseFilters(expr string) (uidlPart, error) {
	fields := strings.Split(expr, "|")
	p := uidlPart{name: strings.TrimSpace(fields[0])}

	switch p.name {
	case varUID, varUIDValidity, varGUID:
	default:
		return p, fmt.Errorf("unsupported variable %q", p.name)
	}

	for _, f := range fields[1:] {
		f = strings.TrimSpace(f)

		switch {
		case f == "upper":
			p.upper = true
		case f == "lower":
			p.upper = false
		case f == "hex":
			p.hex = true
		case strings.HasPrefix(f, "hex(") && strings.HasSuffix(f, ")"):
			w, err := strconv.Atoi(f[4 : len(f)-1])
			if err != nil || w < 0 || w > 32 {
				return p, fmt.Errorf("bad filter %q", f)
			}

			p.hex, p.width = true, w
		default:
			return p, fmt.Errorf("unsupported filter %q", f)
		}
	}

	return p, nil
}

// format renders the UIDL of a message.
func (f uidlFormat) format(uid, uidValidity uint32, guid string) string {
	var b strings.Builder

	for _, p := range f {
		if p.name == "" {
			b.WriteString(p.text)
			continue
		}

		var s string

		switch p.name {
		case varUID, varUIDValidity:
			n := uint64(uid)
			if p.name == varUIDValidity {
				n = uint64(uidValidity)
			}

			if p.hex {
				s = strconv.FormatUint(n, 16)
			} else {
				s = strconv.FormatUint(n, 10)
			}
		case varGUID:
			s = guid
		}

		if p.upper {
			s = strings.ToUpper(s)
		}

		if len(s) < p.width {
			s = strings.Repeat("0", p.width-len(s)) + s
		}

		b.WriteString(s)
	}

	return b.String()
}
