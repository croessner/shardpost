//go:build tentacle

package cephstore

import (
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"shardpost/internal/config"
	"shardpost/internal/model"
	"shardpost/internal/quota"
)

// Tests of the online quota recount (stage B16, temp/16 §8). The test
// store has a shard size of 4, so a few messages span several shards.

// recountStore is a store with a quota rule that ignores Trash and a
// short dead-intent time.
func recountStore(t *testing.T) *Store {
	t.Helper()

	return storeWith(t, func(c *Config) {
		c.QuotaRules = quota.NewPolicy(config.Quota{Rules: []config.QuotaRule{{Mailbox: "Trash", Ignore: true}}})
		c.QuotaRecount = QuotaRecountConfig{Barrier: 20 * time.Second, DeadIntent: 10 * time.Second, Hold: 500 * time.Millisecond}
	})
}

// view opens another view of the user's account (closed with the test).
func view(t *testing.T, u *User) *User {
	t.Helper()

	v, err := u.store.User(u.name)
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(v.Close)

	return v
}

// quietCount is RecountQuota(false) of the account.
func quietCount(t *testing.T, u *User) Quota {
	t.Helper()

	counted, _, err := u.RecountQuota(false)
	if err != nil {
		t.Fatal(err)
	}

	return counted
}

// usage is the stored usage.
func usage(t *testing.T, u *User) Quota {
	t.Helper()

	q, err := u.QuotaUsage()
	if err != nil {
		t.Fatal(err)
	}

	return q
}

// exact fails unless the stored usage is what a quiet recount says.
func exact(t *testing.T, u *User, what string) {
	t.Helper()

	if got, want := usage(t, u), quietCount(t, u); got != want {
		t.Fatalf("%s: usage %+v, quiet recount %+v", what, got, want)
	}
}

// drift puts the stored counter off by the given amounts.
func drift(t *testing.T, u *User, bytes, msgs int64) {
	t.Helper()

	q := usage(t, u)
	if err := u.SetQuotaUsage(Quota{Bytes: q.Bytes + bytes, Msgs: q.Msgs + msgs}); err != nil {
		t.Fatal(err)
	}
}

// onlineRecount runs an online recount on a view of its own.
func onlineRecount(t *testing.T, u *User, tok string, hooks *recountHooks) (model.QuotaRecountReport, error) {
	t.Helper()

	r := view(t, u)
	r.hooks = hooks

	return r.RecountQuotaOnline(model.QuotaRecountRequest{Tok: tok})
}

// hookAt is a hook that runs fn at the step.
func hookAt(step string, fn func()) *recountHooks {
	return &recountHooks{step: func(s, _ string) {
		if s == step {
			fn()
		}
	}}
}

// noCaptureKeys fails when a run key or captures are left.
func noCaptureKeys(t *testing.T, u *User) {
	t.Helper()

	for _, p := range []string{regQuotaRun, regQuotaCaptureHead, regQuotaReleasePrefix, regQuotaMailboxPrefix} {
		if keys := registryKeys(t, u, p); len(keys) != 0 {
			t.Errorf("%s left: %v", p, keys)
		}
	}
}

// removalOf reads the removal intent of a UID (zero when absent).
func removalOf(t *testing.T, u *User, id string, uid uint32) removeIntent {
	t.Helper()

	rs, err := u.removeIntents(id)
	if err != nil {
		t.Fatal(err)
	}

	return rs[removeKey(uid)]
}

// waitFor polls cond for a few seconds.
func waitFor(t *testing.T, cond func() bool) {
	t.Helper()

	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met in time")
		}

		time.Sleep(20 * time.Millisecond)
	}
}

// setRegistry writes and removes registry keys.
func setRegistry(t *testing.T, u *User, set map[string][]byte, rm []string) {
	t.Helper()

	if err := casOmap(u, u.idx, oidRegistry, nil, func(map[string][]byte) (map[string][]byte, []string) { return set, rm }); err != nil {
		t.Fatal(err)
	}
}

// --- casOmapPrefix ------------------------------------------------------------

