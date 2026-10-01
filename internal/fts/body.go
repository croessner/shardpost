package fts

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"strings"
	"sync/atomic"

	"github.com/emersion/go-message"
	"github.com/emersion/go-message/textproto"

	"shardpost/internal/charset"
)

// What BodyText makes of a text/html part.
const (
	// HTMLModeText indexes the visible text (the default).
	HTMLModeText = "text"
	// HTMLModeRaw indexes the markup as it is, tags and all.
	HTMLModeRaw = "raw"
	// HTMLModeSkip leaves text/html parts out.
	HTMLModeSkip = "skip"
)

const (
	htmlText int32 = iota
	htmlRaw
	htmlSkip
)

const (
	mediaHTML = "text/html"
	// MaxPartBytes bounds what is read of one part: its decoded bytes
	// before charset conversion.
	MaxPartBytes = MaxBodyBytes
	// MaxReadBytes bounds what is read of all parts of one message
	// together. It is larger than MaxBodyBytes because markup shrinks
	// to its text.
	MaxReadBytes = 2 * MaxBodyBytes
	// maxDepth bounds the nesting of multiparts and attached messages.
	maxDepth = 16
)

var htmlMode atomic.Int32

// SetHTMLMode sets what BodyText makes of text/html parts; an unknown
// mode is an error and changes nothing.
func SetHTMLMode(mode string) error {
	switch mode {
	case HTMLModeText:
		htmlMode.Store(htmlText)
	case HTMLModeRaw:
		htmlMode.Store(htmlRaw)
	case HTMLModeSkip:
		htmlMode.Store(htmlSkip)
	default:
		return fmt.Errorf("unknown HTML mode %q", mode)
	}

	return nil
}

// BodyText returns the text of a message body as UTF-8: the text/*
// parts, transfer encoding undone, converted from their charset (text
// without one that is not UTF-8 is read in the fallback charset, see
// charset.SetFallback), and text/html reduced to its visible text unless
// SetHTMLMode says otherwise; the file names of all parts (Content-Type
// name, Content-Disposition filename); and of an attached message
// (message/rfc822, message/global) its header fields as "Name: value"
// lines and its body text, recursively. Other parts are skipped.
//
// The result is bounded by MaxBodyBytes. Reading is bounded too: at
// most MaxPartBytes of a part and MaxReadBytes of all parts are read
// before conversion, so a message costs a few times MaxBodyBytes of
// memory at most, whatever it holds. If the message cannot be parsed,
// the raw body after the header is used, within the same bounds.
//
// The index, IMAP SEARCH matched against the message and the cached
// preview all take the body text from here, so they agree.
func BodyText(raw []byte) string { return bodyText(raw, false) }

// ReadableText is BodyText without file names and attached messages:
// the text a reader sees in the message itself, which is what its
// preview (RFC 8970) is cut from.
func ReadableText(raw []byte) string { return bodyText(raw, true) }

func bodyText(raw []byte, readable bool) string {
	ent, err := message.Read(bytes.NewReader(raw))
	if err != nil && ent == nil {
		if i := bytes.Index(raw, []byte("\r\n\r\n")); i >= 0 {
			raw = raw[i+4:]
		}

		return bound(charset.ToUTF8(raw[:min(len(raw), MaxPartBytes)], ""), MaxBodyBytes)
	}

	w := &walker{read: MaxReadBytes, readable: readable}
	w.walk(ent, 0)

	return w.out.String()
}

// bound cuts s to at most n bytes without splitting a character.
func bound(s string, n int) string {
	if len(s) <= n {
		return s
	}

	return strings.ToValidUTF8(s[:n], "")
}

// walker collects the body text of a message.
type walker struct {
	out      strings.Builder
	read     int  // what may still be read of the parts
	readable bool // text parts only, see ReadableText
}

func (w *walker) full() bool { return w.out.Len() >= MaxBodyBytes || w.read <= 0 }

// add appends text and a separator, within MaxBodyBytes.
func (w *walker) add(text string) {
	if text == "" {
		return
	}

	w.out.WriteString(bound(text, MaxBodyBytes-w.out.Len()))
	w.out.WriteByte(' ')
}

