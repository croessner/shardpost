package model_test

import (
	"errors"
	"testing"
	"time"

	"shardpost/internal/model"
	"shardpost/internal/sortedindex"
)

func directory() model.SortedIndex {
	return sortedindex.New(sortedindex.NewMemBackend(), model.AccountIndexName, model.AccountIndexShards)
}

func entryOf(t *testing.T, ix model.SortedIndex, account string) (model.AccountEntry, bool) {
	t.Helper()

	e, ok, err := model.GetAccountEntry(ix, account)
	if err != nil {
		t.Fatal(err)
	}

	return e, ok
}

func TestCheckNamespace(t *testing.T) {
	for _, ok := range []string{"alice", "alice@example.org", "~public", "~server", "~admin", "~learning", "~accounts"} {
		if err := model.CheckNamespace(ok); err != nil {
			t.Errorf("%q refused: %v", ok, err)
		}
	}

	for _, bad := range []string{"", "~", "~jmap", "~other"} {
		if err := model.CheckNamespace(bad); !errors.Is(err, model.ErrNamespace) {
			t.Errorf("%q accepted: %v", bad, err)
		}
	}
}

func TestRegisterAccount(t *testing.T) {
	ix := directory()
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

	if err := model.RegisterAccount(ix, "alice", "", "fsid", now); err != nil {
		t.Fatal(err)
	}

	e, ok := entryOf(t, ix, "alice")
	if !ok || e.State != model.AccountActive || !e.Created.Equal(now) || e.Cluster != "fsid" {
		t.Fatalf("entry %+v %v", e, ok)
	}

	// A second registration keeps the first entry.
	if err := model.RegisterAccount(ix, "alice", "", "other", now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}

	if e2, _ := entryOf(t, ix, "alice"); !e2.Created.Equal(now) || e2.Cluster != "fsid" {
		t.Fatalf("entry rewritten: %+v", e2)
	}
}

// TestRegisterSkipsReservedNames: the store's own namespaces are never
// registered, the public one is.
func TestRegisterSkipsReservedNames(t *testing.T) {
	ix := directory()
	now := time.Now()

	for _, name := range []string{"~server", "~admin", "~learning", "~accounts", ""} {
		if err := model.RegisterAccount(ix, name, "", "", now); err != nil {
			t.Fatal(err)
		}

		if _, ok := entryOf(t, ix, name); ok {
			t.Errorf("%q registered", name)
		}
	}

	if err := model.RegisterAccount(ix, model.PublicNamespace, "", "", now); err != nil {
		t.Fatal(err)
	}

	if _, ok := entryOf(t, ix, model.PublicNamespace); !ok {
		t.Error("~public not registered")
	}
}

func TestPurgingRefusesRegistration(t *testing.T) {
	ix := directory()
	now := time.Now()

	if err := model.MarkAccountPurging(ix, "alice", "job-1", "", now); err != nil {
		t.Fatal(err)
	}

	if err := model.RegisterAccount(ix, "alice", "", "", now); !errors.Is(err, model.ErrImportLocked) {
		t.Fatalf("a late writer: %v", err)
	}

	if err := model.RegisterAccount(ix, "alice", "import-x", "", now); !errors.Is(err, model.ErrImportLocked) {
		t.Fatalf("another session: %v", err)
	}

	if err := model.RegisterAccount(ix, "alice", "job-1", "", now); err != nil {
		t.Fatalf("the purge's own session: %v", err)
	}

	if e, _ := entryOf(t, ix, "alice"); e.State != model.AccountPurging || e.Session != "job-1" {
		t.Fatalf("entry %+v", e)
	}
}

