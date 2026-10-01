package imapproto

import (
	"compress/flate"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"runtime/debug"
	"shardpost/internal/logging"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/otel/attribute"

	"shardpost/internal/auth"
	"shardpost/internal/imapsieve"
	"shardpost/internal/inflate"
	"shardpost/internal/membudget"
	"shardpost/internal/metrics"
	"shardpost/internal/model"
	"shardpost/internal/prototrace"
	"shardpost/internal/provision"
	"shardpost/internal/sharenotice"
	"shardpost/internal/telemetry"
)

// Auth verifies credentials.
type Auth = auth.Authenticator

// Store opens per-user views.
type Store interface {
	User(name string) (model.User, error)
}

// SpecialFolder is a mailbox created on first login.
type SpecialFolder = provision.Folder

// DefaultSpecialFolders are provisioned for every user (Dovecot: autocreate).
var DefaultSpecialFolders = []SpecialFolder{
	{Name: "Drafts", Use: useDrafts, Subscribe: true},
	{Name: "Sent", Use: useSent, Subscribe: true},
	{Name: "Trash", Use: useTrash, Subscribe: true},
	{Name: "Junk", Use: useJunk, Subscribe: true},
	{Name: "Archive", Use: useArchive, Subscribe: true},
}

// Options configures the server.
type Options struct {
	IMAPSieve *imapsieve.Engine
	Store     Store
	// PublicPrefix is the client-visible prefix of the public namespace;
	// "" switches public folders off entirely (no namespace in the
	// NAMESPACE reply, no extra reads per LIST).
	PublicPrefix string
	// SharedPrefix is the client-visible prefix of the mailboxes other
	// accounts share ("<prefix><account>/<mailbox>"); "" switches the
	// namespace off, and no client can then reach a shared mailbox.
	SharedPrefix string
	Auth         Auth
	Log          *slog.Logger
	TLS          *tls.Config // nil: no STARTTLS
	InsecureAuth bool        // allow LOGIN/AUTHENTICATE without TLS
	Autocreate   []SpecialFolder
	MaxLiteral   int64         // APPENDLIMIT
	IdleTimeout  time.Duration // RFC 9051: at least 30 minutes
	ServerID     map[string]string
	// QuotaSelfService lets users change their own quota limits (SETQUOTA).
	QuotaSelfService bool
	// WordSearch lets the full-text index answer SUBJECT, BODY, TEXT and
	// HEADER Subject alone, by its word semantics, instead of only
	// narrowing the candidates of the substring matching RFC 9051
	// specifies (fts.imap_search: word).
	WordSearch bool
	// VerifyParallel bounds the messages one SEARCH checks against its
	// text keys at the same time (0 = 8; fts.verify_parallel).
	VerifyParallel int
	// Limits (0 = unlimited).
	MaxConnsPerIP   int
	MaxConnsPerUser int
	WriteTimeout    time.Duration // backpressure: slow readers are dropped
	// Trace bounds the per-account protocol trace (admin API).
	Trace TraceOptions
	// ShareNotices records the share notifications of the grantees whose
	// rights a SETACL or DELETEACL changes (RFC 9670 3); nil records none.
	ShareNotices *sharenotice.Recorder
}

// TraceOptions configure the protocol trace of accounts whose trace
// switch (model.Tracer) is on when they log in.
type TraceOptions struct {
	// MaxLine is the logged length of a line (0 = prototrace default).
	MaxLine int
	// Recheck is how often a traced session re-reads the switch, so that
	// switching it off takes effect in running sessions (0 = 1 minute).
	Recheck time.Duration
}

// Server accepts IMAP connections.
type Server struct {
	o      Options
	mu     sync.Mutex
	conns  map[*conn]struct{}
	lns    map[net.Listener]struct{}
	done   bool
	perIP  map[string]int
	perUsr map[string]int
}

// New creates a server.
func New(o Options) *Server {
	if o.Log == nil {
		o.Log = slog.Default()
	}

	if o.MaxLiteral == 0 {
		o.MaxLiteral = 64 << 20
	}

	if o.IdleTimeout == 0 {
		o.IdleTimeout = 30 * time.Minute
	}

	if o.ServerID == nil {
		o.ServerID = map[string]string{"name": serverName, "vendor": serverName, "support-url": "https://github.com/croessner"}
	}

	if o.WriteTimeout == 0 {
		o.WriteTimeout = 2 * time.Minute
	}

	if o.Trace.Recheck == 0 {
		o.Trace.Recheck = time.Minute
	}

	return &Server{o: o, conns: map[*conn]struct{}{}, lns: map[net.Listener]struct{}{}, perIP: map[string]int{}, perUsr: map[string]int{}}
}

// Serve accepts connections on ln until Close.
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

		c := newConn(s, nc)
		ip := ipOf(nc.RemoteAddr())

		s.mu.Lock()
		if s.o.MaxConnsPerIP > 0 && s.perIP[ip] >= s.o.MaxConnsPerIP {
			s.mu.Unlock()

			_, _ = fmt.Fprintf(nc, "* BYE [UNAVAILABLE] Too many connections from your address\r\n")
			_ = nc.Close()

			metrics.Commands.WithLabelValues(protoIMAP, "connect", "limit_ip").Inc()
			c.log.Warn("connection rejected", "event", "connection_rejected", "reason", "limit_ip")

			continue
		}

		s.perIP[ip]++
		s.conns[c] = struct{}{}
		s.mu.Unlock()
		metrics.Connections.WithLabelValues(protoIMAP).Inc()

		go s.run(c, ip)
	}
}

// run serves one accepted connection and releases its slots afterwards.
func (s *Server) run(c *conn, ip string) {
	defer func() {
		// a bug in one command must not take every session down:
		// log it with the stack and drop this connection only
		if r := recover(); r != nil {
			c.log.Error("panic in imap session", "panic", r, "stack", string(debug.Stack()))
			_ = c.nc.Close()
		}
	}()

	c.serve()
	s.mu.Lock()
	delete(s.conns, c)
	s.perIP[ip]--

	if c.name != "" {
		if c.name != "" {
			s.perUsr[c.name]--
		}
	}
	s.mu.Unlock()
	metrics.Connections.WithLabelValues(protoIMAP).Dec()
}

// ListenAndServe listens on addr.
func (s *Server) ListenAndServe(addr string) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}

	return s.Serve(ln)
}

