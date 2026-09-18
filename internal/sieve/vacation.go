package sieve

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"github.com/foxcpp/go-sieve/interp"
	"mime"
	"net/mail"
	"net/textproto"
	"strings"
	"time"
)

// Vacation is a requested auto-reply (RFC 5230).
type Vacation struct {
	Reason    string
	Subject   string
	From      string
	Addresses []string
	Handle    string
	Mime      bool
	Seconds   int
	// Config are the settings the reply honours (subject template).
	Config map[string]string
}

// DefaultVacationSeconds is the suppression period when the script does
// not set :days/:seconds (RFC 5230: 7 days).
const DefaultVacationSeconds = 7 * 86400

// VacationKey identifies a (script, sender) pair for duplicate
// suppression: the handle if given, otherwise a hash of subject and reason.
func (v *Vacation) VacationKey(sender string) string {
	h := sha256.New()
	if v.Handle != "" {
		h.Write([]byte("h:" + v.Handle))
	} else {
		h.Write([]byte("r:" + v.Subject + "\x00" + v.Reason))
	}

	h.Write([]byte("\x00" + strings.ToLower(sender)))

	return hex.EncodeToString(h.Sum(nil))[:32]
}

// Period is the suppression duration.
func (v *Vacation) Period() time.Duration {
	if v.Seconds <= 0 {
		return DefaultVacationSeconds * time.Second
	}

	return time.Duration(v.Seconds) * time.Second
}

// ShouldReply applies the RFC 5230 §4.6 rules: no reply to the null
// sender, to automatic mail, to lists, or when none of the user's
// addresses is a recipient of the message. The user's own address (rcpt)
// always counts as one of the addresses.
func (v *Vacation) ShouldReply(hdr textproto.MIMEHeader, envelopeFrom, rcpt string) (bool, string) {
	if strings.Trim(envelopeFrom, " <>") == "" {
		return false, "null sender"
	}

	rcpt = interp.VacationRecipient(rcpt, rcpt, v.Config)
	addresses := interp.VacationAddresses(v.Addresses, v.Config)

	addresses = append(addresses, rcpt)
	if v.Config["sieve_vacation_check_recipient"] == valueNo {
		rcpt = ""
	}

	return interp.VacationShouldReply(hdr, envelopeFrom, rcpt, addresses, v.From)
}

// Compose builds the reply message. rcpt is the user's address (From
// unless :from is set); the reply goes to the envelope sender.
func (v *Vacation) Compose(hdr textproto.MIMEHeader, envelopeFrom, rcpt string, now time.Time) (to string, raw []byte) {
	to = strings.Trim(strings.TrimSpace(envelopeFrom), "<>")
	// the reply comes from the address the message was sent to (RFC 5230
	// 4.6): rcpt or one of :addresses found in the recipient headers
	rcpt = interp.VacationRecipient(rcpt, rcpt, v.Config)

	from := interp.VacationReplyFrom(hdr, rcpt, interp.VacationAddresses(v.Addresses, v.Config))
	if v.From != "" {
		if a, err := mail.ParseAddress(v.From); err == nil && !strings.ContainsAny(v.From, "\r\n") {
			from = a.String()
		}
	}

	subject := interp.VacationSubject(v.Subject, decodeHeader(hdr.Get("Subject")), v.Config["sieve_vacation_default_subject_template"], v.Config["sieve_vacation_default_subject"])

	var b strings.Builder
	fmt.Fprintf(&b, "From: %s\r\n", from)
	fmt.Fprintf(&b, "To: %s\r\n", interp.VacationToHeader(hdr, to, v.Config["sieve_vacation_to_header_ignore_envelope"] == settingYes))
	fmt.Fprintf(&b, "Subject: %s\r\n", mime.QEncoding.Encode("utf-8", subject))
	fmt.Fprintf(&b, "Date: %s\r\n", now.UTC().Format(time.RFC1123Z))
	fmt.Fprintf(&b, "Message-ID: <%s.vacation@%s>\r\n", v.VacationKey(to)[:16], domainOf(rcpt))

	if id := hdr.Get("Message-ID"); id != "" {
		fmt.Fprintf(&b, "In-Reply-To: %s\r\nReferences: %s\r\n", id, interp.VacationReferences(hdr))
	}

	b.WriteString("Auto-Submitted: auto-replied\r\nPrecedence: bulk\r\nX-Auto-Response-Suppress: All\r\n")

	if v.Mime {
		b.WriteString("MIME-Version: 1.0\r\n")
		b.WriteString(strings.ReplaceAll(strings.ReplaceAll(v.Reason, "\r\n", "\n"), "\n", "\r\n"))

		if !strings.HasSuffix(v.Reason, "\n") {
			b.WriteString("\r\n")
		}
	} else {
		b.WriteString("MIME-Version: 1.0\r\nContent-Type: text/plain; charset=utf-8\r\nContent-Transfer-Encoding: 8bit\r\n\r\n")
		b.WriteString(strings.ReplaceAll(strings.ReplaceAll(v.Reason, "\r\n", "\n"), "\n", "\r\n"))
		b.WriteString("\r\n")
	}

	return to, []byte(b.String())
}

func decodeHeader(s string) string {
	d := new(mime.WordDecoder)
	if out, err := d.DecodeHeader(s); err == nil {
		return out
	}

	return s
}

func domainOf(addr string) string {
	if i := strings.LastIndex(addr, "@"); i >= 0 {
		return addr[i+1:]
	}

	return "localhost"
}

// EnvelopeSender keeps the safe null sender by default; the explicit server
// setting can select the recipient as the bounce address.
func (v *Vacation) EnvelopeSender(rcpt string) string {
	if v.Config["sieve_vacation_send_from_recipient"] == settingYes {
		return strings.Trim(strings.TrimSpace(rcpt), "<>")
	}

	return ""
}
