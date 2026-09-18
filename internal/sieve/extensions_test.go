package sieve

import (
	"context"

	"github.com/foxcpp/go-sieve/interp"
	"net/textproto"
	"strings"
	"testing"
	"time"
)

const extMsg = "From: a@example.org\r\nTo: me@example.org\r\nSubject: hello\r\nDate: Tue, 15 Sep 2026 10:30:00 +0200\r\nMessage-ID: <dup-1@example.org>\r\nX-Old: 1\r\nX-Old: 2\r\n\r\nbody\r\n"

func TestSieveExtensions(t *testing.T) {
	ctx := context.Background()
	// date / currentdate
	d, err := Run(ctx, `require ["date", "fileinto"];
if date "Date" "year" "2026" { fileinto "y2026"; }
if date :zone "+0000" "Date" "hour" "08" { fileinto "utc8"; }
if date :originalzone "Date" "zone" "+0200" { fileinto "orig"; }
if currentdate "year" "1999" { fileinto "never"; }`, "a@example.org", "me@example.org", "me@example.org", []byte(extMsg))
	if err != nil {
		t.Fatal(err)
	}

	var boxes []string
	for _, f := range d.FileInto {
		boxes = append(boxes, f.Mailbox)
	}

	if got := strings.Join(boxes, ","); got != "y2026,utc8,orig" {
		t.Fatalf("date: %s", got)
	}
}

func TestSieveMailboxAndDuplicate(t *testing.T) {
	ctx := context.Background()
	// mailboxexists + fileinto :create
	seen := map[string]int{}
	h := Hooks{
		MailboxExists: func(name string) (bool, error) { return name == "Lists", nil },
		Duplicate:     func(key string) (bool, error) { return seen[key] > 0, nil },
	}
	record := func(d *Delivery) { // what the LMTP server does after a final delivery
		for _, t := range d.Tracked {
			seen[t.Key]++
		}
	}

	d, err := RunWithHooks(ctx, `require ["mailbox", "fileinto", "duplicate"];
if mailboxexists "Lists" { fileinto :create "Lists/new"; }
if mailboxexists "Nope" { fileinto "nope"; }
if duplicate { discard; }`, "a@example.org", "me@example.org", "me@example.org", []byte(extMsg), h)
	if err != nil {
		t.Fatal(err)
	}

	if len(d.FileInto) != 1 || d.FileInto[0].Mailbox != "Lists/new" || !d.FileInto[0].Create {
		t.Fatalf("%+v", d.FileInto)
	}

	if len(d.Tracked) != 1 {
		t.Fatalf("tracked: %+v", d.Tracked)
	}

	record(d)
	// second run: duplicate → discard (the first run was recorded)
	d, _ = RunWithHooks(ctx, `require ["duplicate"]; if duplicate :seconds 60 { discard; }`, "a@example.org", "me@example.org", "me@example.org", []byte(extMsg), h)
	if len(d.FileInto) != 0 {
		t.Fatalf("duplicate not discarded: %+v", d.FileInto)
	}
}

func TestSieveEditheader(t *testing.T) {
	ctx := context.Background()
	// editheader: add, delete by index, delete by match; later tests see edits
	d, err := Run(ctx, `require ["editheader", "fileinto"];
addheader "X-Sieve-Tag" "one";
addheader :last "X-Sieve-End" "two";
deleteheader :index 1 "X-Old";
if header :is "X-Sieve-Tag" "one" { fileinto "tagged"; }
if header :is "X-Old" "1" { fileinto "old1-still-there"; }
deleteheader :matches "Subject" "hel*";`, "a@example.org", "me@example.org", "me@example.org", []byte(extMsg))
	if err != nil {
		t.Fatal(err)
	}

	if len(d.FileInto) != 1 || d.FileInto[0].Mailbox != "tagged" {
		t.Fatalf("%+v", d.FileInto)
	}

	raw := string(d.Raw)
	if !strings.HasPrefix(raw, "X-Sieve-Tag: one\r\nFrom:") || !strings.Contains(raw, "X-Old: 2\r\n") || strings.Contains(raw, "X-Old: 1\r\n") ||
		strings.Contains(raw, "Subject:") || !strings.Contains(raw, "X-Sieve-End: two\r\n\r\nbody\r\n") {
		t.Fatalf("edited raw:\n%s", raw)
	}
}

func TestSieveEditheaderProtected(t *testing.T) {
	ctx := context.Background()
	// protected headers cannot be edited: the edit is ignored (RFC 5293 7)
	d, err := Run(ctx, `require ["editheader"]; deleteheader "Received"; deleteheader "Return-Path"; addheader "Received" "x";`, "a@example.org", "me@example.org", "me@example.org",
		[]byte("Received: from a\r\nReturn-Path: <a@example.org>\r\nSubject: s\r\n\r\nbody\r\n"))
	if err != nil {
		t.Fatal(err)
	}

	if raw := string(d.Raw); raw != "" && (!strings.Contains(raw, "Received: from a") || !strings.Contains(raw, "Return-Path:") || strings.Contains(raw, "Received: x")) {
		t.Fatalf("protected header edited:\n%s", raw)
	}
}

