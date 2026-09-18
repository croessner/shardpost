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
