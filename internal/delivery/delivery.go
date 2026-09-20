// Package delivery stores one message for one recipient the way a local
// delivery agent does: it runs the recipient's Sieve script, files the
// message into the resulting mailboxes (all or nothing) and performs the
// side effects (redirect, vacation, notify, duplicate tracking). LMTP and
// the local delivery API of the admin interface share it, so both behave
// identically.
package delivery

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"go.opentelemetry.io/otel/attribute"

	"shardpost/internal/auth"
	"shardpost/internal/extprograms"
	"shardpost/internal/model"
	"shardpost/internal/outbound"
	"shardpost/internal/provision"
	"shardpost/internal/sieve"
	"shardpost/internal/telemetry"
)

// inboxName is the delivery mailbox without (or after failing) Sieve.
const inboxName = "INBOX"

// Errors of a delivery besides those of the store (model.ErrOverQuota,
// model.ErrTooLarge, ...).
var (
	// ErrRedirectFailed means a redirect without a local copy could not
	// be sent; nothing was stored, the caller should try again later.
	ErrRedirectFailed = errors.New("redirect failed")
	// ErrNoMailbox means the explicitly requested mailbox does not exist.
	ErrNoMailbox = errors.New("no such mailbox")
	// ErrQuotaExemptUnsupported means the store cannot skip quota
	// enforcement.
	ErrQuotaExemptUnsupported = errors.New("quota exemption not supported by the store")
)

// RejectedError is a Sieve reject: the message was refused with Reason.
type RejectedError struct{ Reason string }

func (e *RejectedError) Error() string { return "rejected by sieve: " + e.Reason }

// Store opens a per-user view of the mail store.
type Store interface {
	User(name string) (model.User, error)
}

// Service delivers messages. The zero value is not usable; Store and Log
// are required, the rest is optional.
type Service struct {
	Store Store
	Log   *slog.Logger
	// Relay sends Sieve redirect copies, vacation replies and
	// notifications; nil or unconfigured: redirects keep the message,
	// vacation and notify are skipped.
	Relay *outbound.Relay
	// GlobalScripts is the directory of "include :global" scripts.
	GlobalScripts string
	// Programs runs external programs (nil: extensions unavailable).
	Programs *extprograms.Runner
	// SieveConfig are the Sieve settings.
	SieveConfig map[string]string
	// Now is the clock (nil: time.Now).
	Now func() time.Time
	// PublicPrefix is the prefix a Sieve script uses to file into a
	// public folder ("" switches that off). Filing there needs the post
	// right on the folder; the message is stored in the public
	// namespace, not in the account.
	PublicPrefix string
	// SharedPrefix is the prefix under which clients see the mailboxes
	// of other accounts. A delivery cannot write there — the recipient
	// is one account — so a fileinto below it goes to INBOX instead of
	// creating a mailbox nobody can open.
	SharedPrefix string
}

// closeOpened releases the extra namespaces a delivery filed into.
func (r *run) closeOpened() {
	for _, u := range r.opened {
		u.Close()
	}

	r.opened, r.pub = nil, nil
}

// Request is one message for one recipient.
type Request struct {
	// Account is the canonical account from the recipient lookup.
	Account string
	// Rcpt is the envelope recipient as given (Sieve envelope, vacation).
	Rcpt string
	// From is the envelope sender.
	From string
	// Raw is the message, including any trace header the caller adds.
	Raw []byte
	// Lookup is the result of the recipient lookup (userdb attributes
	// such as the quota limit and the over-quota flag); may be nil.
	Lookup *auth.Result
	// Mailbox, if set, stores the message directly into this existing
	// mailbox without running Sieve (like dovecot-lda -m without a
	// script); "" runs Sieve with INBOX as the implicit keep.
	Mailbox string
	// NoSieve skips the user's script and stores into INBOX (or Mailbox).
	NoSieve bool
	// NoQuotaEnforcement stores even over the quota limit; the usage is
	// still counted (dovecot-lda noenforcing).
	NoQuotaEnforcement bool
	// Log is the request logger (nil: the service logger).
	Log *slog.Logger
}

// Stored is one message stored by a delivery.
type Stored struct {
	Mailbox string
	UID     uint32
	GUID    string
	Size    int64
}

// Result is a successful delivery; Stored is empty when Sieve discarded
// the message.
type Result struct {
	Stored []Stored
}

// run is the state of one delivery.
type run struct {
	svc *Service
	req Request
	log *slog.Logger
	u   model.User
	// opened are further namespaces this delivery filed into (public
	// folders); they are closed with the delivery. pub is the public
	// namespace among them, opened at most once.
	opened []model.User
	pub    model.User
}

