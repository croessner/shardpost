package jobs

import (
	"context"
	"errors"
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
// system needed) until its context ends.
func TestRunRegistration(t *testing.T) {
	kv := memstore.NewAdminKVMode(model.KVIndexOff)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})

	go func() {
		RunRegistration(ctx, kv, "pod-a", "0.45.0", 10*time.Millisecond, slog.New(slog.NewTextHandler(io.Discard, nil)))
		close(done)
	}()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := kv.Get(model.ServersPrefix + "pod-a"); err == nil {
			break
		}

		time.Sleep(5 * time.Millisecond)
	}

	cancel()
	<-done

	if _, err := kv.Get(model.ServersPrefix + "pod-a"); err != nil {
		t.Errorf("not registered: %v", err)
	}
}
