package delivery

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"

	"shardpost/internal/memstore"
	"shardpost/internal/model"
)

const account = "user@example.org"

func testService(t *testing.T, script string) (*Service, *memstore.Store) {
	t.Helper()

	store := memstore.New()
	if script != "" {
		u, _ := store.User(account)
		if err := u.PutScript("main", script); err != nil {
			t.Fatal(err)
		}

		if err := u.SetActiveScript("main"); err != nil {
			t.Fatal(err)
		}
	}

	return &Service{Store: store, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}, store
}

func request(raw string) Request {
	return Request{Account: account, Rcpt: account, From: "sender@example.org", Raw: []byte(raw)}
}

const message = "From: sender@example.org\r\nTo: user@example.org\r\nSubject: hi\r\n\r\nbody\r\n"

// count returns the number of messages in the mailbox of the account.
func count(t *testing.T, store *memstore.Store, mailbox string) int {
	t.Helper()

	u, _ := store.User(account)

	mb, err := u.Lookup(mailbox)
	if errors.Is(err, model.ErrNoMailbox) {
		return 0
	}

	if err != nil {
		t.Fatal(err)
	}

	metas, err := u.ListMessages(mb.ID)
	if err != nil {
		t.Fatal(err)
	}

	return len(metas)
}

func TestDeliverWithoutScript(t *testing.T) {
	svc, store := testService(t, "")

	res, err := svc.Deliver(context.Background(), request(message))
	if err != nil || len(res.Stored) != 1 || res.Stored[0].Mailbox != "INBOX" || res.Stored[0].UID == 0 {
		t.Fatalf("%+v %v", res, err)
	}

	if count(t, store, "INBOX") != 1 {
		t.Fatal("not in INBOX")
	}
}

func TestDeliverSieveTargets(t *testing.T) {
	svc, store := testService(t, `require ["fileinto", "mailbox"]; fileinto :create "Archive"; keep;`)

	res, err := svc.Deliver(context.Background(), request(message))
	if err != nil || len(res.Stored) != 2 {
		t.Fatalf("%+v %v", res, err)
	}

	if count(t, store, "INBOX") != 1 || count(t, store, "Archive") != 1 {
		t.Fatal("targets not stored")
	}
}

func TestDeliverRejectAndDiscard(t *testing.T) {
	svc, store := testService(t, `require "reject"; reject "go away";`)

	var rej *RejectedError
	if _, err := svc.Deliver(context.Background(), request(message)); !errors.As(err, &rej) || rej.Reason != "go away" || count(t, store, "INBOX") != 0 {
		t.Fatalf("reject: %v", err)
	}

	svc, store = testService(t, `discard;`)

	res, err := svc.Deliver(context.Background(), request(message))
	if err != nil || len(res.Stored) != 0 || count(t, store, "INBOX") != 0 {
		t.Fatalf("discard: %+v %v", res, err)
	}
}

func TestDeliverNoSieveAndDirectMailbox(t *testing.T) {
	svc, store := testService(t, `require "reject"; reject "go away";`)

	req := request(message)
	req.NoSieve = true

	if _, err := svc.Deliver(context.Background(), req); err != nil || count(t, store, "INBOX") != 1 {
		t.Fatalf("no sieve: %v", err)
	}

	req = request(message)
	req.Mailbox = "Reports"

	if _, err := svc.Deliver(context.Background(), req); !errors.Is(err, ErrNoMailbox) {
		t.Fatalf("missing mailbox: %v", err)
	}

	u, _ := store.User(account)
	if err := u.CreateMailbox("Reports", nil); err != nil {
		t.Fatal(err)
	}

	if _, err := svc.Deliver(context.Background(), req); err != nil || count(t, store, "Reports") != 1 {
		t.Fatalf("direct mailbox: %v", err)
	}
}

func TestDeliverQuotaEnforcement(t *testing.T) {
	svc, store := testService(t, "")

	u, _ := store.User(account)
	if err := u.SetQuotaLimit(model.Quota{Bytes: 10}); err != nil {
		t.Fatal(err)
	}

	if _, err := svc.Deliver(context.Background(), request(message)); !errors.Is(err, model.ErrOverQuota) {
		t.Fatalf("enforced: %v", err)
	}

	req := request(message)
	req.NoQuotaEnforcement = true

	if _, err := svc.Deliver(context.Background(), req); err != nil {
		t.Fatalf("exempt: %v", err)
	}

	if usage, _ := u.QuotaUsage(); usage.Bytes != int64(len(message)) {
		t.Fatalf("exempt delivery not counted: %+v", usage)
	}
	// The exemption belonged to that delivery's view only.
	if _, err := svc.Deliver(context.Background(), request(message)); !errors.Is(err, model.ErrOverQuota) {
		t.Fatalf("exemption leaked: %v", err)
	}
}