// TestCasOmapPrefix: one read covers the keys and the first page of
// every prefix; a write between the read and the write makes the round
// lose and run again; a prefix beyond one page reports more.
func TestCasOmapPrefix(t *testing.T) {
	u := testUser(t, testStore(t))
	mustMailbox(t, u, "INBOX")

	set := map[string][]byte{"x:a": []byte("1"), "x:b": []byte("2"), "y:c": []byte("3")}
	for i := range prefixPageSize + 5 {
		set[fmt.Sprintf("z:%05d", i)] = []byte("z")
	}

	if err := u.setIntent(set); err != nil {
		t.Fatal(err)
	}

	rounds := 0

	err := casOmapPrefix(u, u.idx, oidRegistry, []string{"y:c", "missing"}, []string{"x:", "z:"},
		func(cur map[string][]byte, pages map[string]prefixPage) (map[string][]byte, []string) {
			rounds++
			checkPrefixRead(t, rounds, cur, pages)

			if rounds == 1 {
				// A write between the read and the write of this round.
				if err := rmKeys(u.idx, oidRegistry, []string{"x:b"}); err != nil {
					t.Fatal(err)
				}
			}

			return map[string][]byte{"x:d": []byte("4")}, []string{"x:a"}
		})
	if err != nil {
		t.Fatal(err)
	}

	if rounds != 2 {
		t.Fatalf("%d rounds, want 2 (one lost to the write)", rounds)
	}

	if keys := registryKeys(t, u, "x:"); len(keys) != 1 || keys[0] != "x:d" {
		t.Fatalf("x keys %v", keys)
	}
}

// checkPrefixRead checks what one round of TestCasOmapPrefix read.
func checkPrefixRead(t *testing.T, round int, cur map[string][]byte, pages map[string]prefixPage) {
	t.Helper()

	if string(cur["y:c"]) != "3" || len(cur) != 1 {
		t.Errorf("keys %v", cur)
	}

	if x := pages["x:"]; len(x.vals) != 3-round || x.more || string(x.vals["x:a"]) != "1" {
		t.Errorf("round %d: x page %+v", round, x)
	}

	if z := pages["z:"]; len(z.vals) != prefixPageSize || !z.more {
		t.Errorf("z page %d more %v", len(z.vals), z.more)
	}
}

// --- the run ------------------------------------------------------------------

// TestRecountOnlineQuiet: a run on a quiet account replaces a drifted
// counter by the count of a quiet recount (the ignored mailbox counts
// nowhere), reports what it did and leaves nothing behind.
func TestRecountOnlineQuiet(t *testing.T) {
	u := testUser(t, recountStore(t))
	mustMailbox(t, u, "INBOX")
	mustMailbox(t, u, "Trash")
	deliverN(t, u, "INBOX", 6, 300)
	deliverN(t, u, "Trash", 2, 300)

	want := quietCount(t, u)
	drift(t, u, 1234, 3)

	rep, err := onlineRecount(t, u, "job-1/1", nil)
	if err != nil {
		t.Fatal(err)
	}

	if rep.Counted != want || !rep.Differs || rep.Result != model.QuotaRecountOK || rep.ListedMailboxes != 2 || rep.SwitchRounds != 1 {
		t.Fatalf("report %+v, want counted %+v", rep, want)
	}

	exact(t, u, "after the run")
	noCaptureKeys(t, u)

	// A second run finds nothing to change.
	if rep, err := onlineRecount(t, u, "job-1/2", nil); err != nil || rep.Differs {
		t.Fatalf("second run %+v %v", rep, err)
	}
}

// writers run the concurrent writers of TestRecountOnlineConcurrent
// until stop; each runs on a view of its own.
type writers struct {
	stop atomic.Bool
	wg   sync.WaitGroup
}

func (ws *writers) run(t *testing.T, u *User, fn func(w *User, i int)) {
	t.Helper()

	w := view(t, u)

	ws.wg.Add(1)

	go func() {
		defer ws.wg.Done()

		for i := 0; !ws.stop.Load(); i++ {
			fn(w, i)
		}
	}()
}

func (ws *writers) end() {
	ws.stop.Store(true)
	ws.wg.Wait()
}

// deliverWriter delivers into INBOX and Trash.
func deliverWriter(w *User, i int) {
	box := "INBOX"
	if i%3 == 0 {
		box = "Trash"
	}

	_, _ = w.Deliver(box, testMessage("c"+strconv.Itoa(i), 200+rand.IntN(400)), nil, time.Time{})
}

// expungeWriter expunges the newest INBOX messages.
func expungeWriter(w *User, _ int) {
	mb, _ := w.Lookup("INBOX")

	metas, _ := w.ListMessages(mb.ID)
	if len(metas) > 3 {
		_ = w.Expunge("INBOX", metas[len(metas)-3:])
	}

	time.Sleep(5 * time.Millisecond)
}

