package delivery

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"net/textproto"
	"strings"

	"shardpost/internal/outbound"
	"shardpost/internal/sieve"
)

// maxRedirects bounds the fan-out of one script.
const maxRedirects = sieve.MaxRedirects

// maxHops is the Received-header count beyond which a message is not
// redirected any more (like an MTA's hop limit).
const maxHops = 25

// redirectedFromHeader marks the accounts a message was redirected from
// (Dovecot uses the same header), for loop detection across accounts.
const redirectedFromHeader = "X-Sieve-Redirected-From"

// redirectNow forwards a message that has no local copy. Without a
// relay, or when nothing was forwarded, the message goes to INBOX; a
// relay failure is a temporary delivery failure.
func (r *run) redirectNow(ctx context.Context, raw []byte, d *sieve.Delivery) error {
	user := r.req.Account

	sent, err := r.redirect(ctx, raw, d.Redirects)
	switch {
	case errors.Is(err, outbound.ErrNoRelay):
		r.log.Warn("sieve redirect without relay, keeping message", "user", user, "to", d.Redirects)

		sent = false
	case err != nil:
		r.log.Error("sieve redirect failed", "user", user, "err", err)
		return ErrRedirectFailed
	}

	if !sent {
		d.FileInto = append(d.FileInto, sieve.FileInto{Mailbox: inboxName})
	}

	return nil
}

// redirectAfterStore forwards a message whose local copy is stored; a
// failure only loses the forward.
func (r *run) redirectAfterStore(ctx context.Context, raw []byte, d *sieve.Delivery) {
	user := r.req.Account

	_, err := r.redirect(ctx, raw, d.Redirects)

	switch {
	case errors.Is(err, outbound.ErrNoRelay):
		r.log.Warn("sieve redirect without relay, local copy kept", "user", user, "to", d.Redirects)
	case err != nil:
		r.log.Error("sieve redirect failed, local copy kept", "user", user, "err", err)
	}
}

// redirect forwards the unchanged message with the original envelope
// sender (RFC 5228 4.2). Loops are limited: no redirect to the recipient
// itself, no redirect of automatic mail, and at most maxRedirects targets.
func (r *run) redirect(ctx context.Context, raw []byte, targets []string) (bool, error) {
	user, rcpt := r.req.Account, r.req.Rcpt

	if !r.relayEnabled() {
		return false, outbound.ErrNoRelay
	}

	hdr, _ := textproto.NewReader(bufio.NewReader(bytes.NewReader(raw))).ReadMIMEHeader()
	if as := strings.ToLower(hdr.Get("Auto-Submitted")); as != "" && as != "no" {
		r.log.Info("not redirecting automatic mail", "user", user)
		return false, nil
	}
	// Loop protection across accounts: every redirect stamps the
	// recipient it left; a message that already passed this account (or
	// too many hops) is not forwarded again.
	if len(hdr.Values("Received")) > maxHops {
		r.log.Warn("not redirecting, too many hops", "user", user)
		return false, nil
	}

	seen, loop := redirectedFrom(hdr, user, rcpt)
	if loop {
		r.log.Warn("not redirecting, loop detected", "user", user)
		return false, nil
	}

	raw = append([]byte(redirectedFromHeader+": <"+rcpt+">\r\n"), raw...)

	to := redirectTargets(targets, seen)
	if len(to) == 0 {
		return false, nil
	}

	if err := r.svc.Relay.Send(ctx, r.req.From, to, raw); err != nil {
		return false, err
	}

	r.log.Info("redirected", "user", user, "to", to, "size", len(raw))

	return true, nil
}

// redirectedFrom collects the accounts the message was redirected from
// plus rcpt and user; loop is true when rcpt or user is among them.
func redirectedFrom(hdr textproto.MIMEHeader, user, rcpt string) (map[string]bool, bool) {
	seen := map[string]bool{strings.ToLower(rcpt): true, strings.ToLower(user): true}

	for _, v := range hdr.Values(redirectedFromHeader) {
		for _, a := range strings.Split(v, ",") {
			a = strings.ToLower(strings.Trim(strings.TrimSpace(a), "<>"))
			if a == strings.ToLower(rcpt) || a == strings.ToLower(user) {
				return nil, true
			}

			if a != "" {
				seen[a] = true
			}
		}
	}

	return seen, false
}

// redirectTargets returns up to maxRedirects distinct targets not in
// seen (seen is updated).
func redirectTargets(targets []string, seen map[string]bool) []string {
	var to []string

	for _, t := range targets {
		t = strings.Trim(strings.TrimSpace(t), "<>")

		lt := strings.ToLower(t)
		if t == "" || seen[lt] {
			continue
		}

		seen[lt] = true

		to = append(to, t)
		if len(to) == maxRedirects {
			break
		}
	}

	return to
}

// vacation sends the auto-reply once per sender and period.
func (r *run) vacation(ctx context.Context, raw []byte, v *sieve.Vacation) {
	u, user, rcpt, from := r.u, r.req.Account, r.req.Rcpt, r.req.From

	if !r.relayEnabled() {
		r.log.Warn("vacation without relay, no reply sent", "user", user)
		return
	}

	hdr, _ := textproto.NewReader(bufio.NewReader(bytes.NewReader(raw))).ReadMIMEHeader()

	ok, why := v.ShouldReply(hdr, from, rcpt)
	if !ok {
		r.log.Info("vacation suppressed", "user", user, "reason", why)
		return
	}

	key := v.VacationKey(from)
	// The period is reserved atomically before the send (two parallel
	// deliveries cannot both reply); a failed send releases it again.
	if already, err := u.VacationSeen(key, v.Period()); err != nil {
		r.log.Warn("vacation record failed", "user", user, "err", err)
		return
	} else if already {
		r.log.Info("vacation already sent within period", "user", user)
		return
	}

	to, reply := v.Compose(hdr, from, rcpt, r.now())
	if err := r.svc.Relay.Send(ctx, v.EnvelopeSender(rcpt), []string{to}, reply); err != nil {
		r.log.Error("vacation send failed", "user", user, "err", err)

		if _, rerr := u.VacationSeen(key, 0); rerr != nil {
			r.log.Warn("vacation reservation not released", "user", user, "err", rerr)
		}

		return
	}

	r.log.Info("vacation sent", "user", user, "to", to)
}

// notify sends the mailto notifications of the script over the relay.
func (r *run) notify(ctx context.Context, raw []byte, d *sieve.Delivery) {
	user := r.req.Account

	if len(d.Notifications) == 0 {
		return
	}

	if !r.relayEnabled() {
		r.log.Warn("notify without relay, no notification sent", "user", user)
		return
	}

	hdr, _ := textproto.NewReader(bufio.NewReader(bytes.NewReader(raw))).ReadMIMEHeader()
	for _, n := range d.Notifications {
		from, to, msg, ok := sieve.ComposeNotification(n, hdr, r.req.From, r.req.Rcpt, r.svc.SieveConfig, r.now())
		if !ok {
			continue
		}

		if err := r.svc.Relay.Send(ctx, from, to, msg); err != nil {
			r.log.Error("notification send failed", "user", user, "err", err)
			continue
		}

		r.log.Info("notification sent", "user", user, "to", to)
	}
}
