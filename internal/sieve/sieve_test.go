package sieve

import (
	"context"
	"testing"
)

const msg = "From: boss@example.org\r\nTo: test@example.org\r\nSubject: Invoice 4711\r\nContent-Type: text/plain\r\n\r\nPlease pay.\r\n"

func TestRun(t *testing.T) {
	d, err := Run(context.Background(), "", "boss@example.org", "test@example.org", "test", []byte(msg))
	if err != nil || len(d.FileInto) != 1 || d.FileInto[0].Mailbox != "INBOX" {
		t.Fatalf("%+v %v", d, err)
	}

	script := `require ["fileinto", "imap4flags", "envelope"];
if allof(header :contains "subject" "invoice", envelope :is "from" "boss@example.org") {
  addflag "\\Flagged";
  fileinto "Finance";
  stop;
}
if header :contains "subject" "spam" { discard; }
`
	if err := Compile(script); err != nil {
		t.Fatal(err)
	}

	d, err = Run(context.Background(), script, "boss@example.org", "test@example.org", "test", []byte(msg))
	if err != nil || len(d.FileInto) != 1 || d.FileInto[0].Mailbox != "Finance" || len(d.FileInto[0].Flags) != 1 || d.FileInto[0].Flags[0] != `\flagged` {
		t.Fatalf("%+v %v", d, err)
	}

	d, _ = Run(context.Background(), script, "x@example.org", "test@example.org", "test", []byte("Subject: buy spam\r\n\r\nx"))
	if len(d.FileInto) != 0 {
		t.Fatalf("expected discard, got %+v", d)
	}

	if err := Compile(`if header :contains { }`); err == nil {
		t.Fatal("expected syntax error")
	}
}
