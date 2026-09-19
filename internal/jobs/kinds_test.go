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

const testMessage = "Subject: hello\r\n\r\nsearchable body\r\n"

// kindsSetup is a manager with every kind over a memstore where alice has
// INBOX (one message), Projekte/2026 (two) and Shared (bob lr); bob shares
// Bobs with alice.
func kindsSetup(t *testing.T) (*Manager, *memstore.Store) {
	t.Helper()

	store := memstore.New()
	alice, _ := store.User("alice")
	bob, _ := store.User("bob")

	for _, name := range []string{"INBOX", "Projekte", "Projekte/2026", "Shared"} {
		if err := alice.EnsureMailbox(name, nil); err != nil {
			t.Fatal(err)
		}
	}

	for _, mb := range []string{"INBOX", "Projekte/2026", "Projekte/2026"} {
		if _, err := alice.Deliver(mb, []byte(testMessage), nil, time.Now()); err != nil {
			t.Fatal(err)
		}
	}

	_ = alice.SetACL("Shared", "bob", "lr")
	_ = bob.EnsureMailbox("Bobs", nil)
	_ = bob.SetACL("Bobs", "alice", "lr")

	m := newTestManager(memstore.NewAdminKV(), store, "pod-a", Kinds(time.Hour)...)
	runManager(t, m)

	return m, store
}

// planAndApply plans a kind and waits for its apply.
func planAndApply(t *testing.T, m *Manager, kind, account string, params any, states ...string) (*Plan, *Job) {
	t.Helper()

	k, _ := m.Kind(kind)

	var raw json.RawMessage
	if params != nil {
		raw, _ = json.Marshal(params)
	}

	p, err := m.CreatePlan(context.Background(), k, account, raw, "tester", nil)
	if err != nil {
		t.Fatalf("plan %s: %v", kind, err)
	}

	j, _, err := m.Submit(k, account, nil, p.ID, "tester", "")
	if err != nil {
		t.Fatalf("apply %s: %v", kind, err)
	}

	if len(states) == 0 {
		states = []string{StateSucceeded}
	}

	return p, waitState(t, m, j.ID, states...)
}

func pointers(t *testing.T, store *memstore.Store, account string) []string {
	t.Helper()

	u, _ := store.User(account)
	refs, _ := u.ListShared()

	var out []string
	for _, r := range refs {
		out = append(out, r.Owner+"/"+r.Mailbox)
	}

	return out
}

func TestACLReconcile(t *testing.T) {
	m, store := kindsSetup(t)

	// bob lost his pointer to alice/Shared.
	bob, _ := store.User("bob")
	_ = bob.RemoveSharedRef(model.SharedRef{Owner: "alice", Mailbox: "Shared"})

	p, j := planAndApply(t, m, KindACLReconcile, "alice", nil)

	var s aclSummary

	_ = json.Unmarshal(p.Summary, &s)

	if !strings.Contains(string(p.Summary), `"add_pointer"`) || len(s.Accounts) != 1 || s.Accounts[0] != "bob" {
		t.Fatalf("plan: %s", p.Summary)
	}

	if !strings.Contains(strings.Join(pointers(t, store, "bob"), ","), "alice/Shared") || j.State != StateSucceeded {
		t.Fatalf("pointer not repaired: %v %+v", pointers(t, store, "bob"), j)
	}
}

// stalePointerUser adds a pointer to a mailbox of bob that grants
// nothing, which acl_reconcile must remove.
type stalePointerUser struct {
	model.User
	removed *[]model.SharedRef
}

func (u stalePointerUser) ListShared() ([]model.SharedRef, error) {
	refs, err := u.User.ListShared()
	if len(*u.removed) == 0 {
		refs = append(refs, model.SharedRef{Owner: "bob", Mailbox: "Bobs-old"})
	}

	return refs, err
}

func (u stalePointerUser) RemoveSharedRef(ref model.SharedRef) error {
	*u.removed = append(*u.removed, ref)
	return u.User.RemoveSharedRef(ref)
}

func TestACLReconcileRemovesStalePointer(t *testing.T) {
	store := memstore.New()
	alice, _ := store.User("alice")
	_ = alice.EnsureMailbox("INBOX", nil)

	var removed []model.SharedRef

	env := &Env{Accounts: store, Log: newTestManager(nil, store, "x").env.Log}

	s, err := reconcileFixes(env, stalePointerUser{alice, &removed}, "alice")
	if err != nil || len(s.Fixes) != 1 || s.Fixes[0].Action != fixRemovePointer {
		t.Fatalf("fixes: %v %+v", err, s)
	}

	run := &Run{User: stalePointerUser{alice, &removed}, Env: env}
	if err := applyFix(run, s.Fixes[0]); err != nil || len(removed) != 1 || removed[0].Mailbox != "Bobs-old" {
		t.Fatalf("remove: %v %v", err, removed)
	}
}

