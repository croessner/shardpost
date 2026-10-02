//go:build tentacle

package main

import (
	"crypto/rand"
	"encoding/hex"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ceph/go-ceph/rados"

	"shardpost/internal/cephstore"
	"shardpost/internal/model"
)

// TestSystemCommands runs the system tools through the command line on
// pools of their own: a migration, its check, a cleanup that is too
// early, a revert, and an init that refuses the index pool the revert
// filled. The exit codes are those of shardpost-system(1).
func TestSystemCommands(t *testing.T) {
	if os.Getenv("SHARDPOST_TEST_CEPH_MON") == "" {
		t.Skip("no test cluster (run through hack/ceph-test-cluster.sh, as make test-docker does)")
	}

	if os.Getenv("SHARDPOST_TEST_CEPH_SYSTEM_POOL") != "" {
		t.Skip("covered by the run without a system pool")
	}

	index, system := cliPools(t)
	pre := []string{"--config", cliConfig(t, index)}

	run := func(want int, args ...string) {
		t.Helper()

		if got := runSystem(pre, args); got != want {
			t.Fatalf("shardpost system %v: exit %d, want %d", args, got, want)
		}
	}

	run(systemOK, systemStatus)
	run(systemOK, systemMigrate, "--to", system, "--check")
	run(systemRefused, systemMigrate, "--to", index)
	run(systemOK, systemMigrate, "--to", system, "--parallel", "2")
	run(systemOK, systemVerify)
	run(systemOK, systemVerify, "--source-only")

	r := fenceRun(t, index, system)

	run(systemRefused, systemCleanup, "--run", r, "--confirm", r)
	run(systemRefused, systemAbort, "--run", r)
	run(systemOK, systemRevert, "--run", r, "--confirm", r)
	run(systemRefused, systemInit, "--pool", system)
	run(systemRefused, "nonsense")
}

// cliPools creates an index and a system pool for the test and writes an
// admin record into the index pool (an existing cluster).
func cliPools(t *testing.T) (string, string) {
	t.Helper()

	var b [4]byte

	_, _ = rand.Read(b[:])
	index, system := "shardpost-test-cli-"+hex.EncodeToString(b[:])+"-index", "shardpost-test-cli-"+hex.EncodeToString(b[:])+"-system"

	s := testToolStore(t, os.Getenv("SHARDPOST_TEST_CEPH_INDEX_POOL"), "")

	err := s.WithConn(func(conn *rados.Conn) error {
		for _, pool := range []string{index, system} {
			if err := conn.MakePool(pool); err != nil {
				return err
			}

			if err := writable(conn, pool); err != nil {
				return err
			}
		}

		c, err := conn.OpenIOContext(index)
		if err != nil {
			return err
		}
		defer c.Destroy()

		c.SetNamespace(model.AdminNamespace)

		return c.WriteFull("kv.routing/x", []byte(`{"x":1}`))
	})
	if err != nil {
		t.Fatal(err)
	}

	return index, system
}

// writable waits until an object can be written to pool.
func writable(conn *rados.Conn, pool string) error {
	var err error

	for range 60 {
		var c *rados.IOContext

		if c, err = conn.OpenIOContext(pool); err == nil {
			if err = c.WriteFull("probe", []byte("x")); err == nil {
				err = c.Delete("probe")
			}

			c.Destroy()
		}

		if err == nil {
			return nil
		}

		time.Sleep(time.Second)
	}

	return err
}

// testToolStore opens a tool store on the test cluster.
func testToolStore(t *testing.T, index, system string) *cephstore.Store {
	t.Helper()

	s, err := cephstore.Open(cephstore.Config{MonHost: os.Getenv("SHARDPOST_TEST_CEPH_MON"), User: "admin",
		Key: os.Getenv("SHARDPOST_TEST_CEPH_KEY"), IndexPool: index, DataPool: os.Getenv("SHARDPOST_TEST_CEPH_DATA_POOL"),
		SystemPool: system, PlacementTool: true}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() {
		if s.Drain(10 * time.Second) {
			s.Close()
		}
	})

	return s
}

// cliConfig writes the configuration of the tools: the test index pool,
// no system pool.
func cliConfig(t *testing.T, index string) string {
	t.Helper()

	yaml := `server:
  auth_backend: static
  users: "user:secret"
ceph:
  user: admin
  key: "` + os.Getenv("SHARDPOST_TEST_CEPH_KEY") + `"
  mon_host: "` + os.Getenv("SHARDPOST_TEST_CEPH_MON") + `"
  index_pool: "` + index + `"
  data_pool: "` + os.Getenv("SHARDPOST_TEST_CEPH_DATA_POOL") + `"
  system_pool: ""
`

	path := filepath.Join(t.TempDir(), "shardpost.yaml")
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}

	return path
}

// fenceRun is the run of the fence of index.
func fenceRun(t *testing.T, index, system string) string {
	t.Helper()

	rep, err := testToolStore(t, index, system).SystemStatus(cephstore.SystemOptions{})
	if err != nil || rep.Run == "" {
		t.Fatalf("status: %+v %v", rep, err)
	}

	return rep.Run
}
