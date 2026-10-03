package memstore

import (
	"strings"

	"shardpost/internal/fts"
)

// FTSOptions are the settings of the full-text index of B14h that the
// store emulates, so that tests over memstore see what a cephstore
// generation with them answers. memstore has no generations: the
// options apply to everything, as configured.
type FTSOptions struct {
	// DictIDMinRunes is the identifier filter of fts.dict_filter "ids"
	// (fts.dict_id_min_runes; 0: none): an inner part of a body term
	// that is an identifier (fts.DictIdentifier) is not found, its start
	// is.
	DictIDMinRunes int
	// AttachedHeaders is fts.attached_headers ("" or "all", "nosig"):
	// the text preparation of the terms. The IMAP check of the
	// candidates follows the process setting (fts.SetAttachedHeaders).
	AttachedHeaders string
}

// SetFTSOptions sets the emulated full-text settings; call it before
// the store is used.
func (s *Store) SetFTSOptions(o FTSOptions) { s.fts = o }

// ftsTerms are the index terms of a message in the store's text
// preparation.
func (u *User) ftsTerms(raw []byte) []string {
	return fts.TermsWith(raw, fts.TextOptions{AttachedHeaders: u.store.fts.AttachedHeaders})
}

// infixMatch is the in-word substring test of a query token against the
// token of an index term ("<field>:<token>" split): Contains, but for a
// body identifier of a store with the identifier filter only HasPrefix
// — the cephstore dictionary has its start entry, not its trigrams.
func (u *User) infixMatch(field, term, tok string) bool {
	if fts.DictIdentifier(field+":"+term, u.store.fts.DictIDMinRunes) {
		return strings.HasPrefix(term, tok)
	}

	return strings.Contains(term, tok)
}
