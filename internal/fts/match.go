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
//
// A Matcher remembers its answer per word, so a text that repeats its
// words stems each of them once. It is not safe for concurrent use.
type Matcher struct {
	toks     []string
	expanded [][]string
	// firsts are the first letters, folded, of the tokens and of their
	// stems: a word that contains none of them cannot match (Match).
	firsts []rune
	memo   map[string]bool
}

// NewMatcher returns the matcher of query tokens, as Tokenize returns
// them. A matcher without tokens matches nothing.
func NewMatcher(toks []string) *Matcher {
	m := &Matcher{toks: toks, memo: map[string]bool{}}

	for _, t := range toks {
		exp := Expand(t)
		m.expanded = append(m.expanded, exp)

		for _, e := range exp {
			if r, _ := utf8.DecodeRuneInString(fold(e)); !slices.Contains(m.firsts, r) {
				m.firsts = append(m.firsts, r)
			}
		}
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

	if v, ok := m.memo[w]; ok {
		return v
	}

	v := m.match(w)
	m.memo[w] = v

	return v
}

// match is Match without the memo.
func (m *Matcher) match(w string) bool {
	for _, tok := range m.toks {
		if termMatch(w, tok) {
			return true
		}
	}

	// A stem keeps the letters of its word apart from a changed ending
	// (and the German umlauts, which fold), so a stem that equals or
	// contains a token, or a token's stem, has that token's first letter
	// among the word's. A word without any of them is not stemmed.
	if !m.mayStem(w) {
		return false
	}

	terms := Expand(w)

	for i, tok := range m.toks {
		for _, t := range terms {
			if slices.Contains(m.expanded[i], t) || termMatch(t, tok) {
				return true
			}
		}
	}

	return false
}

// mayStem is the cheap test before stemming.
func (m *Matcher) mayStem(w string) bool {
	f := fold(w)

	for _, r := range m.firsts {
		if strings.ContainsRune(f, r) {
			return true
		}
	}

	return false
}

// folder maps the letters the German stemmer rewrites.
var folder = strings.NewReplacer("ä", "a", "ö", "o", "ü", "u", "ß", "ss")

// fold is a lower-case word with the German letters the stemmer rewrites
// replaced.
func fold(w string) string { return folder.Replace(w) }

// termMatch is the substring test of a term against a query token: a
// token of a trigram or more is looked for anywhere in the term, a
// shorter one at its start.
func termMatch(term, tok string) bool {
	if ShortToken(tok) {
		return strings.HasPrefix(term, tok)
	}

	return strings.Contains(term, tok)
}

// ShortToken reports whether a query token is too short for the
// substring dictionary: it matches the terms it is a prefix of, not the
// ones it occurs in.
func ShortToken(tok string) bool { return utf8.RuneCountInString(tok) < 3 }
