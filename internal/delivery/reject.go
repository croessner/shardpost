package delivery

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"net/textproto"

	"shardpost/internal/model"
	"shardpost/internal/sieve"
)

// reject carries out a Sieve reject or ereject (RFC 5429). ereject, and
// reject under sieve_reject_mode=protocol, refuse the message in the
// protocol. Otherwise reject accepts the message and sends a rejection
// message (an MDN) to the envelope sender through the relay; the
// message then counts as delivered, nothing is stored. Mail from the
// null sender and automatic mail get no rejection message: it is
// discarded silently. Without a relay, or when the relay refuses the
// rejection message, the protocol-level rejection is the fallback, so
// the sender still learns of it.
func (r *run) reject(ctx context.Context, d *sieve.Delivery) (*Result, error) {
	user, cfg := r.req.Account, r.svc.SieveConfig
	refuse := &RejectedError{d.Rejected}

	if d.EReject || sieve.RejectMode(cfg) == sieve.RejectProtocol {
		return nil, refuse
	}

	raw := r.req.Raw
	hdr, _ := textproto.NewReader(bufio.NewReader(bytes.NewReader(raw))).ReadMIMEHeader()

	if ok, why := sieve.ShouldReject(hdr, r.req.From); !ok {
		r.log.Info("rejected by sieve, no rejection message sent, message discarded", "user", user, "reason", why)
		r.finishDiscard(ctx, raw, d)

		return &Result{}, nil
	}

	if !r.relayEnabled() {
		r.log.Warn("sieve reject without relay, rejecting in the protocol", "user", user)

		return nil, refuse
	}

	to, msg := sieve.ComposeRejection(sieve.Rejection{Reason: d.Rejected, Original: raw, EnvelopeFrom: r.req.From,
		Rcpt: r.req.Rcpt, Host: r.svc.Hostname}, cfg, r.now())
	if err := r.svc.Relay.Send(ctx, "", []string{to}, msg); err != nil {
		r.log.Error("rejection message not sent, rejecting in the protocol", "user", user, "err", err)

		return nil, refuse
	}

	r.log.Info("rejected by sieve, rejection message sent", "user", user, "to", to)
	r.finishDiscard(ctx, raw, d)

	return &Result{}, nil
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
