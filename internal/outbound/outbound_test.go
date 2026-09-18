package outbound

import (
	"bufio"
	"context"
	"net"
	"strings"
	"testing"
)

// fakeSMTP accepts one plaintext session and records envelope and data.
func fakeSMTP(t *testing.T) (addr string, got chan string) {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	got = make(chan string, 1)

	go func() {
		defer func() { _ = ln.Close() }()

		c, err := ln.Accept()
		if err != nil {
			return
		}

		defer func() { _ = c.Close() }()

		serveFakeSMTP(c, got)
	}()

	return ln.Addr().String(), got
}

// reply writes a response line; a failed write shows up as a client error.
func reply(c net.Conn, s string) {
	_, _ = c.Write([]byte(s))
}

// serveFakeSMTP runs one session on c and sends the transcript to got on
// QUIT.
func serveFakeSMTP(c net.Conn, got chan<- string) {
	r := bufio.NewReader(c)

	var log strings.Builder

	reply(c, "220 fake ESMTP\r\n")

	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}

		log.WriteString(line)

		switch cmd := strings.ToUpper(strings.TrimSpace(line)); {
		case strings.HasPrefix(cmd, "EHLO"):
			reply(c, "250-fake\r\n250 8BITMIME\r\n")
		case strings.HasPrefix(cmd, "MAIL"), strings.HasPrefix(cmd, "RCPT"):
			reply(c, "250 ok\r\n")
		case cmd == "DATA":
			reply(c, "354 go\r\n")

			if !readData(r, &log) {
				return
			}

			reply(c, "250 queued\r\n")
		case cmd == "QUIT":
			reply(c, "221 bye\r\n")

			got <- log.String()

			return
		default:
			reply(c, "500 what\r\n")
		}
	}
}

// readData copies the message up to the terminating dot into log; it
// returns false when the connection failed.
func readData(r *bufio.Reader, log *strings.Builder) bool {
	for {
		l, err := r.ReadString('\n')
		if err != nil {
			return false
		}

		log.WriteString(l)

		if l == ".\r\n" {
			return true
		}
	}
}

func TestSend(t *testing.T) {
	addr, got := fakeSMTP(t)

	r := &Relay{Addr: addr, HELO: "test.local"}
	if err := r.Send(context.Background(), "", []string{"a@example.org", "b@example.org"}, []byte("Subject: x\r\n\r\nhi\r\n")); err != nil {
		t.Fatal(err)
	}

	log := <-got
	for _, want := range []string{"EHLO test.local", "MAIL FROM:<>", "RCPT TO:<a@example.org>", "RCPT TO:<b@example.org>", "Subject: x", "hi\r\n.\r\n"} {
		if !strings.Contains(log, want) {
			t.Fatalf("missing %q in %q", want, log)
		}
	}

	if err := (&Relay{Addr: addr, StartTLS: true}).Send(context.Background(), "x@y", []string{"a@b"}, nil); err == nil {
		t.Fatal("expected STARTTLS failure")
	}

	if err := (&Relay{}).Send(context.Background(), "", nil, nil); err != ErrNoRelay {
		t.Fatal(err)
	}
}
