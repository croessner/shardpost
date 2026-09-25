package outbound

import (
	"bufio"
	"context"
	"net"
	"strings"
	"testing"
)

// fakeSubmission is a relay that offers XCLIENT, refuses recipients at
// "bad." domains and records the session.
func fakeSubmission(t *testing.T, offerXClient bool) (string, chan string) {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	got := make(chan string, 1)

	go func() {
		defer func() { _ = ln.Close() }()

		c, err := ln.Accept()
		if err != nil {
			return
		}

		defer func() { _ = c.Close() }()

		serveSubmission(c, offerXClient, got)
	}()

	return ln.Addr().String(), got
}

func serveSubmission(c net.Conn, offerXClient bool, got chan<- string) {
	r := bufio.NewReader(c)

	var log strings.Builder

	defer func() { got <- log.String() }()

	reply(c, "220 fake ESMTP\r\n")

	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}

		log.WriteString(line)

		cmd := strings.ToUpper(strings.TrimSpace(line))
		if answer := submissionReply(cmd, offerXClient); answer != "" {
			reply(c, answer)

			continue
		}

		switch cmd {
		case "DATA":
			reply(c, "354 go\r\n")

			if !readData(r, &log) {
				return
			}

			reply(c, "250 2.0.0 queued as ABC\r\n")
		case "RSET":
			reply(c, "250 ok\r\n")
		case "QUIT":
			reply(c, "221 bye\r\n")

			return
		default:
			reply(c, "500 what\r\n")
		}
	}
}

// submissionReply is the fake relay's answer to the commands that need
// no more than one ("" for the others).
func submissionReply(cmd string, offerXClient bool) string {
	switch {
	case strings.HasPrefix(cmd, "EHLO") && offerXClient:
		return "250-fake\r\n250-XCLIENT NAME ADDR LOGIN\r\n250 8BITMIME\r\n"
	case strings.HasPrefix(cmd, "EHLO"):
		return "250-fake\r\n250 8BITMIME\r\n"
	case strings.HasPrefix(cmd, "XCLIENT"):
		return "220 fake ESMTP again\r\n"
	case strings.HasPrefix(cmd, "MAIL"):
		return "250 2.1.0 ok\r\n"
	case strings.HasPrefix(cmd, "RCPT") && strings.Contains(cmd, "@BAD."):
		return "553-5.7.1 <x@bad.example>: Sender address rejected:\r\n553 5.7.1 not owned by user\r\n"
	case strings.HasPrefix(cmd, "RCPT"):
		return "250 2.1.5 ok\r\n"
	}

	return ""
}

// TestSubmitWithXClient: the user is named with XCLIENT, the session is
// greeted anew, and the message goes through.
func TestSubmitWithXClient(t *testing.T) {
	addr, got := fakeSubmission(t, true)

	r := &Relay{Addr: addr, HELO: "shardpost.test"}

	rep, err := r.Submit(context.Background(), &XClient{Login: "user+tag@example.org", Addr: "2001:db8::1"},
		"user@example.org", []string{"a@example.org"}, []byte("Subject: x\r\n\r\nhi\r\n"))
	if err != nil || !rep.Accepted() || !strings.Contains(rep.Final.Message, "queued as ABC") {
		t.Fatalf("submit: %v %+v", err, rep)
	}

	log := <-got
	for _, want := range []string{"XCLIENT LOGIN=user+2Btag@example.org ADDR=IPV6:2001:db8::1", "MAIL FROM:<user@example.org>",
		"RCPT TO:<a@example.org>", "hi\r\n.\r\n"} {
		if !strings.Contains(log, want) {
			t.Errorf("missing %q in %q", want, log)
		}
	}

	if strings.Count(log, "EHLO") != 2 {
		t.Errorf("EHLO was not sent again after XCLIENT: %q", log)
	}
}

// TestSubmitRefusedRecipient: a refused recipient ends the transaction
// before DATA and is reported with its reply.
func TestSubmitRefusedRecipient(t *testing.T) {
	addr, got := fakeSubmission(t, true)

	rep, err := (&Relay{Addr: addr}).Submit(context.Background(), &XClient{Login: "u"}, "u@example.org",
		[]string{"ok@example.org", "x@bad.example"}, []byte("Subject: x\r\n\r\nhi\r\n"))
	if err != nil {
		t.Fatal(err)
	}

	if rep.Accepted() || rep.Stage != "RCPT" || rep.Refused["x@bad.example"].Code != 553 {
		t.Fatalf("report %+v", rep)
	}

	if s := rep.Refused["x@bad.example"].String(); !strings.HasPrefix(s, "553 5.7.1 <x@bad.example>: Sender address rejected:") {
		t.Errorf("reply %q", s)
	}

	if log := <-got; strings.Contains(log, "DATA") {
		t.Errorf("DATA was sent after a refusal: %q", log)
	}
}

// TestSubmitNeedsXClient: a relay that does not offer XCLIENT to this
// host is not used as an open door.
func TestSubmitNeedsXClient(t *testing.T) {
	addr, _ := fakeSubmission(t, false)

	_, err := (&Relay{Addr: addr}).Submit(context.Background(), &XClient{Login: "u"}, "u@example.org",
		[]string{"a@example.org"}, []byte("x"))
	if err != ErrNoXClient {
		t.Fatalf("got %v, want ErrNoXClient", err)
	}
}
