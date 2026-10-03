package delivery

import (
	"context"
	"errors"
	"testing"
	"time"

	"shardpost/internal/memstore"
	"shardpost/internal/model"
)

// lockArchive creates Archive for the account and locks it as a
// mailbox_delete job would (stage B17).
func lockArchive(t *testing.T, store *memstore.Store) {
	t.Helper()

	u, _ := store.User(account)
	if err := u.EnsureMailbox("Archive", nil); err != nil {
		t.Fatal(err)
	}

	mb, _ := u.Lookup("Archive")
	if err := u.(model.MailboxLocker).LockMailboxes("job-1", []model.MailboxLockTarget{{ID: mb.ID, Name: mb.Name}}, time.Minute); err != nil {
		t.Fatal(err)
	}
}

// TestFileintoLockedGoesToInbox: a fileinto into a mailbox that is being
// deleted lands in INBOX, with and without a keep, in either order, as
// exactly one copy.
func TestFileintoLockedGoesToInbox(t *testing.T) {
	for _, script := range []string{
		`require "fileinto"; fileinto "Archive";`,
		`require "fileinto"; fileinto "Archive"; keep;`,
		`require "fileinto"; keep; fileinto "Archive";`,
		`require ["fileinto", "mailbox"]; fileinto :create "Archive/sub";`,
	} {
		svc, store := testService(t, script)
		lockArchive(t, store)

		res, err := svc.Deliver(context.Background(), request(message))
		if err != nil {
			t.Fatalf("%s: %v", script, err)
		}

		if len(res.Stored) != 1 || res.Stored[0].Mailbox != "INBOX" {
			t.Fatalf("%s: stored %+v", script, res.Stored)
		}

		if count(t, store, "INBOX") != 1 || count(t, store, "Archive") != 0 || count(t, store, "Archive/sub") != 0 {
			t.Fatalf("%s: INBOX %d Archive %d", script, count(t, store, "INBOX"), count(t, store, "Archive"))
		}
	}
}

// TestFileintoLockedWithOtherTargets: a locked target among several is
// redirected with an intent of its own; the other targets are stored
// as planned.
func TestFileintoLockedWithOtherTargets(t *testing.T) {
	svc, store := testService(t, `require ["fileinto", "mailbox"]; fileinto "Archive"; fileinto :create "Lists";`)
	lockArchive(t, store)

	res, err := svc.Deliver(context.Background(), request(message))
	if err != nil {
		t.Fatal(err)
	}

	if len(res.Stored) != 2 || count(t, store, "INBOX") != 1 || count(t, store, "Lists") != 1 || count(t, store, "Archive") != 0 {
		t.Fatalf("stored %+v: INBOX %d Lists %d Archive %d", res.Stored, count(t, store, "INBOX"), count(t, store, "Lists"), count(t, store, "Archive"))
	}
}

// TestExplicitLockedMailbox: a caller that named the mailbox itself gets
// ErrNoMailbox: the mailbox is gone in seconds.
func TestExplicitLockedMailbox(t *testing.T) {
	svc, store := testService(t, "")
	lockArchive(t, store)

	req := request(message)
	req.Mailbox = "Archive"

	if _, err := svc.Deliver(context.Background(), req); !errors.Is(err, ErrNoMailbox) {
		t.Fatalf("explicit locked target: %v", err)
	}
}
