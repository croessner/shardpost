//go:build tentacle

package cephstore

import (
	"context"
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

	if _, err := s.Sweep(context.Background(), SweepOptions{Repair: true, Restart: true, afterLoad: hook}); err != nil {
		t.Fatal(err)
	}

	if !created {
		t.Fatal("the hook did not run")
	}

	for _, oid := range left {
		if !u.objectExists(oid) {
			t.Fatalf("%s of the account created again was deleted", oid)
		}
	}

	if _, err := u.Lookup("INBOX"); err != nil || !u.objectExists(oidRegistry) {
		t.Fatalf("the new account lost its mailbox or registry: %v", err)
	}
}
