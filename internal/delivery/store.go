package delivery

import (
	"fmt"

	"shardpost/internal/model"
	"shardpost/internal/sieve"
)

// store files the message into every target of d.
//
// Several fileinto targets are delivered one after another; if a later
// one fails the earlier ones are removed again, so a retry does not
// create duplicates. A delivery intent names each message before it is
// stored, so the scrubber can roll back after a crash at any point; the
// intent must be gone before the caller hears of success, otherwise a
// later rollback would delete confirmed mail.
func (r *run) store(raw []byte, d *sieve.Delivery) ([]Stored, error) {
	u, user := r.u, r.req.Account

	var done []delivered

	intent := ""

	if len(d.FileInto) > 1 {
		id, err := u.BeginDelivery()
		if err != nil {
			return nil, err
		}

		intent = id
	}

	fail := func(err error) ([]Stored, error) {
		if r.rollback(done) {
			_ = u.EndDelivery(intent) // nothing left to roll back
		}

		return nil, err
	}
	for _, f := range d.FileInto {
		meta, err := r.storeOne(intent, raw, f)
		if err != nil {
			return fail(err)
		}

		done = append(done, delivered{f.Mailbox, *meta})
		r.log.Info("delivered", "user", user, "from", r.req.From, "mailbox", f.Mailbox, "uid", meta.UID, "guid", meta.GUID, "size", meta.Size)
	}

	if err := u.EndDelivery(intent); err != nil {
		r.log.Error("delivery intent removal failed, rolling back", "user", user, "err", err)
		return fail(fmt.Errorf("delivery intent removal: %w", err))
	}

	stored := make([]Stored, 0, len(done))
	for _, d := range done {
		stored = append(stored, Stored{Mailbox: d.mailbox, UID: d.meta.UID, GUID: d.meta.GUID, Size: d.meta.Size})
	}

	return stored, nil
}

// storeOne records the target in the intent and stores the message (or
// the target's own edited copy) there.
func (r *run) storeOne(intent string, raw []byte, f sieve.FileInto) (*model.Meta, error) {
	u := r.u
	if err := u.EnsureMailbox(f.Mailbox, nil); err != nil {
		return nil, err
	}

	guid := model.NewGUID()
	if err := u.RecordDelivery(intent, f.Mailbox, guid); err != nil {
		return nil, err
	}

	body := f.Raw
	if body == nil {
		body = raw
	}

	return u.DeliverGUID(f.Mailbox, guid, body, f.Flags, r.now())
}

// delivered is one completed fileinto of a multi-target delivery.
type delivered struct {
	mailbox string
	meta    model.Meta
}

// rollback removes what a failed multi-target delivery stored; ok is
// false when something could not be removed (the intent then stays for
// the scrubber).
func (r *run) rollback(done []delivered) bool {
	ok := true

	for _, d := range done {
		if err := r.u.Expunge(d.mailbox, []model.Meta{d.meta}); err != nil {
			r.log.Error("delivery rollback failed", "mailbox", d.mailbox, "uid", d.meta.UID, "err", err)

			ok = false
		}
	}

	return ok
}

// track records the duplicate-test keys of a finished delivery.
func (r *run) track(d *sieve.Delivery) {
	for _, t := range d.Tracked {
		if _, err := r.u.VacationSeen("dup:"+t.Key, t.TTL); err != nil {
			r.log.Warn("duplicate record failed", "user", r.req.Account, "err", err)
		}
	}
}
