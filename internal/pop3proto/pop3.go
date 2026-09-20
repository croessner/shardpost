// Package pop3proto is a POP3 (RFC 1939) front-end on the same model.User
// storage API as the IMAP layer. It serves INBOX only, takes the maildrop
// lock for the session, and marks retrieved messages \Seen so IMAP clients
// share the view. Supported: USER/PASS, AUTH PLAIN (RFC 5034), CAPA, STLS,
// STAT, LIST, UIDL, RETR, TOP, DELE, RSET, NOOP, QUIT, UTF8.
package pop3proto

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"shardpost/internal/logging"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/otel/attribute"

	"shardpost/internal/auth"
	"shardpost/internal/membudget"
	"shardpost/internal/metrics"
	"shardpost/internal/model"
	"shardpost/internal/provision"
	"shardpost/internal/telemetry"
)

// Auth verifies credentials.
type Auth = auth.Authenticator

// Store opens per-user views.
type Store interface {
	User(name string) (model.User, error)
}

// Options configures the server.
type Options struct {
	Store        Store
	Auth         Auth
	Log          *slog.Logger
	TLS          *tls.Config // nil: no STLS
	InsecureAuth bool
	LockTTL      time.Duration
	IdleTimeout  time.Duration
	// MarkSeen sets \Seen on RETR (Dovecot default behaviour).
	MarkSeen bool
}

// Server accepts POP3 connections.
type Server struct {
	o     Options
	mu    sync.Mutex
	lns   map[net.Listener]struct{}
	conns map[net.Conn]struct{}
	done  bool
}

// New creates a server.
func New(o Options) *Server {
	if o.Log == nil {
		o.Log = slog.Default()
	}

	if o.LockTTL == 0 {
		o.LockTTL = 10 * time.Minute
	}

	if o.IdleTimeout == 0 {
		o.IdleTimeout = 10 * time.Minute
	}

	return &Server{o: o, lns: map[net.Listener]struct{}{}, conns: map[net.Conn]struct{}{}}
}

// Serve accepts connections until Close.
func (s *Server) Serve(ln net.Listener) error {
	s.mu.Lock()
	s.lns[ln] = struct{}{}
	s.mu.Unlock()

	for {
		nc, err := ln.Accept()
		if err != nil {
			s.mu.Lock()
			done := s.done
			s.mu.Unlock()

			if done {
				return nil
			}

			return err
		}

		s.mu.Lock()
		s.conns[nc] = struct{}{}
		s.mu.Unlock()
		metrics.Connections.WithLabelValues(protoPOP3).Inc()

		go func() {
			newConn(s, nc).serve()
			s.mu.Lock()
			delete(s.conns, nc)
			s.mu.Unlock()
			metrics.Connections.WithLabelValues(protoPOP3).Dec()
		}()
	}
}

// ListenAndServe listens on addr.
func (s *Server) ListenAndServe(addr string) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}

	return s.Serve(ln)
}

// Close stops listening and closes all connections.
func (s *Server) Close() error { return s.Shutdown(0) }

// Shutdown stops listening, waits up to timeout for sessions to finish
// (POP3 sessions are short), then closes the rest.
func (s *Server) Shutdown(timeout time.Duration) error {
	s.mu.Lock()

	s.done = true
	for ln := range s.lns {
		_ = ln.Close()
	}
	s.mu.Unlock()

	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		s.mu.Lock()
		n := len(s.conns)
		s.mu.Unlock()

		if n == 0 {
			return nil
		}

		time.Sleep(100 * time.Millisecond)
	}

	s.mu.Lock()
	for nc := range s.conns {
		_, _ = fmt.Fprint(nc, "-ERR [SYS/TEMP] Server shutting down\r\n")
		_ = nc.Close()
	}
	s.mu.Unlock()

	return nil
}

type conn struct {
	lockProbe model.LockProbe
	s         *Server
	nc        net.Conn
	br        *bufio.Reader
	bw        *bufio.Writer
	log       *slog.Logger
	tls       bool

	user    model.User
	name    string
	pending string // USER before PASS
	lease   model.Lease
	mbox    *model.Mailbox
	msgs    []model.Meta // maildrop snapshot, index+1 == message number
	deleted map[int]bool
}

