package fts

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// Identifiers in the substring dictionary (temp/13 §4.1, stage B14h). A
// term of the body with at least one letter, at least one digit and at
// least n runes is an identifier: a tracking token, a hash, a base64
// run, a signature. Such terms are new in every message and cost one
// dictionary key per distinct trigram, so the dictionary grows with
// them linearly. A dictionary generation with the identifier filter
// (fts.dict_filter "ids") gives such a term one start entry instead of
// its trigram keys, so that it is found as a whole word and by its
// start but no longer as an inner part. The same rule serves the
// projection of "shardpostctl fts stats" on test accounts, the writers,
// the removals, the conversion and the counters alike: it depends on
// the term and n alone.

// Bounds of the identifier length n.
const (
	IdentMinRunesLow  = 13
	IdentMinRunesHigh = maxToken
)

// IsIdentifier reports whether a token is an identifier of at least
// minRunes runes: letters and digits as the tokenizer sees them
// (unicode.IsLetter, unicode.IsDigit), runes not bytes. minRunes 0 never
// matches.
func IsIdentifier(tok string, minRunes int) bool {
	if minRunes <= 0 || utf8.RuneCountInString(tok) < minRunes {
		return false
	}

	var letter, digit bool

	for _, r := range tok {
		switch {
		case unicode.IsLetter(r):
			letter = true
		case unicode.IsDigit(r):
			digit = true
		}

		if letter && digit {
			return true
		}
	}

	return false
}

// DictIdentifier reports whether a term ("<field>:<token>") gets a start
// entry instead of its trigram keys in a dictionary with the identifier
// filter of minRunes runes: a body term whose token is an identifier.
// The fields s, f and t are never filtered (their IMAP keys keep the
// substring rule of RFC 9051), h has no dictionary.
func DictIdentifier(term string, minRunes int) bool {
	field, tok, _ := strings.Cut(term, ":")

	return field == FieldBody && IsIdentifier(tok, minRunes)
}

// Tri0 is the first trigram of a token: its first three runes ("" for a
// shorter token). A start entry is routed by it, and a search reads the
// start entries of a query word in the same object as that trigram's
// range.
func Tri0(tok string) string {
	i, n := 0, 0
	for i < len(tok) && n < 3 {
		_, size := utf8.DecodeRuneInString(tok[i:])
		i += size
		n++
	}

	if n < 3 {
		return ""
	}

	return tok[:i]
}

// DictKeys is the number of substring dictionary keys of a term
// ("<field>:<token>"): one per distinct trigram of its token, none in a
// field without substring matching. With identMinRunes > 0 a body term
// that is an identifier of at least that many runes counts one key (its
// start entry, temp/13 §4.2) instead; 0 is the dictionary without the
// filter.
func DictKeys(term string, identMinRunes int) int {
	if !Substring(fieldOf(term)) {
		return 0
	}

	if DictIdentifier(term, identMinRunes) {
		return 1
	}

	_, tok, _ := strings.Cut(term, ":")

	return len(Trigrams(tok))
}

// fieldOf is the field of a term.
func fieldOf(term string) string {
	field, _, _ := strings.Cut(term, ":")

	return field
}
