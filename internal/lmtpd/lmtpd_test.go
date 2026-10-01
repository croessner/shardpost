package lmtpd

import (
	"errors"
	"io"
	"log/slog"
	"net"
	"strings"
	"testing"

	"github.com/emersion/go-smtp"

	"shardpost/internal/auth"
	"shardpost/internal/delivery"
	"shardpost/internal/memstore"
	"shardpost/internal/model"
	"shardpost/internal/smtptest"
)

// serve runs an LMTP server with a memstore and the users "full" (quota
// exhausted), "reject" (Sieve reject) and "ok".
func serve(t *testing.T) (string, *memstore.Store) {
	t.Helper()

	store := memstore.New()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	full, _ := store.User("full")
	if err := full.SetQuotaLimit(model.Quota{Bytes: 5}); err != nil {
		t.Fatal(err)
	}

	rej, _ := store.User("reject")
	if err := rej.PutScript("s", `require "reject"; reject "not wanted";`); err != nil {
		t.Fatal(err)
	}

	if err := rej.SetActiveScript("s"); err != nil {
		t.Fatal(err)
	}

	srv := NewServer("127.0.0.1:0", &Backend{Delivery: &delivery.Service{Store: store, Log: log,
		ReturnPath: true, DeliveredTo: delivery.DeliveredToFinal}, Log: log, Host: "test.example",
		Auth: auth.Static{"full": "x", "reject": "x", "ok": "x"}}, nil, true)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	go func() { _ = srv.Serve(ln) }()

	t.Cleanup(func() { _ = srv.Close() })

	return ln.Addr().String(), store
}

// transaction sends one message to rcpts and returns the per-recipient
// failures of the DATA reply; failures at RCPT are returned separately.
func transaction(t *testing.T, addr string, rcpts []string) (smtp.LMTPDataError, map[string]error) {
	t.Helper()

	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}

	c := smtp.NewClientLMTP(conn)

	t.Cleanup(func() { _ = c.Close() })

	if err := c.Hello("client.example"); err != nil {
		t.Fatal(err)
	}

	if err := c.Mail("sender@example.org", nil); err != nil {
		t.Fatal(err)
	}

	rcptErr := map[string]error{}

	for _, r := range rcpts {
		if err := c.Rcpt(r, nil); err != nil {
			rcptErr[r] = err
		}
	}

	w, err := c.Data()
	if err != nil {
		t.Fatal(err)
	}

	_, _ = io.WriteString(w, "From: sender@example.org\r\nSubject: hi\r\n\r\nbody\r\n")

	_, err = w.CloseWithLMTPResponse()

	var perRcpt smtp.LMTPDataError
	if err != nil && !errors.As(err, &perRcpt) {
		t.Fatalf("DATA: %v", err)
	}

	return perRcpt, rcptErr
}

// hasStatus reports whether err is an SMTP error with code and enhanced code.
func hasStatus(err error, code int, enhanced smtp.EnhancedCode) bool {
	var e *smtp.SMTPError
	return errors.As(err, &e) && e.Code == code && e.EnhancedCode == enhanced
}

func TestLMTPPerRecipientStatus(t *testing.T) {
	addr, store := serve(t)

	perRcpt, rcptErr := transaction(t, addr, []string{"ok@example.org", "full@example.org", "reject@example.org", "nobody@example.org"})

	if !hasStatus(rcptErr["nobody@example.org"], 550, smtp.EnhancedCode{5, 1, 1}) || len(rcptErr) != 1 {
		t.Errorf("RCPT: %v", rcptErr)
	}

	if !hasStatus(perRcpt["full@example.org"], 552, smtp.EnhancedCode{5, 2, 2}) {
		t.Errorf("over quota: %v", perRcpt["full@example.org"])
	}

	if e := perRcpt["reject@example.org"]; !hasStatus(e, 550, smtp.EnhancedCode{5, 7, 1}) || !strings.Contains(e.Message, "not wanted") {
		t.Errorf("sieve reject: %v", e)
	}

	if _, failed := perRcpt["ok@example.org"]; failed {
		t.Error("ok recipient failed")
	}

	u, _ := store.User("ok")
	mb, _ := u.Lookup("INBOX")
	metas, _ := u.ListMessages(mb.ID)

	if len(metas) != 1 {
		t.Fatalf("ok recipient: %d messages", len(metas))
	}

	raw, _ := u.Read(&metas[0])
	if !strings.Contains(string(raw), "by test.example with LMTP") || !strings.Contains(string(raw), "for <ok@example.org>") {
		t.Fatalf("trace header: %q", raw)
	}

	// Return-Path and Delivered-To above Received, as Dovecot writes them.
	if !strings.HasPrefix(string(raw), "Return-Path: <sender@example.org>\r\nDelivered-To: ok@example.org\r\nReceived: from ") {
		t.Fatalf("delivery headers: %q", raw)
	}
}

