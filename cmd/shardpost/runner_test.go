//go:build tentacle

package main

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"shardpost/internal/cephstore"
	"shardpost/internal/config"
	"shardpost/internal/extprograms"
	"shardpost/internal/jobs"
	"shardpost/internal/model"
	"shardpost/internal/sortedindex"
	"shardpost/internal/version"
)

// The tests of "shardpost runner" and of a server without workers (stage
// B18b, §8.1 and §8.2) against the test cluster of
// hack/ceph-test-cluster.sh.

// runnerConfig is serveConfig with the job system on: what a runner runs.
func runnerConfig(t *testing.T, dir string) *config.Config {
	t.Helper()

	settings := serveConfig(t, dir)
	settings.Jobs.Enabled = true

	return settings
}

// TestRunnerRegistersAndEnds: the runner registers with the role runner,
// opens none of the configured protocol listeners and no admin socket,
// takes the worker leases, and records its orderly end.
func TestRunnerRegistersAndEnds(t *testing.T) {
	if os.Getenv("SHARDPOST_TEST_CEPH_MON") == "" {
		t.Skip("no test cluster (run through hack/ceph-test-cluster.sh, as make test-docker does)")
	}

	dir, err := os.MkdirTemp("", "sp")
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	settings := runnerConfig(t, dir)
	store := serveStore(t, settings)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ran := make(chan error, 1)

	go func() { ran <- runRunner(ctx, quietLog(), settings, store, extprograms.New(settings.ExtPrograms)) }()

	instance := waitRole(t, store, ran, model.RoleRunner)

	expectNoListeners(t, settings)

	// The upload expiry worker takes its lease at once: a worker runs.
	waitFor(t, "the upload expiry lease", func() bool {
		return lockHeld(t, store, model.AccountsNamespace, sortedindex.LockOID(model.UploadsQueue))
	})

	cancel()

	select {
	case err := <-ran:
		if err != nil {
			t.Fatalf("runner: %v", err)
		}
	case <-time.After(time.Minute):
		t.Fatal("the runner did not return after the signal")
	}

	rec := registration(t, store, instance)
	if rec.StoppedAt.IsZero() || rec.Role != model.RoleRunner {
		t.Fatalf("the end was not recorded: %+v", rec)
	}

	if lockHeld(t, store, model.AccountsNamespace, sortedindex.LockOID(model.UploadsQueue)) {
		t.Error("the upload expiry lease outlived the runner")
	}
}

// expectNoListeners checks that the configured protocol listeners and
// the admin socket are not open: the ports are free and the socket path
// does not exist.
func expectNoListeners(t *testing.T, settings *config.Config) {
	t.Helper()

	for _, addr := range []string{settings.Server.ImapAddr, settings.Server.LmtpAddr, settings.Sieve.Addr} {
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			t.Errorf("the runner opened %s", addr)

			continue
		}

		_ = ln.Close()
	}

	if _, err := os.Stat(settings.Admin.Socket); err == nil {
		t.Error("the runner opened the admin socket")
	}
}

// TestServeWithoutWorkers: a server with workers.run false registers
// with workers false, takes none of the worker leases, and still
// answers (its listeners are up); the admin store it registers in sees
// no runner and the server warns, which is not an error.
func TestServeWithoutWorkers(t *testing.T) {
	if os.Getenv("SHARDPOST_TEST_CEPH_MON") == "" {
		t.Skip("no test cluster (run through hack/ceph-test-cluster.sh, as make test-docker does)")
	}

	dir, err := os.MkdirTemp("", "sp")
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	settings := runnerConfig(t, dir)
	settings.Workers.Run = false
	store := serveStore(t, settings)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	served := make(chan error, 1)

	go func() { served <- serve(ctx, quietLog(), settings, store, extprograms.New(settings.ExtPrograms), nil) }()

	instance := waitRole(t, store, served, model.RoleServe)

	rec := registration(t, store, instance)
	if rec.Workers == nil || *rec.Workers {
		t.Fatalf("the registration does not say workers false: %+v", rec)
	}

	time.Sleep(2 * time.Second) // a worker would have taken its lease by now

	if lockHeld(t, store, model.AccountsNamespace, sortedindex.LockOID(model.UploadsQueue)) {
		t.Error("a front-end took the upload expiry lease")
	}

	if lockHeld(t, store, model.AdminNamespace, sortedindex.LockOID(model.JobsExpiryQueue)) {
		t.Error("a front-end took the jobs expiry lease")
	}

	conn, err := net.DialTimeout("tcp", settings.Server.ImapAddr, 5*time.Second)
	if err != nil {
		t.Fatalf("the front-end does not listen: %v", err)
	}

	_ = conn.Close()

	cancel()

	select {
	case err := <-served:
		if err != nil {
			t.Fatalf("serve: %v", err)
		}
	case <-time.After(time.Minute):
		t.Fatal("serve did not return after the signal")
	}
}