// Close stops listening and closes all connections immediately.
func (s *Server) Close() error {
	return s.Shutdown(0)
}

// Shutdown stops listening, tells every client with BYE that the server
// is going away (IDLE sessions included), waits up to timeout for them to
// leave, then closes what is left. Clients reconnect to another pod.
func (s *Server) Shutdown(timeout time.Duration) error {
	s.mu.Lock()

	s.done = true
	for ln := range s.lns {
		_ = ln.Close()
	}

	conns := make([]*conn, 0, len(s.conns))
	for c := range s.conns {
		conns = append(conns, c)
	}
	s.mu.Unlock()

	for _, c := range conns {
		c.bye("Server shutting down, please reconnect")
	}

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
	for c := range s.conns {
		c.closing.Store(true)
		_ = c.nc.Close()
	}
	s.mu.Unlock()

	return nil
}

// knownCommands bounds the label cardinality of the command metrics: the
// commands of the dispatch tables and the UID prefix.
var knownCommands = func() map[string]bool {
	known := map[string]bool{kwUID: true}

	for _, table := range []map[string]cmdHandler{anyStateCommands, notAuthCommands, authCommands, selectedCommands} {
		for name := range table {
			known[name] = true
		}
	}

	return known
}()

func metricName(cmd string) string {
	if knownCommands[cmd] {
		return cmd
	}

	return "UNKNOWN"
}

func ipOf(a net.Addr) string {
	if t, ok := a.(*net.TCPAddr); ok {
		return t.IP.String()
	}

	return a.String()
}

func (s *Server) releaseSlot(name string) {
	s.mu.Lock()
	s.perUsr[name]--
	s.mu.Unlock()
}

// userSlot reserves a per-user connection slot; false if the limit is hit.
func (s *Server) userSlot(name string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.o.MaxConnsPerUser > 0 && s.perUsr[name] >= s.o.MaxConnsPerUser {
		return false
	}

	s.perUsr[name]++

	return true
}

type state int

const (
	stateNotAuth state = iota
	stateAuth
	stateSelected
	stateLogout
)

// conn is one client connection.
type conn struct {
	s         *Server
	nc        net.Conn
	wmu       sync.Mutex // serialises writes from the command loop and bye()
	lockProbe model.LockProbe
	// othersProbe is when the views of other namespaces (see other) are
	// next checked for a lock.
	othersProbe time.Time
	r           *reader
	w           *writer
	log         *slog.Logger
	sessionLog  *slog.Logger
	// trace wraps the connection's byte stream; a trace is attached when
	// the account's trace switch is on (traceRead: last read of it).
	// traceSeen keeps re-reading the switch of a session that was traced,
	// so an extension resumes a trace that expired in between.
	trace      *prototrace.Conn
	traceRead  time.Time
	traceUntil time.Time
	traceSeen  bool

	state   state
	tls     bool
	user    model.User
	name    string
	cmdName string
	// other users' namespaces opened for shared folders (RFC 4314)
	others map[string]model.User
	// cmdCtx is the context of the command being handled; the store views
	// use it for their spans.
	cmdCtx context.Context
	// cmdMu serialises the command loop and the NOTIFY pusher on the
	// session state and the writer; released while waiting for input.
	cmdMu  sync.Mutex
	notify *notifyState

	// ENABLE; utf8 is UTF8=ACCEPT, rev2 IMAP4rev2. Both switch the
	// session to UTF-8 mailbox names (see mboxname.go). rev2 also drops
	// \Recent, the RECENT response and STATUS item and [UNSEEN] from
	// SELECT, answers SEARCH with ESEARCH, sends a LIST with SELECT and
	// puts the UID into every FETCH the client did not ask for by UID
	// (RFC 9051 appendix E). LSUB, CHECK and the RFC822 items stay
	// available, as the deprecated parts of IMAP4rev1 a client may still
	// use.
	condstore  bool
	qresync    bool
	utf8       bool
	rev2       bool
	compressed bool
	// closing is set when the server closes the connection (bye,
	// shutdown), so that a running search stops.
	closing atomic.Bool

	// per-command memory accounting for literals
	cmdBytes int64
	// cmdLiterals counts the literals of the current command; each one
	// costs a token and a reservation even when it is empty.
	cmdLiterals int
	releases    []func()

	// selected mailbox; mu is the user object owning it (c.user for the
	// account's own mailboxes), mrights the account's rights on it
	mu      model.User
	mowner  string
	mpublic bool // the selected mailbox is a public folder
	mprefix string
	// shadowWarned keeps the warning about a mailbox below a namespace
	// prefix to one per session.
	shadowWarned bool
	// pseen are the account's private \Seen marks of the selected public
	// folder (see private.go).
	pseen      map[uint32]model.PrivateMark
	mrights    string
	mbox       *model.Mailbox
	info       *model.MailboxInfo
	recentFrom uint32       // messages with UID >= recentFrom are \Recent in this session
	msgs       []model.Meta // snapshot, index+1 == sequence number
	readOnly   bool
	examine    bool     // selected with EXAMINE
	searchRes  []uint32 // saved SEARCH result (UIDs)
	// preread is the message a FETCH read for its BINARY check, written
	// without a second read (see binaryPrecheck).
	preread *preread
}

// maxLiteralsPerCommand bounds the number of literals in one command
// (MULTIAPPEND, SEARCH with many literals). Beyond that the command is
// rejected like an oversized literal.
const maxLiteralsPerCommand = 256

func newConn(s *Server, nc net.Conn) *conn {
	c := &conn{s: s, nc: nc, log: logging.Session(s.o.Log, nc)}
	c.sessionLog = c.log
	_, c.tls = nc.(*tls.Conn)
	c.setIO(nc)

	return c
}

