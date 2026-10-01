package delivery

import (
	"context"
	"testing"

	"shardpost/internal/model"
)

// TestFileIntoAtMailboxLimit: a fileinto that would create a mailbox
// beyond the account's mailbox limit goes to INBOX, like any other failed
// fileinto, and the message is stored once.
func TestFileIntoAtMailboxLimit(t *testing.T) {
	svc, store := testService(t, `require ["fileinto", "mailbox"]; fileinto :create "Lists/new";`)
	store.SetLimits(model.Limits{Mailboxes: 1})

	res, err := svc.Deliver(context.Background(), request(message))
	if err != nil || len(res.Stored) != 1 || res.Stored[0].Mailbox != "INBOX" {
		t.Fatalf("%+v %v", res, err)
	}

	if count(t, store, "INBOX") != 1 || count(t, store, "Lists/new") != 0 {
		t.Fatal("not in INBOX alone")
	}

	// With the same script, a keep next to the refused fileinto is one
	// copy, not two.
	svc, store = testService(t, `require ["fileinto", "mailbox"]; fileinto :create "Lists/new"; keep;`)
	store.SetLimits(model.Limits{Mailboxes: 1})

	if res, err := svc.Deliver(context.Background(), request(message)); err != nil || len(res.Stored) != 1 || count(t, store, "INBOX") != 1 {
		t.Fatalf("%+v %v", res, err)
	}
}

// TestAddFlagAtKeywordLimit: keywords a Sieve script adds beyond the
// keyword limit of the mailbox are dropped, the message is stored.
func TestAddFlagAtKeywordLimit(t *testing.T) {
	svc, store := testService(t, `require ["imap4flags"]; addflag ["$a", "$b", "\\Flagged"]; keep;`)
	store.SetLimits(model.Limits{KeywordsPerMailbox: 1})

	res, err := svc.Deliver(context.Background(), request(message))
	if err != nil || len(res.Stored) != 1 {
		t.Fatalf("%+v %v", res, err)
	}

	u, _ := store.User(account)
	mb, _ := u.Lookup("INBOX")

	metas, err := u.ListMessages(mb.ID)
	if err != nil || len(metas) != 1 {
		t.Fatalf("%v %v", metas, err)
	}

	if model.HasFlag(metas[0].Flags, "$a") || !model.HasFlag(metas[0].Flags, `\Flagged`) {
		t.Fatalf("flags %v", metas[0].Flags)
	}
}
