package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"shardpost/internal/admin"
)

func TestSupportCommands(t *testing.T) {
	socket := serveFilled(t, admin.RoleDiagnose, admin.RoleContent, admin.RoleSupport, admin.RoleDelete)

	steps := []struct {
		stdin string
		args  []string
		want  string
	}{
		{"", []string{"messages", "alice", "INBOX"}, "uidvalidity"},
		{"", []string{"search", "-subject", "X", "alice"}, "2 hits"},
		{"", []string{"message", "show", "alice", "INBOX", "1"}, "Subject: x"},
		{"", []string{"message", "export", "alice", "INBOX", "2"}, "body"},
		{"", []string{"flags", "-add", `\Flagged`, "alice", "INBOX", "1,2"}, "2 changed"},
		{"", []string{"copy", "alice", "INBOX", "1", "A"}, "1 -> 1"},
		{"", []string{"move", "alice", "INBOX", "2", "B"}, "2 -> 1"},
		{`require "fileinto"; fileinto "A";`, []string{"sieve", "put", "-activate", "alice", "main"}, "active true"},
		{"", []string{"sieve", "list", "alice"}, "* main"},
		{"", []string{"sieve", "check", "-message", "INBOX:0:1", "alice"}, ""},
		{"", []string{"plan", "-mailbox", "A", "-uids", "1", "expunge", "alice"}, `"messages": 1`},
	}

	for _, s := range steps {
		code, out, errOut := call(socket, s.stdin, s.args...)

		if s.want == "" { // the stale UIDVALIDITY 0 must be refused
			if code != exDataErr {
				t.Errorf("%v: exit %d %s", s.args, code, errOut)
			}

			continue
		}

		if code != exOK || !strings.Contains(out, s.want) {
			t.Errorf("%v: exit %d, out %q, err %q", s.args, code, out, errOut)
		}
	}

	if code, _, _ := call(socket, "", "message", "show", "alice", "INBOX", "99"); code != exNoUser {
		t.Errorf("unknown message: exit %d", code)
	}

	if code, out, _ := call(socket, "if bogus;", "sieve", "check", "alice"); code != exDataErr || !strings.Contains(out, "invalid") {
		t.Errorf("broken script check: exit %d %s", code, out)
	}
	// A script from a file leaves standard input for a test message, so
	// a rule can be tried on mail that is not stored anywhere.
	script := filepath.Join(t.TempDir(), "rules.sieve")
	if err := os.WriteFile(script, []byte(`require "fileinto"; if header :contains "subject" "invoice" { fileinto "Archive"; }`), 0o600); err != nil {
		t.Fatal(err)
	}

	msg := "From: shop@example.net\r\nSubject: your invoice\r\n\r\nbody\r\n"

	code, out, errOut := call(socket, msg, "sieve", "check", "-file", script, "alice")
	if code != exOK || !strings.Contains(out, "Archive") {
		t.Errorf("test message from standard input: exit %d, out %q, err %q", code, out, errOut)
	}
}

// TestSieveCheckVacationText: the text output of a check names the
// effective vacation period.
func TestSieveCheckVacationText(t *testing.T) {
	socket := serveFilled(t, admin.RoleDiagnose)

	script := filepath.Join(t.TempDir(), "away.sieve")
	if err := os.WriteFile(script, []byte(`require "vacation"; vacation :days 3 :subject "Away" "back soon";`), 0o600); err != nil {
		t.Fatal(err)
	}

	code, out, errOut := call(socket, "From: shop@example.net\r\nTo: alice@example.org\r\nSubject: x\r\n\r\nbody\r\n", "sieve", "check", "-file", script, "alice")
	if code != exOK || !strings.Contains(out, `"Away" every 3 days`) {
		t.Errorf("vacation period: exit %d, out %q, err %q", code, out, errOut)
	}
}

func TestVacationPeriodText(t *testing.T) {
	for _, c := range []struct {
		days    int
		seconds int64
		want    string
	}{
		{7, 7 * 86400, "every 7 days"},
		{0, 3600, "every 3600 seconds"},
		{1, 90000, "every 90000 seconds"},
		{0, 0, "for every message"},
		{5, 0, "every 5 days"}, // a server that reports days only
	} {
		if got := vacationPeriod(c.days, c.seconds); got != c.want {
			t.Errorf("%d %d: %q, want %q", c.days, c.seconds, got, c.want)
		}
	}
}

// TestPublicFolderCommands: public folders are created and granted with
// the ordinary commands, using ~public as the account.
func TestPublicFolderCommands(t *testing.T) {
	socket := serveFilled(t, admin.RoleDiagnose, admin.RoleProvision)

	steps := []struct {
		args []string
		want string
	}{
		{[]string{"mailbox", "create", "~public", "Announcements"}, "Announcements"},
		{[]string{"acl", "set", "~public", "Announcements", "anyone", "lr"}, "anyone"},
		{[]string{"acl", "list", "~public", "Announcements"}, "lr"},
		{[]string{"mailbox", "list", "-all", "~public"}, "Announcements"},
		{[]string{"account", "show", "~public"}, "public"},
	}

	for _, s := range steps {
		code, out, errOut := call(socket, "", s.args...)
		if code != exOK || !strings.Contains(out, s.want) {
			t.Errorf("%v: exit %d, out %q, err %q", s.args, code, out, errOut)
		}
	}
}