func newConn(s *Server, nc net.Conn) *conn {
	c := &conn{s: s, nc: nc, log: logging.Session(s.o.Log, nc), deleted: map[int]bool{}}
	_, c.tls = nc.(*tls.Conn)
	c.setIO(nc)

	return c
}

// maxLineLen bounds a POP3 command line; RFC 1939 allows 255 octets.
const maxLineLen = 4096

func (c *conn) setIO(rw io.ReadWriter) {
	c.br = bufio.NewReaderSize(rw, maxLineLen+2)
	c.bw = bufio.NewWriterSize(rw, 64<<10)
}

// readLine reads one command line, refusing oversized lines without
// buffering them.
func (c *conn) readLine() (string, error) {
	line, err := c.br.ReadSlice('\n')
	if errors.Is(err, bufio.ErrBufferFull) {
		for errors.Is(err, bufio.ErrBufferFull) {
			_, err = c.br.ReadSlice('\n')
		}

		return "", errLineTooLong
	}

	if err != nil && len(line) == 0 {
		return "", err
	}

	return strings.TrimRight(string(line), "\r\n"), nil
}

var errLineTooLong = errors.New("line too long")

// protoPOP3 is the protocol name in metrics and authentication requests.
const protoPOP3 = "pop3"

// POP3 commands.
const (
	cmdCAPA = "CAPA"
	cmdNOOP = "NOOP"
	cmdQUIT = "QUIT"
	cmdUTF8 = "UTF8"
	cmdSTLS = "STLS"
	cmdUSER = "USER"
	cmdPASS = "PASS"
	cmdAUTH = "AUTH"
	cmdSTAT = "STAT"
	cmdLIST = "LIST"
	cmdUIDL = "UIDL"
	cmdRETR = "RETR"
	cmdTOP  = "TOP"
	cmdDELE = "DELE"
	cmdRSET = "RSET"
)

// SASL mechanisms.
const (
	mechPlain       = "PLAIN"
	mechXOAUTH2     = "XOAUTH2"
	mechOAuthBearer = "OAUTHBEARER"
)

// knownCommands bounds the label cardinality of the command metrics.
var knownCommands = map[string]bool{
	cmdCAPA: true, cmdNOOP: true, cmdQUIT: true, cmdUTF8: true, cmdSTLS: true, cmdUSER: true, cmdPASS: true,
	cmdAUTH: true, cmdSTAT: true, cmdLIST: true, cmdUIDL: true, cmdRETR: true, cmdTOP: true, cmdDELE: true, cmdRSET: true,
}

func metricName(cmd string) string {
	if knownCommands[cmd] {
		return cmd
	}

	return "UNKNOWN"
}

func (c *conn) ok(format string, args ...any) {
	// Write errors on the buffer surface on the next Flush.
	_, _ = fmt.Fprintf(c.bw, "+OK "+format+"\r\n", args...)
}

func (c *conn) err(format string, args ...any) {
	_, _ = fmt.Fprintf(c.bw, "-ERR "+format+"\r\n", args...)
}

func (c *conn) serve() {
	defer logging.Begin(c.log, c.tls)()

	// The raw connection is closed even after STLS replaced c.nc.
	nc := c.nc
	defer func() { _ = nc.Close() }()
	defer c.cleanup()

	c.ok("ShardPost POP3 ready")

	if err := c.bw.Flush(); err != nil {
		return
	}

	for {
		_ = c.nc.SetReadDeadline(time.Now().Add(c.s.o.IdleTimeout))

		line, err := c.readLine()
		if errors.Is(err, errLineTooLong) {
			c.err("Line too long")

			if c.bw.Flush() != nil {
				return
			}

			continue
		}

		if err != nil {
			return
		}

		quit, err := c.dispatch(line)
		if err != nil {
			c.log.Debug("connection error", "err", err)
			return
		}

		if err := c.bw.Flush(); err != nil || quit {
			return
		}
	}
}

