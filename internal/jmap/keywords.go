package jmap

import (
	"strings"
)

// JMAP keywords and IMAP flags are the same thing under two spellings
// (RFC 8621 4.1.1). The four system flags a client can set have a
// keyword each; \Recent is a session artefact and \Deleted belongs to
// IMAP's delete-then-expunge model, which JMAP does not have, so neither
// is exposed. Every other flag is an IMAP keyword already, and it keeps
// its name — lower-cased, because both protocols compare keywords without
// regard to case and JMAP returns them in lower case.
var systemKeywords = []struct{ flag, keyword string }{
	{`\Seen`, "$seen"},
	{imapFlagged, "$flagged"},
	{`\Answered`, "$answered"},
	{`\Draft`, "$draft"},
}

// imapFlagged is the one IMAP atom that is both a system flag and a
// special-use attribute (RFC 6154), with a JMAP keyword in the one role
// and a mailbox role in the other.
const imapFlagged = `\Flagged`

// keywordOfSystemFlag and systemFlagOfKeyword are the table read both
// ways.
var keywordOfSystemFlag, systemFlagOfKeyword = func() (map[string]string, map[string]string) {
	k, f := map[string]string{}, map[string]string{}
	for _, e := range systemKeywords {
		k[strings.ToLower(e.flag)] = e.keyword
		f[e.keyword] = e.flag
	}

	return k, f
}()

// keywordOf is the JMAP keyword of an IMAP flag, and whether the flag has
// one.
func keywordOf(flag string) (string, bool) {
	lower := strings.ToLower(flag)

	if strings.HasPrefix(lower, `\`) {
		k, ok := keywordOfSystemFlag[lower]

		return k, ok
	}

	if !validKeyword(lower) {
		return "", false
	}

	return lower, true
}

// flagOf is the IMAP flag of a JMAP keyword.
func flagOf(keyword string) string {
	lower := strings.ToLower(keyword)
	if f, ok := systemFlagOfKeyword[lower]; ok {
		return f
	}

	return lower
}

// keywordSpecials are the characters RFC 8621 4.1.1 keeps out of a
// keyword; they are the IMAP atom-specials, so every IMAP keyword is a
// JMAP keyword and the other way round.
const keywordSpecials = `(){]%*"\`

// validKeyword reports whether s can be a JMAP keyword: 1 to 255
// printable ASCII characters without the specials.
func validKeyword(s string) bool {
	if s == "" || len(s) > 255 {
		return false
	}

	for i := 0; i < len(s); i++ {
		c := s[i]
		if c < 0x21 || c > 0x7e || strings.IndexByte(keywordSpecials, c) >= 0 {
			return false
		}
	}

	return true
}

// keywordsOf is the keyword set of a message from the flags of all its
// placements. A message copied by IMAP can carry different flags in its
// copies, and JMAP has one keyword set per message: a keyword counts when
// any copy has it, so a message read in one folder is read, and one
// flagged in one folder is flagged. A keyword write through JMAP sets all
// copies alike, which is how the two come back together.
func keywordsOf(flagSets ...[]string) map[string]bool {
	out := map[string]bool{}

	for _, flags := range flagSets {
		for _, f := range flags {
			if k, ok := keywordOf(f); ok {
				out[k] = true
			}
		}
	}

	return out
}