func (c *conn) setIO(rw io.ReadWriter) {
	c.trace = prototrace.Wrap(rw, c.trace)
	rw = c.trace
	c.r = newReader(rw, c.s.o.MaxLiteral)
	// Client input is traced as the parser consumes it.
	c.r.onLine = func(line []byte) { c.trace.Line(line) }
	c.r.onLiteral = func(n int64) { c.trace.Literal(n) }
	c.r.literalPlus = true
	// Literal bytes are accounted against the process memory budget for
	// the duration of the command (all literals of one command together).
	c.r.reserve = func(n int64) error {
		if c.cmdBytes+n > c.s.o.MaxLiteral || c.cmdLiterals >= maxLiteralsPerCommand {
			return &literalTooLarge{n}
		}

		c.cmdLiterals++

		rel, err := membudget.Acquire(n)
		if err != nil {
			return &literalTooLarge{n}
		}

		c.cmdBytes += n
		c.releases = append(c.releases, rel)

		return nil
	}
	c.r.cont = func(_ int64) error {
		c.w.line("+ Ready for literal data")
		return c.w.flush()
	}
	c.w = newWriter(rw)
	c.w.beforeFlush = func() { _ = c.nc.SetWriteDeadline(time.Now().Add(c.s.o.WriteTimeout)) }
}

func (c *conn) serve() {
	defer logging.Begin(c.log, c.tls)()
	defer func() { _ = c.nc.Close() }()
	defer func() {
		c.cmdMu.Lock()
		c.notifyStop()
		c.cmdMu.Unlock()
		c.closeOthers()

		if c.user != nil {
			c.user.Close()
		}
	}()

	c.w.line("* OK [CAPABILITY %s] ShardPost ready", strings.Join(c.caps(), " "))

	if err := c.w.flush(); err != nil {
		return
	}

	for c.state != stateLogout {
		_ = c.nc.SetReadDeadline(time.Now().Add(c.s.o.IdleTimeout))
		if err := c.r.readLine(); err != nil {
			if errors.Is(err, errLineTooLong) {
				c.cmdMu.Lock() // the notifier shares the writer
				c.w.line("* BAD Command line too long")
				ferr := c.w.flush()
				c.cmdMu.Unlock()

				if ferr != nil {
					return
				}

				continue
			}

			if !errors.Is(err, io.EOF) && !errors.Is(err, net.ErrClosed) {
				c.log.Debug("read", "err", err)
				c.cmdMu.Lock()
				c.w.line("* BYE Connection closed")
				_ = c.w.flush()
				c.cmdMu.Unlock()
			}

			return
		}

		c.cmdMu.Lock()
		c.recheckTrace()
		err := c.handleLine()
		c.cmdMu.Unlock()

		if err != nil {
			c.log.Debug("connection error", "err", err)
			return
		}
	}
}

// useRequest hands ctx to every store view of this session, including
// the views of other users' shared mailboxes.
func (c *conn) useRequest(ctx context.Context) {
	c.cmdCtx = ctx

	model.UseRequest(ctx, c.user)

	for _, u := range c.others {
		model.UseRequest(ctx, u)
	}
}

// requestContext is the context of the command being handled.
func (c *conn) requestContext() context.Context {
	if c.cmdCtx == nil {
		return context.Background()
	}

	return c.cmdCtx
}

// handleLine parses tag and command and dispatches.
func (c *conn) handleLine() error {
	if c.r.eol() {
		return nil
	}

	tag, err := c.r.atom()
	if err != nil {
		c.w.line("* BAD Missing tag")
		return c.w.flush()
	}

	cmdName, err := c.r.atom()
	if err != nil {
		c.w.line("%s BAD Missing command", tag)
		return c.w.flush()
	}

	cmdName = strings.ToUpper(cmdName)
	c.cmdName = metricName(cmdName)

	uid := false
	if cmdName == kwUID {
		uid = true

		if cmdName, err = c.r.atom(); err != nil {
			c.w.line("%s BAD Missing command", tag)
			return c.w.flush()
		}

		cmdName = strings.ToUpper(cmdName)
	}

	start := time.Now()

	_ = c.nc.SetWriteDeadline(time.Now().Add(c.s.o.WriteTimeout))
	defer c.releaseCommand()

	ctx, span := telemetry.Span(context.Background(), "imap."+c.cmdName,
		telemetry.Personal(attribute.String("imap.user", c.name), attribute.String("net.peer", c.nc.RemoteAddr().String()))...)
	if c.mbox != nil {
		span.SetAttributes(telemetry.Personal(attribute.String("imap.mailbox", c.mbox.Name))...)
	}

	// The store records its operations below this command's span.
	c.useRequest(ctx)
	defer c.useRequest(context.Background())

	err = c.dispatch(tag, cmdName, uid)
	if err != nil {
		span.RecordError(err)
	}

	span.End()
	metrics.CommandLatency.WithLabelValues(protoIMAP, c.cmdName).Observe(time.Since(start).Seconds())
	c.log.Debug("command completed", "event", "command_complete", "command", metricName(cmdName), "uid", uid, "duration_ms", time.Since(start).Milliseconds(), "handler_error", err != nil)

	return c.commandResult(tag, err)
}

// commandResult answers handler errors that end the command (oversized
// literal, syntax error) and flushes; any other error is connection-level
// and returned unflushed.
func (c *conn) commandResult(tag string, err error) error {
	var (
		lt *literalTooLarge
		mn *mailboxNameError
	)

	switch {
	case err == nil:
	case errors.As(err, &lt):
		c.drain() // further arguments and literals of the command
		c.w.line("%s NO [TOOBIG] %v", tag, err)
	case errors.As(err, &mn):
		c.drain()

		if mn.bad {
			c.bad(tag, mn.msg)
		} else {
			c.no(tag, "CANNOT", mn.msg)
		}
	case errors.Is(err, errSyntax):
		c.w.line("%s BAD %v", tag, err)
	default:
		return err // connection-level error
	}

	return c.w.flush()
}

// releaseCommand returns the literal memory of the finished command.
func (c *conn) releaseCommand() {
	for _, rel := range c.releases {
		rel()
	}

	c.releases, c.cmdBytes, c.cmdLiterals = nil, 0, 0
}

// respond writes a tagged response with an optional response code.
func (c *conn) ok(tag, code, text string) {
	metrics.Commands.WithLabelValues(protoIMAP, c.cmdName, "ok").Inc()

	if code != "" {
		c.w.line("%s OK [%s] %s", tag, code, text)
	} else {
		c.w.line("%s OK %s", tag, text)
	}
}

func (c *conn) no(tag, code, text string) {
	metrics.Commands.WithLabelValues(protoIMAP, c.cmdName, "no").Inc()

	if code != "" {
		c.w.line("%s NO [%s] %s", tag, code, text)
	} else {
		c.w.line("%s NO %s", tag, text)
	}
}