// TestLMTPRejectPerRecipient: in one transaction a recipient whose
// script rejects gets 250 and the sender one MDN, a recipient whose
// script erejects gets 550, and the others get their copy.
func TestLMTPRejectPerRecipient(t *testing.T) {
	store := memstore.New()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	activeScripts(t, store, map[string]string{"rej": `require "reject"; reject "go away";`, "erej": `require "ereject"; ereject "not here";`})

	relay, rec := smtptest.Start(t)
	addr := serveBackend(t, &Backend{Delivery: &delivery.Service{Store: store, Log: log, Relay: relay}, Log: log, Host: "test.example",
		Auth: auth.Static{"ok": "x", "rej": "x", "erej": "x"}})

	perRcpt, rcptErr := transaction(t, addr, []string{"ok@example.org", "rej@example.org", "erej@example.org"})
	if len(rcptErr) != 0 {
		t.Fatalf("RCPT: %v", rcptErr)
	}

	if e := perRcpt["erej@example.org"]; !hasStatus(e, 550, smtp.EnhancedCode{5, 7, 1}) || !strings.Contains(e.Message, "not here") {
		t.Errorf("ereject: %v", e)
	}

	for _, r := range []string{"ok@example.org", "rej@example.org"} {
		if e, failed := perRcpt[r]; failed {
			t.Errorf("%s: %v", r, e)
		}
	}

	env := rec.Envelopes()
	if len(env) != 1 || env[0].From != "" || env[0].To[0] != "sender@example.org" || !strings.Contains(env[0].Data, "go away") {
		t.Fatalf("MDNs: %+v", env)
	}

	for user, want := range map[string]int{"ok": 1, "rej": 0, "erej": 0} {
		if n := inboxCount(store, user); n != want {
			t.Errorf("%s: %d messages, want %d", user, n, want)
		}
	}
}

// serveBackend runs an LMTP server for the backend until the test ends
// and returns its address.
func serveBackend(t *testing.T, be *Backend) string {
	t.Helper()

	srv := NewServer("127.0.0.1:0", be, nil, true)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	go func() { _ = srv.Serve(ln) }()

	t.Cleanup(func() { _ = srv.Close() })

	return ln.Addr().String()
}

// inboxCount is the number of messages in the INBOX of user.
func inboxCount(store *memstore.Store, user string) int {
	u, _ := store.User(user)

	mb, err := u.Lookup("INBOX")
	if err != nil {
		return 0
	}

	metas, _ := u.ListMessages(mb.ID)

	return len(metas)
}

// activeScripts gives each user the script as the active one.
func activeScripts(t *testing.T, store *memstore.Store, scripts map[string]string) {
	t.Helper()

	for user, script := range scripts {
		u, _ := store.User(user)
		if err := u.PutScript("s", script); err != nil {
			t.Fatal(err)
		}

		if err := u.SetActiveScript("s"); err != nil {
			t.Fatal(err)
		}
	}
}

// TestLMTPRcptBeforeAuth: with service credentials a transaction before
// AUTH is refused with 530 5.7.0 (RFC 4954 6).
func TestLMTPRcptBeforeAuth(t *testing.T) {
	store := memstore.New()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	srv := NewServer("127.0.0.1:0", &Backend{Delivery: &delivery.Service{Store: store, Log: log}, Log: log,
		Host: "test.example", Auth: auth.Static{"ok": "x"}, ServiceUser: "mta", ServicePassword: "pw"}, nil, true)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	go func() { _ = srv.Serve(ln) }()

	t.Cleanup(func() { _ = srv.Close() })

	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}

	c := smtp.NewClientLMTP(conn)

	t.Cleanup(func() { _ = c.Close() })

	if err := c.Hello("client.example"); err != nil {
		t.Fatal(err)
	}

	if err := c.Mail("sender@example.org", nil); !hasStatus(err, 530, smtp.EnhancedCode{5, 7, 0}) {
		t.Fatalf("MAIL before AUTH: %v, want 530 5.7.0", err)
	}
}
