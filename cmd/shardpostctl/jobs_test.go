package main

import (
	"regexp"
	"strings"
	"testing"

	"shardpost/internal/admin"
)

func TestJobCommands(t *testing.T) {
	socket := serveFilled(t, admin.RoleDiagnose, admin.RoleProvision, admin.RoleDelete, admin.RoleRepair)

	code, out, errOut := call(socket, "", "plan", "-mailbox", "A", "mailbox_delete", "alice")
	if code != exOK || !strings.Contains(out, "apply with: shardpostctl apply ") {
		t.Fatalf("plan: %d %s %s", code, out, errOut)
	}

	planID := regexp.MustCompile(`shardpostctl apply (\S+)`).FindStringSubmatch(out)[1]

	if code, out, errOut := call(socket, "", "apply", "-wait", "-poll", "10ms", planID); code != exOK || !strings.Contains(out, "succeeded") {
		t.Fatalf("apply: %d %s %s", code, out, errOut)
	}

	if code, _, errOut := call(socket, "", "apply", planID); code != exDataErr {
		t.Fatalf("second apply: %d %s", code, errOut)
	}

	if code, out, _ := call(socket, "", "check", "-wait", "-poll", "10ms", "alice"); code != exOK || !strings.Contains(out, "index_check") {
		t.Fatalf("check: %d %s", code, out)
	}

	if code, out, _ := call(socket, "", "job", "list", "-account", "alice"); code != exOK || strings.Count(out, "succeeded") != 2 {
		t.Fatalf("list: %d %s", code, out)
	}
}

func TestJobCommandErrors(t *testing.T) {
	socket := serveFilled(t, admin.RoleDiagnose, admin.RoleRepair)

	for _, args := range [][]string{{"plan", "frobnicate", "alice"}, {"plan", "-classes", "bogus", "scrub", "alice"}} {
		if code, _, _ := call(socket, "", args...); code != exUsage {
			t.Errorf("%v: exit %d", args, code)
		}
	}

	if code, _, _ := call(socket, "", "job", "show", "nope"); code != exNoUser {
		t.Errorf("unknown job: exit %d", code)
	}
}
