package delivery

import (
	"bytes"
	"strings"
)

// Delivered-To modes (Service.DeliveredTo, setting
// server.lmtp_delivered_to), after Dovecot's lmtp_hdr_delivery_address.
const (
	// DeliveredToFinal adds "Delivered-To: <envelope recipient>".
	DeliveredToFinal = "final"
	// DeliveredToNone adds no Delivered-To field.
	DeliveredToNone = "none"
)

// withDeliveryHeaders returns the copy of a message one recipient gets:
// "Return-Path: <envelope sender>" (the null sender is "<>") and
// "Delivered-To: <recipient>" on top, the way Dovecot's LMTP writes them,
// above the Received field the caller already added. Return-Path fields
// the message brought along are removed first: only the final delivery
// sets one (RFC 5321 4.4), so any other is untrustworthy. Without either
// setting the message is returned as it is.
func (s *Service) withDeliveryHeaders(req Request) []byte {
	var top []string

	if s.ReturnPath {
		if from := envelopePath(req.From); !hasControl(from) {
			top = append(top, "Return-Path: <"+from+">\r\n")
		}
	}

	if s.DeliveredTo == DeliveredToFinal {
		if rcpt := envelopePath(req.Rcpt); rcpt != "" && !hasControl(rcpt) {
			top = append(top, "Delivered-To: "+rcpt+"\r\n")
		}
	}

	if len(top) == 0 {
		return req.Raw
	}

	raw := req.Raw
	if s.ReturnPath {
		raw = removeField(raw, "Return-Path")
	}

	n := len(raw)
	for _, h := range top {
		n += len(h)
	}

	out := make([]byte, 0, n)
	for _, h := range top {
		out = append(out, h...)
	}

	return append(out, raw...)
}

// envelopePath is an envelope address without angle brackets ("" for
// the null sender).
func envelopePath(a string) string {
	return strings.Trim(strings.TrimSpace(a), "<>")
}

// hasControl reports control characters, which must not reach a header.
func hasControl(s string) bool {
	return strings.ContainsFunc(s, func(r rune) bool { return r < 0x20 || r == 0x7f })
}

// removeField drops every field called name (with its continuation
// lines) from the header of raw; the body stays untouched. raw is
// returned as it is when there is no such field.
func removeField(raw []byte, name string) []byte {
	var (
		out  []byte
		skip bool // inside a removed field
	)

	end := headerEnd(raw)

	for pos := 0; pos < end; {
		line := nextLine(raw[pos:end])

		if line[0] != ' ' && line[0] != '\t' {
			skip = isField(line, name)
		}

		switch {
		case skip && out == nil:
			out = append(make([]byte, 0, len(raw)), raw[:pos]...)
		case !skip && out != nil:
			out = append(out, line...)
		}

		pos += len(line)
	}

	if out == nil {
		return raw
	}

	return append(out, raw[end:]...)
}

// headerEnd is the offset of the empty line that ends the header of
// raw, len(raw) when there is none.
func headerEnd(raw []byte) int {
	for pos := 0; pos < len(raw); {
		line := nextLine(raw[pos:])
		if len(bytes.TrimRight(line, "\r\n")) == 0 {
			return pos
		}

		pos += len(line)
	}

	return len(raw)
}

// nextLine is the first line of b, with its line end.
func nextLine(b []byte) []byte {
	if i := bytes.IndexByte(b, '\n'); i >= 0 {
		return b[:i+1]
	}

	return b
}

// isField reports whether line starts the header field name.
func isField(line []byte, name string) bool {
	field, _, ok := bytes.Cut(line, []byte(":"))

	return ok && strings.EqualFold(strings.TrimSpace(string(field)), name)
}

