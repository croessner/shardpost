package lmtpd

import (
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/emersion/go-smtp"

	"shardpost/internal/auth"
	"shardpost/internal/delivery"
	"shardpost/internal/jmap"
	"shardpost/internal/memstore"
	"shardpost/internal/smtptest"
)

// vacationAccount is the account of the end-to-end vacation test, and
// vacationAddress the address LMTP delivers to it (the static backend
// names the account by the local part).
const (
	vacationAccount = "alice"
	vacationAddress = "alice@example.org"
)

// TestJMAPVacationEndToEnd: a VacationResponse set over JMAP answers mail
// that arrives over LMTP, through the relay, once per sender; turning it
// off over JMAP stops it.
func TestJMAPVacationEndToEnd(t *testing.T) {
	store := memstore.New()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	authn := auth.Static{vacationAccount: "secret"}
	relay, rec := smtptest.Start(t)

	front := jmap.New(jmap.Options{Store: store, Auth: authn, Log: log, InsecureAuth: true, VacationResponse: true})

	setVacation(t, front, `{"isEnabled":true,"subject":"Out of office","textBody":"Back on Monday.","htmlBody":"<p>Back on <b>Monday</b>.</p>"}`)

	srv := NewServer("127.0.0.1:0", &Backend{Delivery: &delivery.Service{Store: store, Log: log, Relay: relay}, Log: log,
		Host: "test.example", Auth: authn}, nil, true)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	go func() { _ = srv.Serve(ln) }()

	t.Cleanup(func() { _ = srv.Close() })

	send(t, ln.Addr().String())
	send(t, ln.Addr().String())

	sent := rec.Messages()
	if len(sent) != 1 {
		t.Fatalf("%d replies, want one: %q", len(sent), sent)
	}

	for _, want := range []string{"Subject: Out of office", "Auto-Submitted: auto-replied", "To: <sender@example.org>",
		"multipart/alternative", "Back on Monday.", "<b>Monday</b>"} {
		if !strings.Contains(sent[0], want) {
			t.Errorf("no %q in the reply %q", want, sent[0])
		}
	}

	setVacation(t, front, `{"isEnabled":false}`)
	sendFrom(t, ln.Addr().String(), "other@example.org")

	if n := len(rec.Messages()); n != 1 {
		t.Errorf("%d replies after the vacation was turned off", n)
	}
}

// setVacation updates the account's VacationResponse over JMAP.
func setVacation(t *testing.T, front *jmap.Server, patch string) {
	t.Helper()

	r := httptest.NewRequest(http.MethodPost, jmap.PathAPI, strings.NewReader(`{"using":["urn:ietf:params:jmap:core",
		"urn:ietf:params:jmap:vacationresponse"],"methodCalls":[["VacationResponse/set",
		{"accountId":"`+vacationAccount+`","update":{"singleton":`+patch+`}},"c1"]]}`))
	r.SetBasicAuth(vacationAccount, "secret")

	w := httptest.NewRecorder()
	front.ServeHTTP(w, r)

	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"updated":{"singleton":null}`) {
		t.Fatalf("VacationResponse/set answered %d: %s", w.Code, w.Body.String())
	}
}

// send delivers one message from sender@example.org over LMTP.
func send(t *testing.T, addr string) {
	t.Helper()

	sendFrom(t, addr, "sender@example.org")
}

// sendFrom delivers one message to the account over LMTP.
func sendFrom(t *testing.T, addr, from string) {
	t.Helper()

	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}

	c := smtp.NewClientLMTP(conn)

	defer func() { _ = c.Close() }()

	if err := c.Hello("client.example"); err != nil {
		t.Fatal(err)
	}

	if err := c.SendMail(from, []string{vacationAddress}, strings.NewReader("From: "+from+"\r\nTo: "+vacationAddress+
		"\r\nSubject: hello\r\nMessage-ID: <1@example.org>\r\n\r\nbody\r\n")); err != nil {
		t.Fatal(err)
	}
}
