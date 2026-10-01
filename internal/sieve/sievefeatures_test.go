package sieve

import (
	"bufio"
	"context"
	"encoding/base64"
	"fmt"
	"net/textproto"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"shardpost/internal/telemetry"
)

// TestVacationPeriodSettings: :days and :seconds are clamped into
// [sieve_vacation_min_period, sieve_vacation_max_period], the default
// applies without either, a maximum of 0 is no maximum and a minimum of
// 0 allows a period of 0.
func TestVacationPeriodSettings(t *testing.T) {
	day := 24 * time.Hour
	cases := []struct {
		name    string
		seconds int
		set     bool
		cfg     map[string]string
		want    time.Duration
	}{
		{"default", 0, false, nil, 7 * day},
		{":days 0 clamped to 1d", 0, true, nil, day},
		{":seconds 60 clamped to 1d", 60, true, nil, day},
		{":days 3", 3 * 86400, true, nil, 3 * day},
		{":days 90 clamped to 60d", 90 * 86400, true, nil, 60 * day},
		{"min 0 allows 0", 0, true, map[string]string{"sieve_vacation_min_period": "0"}, 0},
		{"min 1s", 1, true, map[string]string{"sieve_vacation_min_period": "1s"}, time.Second},
		{"max 0 is no maximum", 400 * 86400, true, map[string]string{"sieve_vacation_max_period": "0"}, 400 * day},
		{"configured default", 0, false, map[string]string{"sieve_vacation_default_period": "2d"}, 2 * day},
		{"long units", 0, false, map[string]string{"sieve_vacation_default_period": "2 weeks"}, 14 * day},
		{"Go duration", 0, false, map[string]string{"sieve_vacation_default_period": "36h"}, 36 * time.Hour},
		{"contradicting settings fall back", 0, false, map[string]string{"sieve_vacation_min_period": "10d", "sieve_vacation_max_period": "2d"}, 7 * day},
		{"unparsable settings fall back", 0, true, map[string]string{"sieve_vacation_min_period": "soon"}, day},
	}

	for _, c := range cases {
		v := &Vacation{Seconds: c.seconds, PeriodSet: c.set, Config: c.cfg}
		if got := v.Period(); got != c.want {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
	}
}

// TestCheckSettings: settings that cannot work are reported.
func TestCheckSettings(t *testing.T) {
	good := []map[string]string{
		nil,
		{"sieve_vacation_min_period": "0", "sieve_vacation_default_period": "0"},
		{"sieve_reject_mode": "protocol", "postmaster_address": "mailer-daemon@example.org"},
		{"sieve_vacation_max_period": "0", "sieve_vacation_default_period": "100d"},
	}
	for _, cfg := range good {
		if err := CheckSettings(cfg); err != nil {
			t.Errorf("%v: %v", cfg, err)
		}
	}

	bad := []map[string]string{
		{"sieve_vacation_min_period": "x"},
		{"sieve_vacation_min_period": "3d", "sieve_vacation_max_period": "2d"},
		{"sieve_vacation_default_period": "90d"},
		{"sieve_vacation_min_period": "8d"},
		{"sieve_reject_mode": "bounce"},
		{"postmaster_address": "Postmaster <pm@example.org>"},
	}
	for _, cfg := range bad {
		if err := CheckSettings(cfg); err == nil {
			t.Errorf("%v accepted", cfg)
		}
	}
}

// TestRejectAndEReject: the delivery tells reject from ereject.
func TestRejectAndEReject(t *testing.T) {
	raw := []byte("From: a@example.org\r\n\r\nbody\r\n")

	for script, ereject := range map[string]bool{`require "reject"; reject "r";`: false, `require "ereject"; ereject "r";`: true} {
		d, err := Run(context.Background(), script, "a@example.org", "u@example.org", "u@example.org", raw)
		if err != nil || d.Rejected != "r" || d.EReject != ereject {
			t.Errorf("%s: %+v %v", script, d, err)
		}
	}
}

// TestComposeRejection checks the MDN of a reject (RFC 5429 2.1, RFC
// 8098): the parts, the encoded subject and the original header only.
func TestComposeRejection(t *testing.T) {
	original := []byte("From: s@example.net\nSubject: =?utf-8?q?gr=C3=BC=C3=9Fe?=\nMessage-ID: <m1@example.net>\n\nsecret body\n")
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

	to, raw := ComposeRejection(Rejection{Reason: "line one\nline two", Original: original, EnvelopeFrom: "<s@example.net>",
		Rcpt: "u@example.org"}, nil, now)
	msg := string(raw)

	if to != "s@example.net" {
		t.Fatalf("to %q", to)
	}

	for _, want := range []string{
		"Subject: =?utf-8?q?Rejected:_gr=C3=BC=C3=9Fe?=\r\n",
		"Date: Thu, 01 Oct 2026 12:00:00 +0000\r\n",
		"line one\r\nline two\r\n",
		"Reporting-UA: example.org; ShardPost\r\n",
		"Content-Type: text/rfc822-headers\r\nContent-Transfer-Encoding: 7bit\r\n\r\nFrom: s@example.net\r\nSubject: =?utf-8?q?gr=C3=BC=C3=9Fe?=\r\nMessage-ID: <m1@example.net>\r\n\r\n--",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("lacks %q:\n%s", want, msg)
		}
	}

	if strings.Contains(msg, "secret body") || strings.Contains(msg, "\n\n") || strings.Count(msg, "\n") != strings.Count(msg, "\r\n") {
		t.Errorf("body or bare line feeds in the MDN:\n%q", msg)
	}

	// The three parts and the closing delimiter.
	head, _, _ := strings.Cut(msg, "\r\n\r\n")
	hdr, _ := textproto.NewReader(bufioString(head + "\r\n\r\n")).ReadMIMEHeader()

	_, params, _ := strings.Cut(hdr.Get("Content-Type"), "boundary=")
	boundary := strings.Trim(params, `"`)

	if boundary == "" || strings.Count(msg, "--"+boundary+"\r\n") != 3 || !strings.HasSuffix(msg, "--"+boundary+"--\r\n") {
		t.Errorf("parts of boundary %q:\n%s", boundary, msg)
	}
}

