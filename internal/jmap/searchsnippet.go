package jmap

import (
	"context"
	"encoding/json"
	"html"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"shardpost/internal/fts"
	"shardpost/internal/mimepart"
	"shardpost/internal/model"
)

// SearchSnippet/get (RFC 8621 5). A snippet shows where a message
// matched the text conditions of an Email/query filter: its subject and a
// piece of its body, with every matched word in <mark>…</mark> and the
// rest HTML-escaped. The full-text index says which messages match, not
// where, so the words are found again here — with the tokenizer and the
// stemmer the index was built with (fts.Matcher), so that what is marked
// is what matched. The subject comes from the index record, the body from
// the front of the message (Limits.SnippetReadBytes): a match beyond it
// gives no preview.

// snippetPreviewMax is the size limit RFC 8621 5 sets for the preview,
// in octets, with the marks.
const snippetPreviewMax = 255

// snippetContext is how much text, escaped, the preview shows before the
// first match.
const snippetContext = 60

// The marks around a matched word.
const (
	markOpen  = "<mark>"
	markClose = "</mark>"
)

// searchSnippetRequest is the argument shape of SearchSnippet/get.
type searchSnippetRequest struct {
	Filter    json.RawMessage `json:"filter"`
	EmailIDs  *[]string       `json:"emailIds"`
	AccountID string          `json:"accountId"`
}

// searchSnippet is one SearchSnippet object.
type searchSnippet struct {
	Subject *string `json:"subject"`
	Preview *string `json:"preview"`
	EmailID string  `json:"emailId"`
}

// searchSnippetResponse is what SearchSnippet/get answers.
type searchSnippetResponse struct {
	AccountID string          `json:"accountId"`
	List      []searchSnippet `json:"list"`
	NotFound  []string        `json:"notFound"`
}

// searchSnippetGet implements SearchSnippet/get.
func searchSnippetGet(_ context.Context, sess *session, args json.RawMessage) (any, *methodError) {
	var req searchSnippetRequest
	if merr := forAccount(sess, args, &req); merr != nil {
		return nil, merr
	}

	if req.EmailIDs == nil {
		return nil, &methodError{Type: errInvalidArguments, Arguments: []string{"emailIds"},
			Description: "SearchSnippet/get needs emailIds"}
	}

	if len(*req.EmailIDs) > sess.limits.MaxObjectsInGet {
		return nil, &methodError{Type: errRequestTooLarge, Description: "more emailIds than maxObjectsInGet"}
	}

	f, merr := readEmailFilter(req.Filter)
	if merr != nil {
		return nil, merr
	}

	_, loc, merr := emailState(sess)
	if merr != nil {
		return nil, merr
	}

	// The filter is checked as Email/query checks it, so that one this
	// server cannot evaluate is refused here as well; the index is not
	// asked, since no result is computed.
	q := &emailQuery{sess: sess, loc: loc, threads: map[string][]map[string]bool{}, noSearch: true}
	if _, merr := q.compile(f); merr != nil {
		return nil, merr
	}

	hits, err := loc.resolve(*req.EmailIDs)
	if err != nil {
		return nil, &methodError{Type: errServerFail, Description: err.Error()}
	}

	subject, body := snippetTerms(f)
	out := &searchSnippetResponse{AccountID: sess.accountID(), List: []searchSnippet{}, NotFound: []string{}}

	for _, id := range *req.EmailIDs {
		found, ok := hits[id]
		if !ok {
			out.NotFound = append(out.NotFound, id)

			continue
		}

		out.List = append(out.List, snippetOf(sess, id, &found.metas[0], subject, body))
	}

	return out, nil
}

// snippetTerms collects the words of the text conditions a filter has
// outside a NOT — what a match was found by — into the matchers of the
// subject (text, subject) and of the body (text, body).
func snippetTerms(f *emailFilter) (subject, body *fts.Matcher) {
	var subj, bod []string

	var collect func(f *emailFilter)

	collect = func(f *emailFilter) {
		if f == nil || f.Operator == opNOT {
			return
		}

		for i := range f.Conditions {
			collect(&f.Conditions[i])
		}

		for _, v := range []*string{f.Text, f.Subject} {
			if v != nil {
				subj = append(subj, fts.Tokenize(*v)...)
			}
		}

		for _, v := range []*string{f.Text, f.Body} {
			if v != nil {
				bod = append(bod, fts.Tokenize(*v)...)
			}
		}
	}

	collect(f)

	return fts.NewMatcher(subj), fts.NewMatcher(bod)
}