// walk appends the text of e and its parts. go-message has no charset
// reader here (message.CharsetReader stays nil), so a text part in a
// charset other than UTF-8 or US-ASCII arrives as it was written, with
// an error that says so, and is converted here. A part in an unknown
// transfer encoding is taken as it is.
func (w *walker) walk(e *message.Entity, depth int) {
	if w.full() || depth > maxDepth {
		return
	}

	if mr := e.MultipartReader(); mr != nil {
		for !w.full() {
			p, err := mr.NextPart()
			if p == nil || (err != nil && !message.IsUnknownCharset(err) && !message.IsUnknownEncoding(err)) {
				return
			}

			w.walk(p, depth+1)
		}

		return
	}

	w.leaf(e, depth)
}

// leaf appends the file name and the text of a part that is not a
// multipart.
func (w *walker) leaf(e *message.Entity, depth int) {
	ct, params := mediaParams(e.Header.Get("Content-Type"))
	if ct == "" {
		ct = "text/plain"
	}

	if !w.readable {
		_, disp := mediaParams(e.Header.Get("Content-Disposition"))
		w.add(strings.TrimSpace(params["name"] + " " + disp["filename"]))
	}

	switch {
	case (ct == "message/rfc822" || ct == "message/global") && !w.readable:
		w.attached(e, depth)
	case strings.HasPrefix(ct, "text/"):
		w.add(w.text(e, ct, params["charset"]))
	}
}

// attached appends the header and the body text of an attached
// message.
func (w *walker) attached(e *message.Entity, depth int) {
	b := w.take(e.Body, MaxPartBytes)

	hdr, err := textproto.ReadHeader(bufio.NewReader(bytes.NewReader(b)))
	if err == nil {
		var lines strings.Builder

		fields := hdr.Fields()
		for fields.Next() {
			lines.WriteString(fields.Key())
			lines.WriteString(": ")
			lines.WriteString(HeaderValue(fields.Value()))
			lines.WriteByte('\n')
		}

		w.add(lines.String())
	}

	inner, err := message.Read(bytes.NewReader(b))
	if err != nil && inner == nil {
		return
	}

	w.walk(inner, depth+1)
}

// text returns the text of a text/* part as UTF-8, or "" for HTML that
// SetHTMLMode leaves out. A part other than HTML is read only as far as
// there is room for its text.
func (w *walker) text(e *message.Entity, ct, label string) string {
	mode := htmlMode.Load()
	if ct == mediaHTML && mode == htmlSkip {
		return ""
	}

	limit := MaxPartBytes
	if ct != mediaHTML {
		limit = min(limit, MaxBodyBytes-w.out.Len())
	}

	b := w.take(e.Body, limit)

	if ct == mediaHTML && !charset.Declared(label) {
		if meta := htmlCharset(b); meta != "" {
			label = meta
		}
	}

	text := charset.ToUTF8(b, label)
	if ct == mediaHTML && mode == htmlText {
		text = HTMLText(text)
	}

	return text
}

// take reads up to limit bytes of r, within what may still be read.
func (w *walker) take(r io.Reader, limit int) []byte {
	b, _ := io.ReadAll(io.LimitReader(r, int64(max(0, min(limit, w.read)))))
	w.read -= len(b)

	return b
}

// htmlCharset returns the charset an HTML part declares in a meta
// element near its start (<meta charset="…"> or <meta
// http-equiv="Content-Type" content="text/html; charset=…">), or "".
func htmlCharset(b []byte) string {
	head := strings.ToLower(string(b[:min(len(b), 4096)]))

	for i := 0; ; {
		j := strings.Index(head[i:], "<meta")
		if j < 0 {
			return ""
		}

		i += j + len("<meta")

		end := strings.IndexByte(head[i:], '>')
		if end < 0 {
			return ""
		}

		tag := head[i : i+end]
		if k := strings.Index(tag, "charset="); k >= 0 {
			v := strings.TrimLeft(tag[k+len("charset="):], `"' `)
			if n := strings.IndexAny(v, `"' ;/`); n >= 0 {
				v = v[:n]
			}

			if v != "" {
				return v
			}
		}
	}
}
