package main

import (
	"bufio"
	"bytes"
	"io"
	"net"
	"strings"
	"sync"
	"testing"

	"shardpost/internal/bench"
)

// fakeLMTP accepts LMTP sessions and keeps every message, dot-stuffing
// undone, by recipient.
type fakeLMTP struct {
	mu   sync.Mutex
	msgs map[string][][]byte
}

func (f *fakeLMTP) serve(t *testing.T) string {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = ln.Close() })

	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}

			go f.session(c)
		}
	}()

	return ln.Addr().String()
}

func (f *fakeLMTP) session(c net.Conn) {
	defer func() { _ = c.Close() }()

	br := bufio.NewReader(c)
	_, _ = io.WriteString(c, "220 fake\r\n")

	var rcpt string

	for {
		line, err := br.ReadString('\n')
		if err != nil {
			return
		}

		switch cmd := strings.ToUpper(strings.TrimSpace(line)); {
		case strings.HasPrefix(cmd, "RCPT TO:"):
			rcpt = strings.Trim(strings.TrimSpace(line)[len("RCPT TO:"):], "<>")
			_, _ = io.WriteString(c, "250 ok\r\n")
		case cmd == "DATA":
			_, _ = io.WriteString(c, "354 go\r\n")

			var msg bytes.Buffer

			for {
				l, err := br.ReadString('\n')
				if err != nil {
					return
				}

				if l == ".\r\n" {
					break
				}

				msg.WriteString(strings.TrimPrefix(l, "."))
			}

			f.mu.Lock()
			f.msgs[rcpt] = append(f.msgs[rcpt], msg.Bytes())
			f.mu.Unlock()

			_, _ = io.WriteString(c, "250 delivered\r\n")
		case cmd == "QUIT":
			_, _ = io.WriteString(c, "221 bye\r\n")
			return
		default:
			_, _ = io.WriteString(c, "250 ok\r\n")
		}
	}
}

// TestFillAccount (temp/13 §7.3, §10): -corpus delivers count messages
// of the generator to one recipient, byte for byte what populate stores
// for an account of that name (same seed, account and numbers).
func TestFillAccount(t *testing.T) {
	f := &fakeLMTP{msgs: map[string][][]byte{}}
	addr := f.serve(t)

	cfg := loadConfig{lmtpAddr: addr, corpus: bench.CorpusIDs, rcpt: "bench-000003", count: 6, first: 2, seed: 7,
		sizes: bench.DefaultSizes, fillRate: 1000, parallel: 1}

	var out bytes.Buffer
	if code := fillAccount(cfg, &out); code != 0 {
		t.Fatalf("exit %d: %s", code, out.String())
	}

	sizes, _ := bench.ParseSizes(bench.DefaultSizes)
	gen := bench.Generator{Seed: 7, Sizes: sizes, Corpus: bench.Corpus{IDs: true}}

	got := f.msgs["bench-000003"]
	if len(got) != 6 {
		t.Fatalf("%d messages", len(got))
	}

	for i, raw := range got {
		want, _ := gen.Message("bench-000003", 2+i)
		if !bytes.Equal(raw, want) {
			t.Errorf("message %d differs from the generator's", 2+i)
		}
	}

	for _, bad := range []loadConfig{{corpus: "nope", rcpt: "a", count: 1}, {corpus: bench.CorpusIDs, count: 1}, {corpus: bench.CorpusIDs, rcpt: "a"}} {
		bad.sizes, bad.fillRate, bad.parallel = bench.DefaultSizes, 1, 1
		if code := fillAccount(bad, io.Discard); code != 2 {
			t.Errorf("%+v: exit %d", bad, code)
		}
	}
}

// TestCRLFLines (review): bare LF line ends become CRLF, so a message
// ending in a bare LF gets no empty line before the terminator.
func TestCRLFLines(t *testing.T) {
	for in, want := range map[string]string{"a\nb\n": "a\r\nb\r\n", "a\r\nb\n": "a\r\nb\r\n", "a\r\n": "a\r\n", "a": "a"} {
		if got := string(crlfLines([]byte(in))); got != want {
			t.Errorf("%q: %q", in, got)
		}
	}
}

// TestDotStuff: lines starting with a dot are doubled.
func TestDotStuff(t *testing.T) {
	for in, want := range map[string]string{"a\r\n.b\r\n": "a\r\n..b\r\n", ".x": "..x", "plain\r\n": "plain\r\n"} {
		if got := string(dotStuff([]byte(in))); got != want {
			t.Errorf("%q: %q", in, got)
		}
	}
}
