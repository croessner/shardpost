package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"shardpost/internal/memstore"
	"shardpost/internal/model"
)

// Tests of quota_recalc online (stage B16) and of mailbox_delete with
// the mailbox lock (stage B17) over the memstore.

// marked is the value of both cluster marks.
type stageMarked bool

func (m stageMarked) QuotaOnlineEnabled() (bool, error) { return bool(m), nil }
func (m stageMarked) MailboxLockEnabled() (bool, error) { return bool(m), nil }

// markedManager is kindsSetup with the marks (on) and the auto settings.
func markedManager(t *testing.T, on, auto bool) (*Manager, *memstore.Store) {
	t.Helper()

	m, store := kindsSetup(t)
	m.env.QuotaOnline, m.env.MailboxLock = stageMarked(on), stageMarked(on)
	m.cfg.QuotaOnlineAuto, m.cfg.MailboxDeleteOnlineAuto = auto, auto
	m.cfg.Settle = 0

	return m, store
}

// TestStageOnlineNormalize: the plan decides "online" for quota_recalc
// and mailbox_delete: asked true without the mark is 409, asked false
// stays, omitted follows the mark and the auto setting.
func TestStageOnlineNormalize(t *testing.T) {
	for _, c := range []struct {
		on, auto bool
		params   string
		want     string
		err      error
	}{
		{false, true, `{"online":true}`, "", model.ErrQuotaOnlineMark},
		{true, false, `{"online":true}`, `"online":true`, nil},
		{true, true, `{}`, `"online":true`, nil},
		{true, false, `{}`, `"online":false`, nil},
		{false, true, `{}`, `"online":false`, nil},
		{true, true, `{"online":false}`, `"online":false`, nil},
	} {
		m, store := markedManager(t, c.on, c.auto)

		got, err := planParams(t, m, KindQuotaRecalc, "alice", c.params)
		if !errors.Is(err, c.err) || !strings.Contains(got, c.want) {
			t.Errorf("quota_recalc on=%v auto=%v %s: %q %v", c.on, c.auto, c.params, got, err)
		}

		alice, _ := store.User("alice")
		mb, _ := alice.Lookup("Projekte")

		params := strings.Replace(c.params, "{", `{"mailbox_id":"`+mb.ID+`",`, 1)
		params = strings.Replace(params, ",}", "}", 1)

		got, err = planParams(t, m, KindMailboxDelete, "alice", params)
		if c.err != nil {
			if !errors.Is(err, model.ErrMailboxLockMark) {
				t.Errorf("mailbox_delete on=%v %s: %v", c.on, params, err)
			}

			continue
		}

		if err != nil || !strings.Contains(got, c.want) {
			t.Errorf("mailbox_delete on=%v auto=%v %s: %q %v", c.on, c.auto, params, got, err)
		}
	}
}

// TestStageExclusiveOf: an online quota_recalc and an online
// mailbox_delete report no lock, in the single kind and in the batch
// (R2-10).
func TestStageExclusiveOf(t *testing.T) {
	m, _ := markedManager(t, true, true)

	qr, _ := m.Kind(KindQuotaRecalc)
	md, _ := m.Kind(KindMailboxDelete)
	batch, _ := m.Kind(KindBatch)

	for _, c := range []struct {
		kind   Kind
		params string
		want   bool
	}{
		{qr, `{"online":true}`, false},
		{qr, `{"online":false}`, true},
		{qr, `{}`, true},
		{md, `{"mailbox_id":"x","online":true}`, false},
		{md, `{"mailbox_id":"x"}`, true},
		{batch, `{"kind":"quota_recalc","params":{"online":true}}`, false},
		{batch, `{"kind":"quota_recalc"}`, true},
	} {
		if got := m.ExclusiveOf(c.kind, json.RawMessage(c.params)); got != c.want {
			t.Errorf("%s %s: exclusive %v, want %v", c.kind.Name(), c.params, got, c.want)
		}
	}
}