func (c *conn) bad(tag, text string) {
	metrics.Commands.WithLabelValues(protoIMAP, c.cmdName, "bad").Inc()
	c.w.line("%s BAD %s", tag, text)
}

// cmdError maps storage errors to responses.
func (c *conn) storeErr(tag string, err error) {
	switch {
	case errors.Is(err, model.ErrNoMailbox):
		c.no(tag, "NONEXISTENT", "No such mailbox")
	case errors.Is(err, model.ErrMailboxExists):
		c.no(tag, "ALREADYEXISTS", "Mailbox already exists")
	case errors.Is(err, model.ErrTooLarge):
		c.no(tag, "LIMIT", "Message too large")
	case errors.Is(err, model.ErrOverQuota):
		c.no(tag, "OVERQUOTA", "Quota exceeded")
	case errors.Is(err, model.ErrImportLocked):
		// Another namespace the command needs — the public folders, a
		// sharing account — is being imported or maintained.
		c.log.Info("command refused, a namespace it needs is locked (import or maintenance)")
		c.no(tag, "UNAVAILABLE", "Mailbox maintenance in progress, try again later")
	default:
		c.log.Error("storage error", "err", err)
		c.no(tag, "SERVERBUG", "Storage error")
	}
}

func (c *conn) canAuth() bool { return c.tls || c.s.o.InsecureAuth }

// bye sends an untagged BYE and closes the connection; safe to call from
// another goroutine while the command loop is blocked in a read (IDLE).
func (c *conn) bye(text string) {
	c.wmu.Lock()
	defer c.wmu.Unlock()

	_ = c.nc.SetWriteDeadline(time.Now().Add(5 * time.Second))
	c.closing.Store(true)
	_, _ = fmt.Fprintf(c.nc, "* BYE [UNAVAILABLE] %s\r\n", text)
	_ = c.nc.Close()
}

// caps lists the capabilities for the current state.
func (c *conn) caps() []string {
	caps := []string{"IMAP4rev2", "IMAP4rev1", "LITERAL+", "SASL-IR", kwID, kwEnable, kwUTF8Accept}
	if c.s.o.IMAPSieve != nil && c.s.o.IMAPSieve.Config.Enabled && c.s.o.IMAPSieve.Config.UserScripts {
		caps = append(caps, "IMAPSIEVE="+c.s.o.IMAPSieve.Config.ManageSieveURL)
	}

	if !c.compressed {
		caps = append(caps, "COMPRESS=DEFLATE")
	}

	if c.state == stateNotAuth {
		if !c.tls && c.s.o.TLS != nil {
			caps = append(caps, "STARTTLS")
		}

		if c.canAuth() {
			caps = append(caps, "AUTH=PLAIN", "AUTH=XOAUTH2", "AUTH=OAUTHBEARER")
		} else {
			caps = append(caps, "LOGINDISABLED")
		}

		return caps
	}

	caps = append(caps,
		"NAMESPACE", kwUnselect, "IDLE", "UIDPLUS", "MOVE", "ESEARCH", "SEARCHRES",
		"LIST-EXTENDED", "LIST-STATUS", "STATUS=SIZE", "SPECIAL-USE", "CREATE-SPECIAL-USE",
		"CHILDREN", "BINARY", "MULTIAPPEND", kwCondstore, kwQresync, "QUOTA", "QUOTA=RES-STORAGE",
		"QUOTA=RES-MESSAGE", "SORT", "SORT=DISPLAY", "THREAD=REFERENCES", "THREAD=ORDEREDSUBJECT", "OBJECTID",
		kwPreview, "SAVEDATE", "ACL", "RIGHTS=kxte", "METADATA", "CATENATE", "NOTIFY", "WITHIN", "REPLACE", "LIST-MYRIGHTS", "UNAUTHENTICATE", fmt.Sprintf("APPENDLIMIT=%d", c.s.o.MaxLiteral))

	// RFC 9208 4.1.3: a server that lets the client set limits says so.
	if c.s.o.QuotaSelfService {
		caps = append(caps, "QUOTASET")
	}

	return caps
}

// cmdHandler runs one command; name is the upper-cased command name and
// uid tells whether it was prefixed with UID.
type cmdHandler func(c *conn, tag, name string, uid bool) error

// anyStateCommands are valid in every state.
var anyStateCommands = map[string]cmdHandler{
	kwCapability: func(c *conn, tag, _ string, _ bool) error {
		c.w.untagged("CAPABILITY %s", strings.Join(c.caps(), " "))
		c.ok(tag, "", "CAPABILITY completed")

		return nil
	},
	"NOOP":  (*conn).cmdNoop,
	kwCheck: (*conn).cmdNoop,
	"LOGOUT": func(c *conn, tag, _ string, _ bool) error {
		c.w.untagged("BYE Logging out")
		c.ok(tag, "", "LOGOUT completed")
		c.state = stateLogout

		return nil
	},
	kwID:       func(c *conn, tag, _ string, _ bool) error { return c.cmdID(tag) },
	"COMPRESS": func(c *conn, tag, _ string, _ bool) error { return c.cmdCompress(tag) },
}

// notAuthCommands are valid only in the not-authenticated state.
var notAuthCommands = map[string]cmdHandler{
	"STARTTLS":     func(c *conn, tag, _ string, _ bool) error { return c.cmdStartTLS(tag) },
	kwLogin:        func(c *conn, tag, _ string, _ bool) error { return c.cmdLogin(tag) },
	kwAuthenticate: func(c *conn, tag, _ string, _ bool) error { return c.cmdAuthenticate(tag) },
}

// uidCommands may follow the UID prefix.
var uidCommands = map[string]bool{
	kwFetch: true, kwStore: true, kwSearch: true, kwCopy: true, kwMove: true,
	kwReplace: true, kwExpunge: true, kwSort: true, kwThread: true,
}

