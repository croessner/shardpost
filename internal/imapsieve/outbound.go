package imapsieve

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"net/mail"
	"net/textproto"
	"shardpost/internal/model"
	"shardpost/internal/sieve"
	"strings"
	"time"
)

func (e *Engine) send(ctx context.Context, u model.User, ev Event, d *sieve.Delivery) error {
	if len(d.IMAPRedirects) == 0 && len(d.Notifications) == 0 {
		return nil
	}

	if e.Sender == nil {
		return fmt.Errorf("no relay")
	}
	// Triggering another owner's script cannot send mail with that owner's identity.
	if ev.Actor != ev.Owner {
		return fmt.Errorf("outbound requires mailbox owner")
	}

	owner, err := mail.ParseAddress(ev.Owner)
	if err != nil || owner.Address != ev.Owner {
		return fmt.Errorf("mailbox owner has no submission address")
	}

	if err := e.sendRedirects(ctx, owner.Address, d); err != nil {
		return err
	}

	return e.sendNotifications(ctx, u, owner.Address, d)
}

// sendRedirects relays each distinct redirect (at most four) from the
// owner, refusing loops and messages over the hop limit.
func (e *Engine) sendRedirects(ctx context.Context, owner string, d *sieve.Delivery) error {
	seen := map[string]bool{}
	for _, r := range d.IMAPRedirects {
		if seen[strings.ToLower(r.Address)] {
			continue
		}

		seen[strings.ToLower(r.Address)] = true
		if len(seen) > 4 {
			return fmt.Errorf("redirect limit")
		}

		if err := checkRedirect(r.Address, r.Raw, owner); err != nil {
			return err
		}

		raw := append([]byte("X-Sieve-Redirected-From: <"+owner+">\r\n"), r.Raw...)
		if err := e.Sender.Send(ctx, owner, []string{r.Address}, raw); err != nil {
			return err
		}
	}

	return nil
}

// checkRedirect refuses a redirect back to the owner, over the hop
// limits or already redirected by the owner.
func checkRedirect(address string, msg []byte, owner string) error {
	if strings.EqualFold(address, owner) {
		return fmt.Errorf("redirect loop")
	}

	h, _ := textproto.NewReader(bufio.NewReader(bytes.NewReader(msg))).ReadMIMEHeader()
	if len(h.Values("Received")) >= 25 || len(h.Values("X-Sieve-Redirected-From")) >= 4 {
		return fmt.Errorf("redirect hop limit")
	}

	for _, v := range h.Values("X-Sieve-Redirected-From") {
		if strings.Contains(strings.ToLower(v), strings.ToLower(owner)) {
			return fmt.Errorf("redirect loop")
		}
	}

	return nil
}

// sendNotifications relays the composed notify messages and stores the
// :fcc copy of each one that went out.
func (e *Engine) sendNotifications(ctx context.Context, u model.User, owner string, d *sieve.Delivery) error {
	for _, n := range d.Notifications {
		h, _ := textproto.NewReader(bufio.NewReader(bytes.NewReader(n.Raw))).ReadMIMEHeader()

		from, to, raw, ok := sieve.ComposeNotification(n, h, "", owner, e.SieveConfig, time.Now())
		if !ok {
			continue
		}

		if err := e.Sender.Send(ctx, from, to, raw); err != nil {
			return err
		}

		e.fcc(u, owner, n.Fcc, raw)
	}

	return nil
}

// fcc stores a copy of a sent notification (RFC 8580) in the owner's
// mailbox. As for fileinto in this context the mailbox must exist
// (:create does not create it) and lie in the owner's namespace. A
// failure is logged; the notification went out, so the run goes on.
func (e *Engine) fcc(u model.User, owner string, f *sieve.Fcc, raw []byte) {
	if f == nil || u == nil {
		return
	}

	name, err := specialUseTarget(u, sieve.FileInto{Mailbox: f.Mailbox, SpecialUse: f.SpecialUse})
	if err == nil && e.SharedPrefix != "" && strings.HasPrefix(name, e.SharedPrefix) {
		err = fmt.Errorf("cross-owner fcc unavailable")
	}

	if err == nil {
		_, err = u.Lookup(name)
	}

	if err == nil {
		_, err = u.Deliver(name, raw, f.Flags, time.Now())
	}

	if err != nil && e.Log != nil {
		e.Log.Warn("imapsieve fcc copy not stored", "user", owner, "mailbox", name, "err", err)
	}
}