// dispatch runs one command line with tracing, metrics and logging.
func (c *conn) dispatch(line string) (quit bool, err error) {
	cmd, arg, _ := strings.Cut(line, " ")
	cmd = strings.ToUpper(cmd)
	mname := metricName(cmd)
	start := time.Now()
	_ = c.nc.SetWriteDeadline(time.Now().Add(2 * time.Minute))
	ctx, span := telemetry.Span(context.Background(), "pop3."+mname, attribute.String("pop3.user", c.name))

	// The store records its operations below this command's span.
	model.UseRequest(ctx, c.user)
	defer model.UseRequest(context.Background(), c.user)

	quit, err = c.handle(cmd, arg)
	if err != nil {
		span.RecordError(err)
	}

	span.End()
	metrics.CommandLatency.WithLabelValues(protoPOP3, mname).Observe(time.Since(start).Seconds())
	c.log.Debug("command completed", "event", "command_complete", "command", mname, "duration_ms", time.Since(start).Milliseconds(), "handler_error", err != nil)

	return quit, err
}

// cleanup releases the maildrop lock and the storage view.
func (c *conn) cleanup() {
	if c.lease != nil {
		c.lease.Release()
		c.lease = nil
	}

	if c.user != nil {
		c.user.Close()
		c.user = nil
	}
}

func (c *conn) canAuth() bool { return c.tls || c.s.o.InsecureAuth }

func (c *conn) capa() []string {
	caps := []string{"TOP", "UIDL", "RESP-CODES", "AUTH-RESP-CODE", "PIPELINING", "UTF8", "IMPLEMENTATION ShardPost"}
	if !c.tls && c.s.o.TLS != nil {
		caps = append(caps, cmdSTLS)
	}

	if c.canAuth() {
		caps = append(caps, "USER", "SASL PLAIN XOAUTH2 OAUTHBEARER")
	}

	return caps
}

func (c *conn) handle(cmd, arg string) (quit bool, err error) {
	if quit, done, err := c.handleAnyState(cmd); done {
		return quit, err
	}

	if c.user == nil {
		return false, c.handleAuthorization(cmd, arg)
	}

	if c.lockProbe.Locked(c.user) {
		c.log.Info("session closed, account locked (import or maintenance)")
		c.err("[SYS/TEMP] Mailbox maintenance in progress, try again later")

		return true, nil
	}

	if c.lease != nil && c.lease.Lost() {
		c.err("[SYS/TEMP] Maildrop lock lost, please reconnect")
		return true, nil
	}

	return false, c.handleTransaction(cmd, arg)
}

// handleAnyState runs the commands valid in every state; done is false for
// other commands.
func (c *conn) handleAnyState(cmd string) (quit, done bool, err error) {
	switch cmd {
	case cmdCAPA:
		c.ok("Capability list follows")

		for _, x := range c.capa() {
			_, _ = fmt.Fprintf(c.bw, "%s\r\n", x)
		}

		_, _ = fmt.Fprint(c.bw, ".\r\n")

		return false, true, nil
	case cmdNOOP:
		c.ok("")
		return false, true, nil
	case cmdQUIT:
		return true, true, c.cmdQuit()
	case cmdUTF8:
		c.ok("UTF8 enabled")
		return false, true, nil
	}

	return false, false, nil
}

// handleAuthorization runs a command in the AUTHORIZATION state.
func (c *conn) handleAuthorization(cmd, arg string) error {
	switch cmd {
	case cmdSTLS:
		return c.cmdSTLS()
	case cmdUSER:
		if !c.canAuth() {
			c.err("[SYS/PERM] TLS required")
			return nil
		}

		c.pending = arg
		c.ok("Send PASS")

		return nil
	case cmdPASS:
		if c.pending == "" {
			c.err("USER first")
			return nil
		}

		return c.login(c.pending, arg)
	case cmdAUTH:
		return c.cmdAuth(arg)
	}

	c.err("[AUTH] Please authenticate first")

	return nil
}

