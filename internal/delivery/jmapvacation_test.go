package delivery

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"shardpost/internal/memstore"
	"shardpost/internal/sieve"
	"shardpost/internal/smtptest"
)

// vacationService is a delivery service with a relay, the given active
// script ("" for none) and the given JMAP vacation (nil for none),
// stored where the JMAP front-end keeps it.
func vacationService(t *testing.T, active string, v *sieve.JMAPVacation) (*Service, *memstore.Store, *smtptest.Relay) {
	t.Helper()

	svc, store := testService(t, active)
	relay, rec := smtptest.Start(t)
	svc.Relay = relay

	if v != nil {
		raw, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}

		storeVacation(t, store, string(raw))
	}

	return svc, store, rec
}

// storeVacation writes a raw JMAP vacation record.
func storeVacation(t *testing.T, store *memstore.Store, raw string) {
	t.Helper()

	u, _ := store.User(account)
	if err := u.SetMetadata("", account, map[string]*string{sieve.JMAPVacationEntry: &raw}); err != nil {
		t.Fatal(err)
	}
}

// enabled is a JMAP vacation that is on, with the text and the window.
func enabled(text string, from, to string) *sieve.JMAPVacation {
	v := &sieve.JMAPVacation{IsEnabled: true, TextBody: &text}

	if from != "" {
		v.FromDate = &from
	}

	if to != "" {
		v.ToDate = &to
	}

	return v
}

// deliverOne delivers the message and fails the test when it is not
// stored in INBOX.
func deliverOne(t *testing.T, svc *Service, store *memstore.Store, raw string) {
	t.Helper()

	if _, err := svc.Deliver(context.Background(), request(raw)); err != nil {
		t.Fatal(err)
	}

	if n := count(t, store, "INBOX"); n < 1 {
		t.Fatal("the message is not in INBOX")
	}
}

// TestJMAPVacationWithoutActiveScript: an account without an active
// script still answers with its JMAP vacation, once per sender.
func TestJMAPVacationWithoutActiveScript(t *testing.T) {
	v := enabled("back soon", "", "")
	subject := "Away"
	v.Subject = &subject

	svc, store, rec := vacationService(t, "", v)
	deliverOne(t, svc, store, message)
	deliverOne(t, svc, store, message)

	sent := rec.Messages()
	if len(sent) != 1 || !strings.Contains(sent[0], "Subject: Away") || !strings.Contains(sent[0], "back soon") {
		t.Fatalf("sent %q", sent)
	}
}

// TestJMAPVacationAfterTheActiveScript: the JMAP vacation answers when
// the active script did not — no vacation in it, or it failed — and
// stays silent when it did, or when it rejected the message.
func TestJMAPVacationAfterTheActiveScript(t *testing.T) {
	v := enabled("from jmap", "", "")

	cases := []struct {
		name, active, want string
	}{
		{"no vacation in the script", `require ["vacation"]; if header :contains "subject" "nomatch" { vacation "from the user"; }`, "from jmap"},
		{"a vacation in the script", `require ["vacation"]; vacation "from the user";`, "from the user"},
		{"a failing script", `require ["fileinto"]; fileinto "INBOX"; if header :matches "subject" "*" { error "x"; }`, "from jmap"},
	}

	for _, c := range cases {
		svc, store, rec := vacationService(t, c.active, v)
		deliverOne(t, svc, store, message)

		if sent := rec.Messages(); len(sent) != 1 || !strings.Contains(sent[0], c.want) {
			t.Errorf("%s: sent %q", c.name, sent)
		}
	}

	svc, _, rec := vacationService(t, `require ["ereject"]; ereject "no";`, v)
	if _, err := svc.Deliver(context.Background(), request(message)); err == nil {
		t.Fatal("the ereject did not reject")
	}

	if sent := rec.Messages(); len(sent) != 0 {
		t.Errorf("a rejected message was answered: %q", sent)
	}

	// A reject sends its rejection message, and nothing else.
	svc, _, rec = vacationService(t, `require ["reject"]; reject "no";`, v)
	if _, err := svc.Deliver(context.Background(), request(message)); err != nil {
		t.Fatal(err)
	}

	if sent := rec.Messages(); len(sent) != 1 || strings.Contains(sent[0], "from jmap") || !strings.Contains(sent[0], "report-type=disposition-notification") {
		t.Errorf("a rejected message was answered: %q", sent)
	}
}

