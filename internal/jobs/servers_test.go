package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"shardpost/internal/memstore"
	"shardpost/internal/model"
	"shardpost/internal/sortedindex"
)

// indexVersion is the version of the servers hint index shard.
func indexVersion(t *testing.T, kv *memstore.AdminKV) uint64 {
	t.Helper()

	_, ver, err := kv.Omap().ReadKeys(sortedindex.ShardOID("servers", 0), []string{"pod-a"})
	if err != nil {
		t.Fatal(err)
	}

	return ver
}

// TestRegisterServer (F8): the first registration writes its expiry
// entry and the hint index; a heartbeat changes the record only, not the
// index shard.
func TestRegisterServer(t *testing.T) {
	kv := memstore.NewAdminKVMode(model.KVIndexRead)
	now := time.Now()

	if err := RegisterServer(kv, "pod-a", "0.45.0", now); err != nil {
		t.Fatal(err)
	}

	q, _ := kv.Queue(model.JobsExpiryQueue)

	due, err := q.Due(now.Add(model.ServerExpiry+time.Second), 10)
	if err != nil || len(due) != 1 || due[0].ID != model.ServersPrefix+"pod-a" {
		t.Fatalf("expiry entries %+v %v", due, err)
	}

	ver := indexVersion(t, kv)

	if err := RegisterServer(kv, "pod-a", "0.45.0", now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}

	if indexVersion(t, kv) != ver {
		t.Error("a heartbeat wrote the hint index")
	}

	if due, _ := q.Due(now.Add(48*time.Hour), 10); len(due) != 1 {
		t.Errorf("the heartbeat added expiry entries: %+v", due)
	}

	checkOneFresh(t, kv, now.Add(time.Minute))
}

// checkOneFresh: one fresh registration with the capability, read from
// the hint index without a listing.
func checkOneFresh(t *testing.T, kv *memstore.AdminKV, now time.Time) {
	t.Helper()

	servers, err := Servers(kv, now)
	if err != nil || len(servers) != 1 || !servers[0].Fresh || !servers[0].HasCap(model.CapFTSLayout3) || kv.Listings() != 0 {
		t.Errorf("servers %+v %v (listings %d)", servers, err, kv.Listings())
	}
}

// TestExpireServer (F8): the expiry branch moves the entry of a renewed
// registration and deletes an expired one with its index entry.
func TestExpireServer(t *testing.T) {
	kv := memstore.NewAdminKVMode(model.KVIndexRead)
	m := NewManager(kv, &Env{Log: slog.New(slog.NewTextHandler(io.Discard, nil))}, Config{}, "pod-x")
	start := time.Now().Add(-25 * time.Hour)

	for _, pod := range []string{"pod-a", "pod-b"} {
		if err := RegisterServer(kv, pod, "0.45.0", start); err != nil {
			t.Fatal(err)
		}
	}

	// pod-b lives on
	if err := RegisterServer(kv, "pod-b", "0.45.0", time.Now()); err != nil {
		t.Fatal(err)
	}

	m.expireDue(context.Background(), func() bool { return false })

	if _, err := kv.Get(model.ServersPrefix + "pod-a"); !errors.Is(err, model.ErrNotFound) {
		t.Errorf("expired registration kept: %v", err)
	}

	if _, err := kv.Get(model.ServersPrefix + "pod-b"); err != nil {
		t.Errorf("renewed registration deleted: %v", err)
	}

	entries, _ := kv.ListIndex(model.ServersPrefix, "", 10)
	if len(entries) != 1 || entries[0].Key != model.ServersPrefix+"pod-b" {
		t.Errorf("index %+v", entries)
	}

	q, _ := kv.Queue(model.JobsExpiryQueue)
	if due, _ := q.Due(time.Now().Add(23*time.Hour), 10); len(due) != 0 {
		t.Errorf("the renewed entry was not moved on: %+v", due)
	}
}

