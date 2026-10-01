// Package fts turns RFC 5322 messages into index terms. It is the
// tokenizer half of a "flatcurve on RADOS": Dovecot's flatcurve plugin keeps
// a Xapian database per mailbox on a file system; here the equivalent index
// is an inverted index kept in RADOS omap (see cephstore/fts.go). This
// package is pure and has no Ceph dependency.
package fts

import (
	"bufio"
	"bytes"
	"io"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/emersion/go-message"
	"github.com/emersion/go-message/mail"
	"github.com/emersion/go-message/textproto"
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

	for _, w := range Words(text) {
		t, ok := token(text[w.Start:w.End])
		if !ok {
			continue
		}

		if _, ok := seen[t]; ok {
			continue
		}

		seen[t] = struct{}{}
		out = append(out, t)
	}

	return out
}

// Span is the byte range of one word of a text.
type Span struct {
	Start, End int
}

// Words returns the words of a text in order: the runs of Unicode letters
// and digits Tokenize makes its tokens of, including the ones too short
// or too long to be a token.
func Words(text string) []Span {
	var out []Span

	start := -1

	for i, r := range text {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			if start < 0 {
				start = i
			}

			continue
		}

		if start >= 0 {
			out = append(out, Span{start, i})
			start = -1
		}
	}

	if start >= 0 {
		out = append(out, Span{start, len(text)})
	}

	return out
}

// token is the index token of a word, and whether it is one: lower case,
// and within the length the index keeps.
func token(word string) (string, bool) {
	t := strings.ToLower(word)
	if n := utf8.RuneCountInString(t); n < minToken || n > maxToken {
		return "", false
	}

	return t, true
}

// Terms extracts all index terms of a raw message: every token and its
// stems, qualified by field.
func Terms(raw []byte) []Term {
	set := map[Term]struct{}{}
	add := func(field, text string) {
		for _, tok := range Tokenize(text) {
			for _, t := range Expand(tok) {
				set[field+":"+t] = struct{}{}
			}
		}
	}

	hdr, err := textproto.ReadHeader(bufio.NewReader(bytes.NewReader(raw)))
	if err == nil {
		mh := mail.Header{Header: message.Header{Header: hdr}}
		for _, kv := range []struct{ key, field string }{
			{"Subject", FieldSubject}, {"From", FieldFrom}, {"Sender", FieldFrom}, {"Reply-To", FieldFrom},
			{"To", FieldTo}, {"Cc", FieldTo}, {"Bcc", FieldTo},
		} {
			if v, err := mh.Text(kv.key); err == nil && v != "" {
				add(kv.field, v)
			} else if v := hdr.Get(kv.key); v != "" {
				add(kv.field, v)
			}
		}

		fields := mh.Fields()
		for fields.Next() {
			v, err := fields.Text()
			if err != nil {
				v = fields.Value()
			}

			add(FieldHeader, v)
		}
	}

	add(FieldBody, BodyText(raw))

	out := make([]Term, 0, len(set))
	for t := range set {
		out = append(out, t)
	}

	sort.Strings(out)

	return out
}

// BodyText returns the concatenated, decoded text/* parts of a message
// (bounded by MaxBodyBytes). Non-text parts are skipped. If the message
// cannot be parsed, the raw body after the header is used.
func BodyText(raw []byte) string {
	var buf bytes.Buffer

	ent, err := message.Read(bytes.NewReader(raw))
	if err != nil && ent == nil {
		if i := bytes.Index(raw, []byte("\r\n\r\n")); i >= 0 {
			return string(raw[i+4:])
		}

		return string(raw)
	}

	walk(ent, &buf)

	return buf.String()
}

func walk(e *message.Entity, buf *bytes.Buffer) {
	if buf.Len() >= MaxBodyBytes {
		return
	}

	if mr := e.MultipartReader(); mr != nil {
		for {
			p, err := mr.NextPart()
			if err != nil {
				return
			}

			walk(p, buf)
		}
	}

	ct, _, _ := e.Header.ContentType()
	if ct == "" || strings.HasPrefix(ct, "text/") {
		_, _ = io.Copy(buf, io.LimitReader(e.Body, int64(MaxBodyBytes-buf.Len())))
		buf.WriteByte(' ')
	}
}
