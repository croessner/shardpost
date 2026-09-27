package sieve

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"runtime/debug"
	"shardpost/internal/logging"
	"strconv"
	"strings"
	"sync"
	"time"

	"shardpost/internal/auth"
	"shardpost/internal/model"
	"shardpost/internal/provision"
)

// Auth verifies credentials.
type Auth = auth.Authenticator

// Store opens per-user views.
type Store interface {
	User(name string) (model.User, error)
}

// Options configures the ManageSieve server.
type Options struct {
	Store        Store
	Auth         Auth
	Log          *slog.Logger
	TLS          *tls.Config
	InsecureAuth bool
	MaxScript    int64
	// GlobalScripts is a directory of <name>.sieve files for
	// "include :global" ("" = none).
	GlobalScripts string
	// Config are the Sieve settings (see DefaultConfig).
	Config map[string]string
}

// Server is a ManageSieve (RFC 5804) server.
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

	if o.MaxScript == 0 {
		o.MaxScript = 256 << 10
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

		go func() {
			defer func() {
				if r := recover(); r != nil {
					s.o.Log.Error("panic in managesieve session", "panic", r, "stack", string(debug.Stack()))

					_ = nc.Close()

					s.mu.Lock()
					delete(s.conns, nc)
					s.mu.Unlock()
				}
			}()

			newConn(s, nc).serve()
			s.mu.Lock()
			delete(s.conns, nc)
			s.mu.Unlock()
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

// Close stops the server.
func (s *Server) Close() error {
	s.mu.Lock()

	s.done = true
	for ln := range s.lns {
		_ = ln.Close()
	}

	for nc := range s.conns {
		_ = nc.Close()
	}
	s.mu.Unlock()

	return nil
}

type conn struct {
	log  *slog.Logger
	s    *Server
	nc   net.Conn
	br   *bufio.Reader
	bw   *bufio.Writer
	tls  bool
	user model.User
	name string
}

func newConn(s *Server, nc net.Conn) *conn {
	c := &conn{s: s, nc: nc, log: logging.Session(s.o.Log, nc)}
	_, c.tls = nc.(*tls.Conn)
	c.setIO(nc)

	return c
}

const maxLineLen = 8192

var errLineTooLong = errors.New("line too long")

func (c *conn) setIO(rw io.ReadWriter) {
	c.br = bufio.NewReaderSize(rw, maxLineLen+2)
	c.bw = bufio.NewWriter(rw)
}

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

// validName bounds script names (RFC 5804: no control characters).
func validName(n string) bool {
	if n == "" || len(n) > 128 {
		return false
	}

	for _, r := range n {
		if r < 32 || r == 127 {
			return false
		}
	}

	return true
}

func (c *conn) canAuth() bool { return c.tls || c.s.o.InsecureAuth }

// printf writes to the session. bufio.Writer errors are sticky and are
// reported by the next Flush, which the session loop checks.
func (c *conn) printf(format string, a ...any) { _, _ = fmt.Fprintf(c.bw, format, a...) }

func (c *conn) capabilities() {
	c.printf("\"IMPLEMENTATION\" \"ShardPost\"\r\n")

	extensions := Extensions
	if c.s.o.Config["extprograms_enabled"] == settingYes {
		extensions += " vnd.dovecot.execute vnd.dovecot.pipe vnd.dovecot.filter"
	}

	if c.s.o.Config["imapsieve_enabled"] == settingYes {
		extensions += " imapsieve"
	}

	c.printf("\"SIEVE\" \"%s\"\r\n", extensions)

	if c.canAuth() {
		c.printf("\"SASL\" \"PLAIN\"\r\n")
	} else {
		c.printf("\"SASL\" \"\"\r\n")
	}

	if !c.tls && c.s.o.TLS != nil {
		c.printf("\"STARTTLS\"\r\n")
	}

	c.printf("\"MAXREDIRECTS\" \"5\"\r\n")
	c.printf("\"VERSION\" \"1.0\"\r\n")
}

func (c *conn) serve() {
	defer logging.Begin(c.log, c.tls)()
	// Close the connection accepted by Serve (not a later TLS wrapper).
	nc := c.nc
	defer func() { _ = nc.Close() }()
	defer func() {
		if c.user != nil {
			c.user.Close()
		}
	}()

	c.capabilities()
	c.printf("OK \"ShardPost ManageSieve ready\"\r\n")

	if c.bw.Flush() != nil {
		return
	}

	for {
		_ = c.nc.SetReadDeadline(time.Now().Add(10 * time.Minute))
		_ = c.nc.SetWriteDeadline(time.Now().Add(2 * time.Minute))

		line, err := c.readLine()
		if errors.Is(err, errLineTooLong) {
			c.no("Line too long")

			if c.bw.Flush() != nil {
				return
			}

			continue
		}

		if err != nil {
			return
		}

		quit, err := c.handle(line)
		if err != nil {
			return
		}

		if c.bw.Flush() != nil || quit {
			return
		}
	}
}

// args parses quoted strings and {n+} literals of a command line.
// maxArgs bounds the number of arguments (each may be a literal up to
// MaxScript) so one command cannot accumulate unbounded memory.
const maxArgs = 8

func (c *conn) args(line string) ([]string, error) {
	var out []string
	for {
		if len(out) >= maxArgs {
			return nil, errors.New("too many arguments")
		}

		line = strings.TrimLeft(line, " ")
		if line == "" {
			return out, nil
		}

		var (
			arg string
			err error
		)

		switch line[0] {
		case '"':
			arg, line, err = parseQuoted(line)
		case '{':
			arg, line, err = c.readLiteral(line)
		default:
			sp := strings.IndexByte(line, ' ')
			if sp < 0 {
				out = append(out, line)
				return out, nil
			}

			arg, line = line[:sp], line[sp:]
		}

		if err != nil {
			return nil, err
		}

		out = append(out, arg)
	}
}

// parseQuoted reads a quoted string with backslash escapes (RFC 5804
// 1.2) at the start of line and returns it with the rest of the line.
func parseQuoted(line string) (string, string, error) {
	var b strings.Builder

	i := 1
	for ; i < len(line); i++ {
		ch := line[i]
		if ch == '\\' && i+1 < len(line) {
			i++
			b.WriteByte(line[i])

			continue
		}

		if ch == '"' {
			break
		}

		b.WriteByte(ch)
	}

	if i >= len(line) {
		return "", "", errors.New("unterminated string")
	}

	return b.String(), line[i+1:], nil
}

// readLiteral reads a {n+} literal (at most MaxScript bytes) announced at
// the start of line and returns it with the next command line.
func (c *conn) readLiteral(line string) (string, string, error) {
	end := strings.IndexByte(line, '}')
	if end < 0 {
		return "", "", errors.New("bad literal")
	}

	n, err := strconv.ParseInt(strings.TrimSuffix(line[1:end], "+"), 10, 64)
	if err != nil || n < 0 || n > c.s.o.MaxScript {
		return "", "", errors.New("bad literal size")
	}

	buf := make([]byte, n)
	if _, err := io.ReadFull(c.br, buf); err != nil {
		return "", "", err
	}

	rest, err := c.readLine()
	if err != nil {
		return "", "", err
	}

	return string(buf), rest, nil
}

func quote(s string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"`
}

func (c *conn) ok(text string) { c.printf("OK %s\r\n", quote(text)) }
func (c *conn) no(text string) { c.printf("NO %s\r\n", quote(text)) }

// preAuthCommands are available before authentication. A handler
// returns true to end the session; an error drops the connection.
var preAuthCommands = map[string]func(*conn, []string) (bool, error){
	"CAPABILITY":   (*conn).cmdCapability,
	"NOOP":         (*conn).cmdNoop,
	"LOGOUT":       (*conn).cmdLogout,
	"STARTTLS":     (*conn).cmdStartTLS,
	"AUTHENTICATE": (*conn).cmdAuthenticate,
}

// userCommands need an authenticated user.
var userCommands = map[string]func(*conn, []string){
	"LISTSCRIPTS":  (*conn).cmdListScripts,
	"HAVESPACE":    (*conn).cmdHaveSpace,
	"PUTSCRIPT":    (*conn).cmdPutScript,
	"GETSCRIPT":    (*conn).cmdGetScript,
	"SETACTIVE":    (*conn).cmdSetActive,
	"DELETESCRIPT": (*conn).cmdDeleteScript,
	"RENAMESCRIPT": (*conn).cmdRenameScript,
	"CHECKSCRIPT":  (*conn).cmdCheckScript,
}

// knownCommand reports whether cmd is a ManageSieve command of this
// server (only those are logged by name).
func knownCommand(cmd string) bool {
	_, pre := preAuthCommands[cmd]
	_, user := userCommands[cmd]

	return pre || user
}

func (c *conn) handle(line string) (bool, error) {
	if c.log == nil {
		c.log = slog.Default()
	}

	cmd, rest, _ := strings.Cut(line, " ")
	cmd = strings.ToUpper(cmd)
	start := time.Now()

	safeCommand := "UNKNOWN"
	if knownCommand(cmd) {
		safeCommand = cmd
	}

	defer func() {
		c.log.Debug("command completed", "event", "command_complete", "command", safeCommand, "duration_ms", time.Since(start).Milliseconds())
	}()

	args, err := c.args(rest)
	if err != nil {
		c.no("Syntax error: " + err.Error())
		return false, nil
	}

	if h, ok := preAuthCommands[cmd]; ok {
		return h(c, args)
	}

	if c.user == nil {
		c.no("Please authenticate first")
		return false, nil
	}

	if h, ok := userCommands[cmd]; ok {
		h(c, args)
	} else {
		c.no("Unknown command")
	}

	return false, nil
}

func (c *conn) cmdCapability(_ []string) (bool, error) {
	c.capabilities()
	c.ok("Done")

	return false, nil
}

func (c *conn) cmdNoop(_ []string) (bool, error) {
	c.ok("Done")
	return false, nil
}

func (c *conn) cmdLogout(_ []string) (bool, error) {
	c.ok("Bye")
	return true, nil
}

func (c *conn) cmdStartTLS(_ []string) (bool, error) {
	if c.tls || c.s.o.TLS == nil {
		c.no("STARTTLS not available")
		return false, nil
	}

	c.ok("Begin TLS negotiation now")

	if err := c.bw.Flush(); err != nil {
		return true, err
	}

	tc := tls.Server(c.nc, c.s.o.TLS)
	if err := tc.Handshake(); err != nil {
		c.log.Warn("TLS handshake failed", "event", "tls_failed")
		return true, err
	}

	c.nc, c.tls = tc, true
	c.log.Info("TLS established", "event", "tls_established", "tls_version", tc.ConnectionState().Version)
	c.setIO(tc)
	c.capabilities()
	c.ok("TLS active")

	return false, nil
}

func (c *conn) cmdAuthenticate(args []string) (bool, error) {
	if c.user != nil {
		c.no("Already authenticated")
		return false, nil
	}

	return false, c.authenticate(args)
}

func (c *conn) cmdListScripts(_ []string) {
	names, active, err := c.user.ListScripts()
	if err != nil {
		c.no("Storage error")
		return
	}

	for _, n := range names {
		if n == active {
			c.printf("%s ACTIVE\r\n", quote(n))
		} else {
			c.printf("%s\r\n", quote(n))
		}
	}

	c.ok("Listscripts completed")
}

func (c *conn) cmdHaveSpace(args []string) {
	if len(args) < 2 {
		c.no("HAVESPACE name size")
		return
	}

	n, _ := strconv.ParseInt(args[1], 10, 64)
	if n > c.s.o.MaxScript {
		c.printf("NO (QUOTA/MAXSIZE) \"Script too large\"\r\n")
	} else {
		c.ok("Putscript would succeed")
	}
}

func (c *conn) cmdPutScript(args []string) {
	if len(args) < 2 || !validName(args[0]) {
		c.no("PUTSCRIPT name script")
		return
	}

	if c.reserved(args[0]) {
		return
	}

	if err := CompileWith(args[1], c.hooks()); err != nil {
		c.no("Script error: " + err.Error())
		return
	}

	if err := c.user.PutScript(args[0], args[1]); err != nil {
		c.no("Storage error")
		return
	}

	c.ok("Putscript completed")
}

func (c *conn) cmdGetScript(args []string) {
	if len(args) < 1 {
		c.no("GETSCRIPT name")
		return
	}

	text, err := c.user.GetScript(args[0])
	if err != nil {
		c.printf(respNoSuchScript)
		return
	}

	c.printf("{%d}\r\n%s\r\n", len(text), text)
	c.ok("Getscript completed")
}

// respNoSuchScript is the NONEXISTENT response for an unknown script.
const respNoSuchScript = "NO (NONEXISTENT) \"No such script\"\r\n"

func (c *conn) cmdSetActive(args []string) {
	if len(args) < 1 {
		c.no("SETACTIVE name")
		return
	}

	if c.reserved(args[0]) {
		return
	}

	if args[0] != "" && !c.validForDelivery(args[0]) {
		return
	}

	if err := c.user.SetActiveScript(args[0]); err != nil {
		c.printf(respNoSuchScript)
		return
	}

	c.ok("Setactive completed")
}

// validForDelivery compiles script name as delivery would (without the
// IMAPSieve context) and answers NO when it cannot be activated.
func (c *conn) validForDelivery(name string) bool {
	script, err := c.user.GetScript(name)
	if err != nil {
		c.no("No such script")
		return false
	}

	hooks := c.hooks()

	hooks.Config = make(map[string]string, len(c.s.o.Config))
	for k, v := range c.s.o.Config {
		hooks.Config[k] = v
	}

	delete(hooks.Config, "imapsieve_enabled")

	if err := CompileWith(script, hooks); err != nil {
		c.no("Script is not valid for delivery")
		return false
	}

	return true
}

func (c *conn) cmdDeleteScript(args []string) {
	if len(args) < 1 {
		c.no("DELETESCRIPT name")
		return
	}

	if c.reserved(args[0]) {
		return
	}

	switch err := c.user.DeleteScript(args[0]); {
	case errors.Is(err, model.ErrScriptActive):
		c.printf("NO (ACTIVE) \"Script is active\"\r\n")
	case err != nil:
		c.no("Storage error")
	default:
		c.ok("Deletescript completed")
	}
}

func (c *conn) cmdRenameScript(args []string) {
	if len(args) < 2 || !validName(args[1]) {
		c.no("RENAMESCRIPT old new")
		return
	}

	if c.reserved(args[0]) || c.reserved(args[1]) {
		return
	}

	switch err := c.user.RenameScript(args[0], args[1]); {
	case errors.Is(err, model.ErrScriptExists):
		c.printf("NO (ALREADYEXISTS) \"Script exists\"\r\n")
		return
	case err != nil:
		c.printf(respNoSuchScript)
		return
	}

	c.ok("Renamescript completed")
}

// reserved answers NO for a script the server keeps itself (the JMAP
// VacationResponse) and reports whether it did. A client may read it with
// GETSCRIPT and sees it in LISTSCRIPTS, so that the setting shows; it
// changes over JMAP.
func (c *conn) reserved(name string) bool {
	if !ReservedScript(name) {
		return false
	}

	c.no(DescReservedScript)

	return true
}

func (c *conn) cmdCheckScript(args []string) {
	if len(args) < 1 {
		c.no("CHECKSCRIPT script")
		return
	}

	if err := CompileWith(args[0], c.hooks()); err != nil {
		c.no("Script error: " + err.Error())
		return
	}

	c.ok("Script is valid")
}

func (c *conn) authenticate(args []string) error {
	if len(args) < 1 || !strings.EqualFold(args[0], "PLAIN") {
		c.no("Unsupported mechanism")
		return nil
	}

	if !c.canAuth() {
		c.printf("NO (ENCRYPT-NEEDED) \"TLS required\"\r\n")
		return nil
	}

	ir, ok, err := c.initialResponse(args)
	if err != nil || !ok {
		return err
	}

	raw, err := base64.StdEncoding.DecodeString(ir)
	if err != nil {
		c.no("Invalid base64")
		return nil
	}

	parts := strings.Split(string(raw), "\x00")
	if len(parts) != 3 {
		c.no("Bad PLAIN response")
		return nil
	}

	c.login(parts[1], parts[2])

	return nil
}

// login checks the PLAIN credentials and opens the account; every
// outcome is answered to the client.
func (c *conn) login(username, password string) {
	res, err := c.s.o.Auth.Authenticate(context.Background(), auth.Request{
		Username: username, Password: password, Mechanism: "PLAIN", Protocol: "sieve",
		Client: c.nc.RemoteAddr(), Local: c.nc.LocalAddr(), TLS: c.tls,
	})
	if errors.Is(err, auth.ErrTemporary) {
		c.log.Warn("authentication unavailable", "event", "auth_unavailable")
		c.printf("NO (TRYLATER) \"Authentication temporarily unavailable\"\r\n")

		return
	}

	if err != nil {
		c.log.Info("authentication failed", "event", "auth_failed", "mechanism", "PLAIN")
		c.no("Authentication failed")

		return
	}

	account := res.Account
	if account == "" {
		account = username
	}

	u, err := c.s.o.Store.User(account)
	if err != nil {
		c.log.Error("storage unavailable", "event", "storage_error")
		c.no("Storage unavailable")

		return
	}

	provision.Apply(u, res, c.s.o.Log)
	c.user, c.name = u, account
	c.log = c.log.With("user", account)
	c.log.Info("authentication succeeded", "event", "auth_success", "mechanism", "PLAIN", "tls", c.tls)
	c.ok("Logged in")
}

// initialResponse returns the SASL PLAIN response: the initial response
// argument, or one requested with an empty challenge. ok is false when
// the client's answer was answered with NO already.
func (c *conn) initialResponse(args []string) (string, bool, error) {
	if len(args) > 1 {
		return args[1], true, nil
	}

	c.printf("\"\"\r\n")

	if err := c.bw.Flush(); err != nil {
		return "", false, err
	}

	line, err := c.readLine()
	if err != nil {
		return "", false, err
	}

	a, err := c.args(line)
	if err != nil || len(a) == 0 {
		c.no("Bad response")
		return "", false, nil
	}

	return a[0], true, nil
}

// Extensions is the SIEVE capability list (RFC 5804 1.7) of this server.
const Extensions = "fileinto envelope encoded-character imap4flags variables relational copy reject ereject subaddress environment body " +
	"vacation vacation-seconds date duplicate editheader mailbox ihave include index regex spamtest spamtestplus virustest " +
	"mboxmetadata servermetadata special-use enotify foreverypart mime extracttext " +
	"comparator-i;ascii-numeric comparator-i;unicode-casemap"

// hooks are the compile-time hooks of a ManageSieve session: includes
// resolve against the user's own scripts (:personal) and the global
// script directory, so PUTSCRIPT rejects what would fail at delivery.
func (c *conn) hooks() Hooks {
	return Hooks{Include: IncludeLoader(c.user, c.s.o.GlobalScripts), Config: c.s.o.Config}
}
