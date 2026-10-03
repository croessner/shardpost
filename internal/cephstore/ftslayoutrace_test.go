//go:build tentacle

package cephstore

import (
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"shardpost/internal/fts"
	"shardpost/internal/model"
)

// TestLayoutCrashResume (§10 crash hooks): a run that stops after any of
// P2, P3, P5, P6 or P7 is continued by the same token and ends with a
// clean record and the same search answers.
func TestLayoutCrashResume(t *testing.T) {
	s2 := testStore(t)
	docs := layoutDocs(8)

	ref := testUser(t, s2)
	mustMailbox(t, ref, "INBOX")
	deliverDocs(t, ref, docs)

	want := searchState(t, ref)

	for _, phase := range []string{phaseP2, phaseP3, phaseP5, phaseP6, phaseP7} {
		t.Run(phase, func(t *testing.T) {
			s := layout3Store(t, nil)
			u := testUser(t, s)
			mustMailbox(t, u, "INBOX")
			deliverDocs(t, u, docs)

			tok := "job-resume/" + phase
			crashAt(t, u.name, phase)

			if _, err := u.ConvertFTSLayout(model.FTSLayoutRequest{Layout: 3, Shards: 2, Tok: tok}); err == nil {
				t.Fatal("no crash")
			}

			ftsLayoutHook = nil

			if _, err := u.ConvertFTSLayout(model.FTSLayoutRequest{Layout: 3, Shards: 2, Tok: tok}); err != nil {
				t.Fatalf("resume: %v", err)
			}

			if l := layoutOf(t, u); l.Next != nil || len(l.Old) != 0 || l.S != 2 {
				t.Errorf("record %+v", l)
			}

			sameSearches(t, "after "+phase, want, searchState(t, u))
		})
	}
}

// TestLayoutLeaseLostBeforeSwitch (R4.1): a run whose fence fails after
// P5 does not switch: the record never names targets the run did not
// finish, and the conversion stays for the run that takes over.
func TestLayoutLeaseLostBeforeSwitch(t *testing.T) {
	s := layout3Store(t, nil)
	u := testUser(t, s)
	mustMailbox(t, u, "INBOX")
	deliverDocs(t, u, layoutDocs(4))

	before := curLoc(layoutOf(t, u), u.ftsShards)

	var p5 bool

	ftsLayoutHook = func(a, p string) error {
		if a == u.name && p == phaseP5 {
			p5 = true
		}

		return nil
	}

	t.Cleanup(func() { ftsLayoutHook = nil })

	errLost := errors.New("lease lost")
	fence := func() error {
		if p5 {
			return errLost
		}

		return nil
	}

	if _, err := u.ConvertFTSLayout(model.FTSLayoutRequest{Layout: 3, Shards: 2, Tok: "job-lost/1", Fence: fence}); !errors.Is(err, errLost) {
		t.Fatalf("run: %v", err)
	}

	l := layoutOf(t, u)
	if !curLoc(l, u.ftsShards).same(before) || l.Next == nil || l.Next.Tok != "job-lost/1" {
		t.Errorf("record after the lost lease %+v", l)
	}
}

// TestLayoutTakenOverRunFails (R4.1): after run B replaced run A's
// conversion, A's late P5 writes fail (its targets are gone) and its P6
// fails at the token check; B's conversion is untouched.
func TestLayoutTakenOverRunFails(t *testing.T) {
	s := layout3Store(t, nil)
	u := testUser(t, s)
	mustMailbox(t, u, "INBOX")
	deliverDocs(t, u, layoutDocs(4))

	src := curLoc(layoutOf(t, u), u.ftsShards)
	tokA, tokB := model.JobLockToken("job-y", 1), model.JobLockToken("job-y", 2)

	crashAt(t, u.name, phaseP5)

	if _, err := u.ConvertFTSLayout(model.FTSLayoutRequest{Layout: 3, Shards: 2, Tok: tokA}); err == nil {
		t.Fatal("no crash")
	}

	targetA := genLoc(*layoutOf(t, u).Next, u.ftsShards)

	if _, err := u.ConvertFTSLayout(model.FTSLayoutRequest{Layout: 3, Shards: 2, Tok: tokB}); err == nil {
		t.Fatal("B did not stop at its P5")
	}

	ftsLayoutHook = nil

	w := u.newPieceWriter(targetA, func() error { return nil })
	if err := w.add(targetA.postingOIDn(0), "b:late", nil); err != nil {
		t.Fatal(err)
	}

	if err := w.finish(); err == nil {
		t.Error("A's late write into its deleted target succeeded")
	}

	if err := u.phase6(model.FTSLayoutRequest{Tok: tokA, Fence: func() error { return nil }, Progress: func(string, int, int) {}},
		src, targetA, convertStats{}); !errors.Is(err, errConversionLost) {
		t.Errorf("A's P6: %v", err)
	}

	if l := layoutOf(t, u); !curLoc(l, u.ftsShards).same(src) || l.Next == nil || l.Next.Tok != tokB {
		t.Errorf("record %+v", l)
	}
}

