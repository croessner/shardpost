//go:build tentacle

package cephstore

import (
	"context"
	"errors"
	"testing"
)

// sweepFixture is an account with live data and debris from before the
// intents, and a namespace without registry.
type sweepFixture struct {
	u       *User
	live    []Meta
	legacy  string // blob
	mbox    string // index object
	marks   string // marks of a public folder that does not exist
	adopted string // id of a live public folder whose marks have no ps:
	gone    *User  // a namespace without registry
}

func newSweepFixture(t *testing.T, s *Store) *sweepFixture {
	t.Helper()

	f := &sweepFixture{u: testUser(t, s)}
	mustMailbox(t, f.u, "INBOX")
	f.live = deliverN(t, f.u, "INBOX", 5, 2500)
	f.legacy, f.mbox, f.marks = legacyDebris(t, f.u)

	f.adopted = legacyLiveMarks(t, s, f.u)

	gone, err := s.User(testAccount(t))
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(gone.Close)

	if _, err := gone.writeBlob(newGUID(), testMessage("left", 300)); err != nil {
		t.Fatal(err)
	}

	if err := gone.createIndexObjectSized(&MailboxRec{ID: newGUID(), UIDValidity: 1}, 4); err != nil {
		t.Fatal(err)
	}

	f.gone = gone

	return f
}

// check verifies what a repairing sweep leaves: the live data, the
// adopted marks with their ps: key, and nothing of the debris.
func (f *sweepFixture) check(t *testing.T) {
	t.Helper()

	metas, err := f.u.ListMessagesByName("INBOX")
	if err != nil || len(metas) != len(f.live) {
		t.Fatalf("live messages: %d, %v", len(metas), err)
	}

	for i := range metas {
		if raw, err := f.u.Read(&metas[i]); err != nil || int64(len(raw)) != metas[i].Size {
			t.Fatalf("live message %d not readable: %v", metas[i].UID, err)
		}
	}

	if blobExistsT(f.u, f.legacy) || f.u.objectExists(f.mbox) || f.u.objectExists(f.marks) {
		t.Fatal("debris left")
	}

	if !f.u.objectExists(oidPrivateSeen+f.adopted) || len(omapKeys(t, f.u, oidRegistry, pubSeenKey(f.adopted))) != 1 {
		t.Fatal("the marks of the live public folder were not adopted")
	}

	if left := append(objects(t, f.gone.idx), objects(t, f.gone.data)...); len(left) > 0 {
		t.Fatalf("objects of the namespace without registry left: %v", left)
	}
}

func TestSweepReportsThenRepairs(t *testing.T) {
	s := testStore(t)
	f := newSweepFixture(t, s)

	rep, err := s.Sweep(context.Background(), SweepOptions{Restart: true})
	if err != nil {
		t.Fatal(err)
	}

	if rep.Removed != 0 || rep.Remnants[remnantBlob] < 1 || rep.Remnants[remnantMailbox] < 1 || rep.Remnants[remnantMarks] < 1 ||
		rep.Remnants[remnantNamespace] < 2 || rep.Adopted < 1 {
		t.Fatalf("report-only sweep: %+v", *rep)
	}

	if !blobExistsT(f.u, f.legacy) {
		t.Fatal("a sweep without --repair deleted")
	}

	rep, err = s.Sweep(context.Background(), SweepOptions{Repair: true, Batch: 7})
	if err != nil {
		t.Fatal(err)
	}

	if rep.Removed < 5 || len(rep.Errors) > 0 {
		t.Fatalf("repairing sweep: %+v", *rep)
	}

	f.check(t)
}

// TestSweepResumes: a sweep stopped in pass 1 goes on from its
// checkpoint; one whose pool changed pg_num lists the pool again.
func TestSweepResumes(t *testing.T) {
	s := testStore(t)
	f := newSweepFixture(t, s)

	stops := 0
	lost := func() bool { stops++; return stops > 2 }

	sw, release, err := s.newSweeper(context.Background(), SweepOptions{Repair: true, Batch: 5, Restart: true}, lost)
	if err != nil {
		t.Fatal(err)
	}

	if err := sw.begin(); err != nil {
		t.Fatal(err)
	}

	if err := sw.listPool(); !errors.Is(err, errSweepStopped) {
		release()
		t.Fatalf("listPool: %v, want stopped", err)
	}

	if !sw.st.Positioned || sw.st.PGNum == 0 {
		release()
		t.Fatalf("no checkpoint: %+v", *sw.st)
	}

	// The pool "changed" its pg_num: the listing starts over.
	sw.st.PGNum++
	if err := sw.saveState(); err != nil {
		t.Fatal(err)
	}

	release()

	rep, err := s.Sweep(context.Background(), SweepOptions{Repair: true, Batch: 5})
	if err != nil {
		t.Fatal(err)
	}

	if rep.Restarts != 1 {
		t.Fatalf("restarts %d, want 1", rep.Restarts)
	}

	f.check(t)
}