// TestLayout3Ready (N2, F12): enable needs exactly --pods fresh
// registrations with the capability; a replaced pod's predecessor keeps
// its record fresh for three minutes; old records are pruned.
func TestLayout3Ready(t *testing.T) {
	kv := memstore.NewAdminKVMode(model.KVIndexWrite)
	now := time.Now()

	for _, r := range []struct {
		pod  string
		seen time.Time
	}{
		{"pod-a", now}, {"pod-b", now.Add(-30 * time.Second)},
		{"pod-old", now.Add(-time.Minute)}, // the predecessor of a replaced pod
		{"pod-gone", now.Add(-30 * time.Hour)},
	} {
		if err := RegisterServer(kv, r.pod, "0.45.0", r.seen); err != nil {
			t.Fatal(err)
		}
	}

	servers, err := Servers(kv, now)
	if err != nil || len(servers) != 3 {
		t.Fatalf("servers %+v %v (the 30 hour old one must be pruned)", servers, err)
	}

	if _, err := kv.Get(model.ServersPrefix + "pod-gone"); !errors.Is(err, model.ErrNotFound) {
		t.Error("old registration not pruned")
	}

	err = Layout3Ready(servers, 2)
	if !errors.Is(err, ErrNotReady) || !strings.Contains(err.Error(), "pod-old") {
		t.Errorf("with the predecessor's record: %v", err)
	}

	readyLater(t, kv, now)
}

// TestDictFilterReady (B14h): the mark of the dictionary filter needs
// every fresh server to report fts_dict_filter; a server of 0.49.0
// knows layout 3 but not the filter.
func TestDictFilterReady(t *testing.T) {
	kv := memstore.NewAdminKVMode(model.KVIndexWrite)
	now := time.Now()

	if err := RegisterServer(kv, "pod-new", "0.50.0", now); err != nil {
		t.Fatal(err)
	}

	old, _ := json.Marshal(model.ServerRecord{Version: "0.49.0", Caps: []string{model.CapFTSLayout3, model.CapSystemPool}, Seen: now.UTC(),
		Role: model.RoleServe})

	if err := kv.Update(model.ServersPrefix+"pod-old", func([]byte) ([]byte, error) { return old, nil }); err != nil {
		t.Fatal(err)
	}

	servers, err := Servers(kv, now)
	if err != nil || len(servers) != 2 {
		t.Fatalf("servers %+v %v", servers, err)
	}

	if err := CapReady(servers, 2, model.CapFTSLayout3); err != nil {
		t.Errorf("layout 3: %v", err)
	}

	if err := CapReady(servers, 2, model.CapFTSDictFilter); !errors.Is(err, ErrNotReady) || !strings.Contains(err.Error(), "1 with fts_dict_filter") {
		t.Errorf("dictionary filter with a 0.49.0 server: %v", err)
	}
}

// readyLater: three minutes on, with pod-a and pod-b renewed, the
// predecessor is stale and the check passes for two pods only.
func readyLater(t *testing.T, kv *memstore.AdminKV, now time.Time) {
	t.Helper()

	later, _ := Servers(kv, now.Add(model.ServerFresh+30*time.Second))
	if err := Layout3Ready(later, 2); err == nil {
		t.Error("ready although pod-a and pod-b are stale too at that time")
	}

	for _, pod := range []string{"pod-a", "pod-b"} {
		if err := RegisterServer(kv, pod, "0.45.0", now.Add(model.ServerFresh)); err != nil {
			t.Fatal(err)
		}
	}

	later, _ = Servers(kv, now.Add(model.ServerFresh+30*time.Second))
	if err := Layout3Ready(later, 2); err != nil {
		t.Errorf("after three minutes: %v", err)
	}

	if err := Layout3Ready(later, 3); !errors.Is(err, ErrNotReady) {
		t.Errorf("wrong pod count accepted: %v", err)
	}
}

// TestRunRegistration (N8): the registration runs on its own (no job
// system needed) until its context ends, renews "seen" at every beat
// and records its orderly end.
func TestRunRegistration(t *testing.T) {
	for _, role := range []string{model.RoleServe, model.RolePush, model.RoleScrub, model.RoleBackfill, model.RoleSweep} {
		t.Run(role, func(t *testing.T) { runRegistration(t, role) })
	}
}

