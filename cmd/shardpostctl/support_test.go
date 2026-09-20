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
