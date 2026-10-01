// Package fts turns RFC 5322 messages into index terms. It is the
// tokenizer half of a "flatcurve on RADOS": Dovecot's flatcurve plugin keeps
// a Xapian database per mailbox on a file system; here the equivalent index
// is an inverted index kept in RADOS omap (see cephstore/fts.go). This
// package is pure and has no Ceph dependency.
package fts

import (
	"bufio"
	"bytes"
	"sort"
	"strings"
	"unicode"

	"github.com/emersion/go-message/textproto"

	"shardpost/internal/charset"
)

// Fields of the index. A term is stored as "<field>:<token>".
const (
	FieldSubject = "s"
	FieldFrom    = "f"
	FieldTo      = "t" // To, Cc, Bcc
	FieldHeader  = "h" // every header value
	FieldBody    = "b" // decoded text/* parts
)

// HeaderSubstringFields are the header fields whose terms the stores list
// for substring matching; a substring query on FieldHeader is answered
// from them.
var HeaderSubstringFields = []string{FieldSubject, FieldFrom, FieldTo}

// Substring reports whether a query token matches the terms of field by
// substring. All fields do except FieldHeader: most of its vocabulary is
// message ids, timestamps and other tokens that occur once, so a
// substring index over it grows with the number of messages instead of
// with the words in them. A token queried on FieldHeader matches a term
// exactly, by stem or as a prefix, and by substring where the term also
// belongs to one of HeaderSubstringFields.
func Substring(field string) bool { return field != FieldHeader }

const (
	minToken = 2
	maxToken = 40
	// MaxBodyBytes bounds the indexed body text per message.
	MaxBodyBytes = 4 << 20
)

// Term is a field-qualified token, e.g. "s:invoice".
type Term = string

// Tokenize splits free text into lowercase tokens (Unicode letters and
// digits, everything else separates). Duplicates are removed.
func Tokenize(text string) []string {
	seen := map[string]struct{}{}

	var out []string

	emit := func(t string) {
		t = strings.ToLower(t)
		if n := len([]rune(t)); n < minToken || n > maxToken {
			return
		}

		if _, ok := seen[t]; ok {
			return
		}

		seen[t] = struct{}{}
		out = append(out, t)
	}
	start := -1

	for i, r := range text {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			if start < 0 {
				start = i
			}

			continue
		}

		if start >= 0 {
			emit(text[start:i])
			start = -1
		}
	}

	if start >= 0 {
		emit(text[start:])
	}

	return out
}

// Terms extracts all index terms of a raw message: every token and its
// stems, qualified by field, and the window terms and markers of words
// too long to be a term (see LongWord). Header values are taken as
// HeaderValue returns them, every value of a field; the body text is
// what BodyText returns.
func Terms(raw []byte) []Term {
	terms, _ := IndexText(raw)

	return terms
}

// IndexText returns the terms of a message (Terms) and its HeaderText,
// parsing and decoding the header once for both.
func IndexText(raw []byte) ([]Term, string) {
	set := map[Term]struct{}{}
	add := func(field, text string) {
		for _, tok := range Tokenize(text) {
			for _, t := range Expand(tok) {
				set[field+":"+t] = struct{}{}
			}
		}

		longWords(text, func(w []rune) {
			for _, t := range longWordTerms(field, w) {
				set[field+":"+t] = struct{}{}
			}
		})
	}

	var htext strings.Builder

	hdr, err := textproto.ReadHeader(bufio.NewReader(bytes.NewReader(raw)))
	if err == nil {
		for _, kv := range []struct{ key, field string }{
			{"Subject", FieldSubject}, {"From", FieldFrom}, {"Sender", FieldFrom}, {"Reply-To", FieldFrom},
			{"To", FieldTo}, {"Cc", FieldTo}, {"Bcc", FieldTo},
		} {
			for _, v := range hdr.Values(kv.key) {
				add(kv.field, HeaderValue(v))
			}
		}

		// names and values: IMAP TEXT matches the header as a whole
		fields := hdr.Fields()
		for fields.Next() {
			v := HeaderValue(fields.Value())

			add(FieldHeader, fields.Key())
			add(FieldHeader, v)
			headerLine(&htext, fields.Key(), v)
		}
	}

	add(FieldBody, BodyText(raw))

	out := make([]Term, 0, len(set))
	for t := range set {
		out = append(out, t)
	}

	sort.Strings(out)

	return out, htext.String()
}

// MaxHeaderTextBytes bounds the HeaderText the store keeps per message;
// a longer one is not kept, and its header is read from the message.
const MaxHeaderTextBytes = 64 << 10

// HeaderText returns the header of a raw message as IMAP SEARCH matches
// it: lower-cased "name: value" lines, each preceded by CRLF, with the
// value as HeaderValue returns it (line breaks that decoding put into a
// value become spaces). The field FieldHeader of the index has the
// words of this text, and the stores keep it per message so that a
// search can check headers without reading messages.
func HeaderText(raw []byte) string {
	hdr, err := textproto.ReadHeader(bufio.NewReader(bytes.NewReader(raw)))
	if err != nil {
		return ""
	}

	var b strings.Builder

	fields := hdr.Fields()
	for fields.Next() {
		headerLine(&b, fields.Key(), HeaderValue(fields.Value()))
	}

	return b.String()
}

// headerLine appends one line of HeaderText.
func headerLine(b *strings.Builder, name, value string) {
	b.WriteString("\r\n")
	b.WriteString(strings.ToLower(name))
	b.WriteString(": ")
	b.WriteString(strings.ToLower(unbreak.Replace(value)))
}

// unbreak turns line breaks that decoding put into a value into spaces,
// so that a value stays on its line.
var unbreak = strings.NewReplacer("\r", " ", "\n", " ")

// unfoldHeader removes the line breaks of a folded header value
// (RFC 5322 2.2.3); the white space after them stays.
var unfoldHeader = strings.NewReplacer("\r\n", "", "\r", "", "\n", "")

// HeaderValue is a header field value as the index and the direct
// matching of IMAP SEARCH see it: unfolded and decoded to UTF-8 by
// charset.DecodeHeader (encoded words in any charset of the WHATWG
// list, raw 8-bit text in the fallback charset).
func HeaderValue(v string) string {
	return charset.DecodeHeader(unfoldHeader.Replace(v))
}
