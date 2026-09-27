package sieve

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"mime/quotedprintable"
	"strconv"
	"strings"
	"time"
)

// The vacation of the JMAP VacationResponse (RFC 8621 8) is a Sieve
// script the server writes into the account's script store under a
// reserved name. It is never the active script: delivery runs it after
// the active one, for its vacation alone, when the active script did not
// answer the message itself. So a vacation a user keeps in the active
// script (Roundcube's managesieve plugin) wins, and a user script is
// never rewritten. ManageSieve and the admin API show the script, and
// refuse to write, rename, delete or activate it.

// JMAPVacationScript is the name of the script the JMAP VacationResponse
// writes.
const JMAPVacationScript = "jmap-vacation"

// ReservedScript reports whether a script name belongs to the server:
// clients may read the script, not change it.
func ReservedScript(name string) bool { return name == JMAPVacationScript }

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
	parts := []string{"vacation", ":days", strconv.Itoa(vacationDays), ":handle", QuoteString(vacationHandle(v))}

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
