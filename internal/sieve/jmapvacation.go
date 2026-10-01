package sieve

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"mime/quotedprintable"
	"strconv"
	"strings"
	"time"

	"shardpost/internal/model"
)

// The vacation of the JMAP VacationResponse (RFC 8621 8). Its source of
// truth is a METADATA entry of the account (JMAPVacationEntry), which
// only the JMAP front-end writes. Delivery generates the Sieve script
// from that entry and runs it after the active script, for its vacation
// alone, when the active script did not answer the message itself. So a
// vacation a user keeps in the active script (Roundcube's managesieve
// plugin) wins, and a user script is never rewritten.
//
// The same script is also stored in the account's script store under a
// reserved name, so that a ManageSieve client can show the setting. That
// copy is for display only: delivery never reads it. ManageSieve and the
// admin API show it and refuse to write, rename, delete or activate it.

// JMAPVacationScript is the name of the script the JMAP VacationResponse
// writes.
const JMAPVacationScript = model.JMAPVacationScript

// JMAPVacationEntry is the METADATA entry (of the server, private to the
// account) that holds the JMAP VacationResponse as JSON.
const JMAPVacationEntry = "/private/vendor/shardpost/jmap/vacation"

// DefaultVacationText is the body of a reply the client gave none for:
// RFC 8621 8 asks for an appropriate default rather than an empty reply.
const DefaultVacationText = "This is an automatic reply. I am away and will read your message when I am back."

// JMAPVacation is the JMAP VacationResponse as it is kept in
// JMAPVacationEntry: the object without its id. The dates are UTCDates.
type JMAPVacation struct {
	FromDate  *string `json:"fromDate"`
	ToDate    *string `json:"toDate"`
	Subject   *string `json:"subject"`
	TextBody  *string `json:"textBody"`
	HTMLBody  *string `json:"htmlBody"`
	IsEnabled bool    `json:"isEnabled"`
}

// ParseJMAPVacation reads a stored JMAP VacationResponse.
func ParseJMAPVacation(raw string) (*JMAPVacation, error) {
	v := &JMAPVacation{}
	if err := json.Unmarshal([]byte(raw), v); err != nil {
		return nil, err
	}

	return v, nil
}

// Settings are what the script is written from. A date that does not
// read is refused, so that a damaged record answers nobody rather than
// everybody; without a body the reply takes DefaultVacationText.
func (v *JMAPVacation) Settings() (VacationSettings, error) {
	var s VacationSettings

	for _, d := range []struct {
		val  *string
		into **time.Time
	}{{v.FromDate, &s.FromDate}, {v.ToDate, &s.ToDate}} {
		if d.val == nil {
			continue
		}

		t, err := time.Parse(time.RFC3339, *d.val)
		if err != nil {
			return s, err
		}

		*d.into = &t
	}

	s.Subject, s.TextBody, s.HTMLBody = orEmpty(v.Subject), orEmpty(v.TextBody), orEmpty(v.HTMLBody)

	if s.TextBody == "" && s.HTMLBody == "" {
		s.TextBody = DefaultVacationText
	}

	return s, nil
}

// orEmpty is a String|null as a string, "" for null.
func orEmpty(s *string) string {
	if s == nil {
		return ""
	}

	return *s
}

// ReservedScript reports whether a script name belongs to the server:
// clients may read the script, not change it.
func ReservedScript(name string) bool { return model.ReservedScript(name) }

// DescReservedScript is what a client that tries to change the reserved
// script is told.
const DescReservedScript = "the script " + JMAPVacationScript + " is reserved for the JMAP VacationResponse"

// VacationSettings are what a JMAP VacationResponse says, as far as the
// script needs it. Nil dates leave that end of the window open; an empty
// subject lets the reply take the server's default subject.
type VacationSettings struct {
	FromDate *time.Time
	ToDate   *time.Time
	Subject  string
	TextBody string
	HTMLBody string
}

// vacationDays is the period one sender is answered once in, the RFC 5230
// default the rest of the server uses as well.
const vacationDays = DefaultVacationSeconds / 86400

