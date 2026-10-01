package delivery

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"
	"time"

	"shardpost/internal/memstore"
	"shardpost/internal/model"
	"shardpost/internal/outbound"
	"shardpost/internal/sieve"
	"shardpost/internal/smtptest"
)

// deadRelay is a relay configuration nobody listens on.
func deadRelay(t *testing.T) *outbound.Relay {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	addr := ln.Addr().String()
	_ = ln.Close()

	return &outbound.Relay{Addr: addr, HELO: "test.local", Timeout: time.Second}
}

// storedMessages returns the messages of a mailbox with their flags.
func storedMessages(t *testing.T, store *memstore.Store, mailbox string) ([]string, [][]string) {
	t.Helper()

	u, _ := store.User(account)

	metas, err := messagesIn(u, mailbox)
	if errors.Is(err, model.ErrNoMailbox) {
		return nil, nil
	}

	if err != nil {
		t.Fatal(err)
	}

	var (
		raws  []string
		flags [][]string
	)

	for i := range metas {
		raw, err := u.Read(&metas[i])
		if err != nil {
			t.Fatal(err)
		}

		raws = append(raws, string(raw))
		flags = append(flags, metas[i].Flags)
	}

	return raws, flags
}

// TestDeliveryHeaders: every copy gets Return-Path and Delivered-To on
// top; a Return-Path the message brought along (folded, in any case) is
// removed, the body is not touched, and the null sender is "<>".
func TestDeliveryHeaders(t *testing.T) {
	const forged = "Received: from mx by mx with ESMTP; Thu, 1 Oct 2026 10:00:00 +0000\r\n" +
		"return-path: <forged@example.net>\r\n\tcontinued\r\n" +
		"From: sender@example.org\r\nSubject: hi\r\n\r\nReturn-Path: <body@example.net>\r\n"

	cases := []struct {
		name, from, wantTop string
	}{
		{"sender", "sender@example.org", "Return-Path: <sender@example.org>\r\nDelivered-To: user@example.org\r\nReceived: "},
		{"null sender", "", "Return-Path: <>\r\nDelivered-To: user@example.org\r\nReceived: "},
		{"null sender in brackets", "<>", "Return-Path: <>\r\nDelivered-To: user@example.org\r\nReceived: "},
	}

	for _, c := range cases {
		svc, store := testService(t, "")
		svc.ReturnPath, svc.DeliveredTo = true, DeliveredToFinal

		req := request(forged)
		req.From = c.from

		if _, err := svc.Deliver(context.Background(), req); err != nil {
			t.Fatal(err)
		}

		raws, _ := storedMessages(t, store, "INBOX")
		if len(raws) != 1 {
			t.Fatalf("%s: %d messages", c.name, len(raws))
		}

		got := raws[0]
		if !strings.HasPrefix(got, c.wantTop) {
			t.Errorf("%s: header starts %q", c.name, got[:min(len(got), 120)])
		}

		if strings.Contains(got, "forged") || strings.Contains(got, "continued") || strings.Count(strings.ToLower(got), "return-path:") != 2 {
			t.Errorf("%s: the old Return-Path is still there: %q", c.name, got)
		}

		if !strings.HasSuffix(got, "\r\n\r\nReturn-Path: <body@example.net>\r\n") {
			t.Errorf("%s: the body changed: %q", c.name, got)
		}
	}
}

// TestRemoveField covers folding, line ends, the position of the field
// and a header without a body.
func TestRemoveField(t *testing.T) {
	cases := map[string]string{
		"A: 1\r\nReturn-Path: <x>\r\nB: 2\r\n\r\nReturn-Path: body\r\n":                "A: 1\r\nB: 2\r\n\r\nReturn-Path: body\r\n",
		"Return-Path: <x>\n\tfolded\nB: 2\n\nbody\n":                                   "B: 2\n\nbody\n",
		"A: 1\r\nRETURN-PATH : <x>\r\n\r\nbody":                                        "A: 1\r\n\r\nbody",
		"A: 1\r\nReturn-Path: <x>\r\n":                                                 "A: 1\r\n",
		"A: 1\r\n\tfold\r\nReturn-Path: <x>\r\nReturn-Path: <y>\r\n B\r\nC: 3\r\n\r\n": "A: 1\r\n\tfold\r\nC: 3\r\n\r\n",
		"A: 1\r\n\r\nReturn-Path: body\r\n":                                            "A: 1\r\n\r\nReturn-Path: body\r\n",
	}

	for in, want := range cases {
		if got := string(removeField([]byte(in), "Return-Path")); got != want {
			t.Errorf("%q: %q, want %q", in, got, want)
		}
	}
}