// Deliver runs the recipient's Sieve script (unless disabled) and stores
// the message into the resulting mailboxes; side effects happen only
// after the message is durably stored.
func (s *Service) Deliver(ctx context.Context, req Request) (*Result, error) {
	ctx, span := telemetry.Span(ctx, "delivery.deliver", attribute.String("delivery.account", req.Account), attribute.Int("delivery.size", len(req.Raw)))
	defer span.End()

	u, err := s.Store.User(req.Account)
	if err != nil {
		return nil, err
	}
	defer u.Close()

	// The store records its operations below this delivery's span.
	model.UseRequest(ctx, u)
	defer model.UseRequest(context.Background(), u)

	r := &run{svc: s, req: req, log: req.Log, u: u}

	defer r.closeOpened()

	if r.log == nil {
		r.log = s.Log
	}

	if req.NoQuotaEnforcement {
		qe, ok := u.(model.QuotaExemptable)
		if !ok {
			return nil, ErrQuotaExemptUnsupported
		}

		qe.SetQuotaEnforced(false)
	}

	provision.Apply(u, req.Lookup, r.log)

	return r.deliver(ctx)
}

// deliver decides the targets and stores the message.
func (r *run) deliver(ctx context.Context) (*Result, error) {
	d, err := r.targets(ctx)
	if err != nil {
		return nil, err
	}

	raw := r.req.Raw
	if len(d.Raw) > 0 {
		raw = d.Raw // editheader results
	}

	if d.Rejected != "" {
		return nil, &RejectedError{d.Rejected}
	}
	// A redirect without a local copy goes out now: a relay failure is
	// a temporary delivery failure (the sender retries, nothing was
	// stored). With a local copy the message is stored first and
	// forwarded after (below): a relay failure then only loses the
	// forward, never the mail, and a retry cannot forward twice. Without
	// a relay, or when loop protection forwards nothing, the message is
	// kept.
	redirectLater := len(d.Redirects) > 0 && len(d.FileInto) > 0
	if len(d.Redirects) > 0 && !redirectLater {
		if err := r.redirectNow(ctx, raw, d); err != nil {
			return nil, err
		}
	}
	// fileinto :specialuse: the mailbox carrying that use wins (RFC 8579 4)
	resolveSpecialUse(r.u, d)

	if len(d.FileInto) == 0 {
		r.log.Info("discarded by sieve", "user", r.req.Account, "from", r.req.From)
		r.track(d)
		r.notify(ctx, raw, d)

		return &Result{}, nil
	}

	stored, err := r.store(raw, d)
	if err != nil {
		return nil, err
	}
	// The delivery may have filled the account: correct the flag now,
	// not only at the next login.
	provision.CheckOverFlag(r.u, r.req.Lookup, r.log)
	// Side effects only after the message is durably stored.
	r.afterStore(ctx, raw, d, redirectLater)

	return &Result{Stored: stored}, nil
}

// targets returns what to do with the message: the direct target
// mailbox, INBOX without Sieve, or the result of the user's script.
func (r *run) targets(ctx context.Context) (*sieve.Delivery, error) {
	if r.req.Mailbox != "" {
		if _, err := r.u.Lookup(r.req.Mailbox); err != nil {
			if errors.Is(err, model.ErrNoMailbox) {
				return nil, ErrNoMailbox
			}

			return nil, err
		}

		return &sieve.Delivery{FileInto: []sieve.FileInto{{Mailbox: r.req.Mailbox}}}, nil
	}

	if err := r.u.EnsureMailbox(inboxName, nil); err != nil {
		return nil, err
	}

	if r.req.NoSieve {
		return &sieve.Delivery{FileInto: []sieve.FileInto{{Mailbox: inboxName}}}, nil
	}

	return r.runSieve(ctx), nil
}

// afterStore runs the side effects of a stored delivery: duplicate
// tracking, the deferred redirect, vacation and notifications.
func (r *run) afterStore(ctx context.Context, raw []byte, d *sieve.Delivery, redirectLater bool) {
	r.track(d)

	if redirectLater {
		r.redirectAfterStore(ctx, raw, d)
	}

	if d.Vacation != nil {
		r.vacation(ctx, raw, d.Vacation)
	}

	r.notify(ctx, raw, d)
}

// now is the service clock.
func (r *run) now() time.Time {
	if r.svc.Now != nil {
		return r.svc.Now()
	}

	return time.Now()
}

// relayEnabled reports whether side effects can go out.
func (r *run) relayEnabled() bool { return r.svc.Relay != nil && r.svc.Relay.Enabled() }
