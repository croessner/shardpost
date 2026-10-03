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
	"shardpost/internal/adminapi"
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

// TestFTSDictFilterCommands (B14h): the mark of the dictionary filter
// has the commands of the layout 3 mark; disable exits 0 when no
// account has a filtered generation and 65 while one has
// (printDisable); -dict-filter takes none or ids.
func TestFTSDictFilterCommands(t *testing.T) {
	socket, kv, ls := serveLayout(t)

	if err := jobs.RegisterServer(kv, "pod-a", "0.50.0", time.Now()); err != nil {
		t.Fatal(err)
	}

	for _, c := range []struct {
		args []string
		code int
		out  string
	}{
		{[]string{"fts", "dictfilter", "enable"}, exUsage, ""},
		{[]string{"fts", "dictfilter", "enable", "-pods", "2"}, exDataErr, ""},
		{[]string{"fts", "dictfilter", "enable", "-pods", "1"}, exOK, "enabled     true"},
		{[]string{"fts", "dictfilter", "status"}, exOK, "fts_dict_filter"},
		{[]string{"fts", "dictfilter", "disable"}, exOK, "no generation has the filter"},
		{[]string{"fts", "dictfilter", "bogus"}, exUsage, ""},
		{[]string{"plan", "-dict-filter", "all", "fts_layout", "alice"}, exUsage, ""},
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

	// the account's index (memstore: layout 2) has no filter
	code, out, errOut := call(socket, "", "fts", "dictfilter", "disable", "-json")
	if code != exOK || strings.Contains(out, "alice") {
		t.Errorf("disable: %d %q %q", code, out, errOut)
	}

	// the exit code of accounts left is printDisable's, shared with
	// layout 3
	d := &adminapi.FTSLayout3Disable{Accounts: []string{"alice"}}

	var o, e strings.Builder
	if code := printDisable(&o, &e, d, false, "done", "have it"); code != exDataErr || !strings.Contains(o.String(), "alice") || !strings.Contains(e.String(), "have it") {
		t.Errorf("printDisable: %d %q %q", code, o.String(), e.String())
	}
}

// TestShapeText: the identifier filter and the text preparation of a
// generation are shown with its shape.
func TestShapeText(t *testing.T) {
	n, s := func(v int) *int { return &v }, func(v string) *string { return &v }
	sh := adminapi.FTSLayoutShape{Layout: 3, Gen: s("g"), S: n(1), D: n(2), H: n(1), DictIdMinRunes: n(25), AttachedHeaders: s("nosig")}

	if got := shapeText(sh); got != "layout 3 gen g S/D/H 1/2/1 dict_filter ids/25 attached_headers nosig" {
		t.Errorf("shape %q", got)
	}
}

// TestFTSStatsCommand (temp/13 §7.1): the key counts of any account;
// classes only for a test account, otherwise exit 77; no term printed.
func TestFTSStatsCommand(t *testing.T) {
	socket := serveFilled(t, admin.RoleDiagnose)

	for _, c := range []struct {
		args []string
		want []string
	}{
		{[]string{"fts", "stats", "alice"}, []string{"dict_keys", "per message"}},
		{[]string{"fts", "stats", "-classes", "alice"}, []string{"identifier bound", "letters"}},
		{[]string{"fts", "stats", "-classes", "-exact", "-json", "alice"}, []string{`"by_ident_min_runes"`}},
	} {
		code, out, errOut := call(socket, "", c.args...)
		if code != exOK || !containsAll(out, c.want) || strings.Contains(out, "body") {
			t.Errorf("%v: %d %s %s (no term of the account may be printed)", c.args, code, out, errOut)
		}
	}

	if code, _, errOut := call(socket, "", "fts", "stats", "-classes", "bob"); code != exNoPerm || !strings.Contains(errOut, "fts.stats_test_accounts") {
		t.Errorf("classes of a real account: %d %s", code, errOut)
	}

	for _, args := range [][]string{{"fts", "stats"}, {"fts", "stats", "-parallel", "17", "-exact", "alice"}, {"fts", "stats", "a", "b"}} {
		if code, _, _ := call(socket, "", args...); code != exUsage {
			t.Errorf("%v: exit %d", args, code)
		}
	}
}

func containsAll(s string, parts []string) bool {
	for _, p := range parts {
		if !strings.Contains(s, p) {
			return false
		}
	}

	return true
}

// TestLargestDict (temp/13 §5.4): list -verify names the account with
// the most dictionary objects on the page, the conversion target
// included.
func TestLargestDict(t *testing.T) {
	n := func(v int) *int { return &v }
	items := []adminapi.FTSLayoutAccount{
		{Account: "a", Layout: &adminapi.FTSLayoutState{Current: adminapi.FTSLayoutShape{Layout: 3, S: n(32), D: n(128), H: n(1)}}},
		{Account: "b", Layout: &adminapi.FTSLayoutState{Current: adminapi.FTSLayoutShape{Layout: 3, S: n(1), D: n(1), H: n(1)},
			Next: &adminapi.FTSLayoutShape{Layout: 3, S: n(1), D: n(256), H: n(1)}}},
		{Account: "c"},
	}

	if account, d := largestDict(items); account != "b" || d != 256 {
		t.Errorf("largest %s %d", account, d)
	}

	if _, d := largestDict(items[2:]); d != 0 {
		t.Errorf("without layouts %d", d)
	}
}
