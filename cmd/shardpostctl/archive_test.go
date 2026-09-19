package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"shardpost/internal/admin"
)

// TestExportRestore: alice is exported, the archive verified, restored
// into carol (isolated: no ACL, no active script) and again (repeatable);
// a damaged archive is refused before anything is restored.
func TestExportRestore(t *testing.T) {
	socket := serveFilled(t, admin.RoleDiagnose, admin.RoleExport, admin.RoleImport, admin.RoleSupport, admin.RoleContent)
	file := filepath.Join(t.TempDir(), "alice.tar")

	if code, _, errOut := call(socket, `require "fileinto"; fileinto "A";`, "sieve", "put", "-activate", "alice", "main"); code != exOK {
		t.Fatalf("sieve put: %s", errOut)
	}

	if code, _, errOut := call(socket, "", "flags", "-add", `\Flagged`, "alice", "INBOX", "2"); code != exOK {
		t.Fatalf("flags: %s", errOut)
	}

	code, out, errOut := call(socket, "", "export", "-o", file, "alice")
	if code != exOK || !strings.Contains(out, "verified: 4 mailboxes, 2 messages") {
		t.Fatalf("export: %d %s %s", code, out, errOut)
	}

	code, out, errOut = call(socket, "", "restore", "-account", "carol", file)
	if code != exOK || strings.Count(out, "verified true") != 4 || !strings.Contains(out, "acl restored false") ||
		!strings.Contains(out, `active ""`) {
		t.Fatalf("restore: %d %s %s", code, out, errOut)
	}

	checkRestored(t, socket)

	if code, out, _ := call(socket, "", "restore", "-account", "carol", file); code != exOK || !strings.Contains(out, "(0 imported, 2 already there)") {
		t.Fatalf("second restore: %d %s", code, out)
	}

	checkDamaged(t, socket, file)
}

// checkDamaged: a damaged archive is refused before anything is restored.
func checkDamaged(t *testing.T, socket, file string) {
	t.Helper()

	raw, _ := os.ReadFile(file)
	damaged := filepath.Join(t.TempDir(), "damaged.tar")
	_ = os.WriteFile(damaged, []byte(strings.Replace(string(raw), "Subject: x", "Subject: y", 1)), 0o600)

	if code, _, errOut := call(socket, "", "restore", "-account", "bob", damaged); code != exDataErr || !strings.Contains(errOut, "nothing restored") {
		t.Fatalf("damaged archive: %d %s", code, errOut)
	}
}

// checkRestored compares carol with alice.
func checkRestored(t *testing.T, socket string) {
	t.Helper()

	for _, args := range [][]string{{"mailbox", "show"}, {"messages"}} {
		_, a, _ := call(socket, "", append(args, "alice", "INBOX")...)
		_, c, _ := call(socket, "", append(args, "carol", "INBOX")...)

		if args[0] == "mailbox" {
			a, c = lineWith(a, "uidvalidity"), lineWith(c, "uidvalidity")
		}

		if a != c || a == "" {
			t.Errorf("%v differs:\nalice %s\ncarol %s", args, a, c)
		}
	}

	if _, out, _ := call(socket, "", "acl", "list", "carol", "Shared"); strings.Contains(out, "bob") {
		t.Errorf("ACL restored into another account: %s", out)
	}

	if _, out, _ := call(socket, "", "sieve", "list", "carol"); !strings.Contains(out, "  main") || strings.Contains(out, "* main") {
		t.Errorf("sieve in carol: %q", out)
	}
}

func lineWith(s, prefix string) string {
	for _, l := range strings.Split(s, "\n") {
		if strings.HasPrefix(l, prefix) {
			return l
		}
	}

	return ""
}
