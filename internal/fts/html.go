package fts

import (
	"html"
	"strings"
)

// HTMLText reduces an HTML document to its visible text: tags,
// comments, declarations and the content of script, style, template
// and title elements are dropped, character references are decoded,
// and block elements and line breaks become a line break, so that the
// words of two paragraphs do not run into each other. Inline elements
// (b, span, a, …) leave nothing behind, so "<b>Rech</b>nung" stays one
// word. It is a tolerant scanner, not a parser: unclosed tags and
// quotes end the text instead of failing.
func HTMLText(s string) string {
	var out strings.Builder

	for i := 0; i < len(s); {
		if !markupAt(s, i) {
			j := i + 1
			for j < len(s) && !markupAt(s, j) {
				j++
			}

			out.WriteString(html.UnescapeString(s[i:j]))
			i = j

			continue
		}

		next, block := skipMarkup(s, i)
		if block {
			out.WriteByte('\n')
		}

		i = next
	}

	return out.String()
}

// markupAt reports whether a tag, comment or declaration starts at i.
func markupAt(s string, i int) bool {
	return s[i] == '<' && i+1 < len(s) && tagStart(s[i+1])
}

// skipMarkup returns the index after the markup that starts at i — with
// the content of a raw text element such as script — and whether it
// breaks a line.
func skipMarkup(s string, i int) (int, bool) {
	switch {
	case strings.HasPrefix(s[i:], "<!-->"): // an empty comment, closed
		return i + len("<!-->"), false
	case strings.HasPrefix(s[i:], "<!--->"):
		return i + len("<!--->"), false
	case strings.HasPrefix(s[i:], "<!--"):
		return skipPast(s, i+4, "-->"), false
	case s[i+1] == '!' || s[i+1] == '?':
		// <!DOCTYPE …>, <![CDATA[…]]> and <?xml …?> carry no text a
		// reader sees.
		return skipPast(s, i+2, ">"), false
	}

	end := tagEnd(s, i+1)
	name, closing := tagName(s[i+1 : end])
	next := min(end+1, len(s))

	// A "/" before ">" does not close script or style (HTML ignores it
	// on those elements): their content ends only at the end tag.
	if !closing && rawTextElements[name] {
		next = skipRawText(s, next, name)
	}

	return next, blockElements[name]
}

// tagStart reports whether c may follow "<" in a tag, comment or
// declaration; anything else ("a < b") is text.
func tagStart(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c == '/' || c == '!' || c == '?'
}

// skipPast returns the index after the next occurrence of end at or
// after i, or len(s) if there is none.
func skipPast(s string, i int, end string) int {
	if j := strings.Index(s[i:], end); j >= 0 {
		return i + j + len(end)
	}

	return len(s)
}

// tagEnd returns the index of the ">" that closes the tag starting at
// i, skipping quoted attribute values; len(s) if the tag is not closed.
func tagEnd(s string, i int) int {
	var quote byte

	for ; i < len(s); i++ {
		c := s[i]

		switch {
		case quote != 0:
			if c == quote {
				quote = 0
			}
		case c == '"' || c == '\'':
			quote = c
		case c == '>':
			return i
		}
	}

	return len(s)
}

// tagName returns the lower-case element name of the inside of a tag
// ("/P class=x" → "p", true).
func tagName(t string) (string, bool) {
	closing := strings.HasPrefix(t, "/")
	t = strings.TrimPrefix(t, "/")

	n := 0
	for n < len(t) && (t[n] >= 'a' && t[n] <= 'z' || t[n] >= 'A' && t[n] <= 'Z' || t[n] >= '0' && t[n] <= '9' || t[n] == '-' || t[n] == ':') {
		n++
	}

	return strings.ToLower(t[:n]), closing
}

// skipRawText returns the index after the end tag of a raw text element
// (script, style, …) whose content starts at i, compared without regard
// to case; len(s) if it is not closed. The name has to end there:
// "</scripts>" does not end a script.
func skipRawText(s string, i int, name string) int {
	end := "</" + name

	for i < len(s) {
		j := strings.IndexByte(s[i:], '<')
		if j < 0 {
			break
		}

		i += j
		if k := i + len(end); k <= len(s) && strings.EqualFold(s[i:k], end) && (k == len(s) || endsTagName(s[k])) {
			return min(len(s), tagEnd(s, k)+1)
		}

		i++
	}

	return len(s)
}

// endsTagName reports whether c ends a tag name: white space, "/" or
// ">".
func endsTagName(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\f' || c == '/' || c == '>'
}

// rawTextElements are the elements whose content is not text a reader
// sees.
var rawTextElements = map[string]bool{
	"script": true, "style": true, "template": true, "title": true,
	"noembed": true, "noframes": true, "xml": true,
}

// blockElements are the elements that break a line, so that words on
// either side stay apart.
var blockElements = map[string]bool{
	"address": true, "article": true, "aside": true, "blockquote": true, "body": true,
	"br": true, "caption": true, "center": true, "dd": true, "details": true,
	"dialog": true, "div": true, "dl": true, "dt": true, "fieldset": true,
	"figcaption": true, "figure": true, "footer": true, "form": true,
	"h1": true, "h2": true, "h3": true, "h4": true, "h5": true, "h6": true,
	"head": true, "header": true, "hr": true, "html": true, "img": true,
	"li": true, "main": true, "nav": true, "ol": true, "option": true,
	"p": true, "pre": true, "section": true, "summary": true, "table": true,
	"tbody": true, "td": true, "tfoot": true, "th": true, "thead": true,
	"tr": true, "ul": true, "button": true, "input": true, "select": true,
	"textarea": true,
}
