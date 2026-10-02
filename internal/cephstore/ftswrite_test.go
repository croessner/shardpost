//go:build tentacle

package cephstore

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"slices"
	"strconv"
	"sync"
	"testing"
	"time"

	"shardpost/internal/postings"
)

// idsOf returns the document ids of the GUIDs from the id map.
func idsOf(t *testing.T, u *User, guids ...string) []uint64 {
	t.Helper()

	m := omapKeys(t, u, oidFTSMap, "g:")
	out := make([]uint64, 0, len(guids))

	for _, g := range guids {
		v, ok := m[docKey(g)]
		if !ok {
			t.Fatalf("GUID %s has no document id (id map %v)", g, m)
		}

		id, _ := strconv.ParseUint(string(v), 10, 64)
		out = append(out, id)
	}

	slices.Sort(out)

	return out
}

// postingIDs returns the decoded posting list of a term; a list still
// in the GUID layout fails the test.
func postingIDs(t *testing.T, u *User, term string) []uint64 {
	t.Helper()

	p, ok := omapKeys(t, u, u.ftsShardOID(term), term)[term]
	if !ok {
		t.Fatalf("no posting list for %s", term)
	}

	if postings.IsLegacy(p) {
		t.Fatalf("the posting list of %s is still a GUID list", term)
	}

	ids, ok := postings.Decode(p)
	if !ok {
		t.Fatalf("posting list of %s does not decode", term)
	}

	return ids
}

// TestLegacyPostingsConvertedOnDelivery: posting lists of GUIDs from
// before document ids are converted by the delivery that adds to them
// (the legacy check runs on the compare-and-swap's own read), the GUIDs
// get ids, the index is not marked as written by this version from the
// start, and the old GUIDs are still found.
func TestLegacyPostingsConvertedOnDelivery(t *testing.T) {
	s := testStore(t)
	u := testUser(t, s)
	mustMailbox(t, u, "INBOX")

	var g1, g2 [guidBytes]byte

	_, _ = rand.Read(g1[:])
	_, _ = rand.Read(g2[:])

	const (
		subject = "s:zebracorn"
		body    = "b:lorem"
	)

	lists := map[string][]byte{subject: g1[:], body: append(slices.Clone(g1[:]), g2[:]...)}
	for term, list := range lists {
		if err := u.idx.SetOmap(u.ftsShardOID(term), map[string][]byte{term: list}); err != nil {
			t.Fatal(err)
		}
	}

	m, err := u.Deliver("INBOX", testMessage("zebracorn", 300), nil, time.Time{})
	if err != nil {
		t.Fatal(err)
	}

	h1, h2 := hex.EncodeToString(g1[:]), hex.EncodeToString(g2[:])

	if got, want := postingIDs(t, u, subject), idsOf(t, u, h1, m.GUID); !slices.Equal(got, want) {
		t.Errorf("%s: ids %v, want %v", subject, got, want)
	}

	if got, want := postingIDs(t, u, body), idsOf(t, u, h1, h2, m.GUID); !slices.Equal(got, want) {
		t.Errorf("%s: ids %v, want %v", body, got, want)
	}

	if v := omapKeys(t, u, oidFTSMap, ftsVersionKey); len(v) != 0 {
		t.Errorf("an index with GUID lists got the version marker: %v", v)
	}

	hits, err := u.SearchFields([]string{"s"}, "zebracorn")
	if err != nil {
		t.Fatal(err)
	}

	if !hits[h1] || !hits[m.GUID] || len(hits) != 2 {
		t.Errorf("search hits %v, want %s and %s", hits, h1, m.GUID)
	}
}

// TestFreshIndexMarkedInCAS: the first ids of a fresh index carry the
// version marker; the view then knows that the index has ids, and a
// later delivery keeps the marker.
func TestFreshIndexMarkedInCAS(t *testing.T) {
	s := testStore(t)
	name := testAccount(t)

	for i := range 2 {
		u, err := s.User(name)
		if err != nil {
			t.Fatal(err)
		}

		if i == 0 {
			mustMailbox(t, u, "INBOX")
		}

		if _, err := u.Deliver("INBOX", testMessage(fmt.Sprintf("fresh%d", i), 300), nil, time.Time{}); err != nil {
			t.Fatal(err)
		}

		if !u.ftsHasIDs.Load() {
			t.Errorf("delivery %d: the view does not know that the index has ids", i)
		}

		if v := omapKeys(t, u, oidFTSMap, ftsVersionKey)[ftsVersionKey]; string(v) != ftsVersion {
			t.Errorf("delivery %d: version marker %q, want %q", i, v, ftsVersion)
		}

		if n := omapKeys(t, u, oidFTSMap, "n")["n"]; string(n) != strconv.Itoa(i+2) {
			t.Errorf("delivery %d: next id %q, want %d", i, n, i+2)
		}

		u.Close()
	}
}