// TestLayoutDeliveriesDuringConversion (§10 races): eight deliveries
// into an account while its conversion runs wait for the switch and land
// in the new generation; searches during the switch answer.
func TestLayoutDeliveriesDuringConversion(t *testing.T) {
	s := layout3Store(t, func(c *FTSConfig) { c.FrozenWait = 20 * time.Second })
	u := testUser(t, s)
	mustMailbox(t, u, "INBOX")
	deliverDocs(t, u, layoutDocs(4))

	cd := &concurrentDeliveries{}

	ftsLayoutHook = func(a, p string) error {
		switch {
		case a != u.name:
		case p == phaseP3:
			cd.start(s, u.name, 8)
			time.Sleep(200 * time.Millisecond) // the deliveries meet the freeze
		case p == phaseP6:
			// a search between the switch and the removal of the sources
			_, err := u.SearchFields([]string{fts.FieldBody}, "lorem")
			return err
		}

		return nil
	}

	t.Cleanup(func() { ftsLayoutHook = nil })

	if _, err := u.ConvertFTSLayout(model.FTSLayoutRequest{Layout: 3, Shards: 2, Tok: "job-during/1"}); err != nil {
		t.Fatal(err)
	}

	guids := cd.wait(t)

	u.ftsForget()

	hits, err := u.SearchFields([]string{fts.FieldBody}, "lorem")
	if err != nil {
		t.Fatal(err)
	}

	for _, g := range guids {
		if !hits[g] {
			t.Errorf("message %s delivered during the conversion is not found", g)
		}
	}

	// the substring search goes through the dictionary of the new
	// generation: the deliveries' new subject terms must be in it
	cands, err := u.SearchCandidates([]string{fts.FieldSubject}, "uring")
	if err != nil || cands == nil {
		t.Fatalf("substring search: %v %v", cands, err)
	}

	for _, g := range guids {
		if !cands[g] {
			t.Errorf("message %s delivered during the conversion is not a substring candidate", g)
		}
	}
}

// concurrentDeliveries are deliveries into one account at the same time,
// each through a view of its own.
type concurrentDeliveries struct {
	wg    sync.WaitGroup
	mu    sync.Mutex
	guids []string
	errs  []error
}

func (c *concurrentDeliveries) start(s *Store, account string, n int) {
	for i := range n {
		c.wg.Add(1)

		go func() {
			defer c.wg.Done()

			guid, err := deliverOnce(s, account, fmt.Sprintf("during%d", i))

			c.mu.Lock()
			defer c.mu.Unlock()

			c.errs = append(c.errs, err)

			if err == nil {
				c.guids = append(c.guids, guid)
			}
		}()
	}
}

// wait waits for the deliveries, fails the test on an error and returns
// the GUIDs.
func (c *concurrentDeliveries) wait(t *testing.T) []string {
	t.Helper()

	c.wg.Wait()

	for _, err := range c.errs {
		if err != nil {
			t.Errorf("delivery during the conversion: %v", err)
		}
	}

	return c.guids
}

// deliverOnce delivers a message through a view of its own.
func deliverOnce(s *Store, account, subject string) (string, error) {
	v, err := s.User(account)
	if err != nil {
		return "", err
	}
	defer v.Close()

	m, err := v.Deliver("INBOX", testMessage(subject, 400), nil, time.Time{})
	if err != nil {
		return "", err
	}

	return m.GUID, nil
}

// TestLayoutDictMissingElsewhereIgnored (N10): a dictionary write that
// finds its object gone in a generation the record no longer names is
// ignored; in the named generation it is an error.
func TestLayoutDictMissingElsewhereIgnored(t *testing.T) {
	s := layout3Store(t, nil)
	u := testUser(t, s)
	mustMailbox(t, u, "INBOX")
	deliverDocs(t, u, layoutDocs(2))

	old := curLoc(layoutOf(t, u), u.ftsShards)
	convert(t, u, model.FTSLayoutRequest{Layout: 3, Shards: 2})

	if _, err := u.dictApplyAt(old, []string{"b:latecomer"}, true); err != nil {
		t.Errorf("dictionary write into the removed generation: %v", err)
	}

	cur := curLoc(layoutOf(t, u), u.ftsShards)
	if err := u.deleteObjects(cur.dictOIDs()); err != nil {
		t.Fatal(err)
	}

	if _, err := u.dictApplyAt(cur, []string{"b:latecomer"}, true); !errors.Is(err, errFTSShardMissing) {
		t.Errorf("dictionary write into the named generation without its object: %v", err)
	}
}

// TestLayoutImportHint (N6): a fresh index of an import with a hint of
// 5000 messages gets the counts the bench corpus asks for.
func TestLayoutImportHint(t *testing.T) {
	s := layout3Store(t, nil)
	u := testUser(t, s)
	mustMailbox(t, u, "INBOX")

	u.SetImportHint(model.ImportHint{Messages: 5000})

	if _, err := u.Deliver("INBOX", testMessage("hinted", 300), nil, time.Time{}); err != nil {
		t.Fatal(err)
	}

	if l := layoutOf(t, u); l == nil || l.S != 1 || l.D != 8 || l.H != 1 {
		t.Errorf("record %+v, want S/D/H 1/8/1", l)
	}
}