// TestLongLinesFolded: a long References field is folded, a long reason
// line wrapped, in rejections and vacation replies alike; no line of the
// generated messages exceeds 998 octets.
func TestLongLinesFolded(t *testing.T) {
	var refs []string
	for i := range 60 {
		refs = append(refs, fmt.Sprintf("<%d.reference@example.org>", i))
	}

	// The sender folded its References, as a conforming one does.
	original := "From: s@example.net\r\nMessage-ID: <m@example.net>\r\nReferences: " + strings.Join(refs, "\r\n ") + "\r\nTo: u@example.org\r\nSubject: x\r\n\r\nbody\r\n"
	reason := strings.Repeat("word ", 400) + strings.Repeat("ü", 600)

	_, mdn := ComposeRejection(Rejection{Reason: reason, Original: []byte(original), EnvelopeFrom: "s@example.net", Rcpt: "u@example.org"}, nil, time.Now())

	hdr := textproto.MIMEHeader{"Message-Id": {"<m@example.net>"}, "References": {strings.Join(refs, " ")}, "To": {"u@example.org"}}
	_, reply := (&Vacation{Reason: reason}).Compose(hdr, "s@example.net", "u@example.org", time.Now())

	for name, msg := range map[string]string{"rejection": string(mdn), "vacation": string(reply)} {
		for _, l := range strings.Split(msg, "\r\n") {
			if len(l) > maxLine {
				t.Errorf("%s: a line of %d octets", name, len(l))
			}

			if !utf8.ValidString(l) {
				t.Errorf("%s: a line cut inside a character", name)
			}
		}

		if !strings.Contains(msg, "<59.reference@example.org> <m@example.net>") {
			t.Errorf("%s: references lost", name)
		}
	}

	if got := foldField("References", "<a> <b>"); got != "References: <a> <b>\r\n" {
		t.Errorf("short field folded: %q", got)
	}
}

