package tests

import (
	"context"
	"fmt"
	"io/fs"
	"net/mail"
	"net/textproto"
	"os"
	"path/filepath"
	"strings"

	"github.com/foxcpp/go-sieve/interp"
)

// simpleExecuteRuntime is the in-memory mail store and outbox of the
// testsuite runs.
type simpleExecuteRuntime struct {
	smtp      []*interp.ExecuteTestMessage
	mailboxes map[string][]*interp.ExecuteTestMessage
}

// Reset drops the outbox (test_result_reset); the mail store persists.
func (s *simpleExecuteRuntime) Reset() { s.smtp = nil }

// dirFS is a file system rooted at a directory that, unlike os.DirFS,
// follows ".." (the Pigeonhole tests reference ../../examples/).
type dirFS string

func (d dirFS) Open(name string) (fs.File, error) {
	return os.Open(filepath.Join(string(d), filepath.FromSlash(name)))
}

// HasMailbox reports whether test_mailbox_create made the mailbox.
func (s *simpleExecuteRuntime) HasMailbox(name string) bool {
	if strings.EqualFold(name, "INBOX") {
		return true
	}
	_, ok := s.mailboxes[s.canonical(name)]
	return ok
}

// canonical folds the INBOX namespace prefix like Dovecot does.
func (s *simpleExecuteRuntime) canonical(name string) string {
	if len(name) >= 5 && strings.EqualFold(name[:5], "INBOX") {
		return "INBOX" + name[5:]
	}
	return name
}

// MailboxExists is the mailboxexists hook of the test environment.
func (s *simpleExecuteRuntime) MailboxExists(_ context.Context, name string) (bool, error) {
	return s.HasMailbox(name), nil
}

// storedMessage is the copy a delivering action stores: the message with
// its header edits applied and the flags of the action.
func storedMessage(d *interp.RuntimeData, i int, flags []string) *interp.ExecuteTestMessage {
	msg := d.ActionMessage(i)
	if ms, ok := msg.(*interp.MessageStatic); ok {
		c := *ms
		msg = &c
	}
	// Dovecot marks stored mail with $HasAttachment / $HasNoAttachment
	kw := "$HasNoAttachment"
	if ms, ok := msg.(*interp.MessageStatic); ok && strings.Contains(strings.ToLower(string(ms.RawMessage)), "content-disposition: attachment") {
		kw = "$HasAttachment"
	}
	return &interp.ExecuteTestMessage{Envelope: d.Envelope, Message: msg, Flags: append(append([]string{}, flags...), kw)}
}

// redirected is the forwarded copy: the envelope changes and a
// X-Sieve-Redirected-From header names the account it left.
func redirected(d *interp.RuntimeData, i int, to string) *interp.ExecuteTestMessage {
	m := storedMessage(d, i, nil)
	m.Flags = nil
	identity := d.Envelope.EnvelopeTo()
	if u := d.Script.Options().Conf("sieve_user_email"); u != "" {
		identity = u
	}
	for _, v := range headerValues(d, "X-Sieve-Redirected-From") {
		for _, a := range strings.Split(v, ",") {
			a = strings.Trim(strings.TrimSpace(a), "<>")
			if strings.EqualFold(a, identity) || strings.EqualFold(a, d.Envelope.EnvelopeTo()) {
				return nil
			}
		}
	}
	from := d.Envelope.EnvelopeFrom()
	if from == "<>" {
		from = ""
	}
	nullSender := strings.TrimSpace(d.Envelope.EnvelopeFrom()) == "<>"
	switch pol := strings.TrimSpace(d.Script.Options().Conf("sieve_redirect_envelope_from")); pol {
	case "", "sender":
	case "recipient":
		from = d.Envelope.EnvelopeTo()
	case "orig_recipient":
		from = d.Envelope.EnvelopeTo()
		if o, ok := d.Envelope.(interp.OriginalRecipient); ok && o.OriginalRecipient() != "" {
			from = o.OriginalRecipient()
		}
	case "user_email":
		from = d.Envelope.EnvelopeTo()
		if u := d.Script.Options().Conf("sieve_user_email"); u != "" {
			from = u
		}
	default:
		if strings.HasPrefix(pol, "<") && strings.HasSuffix(pol, ">") {
			from = strings.Trim(pol, "<>") // explicit address, "<>" = null
		}
	}
	if nullSender {
		from = "" // a null sender is never replaced (no backscatter)
	}
	m.Envelope = interp.EnvelopeStatic{From: from, To: to, Auth: d.Envelope.AuthUsername()}
	if ms, ok := m.Message.(*interp.MessageStatic); ok {
		ms.RawMessage = append([]byte("X-Sieve-Redirected-From: "+identity+"\r\n"), ms.RawMessage...)
		ms.Size = len(ms.RawMessage)
		hdr := textproto.MIMEHeader{}
		if h, ok := ms.Header.(textproto.MIMEHeader); ok {
			for k, v := range h {
				hdr[k] = v
			}
		}
		hdr.Set("X-Sieve-Redirected-From", identity)
		ms.Header = hdr
	}
	return m
}

