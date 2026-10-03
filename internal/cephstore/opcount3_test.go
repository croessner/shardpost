//go:build tentacle

package cephstore

import (
	"fmt"
	"os"
	"testing"
	"time"
)

// TestHotPathOpsLayout3 counts the hot paths with a layout 3 index of
// S = D = H = 1 and of 4 (plan §4.8: about 23 ops per delivery at 1/1,
// 7 ops and the blob read per expunge). Run with
// SHARDPOST_TEST_OPCOUNT=1 inside hack/ceph-test-cluster.sh. Each runs
// once without and once with the identifier filter of the dictionary
// (B14h, temp/13 §10).
func TestHotPathOpsLayout3(t *testing.T) {
	if os.Getenv("SHARDPOST_TEST_OPCOUNT") == "" {
		t.Skip("set SHARDPOST_TEST_OPCOUNT=1 to count")
	}

	for _, run := range []struct {
		n      int
		filter bool
	}{{1, false}, {4, false}, {1, true}, {4, true}} {
		n := run.n

		t.Run(fmt.Sprintf("S%d/filter=%v", n, run.filter), func(t *testing.T) {
			s := layout3Store(t, func(c *FTSConfig) {
				c.ShardsMin, c.ShardsMax = n, max(n, 32)
				c.DictFilter, c.DictIDMinRunes = run.filter, 25
			})
			setDictFilterMark(t, s, run.filter)
			t.Cleanup(func() { setDictFilterMark(t, s, false) })

			name := testAccount(t)

			msg := append(testMessage("ops", 500), []byte("track "+idOf(n)+" sig "+longIDOf(n)+"\r\n")...)

			setup, err := s.User(name)
			must(t, err)

			mustMailbox(t, setup, "INBOX")
			mustMailbox(t, setup, "Box")

			// the first delivery creates the index
			_, err = setup.Deliver("INBOX", msg, nil, time.Time{})
			must(t, err)
			setup.Close()

			countOps(t, fmt.Sprintf("layout 3 S=%d: LMTP delivery", n), 10, func() {
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

			if l := layoutOf(t, u); l == nil || l.S != n || (dxRunes(l.DX) > 0) != run.filter {
				t.Fatalf("layout %+v", l)
			}

			countSessionOps(t, u, msg)

			// due_only reads S + D + H + 1 (one each object and the id map)
			countOps(t, fmt.Sprintf("layout 3 S=%d: reshard check (due_only)", n), 1, func() {
				_, _, err := u.FTSLayoutDue()
				must(t, err)
			})
		})
	}
}