// handleTransaction runs a command in the TRANSACTION state.
func (c *conn) handleTransaction(cmd, arg string) error {
	switch cmd {
	case cmdSTAT:
		c.cmdStat()
	case cmdLIST:
		return c.listing(arg, func(i int, m *model.Meta) string { return fmt.Sprintf("%d %d", i, m.Size) })
	case cmdUIDL:
		return c.listing(arg, func(i int, m *model.Meta) string { return fmt.Sprintf("%d %s", i, m.UIDL()) })
	case cmdRETR:
		return c.retr(arg, -1)
	case cmdTOP:
		return c.cmdTop(arg)
	case cmdDELE:
		i, m := c.message(arg)
		if m == nil {
			return nil
		}

		c.deleted[i] = true
		c.ok("Message %d deleted", i)
	case cmdRSET:
		c.deleted = map[int]bool{}
		c.ok("Reset")
	default:
		c.err("Unknown command")
	}

	return nil
}

// cmdStat reports the number and total size of the undeleted messages.
func (c *conn) cmdStat() {
	var size int64

	n := 0

	for i, m := range c.msgs {
		if !c.deleted[i+1] {
			n++
			size += m.Size
		}
	}

	c.ok("%d %d", n, size)
}

// cmdTop handles "TOP msg lines".
func (c *conn) cmdTop(arg string) error {
	numStr, linesStr, _ := strings.Cut(arg, " ")

	lines, err := strconv.Atoi(strings.TrimSpace(linesStr))
	if err != nil || lines < 0 {
		c.err("TOP msg lines")
		return nil
	}

	return c.retr(numStr, lines)
}

func (c *conn) cmdSTLS() error {
	if c.tls || c.s.o.TLS == nil {
		c.err("STLS not available")
		return nil
	}

	c.ok("Begin TLS negotiation")

	if err := c.bw.Flush(); err != nil {
		return err
	}

	tc := tls.Server(c.nc, c.s.o.TLS)
	if err := tc.Handshake(); err != nil {
		c.log.Warn("TLS handshake failed", "event", "tls_failed")
		return err
	}

	c.nc, c.tls = tc, true
	c.log.Info("TLS established", "event", "tls_established", "tls_version", tc.ConnectionState().Version)
	c.setIO(tc)

	return nil
}

func (c *conn) cmdAuth(arg string) error {
	mech, ir, _ := strings.Cut(arg, " ")

	mech = strings.ToUpper(mech)
	switch mech {
	case mechPlain, mechXOAUTH2, mechOAuthBearer:
	default:
		c.err("[SYS/PERM] Unsupported mechanism")
		return nil
	}

	if !c.canAuth() {
		c.err("[SYS/PERM] TLS required")
		return nil
	}

	var err error
	if ir == "" {
		if ir, err = c.continuation(""); err != nil {
			return err
		}

		if ir == "*" {
			c.err("Authentication cancelled")
			return nil
		}
	}

	req := auth.Request{Mechanism: mech, Protocol: protoPOP3, Client: c.nc.RemoteAddr(), Local: c.nc.LocalAddr(), TLS: c.tls}
	if err := parseSASL(&req, ir); err != nil {
		c.err("%s", err.Error())
		return nil
	}

	res, aerr := c.s.o.Auth.Authenticate(context.Background(), req)
	if aerr != nil && mech != mechPlain {
		if _, err := c.continuation(auth.OAuthBearerFailure(401, "")); err != nil {
			return err
		}
	}

	return c.finishLogin(req, res, aerr)
}

// parseSASL decodes the initial response of req.Mechanism into req.
func parseSASL(req *auth.Request, ir string) error {
	var err error

	switch req.Mechanism {
	case mechPlain:
		req.Username, req.Password, err = auth.ParsePlain(ir)
	case mechXOAUTH2:
		req.Username, req.Token, err = auth.ParseXOAUTH2(ir)
	case mechOAuthBearer:
		req.Username, req.Token, err = auth.ParseOAuthBearer(ir)
	}

	return err
}

func (c *conn) continuation(text string) (string, error) {
	_, _ = fmt.Fprintf(c.bw, "+ %s\r\n", text)

	if err := c.bw.Flush(); err != nil {
		return "", err
	}

	return c.readLine()
}