// authCommands are valid in the authenticated and selected states.
var authCommands = map[string]cmdHandler{
	kwEnable: func(c *conn, tag, _ string, _ bool) error { return c.cmdEnable(tag) },
	"NAMESPACE": func(c *conn, tag, _ string, _ bool) error {
		c.w.untagged(`NAMESPACE (("" "/")) %s %s`, c.namespaceField(c.sharedPrefix()), c.namespaceField(c.publicPrefix()))
		c.ok(tag, "", "NAMESPACE completed")

		return nil
	},
	kwList:           func(c *conn, tag, name string, _ bool) error { return c.cmdList(tag, name == kwLsub) },
	kwLsub:           func(c *conn, tag, name string, _ bool) error { return c.cmdList(tag, name == kwLsub) },
	"SETACL":         func(c *conn, tag, _ string, _ bool) error { return c.cmdSetACL(tag, false) },
	kwDeleteACL:      func(c *conn, tag, _ string, _ bool) error { return c.cmdSetACL(tag, true) },
	"GETACL":         func(c *conn, tag, _ string, _ bool) error { return c.cmdGetACL(tag) },
	"LISTRIGHTS":     func(c *conn, tag, _ string, _ bool) error { return c.cmdListRights(tag) },
	kwMyRights:       func(c *conn, tag, _ string, _ bool) error { return c.cmdMyRights(tag) },
	"NOTIFY":         func(c *conn, tag, _ string, _ bool) error { return c.cmdNotify(tag) },
	"UNAUTHENTICATE": func(c *conn, tag, _ string, _ bool) error { return c.cmdUnauthenticate(tag) },
	"GETMETADATA":    func(c *conn, tag, _ string, _ bool) error { return c.cmdGetMetadata(tag) },
	"SETMETADATA":    func(c *conn, tag, _ string, _ bool) error { return c.cmdSetMetadata(tag) },
	kwCreate:         func(c *conn, tag, _ string, _ bool) error { return c.cmdCreate(tag) },
	kwDelete:         func(c *conn, tag, _ string, _ bool) error { return c.cmdDelete(tag) },
	"RENAME":         func(c *conn, tag, _ string, _ bool) error { return c.cmdRename(tag) },
	kwSubscribe:      func(c *conn, tag, name string, _ bool) error { return c.cmdSubscribe(tag, name == kwSubscribe) },
	kwUnsubscribe:    func(c *conn, tag, name string, _ bool) error { return c.cmdSubscribe(tag, name == kwSubscribe) },
	kwStatus:         func(c *conn, tag, _ string, _ bool) error { return c.cmdStatus(tag) },
	kwAppend:         func(c *conn, tag, _ string, _ bool) error { return c.cmdAppend(tag) },
	kwSelect:         func(c *conn, tag, name string, _ bool) error { return c.cmdSelect(tag, name == kwExamine) },
	kwExamine:        func(c *conn, tag, name string, _ bool) error { return c.cmdSelect(tag, name == kwExamine) },
	kwGetQuota:       func(c *conn, tag, _ string, _ bool) error { return c.cmdGetQuota(tag) },
	"GETQUOTAROOT":   func(c *conn, tag, _ string, _ bool) error { return c.cmdGetQuotaRoot(tag) },
	"SETQUOTA":       func(c *conn, tag, _ string, _ bool) error { return c.cmdSetQuota(tag) },
	"IDLE":           func(c *conn, tag, _ string, _ bool) error { return c.cmdIdle(tag) },
}

// selectedCommands are valid only with a mailbox selected.
var selectedCommands = map[string]cmdHandler{
	kwClose:    func(c *conn, tag, name string, _ bool) error { return c.cmdClose(tag, name == kwClose) },
	kwUnselect: func(c *conn, tag, name string, _ bool) error { return c.cmdClose(tag, name == kwClose) },
	kwExpunge:  func(c *conn, tag, _ string, uid bool) error { return c.cmdExpunge(tag, uid) },
	kwFetch:    func(c *conn, tag, _ string, uid bool) error { return c.cmdFetch(tag, uid) },
	kwStore:    func(c *conn, tag, _ string, uid bool) error { return c.cmdStore(tag, uid) },
	kwSearch:   func(c *conn, tag, _ string, uid bool) error { return c.cmdSearch(tag, uid) },
	kwSort:     func(c *conn, tag, _ string, uid bool) error { return c.cmdSort(tag, uid) },
	kwThread:   func(c *conn, tag, _ string, uid bool) error { return c.cmdThread(tag, uid) },
	kwCopy:     func(c *conn, tag, _ string, uid bool) error { return c.cmdCopy(tag, uid) },
	kwMove:     func(c *conn, tag, _ string, uid bool) error { return c.cmdMove(tag, uid) },
	kwReplace:  func(c *conn, tag, _ string, uid bool) error { return c.cmdReplace(tag, uid) },
}

// cmdNoop implements NOOP and CHECK: report pending changes.
func (c *conn) cmdNoop(tag, name string, _ bool) error {
	if err := c.poll(true); err != nil {
		return err
	}

	c.ok(tag, "", name+" completed")

	return nil
}

// dispatch routes a command to its handler and enforces the state machine.
func (c *conn) dispatch(tag, name string, uid bool) error {
	// Any state.
	if h, ok := anyStateCommands[name]; ok {
		return h(c, tag, name, uid)
	}

	if c.state == stateNotAuth {
		if h, ok := notAuthCommands[name]; ok {
			return h(c, tag, name, uid)
		}

		c.no(tag, "", "Please authenticate first")

		return nil
	}

	if c.lockProbe.Locked(c.user) || c.lockedOther() {
		c.log.Info("session closed, account locked (import or maintenance)")
		c.w.untagged("BYE [UNAVAILABLE] Mailbox maintenance in progress, try again later")
		c.state = stateLogout

		return nil
	}

	if uid && !uidCommands[name] {
		c.bad(tag, "UID "+name+" is not a valid command")
		return nil
	}
	// Authenticated state.
	if h, ok := authCommands[name]; ok {
		return h(c, tag, name, uid)
	}

	if c.state != stateSelected {
		c.bad(tag, "No mailbox selected")
		return nil
	}

	return c.dispatchSelected(tag, name, uid)
}

// dispatchSelected routes a command of the selected state, announcing
// pending changes first (except for CLOSE and UNSELECT).
func (c *conn) dispatchSelected(tag, name string, uid bool) error {
	// Selected state. RFC 9051 7.5.1: no EXPUNGE responses while a
	// non-UID FETCH, STORE or SEARCH is in progress.
	allowExpunge := uid || (name != kwFetch && name != kwStore && name != kwSearch)
	if name != kwClose && name != kwUnselect {
		if err := c.poll(allowExpunge); err != nil {
			return err
		}
	}

	if h, ok := selectedCommands[name]; ok {
		return h(c, tag, name, uid)
	}

	c.bad(tag, "Unknown command "+name)

	return nil
}