func TestStopKeepsImplicitKeep(t *testing.T) {
	d, err := Run(context.Background(), `stop;`, "a@example.org", "me@example.org", "me@example.org", []byte(extMsg))
	if err != nil {
		t.Fatal(err)
	}

	if len(d.FileInto) != 1 || d.FileInto[0].Mailbox != "INBOX" {
		t.Fatalf("stop lost the implicit keep: %+v", d.FileInto)
	}

	d, _ = Run(context.Background(), `require "vacation"; vacation "away"; stop;`, "a@example.org", "me@example.org", "me@example.org", []byte(extMsg))
	if d == nil || d.Vacation == nil || len(d.FileInto) != 1 {
		t.Fatalf("%+v", d)
	}
}

func TestVacationFromInjection(t *testing.T) {
	v := &Vacation{Reason: "away", From: "x@example.org\r\nBcc: evil@example.org"}
	hdr := textproto.MIMEHeader{"From": {"a@example.org"}, "Subject": {"hi"}}

	_, raw := v.Compose(hdr, "a@example.org", "me@example.org", time.Now())
	if strings.Contains(string(raw), "Bcc:") || !strings.HasPrefix(string(raw), "From: me@example.org\r\n") {
		t.Fatalf("%q", raw)
	}

	v = &Vacation{Reason: "away", From: "Me Away <me@example.org>"}

	_, raw = v.Compose(hdr, "a@example.org", "me@example.org", time.Now())
	if !strings.HasPrefix(string(raw), "From: \"Me Away\" <me@example.org>\r\n") {
		t.Fatalf("%q", raw)
	}
}

// extensionHooks backs metadata, specialuse_exists and include for
// runExtensionScript.
func extensionHooks() Hooks {
	scripts := map[string]string{"filter": `require ["fileinto", "variables", "include"]; global "tag"; set "tag" "inc"; fileinto "Lists";`}

	return Hooks{
		Metadata: func(mailbox, ann string) (string, bool, error) {
			if mailbox == "INBOX" && ann == "/private/comment" {
				return "hello", true, nil
			}

			return "", false, nil
		},
		SpecialUseExists: func(_ string, uses []string) (bool, error) { return uses[0] == `\Junk`, nil },
		Include: func(name string, global bool) ([]byte, bool, error) {
			s, ok := scripts[name]
			return []byte(s), ok && !global, nil
		},
		Config: DefaultConfig(),
	}
}

// runExtensionScript runs a script using the newer extensions through
// the hooks of extensionHooks.
func runExtensionScript(t *testing.T, h Hooks) *Delivery {
	t.Helper()

	ctx := context.Background()
	msg := "From: a@example.org\r\nTo: me@example.org\r\nSubject: hello\r\nX-Spam-Score: 9.5\r\nContent-Type: multipart/mixed; boundary=B\r\n\r\n--B\r\nContent-Type: text/plain\r\n\r\nfirst part\r\n--B\r\nContent-Type: text/html\r\n\r\n<b>second</b> part\r\n--B--\r\n"

	d, err := RunWithHooks(ctx, `require ["fileinto", "variables", "include", "mboxmetadata", "special-use", "spamtest", "relational", "comparator-i;ascii-numeric", "foreverypart", "extracttext", "mime", "enotify", "ihave", "regex", "index"];
global "tag";
include "filter";
if not string "${tag}" "inc" { fileinto "no-global"; }
if not metadata :is "INBOX" "/private/comment" "hello" { fileinto "no-metadata"; }
if not specialuse_exists "\\Junk" { fileinto "no-specialuse"; }
if not spamtest :value "ge" :comparator "i;ascii-numeric" "6" { fileinto "no-spamtest"; }
if not ihave "regex" { fileinto "no-ihave"; }
if not header :regex "subject" "^hel+o$" { fileinto "no-regex"; }
if not header :index 1 "to" "me@example.org" { fileinto "no-index"; }
set "n" "";
foreverypart {
  if header :mime :type "content-type" "text" {
    extracttext :first 5 "t";
    set "n" "${n}[${t}]";
  }
}
if not string "${n}" "[first][secon]" { fileinto "no-mime-${n}"; }
fileinto :specialuse "\\Junk" "Spam";
notify :importance "1" :message "urgent" "mailto:x@example.org?cc=y@example.org";`,
		"a@example.org", "me@example.org", "me@example.org", []byte(msg), h)
	if err != nil {
		t.Fatal(err)
	}

	return d
}

