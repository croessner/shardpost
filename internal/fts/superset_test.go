package fts

import (
	"math/rand"
	"os"
	"strconv"
	"strings"
	"testing"
)

// fieldTerms is what Terms adds for one field of text (without
// "<field>:").
func fieldTerms(field, text string) map[string]bool {
	set := map[string]bool{}

	for _, tok := range Tokenize(text) {
		for _, t := range Expand(tok) {
			set[t] = true
		}
	}

	longWords(text, func(w []rune) {
		for _, t := range longWordTerms(field, w) {
			set[t] = true
		}
	})

	return set
}

// covered reports whether the candidate search keeps a message whose
// field has terms, for the span: a term SpanTerm accepts, or a marker
// the span needs. A span the index cannot look up never narrows.
func covered(field string, terms map[string]bool, sp Span) bool {
	if !SpanIndexed(field, sp) {
		return true
	}

	for t := range terms {
		if SpanTerm(sp, t) {
			return true
		}
	}

	for _, m := range Markers(sp) {
		if terms[m] {
			return true
		}
	}

	return false
}

// The runes of random texts: letters with case and folding quirks,
// digits, letters of other scripts, separators.
var (
	wordRunes = []rune("abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789äöüÄÖÜßẞİıſKΣσςǅǆᲐაéÉ日本語中文𝒜")
	anyRunes  = []rune(string(wordRunes) + "😀 \t-_.:,;/()\r\n")
	seps      = []string{" ", "-", ", ", "\r\n", "_", "/", "ab́c ", "..."}
)

// randRunes returns n runes of set.
func randRunes(r *rand.Rand, set []rune, n int) string {
	w := make([]rune, n)
	for i := range w {
		w[i] = set[r.Intn(len(set))]
	}

	return string(w)
}

// randLength picks a word length around the bounds that matter: short
// words, maxToken, long words, the HugeWord bound.
func randLength(r *rand.Rand) int {
	switch r.Intn(6) {
	case 0:
		return 1 + r.Intn(5)
	case 1:
		return 15 + r.Intn(30)
	case 2:
		return 41 + r.Intn(60)
	case 3:
		return 150 + r.Intn(120)
	}

	return 2 + r.Intn(12)
}

// randText returns up to six words, some of random runes, joined by
// separators.
func randText(r *rand.Rand) string {
	var b strings.Builder

	for i := 0; i < 1+r.Intn(6); i++ {
		if i > 0 {
			b.WriteString(seps[r.Intn(len(seps))])
		}

		set := wordRunes
		if r.Intn(4) == 0 {
			set = anyRunes
		}

		b.WriteString(randRunes(r, set, randLength(r)))
	}

	return b.String()
}

// randQuery returns a substring of text, rune aligned, often of the
// lengths at the bounds of the windows.
func randQuery(r *rand.Rand, rs []rune) string {
	i := r.Intn(len(rs))
	l := []int{21, 22, 40, 41, 2 + r.Intn(200), 1 + r.Intn(60), 1 + r.Intn(60), 1 + r.Intn(60)}[r.Intn(8)]

	return string(rs[i:min(i+l, len(rs))])
}

// TestSpansSuperset checks the superset guarantee of the candidate
// search on random texts and substrings of them: every span of a query
// that occurs in a field's text is held by a term of that field or by a
// marker it needs. FTS_SUPERSET_CASES sets the number of cases (20000
// by default; the review ran 200000).
func TestSpansSuperset(t *testing.T) {
	cases := 20000
	if v, err := strconv.Atoi(os.Getenv("FTS_SUPERSET_CASES")); err == nil && v > 0 {
		cases = v
	}

	r := rand.New(rand.NewSource(1))

	for iter := 0; iter < cases && !t.Failed(); iter++ {
		text := randText(r)

		rs := []rune(text)
		if len(rs) < 2 {
			continue
		}

		query := randQuery(r, rs)

		// what verification compares; case folding that changes the
		// length of a text (İ) is outside what the index promises
		if !strings.Contains(strings.ToLower(text), strings.ToLower(query)) {
			continue
		}

		for _, field := range []string{FieldBody, FieldHeader} {
			checkCovered(t, field, text, query)
		}
	}
}

// coveredFiltered is covered for a dictionary with the identifier
// filter of n runes (B14h): in the body an inner part matches an
// identifier term only at its start.
func coveredFiltered(field string, terms map[string]bool, sp Span, n int) bool {
	if !SpanIndexed(field, sp) {
		return true
	}

	for t := range terms {
		if !SpanTerm(sp, t) {
			continue
		}

		if sp.Match != MatchInfix || !DictIdentifier(field+":"+t, n) || strings.HasPrefix(t, sp.Token) {
			return true
		}
	}

	for _, m := range Markers(sp) {
		if terms[m] {
			return true
		}
	}

	return false
}

// TestSpansSupersetFiltered checks the guarantee of the identifier
// filter (temp/13 §5.1) on random texts: a query that begins where a
// word of the text begins is found also when the body's identifiers
// have their start entries only — the start of a word is the start of
// its term or of its first window. The other fields are not filtered.
func TestSpansSupersetFiltered(t *testing.T) {
	r := rand.New(rand.NewSource(2))

	for iter := 0; iter < 20000 && !t.Failed(); iter++ {
		text := randText(r)
		words := Words(text)

		if len(words) == 0 {
			continue
		}

		w := words[r.Intn(len(words))]
		rs := []rune(text[w.Start:])
		query := string(rs[:min(len(rs), 2+r.Intn(60))])

		if !strings.Contains(strings.ToLower(text), strings.ToLower(query)) {
			continue
		}

		for _, n := range []int{IdentMinRunesLow, 25, IdentMinRunesHigh} {
			terms := fieldTerms(FieldBody, text)

			for _, sp := range Spans(query) {
				if !coveredFiltered(FieldBody, terms, sp, n) {
					t.Errorf("n=%d: text %q query %q span %+v not covered", n, text, query, sp)
				}
			}
		}
	}
}

// checkCovered checks every span of query against the terms of text.
func checkCovered(t *testing.T, field, text, query string) {
	t.Helper()

	terms := fieldTerms(field, text)

	for _, sp := range Spans(query) {
		if !covered(field, terms, sp) {
			t.Errorf("field %s: text %q query %q span %+v not covered", field, text, query, sp)
		}
	}
}