// TestDeliveryHeadersOff: without the settings the message is stored as
// it came, its own Return-Path included.
func TestDeliveryHeadersOff(t *testing.T) {
	svc, store := testService(t, "")
	svc.DeliveredTo = DeliveredToNone

	raw := "Return-Path: <kept@example.net>\r\n" + message
	if _, err := svc.Deliver(context.Background(), request(raw)); err != nil {
		t.Fatal(err)
	}

	if raws, _ := storedMessages(t, store, "INBOX"); len(raws) != 1 || raws[0] != raw {
		t.Fatalf("stored %q", raws)
	}

	// Delivered-To alone.
	svc, store = testService(t, "")
	svc.DeliveredTo = DeliveredToFinal

	if _, err := svc.Deliver(context.Background(), request(raw)); err != nil {
		t.Fatal(err)
	}

	if raws, _ := storedMessages(t, store, "INBOX"); len(raws) != 1 || raws[0] != "Delivered-To: user@example.org\r\n"+raw {
		t.Fatalf("stored %q", raws)
	}
}

// TestDeliveryHeadersVisibleToSieve: a script sees the headers the
// delivery added; the envelope is unchanged.
func TestDeliveryHeadersVisibleToSieve(t *testing.T) {
	svc, store := testService(t, `require ["fileinto", "mailbox", "envelope"];
if allof (header :is "Return-Path" "<sender@example.org>",
          header :is "Delivered-To" "user@example.org",
          envelope :is "from" "sender@example.org") {
    fileinto :create "Seen";
}`)
	svc.ReturnPath, svc.DeliveredTo = true, DeliveredToFinal

	if _, err := svc.Deliver(context.Background(), request("Return-Path: <forged@example.net>\r\n"+message)); err != nil {
		t.Fatal(err)
	}

	if count(t, store, "Seen") != 1 || count(t, store, "INBOX") != 0 {
		t.Fatal("the script did not see the delivery headers")
	}
}

// rejectService is a delivery service with the script and a relay
// (unless noRelay).
func rejectService(t *testing.T, script string, noRelay bool) (*Service, *memstore.Store, *smtptest.Relay) {
	t.Helper()

	svc, store := testService(t, script)
	svc.Hostname = "mx.example.org"

	relay, rec := smtptest.Start(t)
	if !noRelay {
		svc.Relay = relay
	}

	return svc, store, rec
}

// TestRejectSendsMDN: reject accepts the message, stores nothing and
// sends an MDN to the envelope sender from the null sender.
func TestRejectSendsMDN(t *testing.T) {
	svc, store, rec := rejectService(t, `require "reject"; reject "I do not want this";`, false)

	raw := "From: sender@example.org\r\nTo: user@example.org\r\nSubject: buy now\r\nMessage-ID: <42@example.org>\r\n\r\nbody text\r\n"

	res, err := svc.Deliver(context.Background(), request(raw))
	if err != nil || len(res.Stored) != 0 || count(t, store, "INBOX") != 0 {
		t.Fatalf("%+v %v", res, err)
	}

	env := rec.Envelopes()
	if len(env) != 1 || env[0].From != "" || len(env[0].To) != 1 || env[0].To[0] != "sender@example.org" {
		t.Fatalf("envelopes %+v", env)
	}

	mdn := env[0].Data
	for _, want := range []string{
		"From: Mail Delivery Subsystem <postmaster@example.org>\r\n",
		"To: <sender@example.org>\r\n",
		"Subject: Rejected: buy now\r\n",
		"In-Reply-To: <42@example.org>\r\n",
		"Auto-Submitted: auto-replied (rejected)\r\n",
		"Content-Type: multipart/report; report-type=disposition-notification;",
		"Your message to <user@example.org> was automatically rejected:\r\nI do not want this\r\n",
		"Content-Type: message/disposition-notification\r\n",
		"Reporting-UA: mx.example.org; ShardPost\r\n",
		"Final-Recipient: rfc822; user@example.org\r\n",
		"Original-Message-ID: <42@example.org>\r\n",
		"Disposition: automatic-action/MDN-sent-automatically; deleted\r\n",
		"Content-Type: text/rfc822-headers\r\n\r\nFrom: sender@example.org\r\n",
	} {
		if !strings.Contains(mdn, want) {
			t.Errorf("the MDN lacks %q:\n%s", want, mdn)
		}
	}

	if strings.Contains(mdn, "body text") {
		t.Error("the MDN carries the original body")
	}
}

