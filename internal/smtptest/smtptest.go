// Package smtptest is a minimal SMTP relay for tests: it accepts every
// message and keeps what it was sent.
package smtptest

import (
	"bufio"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"shardpost/internal/outbound"
)

// Relay records the messages it accepted.
type Relay struct {
	mu        sync.Mutex
	sent      []string
	envelopes []Envelope
}

// Envelope is one accepted message with its envelope.
type Envelope struct {
	// From is the MAIL FROM path without angle brackets ("" is the
	// null sender), To the RCPT TO paths.
	From string
	To   []string
	Data string
}

// Start runs a relay until the test ends and returns the outbound
// configuration that reaches it.
func Start(t *testing.T) (*outbound.Relay, *Relay) {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = ln.Close() })

	r := &Relay{}

	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}

			go r.serve(c)
		}
	}()

	return &outbound.Relay{Addr: ln.Addr().String(), HELO: "test.local", Timeout: 5 * time.Second}, r
}

// Messages are the messages the relay took so far.
func (r *Relay) Messages() []string {
	r.mu.Lock()
	defer r.mu.Unlock()

	return append([]string(nil), r.sent...)
}

// Envelopes are the messages the relay took so far, with their
// envelopes.
func (r *Relay) Envelopes() []Envelope {
	r.mu.Lock()
	defer r.mu.Unlock()

	return append([]Envelope(nil), r.envelopes...)
}

// path is the address of a MAIL FROM or RCPT TO line.
func path(line string) string {
	_, a, _ := strings.Cut(line, ":")
	if i := strings.IndexByte(a, '>'); i >= 0 {
		a = a[:i]
	}

	return strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(a), "<"))
}

// serve runs one SMTP session. A message is recorded before the relay
// accepts it, so it is there when the client's send returns.
func (r *Relay) serve(c net.Conn) {
	defer func() { _ = c.Close() }()

	br := bufio.NewReader(c)
	say := func(s string) { _, _ = c.Write([]byte(s + "\r\n")) }

	say("220 relay")

	var env Envelope

	for {
		line, err := br.ReadString('\n')
		if err != nil {
			return
		}

		switch cmd := strings.ToUpper(strings.TrimSpace(line)); {
		case strings.HasPrefix(cmd, "EHLO"):
			say("250 relay")
		case strings.HasPrefix(cmd, "MAIL"):
			env = Envelope{From: path(strings.TrimSpace(line))}

			say("250 ok")
		case strings.HasPrefix(cmd, "RCPT"):
			env.To = append(env.To, path(strings.TrimSpace(line)))

			say("250 ok")
		case cmd == "DATA":
			say("354 go")

			msg, ok := readData(br)
			if !ok {
				return
			}

			env.Data = msg

			r.mu.Lock()
			r.sent = append(r.sent, msg)
			r.envelopes = append(r.envelopes, env)
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

// readData reads a message up to the terminating dot.
func readData(br *bufio.Reader) (string, bool) {
	var msg strings.Builder

	for {
		l, err := br.ReadString('\n')
		if err != nil {
			return "", false
		}

		if l == ".\r\n" {
			return msg.String(), true
		}

		msg.WriteString(l)
	}
}
