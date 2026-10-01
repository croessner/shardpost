//go:build tentacle

package cephstore

import (
	"context"
	"errors"
	"testing"
	"time"

	"shardpost/internal/model"
)

// sweepFixture is an account with live data and debris from before the
// intents, a namespace without registry that is being purged, one
// without registry and directory entry, and a Sieve-only account from
// before 0.42.0 (scripts, no registry).
type sweepFixture struct {
	u       *User
	live    []Meta
	legacy  string // blob
	mbox    string // index object
	marks   string // marks of a public folder that does not exist
	adopted string // id of a live public folder whose marks have no ps:
	purging *User  // no registry, directory entry purging
	unknown *User  // no registry, no directory entry
	sieve   *User  // no registry, a Sieve object
}

func newSweepFixture(t *testing.T, s *Store) *sweepFixture {
	t.Helper()

	f := &sweepFixture{u: testUser(t, s)}
	mustMailbox(t, f.u, "INBOX")
	f.live = deliverN(t, f.u, "INBOX", 5, 2500)
	f.legacy, f.mbox, f.marks = legacyDebris(t, f.u)

	f.adopted = legacyLiveMarks(t, s, f.u)
	f.purging = leftovers(t, s)
	f.unknown = leftovers(t, s)
	f.sieve = testUser(t, s)

	// Written the way ManageSieve did before 0.42.0: no registry.
	if err := casOmap(f.sieve, f.sieve.idx, oidSieve, nil, func(map[string][]byte) (map[string][]byte, []string) {
		return map[string][]byte{sieveKeyPrefix + "s": []byte("keep;")}, nil
	}); err != nil {
		t.Fatal(err)
	}

	ix, release, err := s.AccountIndex()
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	if err := model.MarkAccountPurging(ix, f.purging.name, "job-x", "", time.Now()); err != nil {
		t.Fatal(err)
	}

	return f
}

// leftovers is a namespace without registry holding a blob and an index
// object.
func leftovers(t *testing.T, s *Store) *User {
	t.Helper()

	u := testUser(t, s)

	if _, err := u.writeBlob(newGUID(), testMessage("left", 300)); err != nil {
		t.Fatal(err)
	}

	if err := u.createIndexObjectSized(&MailboxRec{ID: newGUID(), UIDValidity: 1}, 4); err != nil {
		t.Fatal(err)
	}

	return u
}

// checkReportOnly verifies the report of a sweep without --repair.
func checkReportOnly(t *testing.T, rep *SweepReport, f *sweepFixture) {
	t.Helper()

	if rep.Removed != 0 || rep.Remnants[remnantBlob] < 1 || rep.Remnants[remnantMailbox] < 1 || rep.Remnants[remnantMarks] < 1 ||
		rep.Remnants[remnantNamespace] < 5 || rep.Adopted < 1 || rep.Unregistered < 2 {
		t.Fatalf("report-only sweep: %+v", *rep)
	}

	if !blobExistsT(f.u, f.legacy) {
		t.Fatal("a sweep without --repair deleted")
	}
}

// check verifies what a repairing sweep leaves: the live data, the
// adopted marks with their ps: key, the namespaces without registry that
// are not being purged, and nothing of the debris.
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

	f.checkUnregistered(t)
}

// checkUnregistered verifies the namespaces without registry: the one
// being purged is gone, the others are kept.
func (f *sweepFixture) checkUnregistered(t *testing.T) {
	t.Helper()

	if left := append(objects(t, f.purging.idx), objects(t, f.purging.data)...); len(left) > 0 {
		t.Fatalf("objects of the purged namespace left: %v", left)
	}

	if n := len(objects(t, f.unknown.idx)) + len(objects(t, f.unknown.data)); n != 2 {
		t.Fatalf("the namespace without directory entry has %d objects, want its 2", n)
	}

	if scripts, _, err := f.sieve.ListScripts(); err != nil || len(scripts) != 1 {
		t.Fatalf("the Sieve-only account lost its script: %v %v", scripts, err)
	}
}

func TestSweepReportsThenRepairs(t *testing.T) {
	s := testStore(t)
	f := newSweepFixture(t, s)

	rep, err := s.Sweep(context.Background(), SweepOptions{Restart: true})
	if err != nil {
		t.Fatal(err)
	}

	checkReportOnly(t, rep, f)

	rep, err = s.Sweep(context.Background(), SweepOptions{Repair: true, Batch: 7})
	if err != nil {
		t.Fatal(err)
	}

	// Other tests leave a namespace whose model cannot be read; the sweep
	// reports it and leaves it alone.
	if rep.Removed < 5 || len(rep.Errors) > 1 {
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