// --- any state -------------------------------------------------------------

func (c *conn) cmdID(tag string) error {
	t, err := c.r.next()
	if err != nil {
		return err
	}

	_ = t // client id is ignored

	var parts []string
	for k, v := range c.s.o.ServerID {
		parts = append(parts, quote(k), quote(v))
	}

	c.w.untagged("ID (%s)", strings.Join(parts, " "))
	c.ok(tag, "", "ID completed")

	return nil
}

// --- not authenticated -----------------------------------------------------

func (c *conn) cmdStartTLS(tag string) error {
	if c.tls || c.s.o.TLS == nil {
		c.bad(tag, "STARTTLS not available")
		return nil
	}

	c.ok(tag, "", "Begin TLS negotiation now")

	if err := c.w.flush(); err != nil {
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

func (c *conn) cmdLogin(tag string) error {
	user, err := c.r.astring()
	if err != nil {
		return err
	}

	pass, err := c.r.astring()
	if err != nil {
		return err
	}

	if !c.canAuth() {
		c.no(tag, "PRIVACYREQUIRED", "LOGIN requires TLS")
		return nil
	}

	req := auth.Request{Username: user, Password: pass, Mechanism: kwLogin, Protocol: protoIMAP, Client: c.nc.RemoteAddr(), Local: c.nc.LocalAddr(), TLS: c.tls}
	res, err := c.authenticate(req)

	return c.finishLogin(tag, req, res, err)
}

// cmdAuthenticate implements AUTHENTICATE for PLAIN, XOAUTH2 and
// OAUTHBEARER, with SASL-IR.
func (c *conn) cmdAuthenticate(tag string) error {
	mech, err := c.r.atom()
	if err != nil {
		return err
	}

	mech = strings.ToUpper(mech)
	switch mech {
	case kwPlain, kwXOAuth2, kwOAuthBearer:
	default:
		c.no(tag, "CANNOT", "Unsupported authentication mechanism")
		return nil
	}

	if !c.canAuth() {
		c.no(tag, "PRIVACYREQUIRED", "AUTHENTICATE requires TLS")
		return nil
	}

	ir, cancelled, err := c.initialResponse()
	if err != nil {
		return err
	}

	if cancelled {
		c.bad(tag, "Authentication cancelled")
		return nil
	}

	req := auth.Request{Mechanism: mech, Protocol: protoIMAP, Client: c.nc.RemoteAddr(), Local: c.nc.LocalAddr(), TLS: c.tls}
	if err := parseSASL(&req, ir); err != nil {
		c.bad(tag, err.Error())
		return nil
	}

	res, aerr := c.authenticate(req)
	if aerr != nil && mech != kwPlain {
		// RFC 7628: the failure goes out as a continuation, the client
		// answers with a single ^A, then the tagged NO follows.
		if _, err := c.continuation(auth.OAuthBearerFailure(401, "")); err != nil {
			return err
		}
	}

	return c.finishLogin(tag, req, res, aerr)
}

// initialResponse returns the SASL initial response: the SASL-IR argument
// or, when absent or empty ("="), the client's answer to an empty
// continuation; cancelled is true when that answer is "*".
func (c *conn) initialResponse() (ir string, cancelled bool, err error) {
	if !c.r.eol() {
		ir, _ = c.r.atom()
	}

	if ir != "" && ir != "=" {
		return ir, false, nil
	}

	if ir, err = c.continuation(""); err != nil {
		return "", false, err
	}

	return ir, ir == "*", nil
}

// parseSASL decodes the client response ir of req.Mechanism into the
// credentials of req.
func parseSASL(req *auth.Request, ir string) error {
	var err error

	switch req.Mechanism {
	case kwPlain:
		req.Username, req.Password, err = auth.ParsePlain(ir)
	case kwXOAuth2:
		req.Username, req.Token, err = auth.ParseXOAUTH2(ir)
	case kwOAuthBearer:
		req.Username, req.Token, err = auth.ParseOAuthBearer(ir)
	}

	return err
}

// continuation sends "+ <text>" and reads the client's reply line.
func (c *conn) continuation(text string) (string, error) {
	c.w.line("+ %s", text)

	if err := c.w.flush(); err != nil {
		return "", err
	}

	if err := c.r.readLine(); err != nil {
		return "", err
	}

	return c.r.rest(), nil
}

func (c *conn) authenticate(req auth.Request) (*auth.Result, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	return c.s.o.Auth.Authenticate(ctx, req)
}

// finishLogin maps the authentication outcome to the tagged response and
// opens the store on success.
func (c *conn) finishLogin(tag string, req auth.Request, res *auth.Result, err error) error {
	switch {
	case errors.Is(err, auth.ErrTemporary):
		metrics.Commands.WithLabelValues(protoIMAP, kwLogin, "tempfail").Inc()
		c.log.Warn("authentication temporarily unavailable", "user", req.Username, "err", err)
		c.no(tag, "UNAVAILABLE", "Authentication temporarily unavailable")

		return nil
	case err != nil:
		metrics.Commands.WithLabelValues(protoIMAP, kwLogin, "failed").Inc()
		c.log.Info("authentication failed", "event", "auth_failed", "mechanism", req.Mechanism)
		c.no(tag, "AUTHENTICATIONFAILED", "Authentication failed")

		return nil
	}

	account := res.Account
	if account == "" {
		account = req.Username
	}

	if !c.s.userSlot(account) {
		c.log.Warn("session limit reached", "event", "connection_rejected", "reason", "limit_user", "user", account)
		c.no(tag, "LIMIT", "Too many connections for this user")

		return nil
	}

	u, err := c.s.o.Store.User(account)
	if err != nil {
		c.s.releaseSlot(account)

		if errors.Is(err, model.ErrImportLocked) {
			c.log.Info("login refused, account locked (import or maintenance)", "user", account)
			c.no(tag, "UNAVAILABLE", "Mailbox maintenance in progress, try again later")

			return nil
		}

		c.log.Error("open user", "user", account, "err", err)
		c.no(tag, "UNAVAILABLE", "Storage unavailable")

		return nil
	}

	c.user, c.name, c.state = u, account, stateAuth
	c.log = c.log.With("user", account)
	provision.Apply(u, res, c.log)

	if err := provision.Mailboxes(u, c.s.o.Autocreate); err != nil {
		c.log.Error("provision user", "err", err)
	}

	c.log.Info("authentication succeeded", "event", "auth_success", "mechanism", req.Mechanism, "tls", c.tls)
	c.readTrace()
	c.ok(tag, "CAPABILITY "+strings.Join(c.caps(), " "), "Logged in")

	return nil
}

// readTrace attaches or detaches the protocol trace according to the
// account's trace switch.
func (c *conn) readTrace() {
	t, ok := c.user.(model.Tracer)
	if !ok {
		return
	}

	c.traceRead = time.Now()

	st, err := t.Trace()
	switch {
	case err != nil:
		c.log.Warn("trace switch not readable", "err", err)
	case st.Active(c.traceRead):
		c.traceSeen, c.traceUntil = true, st.Until
		c.trace.Start(c.log, st.Until, c.s.o.Trace.MaxLine, traceFilter)
	default:
		c.traceSeen = false
		c.trace.Stop("switched off")
	}
}

// recheckTrace re-reads the switch of a traced session periodically,
// and at once when its trace expired (it may have been extended). A
// trace halted at an authentication command stays off until the next
// login.
func (c *conn) recheckTrace() {
	if c.user == nil || !c.traceSeen || c.trace.Halted() {
		return
	}

	// Once at expiry (not read since), otherwise every Recheck: an
	// unreadable switch does not cost a read per command.
	expired := !c.trace.Active() && c.traceRead.Before(c.traceUntil)
	if expired || time.Since(c.traceRead) >= c.s.o.Trace.Recheck {
		c.readTrace()
	}
}

// traceMaskedAuth replaces a client line that starts authentication.
const traceMaskedAuth = "<authentication command, trace stopped>"

// traceFilter keeps credentials and message text out of the protocol
// trace: an authentication command (with or without tag) halts the trace
// unlogged until the next login, so neither its literals nor pipelined
// SASL data appear. PREVIEW texts, which quote the message body, are
// masked.
func traceFilter(dir, line string) (string, bool) {
	if dir == prototrace.DirClient {
		f := strings.Fields(line)
		for i := 0; i < len(f) && i < 2; i++ {
			if strings.EqualFold(f[i], kwLogin) || strings.EqualFold(f[i], "AUTHENTICATE") {
				return traceMaskedAuth, true
			}
		}

		return line, false
	}

	return maskPreview(line), false
}

// maskPreview replaces the quoted text of every PREVIEW item (a
// PREVIEW literal is skipped like every literal).
func maskPreview(line string) string {
	const item = `PREVIEW "`

	var b strings.Builder

	for {
		i := strings.Index(line, item)
		if i < 0 {
			b.WriteString(line)
			return b.String()
		}

		b.WriteString(line[:i])
		b.WriteString(`PREVIEW "<masked>"`)

		rest := line[i+len(item):]

		end := closingQuote(rest)
		if end < 0 { // cut line
			return b.String()
		}

		line = rest[end+1:]
	}
}

// closingQuote is the index of the quote ending a quoted string whose
// opening quote precedes s, or -1.
func closingQuote(s string) int {
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '\\':
			i++
		case '"':
			return i
		}
	}

	return -1
}