func headerMessage(hdr textproto.MIMEHeader, body string) interp.Message {
	var b strings.Builder
	for k, vs := range hdr {
		for _, x := range vs {
			b.WriteString(k + ": " + x + "\r\n")
		}
	}
	b.WriteString("\r\n" + body)
	if !strings.HasSuffix(body, "\n") {
		b.WriteString("\r\n")
	}
	return &interp.MessageStatic{Size: b.Len(), Header: hdr, RawMessage: []byte(b.String())}
}

// rejectBounce is the MDN a reject sends back to the envelope sender
// (nothing for the null sender).
func rejectBounce(d *interp.RuntimeData, reason string) *interp.ExecuteTestMessage {
	sender := strings.TrimSpace(d.Envelope.EnvelopeFrom())
	if sender == "<>" {
		return nil
	}
	sender = strings.Trim(sender, "<>")
	if sender == "" {
		if a, err := mail.ParseAddress(hdr0(d, "From")); err == nil {
			sender = a.Address
		}
	}
	if sender == "" {
		return nil
	}
	rcpt := strings.Trim(strings.TrimSpace(d.Envelope.EnvelopeTo()), "<>")
	hdr := textproto.MIMEHeader{}
	hdr.Set("From", "Mail Delivery Subsystem <postmaster@"+domainOf(rcpt)+">")
	hdr.Set("To", sender)
	hdr.Set("Subject", "Automatically rejected mail")
	hdr.Set("Auto-Submitted", "auto-replied (rejected)")
	hdr.Set("Precedence", "bulk")
	hdr.Set("Content-Type", "multipart/report; report-type=disposition-notification")
	return &interp.ExecuteTestMessage{
		Envelope: interp.EnvelopeStatic{From: "", To: sender},
		Message:  headerMessage(hdr, "Your message was automatically rejected:\r\n"+reason+"\r\n"),
	}
}

func hdr0(d *interp.RuntimeData, name string) string {
	if vals, _ := d.Msg.HeaderGet(name); len(vals) > 0 {
		return vals[0]
	}
	return ""
}

func domainOf(addr string) string {
	if i := strings.LastIndex(addr, "@"); i >= 0 {
		return addr[i+1:]
	}
	return "localhost"
}

// vacationReply composes the auto-reply the way a server would (RFC 5230
// 4.6), or nil when the rules say not to reply.
func vacationReply(d *interp.RuntimeData, v interp.ActionVacation) *interp.ExecuteTestMessage {
	hdr := textproto.MIMEHeader{}
	for _, k := range []string{"From", "Sender", "References", "To", "Cc", "Bcc", "Subject", "Message-ID", "Auto-Submitted", "Precedence", "X-Auto-Response-Suppress", "List-Id", "List-Post", "List-Unsubscribe", "List-Help", "List-Subscribe", "Resent-To", "Resent-Cc", "Resent-Bcc"} {
		if vals, _ := d.Msg.HeaderGet(k); len(vals) > 0 {
			hdr[textproto.CanonicalMIMEHeaderKey(k)] = vals
		}
	}
	rcpt := strings.Trim(strings.TrimSpace(d.Envelope.EnvelopeTo()), "<>")
	original := ""
	if e, ok := d.Envelope.(interp.OriginalRecipient); ok {
		original = e.OriginalRecipient()
	}
	rcpt = interp.VacationRecipient(rcpt, original, d.Script.Options().Config)
	addresses := append(interp.VacationAddresses(v.Addresses, d.Script.Options().Config), rcpt)
	checkRcpt := rcpt
	if d.Script.Options().Conf("sieve_vacation_check_recipient") == "no" {
		checkRcpt = ""
	}
	if ok, _ := interp.VacationShouldReply(hdr, d.Envelope.EnvelopeFrom(), checkRcpt, addresses, v.From); !ok {
		return nil
	}
	from := interp.VacationReplyFrom(hdr, rcpt, addresses)
	if rcpt == "" {
		if a, err := mail.ParseAddress(hdr.Get("To")); err == nil {
			from = a.Address
		}
	}
	if v.From != "" {
		if a, err := mail.ParseAddress(v.From); err == nil {
			from = a.String()
		}
	}
	to := strings.Trim(strings.TrimSpace(d.Envelope.EnvelopeFrom()), "<>")
	if to == "" {
		if a, err := mail.ParseAddress(hdr.Get("From")); err == nil {
			to = a.Address
		}
	}
	conf := d.Script.Options().Conf
	subject := interp.VacationSubject(v.Subject, hdr.Get("Subject"), conf("sieve_vacation_default_subject_template"), conf("sieve_vacation_default_subject"))
	out := textproto.MIMEHeader{}
	out.Set("From", from)
	out.Set("To", interp.VacationToHeader(hdr, to, conf("sieve_vacation_to_header_ignore_envelope") == "yes"))
	out.Set("Subject", subject)
	out.Set("Auto-Submitted", "auto-replied")
	out.Set("Precedence", "bulk")
	out.Set("X-Sieve", "go-sieve")
	if id := hdr.Get("Message-ID"); id != "" {
		out.Set("In-Reply-To", id)
		out.Set("References", interp.VacationReferences(hdr))
	}
	if v.Mime {
		out.Set("MIME-Version", "1.0")
	}
	return &interp.ExecuteTestMessage{
		Envelope: interp.EnvelopeStatic{From: func() string {
			if conf("sieve_vacation_send_from_recipient") == "yes" {
				return rcpt
			}
			return ""
		}(), To: to},
		Message: headerMessage(out, v.Reason),
	}
}