func TestDeliverRollsBackMultiTarget(t *testing.T) {
	svc, store := testService(t, `require ["fileinto", "mailbox"]; fileinto :create "A"; fileinto :create "B";`)

	u, _ := store.User(account)
	if err := u.SetQuotaLimit(model.Quota{Bytes: int64(len(message)) + 1}); err != nil {
		t.Fatal(err)
	}

	if _, err := svc.Deliver(context.Background(), request(message)); !errors.Is(err, model.ErrOverQuota) {
		t.Fatalf("second target should not fit: %v", err)
	}

	if count(t, store, "A")+count(t, store, "B") != 0 {
		t.Fatal("first target not rolled back")
	}
}

func TestRedirectWithoutRelayKeeps(t *testing.T) {
	svc, store := testService(t, `redirect "other@example.org";`)

	if _, err := svc.Deliver(context.Background(), request(message)); err != nil || count(t, store, "INBOX") != 1 {
		t.Fatalf("redirect without relay must keep: %v", err)
	}
}

func TestReadMessage(t *testing.T) {
	data, release, err := ReadMessage(strings.NewReader("hello"), 5)
	if err != nil || string(data) != "hello" {
		t.Fatalf("%q %v", data, err)
	}

	release()

	_, release, err = ReadMessage(strings.NewReader("hello!"), 5)
	release()

	if !errors.Is(err, model.ErrTooLarge) {
		t.Fatalf("too large: %v", err)
	}

	big := strings.Repeat("x", 3*readStep+7)

	data, release, err = ReadMessage(strings.NewReader(big), int64(len(big)))
	if err != nil || len(data) != len(big) {
		t.Fatalf("multi-step: %d %v", len(data), err)
	}

	release()
}

// TestFileIntoPublicFolder: a Sieve script files into a public folder
// when the account may post there; without the right the message lands
// in INBOX instead, and nothing is created in the public namespace.
func TestFileIntoPublicFolder(t *testing.T) {
	script := `require "fileinto"; fileinto "Public/Support";`

	for _, tc := range []struct {
		name    string
		rights  string
		mailbox string
		inPub   int
	}{
		{"with the post right", "lrp", "Public/Support", 1},
		{"without it", "lr", "INBOX", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc, store := testService(t, script)
			svc.PublicPrefix = "Public/"

			pub, err := store.User(model.PublicNamespace)
			if err != nil {
				t.Fatal(err)
			}

			if err := pub.EnsureMailbox("Support", nil); err != nil {
				t.Fatal(err)
			}

			if err := pub.SetACL("Support", account, tc.rights); err != nil {
				t.Fatal(err)
			}

			res, err := svc.Deliver(context.Background(), request(message))
			if err != nil {
				t.Fatal(err)
			}

			if len(res.Stored) != 1 || res.Stored[0].Mailbox != tc.mailbox {
				t.Fatalf("stored %+v, want mailbox %s", res.Stored, tc.mailbox)
			}

			msgs, err := messagesIn(pub, "Support")
			if err != nil {
				t.Fatal(err)
			}

			if len(msgs) != tc.inPub {
				t.Errorf("%d messages in the public folder, want %d", len(msgs), tc.inPub)
			}

			u, _ := store.User(account)

			own, err := messagesIn(u, "INBOX")
			if err != nil {
				t.Fatal(err)
			}

			if want := 1 - tc.inPub; len(own) != want {
				t.Errorf("%d messages in INBOX, want %d", len(own), want)
			}
		})
	}
}

// messagesIn lists the messages of a mailbox by name.
func messagesIn(u model.User, name string) ([]model.Meta, error) {
	mb, err := u.Lookup(name)
	if err != nil {
		return nil, err
	}

	return u.ListMessages(mb.ID)
}

