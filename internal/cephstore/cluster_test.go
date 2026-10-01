//go:build tentacle

package cephstore

import (
	"crypto/rand"
	"encoding/hex"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ceph/go-ceph/rados"
)

// Tests against a cluster: hack/ceph-test-cluster.sh starts a throwaway
// single-node cluster in the lint image and exports its address (make
// test-docker does). Without it these tests skip. Every test works in an
// account namespace of its own, so they need no cleanup between them.

func init() {
	// The tests take everything they wrote for old (scrubNow sleeps past
	// the mtime resolution); TestRepairGrace checks the minimum itself.
	minRepairGrace = 0
	sweepClockMargin = 0
}

var (
	clusterOnce  sync.Once
	clusterStore *Store
	clusterErr   error
)

// testConfig is the store configuration of the cluster tests: small
// chunks and index shards, so that a few messages already span several
// of each.
func testConfig() Config {
	return Config{
		MonHost:   os.Getenv("SHARDPOST_TEST_CEPH_MON"),
		User:      "admin",
		Key:       os.Getenv("SHARDPOST_TEST_CEPH_KEY"),
		IndexPool: os.Getenv("SHARDPOST_TEST_CEPH_INDEX_POOL"),
		DataPool:  os.Getenv("SHARDPOST_TEST_CEPH_DATA_POOL"),
		ChunkSize: 1024,
		FTSShards: 4,
		ShardSize: 4,
	}
}

// clusterStore returns the store of the test cluster (shared by the
// tests of the package) or skips the test.
func testStore(t *testing.T) *Store {
	t.Helper()

	if os.Getenv("SHARDPOST_TEST_CEPH_MON") == "" {
		t.Skip("no test cluster (run through hack/ceph-test-cluster.sh, as make test-docker does)")
	}

	clusterOnce.Do(func() {
		clusterStore, clusterErr = Open(testConfig(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	})

	if clusterErr != nil {
		t.Fatalf("open test cluster: %v", clusterErr)
	}

	return clusterStore
}

// storeWith opens a store of its own with the configuration changed by
// fn (closed with the test).
func storeWith(t *testing.T, fn func(*Config)) *Store {
	t.Helper()

	testStore(t)

	cfg := testConfig()
	fn(&cfg)

	s, err := Open(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("open test cluster: %v", err)
	}

	t.Cleanup(s.Close)

	return s
}

// testAccount returns a fresh account name for the test.
func testAccount(t *testing.T) string {
	t.Helper()

	var b [6]byte

	_, _ = rand.Read(b[:])

	name := strings.NewReplacer("/", "-", " ", "-").Replace(t.Name())

	return strings.ToLower(name) + "-" + hex.EncodeToString(b[:]) + "@test.invalid"
}

// testUser opens a fresh account of s, closed with the test.
func testUser(t *testing.T, s *Store) *User {
	t.Helper()

	u, err := s.User(testAccount(t))
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(u.Close)

	return u
}

// objects lists the objects of the account's namespace in one pool (a
// listing is fine in a test).
func objects(t *testing.T, ioctx *rados.IOContext) []string {
	t.Helper()

	it, err := ioctx.Iter()
	if err != nil {
		t.Fatal(err)
	}
	defer it.Close()

	var out []string

	for it.Next() {
		out = append(out, it.Value())
	}

	if err := it.Err(); err != nil {
		t.Fatal(err)
	}

	return out
}

// omapKeys returns the omap keys of an index object with the prefix.
func omapKeys(t *testing.T, u *User, oid, prefix string) map[string][]byte {
	t.Helper()

	vals, err := u.idx.GetAllOmapValues(oid, "", prefix, omapPageSize)
	if err != nil && errCode(err) != 2 { // ENOENT: no object, no keys
		t.Fatal(err)
	}

	return vals
}

// testMessage returns a message of about size bytes.
func testMessage(subject string, size int) []byte {
	head := "From: a@test.invalid\r\nTo: b@test.invalid\r\nSubject: " + subject + "\r\n\r\n"
	body := strings.Repeat("lorem ipsum dolor sit amet ", max(size-len(head), 1)/27+1)

	return []byte(head + body[:max(size-len(head), 1)] + "\r\n")
}

func TestClusterReachable(t *testing.T) {
	s := testStore(t)
	u := testUser(t, s)

	if err := u.CreateMailbox("INBOX", nil); err != nil {
		t.Fatal(err)
	}

	m, err := u.Deliver("INBOX", testMessage("hello", 3000), nil, time.Time{})
	if err != nil {
		t.Fatal(err)
	}

	if m.Chunks != 3 {
		t.Errorf("chunks %d, want 3", m.Chunks)
	}

	raw, err := u.Read(m)
	if err != nil || len(raw) != int(m.Size) {
		t.Fatalf("read back: %d bytes, %v", len(raw), err)
	}
}
