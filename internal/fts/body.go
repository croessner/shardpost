package fts

import (
	"bytes"
	"fmt"
	"io"
	"strings"
	"sync/atomic"

	"github.com/emersion/go-message"

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

const mediaHTML = "text/html"

var htmlMode atomic.Value // string

func init() { htmlMode.Store(HTMLModeText) }

// SetHTMLMode sets what BodyText makes of text/html parts; an unknown
// mode is an error and changes nothing.
func SetHTMLMode(mode string) error {
	switch mode {
	case HTMLModeText, HTMLModeRaw, HTMLModeSkip:
		htmlMode.Store(mode)

		return nil
	}

	return fmt.Errorf("unknown HTML mode %q", mode)
}

// BodyText returns the concatenated text/* parts of a message as UTF-8,
// bounded by MaxBodyBytes: transfer encoding undone, converted from the
// part's charset (text without one that is not UTF-8 is read in the
// fallback charset, see charset.SetFallback), and text/html reduced to
// its visible text unless SetHTMLMode says otherwise. Non-text parts
// are skipped. If the message cannot be parsed, the raw body after the
// header is used. The index, IMAP SEARCH read without the index and the
// cached preview all take the body text from here, so they agree.
func BodyText(raw []byte) string {
	var buf strings.Builder

	ent, err := message.Read(bytes.NewReader(raw))
	if err != nil && ent == nil {
		if i := bytes.Index(raw, []byte("\r\n\r\n")); i >= 0 {
			raw = raw[i+4:]
		}

		return charset.ToUTF8(raw[:min(len(raw), MaxBodyBytes)], "")
	}

	walk(ent, &buf)

	return buf.String()
}

// walk appends the text of e and its parts to buf. go-message has no
// charset reader here (message.CharsetReader stays nil), so a text part
// in a charset other than UTF-8 or US-ASCII arrives as it was written,
// with an error that says so, and is converted here. A part in an
// unknown transfer encoding is taken as it is.
func walk(e *message.Entity, buf *strings.Builder) {
	if buf.Len() >= MaxBodyBytes {
		return
	}

	if mr := e.MultipartReader(); mr != nil {
		for {
			p, err := mr.NextPart()
			if p == nil || err != nil && !message.IsUnknownCharset(err) && !message.IsUnknownEncoding(err) {
				return
			}

			walk(p, buf)
		}
	}

	text, ok := partText(e)
	if !ok {
		return
	}

	if room := MaxBodyBytes - buf.Len(); len(text) > room {
		text = strings.ToValidUTF8(text[:room], "")
	}

	buf.WriteString(text)
	buf.WriteByte(' ')
}

// partText returns the text of a leaf part as UTF-8; false for a part
// that is not text, or HTML that SetHTMLMode leaves out.
func partText(e *message.Entity) (string, bool) {
	ct, params, _ := e.Header.ContentType()
	if ct != "" && !strings.HasPrefix(ct, "text/") {
		return "", false
	}

	mode, _ := htmlMode.Load().(string)
	if ct == mediaHTML && mode == HTMLModeSkip {
		return "", false
	}

	b, _ := io.ReadAll(io.LimitReader(e.Body, MaxBodyBytes))

	label := params["charset"]
	if ct == mediaHTML && label == "" {
		label = htmlCharset(b)
	}

	text := charset.ToUTF8(b, label)
	if ct == mediaHTML && mode == HTMLModeText {
		text = HTMLText(text)
	}

	return text, true
}

// htmlCharset returns the charset an HTML part without a charset
// parameter declares in a meta element near its start (<meta
// charset="…"> or <meta http-equiv="Content-Type" content="text/html;
// charset=…">), or "".
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
