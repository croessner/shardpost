package main

import (
	"regexp"
	"strings"
	"testing"

	"shardpost/internal/admin"
)

func TestAccountRepair(t *testing.T) {
	socket := serveFilled(t, "repair")

	code, out, errOut := call(socket, "", "account", "repair", "alice")
	if code != exOK || !strings.Contains(out, "none") || !strings.Contains(out, "active") {
		t.Fatalf("exit %d: %s %s", code, out, errOut)
	}

	code, out, _ = call(socket, "", "account", "repair", "--json", "nobody")
	if code != exOK || !strings.Contains(out, `"found": false`) {
		t.Fatalf("unknown account: exit %d %s", code, out)
	}

	if code, _, _ := call(serveFilled(t, admin.RoleDiagnose), "", "account", "repair", "alice"); code != exNoPerm {
		t.Fatalf("without role: exit %d", code)
	}
}

func TestAccountDirectory(t *testing.T) {
	socket := serveFilled(t, admin.RoleDiagnose)

	code, out, errOut := call(socket, "", "account", "directory", "alice")
	if code != exOK || !strings.Contains(out, "active") || !regexp.MustCompile(`registry +true`).MatchString(out) ||
		!strings.Contains(out, "256 shards, stable") {
		t.Fatalf("exit %d: %s %s", code, out, errOut)
	}

	code, out, _ = call(socket, "", "account", "directory", "--json", "nobody")
	if code != exOK || !strings.Contains(out, `"found": false`) || !strings.Contains(out, `"registry": false`) {
		t.Fatalf("unknown account: exit %d %s", code, out)
	}
}

func TestProvisionCommands(t *testing.T) {
	socket := serveFilled(t, admin.RoleDiagnose, admin.RoleProvision)

	for _, tc := range []struct {
		args []string
		want []string
	}{
		{[]string{"mailbox", "create", "-special-use", `\Archive`, "alice", "Projekte"}, []string{"Projekte", `\Archive`, "etag \"m-"}},
		{[]string{"mailbox", "rename", "alice", "Projekte", "Archiv 2026"}, []string{"Archiv 2026"}},
		{[]string{"mailbox", "unsubscribe", "alice", "Archiv 2026"}, []string{"false"}},
		{[]string{"acl", "set", "-mode", "add", "alice", "Shared", "bob", "w"}, []string{"bob", "lrw", "etag \"acl-"}},
		{[]string{"acl", "set", "-mode", "remove", "alice", "Shared", "bob", "rw"}, []string{"bob", " l\n"}},
		{[]string{"acl", "delete", "alice", "Shared", "bob"}, []string{"(owner)"}},
	} {
		code, out, errOut := call(socket, "", tc.args...)
		if code != exOK {
			t.Fatalf("%v: exit %d %s", tc.args, code, errOut)
		}

		for _, w := range tc.want {
			if !strings.Contains(out, w) {
				t.Errorf("%v: output lacks %q:\n%s", tc.args, w, out)
			}
		}
	}

	for _, tc := range []struct {
		args []string
		code int
	}{
		{[]string{"mailbox", "rename", "alice", "INBOX", "Old"}, exDataErr},
		{[]string{"mailbox", "rename", "-if-match", `"m-0000000000000000"`, "alice", "A", "Z"}, exDataErr},
		{[]string{"acl", "set", "alice", "Shared", "nobody", "lr"}, exNoUser},
		{[]string{"acl", "set", "-mode", "sideways", "alice", "Shared", "bob", "lr"}, exUsage},
	} {
		if code, _, errOut := call(socket, "", tc.args...); code != tc.code {
			t.Errorf("%v: exit %d, want %d (%s)", tc.args, code, tc.code, errOut)
		}
	}
}