func (s *simpleExecuteRuntime) ExecuteActions(d *interp.RuntimeData, actions []interp.AppliedAction) error {
	if s.mailboxes == nil {
		s.mailboxes = make(map[string][]*interp.ExecuteTestMessage)
	}
	fail := func(err error) error {
		// a failed delivery falls back to the implicit keep, so that no
		// mail is lost (the error is still reported)
		original := *d
		if d.Test.RunOriginal != nil {
			original.Msg = d.Test.RunOriginal
		}
		s.mailboxes[s.GetDefaultMailbox()] = append(s.mailboxes[s.GetDefaultMailbox()], storedMessage(&original, -1, d.Flags))
		return err
	}
	for i, act := range actions {
		switch act := act.(type) {
		case interp.ActionFileInto:
			if strings.ContainsAny(act.Mailbox, "\x00\r\n\t") || strings.Contains(act.Mailbox, "//") {
				return fail(fmt.Errorf("failed to store into mailbox %q: invalid name", act.Mailbox))
			}
			if !s.HasMailbox(act.Mailbox) && !act.Create {
				return fail(fmt.Errorf("failed to store into mailbox %q: mailbox does not exist", act.Mailbox))
			}
			mb := s.canonical(act.Mailbox)
			s.mailboxes[mb] = append(s.mailboxes[mb], storedMessage(d, i, act.Flags))
		case interp.ActionRedirect:
			if m := redirected(d, i, act.Address); m != nil {
				s.smtp = append(s.smtp, m)
			}
		case interp.ActionDiscard:
			continue
		case interp.ActionKeep:
			s.mailboxes[s.GetDefaultMailbox()] = append(s.mailboxes[s.GetDefaultMailbox()], storedMessage(d, i, act.Flags))
		case interp.ActionReject:
			if m := rejectBounce(d, act.Reason); m != nil {
				s.smtp = append(s.smtp, m)
			}
		case interp.ActionEReject:
			if m := rejectBounce(d, act.Reason); m != nil {
				s.smtp = append(s.smtp, m)
			}
		case interp.ActionVacation:
			if m := vacationReply(d, act); m != nil {
				s.smtp = append(s.smtp, m)
			}
		case interp.ActionNotify:
			s.smtp = append(s.smtp, notifications(d, act)...)
		case interp.ActionPipe:
			if d.Program == nil {
				return fmt.Errorf("pipe: no program runner")
			}
			var raw []byte
			if rm, ok := d.ActionMessage(i).(interp.RawMessage); ok {
				raw = rm.Raw()
			}
			res, ok, err := d.Program(context.Background(), interp.ProgramPipe, act.Program, act.Args, raw)
			if err != nil || !ok || res.ExitCode != 0 {
				if !act.Try {
					return fail(fmt.Errorf("pipe %q failed", act.Program))
				}
			}
		default:
			return fmt.Errorf("unknown action type: %T", act)
		}
	}
	return nil
}

func (s *simpleExecuteRuntime) CreateMailbox(name string) error {
	if s.mailboxes == nil {
		s.mailboxes = make(map[string][]*interp.ExecuteTestMessage)
	}
	s.mailboxes[name] = []*interp.ExecuteTestMessage{}
	return nil
}

