package interp

import (
	"mime"
	"net/mail"
	"net/textproto"
	"strings"
)

// VacationShouldReply applies RFC 5230 4.5/4.6: no reply to the null
// sender, to list and system senders, to automatic mail, to the user's
// own address, and only when the user (rcpt, :addresses or :from) is
// addressed in the message. The second value names the reason.
func VacationShouldReply(hdr textproto.MIMEHeader, envelopeFrom, rcpt string, addresses []string, from string) (bool, string) {
	sender := strings.TrimSpace(envelopeFrom)
	if sender == "<>" {
		return false, "null sender"
	}
	sender = strings.Trim(sender, "<>")
	if sender == "" {
		// sender unknown (no envelope): the header From decides
		if a, err := mail.ParseAddress(hdr.Get("From")); err == nil {
			sender = a.Address
		}
	}
	if sender == "" {
		return false, "null sender"
	}
	ls := strings.ToLower(sender)
	if strings.HasPrefix(ls, "owner-") || strings.HasSuffix(strings.SplitN(ls, "@", 2)[0], "-request") || strings.Contains(ls, "mailer-daemon") ||
		strings.HasPrefix(ls, "no-reply") || strings.HasPrefix(ls, "noreply") {
		return false, "list or system sender"
	}
	if as := strings.ToLower(hdr.Get("Auto-Submitted")); as != "" && as != "no" {
		return false, "auto-submitted"
	}
	for _, h := range []string{"List-Id", "List-Post", "List-Unsubscribe", "List-Help", "List-Subscribe"} {
		if hdr.Get(h) != "" {
			return false, "mailing list"
		}
	}
	if p := strings.ToLower(hdr.Get("Precedence")); p == "bulk" || p == "list" || p == "junk" {
		return false, "precedence " + p
	}
	for _, v := range strings.Split(strings.ToLower(hdr.Get("X-Auto-Response-Suppress")), ",") {
		if v = strings.TrimSpace(v); v == "all" || v == "oof" {
			return false, "auto-response suppressed"
		}
	}
	mine := map[string]bool{}
	if rcpt != "" {
		mine[strings.ToLower(rcpt)] = true
	}
	for _, a := range addresses {
		mine[strings.ToLower(strings.TrimSpace(a))] = true
	}
	if from != "" {
		if a, err := mail.ParseAddress(from); err == nil {
			mine[strings.ToLower(a.Address)] = true
		}
	}
	if mine[ls] {
		return false, "sender is the user"
	}
	if rcpt == "" {
		return true, "" // recipient unknown: the addressed check is skipped
	}
	found := false
	for _, h := range []string{"To", "Cc", "Bcc", "Resent-To", "Resent-Cc", "Resent-Bcc"} {
		for _, raw := range hdr.Values(h) {
			list, err := mail.ParseAddressList(raw)
			if err != nil {
				continue
			}
			for _, a := range list {
				if mine[strings.ToLower(a.Address)] {
					found = true
				}
			}
		}
	}
	if !found {
		return false, "user not addressed"
	}
	return true, ""
}

// VacationReplyFrom is the address the reply is sent from: the user's
// address that the message was sent to (rcpt, or one of :addresses found
// in To/Cc), so that the reply comes from the address that was written to.
func VacationReplyFrom(hdr textproto.MIMEHeader, rcpt string, addresses []string) string {
	mine := map[string]bool{}
	for _, a := range addresses {
		mine[strings.ToLower(strings.TrimSpace(a))] = true
	}
	for _, h := range []string{"To", "Cc", "Bcc", "Resent-To", "Resent-Cc", "Resent-Bcc"} {
		for _, raw := range hdr.Values(h) {
			list, err := mail.ParseAddressList(raw)
			if err != nil {
				continue
			}
			for _, a := range list {
				la := strings.ToLower(a.Address)
				if la == strings.ToLower(rcpt) {
					return rcpt
				}
				if mine[la] {
					return a.Address
				}
			}
		}
	}
	return rcpt
}

// VacationSubject is the reply subject: :subject, else the configured
// template (%{subject} = the original subject), else the configured
// default when the message has no subject, else "Auto: <subject>".
func VacationSubject(subject, orig, template, dflt string) string {
	if decoded, err := new(mime.WordDecoder).DecodeHeader(orig); err == nil {
		orig = decoded
	}
	if subject != "" {
		return LimitSubject(subject)
	}
	if template != "" && orig != "" {
		return LimitSubject(strings.ReplaceAll(template, "%{subject}", orig))
	}
	if orig == "" {
		if dflt != "" {
			return LimitSubject(dflt)
		}
		return "Automated reply"
	}
	return LimitSubject("Auto: " + orig)
}

// MaxSubjectLen bounds generated Subject headers (characters); longer
// subjects end with an ellipsis.
const MaxSubjectLen = 256

// LimitSubject cuts an over-long generated subject and marks the cut with
// "…" (U+2026).
func LimitSubject(s string) string {
	r := []rune(strings.Join(strings.Fields(s), " "))
	if len(r) <= MaxSubjectLen {
		return string(r)
	}
	return string(r[:MaxSubjectLen-1]) + "\u2026"
}

// VacationToHeader preserves a matching display name without changing the
// envelope destination. ignoreEnvelope affects only the visible To header.
func VacationToHeader(hdr textproto.MIMEHeader, envelope string, ignoreEnvelope bool) string {
	for _, name := range []string{"Sender", "From"} {
		if a, err := mail.ParseAddress(hdr.Get(name)); err == nil && (ignoreEnvelope || strings.EqualFold(a.Address, envelope)) {
			return a.String()
		}
	}
	return (&mail.Address{Address: envelope}).String()
}

// VacationReferences retains only bracketed message IDs and caps the result.
func VacationReferences(hdr textproto.MIMEHeader) string {
	var ids []string
	for _, v := range []string{strings.Join(hdr.Values("References"), " "), hdr.Get("Message-ID")} {
		for _, f := range strings.Fields(v) {
			if strings.HasPrefix(f, "<") && strings.HasSuffix(f, ">") && strings.Contains(f, "@") && len(f) <= 998 {
				ids = append(ids, f)
			}
		}
	}
	for len(strings.Join(ids, " ")) > 4096 && len(ids) > 1 {
		ids = ids[1:]
	}
	return strings.Join(ids, " ")
}

// VacationRecipient applies explicit server configuration to recipient checks.
func VacationRecipient(rcpt, original string, config map[string]string) string {
	if config["sieve_vacation_use_original_recipient"] == "yes" && original != "" {
		rcpt = original
	}
	return rcpt
}

func VacationAddresses(addresses []string, config map[string]string) []string {
	out := append([]string(nil), addresses...)
	if a := config["sieve_user_email"]; a != "" {
		out = append(out, a)
	}
	return out
}