func TestGrantBlocked(t *testing.T) {
	ix := directory()
	reg := false
	hasRegistry := func() (bool, error) { return reg, nil }

	_ = model.RegisterAccount(ix, "bob", "", "", time.Now())

	if b, err := model.GrantBlocked(ix, "bob", hasRegistry); b || err != nil {
		t.Fatalf("active: %v %v", b, err)
	}

	if b, err := model.GrantBlocked(ix, "nobody", hasRegistry); b || err != nil {
		t.Fatalf("unknown: %v %v", b, err)
	}

	_ = model.MarkAccountPurging(ix, "bob", "job-1", "", time.Now())

	if b, err := model.GrantBlocked(ix, "bob", hasRegistry); !b || err != nil {
		t.Fatalf("purging without registry: %v %v", b, err)
	}

	reg = true

	if b, err := model.GrantBlocked(ix, "bob", hasRegistry); b || err != nil {
		t.Fatalf("purging with registry: %v %v", b, err)
	}
}

// repairCase is one case of TestRepairAccountEntry.
type repairCase struct {
	name        string
	purging     bool
	running     bool
	registry    bool
	wantAction  string
	wantPresent bool
	wantState   string
	// age is how old the entry is at the repair.
	age time.Duration
}

func (c repairCase) check(t *testing.T) {
	t.Helper()

	ix := directory()
	_ = model.RegisterAccount(ix, "alice", "", "", time.Now())

	if c.purging {
		_ = model.MarkAccountPurging(ix, "alice", "job-7", "", time.Now())
	}

	var asked string

	rep, err := model.RepairAccountEntry(ix, "alice", model.RepairChecks{
		HasRegistry:  func() (bool, error) { return c.registry, nil },
		PurgeRunning: func(s string) (bool, error) { asked = s; return c.running, nil },
		Now:          time.Now().Add(c.age),
	})
	if err != nil {
		t.Fatal(err)
	}

	if c.purging && asked != "job-7" {
		t.Errorf("asked about session %q", asked)
	}

	e, ok := entryOf(t, ix, "alice")
	if rep.Action != c.wantAction || ok != c.wantPresent || (ok && e.State != c.wantState) || !rep.Found {
		t.Fatalf("repair %+v, entry %+v %v", rep, e, ok)
	}

	if c.wantAction == model.RepairActivated && e.Session != "" {
		t.Errorf("session kept: %+v", e)
	}
}

func TestRepairAccountEntry(t *testing.T) {
	cases := []repairCase{
		{"active", false, false, true, model.RepairNone, true, model.AccountActive, time.Hour},
		{"purge running", true, true, false, model.RepairRunning, true, model.AccountPurging, 0},
		{"purging without registry", true, false, false, model.RepairRemoved, false, "", 0},
		{"purging with registry", true, false, true, model.RepairActivated, true, model.AccountActive, 0},
		{"young active without registry", false, false, false, model.RepairNone, true, model.AccountActive, time.Minute},
		{"stale active without registry", false, false, false, model.RepairStale, false, "", time.Hour},
	}

	for _, c := range cases {
		t.Run(c.name, c.check)
	}

	rep, err := model.RepairAccountEntry(directory(), "nobody", model.RepairChecks{
		HasRegistry: func() (bool, error) { return false, nil }, PurgeRunning: func(string) (bool, error) { return false, nil },
	})
	if err != nil || rep.Found || rep.Action != model.RepairNone {
		t.Fatalf("absent entry: %+v %v", rep, err)
	}
}

// TestStaleRepairRestoresRaced: an account created while the stale
// entry was removed gets its entry back.
func TestStaleRepairRestoresRaced(t *testing.T) {
	ix := directory()
	_ = model.RegisterAccount(ix, "alice", "", "", time.Now().Add(-time.Hour))

	calls := 0

	rep, err := model.RepairAccountEntry(ix, "alice", model.RepairChecks{
		HasRegistry:  func() (bool, error) { calls++; return calls > 1, nil },
		PurgeRunning: func(string) (bool, error) { return false, nil },
		Now:          time.Now(),
	})
	if err != nil || rep.Action != model.RepairNone {
		t.Fatalf("repair %+v %v", rep, err)
	}

	if e, ok := entryOf(t, ix, "alice"); !ok || e.State != model.AccountActive {
		t.Fatalf("entry lost: %+v %v", e, ok)
	}
}