// moveWriter copies and moves between Work, Old and Trash.
func moveWriter(w *User, i int) {
	mb, _ := w.Lookup("Work")

	metas, _ := w.ListMessages(mb.ID)
	if len(metas) == 0 {
		return
	}

	switch i % 4 {
	case 0:
		_, _ = w.Copy(metas[:1], "Old")
	case 1:
		_, _ = w.Move("Work", metas[len(metas)-1:], "Old")
	case 2:
		old, _ := w.Lookup("Old")
		if om, _ := w.ListMessages(old.ID); len(om) > 0 {
			_, _ = w.Move("Old", om[:1], "Work")
		}
	default:
		_, _ = w.Move("Work", metas[:1], "Trash")
	}
}

// flagWriter changes flags in Work.
func flagWriter(w *User, _ int) {
	mb, _ := w.Lookup("Work")

	if metas, _ := w.ListMessages(mb.ID); len(metas) > 0 {
		_, _ = w.SetFlags("Work", []uint32{metas[0].UID}, 0, func([]string) []string { return []string{`\Seen`} })
	}

	time.Sleep(5 * time.Millisecond)
}

// TestRecountOnlineConcurrent: deliveries, expunges over several shards,
// copies, moves between counted mailboxes and into the ignored one, a
// mailbox deletion and flag changes run beside the recount; afterwards
// the counters are what a quiet recount says.
func TestRecountOnlineConcurrent(t *testing.T) {
	u := testUser(t, recountStore(t))

	for _, n := range []string{"INBOX", "Work", "Old", "Trash", "Gone"} {
		mustMailbox(t, u, n)
	}

	deliverN(t, u, "INBOX", 9, 300)
	deliverN(t, u, "Work", 7, 300)
	deliverN(t, u, "Gone", 5, 300)
	drift(t, u, -500, -1)

	var ws writers

	ws.run(t, u, deliverWriter)
	ws.run(t, u, expungeWriter)
	ws.run(t, u, moveWriter)
	ws.run(t, u, flagWriter)

	// The run is slowed down a little, so that the writers interleave
	// with its phases; one deletion halfway.
	hooks := &recountHooks{step: func(step, _ string) {
		switch step {
		case hookAfterBarrier:
			time.Sleep(50 * time.Millisecond)
		case hookBeforeMailbox:
			time.Sleep(20 * time.Millisecond)
		}
	}}

	deleter := view(t, u)

	go func() {
		time.Sleep(60 * time.Millisecond)

		_ = deleter.DeleteMailbox("Gone")
	}()

	rep, err := onlineRecount(t, u, "job-1/1", hooks)

	ws.end()

	if err != nil {
		t.Fatalf("run: %v (%+v)", err, rep)
	}

	// Writers that were in flight when the run switched have settled now.
	u.flushIntents(false)

	exact(t, u, fmt.Sprintf("after the run (report %+v)", rep))
}

// TestRecountReleaseAfterSwitch: an expunge whose removal lies before
// the switch and whose release after it: the run counts the entry (its
// removal intent says removed and not released), the release takes it
// out, the end is exact.
func TestRecountReleaseAfterSwitch(t *testing.T) {
	u := testUser(t, recountStore(t))
	mb := mustMailbox(t, u, "INBOX")
	metas := deliverN(t, u, "INBOX", 6, 300) // shards 0 and 1

	switched := make(chan struct{})
	unlinked := make(chan struct{})

	exp := view(t, u)
	exp.hooks = hookAt(hookAfterUnlink, func() {
		close(unlinked)
		<-switched
	})

	go func() { _ = exp.Expunge("INBOX", metas[1:5]) }() // one on shard 0, three on shard 1

	<-unlinked

	// The intents say removed, not released.
	for _, m := range metas[1:5] {
		if ri := removalOf(t, u, mb.ID, m.UID); !ri.Removed || ri.Released || ri.Nonce == "" {
			t.Errorf("uid %d after unlink: %+v", m.UID, ri)
		}
	}

	rep, err := onlineRecount(t, u, "job-1/1", hookAt(hookAfterSwitch, func() { close(switched) }))
	if err != nil {
		t.Fatal(err)
	}

	// The run counted the removed entries: nothing was released yet.
	if want := sizeOf(metas); rep.Counted != want {
		t.Fatalf("counted %+v, want %+v (removed, not released)", rep.Counted, want)
	}

	waitFor(t, func() bool {
		ri := removalOf(t, u, mb.ID, metas[1].UID)
		return ri.GUID == "" || ri.Released
	})

	time.Sleep(200 * time.Millisecond)
	exact(t, u, "after the release")
}