// TestFileIntoPublicFolderAndKeep: a script that files a copy into a
// public folder and keeps the message delivers into two namespaces. The
// delivery intent that a multi-target delivery opens must exist in both,
// because an intent — like the rollback it exists for — only finds a
// message in the namespace that holds it.
func TestFileIntoPublicFolderAndKeep(t *testing.T) {
	script := `require ["fileinto", "copy"]; fileinto :copy "Public/Support";`

	svc, store := testService(t, script)
	svc.PublicPrefix = "Public/"

	pub, err := store.User(model.PublicNamespace)
	if err != nil {
		t.Fatal(err)
	}

	if err := pub.EnsureMailbox("Support", nil); err != nil {
		t.Fatal(err)
	}

	if err := pub.SetACL("Support", account, "lrp"); err != nil {
		t.Fatal(err)
	}

	res, err := svc.Deliver(context.Background(), request(message))
	if err != nil {
		t.Fatalf("delivery into two namespaces: %v", err)
	}

	if len(res.Stored) != 2 {
		t.Fatalf("stored %+v, want the public copy and the kept message", res.Stored)
	}

	msgs, err := messagesIn(pub, "Support")
	if err != nil {
		t.Fatal(err)
	}

	if len(msgs) != 1 {
		t.Errorf("%d messages in the public folder, want 1", len(msgs))
	}

	u, _ := store.User(account)

	own, err := messagesIn(u, "INBOX")
	if err != nil {
		t.Fatal(err)
	}

	if len(own) != 1 {
		t.Errorf("%d messages in INBOX, want 1", len(own))
	}
}

// TestFileIntoPublicFolderRollback: when a later target of the same
// delivery fails, the copy already lying in the public folder has to go
// again — a retry must not leave two messages behind. The rollback
// therefore has to remove it where it lies, in the public namespace.
func TestFileIntoPublicFolderRollback(t *testing.T) {
	script := `require ["fileinto", "copy"]; fileinto :copy "Public/Support";`

	svc, store := testService(t, script)
	svc.PublicPrefix = "Public/"

	pub, err := store.User(model.PublicNamespace)
	if err != nil {
		t.Fatal(err)
	}

	if err := pub.EnsureMailbox("Support", nil); err != nil {
		t.Fatal(err)
	}

	if err := pub.SetACL("Support", account, "lrp"); err != nil {
		t.Fatal(err)
	}

	// The account is full, so the kept message after the public copy
	// fails.
	u, _ := store.User(account)
	if err := u.SetQuotaLimit(model.Quota{Bytes: 1}); err != nil {
		t.Fatal(err)
	}

	if _, err := svc.Deliver(context.Background(), request(message)); err == nil {
		t.Fatal("delivery into a full account succeeded")
	}

	msgs, err := messagesIn(pub, "Support")
	if err != nil {
		t.Fatal(err)
	}

	if len(msgs) != 0 {
		t.Errorf("%d messages left in the public folder after a failed delivery, want 0", len(msgs))
	}
}

// TestFileIntoSharedFolderRefused: a script that files into what a
// client calls somebody else's mailbox must not create a mailbox of the
// recipient under that name. Nobody could open it — the name resolves
// to the other account — so the message goes to INBOX, as a failed
// fileinto does.
func TestFileIntoSharedFolderRefused(t *testing.T) {
	script := `require "fileinto"; fileinto "Shared/other@example.org/Team";`

	svc, store := testService(t, script)
	svc.SharedPrefix = "Shared/"

	res, err := svc.Deliver(context.Background(), request(message))
	if err != nil {
		t.Fatal(err)
	}

	if len(res.Stored) != 1 || res.Stored[0].Mailbox != "INBOX" {
		t.Fatalf("stored %+v, want INBOX", res.Stored)
	}

	u, _ := store.User(account)

	boxes, err := u.ListMailboxes()
	if err != nil {
		t.Fatal(err)
	}

	for _, b := range boxes {
		if strings.HasPrefix(b.Name, "Shared/") {
			t.Errorf("a mailbox below the shared prefix was created: %s", b.Name)
		}
	}
}

// TestRefusedFileIntoDoesNotDuplicate: a refused fileinto falls back to
// INBOX, and a keep (or a second refused target) names the same mailbox
// — the message must be stored once, not once per name.
func TestRefusedFileIntoDoesNotDuplicate(t *testing.T) {
	for _, tc := range []struct {
		name   string
		script string
	}{
		{"a keep beside it", `require ["fileinto", "copy"]; fileinto :copy "Shared/a@example.org/X";`},
		{"two refused targets", `require "fileinto"; fileinto "Shared/a@example.org/X"; fileinto "Shared/b@example.org/Y";`},
		{"INBOX named as well", `require "fileinto"; fileinto "INBOX"; fileinto "Shared/a@example.org/X";`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc, store := testService(t, tc.script)
			svc.SharedPrefix = "Shared/"

			res, err := svc.Deliver(context.Background(), request(message))
			if err != nil {
				t.Fatal(err)
			}

			if len(res.Stored) != 1 {
				t.Errorf("stored %+v, want one message", res.Stored)
			}

			u, _ := store.User(account)

			msgs, err := messagesIn(u, "INBOX")
			if err != nil {
				t.Fatal(err)
			}

			if len(msgs) != 1 {
				t.Errorf("%d messages in INBOX, want 1", len(msgs))
			}
		})
	}
}
