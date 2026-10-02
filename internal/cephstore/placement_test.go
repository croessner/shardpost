//go:build tentacle

package cephstore

import (
	"errors"
	"strings"
	"testing"

	"shardpost/internal/model"
)

// placementCase is one row of the decision table.
type placementCase struct {
	name   string
	v      placementView
	f      Fence
	m      Mark
	probes probeResult
	state  string // the result when no refusal
	want   string // a part of the refusal ("" none)
	moved  bool   // ErrSystemMoved
}

var (
	sysView  = placementView{SystemPool: "sys", SystemID: 2, IndexPool: "idx", IndexID: 1, FSID: "fsid-a"}
	noneView = placementView{IndexPool: "idx", IndexID: 1, FSID: "fsid-a"}
)

func testFence(state string, to int64) Fence {
	return Fence{Exists: true, Version: 3, State: state, ToID: to, ToName: "sys", IndexID: 1, FSID: "fsid-a", Run: "r1"}
}

func testMark(state string) Mark {
	return Mark{Exists: true, Version: 2, Format: MarkFormat, State: state, IndexID: 1, SystemID: 2, FSID: "fsid-a", Run: "r1"}
}

// placementCases are the rows of the decision table (temp/12 §4.3),
// with the reverted and leased fences that count as no fence.
func placementCases() []placementCase {
	sys, none, fence, mark := sysView, noneView, testFence, testMark
	foreign := func(mut func(*Mark)) Mark { m := mark(MarkActive); mut(&m); return m }

	return []placementCase{
		// Without ceph.system_pool.
		{name: "no system pool, no fence", v: none, state: PlacementNone},
		{name: "no system pool, reverted fence", v: none, f: fence(FenceReverted, 2), state: PlacementNone},
		{name: "no system pool, leased fence", v: none, f: fence(FenceLeased, 0), state: PlacementNone},
		{name: "no system pool, mark ignored", v: none, m: mark(MarkActive), state: PlacementNone},
		{name: "no system pool, initializing", v: none, f: fence(FenceInitializing, 2), want: "set ceph.system_pool", moved: true},
		{name: "no system pool, migrating", v: none, f: fence(FenceMigrating, 2), want: "set ceph.system_pool", moved: true},
		{name: "no system pool, done", v: none, f: fence(FenceDone, 2), want: "set ceph.system_pool", moved: true},
		{name: "no system pool, native", v: none, f: fence(FenceNative, 2), want: "set ceph.system_pool", moved: true},

		// With ceph.system_pool, no effective fence.
		{name: "mark without fence", v: sys, m: mark(MarkActive), want: "carries an active mark"},
		{name: "mark with reverted fence", v: sys, f: fence(FenceReverted, 2), m: mark(MarkActive), want: "carries an active mark"},
		{name: "new cluster", v: sys, probes: probesEmpty, want: "shardpost system init --pool sys"},
		{name: "new cluster, leased fence", v: sys, f: fence(FenceLeased, 0), probes: probesEmpty, want: "shardpost system init --pool sys"},
		{name: "existing cluster", v: sys, probes: probesFound, want: "shardpost system migrate --to sys"},
		{name: "existing cluster, reverted", v: sys, f: fence(FenceReverted, 2), probes: probesFound, want: "shardpost system migrate --to sys"},

		// Fence for this pool.
		{name: "initializing", v: sys, f: fence(FenceInitializing, 2), want: "incomplete"},
		{name: "initializing with mark", v: sys, f: fence(FenceInitializing, 2), m: mark(MarkActive), want: "incomplete"},
		{name: "migrating", v: sys, f: fence(FenceMigrating, 2), want: "under way"},
		{name: "done", v: sys, f: fence(FenceDone, 2), m: mark(MarkActive), state: PlacementMigrated},
		{name: "native", v: sys, f: fence(FenceNative, 2), m: mark(MarkActive), state: PlacementNative},
		{name: "done without mark", v: sys, f: fence(FenceDone, 2), want: "carries no mark"},
		{name: "native without mark", v: sys, f: fence(FenceNative, 2), want: "carries no mark"},

		// Fence for another pool.
		{name: "initializing elsewhere", v: sys, f: fence(FenceInitializing, 9), want: "points to pool"},
		{name: "migrating elsewhere", v: sys, f: fence(FenceMigrating, 9), want: "points to pool"},
		{name: "done elsewhere", v: sys, f: fence(FenceDone, 9), m: mark(MarkActive), want: "points to pool"},
		{name: "native elsewhere", v: sys, f: fence(FenceNative, 9), want: "points to pool"},

		// The mark decides first.
		{name: "retired", v: sys, f: fence(FenceDone, 2), m: mark(MarkRetired), want: "withdrawn"},
		{name: "retired after revert", v: sys, f: fence(FenceReverted, 2), m: mark(MarkRetired), want: "withdrawn"},
		{name: "foreign index", v: sys, f: fence(FenceNative, 2), m: foreign(func(m *Mark) { m.IndexID = 5 }), want: "belongs to"},
		{name: "foreign cluster", v: sys, f: fence(FenceNative, 2), m: foreign(func(m *Mark) { m.FSID = "fsid-b" }), want: "belongs to"},
		{name: "foreign system id", v: sys, f: fence(FenceNative, 2), m: foreign(func(m *Mark) { m.SystemID = 8 }), want: "belongs to"},
		{name: "unknown mark state", v: sys, f: fence(FenceNative, 2), m: mark("broken"), want: "unknown state"},
		{name: "unknown fence state", v: sys, f: fence("broken", 2), m: mark(MarkActive), want: "unknown state"},
	}
}

