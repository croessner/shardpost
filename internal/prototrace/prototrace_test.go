package prototrace

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"
)

// sink is an io.ReadWriter that reads nothing and collects writes.
type sink struct{ out bytes.Buffer }

func (s *sink) Read([]byte) (int, error)    { return 0, io.EOF }
func (s *sink) Write(b []byte) (int, error) { return s.out.Write(b) }

type entry struct {
	Event string `json:"event"`
	Dir   string `json:"dir"`
	Line  string `json:"line"`
	Cut   int    `json:"cut_bytes"`
}

// traced returns the protocol_trace entries of the log.
func traced(t *testing.T, log *bytes.Buffer) []entry {
	t.Helper()

	var out []entry

	for _, l := range strings.Split(strings.TrimSpace(log.String()), "\n") {
		if l == "" {
			continue
		}

		var e entry
		if err := json.Unmarshal([]byte(l), &e); err != nil {
			t.Fatalf("log line %q: %v", l, err)
		}

		if e.Event == "protocol_trace" {
			out = append(out, e)
		}
	}

	return out
}

// lines renders the trace as "dir: line" joined by "|".
func lines(t *testing.T, log *bytes.Buffer) string {
	t.Helper()

	var out []string
	for _, e := range traced(t, log) {
		out = append(out, e.Dir+": "+e.Line)
	}

	return strings.Join(out, "|")
}

func newLog() (*slog.Logger, *bytes.Buffer) {
	var buf bytes.Buffer
	return slog.New(slog.NewJSONHandler(&buf, nil)), &buf
}

// started is a traced wrapper of a sink.
func started(maxLine int, filter Filter) (*Conn, *sink, *bytes.Buffer) {
	log, buf := newLog()
	s := &sink{}
	c := Wrap(s, nil)
	c.Start(log, time.Now().Add(time.Hour), maxLine, filter)

	return c, s, buf
}

func write(t *testing.T, c *Conn, chunks ...string) {
	t.Helper()

	for _, s := range chunks {
		if _, err := c.Write([]byte(s)); err != nil {
			t.Fatal(err)
		}
	}
}

func TestNoTraceWithoutStart(t *testing.T) {
	_, buf := newLog()
	c := Wrap(&sink{}, nil)

	c.Line([]byte("a1 NOOP"))
	c.Literal(3)
	write(t, c, "a1 OK done\r\n")

	if got := traced(t, buf); len(got) != 0 {
		t.Fatalf("traced without a session: %+v", got)
	}
}

func TestLinesBothDirections(t *testing.T) {
	c, s, buf := started(0, nil)

	c.Line([]byte("a1 SELECT INBOX"))
	write(t, c, "* 3 EXI", "STS\r\na1 OK [READ-WRITE] done\r\n")
	c.Line([]byte("a2 APPEND INBOX {6+}"))
	c.Literal(6)
	c.Line([]byte(""))

	if s.out.String() != "* 3 EXISTS\r\na1 OK [READ-WRITE] done\r\n" {
		t.Fatalf("bytes changed: %q", s.out.String())
	}

	want := "C: a1 SELECT INBOX|S: * 3 EXISTS|S: a1 OK [READ-WRITE] done|C: a2 APPEND INBOX {6+}|C: <literal 6 bytes>|C: "
	if got := lines(t, buf); got != want {
		t.Fatalf("got  %q\nwant %q", got, want)
	}
}

func TestServerLiteralsAreNotLogged(t *testing.T) {
	c, _, buf := started(0, nil)

	resp := "* 1 FETCH (UID 7 BODY[] {12}\r\nSECRET\r\nBODY)\r\na1 OK done\r\n"
	for i := 0; i < len(resp); i += 5 { // chunks across the literal
		write(t, c, resp[i:min(i+5, len(resp))])
	}

	want := "S: * 1 FETCH (UID 7 BODY[] {12}|S: <literal 12 bytes>|S: )|S: a1 OK done"
	if got := lines(t, buf); got != want || strings.Contains(buf.String(), "SECRET") {
		t.Fatalf("got %q", got)
	}
}

func TestLongLinesAreCut(t *testing.T) {
	c, _, buf := started(10, nil)

	// The literal announcement survives the cut.
	write(t, c, "0123456789ABCDEF {3}\r\nxyz)\r\n")
	c.Line([]byte("0123456789abcdef"))

	got := traced(t, buf)
	if len(got) != 4 || got[0].Line != "0123456789" || got[0].Cut != 11 || got[1].Line != "<literal 3 bytes>" ||
		got[3].Line != "0123456789" || got[3].Cut != 6 {
		t.Fatalf("got %+v", got)
	}
}

func TestExpiryAndStop(t *testing.T) {
	log, buf := newLog()
	c := Wrap(&sink{}, nil)
	c.Start(log, time.Now().Add(-time.Second), 0, nil)
	write(t, c, "a1 OK\r\n")

	if c.Active() || len(traced(t, buf)) != 0 || !strings.Contains(buf.String(), `"reason":"expired"`) {
		t.Fatalf("expired trace still logs: %s", buf.String())
	}

	c.Start(log, time.Now().Add(time.Hour), 0, nil)
	c.Stop("switched off")
	c.Line([]byte("a2 NOOP"))

	if len(traced(t, buf)) != 0 {
		t.Fatal("stopped trace still logs")
	}
}

func TestWrapKeepsSession(t *testing.T) {
	first, _, buf := started(0, nil)
	second := Wrap(&sink{}, first)
	write(t, second, "* OK compressed\r\n")

	if got := lines(t, buf); got != "S: * OK compressed" {
		t.Fatalf("got %q", got)
	}
}

func TestInvalidUTF8(t *testing.T) {
	c, _, buf := started(0, nil)
	write(t, c, "* \xff\xfe\r\n")

	if got := lines(t, buf); got != "S: * �" {
		t.Fatalf("got %q", got)
	}
}

func TestFilterHaltsTrace(t *testing.T) {
	c, _, buf := started(0, func(dir, line string) (string, bool) {
		if dir == DirClient && strings.Contains(line, "LOGIN") {
			return "<authentication>", true
		}

		return strings.ReplaceAll(line, "a4", "A4"), false
	})

	c.Line([]byte("a4 UNAUTHENTICATE"))
	c.Line([]byte("a5 LOGIN user {6+}"))
	c.Literal(6)
	c.Line([]byte("a6 NOOP"))

	if got := lines(t, buf); got != "C: A4 UNAUTHENTICATE|C: <authentication>" || c.Active() || !c.Halted() {
		t.Fatalf("got %q active=%v halted=%v", got, c.Active(), c.Halted())
	}

	if !strings.Contains(buf.String(), `"reason":"authentication"`) {
		t.Fatal("stop not logged")
	}

	c.Start(slog.New(slog.NewJSONHandler(io.Discard, nil)), time.Now().Add(time.Hour), 0, nil)

	if c.Halted() {
		t.Fatal("Start keeps the halt")
	}
}