func (s *simpleExecuteRuntime) GetDefaultMailbox() string {
	return "INBOX"
}

func (s *simpleExecuteRuntime) GetSMTPMessage(index int) (*interp.ExecuteTestMessage, error) {
	if index >= len(s.smtp) {
		return nil, fmt.Errorf("index out of range")
	}

	return s.smtp[index], nil
}

func (s *simpleExecuteRuntime) HasSMTPMessage(index int) (bool, error) {
	return index < len(s.smtp), nil
}

func (s *simpleExecuteRuntime) GetMailboxMessage(mailboxName string, index int) (*interp.ExecuteTestMessage, error) {
	mailbox, ok := s.mailboxes[mailboxName]
	if !ok {
		return nil, fmt.Errorf("mailbox %s not found", mailboxName)
	}
	if index >= len(mailbox) {
		return nil, fmt.Errorf("index out of range")
	}

	return mailbox[index], nil
}

func (s *simpleExecuteRuntime) HasMailboxMessage(mailboxName string, index int) (bool, error) {
	mailbox, ok := s.mailboxes[mailboxName]
	if !ok {
		return false, nil
	}
	return index < len(mailbox), nil
}

// notifications composes the mailto notifications of one notify action,
// one message per recipient (RFC 5436 2.7). Automatic mail is not
// notified about (RFC 5436 2.7.1).
func notifications(d *interp.RuntimeData, n interp.ActionNotify) []*interp.ExecuteTestMessage {
	if n.Mailto == nil {
		return nil
	}
	if as := strings.ToLower(hdr0(d, "Auto-Submitted")); as != "" && as != "no" {
		return nil
	}
	rcpt := strings.Trim(strings.TrimSpace(d.Envelope.EnvelopeTo()), "<>")
	envFrom := "postmaster@" + domainOf(rcpt)
	if n.From != "" {
		envFrom = n.From
	}
	switch strings.TrimSpace(d.Script.Options().Conf("sieve_notify_mailto_envelope_from")) {
	case "sender":
		envFrom = strings.Trim(d.Envelope.EnvelopeFrom(), "<>")
	case "recipient":
		envFrom = rcpt
	case "user_email":
		if u := d.Script.Options().Conf("sieve_user_email"); u != "" {
			envFrom = u
		}
	}
	if strings.TrimSpace(d.Envelope.EnvelopeFrom()) == "<>" {
		envFrom = "" // a null sender stays null
	}
	hdr := textproto.MIMEHeader{}
	from := n.From
	if from == "" {
		from = "Postmaster <postmaster@" + domainOf(rcpt) + ">"
	}
	hdr.Set("From", from)
	if len(n.Mailto.To) > 0 {
		hdr.Set("To", strings.Join(n.Mailto.To, ", "))
	}
	if len(n.Mailto.Cc) > 0 {
		hdr.Set("Cc", strings.Join(n.Mailto.Cc, ", "))
	}
	subject := n.Mailto.Subject
	if subject == "" {
		if n.Message != "" {
			subject = n.Message
		} else {
			subject = "Notification: " + hdr0(d, "Subject")
		}
	}
	hdr.Set("Subject", interp.LimitSubject(subject))
	hdr.Set("Auto-Submitted", "auto-notified; owner-email=\""+rcpt+"\"")
	hdr.Set("X-Sieve", "go-sieve")
	switch n.Importance {
	case "1":
		hdr.Set("X-Priority", "1 (Highest)")
		hdr.Set("Importance", "High")
	case "3":
		hdr.Set("X-Priority", "5 (Lowest)")
		hdr.Set("Importance", "Low")
	default:
		hdr.Set("X-Priority", "3 (Normal)")
		hdr.Set("Importance", "Normal")
	}
	for _, h := range n.Mailto.Headers {
		hdr.Add(h[0], h[1])
	}
	body := n.Mailto.Body
	if body == "" {
		body = n.Message
	}
	if body == "" {
		body = "Notification of new message from " + hdr0(d, "From") + ": " + hdr0(d, "Subject")
	}
	var out []*interp.ExecuteTestMessage
	for _, r := range n.Mailto.Recipients() {
		out = append(out, &interp.ExecuteTestMessage{
			Envelope: interp.EnvelopeStatic{From: envFrom, To: r},
			Message:  headerMessage(hdr, body),
		})
	}
	return out
}

func headerValues(d *interp.RuntimeData, name string) []string {
	v, _ := d.Msg.HeaderGet(name)
	return v
}