// TestJMAPVacationNotKept: a message the active script does not keep —
// discarded, or only forwarded — is not answered.
func TestJMAPVacationNotKept(t *testing.T) {
	for _, active := range []string{"discard;", `redirect "elsewhere@example.org";`} {
		svc, _, rec := vacationService(t, active, enabled("away", "", ""))

		if _, err := svc.Deliver(context.Background(), request(message)); err != nil {
			t.Fatal(err)
		}

		for _, m := range rec.Messages() {
			if strings.Contains(m, "Auto-Submitted: auto-replied") {
				t.Errorf("%s: answered %q", active, m)
			}
		}
	}
}

// TestJMAPVacationWindow: outside its dates the JMAP vacation is silent.
func TestJMAPVacationWindow(t *testing.T) {
	for _, c := range []struct {
		name, from, to string
		want           int
	}{
		{"inside", "2000-01-01T00:00:00Z", "2999-01-01T00:00:00Z", 1},
		{"not yet", "2998-01-01T00:00:00Z", "2999-01-01T00:00:00Z", 0},
		{"over", "2000-01-01T00:00:00Z", "2001-01-01T00:00:00Z", 0},
	} {
		svc, store, rec := vacationService(t, "", enabled("away", c.from, c.to))
		deliverOne(t, svc, store, message)

		if n := len(rec.Messages()); n != c.want {
			t.Errorf("%s: %d replies, want %d", c.name, n, c.want)
		}
	}
}

// TestJMAPVacationSourceIsTheMetadata: the copy of the script in the
// script store is for display; delivery answers from the metadata, and a
// record that does not read answers nobody and delivers as usual.
func TestJMAPVacationSourceIsTheMetadata(t *testing.T) {
	forged := `require ["vacation"]; vacation "forged";`

	svc, store, rec := vacationService(t, "", &sieve.JMAPVacation{})
	u, _ := store.User(account)

	if err := u.PutScript(sieve.JMAPVacationScript, forged); err != nil {
		t.Fatal(err)
	}

	deliverOne(t, svc, store, message)

	if sent := rec.Messages(); len(sent) != 0 {
		t.Errorf("a vacation that is off answered from the script: %q", sent)
	}

	svc, store, rec = vacationService(t, "", enabled("the setting", "", ""))
	u, _ = store.User(account)

	if err := u.PutScript(sieve.JMAPVacationScript, forged); err != nil {
		t.Fatal(err)
	}

	deliverOne(t, svc, store, message)

	if sent := rec.Messages(); len(sent) != 1 || !strings.Contains(sent[0], "the setting") {
		t.Errorf("sent %q", sent)
	}

	for _, raw := range []string{"{not json", `{"isEnabled":true,"fromDate":"soon"}`} {
		svc, store, rec = vacationService(t, "", nil)
		storeVacation(t, store, raw)
		deliverOne(t, svc, store, message)

		if sent := rec.Messages(); len(sent) != 0 {
			t.Errorf("%s: sent %q", raw, sent)
		}
	}
}

// TestJMAPVacationSkipsAutomaticMail: mail marked Auto-Submitted is not
// answered (RFC 5230 5.6 via the vacation checks).
func TestJMAPVacationSkipsAutomaticMail(t *testing.T) {
	svc, store, rec := vacationService(t, "", enabled("away", "", ""))
	deliverOne(t, svc, store, "Auto-Submitted: auto-generated\r\n"+message)

	if sent := rec.Messages(); len(sent) != 0 {
		t.Errorf("sent %q", sent)
	}
}

// TestJMAPVacationHTML: an HTML body goes out as a MIME reply with the
// text as its alternative.
func TestJMAPVacationHTML(t *testing.T) {
	v := enabled("plain away", "", "")
	html := "<p>away</p>"
	v.HTMLBody = &html

	svc, store, rec := vacationService(t, "", v)
	deliverOne(t, svc, store, message)

	sent := rec.Messages()
	if len(sent) != 1 {
		t.Fatalf("sent %q", sent)
	}

	for _, want := range []string{"Content-Type: multipart/alternative", "text/plain", "text/html", "<p>away</p>", "plain away"} {
		if !strings.Contains(sent[0], want) {
			t.Errorf("no %q in %q", want, sent[0])
		}
	}
}
