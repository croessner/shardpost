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
}

// Enabled reports whether a relay is configured.
func (r *Relay) Enabled() bool { return r != nil && r.Addr != "" }

// Send submits raw to the recipients with the given envelope sender ("" is
// the null sender for automatic replies).
func (r *Relay) Send(ctx context.Context, from string, to []string, raw []byte) error {
	if !r.Enabled() {
		return ErrNoRelay
	}

	c, err := r.connect(ctx)
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
func (r *Relay) connect(ctx context.Context) (*smtp.Client, error) {
	timeout := r.Timeout
	if timeout == 0 {
		timeout = 30 * time.Second
	}

	host, _, err := net.SplitHostPort(r.Addr)
	if err != nil {
		return nil, fmt.Errorf("relay address: %w", err)
	}

	serverName := r.ServerName
	if serverName == "" {
		serverName = host
	}

	d := net.Dialer{Timeout: timeout}

	conn, err := d.DialContext(ctx, "tcp", r.Addr)
	if err != nil {
		return nil, fmt.Errorf("relay connect: %w", err)
	}

	_ = conn.SetDeadline(time.Now().Add(timeout))

	tlsCfg := &tls.Config{ServerName: serverName, MinVersion: tls.VersionTLS12}
	if r.Implicit {
		conn = tls.Client(conn, tlsCfg)
	}

	c, err := smtp.NewClient(conn, host)
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("relay greeting: %w", err)
	}

	if err := r.handshake(c, tlsCfg, host); err != nil {
		_ = c.Close()
		return nil, err
	}

	return c, nil
}

// handshake runs EHLO, STARTTLS and AUTH on a fresh client.
func (r *Relay) handshake(c *smtp.Client, tlsCfg *tls.Config, host string) error {
	helo := r.HELO
	if helo == "" {
		helo = "shardpost"
	}

	if err := c.Hello(helo); err != nil {
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
		return fmt.Errorf("relay DATA end: %w", err)
	}

	return nil
}