// TestMoveIntentsReleasedAtOnce: a MOVE between counted mailboxes marks
// its source intents released at once, so a run never counts them.
func TestMoveIntentsReleasedAtOnce(t *testing.T) {
	u := testUser(t, recountStore(t))
	mb := mustMailbox(t, u, "INBOX")
	mustMailbox(t, u, "Work")
	metas := deliverN(t, u, "INBOX", 2, 300)

	hold := make(chan struct{})
	mover := view(t, u)
	mover.hooks = hookAt(hookAfterUnlink, func() { <-hold })

	go func() { _, _ = mover.Move("INBOX", metas[:1], "Work") }()

	waitFor(t, func() bool { return removalOf(t, u, mb.ID, metas[0].UID).Removed })

	if ri := removalOf(t, u, mb.ID, metas[0].UID); !ri.Released {
		t.Fatalf("move source intent %+v, want released at once", ri)
	}

	close(hold)
}

// TestRecountBusy: an import lock, a mailbox lock before destroying, a
// live scrub mark and a foreign live run make the start busy; expired
// ones and a lock in the phase destroying do not.
func TestRecountBusy(t *testing.T) {
	u := testUser(t, recountStore(t))
	mustMailbox(t, u, "INBOX")
	deliverN(t, u, "INBOX", 2, 300)

	now := time.Now()
	later, earlier := now.Add(time.Minute).Unix(), now.Add(-time.Minute).Unix()

	for _, c := range []struct {
		name   string
		set    map[string][]byte
		rm     []string
		reason string
	}{
		{"import lock", map[string][]byte{regImportLock: encode(importLock{Session: "imp", Expires: later})}, nil, model.BusyImportLock},
		{"expired import lock", map[string][]byte{regImportLock: encode(importLock{Session: "imp", Expires: earlier})}, nil, ""},
		{"mailbox lock", map[string][]byte{mailboxLockKey("job-x"): encode(mailboxLockRec{Exp: later, Phase: model.MailboxLockLocked})},
			[]string{regImportLock}, model.BusyMailboxLck},
		{"destroying", map[string][]byte{mailboxLockKey("job-x"): encode(mailboxLockRec{Exp: later, Phase: model.MailboxLockDestroying})}, nil, ""},
		{"scrub mark", map[string][]byte{scrubMarkKey("s"): encode(scrubMark{Holder: "s", Exp: later})}, []string{mailboxLockKey("job-x")}, model.BusyScrub},
		{"expired scrub mark", map[string][]byte{scrubMarkKey("s"): encode(scrubMark{Holder: "s", Exp: earlier})}, nil, ""},
		{"foreign run", map[string][]byte{regQuotaRun: encode(quotaRun{Tok: "job-9/1", H: "x", Exp: later})}, []string{scrubMarkKey("s")}, model.BusyForeignRun},
		{"expired run", map[string][]byte{regQuotaRun: encode(quotaRun{Tok: "job-9/1", H: "x", Exp: earlier})}, nil, ""},
	} {
		setRegistry(t, u, c.set, c.rm)

		_, err := onlineRecount(t, u, "job-1/1", nil)

		var busy *model.QuotaRecountBusyError

		switch {
		case c.reason == "" && err != nil:
			t.Errorf("%s: %v", c.name, err)
		case c.reason != "" && (!errors.As(err, &busy) || busy.Reason != c.reason):
			t.Errorf("%s: %v, want busy %s", c.name, err, c.reason)
		}
	}
}

// TestRecountBarrierIntents: a reservation of before the run (its
// intent stands) is waited for; once it is gone the run goes on. A dead
// one (older than the dead-intent time) is counted and passed.
func TestRecountBarrierIntents(t *testing.T) {
	u := testUser(t, recountStore(t))
	mb := mustMailbox(t, u, "INBOX")
	deliverN(t, u, "INBOX", 2, 300)

	bi := blobIntentKey(newGUID())
	if err := u.setIntent(map[string][]byte{bi: u.newBlobIntent(100, mb.ID)}); err != nil {
		t.Fatal(err)
	}

	removed := make(chan struct{})

	go func() {
		defer close(removed)

		time.Sleep(3 * barrierPoll / 2)

		_ = rmKeys(u.idx, oidRegistry, []string{bi})
	}()

	rep, err := onlineRecount(t, u, "job-1/2", nil)

	<-removed

	if err != nil || rep.BarrierSeconds < barrierPoll.Seconds() || rep.DeadIntents != 0 {
		t.Fatalf("waited %+v %v", rep, err)
	}

	old := encode(blobIntent{Time: time.Now().Add(-time.Hour).Unix(), Size: 100, Chunks: 1, Mailbox: mb.ID})
	if err := u.setIntent(map[string][]byte{bi: old}); err != nil {
		t.Fatal(err)
	}

	if rep, err := onlineRecount(t, u, "job-1/3", nil); err != nil || rep.DeadIntents != 1 {
		t.Fatalf("dead intent %+v %v", rep, err)
	}

	_ = rmKeys(u.idx, oidRegistry, []string{bi})
}

