package fts

import (
	"strings"
	"unicode"
)

// A word of more than maxToken runes has no term of its own. The index
// keeps it findable this way, and Spans relies on it:
//
//   - In every field the first maxToken runes of the word are a term
//     (window 0). A span that must start a word (MatchPrefix) has at
//     most maxToken runes, so it is a prefix of window 0.
//   - In a field with the substring dictionary the word also has the
//     windows of maxToken runes that start at every multiple of
//     windowStep, and the window that ends with the word — at most
//     maxWindows of them. A substring of at most coveredInfix runes at
//     rune p lies in the window that starts at i = windowStep*floor(p/
//     windowStep): p-i < windowStep, so it ends by i+windowStep-1+
//     coveredInfix = i+maxToken. Where that window would run past the
//     end of the word, i > n-maxToken and the last window [n-maxToken,
//     n) holds the substring. So an inner part of up to coveredInfix
//     runes is found in a window term by the dictionary.
//   - A word that would need more than maxWindows windows gets the
//     first maxWindows-1 and the last, and its field the marker
//     HugeWord: an inner part of up to coveredInfix runes may be in a
//     part of the word no window covers.
//   - Every such word gives its field the marker LongWord: an inner part
//     of more than coveredInfix runes is not in any window.
//
// A substring search adds the messages with the marker a span needs
// (Markers). A whole word (MatchWord) has at most maxToken runes and
// never equals a longer word, so it needs neither windows nor markers.
const (
	// LongWord marks a field with a word of more than maxToken runes.
	LongWord = "~~"
	// HugeWord marks a field with a word longer than its windows cover.
	HugeWord = "~+"

	windowStep   = 20
	coveredInfix = maxToken - windowStep + 1
	maxWindows   = 8
)

// No query token can be a marker: it has no letter or digit. A marker
// has two runes, too few for trigrams, so it never enters the substring
// dictionary, and no query token is a prefix of it.

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
// that a span may be part of: a whole word by its key and a word's
// start by a scan of the keys with that prefix in every field, an inner
// part only through the substring dictionary, which a field has when
// Substring says so and a token has when it is long enough for
// trigrams.
func SpanIndexed(field string, sp Span) bool {
	return sp.Match != MatchInfix || (Substring(field) && len(Trigrams(sp.Token)) > 0)
}

// SpanTerm reports whether the token part of an index term (without
// "<field>:") can hold a span: the word itself, a word it starts, a
// word it is part of. Markers are not terms in this sense; see Markers.
func SpanTerm(sp Span, term string) bool {
	switch {
	case term == LongWord || term == HugeWord:
		return false
	case sp.Match == MatchWord:
		return term == sp.Token
	case sp.Match == MatchPrefix:
		return strings.HasPrefix(term, sp.Token)
	}

	return strings.Contains(term, sp.Token)
}

// Markers returns the marker tokens whose messages a span adds to its
// candidates: an inner part of a long word that no window may hold.
func Markers(sp Span) []string {
	if sp.Match != MatchInfix {
		return nil
	}

	if len([]rune(sp.Token)) > coveredInfix {
		return []string{LongWord}
	}

	return []string{HugeWord}
}

func isWordRune(r rune) bool { return unicode.IsLetter(r) || unicode.IsDigit(r) }

// longWords calls fn with every word of text of more than maxToken
// runes, lower-cased.
func longWords(text string, fn func(w []rune)) {
	var w []rune

	flush := func() {
		if len(w) > maxToken {
			fn([]rune(strings.ToLower(string(w))))
		}

		w = w[:0]
	}

	for _, r := range text {
		if isWordRune(r) {
			w = append(w, r)

			continue
		}

		flush()
	}

	flush()
}

// longWordTerms returns the window terms and markers of a long word in
// a field (tokens without "<field>:"), as described above.
func longWordTerms(field string, w []rune) []string {
	out := []string{string(w[:maxToken])}
	if !Substring(field) {
		return out
	}

	out = append(out, LongWord)
	n := len(w)

	var starts []int
	for i := windowStep; i+maxToken < n; i += windowStep {
		starts = append(starts, i)
	}

	if len(starts)+2 > maxWindows {
		starts = starts[:maxWindows-2]

		out = append(out, HugeWord)
	}

	for _, i := range starts {
		out = append(out, string(w[i:i+maxToken]))
	}

	return append(out, string(w[n-maxToken:]))
}
