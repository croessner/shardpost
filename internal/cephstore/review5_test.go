//go:build tentacle

package cephstore

import (
	"context"
	"strings"
	"testing"
	"time"

	"shardpost/internal/jobs"
	"shardpost/internal/model"
)

// TestRepairForgetsPurged: a repair that activates an account again or
// removes its stale entry drops the purge record.
func TestRepairForgetsPurged(t *testing.T) {
	s := testStore(t)

	for _, action := range []string{model.RepairActivated, model.RepairStale} {
		name := testAccount(t)

		if err := setPurgedAt(s, name, time.Now()); err != nil {
			t.Fatal(err)
		}

		jobs.ForgetActivated(s, model.DirectoryRepair{Account: name, Action: action})

		if purged, err := s.purged(name); err != nil || purged {
			t.Fatalf("%s: record left (%v)", action, err)
		}
	}
}

// TestSweepRecheckBeforeDeleting: an account created again while the
// sweep looks at its purged namespace keeps everything; the sweep checks
// again before it deletes and takes nothing written after it read the
// namespace for a remnant.
func TestSweepRecheckBeforeDeleting(t *testing.T) {
	s := purgeStore(t, false)
	u := leftovers(t, s)

	if err := setPurgedAt(s, u.name, time.Now().Add(-2*time.Hour)); err != nil {
		t.Fatal(err)
	}

	left := objects(t, u.idx)

	time.Sleep(1100 * time.Millisecond)

	created := false
	hook := func(ns string) {
		if ns != u.name || created {
			return
		}

		created = true

		if err := u.CreateMailbox("INBOX", nil); err != nil {
			t.Error(err)
		}
	}

	rep, err := s.Sweep(context.Background(), SweepOptions{Repair: true, Restart: true, afterLoad: hook})
	if err != nil {
		t.Fatal(err)
	}

	// Counted as created again, its index objects not as remnants (in the
	// data pool, looked at after the re-creation, the account's model
	// applies: its old blob is debris of a live account).
	if rep.Recreated < 1 || sampled(rep, u.name+": "+mboxPrefix) {
		t.Fatalf("report: recreated %d, samples %v", rep.Recreated, rep.Samples)
	}

	if !created {
		t.Fatal("the hook did not run")
	}

	checkRecreated(t, u, left)
}

// checkRecreated checks that the account created again kept its old
// objects, its registry and its mailbox.
func checkRecreated(t *testing.T, u *User, left []string) {
	t.Helper()

	for _, oid := range left {
		if !u.objectExists(oid) {
			t.Fatalf("%s of the account created again was deleted", oid)
		}
	}

	if _, err := u.Lookup("INBOX"); err != nil || !u.objectExists(oidRegistry) {
		t.Fatalf("the new account lost its mailbox or registry: %v", err)
	}
}

// sampled reports whether a sample of the report starts with prefix.
func sampled(rep *SweepReport, prefix string) bool {
	for _, x := range rep.Samples {
		if strings.HasPrefix(x, prefix) {
			return true
		}
	}

	return false
}

// TestSweepKeepsFTSShardsAboveCount: a full-text shard above the count of
// this process may be a live one of servers configured with more; the
// sweep reports it and leaves it.
func TestSweepKeepsFTSShardsAboveCount(t *testing.T) {
	s := testStore(t)
	u := testUser(t, s)
	mustMailbox(t, u, "INBOX")

	oid := ftsPrefix + "40"
	if err := casOmap(u, u.idx, oid, nil, func(map[string][]byte) (map[string][]byte, []string) {
		return map[string][]byte{"s:term": []byte("x")}, nil
	}); err != nil {
		t.Fatal(err)
	}

	time.Sleep(1100 * time.Millisecond)

	rep, err := s.Sweep(context.Background(), SweepOptions{Repair: true, Restart: true})
	if err != nil {
		t.Fatal(err)
	}

	if !u.objectExists(oid) || rep.Remnants[remnantFTS] < 1 {
		t.Fatalf("shard kept %v, reported %d", u.objectExists(oid), rep.Remnants[remnantFTS])
	}
}
