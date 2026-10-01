// Package lmtpd is the LMTP (RFC 2033) delivery front-end. Recipients are
// resolved to accounts through the authenticator; each recipient's copy
// is handed to the delivery service (Sieve, mailboxes, side effects).
package lmtpd

import (
	"context"
	"crypto/subtle"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"runtime/debug"
	"time"

	"github.com/emersion/go-sasl"
	"github.com/emersion/go-smtp"

	"go.opentelemetry.io/otel/attribute"

	"shardpost/internal/auth"
	"shardpost/internal/cephstore"
	"shardpost/internal/delivery"
	"shardpost/internal/logging"
	"shardpost/internal/metrics"
	"shardpost/internal/model"
	"shardpost/internal/telemetry"
)

// Backend implements smtp.Backend.
type Backend struct {
	// Delivery stores the messages (Sieve, mailboxes, side effects).
	Delivery *delivery.Service
	Auth     auth.Authenticator
	Log      *slog.Logger
	// ServiceUser/ServicePassword, when set, require the submitting MTA or
	// director to AUTH PLAIN/LOGIN with these service credentials
	// (nauthilus-director backend auth mode "sasl").
	ServiceUser     string
	ServicePassword string
	// Host names this server in Received headers ("" = os.Hostname).
	Host string
}

const (
	// defaultHostname names the server when nothing better is known.
	defaultHostname = "shardpost"
	// msgTooLarge is the text of the 552 5.3.4 reply.
	msgTooLarge = "Message too large"
)

// NewServer builds an LMTP server on addr. tlsConfig enables STARTTLS;
// insecureAuth allows the service AUTH before STARTTLS.
func NewServer(addr string, be *Backend, tlsConfig *tls.Config, insecureAuth bool) *smtp.Server {
	s := smtp.NewServer(be)
	s.TLSConfig = tlsConfig
	s.AllowInsecureAuth = insecureAuth
	s.LMTP = true
	s.Network = "tcp" // go-smtp defaults LMTP to a unix socket
	s.Addr = addr
	s.Domain = defaultHostname
	s.EnableSMTPUTF8 = true
	s.MaxMessageBytes = cephstore.MaxMessageLen
	s.MaxRecipients = 100

	return s
}

// Hostname is the name used in Received headers.
func (b *Backend) Hostname() string {
	if b.Host != "" {
		return b.Host
	}

	if h, err := os.Hostname(); err == nil {
		return h
	}

	return defaultHostname
}

// NewSession implements smtp.Backend.
func (b *Backend) NewSession(c *smtp.Conn) (smtp.Session, error) {
	log := logging.Session(b.Log, c.Conn())
	_, tls := c.TLSConnectionState()

	return &session{be: b, conn: c, log: log, endLog: logging.Begin(log, tls)}, nil
}

type session struct {
	endLog   func()
	be       *Backend
	conn     *smtp.Conn
	log      *slog.Logger
	from     string
	rcpt     []string // canonical accounts
	rcptAddr []string // original envelope addresses
	rcptRes  []*auth.Result
	authed   bool
}

// AuthMechanisms implements smtp.AuthSession (service credentials only,
// nauthilus-director backend auth mode "sasl").
func (s *session) AuthMechanisms() []string {
	if s.be.ServiceUser == "" {
		return nil
	}

	return []string{sasl.Plain}
}

// Auth implements smtp.AuthSession.
func (s *session) Auth(mech string) (sasl.Server, error) {
	check := func(user, pass string) error {
		if user != s.be.ServiceUser || subtle.ConstantTimeCompare([]byte(pass), []byte(s.be.ServicePassword)) != 1 {
			s.log.Info("authentication failed", "event", "auth_failed", "mechanism", "PLAIN")
			return smtp.ErrAuthFailed
		}

		s.authed = true
		s.log.Info("authentication succeeded", "event", "auth_success", "mechanism", "PLAIN")

		return nil
	}
	if mech == sasl.Plain {
		return sasl.NewPlainServer(func(_, user, pass string) error { return check(user, pass) }), nil
	}

	return nil, smtp.ErrAuthUnknownMechanism
}

var errNoUser = &smtp.SMTPError{Code: 550, EnhancedCode: smtp.EnhancedCode{5, 1, 1}, Message: "No such user"}

func (s *session) Reset() { s.from, s.rcpt, s.rcptAddr, s.rcptRes = "", nil, nil, nil }

func (s *session) Logout() error {
	if s.endLog != nil {
		s.endLog()
		s.endLog = nil
	}

	return nil
}

func (s *session) Mail(from string, _ *smtp.MailOptions) error {
	if s.be.ServiceUser != "" && !s.authed {
		return errAuthRequired
	}

	s.from = from

	return nil
}

// errAuthRequired answers MAIL or RCPT before authentication when service
// credentials are configured (RFC 4954 6: 530 5.7.0).
var errAuthRequired = &smtp.SMTPError{Code: 530, EnhancedCode: smtp.EnhancedCode{5, 7, 0}, Message: "Authentication required"}

