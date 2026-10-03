package sieve

import (
	"context"
	"errors"
	"net/textproto"
	"strings"
	"testing"
	"time"
)

func TestInterpreterBudgetReturnsWithoutActions(t *testing.T) {
	script := cpuHeavyScript()

	raw := []byte(msg)
	start := time.Now()

	d, err := RunWithHooks(context.Background(), script, "sender@example.org", "me@example.org", "me@example.org", raw, Hooks{Config: map[string]string{"sieve_max_cpu_time": "20ms"}})
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

// cpuHeavyScript builds a script that spends far more than the 20 ms
// budget in regex matching: a long alternation matched against a long
// subject, many times over, for every MIME part.
func cpuHeavyScript() string {
	var alt []string
	for i := 1; i <= 18; i++ {
		alt = append(alt, "("+strings.Repeat("AB", i)+")")
	}

	var b strings.Builder

	b.WriteString(`require ["mime", "foreverypart", "variables", "regex"];` + "\n")
	b.WriteString(`set "exp" "^((` + strings.Join(alt, "|") + `)|(C))+$";` + "\n")
	b.WriteString(`set "subject" "` + strings.Repeat("ABABC", 20000) + `@";` + "\n")
	b.WriteString("foreverypart {\n")

	for range 200 {
		b.WriteString(`  if string :regex "${subject}" "${exp}" { discard; }` + "\n")
	}

	b.WriteString("}\n")

	return b.String()
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
