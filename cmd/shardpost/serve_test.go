//go:build tentacle

package main

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"runtime/pprof"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"shardpost/internal/cephstore"
	"shardpost/internal/config"
	"shardpost/internal/extprograms"
	"shardpost/internal/jobs"
	"shardpost/internal/version"
)

// serveConfig is a configuration of serve against the test cluster of
// hack/ceph-test-cluster.sh: every listener on a free local port, the
// admin socket in dir, no job runner.
func serveConfig(t *testing.T, dir string) *config.Config {
	t.Helper()

	yaml := `server:
  imap_addr: ` + freeAddr(t) + `
  pop3_addr: ` + freeAddr(t) + `
  lmtp_addr: ` + freeAddr(t) + `
  http_addr: ` + freeAddr(t) + `
  auth_backend: static
  users: "user:secret"
sieve:
  addr: ` + freeAddr(t) + `
admin:
  socket: ` + filepath.Join(dir, "a.sock") + `
  socket_roles: [diagnose]
jobs:
  enabled: false
ceph:
  user: admin
  key: "` + os.Getenv("SHARDPOST_TEST_CEPH_KEY") + `"
  mon_host: "` + os.Getenv("SHARDPOST_TEST_CEPH_MON") + `"
  index_pool: "` + os.Getenv("SHARDPOST_TEST_CEPH_INDEX_POOL") + `"
  data_pool: "` + os.Getenv("SHARDPOST_TEST_CEPH_DATA_POOL") + `"
`

	path := filepath.Join(dir, "shardpost.yaml")
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}

	settings, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}

	return settings
}

// freeAddr returns a local address with a port that was free a moment
// ago (the configuration does not take port 0).
func freeAddr(t *testing.T) string {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()

	return ln.Addr().String()
}

// TestServeRegistersAndStopsInOrder (N8, R4.8): a server without the job
// runner registers in servers/<instance>; at shutdown the admin API
// stops before the registration closes the admin store the API uses,
// and both before serve returns (the caller then closes the store).
func TestServeRegistersAndStopsInOrder(t *testing.T) {
	if os.Getenv("SHARDPOST_TEST_CEPH_MON") == "" {
		t.Skip("no test cluster (run through hack/ceph-test-cluster.sh, as make test-docker does)")
	}

	dir, err := os.MkdirTemp("", "sp") // short: the socket path is limited
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	settings := serveConfig(t, dir)
	log := quietLog()

	store := serveStore(t, settings)

	var (
		mu    sync.Mutex
		steps []string
	)

	shutdownStep = func(s string) {
		mu.Lock()
		defer mu.Unlock()

		steps = append(steps, s)
	}

	t.Cleanup(func() { shutdownStep = func(string) {} })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	served := make(chan error, 1)

	go func() { served <- serve(ctx, log, settings, store, extprograms.New(settings.ExtPrograms), nil) }()

	waitRegistered(t, store, served)
	cancel()

	select {
	case err := <-served:
		if err != nil {
			t.Fatalf("serve: %v", err)
		}
	case <-time.After(time.Minute):
		t.Fatal("serve did not return after the signal")
	}

	mu.Lock()
	defer mu.Unlock()

	if !slices.Equal(steps, []string{"admin", "registration"}) {
		t.Errorf("shutdown steps %v, want the admin API before the registration", steps)
	}
}

// TestServeLeavesStoreDrained: once serve returned, every view and
// background worker it opened on the store has ended, so the store
// drains at once (nothing keeps a reference to the connection the
// caller then closes).
func TestServeLeavesStoreDrained(t *testing.T) {
	if os.Getenv("SHARDPOST_TEST_CEPH_MON") == "" {
		t.Skip("no test cluster (run through hack/ceph-test-cluster.sh, as make test-docker does)")
	}

	dir, err := os.MkdirTemp("", "sp")
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	settings := serveConfig(t, dir)
	store := serveStore(t, settings)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	served := make(chan error, 1)

	go func() {
		served <- serve(ctx, quietLog(), settings, store, extprograms.New(settings.ExtPrograms), nil)
	}()

	waitRegistered(t, store, served)
	cancel()

	select {
	case err := <-served:
		if err != nil {
			t.Fatalf("serve: %v", err)
		}
	case <-time.After(time.Minute):
		t.Fatal("serve did not return after the signal")
	}

	start := time.Now()

	if !store.Drain(10 * time.Second) {
		var b strings.Builder

		_ = pprof.Lookup("goroutine").WriteTo(&b, 1)
		t.Fatalf("the store did not drain within 10 s after serve returned; goroutines:\n%s", b.String())
	}

	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("the store took %v to drain after serve returned", d)
	}
}

// serveStore opens the store of the test.
func serveStore(t *testing.T, settings *config.Config) *cephstore.Store {
	t.Helper()

	store, err := cephstore.Open(cephConfig(settings), quietLog())
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(store.Close)

	return store
}

// waitRegistered waits for this server's registration (with jobs
// disabled) or a failure of serve.
func waitRegistered(t *testing.T, store *cephstore.Store, served <-chan error) {
	t.Helper()

	kv, err := store.AdminKV()
	if err != nil {
		t.Fatal(err)
	}
	defer kv.Close()

	instance, _ := os.Hostname()
	deadline := time.Now().Add(30 * time.Second)

	for time.Now().Before(deadline) {
		select {
		case err := <-served:
			t.Fatalf("serve ended early: %v", err)
		default:
		}

		servers, err := jobs.Servers(kv, time.Now())
		if err != nil {
			t.Fatal(err)
		}

		for _, s := range servers {
			if s.Instance == instance && s.Version == version.Version && s.Fresh {
				return
			}
		}

		time.Sleep(50 * time.Millisecond)
	}

	t.Fatal("the server did not register with jobs.enabled false")
}
