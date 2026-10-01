package fts

import (
	"slices"
	"strings"
	"unicode/utf8"
)

// Matcher tells which words of a text a full-text query matched, with
// the semantics the stores search a substring field (subject, body) by:
// a query token matches an index term — the word or one of its stems —
// that it equals, that equals one of the token's own stems, or that
// contains it; a token shorter than a trigram matches a term it is a
// prefix of. It is what highlighting a search result needs: the index
// says which messages match, not where.
type Matcher struct {
	toks     []string
	expanded [][]string
}

// NewMatcher returns the matcher of query tokens, as Tokenize returns
// them. A matcher without tokens matches nothing.
func NewMatcher(toks []string) *Matcher {
	m := &Matcher{toks: toks}
	for _, t := range toks {
		m.expanded = append(m.expanded, Expand(t))
	}

	return m
}

// Empty reports whether the matcher has no tokens.
func (m *Matcher) Empty() bool { return m == nil || len(m.toks) == 0 }

// Match reports whether a word of a text is one the query matched.
func (m *Matcher) Match(word string) bool {
	if m.Empty() {
		return false
	}

	w, ok := token(word)
	if !ok {
		return false
	}

	var terms []string // the word's index terms, stemmed only when needed

	for i, tok := range m.toks {
		if termMatch(w, tok) {
			return true
		}

		if terms == nil {
			terms = Expand(w)
		}

		for _, t := range terms {
			if slices.Contains(m.expanded[i], t) || termMatch(t, tok) {
				return true
			}
		}
	}

	return false
}

// termMatch is the substring test of a term against a query token: a
// token of a trigram or more is looked for anywhere in the term, a
// shorter one at its start.
func termMatch(term, tok string) bool {
	if utf8.RuneCountInString(tok) < 3 {
		return strings.HasPrefix(term, tok)
	}

	return strings.Contains(term, tok)
}
