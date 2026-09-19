// Package prototrace logs the protocol exchange of accounts an operator
// switched the trace on for (admin API, model.Tracer).
//
// Server output is traced from the byte stream: a Conn wraps the
// connection, passes every byte unchanged and splits what the server
// writes into lines; literals the server announces are skipped (the
// server always sends them at once). Client input is reported by the
// protocol parser (Line, Literal): exactly the lines and literals the
// server consumes, so pipelined data, refused literals and read-ahead
// buffering never shift the view.
//
// This is the one deliberate exception to "never log command arguments"
// (logging.Session): the operator asks for it per account and for a
// bounded time. Literal contents (message bodies, APPEND data, fetched
// sections) are never logged, only their size, and long lines are cut.
// The protocol server passes a Filter that masks further contents and
// halts the trace at authentication commands.
package prototrace

import (
	"bytes"
	"io"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Directions of a traced line.
const (
	DirClient = "C" // client to server
	DirServer = "S" // server to client
)

// DefaultMaxLine is the line length logged when none is configured.
const DefaultMaxLine = 2048

// literalTail is how many bytes of a line's end are kept for literal
// detection even when the line itself is cut.
const literalTail = 32

// Filter inspects a complete (possibly cut) line before it is logged. It
// returns the text to log; halt true ends the trace after logging that
// text (e.g. at an authentication command, whose data must not appear).
type Filter func(dir, line string) (text string, halt bool)

// session is an attached trace.
type session struct {
	log     *slog.Logger
	until   time.Time
	maxLine int
	filter  Filter
}

// Conn traces a connection. The zero value is not usable; use Wrap.
type Conn struct {
	rw  io.ReadWriter
	cur atomic.Pointer[session]
	// halted is set when the filter ended the trace; it stays set until
	// the next Start.
	halted atomic.Bool

	mu  sync.Mutex // serialises logging and the server splitter
	out splitter
}

// Wrap returns a tracing wrapper of rw. A trace attached to prev (the
// wrapper of the connection before STARTTLS or COMPRESS) stays attached.
func Wrap(rw io.ReadWriter, prev *Conn) *Conn {
	c := &Conn{rw: rw}
	if prev != nil {
		c.cur.Store(prev.cur.Load())
		c.halted.Store(prev.halted.Load())
	}

	return c
}

// Start attaches a trace until the given time; log carries the session
// fields. maxLine <= 0 uses DefaultMaxLine; filter may be nil. A partial
// server line seen before is discarded.
func (c *Conn) Start(log *slog.Logger, until time.Time, maxLine int, filter Filter) {
	if maxLine <= 0 {
		maxLine = DefaultMaxLine
	}

	c.mu.Lock()
	c.out.reset()
	c.mu.Unlock()

	c.halted.Store(false)

	if old := c.cur.Swap(&session{log: log, until: until, maxLine: maxLine, filter: filter}); old == nil {
		log.Info("protocol trace started", "event", "protocol_trace_start", "until", until.UTC())
	}
}

// Stop detaches the trace (no-op without one).
func (c *Conn) Stop(reason string) {
	if old := c.cur.Swap(nil); old != nil {
		old.log.Info("protocol trace stopped", "event", "protocol_trace_stop", "reason", reason)
	}
}

// Active reports whether a trace is attached.
func (c *Conn) Active() bool { return c.cur.Load() != nil }

// Halted reports whether the filter ended the trace since the last Start.
func (c *Conn) Halted() bool { return c.halted.Load() }

// Read implements io.Reader; client input is reported by the parser.
func (c *Conn) Read(p []byte) (int, error) { return c.rw.Read(p) }

// Write implements io.Writer and traces the server output.
func (c *Conn) Write(p []byte) (int, error) {
	n, err := c.rw.Write(p)
	if n > 0 {
		c.server(p[:n])
	}

	return n, err
}

// Line traces one line the client sent (without CRLF), as the parser
// consumes it.
func (c *Conn) Line(line []byte) {
	s := c.session()
	if s == nil {
		return
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	text, cut := cutLine(line, s.maxLine)
	c.emit(s, DirClient, text, cut)
}

// Literal traces a literal of n bytes the client sent.
func (c *Conn) Literal(n int64) {
	s := c.session()
	if s == nil {
		return
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	c.emit(s, DirClient, literalText(n), 0)
}

// session returns the attached trace; an expired one detaches itself.
func (c *Conn) session() *session {
	s := c.cur.Load()
	if s == nil {
		return nil
	}

	if !time.Now().Before(s.until) {
		c.Stop("expired")
		return nil
	}

	return s
}

// server feeds server output to the splitter.
func (c *Conn) server(p []byte) {
	s := c.session()
	if s == nil {
		return
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	c.out.feed(p, s.maxLine, func(line string, cut int) bool {
		return c.emit(s, DirServer, line, cut)
	})
}

// emit logs one line through the filter; false means the trace halted.
// The caller holds mu.
func (c *Conn) emit(s *session, dir, line string, cut int) bool {
	halt := false
	if s.filter != nil {
		line, halt = s.filter(dir, line)
	}

	attrs := []any{"event", "protocol_trace", "dir", dir, "line", line}
	if cut > 0 {
		attrs = append(attrs, "cut_bytes", cut)
	}

	s.log.Info("protocol trace", attrs...)

	if halt {
		c.halted.Store(true)
		c.out.reset()
		c.Stop("authentication")
	}

	return !halt
}

// cutLine returns line as valid UTF-8, at most maxLine bytes, and how
// many bytes were cut.
func cutLine(line []byte, maxLine int) (string, int) {
	cut := 0
	if len(line) > maxLine {
		cut = len(line) - maxLine
		line = line[:maxLine]
	}

	return strings.ToValidUTF8(string(line), "�"), cut
}

func literalText(n int64) string { return "<literal " + strconv.FormatInt(n, 10) + " bytes>" }

// splitter cuts the server output into lines and skips the literals
// announced at their ends, logging only their size.
type splitter struct {
	buf     []byte // start of the current line, at most maxLine bytes
	tail    []byte // last literalTail bytes of the current line
	cut     int    // bytes of the current line beyond maxLine
	literal int64  // literal bytes still to skip
	litSize int64  // size of the literal being skipped
}

// reset discards a partial line and literal state.
func (s *splitter) reset() {
	s.buf, s.tail, s.cut, s.literal, s.litSize = s.buf[:0], s.tail[:0], 0, 0, 0
}

// feed consumes p and calls emit for every completed line; emit
// returning false stops the feed.
func (s *splitter) feed(p []byte, maxLine int, emit func(line string, cut int) bool) {
	for len(p) > 0 {
		if s.literal > 0 {
			n := min(int64(len(p)), s.literal)
			s.literal -= n
			p = p[n:]

			if s.literal == 0 && !emit(literalText(s.litSize), 0) {
				return
			}

			continue
		}

		i := bytes.IndexByte(p, '\n')
		if i < 0 {
			s.add(p, maxLine)
			return
		}

		s.add(p[:i], maxLine)

		line := strings.ToValidUTF8(strings.TrimSuffix(string(s.buf), "\r"), "�")
		n := literalAtEnd(strings.TrimSuffix(string(s.tail), "\r"))
		cut := s.cut
		s.buf, s.tail, s.cut = s.buf[:0], s.tail[:0], 0

		if n > 0 {
			s.literal, s.litSize = n, n
		}

		if !emit(line, cut) {
			return
		}

		p = p[i+1:]
	}
}

// add appends part of a line.
func (s *splitter) add(p []byte, maxLine int) {
	if room := maxLine - len(s.buf); room > 0 {
		k := min(room, len(p))
		s.buf = append(s.buf, p[:k]...)
		s.cut += len(p) - k
	} else {
		s.cut += len(p)
	}

	s.tail = append(s.tail, p...)
	if len(s.tail) > literalTail {
		s.tail = append(s.tail[:0], s.tail[len(s.tail)-literalTail:]...)
	}
}

// literalAtEnd returns the size of a literal announced at the end of the
// line ({n}, ~{n}; the server never sends {n+}), 0 if none.
func literalAtEnd(line string) int64 {
	if !strings.HasSuffix(line, "}") {
		return 0
	}

	open := strings.LastIndexByte(line, '{')
	if open < 0 {
		return 0
	}

	n, err := strconv.ParseInt(strings.TrimSuffix(line[open+1:len(line)-1], "+"), 10, 64)
	if err != nil || n < 0 {
		return 0
	}

	return n
}
