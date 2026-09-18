package fts

import (
	"github.com/blevesearch/snowballstem"
	"github.com/blevesearch/snowballstem/english"
	"github.com/blevesearch/snowballstem/german"
)

// Stems returns the Snowball stems of a token for the configured
// languages, without the token itself and without duplicates. Two
// languages are stemmed unconditionally: per-message language detection
// costs more than the handful of extra terms it would save.
func Stems(tok string) []string {
	var out []string

	seen := map[string]bool{tok: true}
	for _, stem := range []func(*snowballstem.Env) bool{english.Stem, german.Stem} {
		env := snowballstem.NewEnv(tok)
		stem(env)

		s := env.Current()
		if len(s) < minToken || seen[s] {
			continue
		}

		seen[s] = true
		out = append(out, s)
	}

	return out
}

// Expand returns the token followed by its stems.
func Expand(tok string) []string {
	return append([]string{tok}, Stems(tok)...)
}

// Trigrams returns the distinct rune trigrams of a token. Tokens shorter
// than three runes have none.
func Trigrams(tok string) []string {
	r := []rune(tok)
	if len(r) < 3 {
		return nil
	}

	seen := map[string]bool{}

	var out []string

	for i := 0; i+3 <= len(r); i++ {
		t := string(r[i : i+3])
		if !seen[t] {
			seen[t] = true
			out = append(out, t)
		}
	}

	return out
}
