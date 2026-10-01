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

// TestBatchCommands: plan -all plans a batch over every account; apply
// runs it as a job without account.
func TestBatchCommands(t *testing.T) {
	socket := serveFilled(t, admin.RoleDiagnose, admin.RoleRepair)

	code, out, errOut := call(socket, "", "plan", "-all", "index_check")
	if code != exOK || !strings.Contains(out, "(all accounts)") || !strings.Contains(out, `"kind": "index_check"`) {
		t.Fatalf("plan -all: %d %s %s", code, out, errOut)
	}

	planID := regexp.MustCompile(`shardpostctl apply (\S+)`).FindStringSubmatch(out)[1]

	code, out, errOut = call(socket, "", "apply", "-wait", "-poll", "10ms", planID)
	if code != exOK || !strings.Contains(out, "succeeded") || !strings.Contains(out, `"done": 2`) {
		t.Fatalf("apply: %d %s %s", code, out, errOut)
	}

	if code, out, _ := call(socket, "", "plan", "-all", "-skip-idle", "scrub"); code != exOK || !strings.Contains(out, `"skip_idle": true`) {
		t.Fatalf("plan -all -skip-idle scrub: %d %s", code, out)
	}

	for _, args := range [][]string{
		{"plan", "-all", "-mailbox", "A", "scrub"}, {"plan", "-all", "scrub", "alice"}, {"plan", "-all", "purge"},
		{"apply", "-all", planID},
	} {
		if code, _, _ := call(socket, "", args...); code == exOK {
			t.Errorf("%v accepted", args)
		}
	}
}
