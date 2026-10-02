package outbound

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/smtp"
	"net/textproto"
	"strings"
	"time"
)

// Submission for a user (JMAP EmailSubmission). The relay is told who the
// user is with Postfix's XCLIENT: after "XCLIENT LOGIN=<account>
// ADDR=<client>" the relay treats the session as if the user had logged
// in with SASL, and its own rules — sender login maps, rate limits,
// policy services — decide what the user may send. The relay has to list
// the ShardPost hosts in smtpd_authorized_xclient_hosts.

// XClient names the user a submission is made for.
type XClient struct {
	Login string // the account, as the relay's SASL login name
	Addr  string // the client's IP address, "" if unknown
}

// Reply is one SMTP reply.
type Reply struct {
	Code    int
	Message string
}

// String is the reply as RFC 8621 7 wants it in smtpReply: code, space,
// the lines joined.
func (r Reply) String() string {
	return fmt.Sprintf("%d %s", r.Code, strings.Join(strings.Fields(r.Message), " "))
}

// Report is what the relay said about a submission.
type Report struct {
	// Stage is where the transaction ended: "MAIL", "RCPT", "DATA" or ""
	// when it was accepted.
	Stage string
	// Refused are the recipients the relay refused, with its replies.
	Refused map[string]Reply
	// Final is the reply that ended the transaction: the refusal of MAIL
	// or DATA, or the acceptance of the message.
	Final Reply
}

// Accepted reports whether the relay took the message.
func (r *Report) Accepted() bool { return r.Stage == "" && r.Final.Code/100 == 2 }

// Temporary reports whether the relay's refusal may pass (4xx).
func (r *Report) Temporary() bool {
	if r.Final.Code/100 == 4 {
		return true
	}

	for _, rep := range r.Refused {
		if rep.Code/100 == 4 {
			return true
		}
	}

	return false
}

// ErrInData marks a transport error after the relay accepted DATA: the
// message was on its way, and whether the relay took it cannot be told.
// A sender that must not send twice (the delayed submission worker)
// takes such a submission as sent with an unknown result instead of
// trying again. Every other error of Submit happened before the message
// was transferred.
var ErrInData = errors.New("the connection failed while the message was transferred")

// ErrNoXClient is returned when the relay does not offer XCLIENT to this
// host.
var ErrNoXClient = errors.New("the relay does not offer XCLIENT to this host")

// Submit sends a message for a user. With as set, the user is named to
// the relay with XCLIENT and no AUTH is made; without it, the relay's
// configured credentials are used. The transaction is all or nothing: a
// refused recipient ends it before DATA, and the report says which. An
// error means the relay could not be talked to, before the message was
// transferred unless it wraps ErrInData.
func (r *Relay) Submit(ctx context.Context, as *XClient, from string, to []string, raw []byte) (*Report, error) {
	if !r.Enabled() {
		return nil, ErrNoRelay
	}

	c, conn, err := r.connectAs(ctx, as)
	if err != nil {
		return nil, err
	}

	defer func() { _ = c.Close() }()

	s := &session{c: c, conn: conn, timeout: r.timeout(), dataTimeout: r.dataTimeout()}

	rep, err := s.transaction(from, to, raw)
	if err != nil {
		return nil, err
	}

	if rep.Accepted() {
		_ = c.Quit()
	} else {
		_ = c.Reset()
	}

	return rep, nil
}

// timeout bounds one step of a conversation with the relay.
func (r *Relay) timeout() time.Duration {
	if r.Timeout == 0 {
		return 30 * time.Second
	}

	return r.Timeout
}

// dataTimeout bounds the wait for the relay's answer to a message: a
// relay runs its content filters before it answers, and a message it has
// taken must not be reported as failed because the answer came late —
// the client would send it again.
func (r *Relay) dataTimeout() time.Duration {
	if r.DataTimeout == 0 {
		return 10 * time.Minute
	}

	return r.DataTimeout
}

// minRate is the transfer rate a message upload is given time for.
const minRate = 256 << 10 // bytes per second

// session is one submission conversation, with a deadline per step.
type session struct {
	c           *smtp.Client
	conn        net.Conn
	timeout     time.Duration
	dataTimeout time.Duration
}

// step starts the deadline of one step.
func (s *session) step(d time.Duration) { _ = s.conn.SetDeadline(time.Now().Add(d)) }