// login handles USER/PASS.
func (c *conn) login(user, pass string) error {
	c.pending = ""
	req := auth.Request{Username: user, Password: pass, Mechanism: cmdUSER, Protocol: protoPOP3, Client: c.nc.RemoteAddr(), Local: c.nc.LocalAddr(), TLS: c.tls}
	res, err := c.s.o.Auth.Authenticate(context.Background(), req)

	return c.finishLogin(req, res, err)
}

// finishLogin takes the maildrop lock and loads the snapshot.
func (c *conn) finishLogin(req auth.Request, res *auth.Result, aerr error) error {
	switch {
	case errors.Is(aerr, auth.ErrTemporary):
		c.log.Warn("authentication unavailable", "event", "auth_unavailable")
		c.err("[SYS/TEMP] Authentication temporarily unavailable")

		return nil
	case aerr != nil:
		c.log.Info("authentication failed", "event", "auth_failed", "mechanism", req.Mechanism)
		metrics.Commands.WithLabelValues(protoPOP3, "LOGIN", "failed").Inc()
		c.err("[AUTH] Authentication failed")

		return nil
	}

	user := res.Account
	if user == "" {
		user = req.Username
	}

	u, err := c.s.o.Store.User(user)
	if err != nil {
		if errors.Is(err, model.ErrImportLocked) {
			c.log.Info("login refused, account locked (import or maintenance)", "user", user)
			c.err("[SYS/TEMP] Mailbox maintenance in progress, try again later")

			return nil
		}

		c.log.Error("open user", "user", user, "err", err)
		c.err("[SYS/TEMP] Storage unavailable")

		return nil
	}

	provision.Apply(u, res, c.log)

	d, ok := c.openMaildrop(u)
	if !ok {
		u.Close()
		return nil
	}

	c.user, c.name, c.lease, c.mbox, c.msgs = u, user, d.lease, d.mbox, d.msgs
	c.log = c.log.With("user", user)
	c.log.Info("authentication succeeded", "event", "auth_success", "mechanism", req.Mechanism, "tls", c.tls)
	metrics.Commands.WithLabelValues(protoPOP3, "LOGIN", "ok").Inc()

	var size int64
	for _, m := range d.msgs {
		size += m.Size
	}

	c.ok("Maildrop locked, %d messages (%d octets)", len(d.msgs), size)

	return nil
}

// maildrop is the locked INBOX snapshot of a session.
type maildrop struct {
	mbox  *model.Mailbox
	lease model.Lease
	msgs  []model.Meta
}

// openMaildrop ensures INBOX, takes its lock and lists the messages. On
// failure the client has been answered and ok is false; the caller closes u.
func (c *conn) openMaildrop(u model.User) (d maildrop, ok bool) {
	if err := u.EnsureMailbox("INBOX", nil); err != nil {
		c.err("[SYS/TEMP] Storage error")
		return d, false
	}

	mb, err := u.Lookup("INBOX")
	if err != nil {
		c.err("[SYS/TEMP] Storage error")
		return d, false
	}

	lease, err := u.Lock(mb.ID, c.s.o.LockTTL)
	if err != nil {
		if errors.Is(err, model.ErrLocked) {
			c.err("[IN-USE] Maildrop already locked")
		} else {
			c.log.Error("lock", "err", err)
			c.err("[SYS/TEMP] Storage error")
		}

		return d, false
	}

	msgs, err := u.ListMessages(mb.ID)
	if err != nil {
		lease.Release()
		c.err("[SYS/TEMP] Storage error")

		return d, false
	}

	return maildrop{mbox: mb, lease: lease, msgs: msgs}, true
}

// message resolves a message number argument.
func (c *conn) message(arg string) (int, *model.Meta) {
	i, err := strconv.Atoi(strings.TrimSpace(arg))
	if err != nil || i < 1 || i > len(c.msgs) || c.deleted[i] {
		c.err("No such message")
		return 0, nil
	}

	return i, &c.msgs[i-1]
}

