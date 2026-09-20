package main

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"shardpost/internal/admin"
	"shardpost/internal/adminapi"
	"shardpost/internal/auth"
	"shardpost/internal/delivery"
	"shardpost/internal/jobs"
	"shardpost/internal/memstore"
)

// serveFilled is an admin socket over a memstore where alice has INBOX
// (two messages), A, B and "Shared" (bob lr).
func serveFilled(t *testing.T, roles ...string) string {
	t.Helper()

	store := memstore.New()
	u, _ := store.User("alice")

	for _, name := range []string{"INBOX", "A", "B", "Shared"} {
		_ = u.EnsureMailbox(name, nil)
	}

	for i := 0; i < 2; i++ {
		_, _ = u.Deliver("INBOX", []byte("Subject: x\r\n\r\nbody\r\n"), nil, time.Now())
	}

	_ = u.SetACL("Shared", "bob", "lr")

	dir, err := os.MkdirTemp("", "spctl")
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	mgr := jobs.NewManager(memstore.NewAdminKV(), &jobs.Env{Accounts: store, Log: log},
		jobs.Config{Poll: 10 * time.Millisecond, LeaseTTL: time.Second}, "pod-1", jobs.Kinds(time.Hour)...)
	s := admin.New(admin.Options{Delivery: &delivery.Service{Store: store, Log: log}, Auth: auth.Static{"alice": "pw", "bob": "pw", "carol": "pw"},
		Log: log, Version: "test", MaxMessageBytes: 1 << 20, SocketRoles: roles, Accounts: store, Instance: "pod-1",
		ConfigFingerprint: "abc", StorePing: func() error { return nil }, Jobs: mgr,
		Import: store, ImportMaxBatchMessages: 500, ImportMaxBatchBytes: 64 << 20, ImportMaxInventory: 5000,
		SharedPrefix: "Shared/", PublicPrefix: "Public/"})

	ctx, cancel := context.WithCancel(context.Background())
	ended := make(chan struct{})

	go func() { mgr.Run(ctx); close(ended) }()

	t.Cleanup(func() { cancel(); <-ended })

	path := filepath.Join(dir, "admin.sock")

	ln, err := admin.ListenUnix(path)
	if err != nil {
		t.Fatal(err)
	}

	srv := s.UnixServer()

	go func() { _ = srv.Serve(ln) }()

	t.Cleanup(func() { _ = srv.Close() })

	return path
}

func TestDiagnoseCommands(t *testing.T) {
	socket := serveFilled(t, admin.RoleDiagnose)

	for _, tc := range []struct {
		args []string
		want []string
	}{
		{[]string{"server", "status"}, []string{"pod-1", "abc", "ok"}},
		{[]string{"account", "show", "alice"}, []string{"canonical alice", "true, 4 mailboxes", "2 / unlimited messages"}},
		{[]string{"mailbox", "list", "-limit", "2", "-all", "alice"}, []string{"INBOX", "Shared", "UIDNEXT"}},
		{[]string{"mailbox", "list", "-limit", "2", "alice"}, []string{"more: -after \"B\""}},
		{[]string{"mailbox", "show", "alice", "INBOX"}, []string{"2 (2 unseen, 0 deleted)"}},
		{[]string{"acl", "list", "alice", "Shared"}, []string{"bob", "lr", "(owner)"}},
		{[]string{"acl", "explain", "alice", "Shared", "bob"}, []string{"visible as Shared/alice/Shared"}},
		{[]string{"quota", "show", "alice"}, []string{"2 / unlimited messages"}},
		{[]string{"shared", "list", "bob"}, []string{"alice", "Shared"}},
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
}

func TestDiagnoseJSONAndErrors(t *testing.T) {
	socket := serveFilled(t, admin.RoleDiagnose)

	// Flags may follow the positional arguments.
	code, out, _ := call(socket, "", "mailbox", "list", "alice", "--json")

	var page adminapi.MailboxPage
	if code != exOK || json.Unmarshal([]byte(out), &page) != nil || len(page.Items) != 4 {
		t.Fatalf("json: %d %s", code, out)
	}

	for _, tc := range []struct {
		args []string
		code int
	}{
		{[]string{"mailbox", "show", "alice", "Nope"}, exNoUser},
		{[]string{"mailbox", "list", "ALICE@example.org"}, exUsage},
		{[]string{"mailbox", "frobnicate", "alice"}, exUsage},
		{[]string{"acl", "explain", "alice", "Shared"}, exUsage},
		{[]string{"server"}, exUsage},
	} {
		if code, _, errOut := call(socket, "", tc.args...); code != tc.code {
			t.Errorf("%v: exit %d, want %d (%s)", tc.args, code, tc.code, errOut)
		}
	}

	noRole := serveFilled(t, admin.RoleImport)
	if code, _, _ := call(noRole, "", "account", "show", "alice"); code != exNoPerm {
		t.Errorf("without role: exit %d", code)
	}
}
