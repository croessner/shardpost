package fts

import (
	"net/url"
	"sort"
	"strconv"
	"strings"

	"shardpost/internal/charset"
)

// mediaParams splits a Content-Type or Content-Disposition value into
// its lower-cased type and its parameters, tolerantly: a malformed
// parameter is skipped instead of failing the whole value, as
// mime.ParseMediaType does. Parameter values are decoded: RFC 2231
// extended values and continuations in any charset of the WHATWG list,
// RFC 2047 encoded words (which mail software writes into file names
// against the rules) and raw 8-bit text in the fallback charset.
func mediaParams(v string) (string, map[string]string) {
	parts := splitParams(v)
	typ := strings.ToLower(strings.TrimSpace(parts[0]))

	plain := map[string]string{}
	ext := map[string]string{}           // name*=charset'lang'value
	pieces := map[string]map[int]piece{} // name*N= and name*N*=

	for _, p := range parts[1:] {
		k, val, ok := strings.Cut(p, "=")
		if !ok {
			continue
		}

		k = strings.ToLower(strings.TrimSpace(k))
		val = unquote(strings.TrimSpace(val))

		name, rest, starred := strings.Cut(k, "*")

		switch {
		case !starred:
			plain[name] = val
		case rest == "":
			ext[name] = val
		default:
			n, err := strconv.Atoi(strings.TrimSuffix(rest, "*"))
			if err != nil || n < 0 || n > 999 {
				continue
			}

			if pieces[name] == nil {
				pieces[name] = map[int]piece{}
			}

			pieces[name][n] = piece{val, strings.HasSuffix(rest, "*")}
		}
	}

	out := map[string]string{}
	for k, v := range plain {
		out[k] = charset.DecodeHeader(v)
	}

	for k, ps := range pieces {
		out[k] = joinPieces(ps)
	}

	for k, v := range ext {
		cs, val := splitExtended(v)
		out[k] = charset.ToUTF8(percentDecode(val), cs)
	}

	return typ, out
}

// piece is one section of an RFC 2231 continuation.
type piece struct {
	value    string
	extended bool // percent-encoded, the first one with charset'lang'
}

// joinPieces assembles an RFC 2231 continuation in section order.
func joinPieces(ps map[int]piece) string {
	keys := make([]int, 0, len(ps))
	for n := range ps {
		keys = append(keys, n)
	}

	sort.Ints(keys)

	var (
		b  []byte
		cs string
	)

	for i, n := range keys {
		p := ps[n]
		if !p.extended {
			b = append(b, p.value...)

			continue
		}

		v := p.value
		if i == 0 {
			cs, v = splitExtended(v)
		}

		b = append(b, percentDecode(v)...)
	}

	if cs == "" {
		return charset.DecodeHeader(string(b))
	}

	return charset.ToUTF8(b, cs)
}

// splitExtended splits "charset'lang'value".
func splitExtended(v string) (string, string) {
	parts := strings.SplitN(v, "'", 3)
	if len(parts) != 3 {
		return "", v
	}

	return parts[0], parts[2]
}

// percentDecode undoes RFC 2231 %XX escapes; a malformed escape stays.
func percentDecode(v string) []byte {
	if s, err := url.PathUnescape(v); err == nil {
		return []byte(s)
	}

	return []byte(v)
}

// splitParams splits a header value at the semicolons that are not
// inside a quoted string.
func splitParams(v string) []string {
	var (
		out   []string
		quote bool
		esc   bool
		start int
	)

	for i := 0; i < len(v); i++ {
		switch c := v[i]; {
		case esc:
			esc = false
		case c == '\\' && quote:
			esc = true
		case c == '"':
			quote = !quote
		case c == ';' && !quote:
			out = append(out, v[start:i])
			start = i + 1
		}
	}

	return append(out, v[start:])
}

// unquote removes the quotes and backslash escapes of a quoted string.
func unquote(v string) string {
	if len(v) < 2 || v[0] != '"' || v[len(v)-1] != '"' {
		return v
	}

	var b strings.Builder

	esc := false

	for i := 1; i < len(v)-1; i++ {
		if c := v[i]; c == '\\' && !esc {
			esc = true
		} else {
			esc = false

			b.WriteByte(c)
		}
	}

	return b.String()
}
