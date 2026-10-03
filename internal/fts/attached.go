package fts

import (
	"fmt"
	"strings"
	"sync/atomic"
)

// Header fields of attached messages (temp/13 §3 c3, stage B14h). The
// body text has the header of every attached message (message/rfc822,
// message/global) as "Name: value" lines and the text of a
// text/rfc822-headers part (the header of a bounced message) as it is.
// Their signature and trace fields — DKIM and ARC signatures,
// authentication results, Received lines — are long runs of letters
// and digits that are new in every message: they cost the substring
// dictionary hundreds of keys per message and are hardly ever searched
// for. AttachedHeadersNoSig leaves exactly these fields out of the body
// text; the fields of the message itself (FieldHeader, the stored
// header text) are not affected.
const (
	// AttachedHeadersAll keeps every header field of an attached message
	// in the body text (the default, as before B14h).
	AttachedHeadersAll = "all"
	// AttachedHeadersNoSig leaves the fields SignatureField names out.
	AttachedHeadersNoSig = "nosig"
)

// signatureFields are the signature and trace fields of temp/13 §3 c3
// (lower case); X-*-Signature counts as well.
var signatureFields = map[string]bool{
	"dkim-signature": true, "arc-seal": true, "arc-message-signature": true, "arc-authentication-results": true,
	"authentication-results": true, "received": true, "received-spf": true, "x-google-dkim-signature": true,
}

// SignatureField reports whether a header field is a signature or trace
// field that AttachedHeadersNoSig leaves out of attached headers: the
// list of temp/13 §3 c3 and every X-*-Signature field.
func SignatureField(name string) bool {
	n := strings.ToLower(strings.TrimSpace(name))

	return signatureFields[n] || (strings.HasPrefix(n, "x-") && strings.HasSuffix(n, "-signature"))
}

// TextOptions are the settings of the text preparation that an index
// generation records, because they change which terms it holds (B14h):
// a generation is searched with the preparation it was built with.
type TextOptions struct {
	// AttachedHeaders is AttachedHeadersAll ("" is the same) or
	// AttachedHeadersNoSig.
	AttachedHeaders string
}

// NoSig reports whether the options leave signature fields of attached
// messages out.
func (o TextOptions) NoSig() bool { return o.AttachedHeaders == AttachedHeadersNoSig }

// ValidAttachedHeaders reports whether mode is a value of
// fts.attached_headers.
func ValidAttachedHeaders(mode string) bool {
	return mode == AttachedHeadersAll || mode == AttachedHeadersNoSig
}

// attachedNoSig is the process setting of fts.attached_headers that
// BodyText, Terms and IndexText use (SetAttachedHeaders).
var attachedNoSig atomic.Bool

// SetAttachedHeaders sets the attached header fields that BodyText,
// Terms and IndexText keep: the server's semantics of IMAP SEARCH BODY
// and TEXT (the check of every candidate against the message) follow it.
// The index of a layout 3 generation is built with the options its
// layout record names, see the cephstore. An unknown mode is an error
// and changes nothing.
func SetAttachedHeaders(mode string) error {
	switch mode {
	case AttachedHeadersAll, "":
		attachedNoSig.Store(false)
	case AttachedHeadersNoSig:
		attachedNoSig.Store(true)
	default:
		return fmt.Errorf("unknown attached headers mode %q (all, nosig)", mode)
	}

	return nil
}

// DefaultTextOptions are the options SetAttachedHeaders set.
func DefaultTextOptions() TextOptions {
	if attachedNoSig.Load() {
		return TextOptions{AttachedHeaders: AttachedHeadersNoSig}
	}

	return TextOptions{}
}

// withoutSignatureFields removes the signature fields (SignatureField)
// and their continuation lines from the text of a header block, keeping
// every other line as it is.
func withoutSignatureFields(text string) string {
	var (
		b    strings.Builder
		skip bool
	)

	for line := range strings.SplitAfterSeq(text, "\n") {
		if line != "" && line[0] != ' ' && line[0] != '\t' {
			name, _, ok := strings.Cut(line, ":")
			skip = ok && SignatureField(name)
		}

		if !skip {
			b.WriteString(line)
		}
	}

	return b.String()
}