// TestDecidePlacement goes through every row of the decision table.
func TestDecidePlacement(t *testing.T) {
	for _, tc := range placementCases() {
		state, err := decidePlacement(tc.v, tc.f, tc.m, tc.probes)

		switch {
		case tc.want == "":
			if err != nil || state != tc.state {
				t.Errorf("%s: %q, %v; want %q", tc.name, state, err, tc.state)
			}
		case err == nil || !errors.Is(err, ErrSystemPlacement) || !strings.Contains(err.Error(), tc.want):
			t.Errorf("%s: %q, %v; want a refusal with %q", tc.name, state, err, tc.want)
		case tc.moved != errors.Is(err, ErrSystemMoved):
			t.Errorf("%s: ErrSystemMoved %v, want %v", tc.name, errors.Is(err, ErrSystemMoved), tc.moved)
		}
	}

	// Without probes (the periodic check) the decision asks for them.
	if _, err := decidePlacement(sysView, Fence{}, Mark{}, probesUnread); !errors.Is(err, errNeedProbes) {
		t.Errorf("no fence, no mark, no probes: %v", err)
	}
}

// TestIndexPoolOf: only the store's own namespaces move, and only with a
// system pool; without one every namespace stays where it was (the
// in-memory store has no pools at all, so the setting means nothing
// there).
func TestIndexPoolOf(t *testing.T) {
	without := Config{IndexPool: "idx", DataPool: "data"}
	with := Config{IndexPool: "idx", DataPool: "data", SystemPool: "sys"}

	for _, ns := range []string{model.AdminNamespace, model.AccountsNamespace, model.LearningNamespace, model.ServerNamespace} {
		if p := without.indexPoolOf(ns); p != "idx" {
			t.Errorf("%s without system pool: %s", ns, p)
		}

		if p := with.indexPoolOf(ns); p != "sys" {
			t.Errorf("%s with system pool: %s", ns, p)
		}
	}

	for _, ns := range []string{model.PublicNamespace, "alice@example.org", "~bench", "~admin.moved/r1"} {
		if p := with.indexPoolOf(ns); p != "idx" {
			t.Errorf("%s with system pool: %s", ns, p)
		}
	}
}

// TestCheckToolClientOptions: a tool keeps its lease only with an OSD
// operation timeout of at most 60 s and throttles that let its
// operations start.
func TestCheckToolClientOptions(t *testing.T) {
	for _, ok := range []map[string]string{
		nil,
		{"rados_osd_op_timeout": "60"},
		{"rados_osd_op_timeout": "1", "objecter_inflight_ops": "64", "objecter_inflight_op_bytes": "67108864"},
		{"rados_mon_op_timeout": "0"},
	} {
		if err := checkToolClientOptions(ok, 16); err != nil {
			t.Errorf("%v refused: %v", ok, err)
		}
	}

	for _, bad := range []map[string]string{
		{"rados_osd_op_timeout": "0"},
		{"rados_osd_op_timeout": "61"},
		{"rados_osd_op_timeout": "x"},
		{"objecter_inflight_ops": "63"},
		{"objecter_inflight_op_bytes": "1048576"},
	} {
		if err := checkToolClientOptions(bad, 16); err == nil {
			t.Errorf("%v accepted", bad)
		}
	}
}