// TestConcurrentDeliveriesIndexed: deliveries into one account at the
// same time (each through a view of its own, as LMTP does) contend for
// the same posting shards and id map; every message gets its own id
// and is in the posting lists.
func TestConcurrentDeliveriesIndexed(t *testing.T) {
	const (
		workers = 8
		each    = 3
	)

	s := testStore(t)
	name := testAccount(t)

	setup, err := s.User(name)
	if err != nil {
		t.Fatal(err)
	}

	mustMailbox(t, setup, "INBOX")
	setup.Close()

	guids := deliverConcurrently(t, s, name, workers, each)
	u := testUserNamed(t, s, name)

	want := idsOf(t, u, guids...)
	if len(slices.Compact(slices.Clone(want))) != workers*each {
		t.Fatalf("document ids %v are not unique", want)
	}

	if got := postingIDs(t, u, "b:lorem"); !slices.Equal(got, want) {
		t.Errorf("b:lorem ids %v, want %v", got, want)
	}

	if n := omapKeys(t, u, oidFTSMap, "n")["n"]; string(n) != strconv.Itoa(workers*each+1) {
		t.Errorf("next id %q, want %d", n, workers*each+1)
	}

	if v := omapKeys(t, u, oidFTSMap, ftsVersionKey)[ftsVersionKey]; string(v) != ftsVersion {
		t.Errorf("version marker %q, want %q", v, ftsVersion)
	}
}

// deliverConcurrently delivers each messages in each of workers
// goroutines into the account, a view per delivery, and returns the
// GUIDs of the messages.
func deliverConcurrently(t *testing.T, s *Store, name string, workers, each int) []string {
	t.Helper()

	var (
		wg    sync.WaitGroup
		mu    sync.Mutex
		guids []string
		errs  = make(chan error, workers*each)
	)

	for w := range workers {
		wg.Add(1)

		go func() {
			defer wg.Done()

			for i := range each {
				u, err := s.User(name)
				if err != nil {
					errs <- err
					return
				}

				m, err := u.Deliver("INBOX", testMessage(fmt.Sprintf("conc%dx%d", w, i), 300), nil, time.Time{})
				u.Close()

				if err != nil {
					errs <- err
					return
				}

				mu.Lock()

				guids = append(guids, m.GUID)
				mu.Unlock()
			}
		}()
	}

	wg.Wait()
	close(errs)

	for err := range errs {
		t.Fatal(err)
	}

	return guids
}

// testUserNamed opens the account name of s, closed with the test.
func testUserNamed(t *testing.T, s *Store, name string) *User {
	t.Helper()

	u, err := s.User(name)
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(u.Close)

	return u
}

// TestCASClass: the object class of the retry counter is one of a small
// fixed set.
func TestCASClass(t *testing.T) {
	for oid, want := range map[string]string{
		"fts/03":         casClassFTS,
		"ftst/127":       casClassFTS,
		"ftsh/01":        casClassFTS,
		"ftsmap":         casClassFTSMap,
		"mailboxes":      casClassRegistry,
		"account":        casClassAccount,
		"mbox/7":         casClassMbox,
		"mbox/7/2":       casClassMbox,
		"msg/abc/0":      casClassMsg,
		"ix/seen/03":     casClassIndex,
		"ix.accounts/00": casClassIndex,
		"metadata":       casClassOther,
		"sieve":          casClassOther,
	} {
		if got := casClass(oid); got != want {
			t.Errorf("casClass(%q) = %q, want %q", oid, got, want)
		}
	}
}

// TestCASBackoff: the wait grows with the rounds lost and stays within
// its bounds.
func TestCASBackoff(t *testing.T) {
	for attempt := range casRetries {
		lo, hi := casBackoffMin*time.Duration(attempt+1), casBackoffMax*time.Duration(attempt+1)

		for range 50 {
			if d := casBackoff(attempt); d < lo || d > hi {
				t.Fatalf("casBackoff(%d) = %v, want %v to %v", attempt, d, lo, hi)
			}
		}
	}
}
