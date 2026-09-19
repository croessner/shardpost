package imapproto

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	"shardpost/internal/logging"
	"shardpost/internal/memstore"
	"shardpost/internal/model"
)

// traceLines returns the traced lines ("C: ..." / "S: ...") of the log.
func traceLines(t *testing.T, raw string) []string {
	t.Helper()

	var out []string

	for _, line := range strings.Split(strings.TrimSpace(raw), "\n") {
		var e struct {
			Event, Dir, Line, User string
		}
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			t.Fatal(err)
		}

		if e.Event == "protocol_trace" {
			if e.User != "test" {
				t.Fatalf("trace line without the account: %s", line)
			}

			out = append(out, e.Dir+": "+e.Line)
		}
	}

	return out
}

func setTrace(t *testing.T, store *memstore.Store, until time.Time) {
	t.Helper()

	u, _ := store.User("test")
	if err := u.(model.Tracer).SetTrace(model.TraceState{Until: until, By: "test"}); err != nil {
		t.Fatal(err)
	}
}

func TestProtocolTrace(t *testing.T) {
	var out logBuffer

	c, store := newTestServer(t, func(o *Options, _ *memstore.Store) {
		o.Log = logging.New(&out, "info")
		o.Trace.Recheck = time.Nanosecond
	})
	defer func() { _ = c.conn.Close() }()

	setTrace(t, store, time.Now().Add(time.Hour))

	c.cmd("LOGIN test test")
	c.cmd("SELECT INBOX")

	msg := "Subject: x\r\n\r\nbody-sentinel\r\n"
	c.cmd("APPEND INBOX {%d+}\r\n%s", len(msg), msg)
	c.cmd("UNAUTHENTICATE")
	c.cmd("LOGIN test password-after-unauth")

	lines := traceLines(t, out.String())
	joined := strings.Join(lines, "\n")

	for _, want := range []string{"S: t1 OK [CAPABILITY", "C: t2 SELECT INBOX", "S: t2 OK [READ-WRITE]", "C: <literal " + strconv.Itoa(len(msg)) + " bytes>", "C: t4 UNAUTHENTICATE"} {
		if !strings.Contains(joined, want) {
			t.Errorf("trace lacks %q:\n%s", want, joined)
		}
	}

	for _, secret := range []string{"body-sentinel", "password-after-unauth", "LOGIN test test"} {
		if strings.Contains(out.String(), secret) {
			t.Errorf("trace leaked %q", secret)
		}
	}
}

func TestProtocolTraceOffAndSwitchedOff(t *testing.T) {
	var out logBuffer

	c, store := newTestServer(t, func(o *Options, _ *memstore.Store) {
		o.Log = logging.New(&out, "info")
		o.Trace.Recheck = time.Nanosecond
	})
	defer func() { _ = c.conn.Close() }()

	// An expired switch traces nothing.
	setTrace(t, store, time.Now().Add(-time.Minute))
	c.cmd("LOGIN test test")
	c.cmd("NOOP")

	if lines := traceLines(t, out.String()); len(lines) != 0 {
		t.Fatalf("traced while off: %v", lines)
	}

	// Switched on: the next login traces; switched off: the running
	// session stops at the next command.
	setTrace(t, store, time.Now().Add(time.Hour))
	c.cmd("UNAUTHENTICATE")
	c.cmd("LOGIN test test")
	c.cmd("NOOP")
	setTrace(t, store, time.Time{})
	// The line that triggers the re-read is already read (and traced);
	// its response and everything after it are not.
	c.cmd("CAPABILITY")
	c.cmd("NOOP")

	joined := strings.Join(traceLines(t, out.String()), "\n")
	if !strings.Contains(joined, "C: t5 NOOP") || strings.Contains(joined, "S: t6") || strings.Contains(joined, "t7") {
		t.Fatalf("unexpected trace:\n%s", joined)
	}

	if !strings.Contains(out.String(), `"reason":"switched off"`) {
		t.Fatal("stop not logged")
	}
}

// sendRaw writes data in one segment and reads until each tag answered.
func sendRaw(t *testing.T, c *client, data string, tags ...string) {
	t.Helper()

	if _, err := c.conn.Write([]byte(data)); err != nil {
		t.Fatal(err)
	}

	for _, tag := range tags {
		for {
			line, err := c.br.ReadString('\n')
			if err != nil {
				t.Fatal(err)
			}

			if strings.HasPrefix(line, tag+" ") {
				break
			}
		}
	}
}