func TestNewExtensionsViaHooks(t *testing.T) {
	d := runExtensionScript(t, extensionHooks())

	var boxes []string
	for _, f := range d.FileInto {
		boxes = append(boxes, f.Mailbox+"/"+f.SpecialUse)
	}

	if got := strings.Join(boxes, ","); got != `Lists/,Spam/\Junk` {
		t.Fatalf("fileinto: %s", got)
	}

	if len(d.Notifications) != 1 || d.Notifications[0].Importance != "1" || len(d.Notifications[0].To) != 1 || len(d.Notifications[0].Cc) != 1 {
		t.Fatalf("notifications: %+v", d.Notifications)
	}
}

func TestComposeNotificationFromScript(t *testing.T) {
	h := extensionHooks()
	d := runExtensionScript(t, h)
	hdr := textproto.MIMEHeader{"From": {"a@example.org"}, "Subject": {"hello"}}

	from, to, raw, ok := ComposeNotification(d.Notifications[0], hdr, "a@example.org", "me@example.org", h.Config, time.Now())
	if !ok || from != "postmaster@example.org" || len(to) != 2 || !strings.Contains(string(raw), "Auto-Submitted: auto-notified") || !strings.Contains(string(raw), "X-Priority: 1") {
		t.Fatalf("compose: %q %v ok=%v\n%s", from, to, ok, raw)
	}
	// automatic mail is not notified about
	hdr.Set("Auto-Submitted", "auto-generated")

	if _, _, _, ok := ComposeNotification(d.Notifications[0], hdr, "a@example.org", "me@example.org", h.Config, time.Now()); ok {
		t.Fatal("notified about automatic mail")
	}
}

func TestCompileWithIncludes(t *testing.T) {
	h := extensionHooks()
	// PUTSCRIPT validation resolves includes against the store
	if err := CompileWith(`require "include"; include "filter";`, h); err != nil {
		t.Fatal(err)
	}

	if err := CompileWith(`require "include"; include "missing";`, h); err == nil {
		t.Fatal("missing include accepted")
	}

	if err := CompileWith(`require "include"; include :optional "missing";`, h); err != nil {
		t.Fatal(err)
	}
}

// fakePrograms is a program runner knowing frame, rewrite, broken and
// learn; it records each run as kind:name:args in runs.
func fakePrograms(runs *[]string) interp.ProgramRunner {
	return func(_ context.Context, kind interp.ProgramKind, name string, args []string, input []byte) (interp.ProgramResult, bool, error) {
		*runs = append(*runs, string(kind)+":"+name+":"+strings.Join(args, ","))
		switch name {
		case "frame":
			return interp.ProgramResult{Output: []byte("[" + string(input) + "]")}, true, nil
		case "rewrite":
			return interp.ProgramResult{Output: []byte("Subject: replaced\r\n\r\nnew body\r\n")}, true, nil
		case "broken":
			return interp.ProgramResult{ExitCode: 1}, true, nil
		case "learn":
			return interp.ProgramResult{}, true, nil
		}

		return interp.ProgramResult{}, false, nil
	}
}

func TestExternalProgramsViaHook(t *testing.T) {
	ctx := context.Background()

	var runs []string

	h := Hooks{Config: DefaultConfig(), Program: fakePrograms(&runs)}

	d, err := RunWithHooks(ctx, `require ["vnd.dovecot.execute", "vnd.dovecot.pipe", "vnd.dovecot.filter", "variables", "fileinto"];
execute :input "x" :output "out" "frame";
if not string "${out}" "[x]" { fileinto "bad-output"; }
if execute "broken" { fileinto "broken-succeeded"; }
if not execute :pipe "frame" { fileinto "pipe-input-failed"; }
pipe :try "learn";
filter "rewrite";
if not header :is "subject" "replaced" { fileinto "not-replaced"; }`,
		"a@example.org", "me@example.org", "me@example.org", []byte(extMsg), h)
	if err != nil {
		t.Fatal(err)
	}

	if len(d.FileInto) != 0 || len(d.Pipes) != 1 || d.Pipes[0].Program != "learn" || !d.Pipes[0].Try {
		t.Fatalf("%+v pipes=%+v", d.FileInto, d.Pipes)
	}

	if !strings.HasPrefix(string(d.Raw), "Subject: replaced") {
		t.Fatalf("filter output not applied: %q", d.Raw)
	}

	if strings.Join(runs, " ") != "execute:frame: execute:broken: execute:frame: filter:rewrite:" {
		t.Fatalf("runs: %v", runs)
	}
}

