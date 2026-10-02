package main

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"shardpost/internal/admin"
	"shardpost/internal/auth"
	"shardpost/internal/delivery"
	"shardpost/internal/jobs"
	"shardpost/internal/memstore"
	"shardpost/internal/model"
	"shardpost/internal/sortedindex"
)

// layoutStore is a presence index in memory.
type layoutStore struct{ b *sortedindex.MemBackend }

func (l layoutStore) FTSLayout3Enabled() (bool, error) { return false, nil }

func (l layoutStore) FTSLayoutIndex() (model.SortedIndex, func(), error) {
	return sortedindex.New(l.b, model.FTSLayoutIndexName, model.FTSLayoutIndexShards), func() {}, nil
}

// serveLayout starts an admin API with the layout administration over
// an in-memory admin store.
func serveLayout(t *testing.T) (string, *memstore.AdminKV, layoutStore) {
	t.Helper()

	dir, err := os.MkdirTemp("", "spctl")
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	store := memstore.New()
	kv := memstore.NewAdminKV()
	ls := layoutStore{b: sortedindex.NewMemBackend()}

	s := admin.New(admin.Options{Delivery: &delivery.Service{Store: store, Log: log}, Auth: auth.Static{"user": "pw"},
		Log: log, Version: "test", MaxMessageBytes: 1 << 20, SocketRoles: []string{admin.RoleDiagnose, jobs.RoleRepair},
		Accounts: store, KV: kv, FTSLayouts: ls, LayoutDisableWait: time.Millisecond})

	path := filepath.Join(dir, "admin.sock")

	ln, err := admin.ListenUnix(path)
	if err != nil {
		t.Fatal(err)
	}

	srv := s.UnixServer()

	go func() { _ = srv.Serve(ln) }()

	t.Cleanup(func() { _ = srv.Close() })

	return path, kv, ls
}

// TestFTSLayoutCommands: enable refuses without -pods and with the wrong
// number; disable exits 65 while accounts have layout 3; show reads an
// account's record.
func TestFTSLayoutCommands(t *testing.T) {
	socket, kv, ls := serveLayout(t)

	if err := jobs.RegisterServer(kv, "pod-a", "0.45.0", time.Now()); err != nil {
		t.Fatal(err)
	}

	for _, c := range []struct {
		args []string
		code int
		out  string
	}{
		{[]string{"fts", "layout3", "enable"}, exUsage, ""},
		{[]string{"fts", "layout3", "enable", "-pods", "2"}, exDataErr, ""},
		{[]string{"fts", "layout3", "enable", "-pods", "1"}, exOK, "enabled     true"},
		{[]string{"fts", "layout3", "status"}, exOK, "server pod-a  0.45.0  fresh"},
		{[]string{"fts", "layout", "show", "user"}, exOK, "current  layout 2"},
		{[]string{"fts", "layout3", "list"}, exOK, ""},
		{[]string{"fts", "bogus"}, exUsage, ""},
	} {
		code, out, errOut := call(socket, "", c.args...)
		if code != c.code || !strings.Contains(out, c.out) {
			t.Errorf("%v: %d %q %q", c.args, code, out, errOut)
		}
	}

	ix, _, _ := ls.FTSLayoutIndex()
	if err := ix.Set("alice", []byte(`{"since":"2026-10-01T00:00:00Z"}`)); err != nil {
		t.Fatal(err)
	}

	code, out, errOut := call(socket, "", "fts", "layout3", "disable")
	if code != exDataErr || !strings.Contains(out, "alice") || !strings.Contains(errOut, "downgrade not possible") {
		t.Errorf("disable: %d %q %q", code, out, errOut)
	}

	if code, out, _ := call(socket, "", "fts", "layout3", "list"); code != exOK || !strings.Contains(out, "alice  since 2026-10-01") {
		t.Errorf("list: %d %q", code, out)
	}
}
