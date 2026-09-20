package delivery

import (
	"fmt"
	"strings"

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
		meta, where, err := r.storeOne(intent, raw, f)
		if err != nil {
			return fail(err)
		}

		done = append(done, delivered{where, *meta})
		r.log.Info("delivered", "user", user, "from", r.req.From, "mailbox", where, "uid", meta.UID, "guid", meta.GUID, "size", meta.Size)
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
// the target's own edited copy) there. It returns where the message
// actually landed, which differs from the requested mailbox when a
// public folder was refused.
func (r *run) storeOne(intent string, raw []byte, f sieve.FileInto) (*model.Meta, string, error) {
	u, name, pub, err := r.target(f.Mailbox)
	if err != nil {
		return nil, "", err
	}

	where := f.Mailbox
	f.Mailbox = name

	if !pub {
		where = name

		if err := u.EnsureMailbox(f.Mailbox, nil); err != nil {
			return nil, "", err
		}
	}

	guid := model.NewGUID()
	if err := r.u.RecordDelivery(intent, f.Mailbox, guid); err != nil {
		return nil, "", err
	}

	body := f.Raw
	if body == nil {
		body = raw
	}

	meta, err := u.DeliverGUID(f.Mailbox, guid, body, f.Flags, r.now())

	return meta, where, err
}

// target resolves a fileinto destination. A name under the public
// prefix goes to the public namespace, which needs the post right (p)
// on that folder and an existing folder: a delivery does not create
// public folders. Without the right the message falls back to INBOX,
// the way a failed fileinto does.
func (r *run) target(name string) (u model.User, mailbox string, public bool, err error) {
	prefix := r.svc.PublicPrefix
	if prefix == "" || !strings.HasPrefix(name, prefix) {
		return r.u, name, false, nil
	}

	folder := strings.TrimPrefix(name, prefix)

	pub, err := r.svc.Store.User(model.PublicNamespace)
	if err != nil {
		return nil, "", false, err
	}

	acl, err := pub.GetACL(folder)
	if err != nil || !model.HasRight(model.EffectiveRights(acl, r.req.Account), 'p') {
		pub.Close()
		r.log.Warn("sieve fileinto into a public folder refused", "mailbox", name, "reason", reasonOf(err))

		return r.u, inboxName, false, nil
	}

	r.opened = append(r.opened, pub)

	return pub, folder, true, nil
}

// reasonOf names why a public fileinto was refused.
func reasonOf(err error) string {
	if err != nil {
		return err.Error()
	}

	return "no post right"
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