// TestRunnerWithCroessnerConfigmap: the runner starts with the
// configmap of the croessner overlay as its configuration, without the
// TLS, VAPID, Nauthilus and other secrets that configmap names in the
// file system: it reads the servers' configuration but touches none of
// those.
func TestRunnerWithCroessnerConfigmap(t *testing.T) {
	if os.Getenv("SHARDPOST_TEST_CEPH_MON") == "" {
		t.Skip("no test cluster (run through hack/ceph-test-cluster.sh, as make test-docker does)")
	}

	raw, err := os.ReadFile(filepath.Join("..", "..", "deploy", "overlays", "croessner-nauthilus", "server.yaml"))
	if err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "server.yaml")

	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}

	// The cluster and the health port of this test; everything else as
	// the configmap says (unknown paths included).
	for k, v := range map[string]string{
		"CEPH_USER": "admin", "CEPH_KEY": os.Getenv("SHARDPOST_TEST_CEPH_KEY"), "CEPH_MON_HOST": os.Getenv("SHARDPOST_TEST_CEPH_MON"),
		"CEPH_INDEX_POOL": os.Getenv("SHARDPOST_TEST_CEPH_INDEX_POOL"), "CEPH_DATA_POOL": os.Getenv("SHARDPOST_TEST_CEPH_DATA_POOL"),
		"CEPH_SYSTEM_POOL": os.Getenv("SHARDPOST_TEST_CEPH_SYSTEM_POOL"), "HTTP_ADDR": freeAddr(t),
	} {
		t.Setenv(k, v)
	}

	// The servers' Load refuses the missing secrets; the runner's does not.
	if _, err := config.Load(path); err == nil || !strings.Contains(err.Error(), "secret file") {
		t.Fatalf("a server would start without its secrets: %v", err)
	}

	settings, command, err := loadConfig([]string{"--config", path, commandRunner})
	if err != nil || command != commandRunner {
		t.Fatalf("loadConfig: %s %v", command, err)
	}

	store := serveStore(t, settings)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ran := make(chan error, 1)

	go func() { ran <- runRunner(ctx, quietLog(), settings, store, extprograms.New(settings.ExtPrograms)) }()

	waitRole(t, store, ran, model.RoleRunner)
	cancel()

	select {
	case err := <-ran:
		if err != nil {
			t.Fatalf("runner: %v", err)
		}
	case <-time.After(time.Minute):
		t.Fatal("the runner did not return after the signal")
	}
}

// waitRole waits for this host's fresh registration with role, or a
// failure of the process, and returns the instance name.
func waitRole(t *testing.T, store *cephstore.Store, ended <-chan error, role string) string {
	t.Helper()

	kv, err := store.AdminKV()
	if err != nil {
		t.Fatal(err)
	}
	defer kv.Close()

	host, _ := os.Hostname()
	deadline := time.Now().Add(30 * time.Second)

	for time.Now().Before(deadline) {
		select {
		case err := <-ended:
			t.Fatalf("the process ended early: %v", err)
		default:
		}

		servers, err := jobs.Servers(kv, time.Now())
		if err != nil {
			t.Fatal(err)
		}

		for _, s := range servers {
			if strings.HasPrefix(s.Instance, host) && s.Role == role && s.Version == version.Version && s.Fresh {
				return s.Instance
			}
		}

		time.Sleep(50 * time.Millisecond)
	}

	t.Fatalf("no fresh registration with role %s", role)

	return ""
}

// registration reads one registration record.
func registration(t *testing.T, store *cephstore.Store, instance string) model.ServerRecord {
	t.Helper()

	kv, err := store.AdminKV()
	if err != nil {
		t.Fatal(err)
	}
	defer kv.Close()

	servers, err := jobs.Servers(kv, time.Now())
	if err != nil {
		t.Fatal(err)
	}

	for _, s := range servers {
		if s.Instance == instance {
			return s.ServerRecord
		}
	}

	t.Fatalf("no registration of %s", instance)

	return model.ServerRecord{}
}

// lockHeld reports whether the worker lock on oid in ns has a locker.
func lockHeld(t *testing.T, store *cephstore.Store, ns, oid string) bool {
	t.Helper()

	u, err := store.User(ns)
	if err != nil {
		t.Fatal(err)
	}
	defer u.Close()

	n, err := u.LockHolders(oid, "worker")
	if err != nil {
		return false
	}

	return n > 0
}

// waitFor polls cond for up to 30 seconds.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()

	for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline); time.Sleep(100 * time.Millisecond) {
		if cond() {
			return
		}
	}

	t.Fatalf("timed out waiting for %s", what)
}