// --- authenticated: ENABLE, IDLE -------------------------------------------

func (c *conn) cmdEnable(tag string) error {
	var enabled []string

	for !c.r.eol() {
		capName, err := c.r.atom()
		if err != nil {
			return err
		}

		switch strings.ToUpper(capName) {
		case kwCondstore:
			if !c.condstore {
				c.condstore = true

				enabled = append(enabled, kwCondstore)
			}
		case kwQresync:
			if !c.qresync {
				c.qresync, c.condstore = true, true

				enabled = append(enabled, kwQresync)
			}
		case kwUTF8Accept:
			if !c.utf8 {
				c.utf8 = true

				enabled = append(enabled, kwUTF8Accept)
			}
		case "IMAP4REV2":
			if !c.rev2 {
				c.rev2 = true

				enabled = append(enabled, "IMAP4rev2")
			}
		}
	}

	c.w.untagged("ENABLED %s", strings.Join(enabled, " "))
	c.ok(tag, "", "ENABLE completed")

	return nil
}

// idleStart arms the watch, tells the client that the session idles and
// reports what happened while the watch was being armed.
func (c *conn) idleStart() (model.Watch, error) {
	watch := c.idleWatch()

	err := c.idleAnnounce()
	if err != nil && watch != nil {
		watch.Close()

		return nil, err
	}

	return watch, err
}

// idleAnnounce tells the client that the session idles and reports what
// happened since the command loop last looked.
func (c *conn) idleAnnounce() error {
	c.w.line("+ idling")

	if err := c.w.flush(); err != nil {
		return err
	}

	if err := c.poll(true); err != nil {
		return err
	}

	return c.w.flush()
}

// cmdIdle pushes changes until the client sends DONE. Changes are
// detected through the RADOS watch on the mailbox object; a slow ticker
// covers lost notifies.
func (c *conn) cmdIdle(tag string) error {
	// The watch is armed before the client is told to idle, and the
	// mailbox is polled once afterwards: a change between the poll of
	// the command loop and the watch would otherwise wake nobody, and
	// the session would sit on it until the ticker.
	watch, err := c.idleStart()
	if err != nil {
		return err
	}

	done := c.idleRead()

	defer func() {
		if watch != nil {
			watch.Close()
		}
	}()

	ticker := time.NewTicker(60 * time.Second)
	defer ticker.Stop()

	// While idling the NOTIFY pusher may use the connection; the lock is
	// taken again before anything is written or the reader state touched.
	c.cmdMu.Unlock()

	locked := false

	relock := func() {
		if !locked {
			c.cmdMu.Lock()
			locked = true
		}
	}
	defer relock()

	for {
		var (
			events <-chan struct{}
			errs   <-chan error
		)
		if watch != nil {
			events, errs = watch.Events(), watch.Errors()
		}

		select {
		case err := <-done:
			relock()

			return c.idleDone(tag, err)
		case <-ticker.C:
			if err := c.idlePush(); err != nil {
				return err
			}
		case <-events:
			if err := c.idlePush(); err != nil {
				return err
			}
		case err := <-errs:
			watch = c.rearmWatch(watch, err)
		}
	}
}

