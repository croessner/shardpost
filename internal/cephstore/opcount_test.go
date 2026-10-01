//go:build tentacle

package cephstore

import (
	"encoding/json"
	"os"
	"os/exec"
	"testing"
	"time"
)

// Round trips of the hot paths, counted by the test cluster's OSD (its
// op_r and op_w counters; run with SHARDPOST_TEST_OPCOUNT=1 inside
// hack/ceph-test-cluster.sh). The numbers it logs are what
// docs/architecture/storage.md quotes for the cost of the intents; the
// test fails only when it cannot count.

// osdOps reads the client read and write ops the OSD has served.
func osdOps(t *testing.T) (reads, writes int64) {
	t.Helper()

	out, err := exec.Command("ceph", "daemon", "osd.0", "perf", "dump").Output()
	if err != nil {
		t.Fatalf("perf dump: %v", err)
	}

	var dump struct {
		OSD struct {
			R  int64 `json:"op_r"`
			W  int64 `json:"op_w"`
			RW int64 `json:"op_rw"`
		} `json:"osd"`
	}

	if err := json.Unmarshal(out, &dump); err != nil {
		t.Fatal(err)
	}

	return dump.OSD.R, dump.OSD.W + dump.OSD.RW
}

// countOps runs fn and logs the ops it caused, per unit.
func countOps(t *testing.T, what string, units int, fn func()) {
	t.Helper()

	r0, w0 := osdOps(t)

	fn()

	time.Sleep(300 * time.Millisecond) // background removals (Close)

	r1, w1 := osdOps(t)
	t.Logf("%-34s reads %5.1f  writes %5.1f", what, float64(r1-r0)/float64(units), float64(w1-w0)/float64(units))
}

func TestHotPathOps(t *testing.T) {
	if os.Getenv("SHARDPOST_TEST_OPCOUNT") == "" {
		t.Skip("set SHARDPOST_TEST_OPCOUNT=1 to count")
	}

	s := testStore(t)
	name := testAccount(t)
	msg := testMessage("ops", 500)

	setup, err := s.User(name)
	if err != nil {
		t.Fatal(err)
	}

	mustMailbox(t, setup, "INBOX")
	mustMailbox(t, setup, "Box")
	setup.Close()

	countOps(t, "LMTP delivery (view per message)", 10, func() {
		for range 10 {
			u, err := s.User(name)
			must(t, err)

			_, err = u.Deliver("INBOX", msg, nil, time.Time{})
			must(t, err)
			u.Close()
		}
	})

	u, err := s.User(name)
	must(t, err)

	defer u.Close()

	countSessionOps(t, u, msg)
}

// countSessionOps counts the paths of one session.
func countSessionOps(t *testing.T, u *User, msg []byte) {
	t.Helper()

	countOps(t, "APPEND in one session", 10, func() {
		for range 10 {
			_, err := u.Deliver("Box", msg, nil, time.Time{})
			must(t, err)
		}
	})

	metas, err := u.ListMessagesByName("INBOX")
	must(t, err)

	countOps(t, "EXPUNGE of one message", 5, func() {
		for i := range 5 {
			must(t, u.Expunge("INBOX", metas[i:i+1]))
		}
	})

	countOps(t, "CREATE and DELETE (empty)", 5, func() {
		for range 5 {
			must(t, u.CreateMailbox("Tmp", nil))
			must(t, u.DeleteMailbox("Tmp"))
		}
	})

	countOps(t, "JMAP upload", 5, func() {
		for range 5 {
			_, err := u.PutBlob(msg)
			must(t, err)
		}
	})
}

// must fails the test on an error.
func must(t *testing.T, err error) {
	t.Helper()

	if err != nil {
		t.Fatal(err)
	}
}