func TestExternalProgramsFailures(t *testing.T) {
	ctx := context.Background()

	var runs []string

	h := Hooks{Config: DefaultConfig(), Program: fakePrograms(&runs)}
	// Pipe errors are handled when the delivery commits its deferred actions.
	unknown, err := RunWithHooks(ctx, `require "vnd.dovecot.pipe"; pipe "nope";`, "a@example.org", "me@example.org", "me@example.org", []byte(extMsg), h)
	if err != nil || unknown.RunPipes(ctx, h.Program) == nil {
		t.Fatal("unknown pipe committed")
	}
	// the extensions compile without a runner and fail at runtime only
	if err := Compile(`require "vnd.dovecot.execute"; execute "anything";`); err != nil {
		t.Fatal(err)
	}

	if _, err := Run(ctx, `require "vnd.dovecot.execute"; execute "anything";`, "a@example.org", "me@example.org", "me@example.org", []byte(extMsg)); err == nil {
		t.Fatal("execute without runner succeeded")
	}
	// a program name from a variable is a compile error
	if err := Compile(`require ["vnd.dovecot.execute", "variables"]; set "p" "x"; execute "${p}";`); err == nil {
		t.Fatal("variable program name accepted")
	}
}

func TestDeferredPipeFailures(t *testing.T) {
	for _, tc := range []struct {
		script     string
		fail, keep bool
	}{
		{`pipe "bad";`, true, false},
		{`pipe :try "bad";`, false, true},
		{`pipe :try "bad"; fileinto "Archive";`, false, false},
		{`pipe "ok";`, false, false},
		{`pipe :copy "ok";`, false, true},
	} {
		t.Run(tc.script, func(t *testing.T) {
			h := Hooks{Program: func(_ context.Context, _ interp.ProgramKind, n string, _ []string, _ []byte) (interp.ProgramResult, bool, error) {
				return interp.ProgramResult{ExitCode: map[string]int{"bad": 1, "ok": 0}[n]}, true, nil
			}}

			d, err := RunWithHooks(context.Background(), `require ["vnd.dovecot.pipe", "fileinto", "copy"];`+tc.script, "", "", "", []byte(extMsg), h)
			if err != nil {
				t.Fatal(err)
			}

			err = d.RunPipes(context.Background(), h.Program)
			if (err != nil) != tc.fail {
				t.Fatalf("pipe error=%v", err)
			}

			kept := false

			for _, f := range d.FileInto {
				if f.Mailbox == "INBOX" {
					kept = true
				}
			}

			if kept != tc.keep {
				t.Fatalf("keep=%v want=%v", kept, tc.keep)
			}
		})
	}
}

// recordingPrograms records each program input; filter replaces the
// message.
func recordingPrograms(inputs *[]string) interp.ProgramRunner {
	return func(_ context.Context, k interp.ProgramKind, _ string, _ []string, b []byte) (interp.ProgramResult, bool, error) {
		*inputs = append(*inputs, string(b))

		if k == interp.ProgramFilter {
			return interp.ProgramResult{Output: []byte("Subject: filtered\r\n\r\nnew body")}, true, nil
		}

		return interp.ProgramResult{}, true, nil
	}
}

func TestPipeSnapshots(t *testing.T) {
	var inputs []string

	h := Hooks{Program: recordingPrograms(&inputs)}

	d, err := RunWithHooks(context.Background(), `require ["vnd.dovecot.filter","vnd.dovecot.pipe","editheader","fileinto","copy"]; fileinto :copy "Before"; addheader "X-Before" "yes"; filter "rewrite"; addheader "X-After" "yes"; pipe :copy "sink"; fileinto "After";`, "", "", "", []byte(extMsg), h)
	if err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(inputs[0], "X-Before: yes") {
		t.Fatal("filter input missed header edit")
	}

	if err = d.RunPipes(context.Background(), h.Program); err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(inputs[1], "Subject: filtered") || !strings.Contains(inputs[1], "X-After: yes") || strings.Contains(inputs[1], "X-Before") {
		t.Fatal("pipe received wrong message version")
	}

	if len(d.FileInto) != 2 || strings.Contains(string(d.FileInto[0].Raw), "filtered") || !strings.Contains(string(d.FileInto[1].Raw), "X-After: yes") {
		t.Fatal("fileinto snapshots lost")
	}
}

func TestInvalidProgramScripts(t *testing.T) {
	var inputs []string

	h := Hooks{Program: recordingPrograms(&inputs)}
	for _, script := range []string{`require "vnd.dovecot.pipe"; pipe "x"; pipe "x";`, `require ["vnd.dovecot.pipe","reject"]; pipe "x"; reject "no";`, `require "vnd.dovecot.execute"; execute :input "" :pipe "x";`} {
		if _, err := RunWithHooks(context.Background(), script, "", "", "", []byte(extMsg), h); err == nil {
			t.Fatal("invalid program script accepted")
		}
	}
}
