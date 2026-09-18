package interp

import (
	"context"
	"encoding/gob"
	"fmt"
	"net/mail"
	"net/url"
	"strconv"
	"strings"

	"github.com/foxcpp/go-sieve/parser"
)

// enotify (RFC 5435) with the mailto method (RFC 5436), shardpost fork.
// The action carries the parsed mailto URI; the server composes and sends
// the notification (RFC 5436 2.7). Configuration (Pigeonhole names):
//
//	sieve_notify_max_notifications   per script (default 10, 0 forbids notify)
//	sieve_notify_mailto_envelope_from sender | recipient | orig_recipient | user_email | <address>

// ActionNotify is a requested notification.
type ActionNotify struct {
	Method     string // the URI as given
	From       string // :from
	Importance string // "1" (high), "2" (normal, default), "3" (low)
	Message    string // :message
	Options    []string
	Mailto     *MailtoURI
}

func (ActionNotify) testActionName() string    { return "notify" }
func (ActionNotify) cancelsImplicitKeep() bool { return false }

// MailtoURI is a parsed mailto: URI (RFC 6068).
type MailtoURI struct {
	To      []string
	Cc      []string
	Bcc     []string
	Subject string
	Body    string
	Headers [][2]string // other headers, in order
}

// Recipients returns To, Cc and Bcc in that order.
func (m *MailtoURI) Recipients() []string {
	out := append([]string{}, m.To...)
	out = append(out, m.Cc...)
	return append(out, m.Bcc...)
}

var disallowedMailtoHeaders = map[string]bool{"received": true, "auto-submitted": true, "message-id": true, "date": true, "return-path": true, "delivered-to": true}

func validMailAddress(a string) bool {
	if a == "" {
		return false
	}
	if _, err := mail.ParseAddress(a); err != nil {
		return false
	}
	return strings.Contains(a, "@")
}

// ParseMailto parses "mailto:addr[,addr]*[?hdr=val[&hdr=val]*]".
func ParseMailto(uri string) (*MailtoURI, error) {
	if len(uri) < 7 || !strings.EqualFold(uri[:7], "mailto:") {
		return nil, fmt.Errorf("not a mailto URI")
	}
	rest := uri[7:]
	m := &MailtoURI{}
	addrPart, query, hasQuery := strings.Cut(rest, "?")
	addrPart, err := url.PathUnescape(addrPart)
	if err != nil {
		return nil, fmt.Errorf("mailto: bad escape in address")
	}
	for _, a := range strings.Split(addrPart, ",") {
		a = strings.TrimSpace(a)
		if a == "" {
			continue
		}
		if !validMailAddress(a) {
			return nil, fmt.Errorf("mailto: invalid address %q", a)
		}
		m.To = append(m.To, a)
	}
	if hasQuery {
		for _, kv := range strings.Split(query, "&") {
			if kv == "" {
				continue
			}
			k, v, _ := strings.Cut(kv, "=")
			k, err1 := url.QueryUnescape(strings.ReplaceAll(k, "+", "%2B"))
			v, err2 := url.QueryUnescape(strings.ReplaceAll(v, "+", "%2B"))
			if err1 != nil || err2 != nil {
				return nil, fmt.Errorf("mailto: bad escape in header")
			}
			if !validHeaderName(k) {
				return nil, fmt.Errorf("mailto: invalid header name %q", k)
			}
			lk := strings.ToLower(k)
			if disallowedMailtoHeaders[lk] {
				return nil, fmt.Errorf("mailto: header %q not allowed", k)
			}
			switch lk {
			case "to", "cc", "bcc":
				for _, a := range strings.Split(v, ",") {
					a = strings.TrimSpace(a)
					if a == "" {
						continue
					}
					if !validMailAddress(a) {
						return nil, fmt.Errorf("mailto: invalid %s address %q", k, a)
					}
					switch lk {
					case "to":
						m.To = append(m.To, a)
					case "cc":
						m.Cc = append(m.Cc, a)
					default:
						m.Bcc = append(m.Bcc, a)
					}
				}
			case "subject":
				m.Subject = v
			case "body":
				m.Body = v
			default:
				if strings.ContainsAny(v, "\r\n") {
					return nil, fmt.Errorf("mailto: invalid header value")
				}
				m.Headers = append(m.Headers, [2]string{k, v})
			}
		}
	}
	if len(m.To) == 0 {
		return nil, fmt.Errorf("mailto: no recipient")
	}
	return m, nil
}

