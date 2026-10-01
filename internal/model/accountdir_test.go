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

	// A registration (only ever one whose registry is missing, so a
	// re-creation) writes the entry again with its own time.
	later := now.Add(time.Hour)
	if err := model.RegisterAccount(ix, "alice", "", "fsid2", later); err != nil {
		t.Fatal(err)
	}

	if e2, _ := entryOf(t, ix, "alice"); !e2.Created.Equal(later) || e2.State != model.AccountActive || e2.Cluster != "fsid2" {
		t.Fatalf("entry not refreshed: %+v", e2)
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

// TestStaleRepairRacesCreation: a creation of the account races the
// removal of its stale entry; in either order of the two directory
// writes the entry exists afterwards.
func TestStaleRepairRacesCreation(t *testing.T) {
	staleRepair := func(ix model.SortedIndex, hasRegistry func() (bool, error)) model.DirectoryRepair {
		t.Helper()

		rep, err := model.RepairAccountEntry(ix, "alice", model.RepairChecks{
			HasRegistry: hasRegistry, PurgeRunning: func(string) (bool, error) { return false, nil }, Now: time.Now(),
		})
		if err != nil {
			t.Fatal(err)
		}

		return rep
	}

	t.Run("hook first", func(t *testing.T) {
		// The hook rewrites the stale entry, the repair's stat still
		// finds no registry (it is created right after), the repair's
		// removal then meets a changed entry.
		ix := directory()
		_ = model.RegisterAccount(ix, "alice", "", "", time.Now().Add(-time.Hour))

		rep := staleRepair(ix, func() (bool, error) {
			if err := model.RegisterAccount(ix, "alice", "", "", time.Now()); err != nil {
				t.Fatal(err)
			}

			return false, nil // the registry is created after the stat
		})
		if rep.Action != model.RepairNone || rep.After == nil {
			t.Fatalf("repair %+v", rep)
		}

		if e, ok := entryOf(t, ix, "alice"); !ok || e.State != model.AccountActive {
			t.Fatalf("entry lost: %+v %v", e, ok)
		}
	})

	t.Run("repair first", func(t *testing.T) {
		// The repair removes the stale entry before the hook runs; the
		// hook then finds no entry and writes one.
		ix := directory()
		_ = model.RegisterAccount(ix, "alice", "", "", time.Now().Add(-time.Hour))

		if rep := staleRepair(ix, func() (bool, error) { return false, nil }); rep.Action != model.RepairStale {
			t.Fatalf("repair %+v", rep)
		}

		if err := model.RegisterAccount(ix, "alice", "", "", time.Now()); err != nil {
			t.Fatal(err)
		}

		if e, ok := entryOf(t, ix, "alice"); !ok || e.State != model.AccountActive {
			t.Fatalf("entry lost: %+v %v", e, ok)
		}
	})
}