// TestRecountLost: an offline recount (SetQuotaUsage) and a rename across
// the quota rules end a live run lost; the counters are untouched by
// the run.
func TestRecountLost(t *testing.T) {
	u := testUser(t, recountStore(t))
	mustMailbox(t, u, "INBOX")
	mustMailbox(t, u, "Keep")
	deliverN(t, u, "Keep", 3, 300)

	for name, abort := range map[string]func(){
		"set usage": func() { _ = u.SetQuotaUsage(usage(t, u)) },
		"rename":    func() { _ = u.RenameMailbox("Keep", "Trash") },
	} {
		before := usage(t, u)

		if _, err := onlineRecount(t, u, "job-1/1", hookAt(hookAfterBarrier, abort)); !errors.Is(err, model.ErrQuotaRecountLost) {
			t.Errorf("%s: %v, want lost", name, err)
		}

		if keys := registryKeys(t, u, regQuotaRun); len(keys) != 0 {
			t.Errorf("%s: run key left", name)
		}

		if name == "set usage" && usage(t, u) != before {
			t.Errorf("%s: counters changed", name)
		}
	}

	// After the rename Keep is Trash (ignored): the quiet recount agrees
	// with the booking of the rename.
	exact(t, u, "after the rename")
}

// TestClearQuotaRecountAndLeftover: captures without a run are found by
// the scrub and cleared; a clear never touches the captures of a live
// run of another token.
func TestClearQuotaRecountAndLeftover(t *testing.T) {
	u := testUser(t, recountStore(t))
	mustMailbox(t, u, "INBOX")
	deliverN(t, u, "INBOX", 1, 300)

	expired := quotaRun{Tok: "job-old/1", H: quotaRunHash("job-old/1"), Exp: time.Now().Add(-time.Minute).Unix()}
	set := map[string][]byte{regQuotaRun: encode(expired), regQuotaCaptureHead: encode(quotaCaptureHead{Tok: expired.Tok, N: 2}),
		quotaReleaseKey(1): encode(quotaRelease{H: expired.H}), quotaReleaseKey(2): encode(quotaRelease{H: expired.H}),
		quotaMailboxKey("m1"): encode(quotaMailboxRelease{H: expired.H})}

	if err := u.setIntent(set); err != nil {
		t.Fatal(err)
	}

	if rep := scrubNow(t, u); !rep.QuotaCaptureLeftover {
		t.Fatalf("leftover not found: %+v", rep)
	}

	noCaptureKeys(t, u)

	// A live run of another token keeps its captures against a clear by
	// token, and loses them to a clear by the lock holder ("").
	live := quotaRun{Tok: "job-live/1", H: quotaRunHash("job-live/1"), Exp: time.Now().Add(time.Minute).Unix()}
	if err := u.setIntent(map[string][]byte{regQuotaRun: encode(live), quotaReleaseKey(1): encode(quotaRelease{H: live.H})}); err != nil {
		t.Fatal(err)
	}

	if err := u.ClearQuotaRecount("job-other/1"); err != nil || len(registryKeys(t, u, regQuotaReleasePrefix)) != 1 {
		t.Fatalf("clear by another token: %v, captures %v", err, registryKeys(t, u, regQuotaReleasePrefix))
	}

	if err := u.ClearQuotaRecount(""); err != nil {
		t.Fatalf("clear by the lock holder: %v", err)
	}

	noCaptureKeys(t, u)
}

// TestScrubSkipsLiveRun: a scrub skips an account with a live run.
func TestScrubSkipsLiveRun(t *testing.T) {
	u := testUser(t, recountStore(t))
	mustMailbox(t, u, "INBOX")

	live := quotaRun{Tok: "job-live/1", H: quotaRunHash("job-live/1"), Exp: time.Now().Add(time.Minute).Unix()}
	if err := u.setIntent(map[string][]byte{regQuotaRun: encode(live)}); err != nil {
		t.Fatal(err)
	}

	rep, err := u.Scrub(ScrubOptions{})
	if err != nil || rep.OnlineSkipped != skipRecount || rep.Complete() {
		t.Fatalf("scrub under a run: %+v %v", rep, err)
	}
}