// transaction runs MAIL, RCPT and DATA and records the replies.
func (s *session) transaction(from string, to []string, raw []byte) (*Report, error) {
	rep := &Report{Refused: map[string]Reply{}}

	s.step(s.timeout)

	if err := s.c.Mail(from); err != nil {
		return refusal(rep, "MAIL", err)
	}

	for _, rcpt := range to {
		s.step(s.timeout)

		if err := s.c.Rcpt(rcpt); err != nil {
			var te *textproto.Error
			if !errors.As(err, &te) {
				return nil, fmt.Errorf("relay RCPT TO: %w", err)
			}

			rep.Refused[rcpt] = Reply{Code: te.Code, Message: te.Msg}
		}
	}

	if len(rep.Refused) > 0 {
		rep.Stage = "RCPT"

		return rep, nil
	}

	return s.data(rep, raw)
}

// data sends the message and reads the relay's final answer, which
// net/smtp would not hand back; it carries the relay's queue id.
func (s *session) data(rep *Report, raw []byte) (*Report, error) {
	s.step(s.timeout)

	if _, err := command(s.c.Text, 354, "DATA"); err != nil {
		return refusal(rep, "DATA", err)
	}

	s.step(s.timeout + time.Duration(len(raw)/minRate)*time.Second)

	w := s.c.Text.DotWriter()
	if _, err := w.Write(raw); err != nil {
		return nil, fmt.Errorf("relay DATA: %w: %w", ErrInData, err)
	}

	if err := w.Close(); err != nil {
		return nil, fmt.Errorf("relay DATA: %w: %w", ErrInData, err)
	}

	s.step(s.dataTimeout)

	code, msg, err := s.c.Text.ReadResponse(2) // any 2xx takes the message
	if err != nil {
		var te *textproto.Error
		if !errors.As(err, &te) {
			return nil, fmt.Errorf("relay DATA: %w: %w", ErrInData, err)
		}

		return refusal(rep, "DATA", err)
	}

	rep.Final = Reply{Code: code, Message: msg}

	return rep, nil
}

// refusal records a refused stage, or passes a transport error on.
func refusal(rep *Report, stage string, err error) (*Report, error) {
	var te *textproto.Error
	if !errors.As(err, &te) {
		return nil, fmt.Errorf("relay %s: %w", stage, err)
	}

	rep.Stage, rep.Final = stage, Reply{Code: te.Code, Message: te.Msg}

	return rep, nil
}

// connectAs opens a session: with as, EHLO, STARTTLS and XCLIENT are run
// by hand, because net/smtp cannot send XCLIENT, and the session the
// relay starts after it is handed to net/smtp; without, as connect does.
func (r *Relay) connectAs(ctx context.Context, as *XClient) (*smtp.Client, net.Conn, error) {
	if as == nil {
		return r.connect(ctx)
	}

	timeout := r.timeout()

	host, _, err := net.SplitHostPort(r.Addr)
	if err != nil {
		return nil, nil, fmt.Errorf("relay address: %w", err)
	}

	serverName := r.ServerName
	if serverName == "" {
		serverName = host
	}

	d := net.Dialer{Timeout: timeout}

	raw, err := d.DialContext(ctx, "tcp", r.Addr)
	if err != nil {
		return nil, nil, fmt.Errorf("relay connect: %w", err)
	}

	_ = raw.SetDeadline(time.Now().Add(timeout))

	conn, err := r.xclient(raw, &tls.Config{ServerName: serverName, MinVersion: tls.VersionTLS12}, as)
	if err != nil {
		_ = conn.Close()

		return nil, nil, err
	}

	c, err := smtp.NewClient(conn, host)
	if err != nil {
		_ = conn.Close()

		return nil, nil, xclientRefused(err)
	}

	if err := c.Hello(r.helo()); err != nil {
		_ = c.Close()

		return nil, nil, fmt.Errorf("relay EHLO: %w", err)
	}

	return c, raw, nil
}

// ErrXClientRefused is returned when the relay refuses XCLIENT: it does
// not trust this host with naming users, which is a matter of its
// configuration, not of the moment.
var ErrXClientRefused = errors.New("the relay refuses XCLIENT from this host")

// xclientRefused names a refusal of XCLIENT, which net/smtp sees as a
// bad greeting.
func xclientRefused(err error) error {
	var te *textproto.Error
	if errors.As(err, &te) && te.Code/100 == 5 {
		return fmt.Errorf("%w: %d %s", ErrXClientRefused, te.Code, te.Msg)
	}

	return fmt.Errorf("relay greeting after XCLIENT: %w", err)
}