// TestRejectSettings: the templates and the postmaster address come
// from the settings; sieve_reject_mode=protocol refuses in the protocol.
func TestRejectSettings(t *testing.T) {
	svc, _, rec := rejectService(t, `require "reject"; reject "no";`, false)
	svc.SieveConfig = map[string]string{"rejection_subject": "Refused (%s) 100%%", "rejection_reason": "%t says: %r",
		"postmaster_address": "mailer@%d"}

	if _, err := svc.Deliver(context.Background(), request(message)); err != nil {
		t.Fatal(err)
	}

	sent := rec.Messages()
	if len(sent) != 1 || !strings.Contains(sent[0], "Subject: Refused (hi) 100%\r\n") ||
		!strings.Contains(sent[0], "user@example.org says: no\r\n") || !strings.Contains(sent[0], "<mailer@example.org>") {
		t.Fatalf("sent %q", sent)
	}

	svc, store, rec := rejectService(t, `require "reject"; reject "no";`, false)
	svc.SieveConfig = map[string]string{"sieve_reject_mode": sieve.RejectProtocol}

	var rej *RejectedError
	if _, err := svc.Deliver(context.Background(), request(message)); !errors.As(err, &rej) || rej.Reason != "no" {
		t.Fatalf("protocol mode: %v", err)
	}

	if len(rec.Messages()) != 0 || count(t, store, "INBOX") != 0 {
		t.Fatal("protocol mode sent or stored something")
	}
}

// TestRejectSuppressed: no rejection message for the null sender or
// automatic mail; the message is discarded silently.
func TestRejectSuppressed(t *testing.T) {
	cases := []struct {
		name, from, raw string
	}{
		{"null sender", "", message},
		{"null sender in brackets", "<>", message},
		{"auto-submitted", "sender@example.org", "Auto-Submitted: auto-replied\r\n" + message},
		{"auto-submitted with a comment", "sender@example.org", "Auto-Submitted: auto-generated (bounce)\r\n" + message},
	}

	for _, c := range cases {
		svc, store, rec := rejectService(t, `require "reject"; reject "no";`, false)

		req := request(c.raw)
		req.From = c.from

		res, err := svc.Deliver(context.Background(), req)
		if err != nil || len(res.Stored) != 0 || count(t, store, "INBOX") != 0 || len(rec.Messages()) != 0 {
			t.Errorf("%s: %+v %v sent %q", c.name, res, err, rec.Messages())
		}
	}

	// Auto-Submitted: no is ordinary mail.
	svc, _, rec := rejectService(t, `require "reject"; reject "no";`, false)
	if _, err := svc.Deliver(context.Background(), request("Auto-Submitted: no\r\n"+message)); err != nil || len(rec.Messages()) != 1 {
		t.Fatalf("Auto-Submitted no: %v %q", err, rec.Messages())
	}
}

// TestRejectFallsBackToProtocol: without a relay, and for ereject, the
// rejection is a protocol-level one.
func TestRejectFallsBackToProtocol(t *testing.T) {
	var rej *RejectedError

	svc, store, _ := rejectService(t, `require "reject"; reject "no";`, true)
	if _, err := svc.Deliver(context.Background(), request(message)); !errors.As(err, &rej) || count(t, store, "INBOX") != 0 {
		t.Fatalf("no relay: %v", err)
	}

	svc, _, rec := rejectService(t, `require "ereject"; ereject "no";`, false)
	if _, err := svc.Deliver(context.Background(), request(message)); !errors.As(err, &rej) || len(rec.Messages()) != 0 {
		t.Fatalf("ereject: %v %q", err, rec.Messages())
	}

	// A relay that is configured but down: the protocol rejection too.
	svc, _, _ = rejectService(t, `require "reject"; reject "no";`, true)
	svc.Relay = deadRelay(t)

	if _, err := svc.Deliver(context.Background(), request(message)); !errors.As(err, &rej) {
		t.Fatalf("dead relay: %v", err)
	}
}