// idleRead reads the client's next line (DONE) in the background, without
// read deadline; the result arrives on the returned channel.
func (c *conn) idleRead() <-chan error {
	done := make(chan error, 1)

	go func() {
		_ = c.nc.SetReadDeadline(time.Time{})

		err := c.r.readLine()
		done <- err
	}()

	return done
}

// idleWatch arms the change watch on the selected mailbox; nil means
// polling only.
func (c *conn) idleWatch() model.Watch {
	if c.mbox == nil {
		return nil
	}

	watch, err := c.mu.Watch(c.mbox.ID)
	if err != nil {
		c.log.Warn("watch failed, polling only", "err", err)
	}

	return watch
}

// rearmWatch replaces a watch that reported werr; nil if re-arming fails.
func (c *conn) rearmWatch(watch model.Watch, werr error) model.Watch {
	c.log.Warn("watch error, re-arming", "err", werr)
	watch.Close()

	watch, err := c.mu.Watch(c.mbox.ID)
	if err != nil {
		return nil
	}

	return watch
}

// idlePush sends pending changes during IDLE under the command lock.
func (c *conn) idlePush() error {
	c.cmdMu.Lock()
	defer c.cmdMu.Unlock()

	if err := c.poll(true); err != nil {
		return err
	}

	return c.w.flush()
}

// idleDone ends IDLE once the client's line (or read error err) arrived.
func (c *conn) idleDone(tag string, err error) error {
	if err != nil {
		return err
	}

	if strings.EqualFold(strings.TrimSpace(string(c.r.line)), "DONE") {
		c.ok(tag, "", "IDLE completed")
	} else {
		c.bad(tag, "Expected DONE")
	}

	c.r.pos = len(c.r.line)

	return nil
}

// --- COMPRESS=DEFLATE (RFC 4978) --------------------------------------------

// compressedConn wraps the raw connection with DEFLATE in both directions.
type compressedConn struct {
	r io.Reader
	w *flate.Writer
}

func (cc *compressedConn) Read(p []byte) (int, error) { return cc.r.Read(p) }
func (cc *compressedConn) Write(p []byte) (int, error) {
	n, err := cc.w.Write(p)
	if err != nil {
		return n, err
	}
	// The protocol is request/response: every flush of our buffered writer
	// must reach the wire, so flush the deflate stream as well.
	return n, cc.w.Flush()
}

func (c *conn) cmdCompress(tag string) error {
	mech, err := c.r.atom()
	if err != nil {
		return err
	}

	if !strings.EqualFold(mech, "DEFLATE") {
		c.no(tag, "", "Unsupported compression mechanism")
		return nil
	}

	if c.compressed {
		c.no(tag, "COMPRESSIONACTIVE", "Compression already active")
		return nil
	}

	c.ok(tag, "", "DEFLATE active")

	if err := c.w.flush(); err != nil {
		return err
	}

	fw, err := flate.NewWriter(c.nc, flate.DefaultCompression)
	if err != nil {
		return err
	}

	c.compressed = true
	// inflate, not compress/flate: it hands out each block at once, so
	// commands of clients that flush with Z_PARTIAL_FLUSH (iOS Mail) are
	// read without waiting for more input.
	c.setIO(&compressedConn{r: inflate.NewReader(c.nc), w: fw})

	return nil
}

// UNAUTHENTICATE (RFC 8437): back to the not-authenticated state with all
// user state discarded (TLS and COMPRESS stay).
func (c *conn) cmdUnauthenticate(tag string) error {
	// The next login must not be traced (credentials); it reads the
	// switch of its own account.
	c.traceSeen = false
	c.trace.Stop("unauthenticated")

	if c.mbox != nil {
		c.unselect()
	}

	c.notifyStop()
	c.closeOthers()

	if c.user != nil {
		c.user.Close()
		c.s.releaseSlot(c.name)
	}

	c.user, c.name = nil, ""
	c.condstore, c.qresync, c.utf8, c.rev2 = false, false, false, false
	c.state = stateNotAuth
	c.log.Info("authentication cleared", "event", "auth_cleared")
	c.log = c.sessionLog
	c.ok(tag, "", "UNAUTHENTICATE completed")

	return nil
}

// enableCondstore turns CONDSTORE on for the session (RFC 7162 3.1.3:
// any CONDSTORE-enabling command does), announcing HIGHESTMODSEQ once.
func (c *conn) enableCondstore() {
	if c.condstore {
		return
	}

	c.condstore = true
	if c.info != nil {
		c.w.untagged("OK [HIGHESTMODSEQ %d] Highest", c.info.HighestModSeq)
	}
}

// isRecent implements the session-scoped \Recent flag: messages that
// arrived at or after the recent watermark this session read at SELECT.
func (c *conn) isRecent(m *model.Meta) bool {
	return c.recentFrom != 0 && m.UID >= c.recentFrom
}

// announceRecent writes the untagged RECENT response that follows
// EXISTS, except for a session that enabled IMAP4rev2, which has no
// RECENT response (RFC 9051 appendix E 12).
func (c *conn) announceRecent() {
	if !c.rev2 {
		c.w.untagged("%d RECENT", c.recentCount())
	}
}

// recentCount counts the \Recent messages of the snapshot.
func (c *conn) recentCount() int {
	n := 0

	for i := range c.msgs {
		if c.isRecent(&c.msgs[i]) {
			n++
		}
	}

	return n
}

// flagsOut renders the flags of a message including \Recent, which a
// session that enabled IMAP4rev2 never sees (RFC 9051 appendix E 12).
func (c *conn) flagsOut(m *model.Meta) string {
	if c.rev2 || !c.isRecent(m) {
		return flagList(m.Flags)
	}

	return flagList(append(append([]string{}, m.Flags...), `\Recent`))
}

// drain consumes the rest of the command line, including literals, so
// that an early error reply does not leave literal data to be read as
// the next command.
func (c *conn) drain() {
	c.r.refuseSync = true
	defer func() { c.r.refuseSync = false }()

	for !c.r.eol() {
		if _, err := c.r.next(); err != nil {
			c.r.pos = len(c.r.line)
			return
		}
	}
}