func TestMailboxDeleteWithChildrenAndDrift(t *testing.T) {
	m, store := kindsSetup(t)
	alice, _ := store.User("alice")

	parent, _ := alice.Lookup("Projekte")

	// Drift: a delivery between plan and apply.
	k, _ := m.Kind(KindMailboxDelete)
	raw, _ := json.Marshal(mailboxDeleteParams{MailboxID: parent.ID})

	p, err := m.CreatePlan(context.Background(), k, "alice", raw, "tester", nil)
	if err != nil {
		t.Fatal(err)
	}

	_, _ = alice.Deliver("Projekte/2026", []byte(testMessage), nil, time.Now())

	j, _, _ := m.Submit(k, "alice", nil, p.ID, "tester", "")
	if failed := waitState(t, m, j.ID, StateFailed); !strings.Contains(strings.Join(failed.Errors, " "), "changed since the plan") {
		t.Fatalf("drift not detected: %+v", failed)
	}

	if _, err := alice.Lookup("Projekte/2026"); err != nil {
		t.Fatal("deleted despite drift")
	}

	// A fresh plan applies: children first, then the mailbox.
	plan, done := planAndApply(t, m, KindMailboxDelete, "alice", mailboxDeleteParams{MailboxID: parent.ID})

	var s deleteSummary

	_ = json.Unmarshal(plan.Summary, &s)

	if len(s.Mailboxes) != 2 || s.Mailboxes[0].Name != "Projekte/2026" || s.Messages != 3 {
		t.Fatalf("plan: %s", plan.Summary)
	}

	for _, name := range []string{"Projekte", "Projekte/2026"} {
		if _, err := alice.Lookup(name); !errors.Is(err, model.ErrNoMailbox) {
			t.Errorf("%s still there (%+v)", name, done)
		}
	}

	inbox, _ := alice.Lookup("INBOX")
	if _, err := m.CreatePlan(context.Background(), k, "alice", mustJSON(mailboxDeleteParams{MailboxID: inbox.ID}), "tester", nil); err == nil {
		t.Fatal("INBOX delete planned")
	}
}

func TestPurge(t *testing.T) {
	m, store := kindsSetup(t)

	var touched []string

	k, _ := m.Kind(KindPurge)

	p, err := m.CreatePlan(context.Background(), k, "alice", nil, "tester", func(accounts []string) error {
		touched = accounts
		return nil
	})
	if err != nil || len(touched) != 1 || touched[0] != "bob" {
		t.Fatalf("plan: %v %v", err, touched)
	}

	j, _, _ := m.Submit(k, "alice", nil, p.ID, "tester", "")
	waitState(t, m, j.ID, StateSucceeded)

	alice, _ := store.User("alice")
	if boxes, _ := alice.ListMailboxes(); len(boxes) != 0 {
		t.Fatalf("mailboxes left: %v", boxes)
	}

	bob, _ := store.User("bob")
	if acl, _ := bob.GetACL("Bobs"); acl["alice"] != "" {
		t.Fatalf("bob still grants alice: %v", acl)
	}

	if ptr := pointers(t, store, "bob"); len(ptr) != 0 {
		t.Fatalf("bob still points to alice: %v", ptr)
	}

	// The scope hook can refuse a plan.
	_, err = m.CreatePlan(context.Background(), k, "bob", nil, "tester", func([]string) error { return errors.New("out of scope") })
	if err == nil || !strings.Contains(err.Error(), "out of scope") {
		t.Fatalf("scope hook: %v", err)
	}
}

func TestQuotaRecalcFTSScrubAndCheck(t *testing.T) {
	m, _ := kindsSetup(t)

	p, j := planAndApply(t, m, KindQuotaRecalc, "alice", nil)
	if !strings.Contains(string(p.Summary), `"differs":false`) || !strings.Contains(string(j.Result), `"Msgs":3`) {
		t.Fatalf("quota: %s %s", p.Summary, j.Result)
	}

	if _, j := planAndApply(t, m, KindFTSRebuild, "alice", nil); j.Progress.Total != 3 || string(j.Result) != `{"indexed":3}` {
		t.Fatalf("fts: %+v", j)
	}

	k, _ := m.Kind(KindScrub)
	if _, err := m.CreatePlan(context.Background(), k, "alice", mustJSON(scrubParams{Classes: []string{"bogus"}}), "tester", nil); err == nil {
		t.Fatal("unknown class accepted")
	}

	if _, j := planAndApply(t, m, KindScrub, "alice", scrubParams{Classes: []string{model.RepairRefs}}); !strings.Contains(string(j.Result), `"refs"`) {
		t.Fatalf("scrub: %s", j.Result)
	}

	check, _ := m.Kind(KindIndexCheck)
	if _, _, err := m.Submit(check, "alice", nil, "plan-x", "tester", ""); !errors.Is(err, ErrNoPlan) {
		t.Fatalf("check with plan: %v", err)
	}

	j, _, err := m.Submit(check, "alice", nil, "", "tester", "")
	if err != nil {
		t.Fatal(err)
	}

	if done := waitState(t, m, j.ID, StateSucceeded); !strings.Contains(string(done.Result), `"DryRun":true`) {
		t.Fatalf("check: %s", done.Result)
	}

	if _, _, err := m.Submit(k, "alice", nil, "", "tester", ""); !errors.Is(err, ErrNeedsPlan) {
		t.Fatalf("scrub without plan: %v", err)
	}
}

func mustJSON(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}