// TestReportDomain: a rejection for a recipient without a domain names
// the configured host, then a literal postmaster address, never
// localhost.
func TestReportDomain(t *testing.T) {
	cases := []struct {
		rcpt, host string
		cfg        map[string]string
		want       string
	}{
		{"u@example.org", "mx.example.net", nil, "example.org"},
		{"alice", "mx.example.net", nil, "mx.example.net"},
		{"alice", "", map[string]string{"postmaster_address": "pm@example.com"}, "example.com"},
		{"alice", "", nil, fallbackDomain},
	}

	for _, c := range cases {
		if got := reportDomain(c.rcpt, c.host, c.cfg); got != c.want {
			t.Errorf("%q %q: %q, want %q", c.rcpt, c.host, got, c.want)
		}
	}

	_, mdn := ComposeRejection(Rejection{Reason: "no", Original: []byte("From: s@example.net\r\n\r\nx\r\n"), EnvelopeFrom: "s@example.net", Rcpt: "alice"}, nil, time.Now())
	if strings.Contains(string(mdn), "localhost") || !strings.Contains(string(mdn), "Reporting-UA: "+fallbackDomain+"; ShardPost") {
		t.Errorf("bare recipient:\n%s", mdn)
	}
}

// TestShouldReject: no rejection message for the null sender or
// automatic mail.
func TestShouldReject(t *testing.T) {
	cases := []struct {
		from, autoSubmitted string
		want                bool
	}{
		{"a@example.org", "", true},
		{"a@example.org", "no", true},
		{"a@example.org", "No (comment)", true},
		{"", "", false},
		{"<>", "", false},
		{"a@example.org", "auto-replied", false},
		{"a@example.org", "auto-generated; owner-email=x@y", false},
	}

	for _, c := range cases {
		hdr := textproto.MIMEHeader{}
		if c.autoSubmitted != "" {
			hdr.Set("Auto-Submitted", c.autoSubmitted)
		}

		if got, _ := ShouldReject(hdr, c.from); got != c.want {
			t.Errorf("%q %q: %v", c.from, c.autoSubmitted, got)
		}
	}

	// The rules every automatic answer follows (RFC 3834).
	for name, hdr := range map[string]textproto.MIMEHeader{
		"precedence bulk": {"Precedence": {"bulk"}},
		"precedence list": {"Precedence": {"List"}},
		"list id":         {"List-Id": {"<l.example.org>"}},
		"list post":       {"List-Post": {"<mailto:l@example.org>"}},
		"suppress oof":    {"X-Auto-Response-Suppress": {"DR, OOF"}},
	} {
		if ok, _ := ShouldReject(hdr, "a@example.org"); ok {
			t.Errorf("%s: a rejection message would go out", name)
		}
	}

	for _, sender := range []string{"MAILER-DAEMON@example.org", "owner-list@example.org", "list-request@example.org", "noreply@example.org"} {
		if ok, _ := ShouldReject(textproto.MIMEHeader{}, sender); ok {
			t.Errorf("%s: a rejection message would go out", sender)
		}
	}
}

