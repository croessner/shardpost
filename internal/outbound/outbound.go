// Package outbound sends mail through an SMTP relay: Sieve redirect
// copies and vacation replies. Without a configured relay Send returns
// ErrNoRelay and callers degrade (redirect keeps the message, vacation is
// skipped) rather than failing the delivery.
package outbound

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/smtp"
	"time"
)

// ErrNoRelay means no relay is configured.
var ErrNoRelay = errors.New("no outbound relay configured")

// ErrMaybeSent marks a timeout after the whole message went to the relay:
// no answer arrived for the end of DATA in time, so the relay may have
// taken the message. A caller for which a second copy is worse than a
// lost one treats it as sent. A refusal (a reply) or a connection the
// relay closed or reset is no ErrMaybeSent: a relay that took the
// message answers before it hangs up. The final flush of the data writer
// is no evidence either way: net/smtp ignores its error and only reports
// what reading the reply returns, so the timeout of that read is what
// decides.
var ErrMaybeSent = errors.New("relay did not confirm the message")

// Relay is an SMTP submission target.
type Relay struct {
	Addr     string // host:port; "" disables sending
	HELO     string
	StartTLS bool   // require STARTTLS
	Implicit bool   // implicit TLS (port 465)
	User     string // optional PLAIN auth
	Password string
	// ServerName overrides the TLS server name (default: host of Addr).
	ServerName string
	Timeout    time.Duration
	// DataTimeout bounds the wait for the relay's answer to a submitted
	// message (default 10 minutes); see Relay.Submit.
	DataTimeout time.Duration
}

// Enabled reports whether a relay is configured.
func (r *Relay) Enabled() bool { return r != nil && r.Addr != "" }

// Send submits raw to the recipients with the given envelope sender ("" is
// the null sender for automatic replies).
func (r *Relay) Send(ctx context.Context, from string, to []string, raw []byte) error {
	if !r.Enabled() {
		return ErrNoRelay
	}

	c, _, err := r.connect(ctx)
	if err != nil {
		return err
	}

	defer func() { _ = c.Close() }()

	if err := r.transaction(c, from, to, raw); err != nil {
		return err
	}
	// Accepted: a failing QUIT must not turn into a retry (duplicate).
	_ = c.Quit()

	return nil
}

// connect dials the relay and returns a client after EHLO, STARTTLS (when
// offered or required) and authentication.
// It also returns the TCP connection, whose deadline governs the session.
func (r *Relay) connect(ctx context.Context) (*smtp.Client, net.Conn, error) {
	timeout := r.Timeout
	if timeout == 0 {
		timeout = 30 * time.Second
	}

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

	conn := raw
	tlsCfg := &tls.Config{ServerName: serverName, MinVersion: tls.VersionTLS12}

	if r.Implicit {
		conn = tls.Client(conn, tlsCfg)
	}

	c, err := smtp.NewClient(conn, host)
	if err != nil {
		_ = conn.Close()
		return nil, nil, fmt.Errorf("relay greeting: %w", err)
	}

	if err := r.handshake(c, tlsCfg, host); err != nil {
		_ = c.Close()
		return nil, nil, err
	}

	return c, raw, nil
}

// handshake runs EHLO, STARTTLS and AUTH on a fresh client.
func (r *Relay) handshake(c *smtp.Client, tlsCfg *tls.Config, host string) error {
	if err := c.Hello(r.helo()); err != nil {
		return fmt.Errorf("relay EHLO: %w", err)
	}

	if !r.Implicit {
		if ok, _ := c.Extension("STARTTLS"); ok {
			if err := c.StartTLS(tlsCfg); err != nil {
				return fmt.Errorf("relay STARTTLS: %w", err)
			}
		} else if r.StartTLS {
			return errors.New("relay does not offer STARTTLS")
		}
	}

	if r.User != "" {
		if err := c.Auth(smtp.PlainAuth("", r.User, r.Password, host)); err != nil {
			return fmt.Errorf("relay AUTH: %w", err)
		}
	}

	return nil
}

// transaction sends the envelope and the message.
func (r *Relay) transaction(c *smtp.Client, from string, to []string, raw []byte) error {
	if err := c.Mail(from); err != nil {
		return fmt.Errorf("relay MAIL FROM: %w", err)
	}

	for _, rcpt := range to {
		if err := c.Rcpt(rcpt); err != nil {
			return fmt.Errorf("relay RCPT TO %s: %w", rcpt, err)
		}
	}

	w, err := c.Data()
	if err != nil {
		return fmt.Errorf("relay DATA: %w", err)
	}

	if _, err := w.Write(raw); err != nil {
		return fmt.Errorf("relay DATA: %w", err)
	}

	if err := w.Close(); err != nil {
		// Only a timeout leaves open whether the relay took the message;
		// a reply is a refusal, a closed or reset connection a failure.
		var nerr net.Error
		if errors.As(err, &nerr) && nerr.Timeout() {
			return fmt.Errorf("relay DATA end: %w: %w", ErrMaybeSent, err)
		}

		return fmt.Errorf("relay DATA end: %w", err)
	}

	return nil
}
