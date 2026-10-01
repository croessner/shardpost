package delivery

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"net/textproto"

	"shardpost/internal/model"
	"shardpost/internal/outbound"
	"shardpost/internal/sieve"
)

// reject carries out a Sieve reject or ereject (RFC 5429). ereject, and
// reject under sieve_reject_mode=protocol, refuse the message in the
// protocol. Otherwise reject accepts the message and sends a rejection
// message (an MDN) to the envelope sender through the relay; the
// message then counts as delivered, nothing is stored. Mail from the
// null sender is discarded silently (RFC 5429 2.1). Where no other
// automatic answer may go (automatic or list mail, list and system
// senders, see sieve.ShouldReject), the rejection is left to the
// sending MTA or list: the protocol-level rejection. Without a relay, or
// when the relay refuses the rejection message, the protocol-level
// rejection is the fallback as well, so the sender still learns of it. raw is the message as the script left it (editheader), which
// the notifications get; the rejection reports the header the sender
// sent.
func (r *run) reject(ctx context.Context, raw []byte, d *sieve.Delivery) (*Result, error) {
	user, cfg := r.req.Account, r.svc.SieveConfig
	refuse := &RejectedError{d.Rejected}

	if d.EReject || sieve.RejectMode(cfg) == sieve.RejectProtocol {
		return nil, refuse
	}

	original := r.req.Raw
	hdr, _ := textproto.NewReader(bufio.NewReader(bytes.NewReader(original))).ReadMIMEHeader()

	if envelopePath(r.req.From) == "" {
		r.log.Info("rejected by sieve, null sender: no rejection message, message discarded", "user", user)
		r.finishDiscard(ctx, raw, d)

		return &Result{Disposition: DispositionDiscarded}, nil
	}

	if ok, why := sieve.ShouldReject(hdr, r.req.From); !ok {
		r.log.Info("rejected by sieve, no rejection message for this mail, rejecting in the protocol", "user", user, "reason", why)

		return nil, refuse
	}

	if !r.relayEnabled() {
		r.log.Warn("sieve reject without relay, rejecting in the protocol", "user", user)

		return nil, refuse
	}

	to, msg := sieve.ComposeRejection(sieve.Rejection{Reason: d.Rejected, Original: original, EnvelopeFrom: r.req.From,
		Rcpt: r.req.Rcpt, Host: r.svc.Hostname}, cfg, r.now())

	err := r.svc.Relay.Send(ctx, "", []string{to}, msg)

	switch {
	case errors.Is(err, outbound.ErrMaybeSent):
		// The whole MDN went out and the relay's answer did not come in
		// time (a timeout, not a closed connection): it has probably
		// taken it. Counting the message as delivered
		// risks a lost rejection message; a protocol rejection on top
		// would tell the sender twice (MDN and the MTA's bounce).
		r.log.Warn("rejection message not confirmed by the relay, counted as sent", "user", user, "to", to, "err", err)
	case err != nil:
		r.log.Error("rejection message not sent, rejecting in the protocol", "user", user, "err", err)

		return nil, refuse
	default:
		r.log.Info("rejected by sieve, rejection message sent", "user", user, "to", to)
	}

	r.finishDiscard(ctx, raw, d)

	return &Result{Disposition: DispositionRejected}, nil
}

// finishDiscard ends a delivery that stores nothing: the duplicate keys
// are recorded and the notifications go out.
func (r *run) finishDiscard(ctx context.Context, raw []byte, d *sieve.Delivery) {
	r.track(d)
	r.notify(ctx, raw, d)
}

// fcc stores a copy of a message the script generated and sent (RFC
// 8580 :fcc of vacation and notify) in the recipient's mailbox, under
// its quota. A missing mailbox is created only with :create; a public
// folder needs the post right, a mailbox of another account is refused.
// Failures are logged and never fail the delivery.
func (r *run) fcc(f *sieve.Fcc, raw []byte) {
	if f == nil {
		return
	}

	user := r.req.Account

	name := f.Mailbox
	if f.SpecialUse != "" {
		if n, ok := mailboxWithUse(r.u, f.SpecialUse); ok {
			name = n
		}
	}

	t, refused, err := r.resolveTarget(name)
	if err == nil && refused == nil && !t.public {
		err = ensureFccMailbox(t, f.Create)
	}

	if err == nil {
		err = refused
	}

	if err != nil {
		r.log.Warn("fcc copy not stored", "user", user, "mailbox", name, "err", err)
		return
	}

	meta, err := t.u.Deliver(t.mailbox, raw, f.Flags, r.now())
	if err != nil {
		r.log.Warn("fcc copy not stored", "user", user, "mailbox", t.where, "err", err)
		return
	}

	r.log.Info("fcc copy stored", "user", user, "mailbox", t.where, "uid", meta.UID, "size", meta.Size)
}

// ensureFccMailbox makes sure an own :fcc mailbox exists: it is created
// with :create, otherwise a missing one is an error.
func ensureFccMailbox(t target, create bool) error {
	_, err := t.u.Lookup(t.mailbox)
	if !errors.Is(err, model.ErrNoMailbox) {
		return err
	}

	if !create {
		return ErrNoMailbox
	}

	return t.u.EnsureMailbox(t.mailbox, nil)
}
