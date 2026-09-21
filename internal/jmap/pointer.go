package jmap

import (
	"errors"
	"strconv"
	"strings"
)

// errPointer is what a pointer that does not resolve returns; the caller
// turns it into invalidResultReference.
var errPointer = errors.New("the result reference does not resolve")

// resolvePointer evaluates a JMAP result-reference pointer (RFC 8620
// 3.7) against a decoded JSON value. It is RFC 6901 with one addition:
// the token "*" applied to an array maps the rest of the pointer over
// every element and flattens the results by one level, which is what
// lets a client say "the ids of everything the previous call returned".
func resolvePointer(v any, pointer string) (any, error) {
	if pointer == "" {
		return v, nil
	}

	if !strings.HasPrefix(pointer, "/") {
		return nil, errPointer
	}

	return walk(v, strings.Split(pointer[1:], "/"))
}

// walk applies the remaining tokens to v.
func walk(v any, tokens []string) (any, error) {
	for i, tok := range tokens {
		tok = unescapeToken(tok)

		switch cur := v.(type) {
		case map[string]any:
			next, ok := cur[tok]
			if !ok {
				return nil, errPointer
			}

			v = next
		case []any:
			if tok == "*" {
				return mapRest(cur, tokens[i+1:])
			}

			next, err := indexOf(cur, tok)
			if err != nil {
				return nil, err
			}

			v = next
		default:
			return nil, errPointer
		}
	}

	return v, nil
}

// mapRest applies the rest of the pointer to every element of the array
// and flattens the results by one level.
func mapRest(items []any, tokens []string) (any, error) {
	out := make([]any, 0, len(items))

	for _, item := range items {
		got, err := walk(item, tokens)
		if err != nil {
			return nil, err
		}

		if inner, ok := got.([]any); ok {
			out = append(out, inner...)
			continue
		}

		out = append(out, got)
	}

	return out, nil
}

// indexOf returns the element the token names, refusing the leading
// zeros and the "-" that RFC 6901 does not allow here.
func indexOf(items []any, tok string) (any, error) {
	if tok == "" || (len(tok) > 1 && tok[0] == '0') {
		return nil, errPointer
	}

	n, err := strconv.Atoi(tok)
	if err != nil || n < 0 || n >= len(items) {
		return nil, errPointer
	}

	return items[n], nil
}

// unescapeToken undoes the RFC 6901 escaping, "~1" before "~0" so that a
// literal "~1" in a key survives.
func unescapeToken(tok string) string {
	if !strings.Contains(tok, "~") {
		return tok
	}

	return strings.ReplaceAll(strings.ReplaceAll(tok, "~1", "/"), "~0", "~")
}
