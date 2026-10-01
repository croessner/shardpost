// Package smtptest is a minimal SMTP relay for tests: it accepts every
// message and keeps what it was sent.
package smtptest

import (
	"bufio"
	"io"
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
	mode      Mode
}

// Mode is how the relay answers.
type Mode int

// The modes of a relay.
const (
	// Accept takes every message (the default).
	Accept Mode = iota
	// RefuseRcpt answers RCPT TO with 550.
	RefuseRcpt
	// RefuseData answers the end of DATA with 554; nothing is recorded.
	RefuseData
	// Silent records the message but never answers the end of DATA, as
	// a relay does that times out after taking the message.
	Silent
)

// SetMode changes how the relay answers from the next command on.
func (r *Relay) SetMode(m Mode) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.mode = m
}

func (r *Relay) currentMode() Mode {
	r.mu.Lock()
	defer r.mu.Unlock()

	return r.mode
}

// Envelope is one accepted message with its envelope.
type Envelope struct {
	// From is the MAIL FROM path without angle brackets ("" is the
	// null sender), To the RCPT TO paths, Params the parameters of
	// MAIL FROM (BODY=8BITMIME, SMTPUTF8).
	From   string
	Params string
	To     []string
	Data   string
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
// accepts it, so it is there when the client's send returns. The relay
// offers 8BITMIME and SMTPUTF8.
func (r *Relay) serve(c net.Conn) {
	defer func() { _ = c.Close() }()

	ses := &session{r: r, br: bufio.NewReader(c), c: c}
	ses.say("220 relay")

	for {
		line, err := ses.br.ReadString('\n')
		if err != nil || ses.command(strings.TrimSpace(line)) {
			return
		}
	}
}

// session is one SMTP session of the relay.
type session struct {
	r   *Relay
	br  *bufio.Reader
	c   net.Conn
	env Envelope
}

func (s *session) say(line string) { _, _ = s.c.Write([]byte(line + "\r\n")) }

// command answers one command line; true ends the session.
func (s *session) command(line string) bool {
	switch cmd := strings.ToUpper(line); {
	case strings.HasPrefix(cmd, "EHLO"):
		s.say("250-relay\r\n250-8BITMIME\r\n250 SMTPUTF8")
	case strings.HasPrefix(cmd, "MAIL"):
		s.env = Envelope{From: path(line)}
		if i := strings.IndexByte(line, '>'); i >= 0 {
			s.env.Params = strings.TrimSpace(line[i+1:])
		}

		s.say("250 ok")
	case strings.HasPrefix(cmd, "RCPT"):
		if s.r.currentMode() == RefuseRcpt {
			s.say("550 5.1.1 no such user")
			return false
		}

		s.env.To = append(s.env.To, path(line))
		s.say("250 ok")
	case cmd == "DATA":
		return s.data()
	case cmd == "QUIT":
		s.say("221 bye")

		return true
	default:
		s.say("500 what")
	}

	return false
}

// data takes a message and answers according to the mode.
func (s *session) data() bool {
	s.say("354 go")

	msg, ok := readData(s.br)
	if !ok {
		return true
	}

	mode := s.r.currentMode()
	if mode == RefuseData {
		s.say("554 5.7.1 refused")
		return false
	}

	s.env.Data = msg

	s.r.mu.Lock()
	s.r.sent = append(s.r.sent, msg)
	s.r.envelopes = append(s.r.envelopes, s.env)
	s.r.mu.Unlock()

	if mode == Silent {
		// Wait for the client to give up.
		_, _ = io.Copy(io.Discard, s.br)

		return true
	}

	s.say("250 queued")

	return false
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