// Rcpt resolves the envelope recipient to its account (aliases included)
// through the authenticator; the director forwards the original address
// unchanged, so this is where the mapping has to happen.
func (s *session) Rcpt(to string, _ *smtp.RcptOptions) error {
	if s.be.ServiceUser != "" && !s.authed {
		return errAuthRequired
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	res, err := s.be.Auth.Lookup(ctx, to, "lmtp", s.conn.Conn().RemoteAddr())
	switch {
	case errors.Is(err, auth.ErrInvalid):
		return errNoUser
	case err != nil:
		s.log.Warn("recipient lookup failed", "rcpt", to, "err", err)
		return &smtp.SMTPError{Code: 451, EnhancedCode: smtp.EnhancedCode{4, 3, 0}, Message: "Recipient lookup temporarily unavailable"}
	}

	s.rcpt = append(s.rcpt, res.Account)
	s.rcptRes = append(s.rcptRes, res)
	s.rcptAddr = append(s.rcptAddr, to)

	return nil
}

// Data is used in SMTP mode only; LMTP calls LMTPData.
func (s *session) Data(r io.Reader) error {
	return s.LMTPData(r, nil)
}

// LMTPData delivers the message to every recipient and reports a
// per-recipient status, as LMTP requires.
func (s *session) LMTPData(r io.Reader, status smtp.StatusCollector) (err error) {
	defer func() {
		// a bug must cost this transaction (temporary failure, the MTA
		// retries), not the process
		if rec := recover(); rec != nil {
			s.log.Error("panic in lmtp delivery", "panic", rec, "stack", string(debug.Stack()))

			err = &smtp.SMTPError{Code: 451, EnhancedCode: smtp.EnhancedCode{4, 3, 0}, Message: "Internal error, try again later"}
		}
	}()
	// Buffer the message in steps, each step reserved against the process
	// memory budget before it is read.
	raw, release, err := delivery.ReadMessage(r, cephstore.MaxMessageLen)
	defer release()

	switch {
	case errors.Is(err, model.ErrTooLarge):
		return &smtp.SMTPError{Code: 552, EnhancedCode: smtp.EnhancedCode{5, 3, 4}, Message: msgTooLarge}
	case errors.Is(err, delivery.ErrBusy):
		return &smtp.SMTPError{Code: 452, EnhancedCode: smtp.EnhancedCode{4, 3, 1}, Message: "Insufficient system storage"}
	case err != nil:
		return err
	}

	var firstErr error

	ctx := context.Background()

	for i, user := range s.rcpt {
		msg := append(s.received(s.rcptAddr[i]), raw...)

		err := s.deliver(ctx, user, s.rcptAddr[i], msg, s.rcptRes[i])
		if err == nil {
			metrics.Deliveries.WithLabelValues("ok").Inc()
			metrics.DeliveryBytes.Add(float64(len(raw)))
		} else {
			metrics.Deliveries.WithLabelValues("failed").Inc()

			err = s.deliveryError(user, err)
			if firstErr == nil {
				firstErr = err
			}
		}

		if status != nil {
			status.SetStatus(s.rcptAddr[i], err)
		}
	}

	if status == nil {
		return firstErr
	}

	return nil
}

// received is the Received header for one recipient, prepended to its
// copy (RFC 5321 4.4).
func (s *session) received(rcpt string) []byte {
	return []byte(fmt.Sprintf("Received: from %s\r\n\tby %s with LMTP\r\n\tfor <%s>; %s\r\n",
		s.conn.Conn().RemoteAddr().String(), s.be.Hostname(), rcpt, timeNow().Format(time.RFC1123Z)))
}

// deliveryError maps a failed delivery to its LMTP reply.
func (s *session) deliveryError(user string, err error) error {
	var rej *delivery.RejectedError
	switch {
	case errors.As(err, &rej):
		return &smtp.SMTPError{Code: 550, EnhancedCode: smtp.EnhancedCode{5, 7, 1}, Message: rej.Reason}
	case errors.Is(err, model.ErrOverQuota):
		return &smtp.SMTPError{Code: 552, EnhancedCode: smtp.EnhancedCode{5, 2, 2}, Message: "Mailbox full"}
	case errors.Is(err, model.ErrTooLarge):
		return &smtp.SMTPError{Code: 552, EnhancedCode: smtp.EnhancedCode{5, 3, 4}, Message: msgTooLarge}
	case errors.Is(err, delivery.ErrRedirectFailed):
		return &smtp.SMTPError{Code: 451, EnhancedCode: smtp.EnhancedCode{4, 4, 0}, Message: "Redirect failed, try again later"}
	case errors.Is(err, model.ErrImportLocked):
		s.log.Info("delivery deferred, account locked (import or maintenance)", "user", user)
		return &smtp.SMTPError{Code: 451, EnhancedCode: smtp.EnhancedCode{4, 2, 1}, Message: "Mailbox temporarily unavailable, try again later"}
	default:
		s.log.Error("delivery failed", "user", user, "err", err)
		return &smtp.SMTPError{Code: 451, EnhancedCode: smtp.EnhancedCode{4, 3, 0}, Message: "Temporary storage failure"}
	}
}

// deliver hands one recipient's copy to the delivery service.
func (s *session) deliver(ctx context.Context, user, rcpt string, raw []byte, res *auth.Result) error {
	ctx, span := telemetry.Span(ctx, "lmtp.deliver",
		append(telemetry.Personal(attribute.String("lmtp.user", user)), attribute.Int("lmtp.size", len(raw)))...)
	defer span.End()

	_, err := s.be.Delivery.Deliver(ctx, delivery.Request{Account: user, Rcpt: rcpt, From: s.from, Raw: raw, Lookup: res, Log: s.log})

	return err
}

func timeNow() time.Time { return time.Now() }

var (
	_ smtp.LMTPSession = (*session)(nil)
	_ smtp.AuthSession = (*session)(nil)
)
