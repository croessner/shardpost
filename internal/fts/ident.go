package fts

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// Identifiers in the substring dictionary (temp/13 §4.1). A term of the
// body with at least one letter, at least one digit and at least n runes
// is an identifier: a tracking token, a hash, a base64 run, a signature.
// Such terms are new in every message and cost one dictionary key per
// distinct trigram, so the dictionary grows with them linearly. B14h
// may give them one key instead of their trigrams; until then the rule
// only serves the projection of "shardpostctl fts stats" on test
// accounts, and it is the same function both use.

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

// DictKeys is the number of substring dictionary keys of a term
// ("<field>:<token>"): one per distinct trigram of its token, none in a
// field without substring matching. With identMinRunes > 0 a body term
// that is an identifier of at least that many runes counts one key (its
// start entry, temp/13 §4.2) instead; 0 is the dictionary as it is.
func DictKeys(term string, identMinRunes int) int {
	field, tok, _ := strings.Cut(term, ":")
	if !Substring(field) {
		return 0
	}

	if field == FieldBody && IsIdentifier(tok, identMinRunes) {
		return 1
	}

	return len(Trigrams(tok))
}
