package sieve

import (
	"context"
	"net/textproto"
	"strings"
	"testing"
	"time"
)

// vacationHeader is the header of the message vacationDelivery runs on.
var vacationHeader = textproto.MIMEHeader{"To": {"alias@example.org"}, "Subject": {"hi"}, "Message-Id": {"<1@example.net>"}}

// vacationDelivery compiles and runs a vacation script over a message
// to one of its :addresses.
func vacationDelivery(t *testing.T) *Delivery {
	t.Helper()

	script := `require ["vacation"];
vacation :days 3 :subject "Out of office" :addresses ["me@example.org", "alias@example.org"] :handle "ooo" "I am away.";`
	if err := Compile(script); err != nil {
		t.Fatal(err)
	}

	raw := []byte("From: sender@example.net\r\nTo: alias@example.org\r\nSubject: hi\r\nMessage-ID: <1@example.net>\r\n\r\nbody\r\n")

	d, err := Run(context.Background(), script, "sender@example.net", "me@example.org", "me@example.org", raw)
	if err != nil {
		t.Fatal(err)
	}

	return d
}

func TestVacationParseAndRules(t *testing.T) {
	d := vacationDelivery(t)

	if d.Vacation == nil || d.Vacation.Seconds != 3*86400 || d.Vacation.Handle != "ooo" || len(d.FileInto) != 1 {
		t.Fatalf("%+v", d)
	}

	if ok, why := d.Vacation.ShouldReply(vacationHeader, "sender@example.net", "me@example.org"); !ok {
		t.Fatal(why)
	}

	cases := []struct {
		hdr  textproto.MIMEHeader
		from string
		want string
	}{
		{textproto.MIMEHeader{"To": {"alias@example.org"}}, "", "null sender"},
		{textproto.MIMEHeader{"To": {"alias@example.org"}, "Auto-Submitted": {"auto-replied"}}, "x@y", "auto-submitted"},
		{textproto.MIMEHeader{"To": {"alias@example.org"}, "List-Id": {"<l.example>"}}, "x@y", "mailing list"},
		{textproto.MIMEHeader{"To": {"alias@example.org"}, "Precedence": {"bulk"}}, "x@y", "precedence bulk"},
		{textproto.MIMEHeader{"To": {"someone-else@example.org"}}, "x@y", "user not addressed"},
		{textproto.MIMEHeader{"To": {"me@example.org"}}, "owner-list@example.org", "list or system sender"},
		{textproto.MIMEHeader{"To": {"me@example.org"}}, "me@example.org", "sender is the user"},
	}
	for _, c := range cases {
		if ok, why := d.Vacation.ShouldReply(c.hdr, c.from, "me@example.org"); ok || why != c.want {
			t.Errorf("from %q: ok=%v why=%q want %q", c.from, ok, why, c.want)
		}
	}
}

func TestVacationCompose(t *testing.T) {
	d := vacationDelivery(t)
	to, reply := d.Vacation.Compose(vacationHeader, "<sender@example.net>", "me@example.org", time.Unix(0, 0))

	msg := string(reply)
	if to != "sender@example.net" || !strings.Contains(msg, "Subject: Out of office") || !strings.Contains(msg, "Auto-Submitted: auto-replied") ||
		!strings.Contains(msg, "In-Reply-To: <1@example.net>") || !strings.HasSuffix(msg, "I am away.\r\n") {
		t.Fatalf("%q %s", to, msg)
	}

	if d.Vacation.VacationKey("A@x") != d.Vacation.VacationKey("a@x") {
		t.Fatal("key must be case-insensitive in the sender")
	}
}

func TestVacationSeconds(t *testing.T) {
	// :seconds needs vacation-seconds
	if err := Compile(`require ["vacation"]; vacation :seconds 10 "x";`); err == nil {
		t.Fatal("expected error for :seconds without vacation-seconds")
	}

	if err := Compile(`require ["vacation-seconds"]; vacation :seconds 10 "x";`); err != nil {
		t.Fatal(err)
	}
}