// VacationScript is the Sieve script of the settings. The reply is sent
// to a sender once per period and per version of the settings: the handle
// changes with them, so a sender answered before a change is answered
// again with what is new. Every text is written as a quoted string, so
// nothing a client puts into it can end the string and become script.
func VacationScript(v VacationSettings) string {
	var b strings.Builder

	b.WriteString("# Written by ShardPost from the JMAP VacationResponse of this account.\n")
	b.WriteString("# Changes are made over JMAP; this script is replaced with every change.\n")

	var window []string

	if v.FromDate != nil {
		window = append(window, currentDate("ge", *v.FromDate))
	}

	if v.ToDate != nil {
		window = append(window, currentDate("lt", *v.ToDate))
	}

	if len(window) > 0 {
		b.WriteString(`require ["date", "relational", "vacation"];` + "\n\n")
	} else {
		b.WriteString(`require ["vacation"];` + "\n\n")
	}

	action := vacationAction(v)

	switch len(window) {
	case 0:
		b.WriteString(action + "\n")
	case 1:
		b.WriteString("if " + window[0] + " {\n    " + action + "\n}\n")
	default:
		b.WriteString("if allof (" + strings.Join(window, ",\n          ") + ") {\n    " + action + "\n}\n")
	}

	return b.String()
}

// currentDate is the test of one end of the window. Both sides are ISO
// 8601 in UTC, which compare as strings in the order of time.
func currentDate(relation string, t time.Time) string {
	return `currentdate :zone "+0000" :value "` + relation + `" "iso8601" ` +
		QuoteString(t.UTC().Format("2006-01-02T15:04:05-07:00"))
}

// vacationAction is the vacation command of the settings.
func vacationAction(v VacationSettings) string {
	parts := []string{actionVacation, ":days", strconv.Itoa(vacationDays), ":handle", QuoteString(vacationHandle(v))}

	if v.Subject != "" {
		parts = append(parts, ":subject", QuoteString(v.Subject))
	}

	if v.HTMLBody == "" {
		return strings.Join(append(parts, QuoteString(v.TextBody)), " ") + ";"
	}

	parts = append(parts, ":mime", QuoteString(vacationEntity(v)))

	return strings.Join(parts, " ") + ";"
}

// vacationHandle names the reply for the duplicate suppression: the
// script's name and a hash of the settings.
func vacationHandle(v VacationSettings) string {
	h := sha256.New()

	for _, t := range []*time.Time{v.FromDate, v.ToDate} {
		if t != nil {
			h.Write([]byte(t.UTC().Format(time.RFC3339)))
		}

		h.Write([]byte{0})
	}

	for _, s := range []string{v.Subject, v.TextBody, v.HTMLBody} {
		h.Write([]byte(s))
		h.Write([]byte{0})
	}

	return JMAPVacationScript + "-" + hex.EncodeToString(h.Sum(nil)[:6])
}

// vacationEntity is the MIME entity of a reply with an HTML body (RFC
// 5230 5.8): the text and the HTML as alternatives, or the HTML alone
// when there is no text. Quoted-printable keeps every line short, so a
// long paragraph survives the relay.
func vacationEntity(v VacationSettings) string {
	if v.TextBody == "" {
		return textPart("html", v.HTMLBody)
	}

	boundary := "=_" + vacationHandle(v)

	return "Content-Type: multipart/alternative; boundary=\"" + boundary + "\"\n\n" +
		"--" + boundary + "\n" + textPart("plain", v.TextBody) + "\n" +
		"--" + boundary + "\n" + textPart("html", v.HTMLBody) + "\n" +
		"--" + boundary + "--\n"
}

// textPart is one text part, quoted-printable.
func textPart(subtype, text string) string {
	var body bytes.Buffer

	w := quotedprintable.NewWriter(&body)
	_, _ = w.Write([]byte(normalizeNewlines(text)))
	_ = w.Close()

	return "Content-Type: text/" + subtype + "; charset=utf-8\nContent-Transfer-Encoding: quoted-printable\n\n" +
		body.String() + "\n"
}

// QuoteString writes a text as a Sieve quoted string (RFC 5228 2.4.2):
// a backslash and a double quote are escaped, and every line ends in a
// plain newline, which the script reads back as CRLF. Nothing inside can
// end the string.
func QuoteString(s string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(normalizeNewlines(s)) + `"`
}

// normalizeNewlines turns CRLF and a lone CR into LF: a lone CR is not
// allowed in a Sieve script.
func normalizeNewlines(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(s, "\r\n", "\n"), "\r", "\n")
}
