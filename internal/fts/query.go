package fts

import (
	"strings"
	"unicode"
)

// LongWord is the token of the term "<field>:~~" that marks a message
// with a word of more than maxToken runes in the field. Such a word has
// no term of its own, so a substring of it cannot be looked up; a
// substring search takes the marked messages as candidates instead. No
// query token can be "~~" (it has no letter or digit), and it is too
// short to have trigrams, so it never enters the substring dictionary.
const LongWord = "~~"

// How a query word must occur in the text of a message that contains
// the whole query as a substring.
const (
	// MatchWord: the word is bounded by separators in the query on both
	// sides, so the text has it as a whole word.
	MatchWord = iota
	// MatchPrefix: bounded on the left only; it starts a word of the
	// text.
	MatchPrefix
	// MatchInfix: not bounded on the left; it may be any part of a word.
	MatchInfix
)

// Span is a word of a substring query and how it must occur.
type Span struct {
	Token string // lower-cased, minToken to maxToken runes
	Match int    // MatchWord, MatchPrefix or MatchInfix
}

// Spans returns the words of a query for a candidate search that must
// not lose a message containing the query as a substring (RFC 3501 and
// RFC 9051 SEARCH): the first word of the query may be the end of a
// longer word of the text, the last may be the start of one, the words
// between are whole words. Words shorter than minToken or longer than
// maxToken runes are left out; they cannot narrow the search.
func Spans(query string) []Span {
	var out []Span

	seen := map[Span]bool{}
	q := strings.ToLower(query)
	start := -1

	emit := func(i, j int) {
		tok := q[i:j]
		if n := len([]rune(tok)); n < minToken || n > maxToken {
			return
		}

		s := Span{Token: tok, Match: MatchWord}

		switch {
		case i == 0:
			s.Match = MatchInfix
		case j == len(q):
			s.Match = MatchPrefix
		}

		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}

	for i, r := range q {
		if isWordRune(r) {
			if start < 0 {
				start = i
			}

			continue
		}

		if start >= 0 {
			emit(start, i)
			start = -1
		}
	}

	if start >= 0 {
		emit(start, len(q))
	}

	return out
}

// SpanIndexed reports whether the index can list every term of field
// that a span may be part of: a whole word or a word's start always
// (by key or key prefix), an inner part only through the substring
// dictionary, which a field has when Substring says so and a token has
// when it is long enough for trigrams.
func SpanIndexed(field string, sp Span) bool {
	return sp.Match != MatchInfix || (Substring(field) && len(Trigrams(sp.Token)) > 0)
}

// SpanTerm reports whether the token part of an index term (without
// "<field>:") can hold a span: the word itself, a word it starts, a
// word it is part of, or the LongWord marker, which stands for a word
// that has no term.
func SpanTerm(sp Span, term string) bool {
	switch {
	case term == LongWord:
		return sp.Match != MatchWord
	case sp.Match == MatchWord:
		return term == sp.Token
	case sp.Match == MatchPrefix:
		return strings.HasPrefix(term, sp.Token)
	}

	return strings.Contains(term, sp.Token)
}

func isWordRune(r rune) bool { return unicode.IsLetter(r) || unicode.IsDigit(r) }

// hasLongWord reports whether text has a word of more than maxToken
// runes.
func hasLongWord(text string) bool {
	n := 0

	for _, r := range text {
		if !isWordRune(r) {
			n = 0

			continue
		}

		if n++; n > maxToken {
			return true
		}
	}

	return false
}
