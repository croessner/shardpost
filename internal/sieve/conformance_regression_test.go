package sieve

import (
	"context"
	"errors"
	"net/textproto"
	"os"
	"strings"
	"testing"
	"time"
)

func TestInterpreterBudgetReturnsWithoutActions(t *testing.T) {
	script, err := os.ReadFile("../../third_party/go-sieve/pigeonhole/tests/execute/errors/cpu-limit.sieve")
	if err != nil {
		t.Fatal(err)
	}

	raw := []byte(msg)
	start := time.Now()

	d, err := RunWithHooks(context.Background(), string(script), "sender@example.org", "me@example.org", "me@example.org", raw, Hooks{Config: map[string]string{"sieve_max_cpu_time": "20ms"}})
	if !errors.Is(err, context.DeadlineExceeded) || d != nil {
		t.Fatalf("delivery=%v error=%v", d, err)
	}

	if time.Since(start) > time.Second {
		t.Fatal("CPU work did not stop promptly")
	}

	if string(raw) != msg {
		t.Fatal("original message changed on timeout")
	}
}

func TestMIMEPartBudgetRejectsBeforeDelivery(t *testing.T) {
	raw := []byte("Content-Type: multipart/mixed; boundary=x\r\n\r\n" + strings.Repeat("--x\r\nContent-Type: text/plain\r\n\r\nx\r\n", 1001) + "--x--\r\n")

	d, err := Run(context.Background(), `require ["foreverypart","fileinto"]; foreverypart { fileinto "Target"; stop; }`, "a@b", "c@d", "c@d", raw)
	if err == nil || !strings.Contains(err.Error(), "MIME structure limit") || d != nil {
		t.Fatalf("%v %v", d, err)
	}
}

func TestVacationProductionRegressions(t *testing.T) {
	hdr := textproto.MIMEHeader{"From": {`"Sender Name" <sender@example.org>`}, "To": {"me@example.org"}, "References": {"<older@example.org> <previous@example.org>"}, "Message-Id": {"<current@example.org>"}, "Subject": {"Hello"}}

	v := &Vacation{Reason: "Away"}
	for _, sender := range []string{"", "<>", "list-request@example.org"} {
		if ok, _ := v.ShouldReply(hdr, sender, "me@example.org"); ok {
			t.Fatalf("replied to %q", sender)
		}
	}

	to, raw := v.Compose(hdr, "sender@example.org", "me@example.org", time.Unix(0, 0))
	if to != "sender@example.org" || !strings.Contains(string(raw), `To: "Sender Name" <sender@example.org>`) || !strings.Contains(string(raw), "References: <older@example.org> <previous@example.org> <current@example.org>") {
		t.Fatalf("bad reply: %s", raw)
	}

	if v.EnvelopeSender("me@example.org") != "" {
		t.Fatal("default envelope sender must be null")
	}

	v.Config = map[string]string{"sieve_vacation_check_recipient": "no"}
	if ok, _ := v.ShouldReply(hdr, "me@example.org", "me@example.org"); ok {
		t.Fatal("recipient-check override allowed reply to self")
	}
}

func TestDynamicRedirectRejectsInvalidAddress(t *testing.T) {
	d, err := Run(context.Background(), `require "variables"; set "target" "not-an-address"; redirect "${target}";`, "a@b", "c@d", "c@d", []byte(msg))
	if err == nil || d != nil {
		t.Fatal("invalid dynamic redirect accepted")
	}
}
