package sieve

import (
	"bufio"
	"bytes"
	"fmt"
	"mime"
	"net/mail"
	"net/textproto"
	"strings"
	"time"

	"github.com/foxcpp/go-sieve/interp"

	"shardpost/internal/model"
)

// Reject modes (setting sieve_reject_mode): what a Sieve "reject" does.
// "ereject" always refuses in the protocol.
const (
	// RejectMDN accepts the message and sends a rejection message, an
	// MDN (RFC 5429 2.1, RFC 8098), to the envelope sender. The default.
	RejectMDN = "mdn"
	// RejectProtocol refuses the message in the protocol (LMTP 550,
	// the local delivery API 422), like ereject.
	RejectProtocol = "protocol"
)

// Defaults of the rejection message settings (Dovecot's names and
// defaults): %s is the original subject, %t the recipient, %r the
// reason, %n a line break, %% a percent sign; %d in postmaster_address
// is the recipient's domain.
const (
	DefaultRejectionSubject  = "Rejected: %s"
	DefaultRejectionReason   = "Your message to <%t> was automatically rejected:%n%r"
	DefaultPostmasterAddress = "postmaster@%d"
)

// reasonNullSender is why no automatic message goes to a sender.
const reasonNullSender = "null sender"

// RejectMode is the configured sieve_reject_mode; unset or unknown is
// RejectMDN.
func RejectMode(cfg map[string]string) string {
	if strings.TrimSpace(cfg["sieve_reject_mode"]) == RejectProtocol {
		return RejectProtocol
	}

	return RejectMDN
}

// PostmasterAddress is postmaster_address for a recipient: the address
// automatic messages (rejections, notifications) come from.
func PostmasterAddress(cfg map[string]string, rcpt string) string {
	a := strings.TrimSpace(cfg["postmaster_address"])
	if a == "" {
		a = DefaultPostmasterAddress
	}

	return strings.ReplaceAll(a, "%d", domainOf(strings.Trim(rcpt, "<> ")))
}

// CheckSettings reports Sieve settings that cannot work: vacation
// periods that do not parse or contradict each other, an unknown reject
// mode, a postmaster address that is no address.
func CheckSettings(cfg map[string]string) error {
	if err := interp.CheckVacationPeriods(cfg); err != nil {
		return err
	}

	switch strings.TrimSpace(cfg["sieve_reject_mode"]) {
	case "", RejectMDN, RejectProtocol:
	default:
		return fmt.Errorf("sieve_reject_mode must be %s or %s", RejectMDN, RejectProtocol)
	}

	if a := PostmasterAddress(cfg, "user@example.org"); !validAddress(a) {
		return fmt.Errorf("postmaster_address is not an address")
	}

	return nil
}

// validAddress accepts a bare address without control characters.
func validAddress(a string) bool {
	p, err := mail.ParseAddress(a)

	return err == nil && p.Address == a && strings.Contains(a, "@") && !strings.ContainsAny(a, "\r\n")
}

// Rejection is the input of ComposeRejection.
type Rejection struct {
	// Reason is the reason the script gave.
	Reason string
	// Original is the rejected message; its header goes into the
	// report.
	Original []byte
	// EnvelopeFrom is the sender the rejection goes to, Rcpt the
	// recipient that rejected.
	EnvelopeFrom, Rcpt string
	// Host names the reporting server (Reporting-UA).
	Host string
}