// snippetOf renders the snippet of one message.
func snippetOf(sess *session, id string, m *model.Meta, subject, body *fts.Matcher) searchSnippet {
	out := searchSnippet{EmailID: id}

	if !subject.Empty() && m.Envelope != nil {
		if s, ok := highlight(m.Envelope.Subject, subject, 0, -1); ok {
			out.Subject = &s
		}
	}

	if body.Empty() {
		return out
	}

	text, err := snippetBody(sess, m)
	if err != nil {
		sess.log.Warn("jmap: message body unreadable", "guid", m.GUID, "err", err)

		return out
	}

	if start, ok := firstMatch(text, body); ok {
		if p, ok := highlight(text, body, start, snippetPreviewMax); ok {
			out.Preview = &p
		}
	}

	return out
}

// snippetBody is the text of a message's text body, read from the front
// of the message up to the configured bound, with HTML turned into text
// and the white space collapsed.
func snippetBody(sess *session, m *model.Meta) (string, error) {
	raw, err := messagePrefix(sess, m, sess.limits.SnippetReadBytes)
	if err != nil {
		return "", err
	}

	text, _, _ := flatten(mimepart.ParseOrOpaque(raw))

	var b strings.Builder

	for _, p := range text {
		if p.Children != nil || !strings.HasPrefix(p.MediaType, "text/") {
			continue
		}

		decoded, _ := p.Decoded()
		s, _ := toUTF8(decoded, p.Params[partCharset])

		if p.MediaType == mediaTextHTML {
			s = htmlText(s)
		}

		b.WriteString(s)
		b.WriteByte(' ')
	}

	return collapseSpace(b.String()), nil
}

// messagePrefix reads at most limit bytes from the front of a message.
func messagePrefix(sess *session, m *model.Meta, limit int64) ([]byte, error) {
	if blobs, ok := sess.user.(model.Blobs); ok {
		return blobs.ReadBlob(m.GUID, m.Size, 0, min(limit, m.Size))
	}

	raw, err := sess.user.Read(m)
	if err != nil {
		return nil, err
	}

	if int64(len(raw)) > limit {
		raw = raw[:limit]
	}

	return raw, nil
}

// firstMatch is where the preview starts: a little before the first
// matched word, at the start of a word, so that the match has context.
func firstMatch(text string, m *fts.Matcher) (int, bool) {
	for _, w := range fts.Words(text) {
		if !m.Match(text[w.Start:w.End]) {
			continue
		}

		start, size := w.Start, 0

		for start > 0 {
			r, n := utf8.DecodeLastRuneInString(text[:start])
			if size += len(escapeHTML(string(r))); size > snippetContext {
				break
			}

			start -= n
		}

		// A word cut in two is not shown: the preview starts after it.
		for start > 0 && start < w.Start && isWordRune(text, start-1) {
			_, n := utf8.DecodeRuneInString(text[start:])
			start += n
		}

		return start, true
	}

	return 0, false
}

// isWordRune reports whether the rune that ends at byte i+1 of text is
// part of a word.
func isWordRune(text string, i int) bool {
	r, _ := utf8.DecodeLastRuneInString(text[:i+1])

	return unicode.IsLetter(r) || unicode.IsDigit(r)
}

// highlight renders text from start, with the words the matcher matched
// in marks and everything else escaped, cut at a word boundary so that
// the result is at most limit octets (no limit when negative). ok is
// false when no word was marked.
func highlight(text string, m *fts.Matcher, start, limit int) (string, bool) {
	var b strings.Builder

	marked := false
	pos := start

	add := func(piece string) bool {
		if limit >= 0 && b.Len()+len(piece) > limit {
			return false
		}

		b.WriteString(piece)

		return true
	}

	for _, w := range fts.Words(text[start:]) {
		ws, we := start+w.Start, start+w.End
		word := text[ws:we]

		piece := escapeHTML(text[pos:ws])
		if m.Match(word) {
			piece += markOpen + escapeHTML(word) + markClose
			marked = true
		} else {
			piece += escapeHTML(word)
		}

		if !add(piece) {
			return strings.TrimRight(b.String(), " "), marked
		}

		pos = we
	}

	add(escapeHTML(text[pos:]))

	return strings.TrimRight(b.String(), " "), marked
}

// htmlEscaper escapes the characters RFC 8621 5 names.
var htmlEscaper = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")

// escapeHTML escapes &, < and >.
func escapeHTML(s string) string { return htmlEscaper.Replace(s) }

// The parts of an HTML body that are not text.
var (
	htmlHidden = regexp.MustCompile(`(?is)<(script|style|head|title)\b.*?</(script|style|head|title)\s*>|<!--.*?-->`)
	htmlTag    = regexp.MustCompile(`(?s)<[^>]*>`)
)

// htmlText is the text of an HTML body: no markup, no scripts or styles,
// the entities decoded. It is for finding and showing words, not for
// rendering, so a tag is a space and nothing more.
func htmlText(s string) string {
	s = htmlHidden.ReplaceAllString(s, " ")
	s = htmlTag.ReplaceAllString(s, " ")

	return html.UnescapeString(s)
}

// collapseSpace turns every run of white space into one space.
func collapseSpace(s string) string {
	return strings.Join(strings.Fields(s), " ")
}
