package delivery

import (
	"bufio"
	"context"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"shardpost/internal/memstore"
	"shardpost/internal/outbound"
	"shardpost/internal/sieve"
)

// relayRecorder is a relay that accepts every message and keeps what it
// was sent.
type relayRecorder struct {
	mu   sync.Mutex
	sent []string
}

// messages are the messages the relay took so far.
func (r *relayRecorder) messages() []string {
	r.mu.Lock()
	defer r.mu.Unlock()

	return append([]string(nil), r.sent...)
}

// startRelay runs a minimal SMTP relay for the test.
func startRelay(t *testing.T) (*outbound.Relay, *relayRecorder) {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = ln.Close() })

	rec := &relayRecorder{}

	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}

			go rec.serve(c)
		}
	}()

	return &outbound.Relay{Addr: ln.Addr().String(), HELO: "test.local", Timeout: 5 * time.Second}, rec
}

// serve runs one SMTP session; the message is recorded before the relay
// accepts it, so it is there when the client's send returns.
func (r *relayRecorder) serve(c net.Conn) {
	defer func() { _ = c.Close() }()

	br := bufio.NewReader(c)
	say := func(s string) { _, _ = c.Write([]byte(s + "\r\n")) }

	say("220 relay")

	for {
		line, err := br.ReadString('\n')
		if err != nil {
			return
		}

		switch cmd := strings.ToUpper(strings.TrimSpace(line)); {
		case strings.HasPrefix(cmd, "EHLO"):
			say("250 relay")
		case strings.HasPrefix(cmd, "MAIL"), strings.HasPrefix(cmd, "RCPT"):
			say("250 ok")
		case cmd == "DATA":
			say("354 go")

			var msg strings.Builder

			for {
				l, err := br.ReadString('\n')
				if err != nil {
					return
				}

				if l == ".\r\n" {
					break
				}

				msg.WriteString(l)
			}

			r.mu.Lock()
			r.sent = append(r.sent, msg.String())
			r.mu.Unlock()

			say("250 queued")
		case cmd == "QUIT":
			say("221 bye")

			return
		default:
			say("500 what")
		}
	}
}

// vacationService is a delivery service with a relay, the given active
// script ("" for none) and the given JMAP vacation script ("" for none).
func vacationService(t *testing.T, active, jmap string) (*Service, *memstore.Store, *relayRecorder) {
	t.Helper()

	svc, store := testService(t, active)
	relay, rec := startRelay(t)
	svc.Relay = relay

	if jmap != "" {
		u, _ := store.User(account)
		if err := u.PutScript(sieve.JMAPVacationScript, jmap); err != nil {
			t.Fatal(err)
		}
	}

	return svc, store, rec
}

// deliverOne delivers the test message and fails the test when it is not
// stored in INBOX.
func deliverOne(t *testing.T, svc *Service, store *memstore.Store) {
	t.Helper()

	if _, err := svc.Deliver(context.Background(), request(message)); err != nil {
		t.Fatal(err)
	}

	if n := count(t, store, "INBOX"); n != 1 {
		t.Fatalf("INBOX holds %d messages", n)
	}
}

func year(y int) *time.Time {
	t := time.Date(y, 1, 1, 0, 0, 0, 0, time.UTC)
	return &t
}

// TestJMAPVacationWithoutActiveScript: an account without an active
// script still answers with the vacation of the JMAP VacationResponse.
func TestJMAPVacationWithoutActiveScript(t *testing.T) {
	svc, store, rec := vacationService(t, "", sieve.VacationScript(sieve.VacationSettings{Subject: "Away", TextBody: "back soon"}))

	deliverOne(t, svc, store)

	sent := rec.messages()
	if len(sent) != 1 || !strings.Contains(sent[0], "Subject: Away") || !strings.Contains(sent[0], "back soon") {
		t.Fatalf("sent %q", sent)
	}

	// The period holds: a second message from the sender is not answered.
	deliverAgain(t, svc)

	if n := len(rec.messages()); n != 1 {
		t.Errorf("%d replies to one sender", n)
	}
}

// deliverAgain delivers the test message once more.
func deliverAgain(t *testing.T, svc *Service) {
	t.Helper()

	if _, err := svc.Deliver(context.Background(), request(message)); err != nil {
		t.Fatal(err)
	}
}

// TestJMAPVacationAfterTheActiveScript: the JMAP vacation answers when
// the active script did not, and stays silent when it did — the vacation
// a user keeps in the active script wins.
func TestJMAPVacationAfterTheActiveScript(t *testing.T) {
	jmap := sieve.VacationScript(sieve.VacationSettings{TextBody: "from jmap"})

	svc, store, rec := vacationService(t, `require ["vacation"]; if header :contains "subject" "nomatch" { vacation "from the user"; }`, jmap)
	deliverOne(t, svc, store)

	if sent := rec.messages(); len(sent) != 1 || !strings.Contains(sent[0], "from jmap") {
		t.Errorf("an active script without a vacation: sent %q", sent)
	}

	svc, store, rec = vacationService(t, `require ["vacation"]; vacation "from the user";`, jmap)
	deliverOne(t, svc, store)

	if sent := rec.messages(); len(sent) != 1 || !strings.Contains(sent[0], "from the user") {
		t.Errorf("an active script with a vacation: sent %q", sent)
	}

	svc, _, rec = vacationService(t, `require ["reject"]; reject "no";`, jmap)
	if _, err := svc.Deliver(context.Background(), request(message)); err == nil {
		t.Fatal("the reject did not reject")
	}

	if sent := rec.messages(); len(sent) != 0 {
		t.Errorf("a rejected message was answered: %q", sent)
	}
}

// TestJMAPVacationWindow: outside its dates the JMAP vacation is silent.
func TestJMAPVacationWindow(t *testing.T) {
	for _, c := range []struct {
		name     string
		from, to *time.Time
		want     int
	}{
		{"inside", year(2000), year(2999), 1},
		{"not yet", year(2998), year(2999), 0},
		{"over", year(2000), year(2001), 0},
	} {
		svc, store, rec := vacationService(t, "",
			sieve.VacationScript(sieve.VacationSettings{FromDate: c.from, ToDate: c.to, TextBody: "away"}))
		deliverOne(t, svc, store)

		if n := len(rec.messages()); n != c.want {
			t.Errorf("%s: %d replies, want %d", c.name, n, c.want)
		}
	}
}

// TestJMAPVacationBrokenScript: a JMAP vacation script that does not
// compile leaves the delivery alone.
func TestJMAPVacationBrokenScript(t *testing.T) {
	svc, store, rec := vacationService(t, `require ["fileinto"]; fileinto "Archive";`, "this is not sieve {")

	if _, err := svc.Deliver(context.Background(), request(message)); err != nil {
		t.Fatal(err)
	}

	if n := count(t, store, "Archive"); n != 1 {
		t.Errorf("the active script's target holds %d messages", n)
	}

	if sent := rec.messages(); len(sent) != 0 {
		t.Errorf("sent %q", sent)
	}
}
