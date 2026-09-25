// Package mailmeta derives the metadata that is cached in the mailbox index
// record of a message: the IMAP envelope (for FETCH ENVELOPE, SORT and
// THREAD without touching the blob), a short text preview (RFC 8970) and
// the parsed Date header (for SEARCH SENTSINCE/SENTBEFORE).
package mailmeta

import (
	"bufio"
	"bytes"
	"fmt"
	"hash/fnv"
	"strings"
	"unicode"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapserver"
	"github.com/emersion/go-message/textproto"

	"shardpost/internal/fts"
	"shardpost/internal/model"
)

// ThreadID is the conversation a message belongs to: the root of its
// References chain, or its own Message-ID, or — for a message with
// neither — itself. It is what IMAP reports as THREADID and what JMAP
// calls the Email's threadId, and both have to be the same string, so
// there is one implementation of it.
func ThreadID(m *model.Meta) string {
	root := ""

	switch {
	case len(m.References) > 0:
		root = m.References[0]
	case m.Envelope != nil:
		if ids := MessageIDs(m.Envelope.MessageID); len(ids) > 0 {
			root = ids[0]
		}
	}

	if root == "" {
		root = m.GUID
	}

	h := fnv.New64a()
	_, _ = h.Write([]byte(root))

	return fmt.Sprintf("T%016x", h.Sum64())
}

// PreviewLen is the length of the cached preview in runes.
const PreviewLen = 200

// Extract returns envelope, referenced Message-IDs (References, then
// In-Reply-To; used by THREAD and THREADID) and preview of a raw message.
// Envelope is nil if the message cannot be parsed.
func Extract(raw []byte) (*imap.Envelope, []string, string) {
	var (
		env  *imap.Envelope
		refs []string
	)

	if hdr, err := textproto.ReadHeader(bufio.NewReader(bytes.NewReader(raw))); err == nil {
		env = imapserver.ExtractEnvelope(hdr)
		if env != nil {
			// The envelope carries the header values as they are (with
			// angle brackets); THREAD and OBJECTID use MessageIDs().
			env.MessageID = strings.TrimSpace(hdr.Get("Message-Id"))
			env.From = ParseAddressList(hdr.Get("From"))
			env.Sender = ParseAddressList(hdr.Get("Sender"))
			env.ReplyTo = ParseAddressList(hdr.Get("Reply-To"))
			env.To = ParseAddressList(hdr.Get("To"))
			env.Cc = ParseAddressList(hdr.Get("Cc"))
			env.Bcc = ParseAddressList(hdr.Get("Bcc"))
			// RFC 3501 7.4.2: absent (or empty) Sender and Reply-To
			// default to the From value
			if len(env.Sender) == 0 {
				env.Sender = env.From
			}

			if len(env.ReplyTo) == 0 {
				env.ReplyTo = env.From
			}

			env.InReplyTo = nil
			if v := strings.TrimSpace(hdr.Get("In-Reply-To")); v != "" {
				env.InReplyTo = []string{v}
			}
		}

		refs = MessageIDs(hdr.Get("References"))
		for _, id := range MessageIDs(hdr.Get("In-Reply-To")) {
			if !contains(refs, id) {
				refs = append(refs, id)
			}
		}
	}

	return env, refs, Preview(fts.BodyText(raw))
}

// MessageIDs extracts the <...> tokens of a References/In-Reply-To value,
// without the angle brackets (the same normalisation go-imap applies to
// Envelope.MessageID).
func MessageIDs(v string) []string {
	var out []string

	for {
		i := strings.Index(v, "<")
		if i < 0 {
			return out
		}

		j := strings.Index(v[i:], ">")
		if j < 0 {
			return out
		}

		out = append(out, v[i+1:i+j])
		v = v[i+j+1:]
	}
}

func contains(l []string, s string) bool {
	for _, x := range l {
		if x == s {
			return true
		}
	}

	return false
}

// Preview collapses whitespace and cuts the text to PreviewLen runes.
func Preview(text string) string {
	var b strings.Builder

	space := true
	n := 0

	for _, r := range text {
		if unicode.IsSpace(r) {
			if !space {
				b.WriteByte(' ')

				space = true
			}

			continue
		}

		space = false

		b.WriteRune(r)

		if n++; n >= PreviewLen {
			break
		}
	}

	return strings.TrimSpace(b.String())
}