func (c *conn) listing(arg string, line func(int, *model.Meta) string) error {
	if strings.TrimSpace(arg) != "" {
		i, m := c.message(arg)
		if m == nil {
			return nil
		}

		c.ok("%s", line(i, m))

		return nil
	}

	c.ok("%d messages", len(c.msgs)-len(c.deleted))

	for i := range c.msgs {
		if !c.deleted[i+1] {
			_, _ = fmt.Fprintf(c.bw, "%s\r\n", line(i+1, &c.msgs[i]))
		}
	}

	_, _ = fmt.Fprint(c.bw, ".\r\n")

	return nil
}

// retr sends a message (lines < 0) or its header plus lines body lines.
func (c *conn) retr(arg string, lines int) error {
	i, m := c.message(arg)
	if m == nil {
		return nil
	}

	rel, err := membudget.Acquire(m.Size)
	if err != nil {
		c.err("[SYS/TEMP] Message too large")
		return nil
	}
	defer rel()

	raw, err := c.user.Read(m)
	if err != nil {
		c.log.Error("read", "guid", m.GUID, "err", err)
		c.err("[SYS/TEMP] Storage error")

		return nil
	}

	if lines >= 0 {
		raw = top(raw, lines)

		c.ok("Top of message follows")
	} else {
		c.ok("%d octets", len(raw))

		if c.s.o.MarkSeen && !model.HasFlag(m.Flags, `\Seen`) {
			if res, err := c.user.SetFlags(c.mbox.Name, []uint32{m.UID}, 0, func(old []string) []string { return append(append([]string{}, old...), `\seen`) }); err == nil && len(res.Updated) == 1 {
				c.msgs[i-1] = res.Updated[0]
			}
		}

		metrics.Commands.WithLabelValues(protoPOP3, cmdRETR, "ok").Inc()
	}

	c.writeDotStuffed(raw)

	return nil
}

// writeDotStuffed writes raw byte-stuffed with CRLF line ends and the
// terminating dot line. Write errors on the buffer surface on the next
// Flush.
func (c *conn) writeDotStuffed(raw []byte) {
	for _, l := range bytes.SplitAfter(raw, []byte("\n")) {
		if len(l) == 0 {
			continue
		}

		if l[0] == '.' {
			_ = c.bw.WriteByte('.')
		}

		_, _ = c.bw.Write(bytes.TrimRight(l, "\r\n"))
		_, _ = c.bw.WriteString("\r\n")
	}

	_, _ = fmt.Fprint(c.bw, ".\r\n")
}

// top returns the header and the first n body lines.
func top(raw []byte, n int) []byte {
	sep := bytes.Index(raw, []byte("\r\n\r\n"))
	if sep < 0 {
		if sep = bytes.Index(raw, []byte("\n\n")); sep < 0 {
			return raw
		}

		sep += 2
	} else {
		sep += 4
	}

	body := raw[sep:]

	end := 0
	for i := 0; i < n && end < len(body); i++ {
		j := bytes.IndexByte(body[end:], '\n')
		if j < 0 {
			end = len(body)
			break
		}

		end += j + 1
	}

	return raw[:sep+end]
}

// cmdQuit expunges the deleted messages and releases the lock. Nothing is
// expunged when the lock was lost meanwhile; the client is told so
// (RFC 1939: the deletions are discarded, the reply is negative).
func (c *conn) cmdQuit() error {
	if c.user != nil && len(c.deleted) > 0 && c.lease != nil && c.lease.Lost() {
		c.cleanup()
		c.err("[SYS/TEMP] Maildrop lock lost, deletions discarded")

		return nil
	}

	if c.user != nil && len(c.deleted) > 0 {
		var victims []model.Meta
		for i := range c.deleted {
			victims = append(victims, c.msgs[i-1])
		}

		if err := c.user.Expunge(c.mbox.Name, victims); err != nil {
			c.log.Error("expunge", "err", err)
			c.err("[SYS/TEMP] Some messages were not deleted")

			return nil
		}

		metrics.Commands.WithLabelValues(protoPOP3, cmdDELE, "ok").Add(float64(len(victims)))
	}

	c.cleanup()
	c.ok("Bye")

	return nil
}