// ValidNotifyMethod tells whether a notification URI is supported and well
// formed (only mailto is supported).
func ValidNotifyMethod(uri string) bool {
	_, err := ParseMailto(uri)
	return err == nil
}

type CmdNotify struct {
	Method     string
	From       string
	Importance string
	Message    string
	Options    []string
}

func notifyLimit(o *Options) int {
	if v := o.Conf("sieve_notify_max_notifications"); v != "" {
		n, err := strconv.Atoi(v)
		if err == nil {
			return n
		}
	}
	return 10
}

func loadNotify(s *Script, pcmd parser.Cmd) (Cmd, error) {
	if !s.RequiresExtension("enotify") {
		return nil, parser.ErrorAt(pcmd.Position, "missing require 'enotify'")
	}
	if notifyLimit(s.opts) == 0 {
		return nil, parser.ErrorAt(pcmd.Position, "local policy prohibits the use of a notify action")
	}
	cmd := CmdNotify{Importance: "2"}
	err := LoadSpec(s, &Spec{
		Tags: map[string]SpecTag{
			"from":       {NeedsValue: true, MinStrCount: 1, MaxStrCount: 1, MatchStr: func(v []string) { cmd.From = v[0] }},
			"importance": {NeedsValue: true, MinStrCount: 1, MaxStrCount: 1, NoVariables: true, MatchStr: func(v []string) { cmd.Importance = v[0] }},
			"message":    {NeedsValue: true, MinStrCount: 1, MaxStrCount: 1, MatchStr: func(v []string) { cmd.Message = v[0] }},
			"options":    {NeedsValue: true, MinStrCount: 1, MatchStr: func(v []string) { cmd.Options = v }},
		},
		Pos: []SpecPosArg{{MinStrCount: 1, MaxStrCount: 1, MatchStr: func(v []string) { cmd.Method = v[0] }}},
	}, pcmd.Position, pcmd.Args, pcmd.Tests, pcmd.Block)
	if err != nil {
		return nil, err
	}
	if cmd.Importance != "1" && cmd.Importance != "2" && cmd.Importance != "3" {
		return nil, parser.ErrorAt(pcmd.Position, "notify: :importance must be 1, 2 or 3")
	}
	if len(usedVars(s, cmd.Method)) == 0 {
		if _, err := ParseMailto(cmd.Method); err != nil {
			return nil, parser.ErrorAt(pcmd.Position, "notify: %v", err)
		}
	}
	if cmd.From != "" && len(usedVars(s, cmd.From)) == 0 && !validMailAddress(cmd.From) {
		return nil, parser.ErrorAt(pcmd.Position, "notify: invalid :from address %q", cmd.From)
	}
	for _, o := range cmd.Options {
		if len(usedVars(s, o)) == 0 && !strings.Contains(o, "=") {
			return nil, parser.ErrorAt(pcmd.Position, "notify: :options entries are name=value")
		}
	}
	return cmd, nil
}