// TestQuotaRecalcOnlineRun: an online recount runs without the account
// lock, replaces a drifted counter and reports the online mode; an
// offline run does the same under the lock.
func TestQuotaRecalcOnlineRun(t *testing.T) {
	for _, online := range []bool{true, false} {
		m, store := markedManager(t, true, online)
		alice, _ := store.User("alice")

		// Drift: the stored counter is off by a message.
		if err := alice.SetQuotaLimit(model.Quota{}); err != nil {
			t.Fatal(err)
		}

		usage, _ := alice.QuotaUsage()
		if _, _, err := alice.(model.QuotaRecounter).RecountQuota(true); err != nil {
			t.Fatal(err)
		}

		if _, err := alice.Deliver("INBOX", []byte(testMessage), nil, time.Now()); err != nil {
			t.Fatal(err)
		}

		p, j := planAndApply(t, m, KindQuotaRecalc, "alice", nil)

		var rep model.QuotaRecountReport
		if err := json.Unmarshal(j.Result, &rep); err != nil {
			t.Fatal(err)
		}

		wantMode := model.QuotaRecountOffline
		if online {
			wantMode = model.QuotaRecountOnline
		}

		if rep.Mode != wantMode || rep.Result != model.QuotaRecountOK || rep.Counted.Msgs != usage.Msgs+1 {
			t.Fatalf("online=%v: plan %s result %+v", online, p.Params, rep)
		}

		if after, _ := alice.QuotaUsage(); after != rep.Counted {
			t.Errorf("online=%v: counters %+v, counted %+v", online, after, rep.Counted)
		}
	}
}

// TestQuotaRecalcOnlineBusyPostponed: a busy run (the account is under
// an import lock) is given back and tried again; once the lock is gone
// it succeeds.
func TestQuotaRecalcOnlineBusyPostponed(t *testing.T) {
	m, store := markedManager(t, true, true)
	alice, _ := store.User("alice")

	imp := alice.(model.Importer)
	if _, err := imp.ImportLock("import-1", time.Minute); err != nil {
		t.Fatal(err)
	}

	k, _ := m.Kind(KindQuotaRecalc)

	p, err := m.CreatePlan(context.Background(), k, "alice", nil, "tester", nil)
	if err != nil {
		t.Fatal(err)
	}

	j, _, err := m.Submit(k, "alice", nil, p.ID, "tester", "")
	if err != nil {
		t.Fatal(err)
	}

	time.Sleep(200 * time.Millisecond)

	if cur, _ := m.Get(j.ID); cur.Terminal() {
		t.Fatalf("a busy run ended: %+v", cur)
	}

	if err := imp.ImportUnlock("import-1"); err != nil {
		t.Fatal(err)
	}

	done := waitState(t, m, j.ID, StateSucceeded)
	if !strings.Contains(string(done.Result), `"mode":"online"`) {
		t.Fatalf("result %s", done.Result)
	}
}

// TestMailboxDeleteLockedRun: a mailbox_delete with the mailbox lock
// deletes the subtree without the account lock, with the online mode in
// the plan and the result; a session that writes elsewhere meanwhile is
// not refused.
func TestMailboxDeleteLockedRun(t *testing.T) {
	m, store := markedManager(t, true, true)
	alice, _ := store.User("alice")
	parent, _ := alice.Lookup("Projekte")

	p, j := planAndApply(t, m, KindMailboxDelete, "alice", mailboxDeleteParams{MailboxID: parent.ID})

	var s deleteSummary
	if _ = json.Unmarshal(p.Summary, &s); !s.Online || len(s.Mailboxes) != 2 {
		t.Fatalf("plan %s", p.Summary)
	}

	var rep deleteReport
	if _ = json.Unmarshal(j.Result, &rep); rep.Mode != model.QuotaRecountOnline || rep.Result != "ok" || len(rep.Mailboxes) != 2 {
		t.Fatalf("result %s", j.Result)
	}

	for _, name := range []string{"Projekte", "Projekte/2026"} {
		if _, err := alice.Lookup(name); !errors.Is(err, model.ErrNoMailbox) {
			t.Errorf("%s still there", name)
		}
	}

	if locked, _ := alice.(model.Importer).ImportLocked(); locked {
		t.Error("the run took the account lock")
	}

	if _, err := alice.Deliver("INBOX", []byte(testMessage), nil, time.Now()); err != nil {
		t.Errorf("INBOX after the run: %v", err)
	}
}

