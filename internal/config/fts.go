package config

import (
	"fmt"

	"shardpost/internal/charset"
)

// FTS configures how message text is prepared for the full-text index.
// The same preparation answers IMAP SEARCH without the index and the
// cached preview, so a change applies to new messages at once and to
// existing ones after an fts_rebuild.
type FTS struct {
	// FallbackCharset is the charset text is read in that is not valid
	// UTF-8 and has no charset label, is labelled US-ASCII or names a
	// charset that is not known.
	FallbackCharset string `mapstructure:"fallback_charset"`
	// HTML is what is made of a text/html part: "text" indexes its
	// visible text, "raw" the markup as it is, "skip" leaves it out.
	HTML string `mapstructure:"html" validate:"oneof=text raw skip"`
	// IMAPSearch is how IMAP SEARCH matches text: "substring" as RFC 9051
	// specifies, the index narrowing the candidates and each candidate
	// checked against the message; "word" lets the index answer SUBJECT,
	// BODY, TEXT and HEADER Subject alone, by word, stem and in-word
	// substring, without reading messages.
	IMAPSearch string `mapstructure:"imap_search" validate:"oneof=substring word"`
}

// IMAPWordSearch reports whether IMAP SEARCH uses word semantics.
func (f FTS) IMAPWordSearch() bool { return f.IMAPSearch == "word" }

// Validate refuses a fallback charset that is unknown or cannot read
// 8-bit text.
func (f FTS) Validate() error {
	if _, err := charset.Fallback(f.FallbackCharset); err != nil {
		return fmt.Errorf("fts.fallback_charset: %w", err)
	}

	return nil
}