// TestScrubMarkBlocksRun: a scrub that repairs sets its mark first,
// which makes the start of a run busy; the mark goes with the scrub.
func TestScrubMarkBlocksRun(t *testing.T) {
	u := testUser(t, recountStore(t))
	mustMailbox(t, u, "INBOX")
	metas := deliverN(t, u, "INBOX", 2, 300)

	// An entry without blob makes the scrub repair (dropEntry).
	if !u.deleteBlobAll(metas[0].GUID, metas[0].Chunks) {
		t.Fatal("blob not deleted")
	}

	var (
		marked  bool
		busyErr error
	)

	time.Sleep(1100 * time.Millisecond)

	rep, err := u.Scrub(ScrubOptions{afterMark: func() {
		marked = true
		_, busyErr = onlineRecount(t, u, "job-1/1", nil)
	}})
	if err != nil || rep.MissingBlobs != 1 {
		t.Fatalf("scrub %+v %v", rep, err)
	}

	if !marked {
		t.Fatal("the scrub repaired without a mark")
	}

	var busy *model.QuotaRecountBusyError
	if !errors.As(busyErr, &busy) || busy.Reason != model.BusyScrub {
		t.Fatalf("run beside a repairing scrub: %v", busyErr)
	}

	if keys := registryKeys(t, u, regScrubMarkPrefix); len(keys) != 0 {
		t.Fatalf("mark left: %v", keys)
	}

	if _, err := onlineRecount(t, u, "job-1/2", nil); err != nil {
		t.Fatalf("after the scrub: %v", err)
	}
}

// tombstoneQ reads q of the mailbox's tombstone (-1 when absent).
func tombstoneQ(t *testing.T, u *User, id string) int {
	t.Helper()

	var tm mailboxIntent

	v, ok := omapKeys(t, u, oidRegistry, deleteKey(id))[deleteKey(id)]
	if !ok || json.Unmarshal(v, &tm) != nil || tm.Released == nil {
		return -1
	}

	return *tm.Released
}

// TestTombstoneKeepsReleased: a deletion marks its tombstone released
// in the release CAS; a later rewrite of the tombstone keeps q.
func TestTombstoneKeepsReleased(t *testing.T) {
	u := testUser(t, recountStore(t))
	mb := mustMailbox(t, u, "Box")
	deliverN(t, u, "Box", 2, 300)

	info, metas, err := u.readForDelete(mb)
	if err != nil {
		t.Fatal(err)
	}

	if _, lost, err := u.unregisterMailbox(mb, &lease{}, info, ""); err != nil || lost {
		t.Fatal(err, lost)
	}

	if q := tombstoneQ(t, u, mb.ID); q != 0 {
		t.Fatalf("tombstone after the unregistration: q %d", q)
	}

	if err := u.releaseDeleted(mb, metas); err != nil {
		t.Fatal(err)
	}

	if q := tombstoneQ(t, u, mb.ID); q != 1 {
		t.Fatalf("tombstone after the release: q %d", q)
	}

	u.keepTombstone(mb, info, "Box")

	if q := tombstoneQ(t, u, mb.ID); q != 1 {
		t.Fatalf("tombstone after keepTombstone: q %d", q)
	}

	if _, _, err := u.FinishDeletion(mb.ID); err != nil {
		t.Fatal(err)
	}

	exact(t, u, "after the deletion")
}

// TestRecountCaptureLimit: a writer that takes the captures beyond the
// limit ends the run, which is lost.
func TestRecountCaptureLimit(t *testing.T) {
	u := testUser(t, storeWith(t, func(c *Config) {
		c.QuotaRecount = QuotaRecountConfig{MaxCaptureKeys: 10, ReleaseChunk: 100}
	}))
	mustMailbox(t, u, "INBOX")
	metas := deliverN(t, u, "INBOX", 12, 200)

	w := view(t, u)

	_, err := onlineRecount(t, u, "job-1/1", hookAt(hookAfterBarrier, func() {
		for _, m := range metas {
			_ = w.Expunge("INBOX", []Meta{m}) // one capture key each
		}
	}))
	if !errors.Is(err, model.ErrQuotaRecountLost) {
		t.Fatalf("%v, want lost", err)
	}

	exact(t, u, "after the lost run")
}