func runRegistration(t *testing.T, role string) {
	kv := memstore.NewAdminKVMode(model.KVIndexOff)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	started := time.Now().Add(-time.Hour)
	reg := Registration{Instance: "pod-a", Version: "0.47.0", Role: role,
		SystemPool: &model.PoolRef{ID: 7, Name: "shardpost-system"}, StartedAt: started}

	go func() {
		RunRegistration(ctx, kv, reg, 10*time.Millisecond, slog.New(slog.NewTextHandler(io.Discard, nil)))
		close(done)
	}()

	first := waitBeats(t, kv)

	if rec, _ := readRecord(t, kv, "pod-a"); !rec.Seen.After(first) || !sameRegistration(rec, reg) || !rec.Running(time.Now()) {
		t.Fatalf("no heartbeat or wrong record: %+v", rec)
	}

	cancel()
	<-done

	if rec, _ := readRecord(t, kv, "pod-a"); rec.StoppedAt.IsZero() || rec.Running(time.Now()) {
		t.Errorf("orderly end not recorded: %+v", rec)
	}

	servers, err := Servers(kv, time.Now())
	if err != nil || len(servers) != 1 || servers[0].Fresh {
		t.Errorf("a stopped process is listed as fresh: %+v %v", servers, err)
	}
}

// sameRegistration reports whether rec holds what reg registers.
func sameRegistration(rec model.ServerRecord, reg Registration) bool {
	return rec.Role == reg.Role && rec.SystemPool != nil && *rec.SystemPool == *reg.SystemPool &&
		rec.StartedAt.Equal(reg.StartedAt.UTC()) && rec.HasCap(model.CapSystemPool)
}

// waitBeats waits for two heartbeats of pod-a and returns the first.
func waitBeats(t *testing.T, kv model.AdminKV) time.Time {
	t.Helper()

	var first time.Time

	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		rec, ok := readRecord(t, kv, "pod-a")

		switch {
		case !ok:
		case first.IsZero():
			first = rec.Seen
		case rec.Seen.After(first):
			return first
		}
	}

	return first
}

func readRecord(t *testing.T, kv model.AdminKV, instance string) (model.ServerRecord, bool) {
	t.Helper()

	b, err := kv.Get(model.ServersPrefix + instance)
	if err != nil {
		return model.ServerRecord{}, false
	}

	var rec model.ServerRecord
	if err := json.Unmarshal(b, &rec); err != nil {
		t.Fatal(err)
	}

	return rec, true
}

// TestLayout3ReadyCountsServers: the push worker, scrubs and backfills
// register too; only servers (role serve, or none before 0.47.0) count
// for "fts layout3 enable --pods".
func TestLayout3ReadyCountsServers(t *testing.T) {
	kv := memstore.NewAdminKVMode(model.KVIndexOff)
	now := time.Now()

	if err := RegisterServer(kv, "pod-a", "0.47.0", now); err != nil {
		t.Fatal(err)
	}

	// A record before 0.47.0: no role.
	old, _ := json.Marshal(model.ServerRecord{Version: "0.46.0", Caps: []string{model.CapFTSLayout3}, Seen: now.UTC()})

	if err := kv.Update(model.ServersPrefix+"pod-b", func([]byte) ([]byte, error) { return old, nil }); err != nil {
		t.Fatal(err)
	}

	for i, role := range []string{model.RolePush, model.RoleScrub, model.RoleBackfill, model.RoleSweep} {
		if err := Register(kv, Registration{Instance: fmt.Sprintf("tool-%d", i), Version: "0.47.0", Role: role, StartedAt: now}, now); err != nil {
			t.Fatal(err)
		}
	}

	servers, err := Servers(kv, now)
	if err != nil || len(servers) != 6 {
		t.Fatalf("%d registrations, %v", len(servers), err)
	}

	if err := Layout3Ready(servers, 2); err != nil {
		t.Errorf("two servers and four other processes: %v", err)
	}

	if err := Layout3Ready(servers, 6); !errors.Is(err, ErrNotReady) {
		t.Errorf("the other roles were counted: %v", err)
	}
}
