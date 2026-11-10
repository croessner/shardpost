package delivery

import (
	"context"
	"errors"
	"testing"

	"shardpost/internal/model"
)

// TestFileIntoRefusesABadNewName: a fileinto that would create a mailbox
// under a name a new mailbox must not get goes to INBOX; a mailbox of
// that name that exists from before still takes the message.
func TestFileIntoRefusesABadNewName(t *testing.T) {
	svc, store := testService(t, `require ["fileinto", "mailbox"]; fileinto :create "Lists/a*b";`)

	res, err := svc.Deliver(context.Background(), request(message))
	if err != nil || len(res.Stored) != 1 || res.Stored[0].Mailbox != "INBOX" {
		t.Fatalf("%+v %v", res, err)
	}

	u, _ := store.User(account)
	if _, err := u.Lookup("Lists/a*b"); !errors.Is(err, model.ErrNoMailbox) {
		t.Fatalf("the mailbox was created: %v", err)
	}

	if err := u.EnsureMailbox("Lists/a*b", nil); err != nil {
		t.Fatal(err)
	}

	if res, err := svc.Deliver(context.Background(), request(message)); err != nil || len(res.Stored) != 1 || res.Stored[0].Mailbox != "Lists/a*b" {
		t.Fatalf("existing mailbox: %+v %v", res, err)
	}
}

// TestFileIntoBelowAnOldName: below an existing mailbox whose name the
// rules would refuse, fileinto still creates a new mailbox with a valid
// leaf.
func TestFileIntoBelowAnOldName(t *testing.T) {
	svc, store := testService(t, `require ["fileinto", "mailbox"]; fileinto :create "50% off/2026-11";`)

	u, _ := store.User(account)
	if err := u.EnsureMailbox("50% off", nil); err != nil {
		t.Fatal(err)
	}

	if res, err := svc.Deliver(context.Background(), request(message)); err != nil || len(res.Stored) != 1 || res.Stored[0].Mailbox != "50% off/2026-11" {
		t.Fatalf("%+v %v", res, err)
	}
}

// TestFccRefusesABadNewName: :fcc with :create does not create a mailbox
// under a name a new mailbox must not get; the reply still goes out.
func TestFccRefusesABadNewName(t *testing.T) {
	svc, store, rec := vacationService(t, `require ["vacation", "fcc", "mailbox"];
vacation :fcc "Sent/a*b" :create "I am away.";`, nil)
	deliverOne(t, svc, store, message)

	u, _ := store.User(account)
	if _, err := u.Lookup("Sent/a*b"); !errors.Is(err, model.ErrNoMailbox) || len(rec.Messages()) != 1 {
		t.Fatalf("mailbox %v, sent %d", err, len(rec.Messages()))
	}
}
