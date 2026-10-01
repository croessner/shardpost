package jobs

import (
	"encoding/json"
	"slices"
	"testing"
	"time"

	"shardpost/internal/memstore"
	"shardpost/internal/model"
)

// legacyNames seeds alice with mailboxes a client without UTF-8 support
// created before names were converted: "Entwürfe" under its modified
// UTF-7 name with an encoded child holding a message; "R&D", which is
// no modified UTF-7; "&AMQ-rger", which collides with an existing
// "Ärger", with a child; and a subscription without a mailbox.
func legacyNames(t *testing.T, store *memstore.Store) model.User {
	t.Helper()

	alice, _ := store.User("alice")

	for _, name := range []string{"INBOX", "Entw&APw-rfe", "Entw&APw-rfe/&ANY-l", "R&D", "&AMQ-rger", "Ärger", "&AMQ-rger/x"} {
		if err := alice.EnsureMailbox(name, nil); err != nil {
			t.Fatal(err)
		}
	}

	if _, err := alice.Deliver("Entw&APw-rfe/&ANY-l", []byte(testMessage), nil, time.Now()); err != nil {
		t.Fatal(err)
	}

	_ = alice.SetSubscribed("Gel&APY-scht", true)

	return alice
}

// planSummary decodes the summary of a mailbox_name_convert plan.
func planSummary(t *testing.T, p *Plan) nameSummary {
	t.Helper()

	var s nameSummary
	if err := json.Unmarshal(p.Summary, &s); err != nil {
		t.Fatal(err)
	}

	return s
}

// mailboxNames lists the names of the account's mailboxes.
func mailboxNames(u model.User) []string {
	boxes, _ := u.ListMailboxes()

	names := make([]string, 0, len(boxes))
	for _, mb := range boxes {
		names = append(names, mb.Name)
	}

	return names
}

// TestMailboxNameConvert: names a legacy client stored in modified UTF-7
// are renamed to UTF-8, children with their parent; a name that is in
// the way is a conflict; subscriptions without a mailbox move too.
func TestMailboxNameConvert(t *testing.T) {
	store := memstore.New()
	alice := legacyNames(t, store)

	m := newTestManager(memstore.NewAdminKV(), store, "pod-a", Kinds(time.Hour)...)
	runManager(t, m)

	p, j := planAndApply(t, m, KindMailboxName, "alice", nil)
	if s := planSummary(t, p); s.Renames != 2 || s.Conflicts != 2 || s.Subscriptions != 1 || s.Invalid != 0 {
		t.Fatalf("plan: %s", p.Summary)
	}

	checkConverted(t, alice, string(j.Result))

	// A second plan finds nothing left but the conflicts.
	p, _ = planAndApply(t, m, KindMailboxName, "alice", nil)
	if s := planSummary(t, p); s.Renames != 0 || s.Subscriptions != 0 || s.Conflicts != 2 {
		t.Fatalf("second plan: %s", p.Summary)
	}
}

// checkConverted checks the mailboxes, the message and the subscription
// after the conversion of legacyNames.
func checkConverted(t *testing.T, alice model.User, result string) {
	t.Helper()

	names := mailboxNames(alice)
	for _, want := range []string{"Entwürfe", "Entwürfe/Öl", "R&D", "&AMQ-rger", "Ärger", "&AMQ-rger/x"} {
		if !slices.Contains(names, want) {
			t.Errorf("missing %q after %s: %v", want, result, names)
		}
	}

	if slices.Contains(names, "Entw&APw-rfe") {
		t.Errorf("not renamed: %v", names)
	}

	mb, err := alice.Lookup("Entwürfe/Öl")
	if err != nil {
		t.Fatal(err)
	}

	if msgs, _ := alice.ListMessages(mb.ID); len(msgs) != 1 {
		t.Errorf("message lost: %v", msgs)
	}

	if missing, _ := alice.ListSubscribedMissing(); !slices.Equal(missing, []string{"Gelöscht"}) {
		t.Errorf("subscriptions: %v", missing)
	}
}

func TestUTF8Name(t *testing.T) {
	for in, want := range map[string]string{
		"Entw&APw-rfe":       "Entwürfe",
		"a/Entw&APw-rfe/b":   "a/Entwürfe/b",
		"R&D":                "",
		"A&-B":               "A&B",
		"Entwürfe":           "",
		"&AGE-":              "", // not canonical
		"Entwürfe/&AMQ-rger": "Entwürfe/Ärger",
	} {
		got, ok := utf8Name(in)
		if (want == "") == ok || (ok && got != want) {
			t.Errorf("%q: %q %v, want %q", in, got, ok, want)
		}
	}
}