// TestVacationPeriods: the period follows the settings — clamped into
// [min, max], the default without :days/:seconds — and a minimum of 0
// lets ":seconds 0" answer every message.
func TestVacationPeriods(t *testing.T) {
	cases := []struct {
		name, script string
		cfg          map[string]string
		replies      int
	}{
		{"default period", `require "vacation"; vacation "away";`, nil, 1},
		{":days 0 is clamped to the minimum", `require "vacation"; vacation :days 0 "away";`, nil, 1},
		{":seconds 0 with min 0 answers every message", `require "vacation-seconds"; vacation :seconds 0 "away";`,
			map[string]string{"sieve_vacation_min_period": "0"}, 3},
		{":days 0 with min 0 answers every message", `require "vacation"; vacation :days 0 "away";`,
			map[string]string{"sieve_vacation_min_period": "0"}, 3},
		{"default period 0", `require "vacation"; vacation "away";`,
			map[string]string{"sieve_vacation_min_period": "0", "sieve_vacation_default_period": "0"}, 3},
	}

	for _, c := range cases {
		svc, store, rec := vacationService(t, c.script, nil)
		svc.SieveConfig = c.cfg

		for range 3 {
			deliverOne(t, svc, store, message)
		}

		if n := len(rec.Messages()); n != c.replies {
			t.Errorf("%s: %d replies, want %d", c.name, n, c.replies)
		}
	}
}

// TestVacationFcc: a sent vacation reply is also stored (RFC 8580):
// with :create in a new mailbox, with its flags; without :create a
// missing mailbox gets no copy, and a suppressed reply none either.
func TestVacationFcc(t *testing.T) {
	svc, store, rec := vacationService(t, `require ["vacation", "fcc", "mailbox", "imap4flags"];
vacation :fcc "Sent/Auto" :create :flags "\\Seen" :subject "Away" "I am away.";`, nil)
	deliverOne(t, svc, store, message)
	deliverOne(t, svc, store, message) // within the period: no reply, no copy

	sent := rec.Messages()
	raws, flags := storedMessages(t, store, "Sent/Auto")

	if len(sent) != 1 || len(raws) != 1 || !strings.Contains(raws[0], "Subject: Away") || !strings.Contains(raws[0], "I am away.") {
		t.Fatalf("sent %q, stored %q", sent, raws)
	}

	if !model.HasFlag(flags[0], `\Seen`) {
		t.Errorf("flags %v", flags[0])
	}

	// Without :create the missing mailbox gets nothing; the reply still
	// goes out and the delivery succeeds.
	svc, store, rec = vacationService(t, `require ["vacation", "fcc"]; vacation :fcc "Missing" "I am away.";`, nil)
	deliverOne(t, svc, store, message)

	if len(rec.Messages()) != 1 || count(t, store, "Missing") != 0 {
		t.Fatalf("sent %d, stored %d", len(rec.Messages()), count(t, store, "Missing"))
	}

	// An existing mailbox needs no :create.
	svc, store, _ = vacationService(t, `require ["vacation", "fcc"]; vacation :fcc "Sent" "I am away.";`, nil)

	u, _ := store.User(account)
	if err := u.CreateMailbox("Sent", nil); err != nil {
		t.Fatal(err)
	}

	deliverOne(t, svc, store, message)

	if count(t, store, "Sent") != 1 {
		t.Fatal("no copy in the existing mailbox")
	}
}

// TestNotifyFcc: a sent notification is also stored, the special-use
// mailbox winning over the name.
func TestNotifyFcc(t *testing.T) {
	svc, store, rec := vacationService(t, `require ["enotify", "fcc", "special-use"];
notify :fcc "Elsewhere" :specialuse "\\Sent" :message "new mail" "mailto:owner@example.org";`, nil)

	u, _ := store.User(account)
	if err := u.CreateMailbox("Gesendet", []string{`\Sent`}); err != nil {
		t.Fatal(err)
	}

	deliverOne(t, svc, store, message)

	raws, _ := storedMessages(t, store, "Gesendet")
	if len(rec.Messages()) != 1 || len(raws) != 1 || !strings.Contains(raws[0], "new mail") || count(t, store, "Elsewhere") != 0 {
		t.Fatalf("sent %q, stored %q", rec.Messages(), raws)
	}

	// Over quota: the copy is lost, the delivery is not.
	svc, store, rec = vacationService(t, `require ["enotify", "fcc", "mailbox"];
notify :fcc "Notes" :create "mailto:owner@example.org";`, nil)

	u, _ = store.User(account)
	if err := u.SetQuotaLimit(model.Quota{Bytes: int64(len(message) + 10)}); err != nil {
		t.Fatal(err)
	}

	deliverOne(t, svc, store, message)

	if len(rec.Messages()) != 1 || count(t, store, "Notes") != 0 {
		t.Fatalf("sent %d, stored %d", len(rec.Messages()), count(t, store, "Notes"))
	}
}