func TestProtocolTracePipelinedLogin(t *testing.T) {
	var out logBuffer

	c, store := newTestServer(t, func(o *Options, _ *memstore.Store) {
		o.Log = logging.New(&out, "info")
		o.Trace.Recheck = time.Nanosecond
	})
	defer func() { _ = c.conn.Close() }()

	setTrace(t, store, time.Now().Add(time.Hour))
	c.cmd("LOGIN test test")

	// LOGIN while authenticated, password as literal, SASL data pipelined:
	// the trace halts at the command and stays off until the next login.
	sendRaw(t, c, "p0 LOGIN test {12+}\r\nliteral-pass\r\nc2VjcmV0\r\np1 NOOP\r\n", "p0", "c2VjcmV0", "p1")

	joined := strings.Join(traceLines(t, out.String()), "\n")
	if !strings.Contains(joined, "C: "+traceMaskedAuth) || strings.Contains(joined, "p1") {
		t.Fatalf("trace not halted:\n%s", joined)
	}

	// Pipelined behind UNAUTHENTICATE: the next login is not traced.
	sendRaw(t, c, "p2 UNAUTHENTICATE\r\np3 LOGIN test pipelined-secret\r\np4 AUTHENTICATE PLAIN c2VjcmV0\r\n", "p2", "p3", "p4")

	for _, secret := range []string{"literal-pass", "pipelined-secret", "c2VjcmV0"} {
		if strings.Contains(out.String(), secret) {
			t.Fatalf("trace leaked %q", secret)
		}
	}
}

func TestProtocolTraceResumesAfterExtension(t *testing.T) {
	var out logBuffer

	c, store := newTestServer(t, func(o *Options, _ *memstore.Store) {
		o.Log = logging.New(&out, "info")
		o.Trace.Recheck = time.Hour
	})
	defer func() { _ = c.conn.Close() }()

	setTrace(t, store, time.Now().Add(300*time.Millisecond))
	c.cmd("LOGIN test test")

	// Extended before the session's trace ends, without a recheck in
	// between: the expired trace re-reads the switch at once.
	setTrace(t, store, time.Now().Add(time.Hour))
	time.Sleep(400 * time.Millisecond)
	c.cmd("NOOP")
	c.cmd("CAPABILITY")

	if !strings.Contains(strings.Join(traceLines(t, out.String()), "\n"), "C: t3 CAPABILITY") {
		t.Fatalf("trace not resumed:\n%s", out.String())
	}
}

func TestMaskPreview(t *testing.T) {
	for in, want := range map[string]string{
		`* 1 FETCH (UID 4 PREVIEW "Hallo \"du\" da" FLAGS (\Seen))`: `* 1 FETCH (UID 4 PREVIEW "<masked>" FLAGS (\Seen))`,
		`* 1 FETCH (PREVIEW NIL)`:                                   `* 1 FETCH (PREVIEW NIL)`,
		`* 1 FETCH (PREVIEW "cut off`:                               `* 1 FETCH (PREVIEW "<masked>"`,
		`* 2 FETCH (PREVIEW "a") * PREVIEW "b"`:                     `* 2 FETCH (PREVIEW "<masked>") * PREVIEW "<masked>"`,
	} {
		if got := maskPreview(in); got != want {
			t.Errorf("maskPreview(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestProtocolTraceRefusedNonSyncLiteral(t *testing.T) {
	var out logBuffer

	c, store := newTestServer(t, func(o *Options, _ *memstore.Store) {
		o.Log = logging.New(&out, "info")
		o.MaxLiteral = 64
	})
	defer func() { _ = c.conn.Close() }()

	setTrace(t, store, time.Now().Add(time.Hour))
	c.cmd("LOGIN test test")

	body := strings.Repeat("body-sentinel\r\n", 6) // 90 bytes > APPENDLIMIT
	sendRaw(t, c, fmt.Sprintf("r1 APPEND INBOX {%d+}\r\n%s\r\nr2 NOOP\r\n", len(body), body), "r1", "r2")

	joined := strings.Join(traceLines(t, out.String()), "\n")
	if strings.Contains(out.String(), "body-sentinel") || !strings.Contains(joined, "S: r1 NO [TOOBIG]") || !strings.Contains(joined, "S: r2 OK") {
		t.Fatalf("refused literal not skipped:\n%s", joined)
	}
}