// TestParseSettingsEscape: a backslash before a semicolon keeps it in the
// value; other backslashes stay as they are.
func TestParseSettingsEscape(t *testing.T) {
	cfg := ParseSettings(`rejection_reason=No\; really not.%n%r;sieve_reject_mode=mdn;x=a\.b;y=ends in\\;z=a\\\;b`)

	if cfg["rejection_reason"] != "No; really not.%n%r" || cfg["sieve_reject_mode"] != RejectMDN || cfg["x"] != `a\.b` {
		t.Fatalf("%q %q %q", cfg["rejection_reason"], cfg["sieve_reject_mode"], cfg["x"])
	}

	// Two backslashes are one: a value can end in a backslash, and one
	// can stand before a literal semicolon.
	if cfg["y"] != `ends in\` || cfg["z"] != `a\;b` {
		t.Fatalf("%q %q", cfg["y"], cfg["z"])
	}

	if cfg["sieve_spamtest_status_header"] != DefaultConfig()["sieve_spamtest_status_header"] {
		t.Fatal("the defaults are gone")
	}
}

// TestRejectConflictInSequence: a script that rejects after an earlier
// one filed a copy fails with an action conflict and leaves the keep.
func TestRejectConflictInSequence(t *testing.T) {
	raw := []byte("From: a@example.org\r\n\r\nbody\r\n")
	scripts := []Script{
		{Name: "before/1", Text: `require ["fileinto", "copy"]; fileinto :copy "X";`},
		{Name: PersonalScript, Text: `require "reject"; reject "no";`},
	}

	d, failed := RunSequence(context.Background(), scripts, "a@example.org", "u@example.org", "u@example.org", raw, Hooks{})
	if len(failed) != 1 || failed[0].Script != PersonalScript || !strings.Contains(failed[0].Error(), "action conflict") {
		t.Fatalf("failed %v", failed)
	}

	if d.Rejected != "" || len(d.FileInto) != 2 {
		t.Fatalf("%+v", d)
	}
}

// TestFccArguments: :fcc with its companions compiles where RFC 8580
// allows it, needs its extensions, and reaches the actions.
func TestFccArguments(t *testing.T) {
	ok := []string{
		`require ["vacation", "fcc"]; vacation :fcc "Sent" "away";`,
		`require ["vacation", "fcc", "mailbox", "imap4flags", "special-use"]; vacation :fcc "Sent" :create :flags ["\\Seen"] :specialuse "\\Sent" "away";`,
		`require ["enotify", "fcc", "mailbox"]; notify :fcc "Notes" :create "mailto:a@example.org";`,
		`require ["enotify", "fcc", "variables"]; set "box" "Notes"; notify :fcc "${box}" "mailto:a@example.org";`,
	}
	for _, s := range ok {
		if err := Compile(s); err != nil {
			t.Errorf("%s: %v", s, err)
		}
	}

	bad := []string{
		`require "vacation"; vacation :fcc "Sent" "away";`,                                            // fcc not required
		`require ["vacation", "fcc"]; vacation :fcc "Sent" :create "away";`,                           // :create needs mailbox
		`require ["vacation", "fcc"]; vacation :fcc "Sent" :flags "\\Seen" "away";`,                   // :flags needs imap4flags
		`require ["vacation", "fcc"]; vacation :fcc "Sent" :specialuse "\\Sent" "away";`,              // :specialuse needs special-use
		`require ["vacation", "fcc", "mailbox"]; vacation :create "away";`,                            // :create without :fcc
		`require ["vacation", "fcc", "special-use"]; vacation :fcc "Sent" :specialuse "Sent" "away";`, // not a special use
		`require ["enotify", "fcc", "imap4flags"]; notify :flags "\\Seen" "mailto:a@example.org";`,    // :flags without :fcc
		`require ["enotify", "fcc"]; notify :fcc "" "mailto:a@example.org";`,                          // empty mailbox
	}
	for _, s := range bad {
		if err := Compile(s); err == nil {
			t.Errorf("%s compiled", s)
		}
	}
}

// TestFccReachesTheDelivery: the :fcc arguments arrive in the delivery
// result, flags lower-cased as for fileinto.
func TestFccReachesTheDelivery(t *testing.T) {
	raw := []byte("From: a@example.org\r\nTo: u@example.org\r\nSubject: x\r\n\r\nbody\r\n")

	d, err := Run(context.Background(), `require ["vacation", "fcc", "mailbox", "imap4flags", "enotify"];
vacation :fcc "Sent" :create :flags ["\\Seen", "$Auto"] "away";
notify :fcc "Notes" "mailto:n@example.org";`, "a@example.org", "u@example.org", "u@example.org", raw)
	if err != nil {
		t.Fatal(err)
	}

	if f := d.Vacation.Fcc; f == nil || f.Mailbox != "Sent" || !f.Create || !slices.Equal(f.Flags, []string{`\seen`, "$auto"}) {
		t.Errorf("vacation fcc %+v", d.Vacation.Fcc)
	}

	if len(d.Notifications) != 1 || d.Notifications[0].Fcc == nil || d.Notifications[0].Fcc.Mailbox != "Notes" || d.Notifications[0].Fcc.Create {
		t.Errorf("notify fcc %+v", d.Notifications)
	}
}

// TestFccAdvertised: fcc is in the SIEVE capability.
func TestFccAdvertised(t *testing.T) {
	if !slices.Contains(ExtensionList(nil), "fcc") {
		t.Fatal("fcc is not advertised")
	}
}

// TestUnauthenticate: UNAUTHENTICATE is advertised, refused before
// authentication, and afterwards the session is unauthenticated and can
// log in again (RFC 5804 2.14.1).
func TestUnauthenticate(t *testing.T) {
	c := dialSieveServer(t)

	caps := c.send("CAPABILITY", false)
	if !slices.Contains(caps, `"UNAUTHENTICATE"`) {
		t.Fatalf("capabilities %q", caps)
	}

	if r := c.send("UNAUTHENTICATE", false); !strings.HasPrefix(r[len(r)-1], "NO") {
		t.Fatalf("before login: %q", r)
	}

	login := `AUTHENTICATE "PLAIN" "` + base64.StdEncoding.EncodeToString([]byte("\x00alice\x00secret")) + `"`
	if r := c.send(login, false); !strings.HasPrefix(r[len(r)-1], "OK") {
		t.Fatalf("login: %q", r)
	}

	if r := c.send("LISTSCRIPTS", false); !strings.HasPrefix(r[len(r)-1], "OK") {
		t.Fatalf("listscripts: %q", r)
	}

	if r := c.send(`UNAUTHENTICATE "x"`, false); !strings.HasPrefix(r[len(r)-1], "NO") {
		t.Fatalf("unauthenticate with an argument: %q", r)
	}

	if r := c.send("LISTSCRIPTS", false); !strings.HasPrefix(r[len(r)-1], "OK") {
		t.Fatalf("still logged in after the syntax error: %q", r)
	}

	if r := c.send("UNAUTHENTICATE", false); !strings.HasPrefix(r[len(r)-1], "OK") {
		t.Fatalf("unauthenticate: %q", r)
	}

	if r := c.send("LISTSCRIPTS", false); !strings.HasPrefix(r[len(r)-1], "NO") {
		t.Fatalf("listscripts after unauthenticate: %q", r)
	}

	if r := c.send(login, false); !strings.HasPrefix(r[len(r)-1], "OK") {
		t.Fatalf("second login: %q", r)
	}
}

// TestManageSieveSpans: every command gets a span named after it, with
// the account but never the arguments.
func TestManageSieveSpans(t *testing.T) {
	rec := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec))
	old := telemetry.Tracer
	telemetry.Tracer = tp.Tracer("shardpost")

	t.Cleanup(func() { telemetry.Tracer = old })

	c := dialSieveServer(t)

	secret := base64.StdEncoding.EncodeToString([]byte("\x00alice\x00secret"))
	c.send(`AUTHENTICATE "PLAIN" "`+secret+`"`, false)
	c.send("LISTSCRIPTS", false)
	c.send("BOGUS", false)

	var names []string

	for _, s := range rec.Ended() {
		names = append(names, s.Name())

		for _, a := range s.Attributes() {
			if strings.Contains(a.Value.String(), secret) || strings.Contains(a.Value.String(), "secret") {
				t.Errorf("span %s carries the credentials in %s", s.Name(), a.Key)
			}
		}

		if s.Name() == "managesieve.LISTSCRIPTS" && !slices.Contains(s.Attributes(), attribute.String("managesieve.user", "alice")) {
			t.Errorf("LISTSCRIPTS span without the account: %v", s.Attributes())
		}
	}

	for _, want := range []string{"managesieve.AUTHENTICATE", "managesieve.LISTSCRIPTS", "managesieve.UNKNOWN"} {
		if !slices.Contains(names, want) {
			t.Errorf("no span %s in %v", want, names)
		}
	}
}

// bufioString reads a string.
func bufioString(s string) *bufio.Reader { return bufio.NewReader(strings.NewReader(s)) }
