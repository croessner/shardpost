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
	"shardpost/internal/model"
)

// registrations is an admin store with the registrations of a server
// and of a push worker that ended, both with a system pool.
func registrations() (*memstore.AdminKV, *model.PoolRef) {
	regs := memstore.NewAdminKV()
	pool := &model.PoolRef{ID: 7, Name: "shardpost-system"}
	_ = jobs.Register(regs, jobs.Registration{Instance: "pod-1", Version: "0.47.0", Role: model.RoleServe, SystemPool: pool, StartedAt: time.Now()}, time.Now())
	_ = jobs.Deregister(regs, jobs.Registration{Instance: "push-1", Version: "0.47.0", Role: model.RolePush, SystemPool: pool, StartedAt: time.Now()}, time.Now())

	return regs, pool
}

// serveFilled is an admin socket over a memstore where alice has INBOX
// (two messages), A, B and "Shared" (bob lr).
// isAlice is the test account of fts stats -classes.
func isAlice(a string) bool { return a == "alice" }

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

	// The directory names alice and bob (the grant gave bob a registry)
	// and has completed its backfill, so batches can run.
	ix, release, _ := store.AccountIndex()
	_ = ix.SetMarker(model.AccountBackfillMarker, `{"finished":"2026-10-01T00:00:00Z"}`)

	release()

	dir, err := os.MkdirTemp("", "spctl")
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	mgr := jobs.NewManager(memstore.NewAdminKV(), &jobs.Env{Accounts: store, Directory: store, Log: log},
		jobs.Config{Poll: 10 * time.Millisecond, LeaseTTL: time.Second}, "pod-1", jobs.Kinds(time.Hour)...)

	regs, pool := registrations()

	s := admin.New(admin.Options{Delivery: &delivery.Service{Store: store, Log: log}, Auth: auth.Static{"alice": "pw", "bob": "pw", "carol": "pw"},
		Log: log, Version: "test", MaxMessageBytes: 1 << 20, SocketRoles: roles, Accounts: store, Instance: "pod-1",
		ConfigFingerprint: "abc", StorePing: func() error { return nil }, Jobs: mgr, Directory: store, KV: regs,
		Import: store, ImportMaxBatchMessages: 500, ImportMaxBatchBytes: 64 << 20, ImportMaxInventory: 5000,
		SharedPrefix: "Shared/", PublicPrefix: "Public/", Placement: func() (string, *model.PoolRef) { return "native", pool }, FTSStatsTestAccount: isAlice})

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
		{[]string{"server", "status"}, []string{"pod-1", "abc", "ok", "native (system pool shardpost-system, id 7)"}},
		{[]string{"server", "list"}, []string{"pod-1", "serve", "push-1", "push", "shardpost-system (id 7)", "true", "false"}},
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

func TestAccountList(t *testing.T) {
	socket := serveFilled(t, admin.RoleDiagnose)

	for _, tc := range []struct {
		args []string
		ok   func(out string) bool
	}{
		{[]string{"-limit", "1"}, func(out string) bool {
			return strings.Contains(out, "more: -after") && strings.Count(out, "active") == 1
		}},
		{[]string{"-limit", "1", "-all"}, func(out string) bool {
			return strings.Contains(out, "alice") && strings.Contains(out, "bob") && !strings.Contains(out, "more:")
		}},
		{[]string{"-all", "--json", "-limit", "1"}, func(out string) bool {
			return strings.Count(out, "\n") == 2 && strings.Contains(out, `"account":"bob"`)
		}},
		{[]string{"-state", "purging"}, func(out string) bool { return !strings.Contains(out, "alice") }},
	} {
		code, out, errOut := call(socket, "", append([]string{"account", "list"}, tc.args...)...)
		if code != exOK || !tc.ok(out) {
			t.Errorf("%v: %d %s %s", tc.args, code, out, errOut)
		}
	}
}
