package delivery

import (
	"context"
	"errors"
	"strings"

	"shardpost/internal/extprograms"
	"shardpost/internal/model"
	"shardpost/internal/sieve"
)

// runSieve runs the active script with its pipes; a missing script
// delivers to INBOX, a failing script or pipe keeps the original in
// INBOX. When the active script did not answer a message it keeps with
// a vacation, the vacation of the JMAP VacationResponse may.
func (r *run) runSieve(ctx context.Context) *sieve.Delivery {
	user, raw := r.req.Account, r.req.Raw

	script, err := r.u.ActiveScript()
	if err != nil {
		r.log.Warn("sieve script unavailable, delivering to INBOX", "user", user, "err", err)

		script = ""
	}

	hooks := r.sieveHooks()

	d, err := sieve.RunWithHooks(ctx, script, r.req.From, r.req.Rcpt, user, raw, hooks)
	if err != nil {
		r.log.Warn("sieve failed, delivering to INBOX", "user", user, "err", err)

		d = &sieve.Delivery{FileInto: []sieve.FileInto{{Mailbox: inboxName}}, Raw: raw}
	}

	if err := d.RunPipes(ctx, hooks.Program); err != nil {
		r.log.Warn("sieve pipe failed, keeping original in INBOX")

		d = &sieve.Delivery{FileInto: []sieve.FileInto{{Mailbox: inboxName}}, Raw: raw}
	}

	// A message that is not kept is not answered: that is how every Sieve
	// vacation behaves here (the reply goes out after the message is
	// stored), so there is nothing to evaluate.
	if d.Vacation == nil && d.Rejected == "" && len(d.FileInto) > 0 {
		d.Vacation = r.jmapVacation(ctx)
	}

	return d
}

// jmapVacation evaluates the account's JMAP VacationResponse and returns
// the vacation it asks for. Its source is the account's METADATA entry,
// which only the JMAP front-end writes; the script is generated from it
// here, and the copy in the script store — for display in ManageSieve
// clients — is never read. It runs after the active script, so a
// vacation the user keeps there wins, and a second vacation in one run —
// which Sieve forbids — cannot happen; it runs without store hooks or
// programs. A setting that is missing, unreadable or does not run leaves
// the delivery as it is.
func (r *run) jmapVacation(ctx context.Context) *sieve.Vacation {
	user := r.req.Account

	md, err := r.u.GetMetadata("", user)
	if err != nil {
		r.log.Warn("jmap vacation unavailable", "user", user, "err", err)

		return nil
	}

	raw := md[sieve.JMAPVacationEntry]
	if raw == "" {
		return nil
	}

	v, err := sieve.ParseJMAPVacation(raw)
	if err != nil || !v.IsEnabled {
		if err != nil {
			r.log.Warn("jmap vacation does not read", "user", user, "err", err)
		}

		return nil
	}

	settings, err := v.Settings()
	if err != nil {
		r.log.Warn("jmap vacation has a bad date", "user", user, "err", err)

		return nil
	}

	d, err := sieve.RunWithHooks(ctx, sieve.VacationScript(settings), r.req.From, r.req.Rcpt, user, r.req.Raw,
		sieve.Hooks{Config: r.svc.SieveConfig})
	if err != nil {
		r.log.Warn("jmap vacation failed", "user", user, "err", err)

		return nil
	}

	return d.Vacation
}

// sieveHooks connects the script to the user's store and, when enabled,
// to the external programs.
func (r *run) sieveHooks() sieve.Hooks {
	hooks := r.svc.StoreHooks(r.u, r.req.Account)
	if r.svc.Programs != nil && r.svc.Programs.Enabled() {
		hooks.Program = r.svc.Programs.SieveHook(extprograms.Context{Account: r.req.Account, Cause: "DELIVERY"})
	}

	return hooks
}

// StoreHooks connect a script to the user's store: mailbox, metadata and
// special-use tests, the duplicate query (nothing is recorded) and
// includes. External programs are not connected.
func (s *Service) StoreHooks(u model.User, user string) sieve.Hooks {
	return sieve.Hooks{
		MailboxExists: func(name string) (bool, error) {
			_, err := u.Lookup(name)
			if errors.Is(err, model.ErrNoMailbox) {
				return false, nil
			}

			return err == nil, err
		},
		Duplicate: func(key string) (bool, error) {
			return u.VacationSeen("dup:"+key, -1)
		},
		Metadata: func(mailbox, annotation string) (string, bool, error) {
			return metadataEntry(u, user, mailbox, annotation)
		},
		SpecialUseExists: func(mailbox string, uses []string) (bool, error) {
			return specialUseExists(u, mailbox, uses)
		},
		Include: sieve.IncludeLoader(u, s.GlobalScripts),
		Config:  s.SieveConfig,
	}
}

// metadataEntry backs mboxmetadata/servermetadata; a missing mailbox is
// "not found", not an error.
func metadataEntry(u model.User, user, mailbox, annotation string) (string, bool, error) {
	entries, err := u.GetMetadata(mailbox, user)
	if err != nil {
		if errors.Is(err, model.ErrNoMailbox) {
			return "", false, nil
		}

		return "", false, err
	}

	v, ok := entries[annotation]

	return v, ok, nil
}

// resolveSpecialUse replaces the mailbox of fileinto :specialuse targets
// with the user's mailbox carrying that use, if there is one.
func resolveSpecialUse(u model.User, d *sieve.Delivery) {
	for i, f := range d.FileInto {
		if f.SpecialUse != "" {
			if name, ok := mailboxWithUse(u, f.SpecialUse); ok {
				d.FileInto[i].Mailbox = name
			}
		}
	}
}

// mailboxWithUse finds the user's mailbox with the special use (\Junk ...).
func mailboxWithUse(u model.User, use string) (string, bool) {
	boxes, err := u.ListMailboxes()
	if err != nil {
		return "", false
	}

	for _, b := range boxes {
		for _, su := range b.SpecialUse {
			if strings.EqualFold(su, use) {
				return b.Name, true
			}
		}
	}

	return "", false
}

// specialUseExists is the specialuse_exists test: with a mailbox name,
// that mailbox must carry every use; without, any mailbox may.
func specialUseExists(u model.User, mailbox string, uses []string) (bool, error) {
	boxes, err := u.ListMailboxes()
	if err != nil {
		return false, err
	}

	has := func(b model.Mailbox, use string) bool {
		for _, su := range b.SpecialUse {
			if strings.EqualFold(su, use) {
				return true
			}
		}

		return false
	}
	for _, use := range uses {
		found := false

		for _, b := range boxes {
			if (mailbox == "" || strings.EqualFold(b.Name, mailbox)) && has(b, use) {
				found = true
				break
			}
		}

		if !found {
			return false, nil
		}
	}

	return true, nil
}