// ComposeRejection builds the rejection message of a Sieve reject
// (RFC 5429 2.1): a multipart/report MDN (RFC 8098) with the reason, the
// disposition "automatic-action/MDN-sent-automatically; deleted" and
// the header of the original. It goes to the envelope sender with the
// null sender; the caller must not send it for mail from the null
// sender or automatic mail (see ShouldReject). cfg holds
// rejection_subject, rejection_reason and postmaster_address.
func ComposeRejection(rej Rejection, cfg map[string]string, now time.Time) (to string, raw []byte) {
	to = strings.Trim(strings.TrimSpace(rej.EnvelopeFrom), "<>")
	rcpt := strings.Trim(strings.TrimSpace(rej.Rcpt), "<>")

	head := headerBlock(rej.Original)
	hdr, _ := textproto.NewReader(bufio.NewReader(bytes.NewReader(rej.Original))).ReadMIMEHeader()

	expand := func(tmpl string) string {
		return expandRejection(tmpl, decodeHeader(hdr.Get("Subject")), rcpt, rej.Reason)
	}

	subject := cfgOr(cfg, "rejection_subject", DefaultRejectionSubject)
	reason := cfgOr(cfg, "rejection_reason", DefaultRejectionReason)
	domain := domainOf(rcpt)
	id := model.NewGUID()

	host := rej.Host
	if host == "" {
		host = domain
	}

	boundary := "=_" + id + "_rejection"

	var b strings.Builder
	fmt.Fprintf(&b, "From: Mail Delivery Subsystem <%s>\r\n", PostmasterAddress(cfg, rcpt))
	fmt.Fprintf(&b, "To: <%s>\r\n", to)
	fmt.Fprintf(&b, "Subject: %s\r\n", mime.QEncoding.Encode("utf-8", interp.LimitSubject(oneLine(expand(subject)))))
	fmt.Fprintf(&b, "Date: %s\r\n", now.UTC().Format(time.RFC1123Z))
	fmt.Fprintf(&b, "Message-ID: <%s.reject@%s>\r\n", id, domain)

	msgID := oneLine(hdr.Get("Message-Id"))
	if msgID != "" {
		fmt.Fprintf(&b, "In-Reply-To: %s\r\n", msgID)
	}

	b.WriteString("Auto-Submitted: auto-replied (rejected)\r\nPrecedence: bulk\r\nMIME-Version: 1.0\r\n")
	fmt.Fprintf(&b, "Content-Type: multipart/report; report-type=disposition-notification;\r\n\tboundary=\"%s\"\r\n\r\n", boundary)
	b.WriteString("This is a MIME-encapsulated message.\r\n\r\n")

	fmt.Fprintf(&b, "--%s\r\nContent-Type: text/plain; charset=utf-8\r\nContent-Disposition: inline\r\nContent-Transfer-Encoding: 8bit\r\n\r\n", boundary)
	b.WriteString(crlf(expand(reason)))
	b.WriteString("\r\n\r\n")

	fmt.Fprintf(&b, "--%s\r\nContent-Type: message/disposition-notification\r\n\r\n", boundary)
	fmt.Fprintf(&b, "Reporting-UA: %s; %s\r\n", oneLine(host), Implementation)
	fmt.Fprintf(&b, "Final-Recipient: rfc822; %s\r\n", rcpt)

	if msgID != "" {
		fmt.Fprintf(&b, "Original-Message-ID: %s\r\n", msgID)
	}

	b.WriteString("Disposition: automatic-action/MDN-sent-automatically; deleted\r\n\r\n")

	fmt.Fprintf(&b, "--%s\r\nContent-Type: text/rfc822-headers\r\n\r\n", boundary)
	b.WriteString(crlf(string(head)))
	b.WriteString("\r\n")
	fmt.Fprintf(&b, "--%s--\r\n", boundary)

	return to, []byte(b.String())
}

// ShouldReject reports whether a rejection message may go out for a
// message (RFC 5429 2.1, RFC 3834): never to the null sender and never
// for automatic mail. The second value names the reason.
func ShouldReject(hdr textproto.MIMEHeader, envelopeFrom string) (bool, string) {
	if strings.Trim(strings.TrimSpace(envelopeFrom), "<>") == "" {
		return false, reasonNullSender
	}

	if automatic(hdr) {
		return false, "auto-submitted"
	}

	return true, ""
}

// automatic reports an Auto-Submitted field other than "no" (RFC 3834
// 5); comments and parameters after the keyword do not count.
func automatic(hdr textproto.MIMEHeader) bool {
	v := strings.ToLower(hdr.Get("Auto-Submitted"))
	if i := strings.IndexAny(v, ";("); i >= 0 {
		v = v[:i]
	}

	v = strings.TrimSpace(v)

	return v != "" && v != valueNo
}

// expandRejection fills a rejection template (%s subject, %t recipient,
// %r reason, %n line break, %% percent sign; other sequences stay).
func expandRejection(tmpl, subject, rcpt, reason string) string {
	var b strings.Builder

	for i := 0; i < len(tmpl); i++ {
		if tmpl[i] != '%' || i+1 == len(tmpl) {
			b.WriteByte(tmpl[i])
			continue
		}

		i++
		switch tmpl[i] {
		case 's':
			b.WriteString(subject)
		case 't':
			b.WriteString(rcpt)
		case 'r':
			b.WriteString(reason)
		case 'n':
			b.WriteString("\n")
		case '%':
			b.WriteByte('%')
		default:
			b.WriteByte('%')
			b.WriteByte(tmpl[i])
		}
	}

	return b.String()
}

// headerBlock is the header of a message, without the empty line.
func headerBlock(raw []byte) []byte {
	if i := bytes.Index(raw, []byte("\r\n\r\n")); i >= 0 {
		return raw[:i+2]
	}

	if i := bytes.Index(raw, []byte("\n\n")); i >= 0 {
		return raw[:i+1]
	}

	return raw
}

// crlf normalises line endings to CRLF.
func crlf(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(s, "\r\n", "\n"), "\n", "\r\n")
}

// oneLine removes line breaks from a header value.
func oneLine(s string) string {
	return strings.NewReplacer("\r", " ", "\n", " ").Replace(s)
}

// cfgOr is cfg[key], def when it is unset.
func cfgOr(cfg map[string]string, key, def string) string {
	if v := cfg[key]; v != "" {
		return v
	}

	return def
}
