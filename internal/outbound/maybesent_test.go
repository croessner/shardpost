package outbound

import (
	"bufio"
	"context"
	"errors"
	"net"
	"strings"
	"testing"
	"time"
)

// endOfData runs a relay that takes one message and then ends DATA with
// answer: "554" refuses, "silent" never answers, "close" hangs up.
func endOfData(t *testing.T, answer string) string {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = ln.Close() })

	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}

		defer func() { _ = c.Close() }()

		serveUntilDataEnd(c, answer)
	}()

	return ln.Addr().String()
}

// serveUntilDataEnd answers a session up to the end of DATA, then as
// answer says.
func serveUntilDataEnd(c net.Conn, answer string) {
	r := bufio.NewReader(c)
	reply(c, "220 fake\r\n")

	inData := false

	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}

		switch cmd := strings.ToUpper(strings.TrimSpace(line)); {
		case inData && line == ".\r\n":
			endData(c, r, answer)

			return
		case inData:
		case cmd == "DATA":
			inData = true

			reply(c, "354 go\r\n")
		default:
			reply(c, "250 ok\r\n")
		}
	}
}

// endData gives the answer to the end of DATA.
func endData(c net.Conn, r *bufio.Reader, answer string) {
	switch answer {
	case "554":
		reply(c, "554 5.7.1 refused\r\n")
	case "silent":
		_, _ = r.ReadString('\n') // until the client gives up
	}
}

// TestMaybeSentOnlyOnTimeout: only a timeout after the whole message
// leaves open whether the relay took it; a refusal or a connection the
// relay closed is a plain failure.
func TestMaybeSentOnlyOnTimeout(t *testing.T) {
	for _, c := range []struct {
		answer string
		maybe  bool
	}{
		{"554", false},
		{"close", false},
		{"silent", true},
	} {
		r := &Relay{Addr: endOfData(t, c.answer), HELO: "test", Timeout: 300 * time.Millisecond}

		err := r.Send(context.Background(), "", []string{"a@example.org"}, []byte("Subject: x\r\n\r\nx\r\n"))
		if err == nil || errors.Is(err, ErrMaybeSent) != c.maybe {
			t.Errorf("%s: %v, maybe sent want %v", c.answer, err, c.maybe)
		}
	}
}