// defaultHELO is the name the relay is greeted with when none is set.
const defaultHELO = "shardpost"

// helo is the name the relay is greeted with.
func (r *Relay) helo() string {
	if r.HELO == "" {
		return defaultHELO
	}

	return r.HELO
}

// xclient runs the greeting, EHLO, STARTTLS and XCLIENT on a fresh
// connection and returns the connection (TLS by then) for the session the
// relay opens with its answer to XCLIENT.
func (r *Relay) xclient(conn net.Conn, tlsCfg *tls.Config, as *XClient) (net.Conn, error) {
	if r.Implicit {
		conn = tls.Client(conn, tlsCfg)
	}

	tp := textproto.NewConn(conn)

	if _, _, err := tp.ReadResponse(220); err != nil {
		return conn, fmt.Errorf("relay greeting: %w", err)
	}

	ext, err := ehlo(tp, r.helo())
	if err != nil {
		return conn, err
	}

	if !r.Implicit && (ext["STARTTLS"] || r.StartTLS) {
		if conn, tp, ext, err = r.startTLS(conn, tp, ext, tlsCfg); err != nil {
			return conn, err
		}
	}

	if !ext["XCLIENT"] || !ext["XCLIENT LOGIN"] {
		return conn, ErrNoXClient
	}

	attrs := "LOGIN=" + xtext(as.Login)
	if as.Addr != "" {
		attrs += " ADDR=" + xclientAddr(as.Addr)
	}

	// The relay answers XCLIENT with the greeting of a new session, which
	// net/smtp reads as its own.
	if err := tp.PrintfLine("XCLIENT %s", attrs); err != nil {
		return conn, fmt.Errorf("relay XCLIENT: %w", err)
	}

	return conn, nil
}

// startTLS upgrades the connection and greets the relay again.
func (r *Relay) startTLS(conn net.Conn, tp *textproto.Conn, ext map[string]bool, tlsCfg *tls.Config,
) (net.Conn, *textproto.Conn, map[string]bool, error) {
	if !ext["STARTTLS"] {
		return conn, tp, nil, errors.New("relay does not offer STARTTLS")
	}

	if _, err := command(tp, 220, "STARTTLS"); err != nil {
		return conn, tp, nil, fmt.Errorf("relay STARTTLS: %w", err)
	}

	conn = tls.Client(conn, tlsCfg)
	tp = textproto.NewConn(conn)

	ext, err := ehlo(tp, r.helo())

	return conn, tp, ext, err
}

// ehlo greets the relay and returns the extensions it offers.
func ehlo(tp *textproto.Conn, name string) (map[string]bool, error) {
	msg, err := command(tp, 250, "EHLO "+name)
	if err != nil {
		return nil, fmt.Errorf("relay EHLO: %w", err)
	}

	ext := map[string]bool{}

	for _, line := range strings.Split(msg, "\n")[1:] {
		f := strings.Fields(strings.ToUpper(line))
		if len(f) == 0 {
			continue
		}

		// An extension, and each of its parameters as "EXT PARAM", so
		// that XCLIENT's list of attributes can be asked about.
		ext[f[0]] = true
		for _, p := range f[1:] {
			ext[f[0]+" "+p] = true
		}
	}

	return ext, nil
}

// command sends one command and reads its reply.
func command(tp *textproto.Conn, want int, line string) (string, error) {
	id, err := tp.Cmd("%s", line)
	if err != nil {
		return "", err
	}

	tp.StartResponse(id)
	defer tp.EndResponse(id)

	_, msg, err := tp.ReadResponse(want)

	return msg, err
}

// xtext encodes an XCLIENT attribute value (RFC 3461 4): '+', '=' and
// everything outside printable ASCII as "+HH".
func xtext(s string) string {
	var b strings.Builder

	for i := 0; i < len(s); i++ {
		c := s[i]
		if c < '!' || c > '~' || c == '+' || c == '=' {
			fmt.Fprintf(&b, "+%02X", c)

			continue
		}

		b.WriteByte(c)
	}

	return b.String()
}

// xclientAddr writes a client address as XCLIENT wants it: IPv6 with the
// "IPV6:" prefix.
func xclientAddr(addr string) string {
	if ip := net.ParseIP(addr); ip != nil && ip.To4() == nil {
		return "IPV6:" + ip.String()
	}

	return addr
}