// TestMailboxDeleteLockedDrift: a delivery into a target between the
// plan and the run is drift (fail safe, the mail stays); a keyword
// change is not (the online fingerprint leaves HIGHESTMODSEQ out).
func TestMailboxDeleteLockedDrift(t *testing.T) {
	m, store := markedManager(t, true, true)
	alice, _ := store.User("alice")
	parent, _ := alice.Lookup("Projekte")
	child, _ := alice.Lookup("Projekte/2026")

	k, _ := m.Kind(KindMailboxDelete)

	// A flag change is no drift.
	p, err := m.CreatePlan(context.Background(), k, "alice", mustJSON(mailboxDeleteParams{MailboxID: parent.ID}), "tester", nil)
	if err != nil {
		t.Fatal(err)
	}

	msgs, _ := alice.ListMessages(child.ID)
	if _, err := alice.SetFlags("Projekte/2026", []uint32{msgs[0].UID}, 0, func([]string) []string { return []string{`\Seen`} }); err != nil {
		t.Fatal(err)
	}

	j, _, _ := m.Submit(k, "alice", nil, p.ID, "tester", "")
	waitState(t, m, j.ID, StateSucceeded)

	// A delivery is.
	if err := alice.EnsureMailbox("Archive", nil); err != nil {
		t.Fatal(err)
	}

	archive, _ := alice.Lookup("Archive")

	p, err = m.CreatePlan(context.Background(), k, "alice", mustJSON(mailboxDeleteParams{MailboxID: archive.ID}), "tester", nil)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := alice.Deliver("Archive", []byte(testMessage), nil, time.Now()); err != nil {
		t.Fatal(err)
	}

	j, _, _ = m.Submit(k, "alice", nil, p.ID, "tester", "")
	if failed := waitState(t, m, j.ID, StateFailed); !strings.Contains(strings.Join(failed.Errors, " "), "changed since the plan") {
		t.Fatalf("drift not detected: %+v", failed)
	}

	if _, err := alice.Lookup("Archive"); err != nil {
		t.Fatal("deleted despite drift")
	}

	if locked := alice.(model.MailboxLockReader).LockedMailbox("Archive"); locked != nil {
		t.Fatalf("still locked after the drift: %v", locked)
	}
}

// TestMailboxDeleteLockedPostponed: a target another mailbox_delete
// locks, or an account under an import lock, postpones the job.
func TestMailboxDeleteLockedPostponed(t *testing.T) {
	m, store := markedManager(t, true, true)
	alice, _ := store.User("alice")
	parent, _ := alice.Lookup("Projekte")

	if err := alice.(model.MailboxLocker).LockMailboxes("job-other", []model.MailboxLockTarget{{ID: parent.ID, Name: parent.Name}}, time.Minute); err != nil {
		t.Fatal(err)
	}

	k, _ := m.Kind(KindMailboxDelete)

	p, err := m.CreatePlan(context.Background(), k, "alice", mustJSON(mailboxDeleteParams{MailboxID: parent.ID}), "tester", nil)
	if err != nil {
		t.Fatal(err)
	}

	j, _, _ := m.Submit(k, "alice", nil, p.ID, "tester", "")

	time.Sleep(200 * time.Millisecond)

	if cur, _ := m.Get(j.ID); cur.Terminal() {
		t.Fatalf("ended despite the foreign lock: %+v", cur)
	}

	if err := alice.(model.MailboxLocker).UnlockMailboxes("job-other"); err != nil {
		t.Fatal(err)
	}

	waitState(t, m, j.ID, StateSucceeded)
}

// TestExpungePostponedOnLockedMailbox: an expunge job on a locked
// mailbox writes nothing and waits for the lock to end.
func TestExpungePostponedOnLockedMailbox(t *testing.T) {
	m, store := markedManager(t, true, true)
	alice, _ := store.User("alice")
	child, _ := alice.Lookup("Projekte/2026")

	k, _ := m.Kind(KindExpunge)

	p, err := m.CreatePlan(context.Background(), k, "alice", mustJSON(expungeParams{MailboxID: child.ID, UIDs: []uint32{1, 2}}), "tester", nil)
	if err != nil {
		t.Fatal(err)
	}

	if err := alice.(model.MailboxLocker).LockMailboxes("job-other", []model.MailboxLockTarget{{ID: child.ID, Name: child.Name}}, time.Minute); err != nil {
		t.Fatal(err)
	}

	j, _, _ := m.Submit(k, "alice", nil, p.ID, "tester", "")

	time.Sleep(200 * time.Millisecond)

	if cur, _ := m.Get(j.ID); cur.Terminal() {
		t.Fatalf("ended despite the lock: %+v", cur)
	}

	if msgs, _ := alice.ListMessages(child.ID); len(msgs) != 2 {
		t.Fatalf("expunged under the lock: %d left", len(msgs))
	}

	if err := alice.(model.MailboxLocker).UnlockMailboxes("job-other"); err != nil {
		t.Fatal(err)
	}

	waitState(t, m, j.ID, StateSucceeded)

	if msgs, _ := alice.ListMessages(child.ID); len(msgs) != 0 {
		t.Fatalf("%d left after the lock", len(msgs))
	}
}