func (c CmdNotify) Execute(ctx context.Context, d *RuntimeData) error {
	method := expandVars(d, c.Method)
	m, err := ParseMailto(method)
	if err != nil {
		return fmt.Errorf("notify: %v", err)
	}
	from := expandVars(d, c.From)
	if from != "" && !validMailAddress(from) {
		from = "" // an invalid :from is ignored (RFC 5435 3.3)
	}
	count := 0
	seen := map[string]bool{}
	for _, a := range d.AppliedActions {
		if n, ok := a.(ActionNotify); ok {
			count++
			if n.Mailto != nil {
				for _, r := range n.Mailto.Recipients() {
					seen[strings.ToLower(r)] = true
				}
			}
		}
	}
	if count >= notifyLimit(d.Script.opts) {
		return fmt.Errorf("number of notify actions exceeds policy limit")
	}
	// duplicate recipients across notify actions get one notification
	dedupe := func(l []string) []string {
		var out []string
		for _, r := range l {
			k := strings.ToLower(r)
			if seen[k] {
				continue
			}
			seen[k] = true
			out = append(out, r)
		}
		return out
	}
	m.To, m.Cc, m.Bcc = dedupe(m.To), dedupe(m.Cc), dedupe(m.Bcc)
	return d.OnAction(ctx, ActionNotify{
		Method:     method,
		From:       from,
		Importance: c.Importance,
		Message:    expandVars(d, c.Message),
		Options:    expandVarsList(d, c.Options),
		Mailto:     m,
	}, d)
}

// ValidNotifyMethodTest is valid_notify_method (RFC 5435 5).
type ValidNotifyMethodTest struct{ URIs []string }

func (t ValidNotifyMethodTest) Check(_ context.Context, d *RuntimeData) (bool, error) {
	for _, u := range t.URIs {
		if !ValidNotifyMethod(expandVars(d, u)) {
			return false, nil
		}
	}
	return true, nil
}

func loadValidNotifyMethodTest(s *Script, test parser.Test) (Test, error) {
	if !s.RequiresExtension("enotify") {
		return nil, parser.ErrorAt(test.Position, "missing require 'enotify'")
	}
	loaded := ValidNotifyMethodTest{}
	err := LoadSpec(s, &Spec{Pos: []SpecPosArg{{MinStrCount: 1, MatchStr: func(v []string) { loaded.URIs = v }}}}, test.Position, test.Args, test.Tests, nil)
	return loaded, err
}

// NotifyMethodCapabilityTest is notify_method_capability (RFC 5435 6):
// for mailto the "online" capability is "maybe" (RFC 5436 2.4).
type NotifyMethodCapabilityTest struct {
	matcherTest
	URI        string
	Capability string
}

func (t NotifyMethodCapabilityTest) Check(_ context.Context, d *RuntimeData) (bool, error) {
	uri := expandVars(d, t.URI)
	if !ValidNotifyMethod(uri) {
		return false, nil
	}
	if !strings.EqualFold(expandVars(d, t.Capability), "online") {
		return false, nil
	}
	if t.isCount() {
		return t.countMatches(d, 1), nil
	}
	return t.tryMatch(d, "maybe")
}

func loadNotifyMethodCapabilityTest(s *Script, test parser.Test) (Test, error) {
	if !s.RequiresExtension("enotify") {
		return nil, parser.ErrorAt(test.Position, "missing require 'enotify'")
	}
	loaded := NotifyMethodCapabilityTest{matcherTest: newMatcherTest()}
	var key []string
	spec := loaded.addSpecTags(&Spec{Pos: []SpecPosArg{
		{MinStrCount: 1, MaxStrCount: 1, MatchStr: func(v []string) { loaded.URI = v[0] }},
		{MinStrCount: 1, MaxStrCount: 1, MatchStr: func(v []string) { loaded.Capability = v[0] }},
		{MinStrCount: 1, MatchStr: func(v []string) { key = v }},
	}})
	if err := LoadSpec(s, spec, test.Position, test.Args, test.Tests, nil); err != nil {
		return nil, err
	}
	if err := loaded.setKey(s, key); err != nil {
		return nil, err
	}
	return loaded, nil
}

// EncodeURL is the :encodeurl modifier (RFC 5435 7): percent-encoding of
// everything but the unreserved characters.
func EncodeURL(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '.' || c == '_' || c == '~' {
			b.WriteByte(c)
		} else {
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

func init() {
	gob.Register(ActionNotify{})
	gob.Register(CmdNotify{})
	gob.Register(ValidNotifyMethodTest{})
	gob.Register(NotifyMethodCapabilityTest{})
}
