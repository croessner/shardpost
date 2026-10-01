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
