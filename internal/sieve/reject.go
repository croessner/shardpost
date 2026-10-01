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

// fallbackDomain names the domain of generated messages when nothing
// better is known; .invalid (RFC 2606) says so honestly.
const fallbackDomain = "shardpost.invalid"

// reportDomain is the domain of the Message-ID, the Reporting-UA (when
// no host is set) and the postmaster address of a rejection: the
// recipient's domain; for a recipient without one (a bare account name)
// the configured host (server.lmtp_hostname), else the domain of a
// literal postmaster_address, else fallbackDomain — never "localhost".
func reportDomain(rcpt, host string, cfg map[string]string) string {
	if i := strings.LastIndex(rcpt, "@"); i >= 0 && i+1 < len(rcpt) {
		return rcpt[i+1:]
	}

	if host != "" {
		return host
	}

	if pm := strings.TrimSpace(cfg["postmaster_address"]); !strings.Contains(pm, "%d") {
		if i := strings.LastIndex(pm, "@"); i >= 0 && i+1 < len(pm) {
			return pm[i+1:]
		}
	}

	return fallbackDomain
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
	to = oneLine(strings.Trim(strings.TrimSpace(rej.EnvelopeFrom), "<>"))
	rcpt := oneLine(strings.Trim(strings.TrimSpace(rej.Rcpt), "<>"))

	head := headerBlock(rej.Original)
	hdr, _ := textproto.NewReader(bufio.NewReader(bytes.NewReader(rej.Original))).ReadMIMEHeader()

	expand := func(tmpl string) string {
		return expandRejection(tmpl, decodeHeader(hdr.Get("Subject")), rcpt, rej.Reason)
	}

	subject := cfgOr(cfg, "rejection_subject", DefaultRejectionSubject)
	reason := cfgOr(cfg, "rejection_reason", DefaultRejectionReason)
	domain := reportDomain(rcpt, rej.Host, cfg)
	id := model.NewGUID()

	// Without a configured name the recipient's domain stands in: the
	// host name of a pod is nothing a sender should see.
	host := rej.Host
	if host == "" {
		host = domain
	}

	parts := reportParts{
		boundary: "=_" + id + "_rejection",
		text:     wrapLongLines(crlf(expand(reason))),
		report:   mdnFields(oneLine(host), rcpt, oneLine(hdr.Get("Message-Id"))),
		headers:  crlf(string(head)),
	}
	parts.global = !isASCII(rcpt) || !isASCII(parts.headers)

	var b strings.Builder
	fmt.Fprintf(&b, "From: Mail Delivery Subsystem <%s>\r\n", PostmasterAddress(cfg, "@"+domain))
	fmt.Fprintf(&b, "To: <%s>\r\n", to)
	fmt.Fprintf(&b, "Subject: %s\r\n", mime.QEncoding.Encode("utf-8", interp.LimitSubject(oneLine(expand(subject)))))
	fmt.Fprintf(&b, "Date: %s\r\n", now.UTC().Format(time.RFC1123Z))
	fmt.Fprintf(&b, "Message-ID: <%s.reject@%s>\r\n", id, domain)

	if msgID := oneLine(hdr.Get("Message-Id")); msgID != "" {
		fmt.Fprintf(&b, "In-Reply-To: %s\r\n", msgID)
		b.WriteString(foldField("References", oneLine(interp.VacationReferences(hdr))))
	}

	b.WriteString("Auto-Submitted: auto-replied (rejected)\r\nPrecedence: bulk\r\nMIME-Version: 1.0\r\n")
	parts.write(&b)

	return to, []byte(b.String())
}

// reportParts are the three parts of a rejection MDN.
type reportParts struct {
	boundary, text, report, headers string
	// global marks an MDN about a recipient or a header that is not
	// ASCII (RFC 6533 6): its report and header parts carry UTF-8.
	global bool
}

// write writes the Content-Type of the report and its three parts.
func (p reportParts) write(b *strings.Builder) {
	reportType, headersType := "disposition-notification", "text/rfc822-headers"
	if p.global {
		reportType, headersType = "global-disposition-notification", "message/global-headers"
	}

	fmt.Fprintf(b, "Content-Type: multipart/report; report-type=%s;\r\n\tboundary=\"%s\"\r\n", reportType, p.boundary)
	// 8-bit content is declared, on the parts and on the whole (RFC 2045
	// 6.4); the relay client asks for BODY=8BITMIME and SMTPUTF8 when the
	// relay offers them (net/smtp does).
	b.WriteString(transferEncoding(p.text + p.report + p.headers))
	b.WriteString("\r\nThis is a MIME-encapsulated message.\r\n\r\n")

	fmt.Fprintf(b, "--%s\r\nContent-Type: text/plain; charset=utf-8\r\nContent-Disposition: inline\r\n%s\r\n", p.boundary, transferEncoding(p.text))
	b.WriteString(p.text)
	b.WriteString("\r\n\r\n")

	fmt.Fprintf(b, "--%s\r\nContent-Type: message/%s\r\n%s\r\n", p.boundary, reportType, transferEncoding(p.report))
	b.WriteString(p.report)
	b.WriteString("\r\n")

	fmt.Fprintf(b, "--%s\r\nContent-Type: %s\r\n%s\r\n", p.boundary, headersType, transferEncoding(p.headers))
	b.WriteString(p.headers)
	b.WriteString("\r\n")
	fmt.Fprintf(b, "--%s--\r\n", p.boundary)
}

// mdnFields is the message/disposition-notification part (RFC 8098 3).
// A recipient that is not ASCII is of the utf-8 address type (RFC 6533).
func mdnFields(host, rcpt, msgID string) string {
	addrType := "rfc822"
	if !isASCII(rcpt) {
		addrType = "utf-8"
	}

	var b strings.Builder
	fmt.Fprintf(&b, "Reporting-UA: %s; %s\r\n", host, Implementation)
	fmt.Fprintf(&b, "Final-Recipient: %s; %s\r\n", addrType, rcpt)

	if msgID != "" {
		fmt.Fprintf(&b, "Original-Message-ID: %s\r\n", msgID)
	}

	b.WriteString("Disposition: automatic-action/MDN-sent-automatically; deleted\r\n")

	return b.String()
}

// transferEncoding is the Content-Transfer-Encoding field for content:
// 8bit when it is not ASCII, 7bit otherwise.
func transferEncoding(content string) string {
	if isASCII(content) {
		return "Content-Transfer-Encoding: 7bit\r\n"
	}

	return "Content-Transfer-Encoding: 8bit\r\n"
}

// isASCII reports whether s is 7-bit.
func isASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= 0x80 {
			return false
		}
	}

	return true
}

// ShouldReject reports whether a rejection message may go out for a
// message (RFC 5429 2.1, RFC 3834): never to the null sender, and not
// where a vacation reply would not go either — automatic mail, list
// mail, list or system senders, X-Auto-Response-Suppress (see
// interp.AutoResponseSuppressed). The second value names the reason.
func ShouldReject(hdr textproto.MIMEHeader, envelopeFrom string) (bool, string) {
	sender := strings.Trim(strings.TrimSpace(envelopeFrom), "<>")
	if sender == "" {
		return false, reasonNullSender
	}

	if why := interp.AutoResponseSuppressed(hdr, sender); why != "" {
		return false, why
	}

	return true, ""
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
