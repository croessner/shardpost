package sieve

import (
	"context"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// vacationMessage is the message the generated scripts run over.
const vacationMessage = "From: bob@example.org\r\nTo: alice@example.org\r\nSubject: hello\r\n\r\nhi\r\n"

// runVacation compiles and runs a generated script and returns its
// vacation, failing the test when the script does not run.
func runVacation(t *testing.T, v VacationSettings) *Delivery {
	t.Helper()

	script := VacationScript(v)

	d, err := Run(context.Background(), script, "bob@example.org", "alice@example.org", "alice", []byte(vacationMessage))
	if err != nil {
		t.Fatalf("the script does not run: %v\n%s", err, script)
	}

	return d
}

func at(year int) *time.Time {
	t := time.Date(year, 1, 2, 3, 4, 5, 0, time.UTC)
	return &t
}

// TestVacationScriptWindow: the reply goes out on or after fromDate and
// before toDate, with the ends given.
func TestVacationScriptWindow(t *testing.T) {
	cases := []struct {
		name     string
		from, to *time.Time
		want     bool
	}{
		{"no window", nil, nil, true},
		{"inside", at(2000), at(2999), true},
		{"before", at(2998), at(2999), false},
		{"after", at(2000), at(2001), false},
		{"from only, begun", at(2000), nil, true},
		{"from only, not yet", at(2999), nil, false},
		{"to only, not over", nil, at(2999), true},
		{"to only, over", nil, at(2000), false},
	}

	for _, c := range cases {
		d := runVacation(t, VacationSettings{FromDate: c.from, ToDate: c.to, TextBody: "away"})
		if (d.Vacation != nil) != c.want {
			t.Errorf("%s: vacation %v, want %v", c.name, d.Vacation != nil, c.want)
		}

		if len(d.FileInto) != 1 || d.FileInto[0].Mailbox != inboxName {
			t.Errorf("%s: the script changed where the message goes: %#v", c.name, d.FileInto)
		}
	}
}

// TestVacationScriptEscapes: whatever a client writes stays text. A
// subject that tries to end the string and add a command, a backslash
// before the closing quote, CR and LF — all of it reaches the reply as
// it was sent, and the message is still kept.
func TestVacationScriptEscapes(t *testing.T) {
	subject := `"; discard; "`
	text := "line one\r\nline \"two\"\\\rend \\\"; discard; vacation \"x"

	d := runVacation(t, VacationSettings{Subject: subject, TextBody: text})
	if d.Vacation == nil {
		t.Fatal("no vacation")
	}

	if d.Vacation.Subject != subject {
		t.Errorf("subject %q, want %q", d.Vacation.Subject, subject)
	}

	if want := strings.ReplaceAll(normalizeNewlines(text), "\n", "\r\n"); d.Vacation.Reason != want {
		t.Errorf("reason %q, want %q", d.Vacation.Reason, want)
	}

	if len(d.FileInto) != 1 {
		t.Errorf("the message was not kept: %#v", d.FileInto)
	}

	for in, want := range map[string]string{`a"b`: `"a\"b"`, `a\b`: `"a\\b"`, "a\r\nb\rc": "\"a\nb\nc\""} {
		if got := QuoteString(in); got != want {
			t.Errorf("QuoteString(%q) = %s, want %s", in, got, want)
		}
	}
}

// TestVacationScriptText: a text alone is a plain reply with the
// default period, a handle of its own and the server's subject.
func TestVacationScriptText(t *testing.T) {
	d := runVacation(t, VacationSettings{TextBody: "away"})
	if v := d.Vacation; v == nil || v.Mime || v.Reason != "away" || v.Subject != "" || v.Period() != 7*24*time.Hour {
		t.Fatalf("text %#v", v)
	}

	if !strings.HasPrefix(d.Vacation.Handle, JMAPVacationScript+"-") {
		t.Errorf("handle %q", d.Vacation.Handle)
	}

	a, b := VacationSettings{TextBody: "away"}, VacationSettings{TextBody: "away again"}
	if vacationHandle(a) == vacationHandle(b) || vacationHandle(a) != vacationHandle(VacationSettings{TextBody: "away"}) {
		t.Error("the handle does not follow the settings")
	}
}

// TestVacationScriptHTML: with HTML the reply is a MIME entity, with the
// text as its alternative when there is one, and no line is too long for
// a relay.
func TestVacationScriptHTML(t *testing.T) {
	long := strings.Repeat("<p>away until next week</p>", 100)

	d := runVacation(t, VacationSettings{TextBody: "away", HTMLBody: long})

	v := d.Vacation
	if v == nil || !v.Mime {
		t.Fatalf("text and html %#v", v)
	}

	for _, want := range []string{"multipart/alternative", "text/plain", "text/html"} {
		if !strings.Contains(v.Reason, want) {
			t.Errorf("no %s in %q", want, v.Reason)
		}
	}

	for _, line := range strings.Split(v.Reason, "\r\n") {
		if len(line) > 78 {
			t.Errorf("a line of %d characters in the reply", len(line))
		}
	}

	d = runVacation(t, VacationSettings{HTMLBody: "<b>away</b>"})
	if v := d.Vacation; v == nil || !v.Mime || strings.Contains(v.Reason, "multipart") ||
		!strings.HasPrefix(v.Reason, "Content-Type: text/html") {
		t.Errorf("html only %#v", v)
	}
}

// TestReservedScript: only the name of the JMAP vacation is reserved.
func TestReservedScript(t *testing.T) {
	if !ReservedScript("jmap-vacation") || ReservedScript("vacation") || ReservedScript("JMAP-vacation") {
		t.Error("reserved names")
	}
}

// vacationInputs are texts that have broken quoting or parsing elsewhere:
// quotes, backslashes, line breaks of every kind, a dot alone on a line,
// comment and multi-line markers, Sieve keywords, variables and non-ASCII.
var vacationInputs = []string{
	"", "plain", `"`, `\`, `\"`, `"\`, `\\"; discard; "`, "\"; discard; \"",
	"a\r\nb", "a\rb", "a\nb", "\r\n.\r\n", ".\n", "text:\nx\n.\n", "# comment", "/* open", "*/",
	"${subject}", "${1}", "}", "{", ";", "vacation \"x\";", "require [\"fileinto\"];",
	"Grüße aus dem Büro – 休暇中 🏖", "\t tabbed \t", strings.Repeat(`\"`, 300),
	strings.Repeat("long line ", 400),
}

// vacationDates are the ends a window is tried with.
var vacationDates = []*time.Time{nil, at(1970), at(2026), at(2999)}

// TestVacationScriptAlwaysCompiles: every combination of the inputs
// gives a script that compiles and runs, and the texts come back as
// they were given.
func TestVacationScriptAlwaysCompiles(t *testing.T) {
	for i, text := range vacationInputs {
		subject := vacationInputs[(i+7)%len(vacationInputs)]
		html := vacationInputs[(i+13)%len(vacationInputs)]

		for _, from := range vacationDates {
			for _, to := range vacationDates {
				checkVacationScript(t, VacationSettings{FromDate: from, ToDate: to, Subject: strings.ReplaceAll(subject, "\n", " "),
					TextBody: text, HTMLBody: html})
			}
		}
	}
}

// FuzzVacationScript: whatever the texts, the script compiles and the
// subject and text come back unchanged.
func FuzzVacationScript(f *testing.F) {
	for i, s := range vacationInputs {
		f.Add(s, vacationInputs[(i+1)%len(vacationInputs)])
	}

	f.Fuzz(func(t *testing.T, subject, text string) {
		if !utf8.ValidString(subject) || !utf8.ValidString(text) || strings.ContainsAny(subject, "\r\n") {
			t.Skip()
		}

		checkVacationScript(t, VacationSettings{Subject: subject, TextBody: text})
	})
}

// checkVacationScript compiles the script of the settings and, when its
// window is open now, runs it and compares what comes back.
func checkVacationScript(t *testing.T, v VacationSettings) {
	t.Helper()

	script := VacationScript(v)
	if err := Compile(script); err != nil {
		t.Fatalf("%+v does not compile: %v\n%s", v, err, script)
	}

	if v.FromDate != nil || v.ToDate != nil || v.HTMLBody != "" || v.TextBody == "" {
		return
	}

	d, err := Run(context.Background(), script, "bob@example.org", "alice@example.org", "alice", []byte(vacationMessage))
	if err != nil || d.Vacation == nil {
		t.Fatalf("%+v does not run: %v", v, err)
	}

	if d.Vacation.Subject != v.Subject || d.Vacation.Reason != strings.ReplaceAll(normalizeNewlines(v.TextBody), "\n", "\r\n") {
		t.Errorf("%+v came back as %q / %q", v, d.Vacation.Subject, d.Vacation.Reason)
	}
}
