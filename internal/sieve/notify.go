package sieve

import (
	"fmt"
	"mime"
	"net/textproto"
	"strings"
	"time"

	"github.com/foxcpp/go-sieve/interp"
)

// ComposeNotification builds one mailto notification (RFC 5436 2.7) for
// the recipients of n. It returns the envelope sender, the recipients and
// the message; ok is false when the original message is automatic mail
// (RFC 5436 2.7.1) or nothing is left to notify. rcpt is the user's
// address, envelopeFrom the original sender ("<>" or "" = null), cfg the
// settings (sieve_notify_mailto_envelope_from, sieve_user_email).
func ComposeNotification(n Notification, hdr textproto.MIMEHeader, envelopeFrom, rcpt string, cfg map[string]string, now time.Time) (from string, to []string, raw []byte, ok bool) {
	if as := strings.ToLower(hdr.Get("Auto-Submitted")); as != "" && as != valueNo {
		return "", nil, nil, false
	}

	to = append(append(append([]string{}, n.To...), n.Cc...), n.Bcc...)
	if len(to) == 0 {
		return "", nil, nil, false
	}

	from = notificationSender(n, envelopeFrom, rcpt, cfg)

	var b strings.Builder

	hdrFrom := n.From
	if hdrFrom == "" {
		hdrFrom = "Postmaster <postmaster@" + domainOf(rcpt) + ">"
	}

	fmt.Fprintf(&b, "From: %s\r\n", hdrFrom)

	if len(n.To) > 0 {
		fmt.Fprintf(&b, "To: %s\r\n", strings.Join(n.To, ", "))
	}

	if len(n.Cc) > 0 {
		fmt.Fprintf(&b, "Cc: %s\r\n", strings.Join(n.Cc, ", "))
	}

	fmt.Fprintf(&b, "Subject: %s\r\n", mime.QEncoding.Encode("utf-8", interp.LimitSubject(notificationSubject(n, hdr))))
	fmt.Fprintf(&b, "Date: %s\r\n", now.UTC().Format(time.RFC1123Z))
	fmt.Fprintf(&b, "Message-ID: <%d.notify@%s>\r\n", now.UnixNano(), domainOf(rcpt))
	fmt.Fprintf(&b, "Auto-Submitted: auto-notified; owner-email=\"%s\"\r\n", rcpt)
	b.WriteString("X-Sieve: ShardPost\r\nPrecedence: bulk\r\n")
	b.WriteString(importanceHeaders(n.Importance))

	for _, h := range n.Headers {
		fmt.Fprintf(&b, "%s: %s\r\n", h[0], h[1])
	}

	b.WriteString("MIME-Version: 1.0\r\nContent-Type: text/plain; charset=utf-8\r\nContent-Transfer-Encoding: 8bit\r\n\r\n")

	body := notificationBody(n, hdr)
	b.WriteString(strings.ReplaceAll(strings.ReplaceAll(body, "\r\n", "\n"), "\n", "\r\n"))
	b.WriteString("\r\n")

	return from, to, []byte(b.String()), true
}

// notificationSender is the envelope sender of a notification: :from or
// postmaster of the user's domain, overridden by
// sieve_notify_mailto_envelope_from, and null for mail from the null
// sender.
func notificationSender(n Notification, envelopeFrom, rcpt string, cfg map[string]string) string {
	from := "postmaster@" + domainOf(rcpt)
	if n.From != "" {
		from = n.From
	}

	switch strings.TrimSpace(cfg["sieve_notify_mailto_envelope_from"]) {
	case "sender":
		from = strings.Trim(envelopeFrom, "<>")
	case "recipient":
		from = rcpt
	case "user_email":
		if u := cfg["sieve_user_email"]; u != "" {
			from = u
		}
	}

	if strings.TrimSpace(envelopeFrom) == "<>" || strings.TrimSpace(envelopeFrom) == "" {
		from = "" // notifications about mail from the null sender go out as null sender too
	}

	return from
}

// notificationSubject is the mailto subject, else the :message, else a
// subject derived from the original message.
func notificationSubject(n Notification, hdr textproto.MIMEHeader) string {
	if n.Subject != "" {
		return n.Subject
	}

	if n.Message != "" {
		return n.Message
	}

	return "Notification: " + decodeHeader(hdr.Get("Subject"))
}

// importanceHeaders maps :importance to the priority header fields.
func importanceHeaders(importance string) string {
	switch importance {
	case "1":
		return "X-Priority: 1 (Highest)\r\nImportance: High\r\n"
	case "3":
		return "X-Priority: 5 (Lowest)\r\nImportance: Low\r\n"
	default:
		return "X-Priority: 3 (Normal)\r\nImportance: Normal\r\n"
	}
}

// notificationBody is the mailto body, else the :message, else a line
// naming the original sender and subject.
func notificationBody(n Notification, hdr textproto.MIMEHeader) string {
	body := n.Body
	if body == "" {
		body = n.Message
	}

	if body == "" {
		body = "Notification of new message from " + decodeHeader(hdr.Get("From")) + ": " + decodeHeader(hdr.Get("Subject"))
	}

	return body
}
