package delivery

import (
	"errors"
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
//
// A message filed into a public folder lies in the public namespace, so
// its intent is kept there as well: an intent, like a rollback, only
// finds a message in the namespace that holds it.
func (r *run) store(raw []byte, d *sieve.Delivery) ([]Stored, error) {
	user := r.req.Account

	targets, files, err := r.resolve(d.FileInto)
	if err != nil {
		return nil, err
	}

	intents, err := r.beginIntents(targets, len(targets) > 1)
	if err != nil {
		return nil, err
	}

	var done []delivered

	fail := func(err error) ([]Stored, error) {
		if r.rollback(done) {
			_ = r.endIntents(intents) // nothing left to roll back
		}

		return nil, err
	}

	for i, f := range files {
		meta, err := r.storeOne(intents, targets[i], raw, f)
		if err != nil {
			return fail(err)
		}

		done = append(done, delivered{targets[i], *meta})
		r.log.Info("delivered", "user", user, "from", r.req.From, "mailbox", targets[i].where, "uid", meta.UID, "guid", meta.GUID, "size", meta.Size)
	}

	if err := r.endIntents(intents); err != nil {
		r.log.Error("delivery intent removal failed, rolling back", "user", user, "err", err)
		return fail(fmt.Errorf("delivery intent removal: %w", err))
	}

	stored := make([]Stored, 0, len(done))
	for _, d := range done {
		stored = append(stored, Stored{Mailbox: d.target.where, UID: d.meta.UID, GUID: d.meta.GUID, Size: d.meta.Size})
	}

	return stored, nil
}

// resolve turns the fileinto list into targets, dropping a target that
// another one already names. Sieve deduplicates by name before the
// names are resolved (RFC 5232 5.1), so two names that end in the same
// mailbox — a refused public folder and a keep, for instance — would
// otherwise store the message twice. The last one wins, as it does
// there.
func (r *run) resolve(files []sieve.FileInto) ([]target, []sieve.FileInto, error) {
	var (
		targets []target
		kept    []sieve.FileInto
	)

	at := map[string]int{} // "<namespace>\x00<mailbox>" -> index

	for _, f := range files {
		t, err := r.target(f.Mailbox)
		if err != nil {
			return nil, nil, err
		}

		key := t.where + "\x00" + t.mailbox
		if i, ok := at[key]; ok {
			targets[i], kept[i] = t, f

			continue
		}

		at[key] = len(targets)
		targets = append(targets, t)
		kept = append(kept, f)
	}

	return targets, kept, nil
}

// canonicalMailbox folds the name the way the store does, so that two
// spellings of the same mailbox are one target: INBOX is
// case-insensitive, and so is the INBOX part of a name below it.
func canonicalMailbox(name string) string {
	if strings.EqualFold(name, inboxName) {
		return inboxName
	}

	if i := strings.Index(name, "/"); i == len(inboxName) && strings.EqualFold(name[:i], inboxName) {
		return inboxName + name[i:]
	}

	return name
}

// intent is a delivery intent in one namespace.
type intent struct {
	u  model.User
	id string
}

// beginIntents opens one intent per namespace the delivery files into.
// A single target needs none: it either lands or it does not.
func (r *run) beginIntents(targets []target, multi bool) ([]intent, error) {
	if !multi {
		return nil, nil
	}

	var intents []intent

	for _, t := range targets {
		if intentOf(intents, t.u) != "" {
			continue
		}

		id, err := t.u.BeginDelivery()
		if err != nil {
			_ = r.endIntents(intents)

			return nil, err
		}

		intents = append(intents, intent{u: t.u, id: id})
	}

	return intents, nil
}

// intentOf returns the intent of a namespace, empty when there is none.
func intentOf(intents []intent, u model.User) string {
	for _, in := range intents {
		if in.u == u {
			return in.id
		}
	}

	return ""
}

// endIntents removes the intents; the first failure is returned, the
// others are still attempted.
func (r *run) endIntents(intents []intent) error {
	var first error

	for _, in := range intents {
		if err := in.u.EndDelivery(in.id); err != nil && first == nil {
			first = err
		}
	}

	return first
}

// storeOne records the target in the intent of its namespace and stores
// the message (or the target's own edited copy) there.
func (r *run) storeOne(intents []intent, t target, raw []byte, f sieve.FileInto) (*model.Meta, error) {
	if !t.public {
		if err := t.u.EnsureMailbox(t.mailbox, nil); err != nil {
			return nil, err
		}
	}

	guid := model.NewGUID()
	if err := t.u.RecordDelivery(intentOf(intents, t.u), t.mailbox, guid); err != nil {
		return nil, err
	}

	body := f.Raw
	if body == nil {
		body = raw
	}

	return t.u.DeliverGUID(t.mailbox, guid, body, f.Flags, r.now())
}

// target is a resolved fileinto destination: the namespace that stores
// the message, the mailbox name inside it and the name everyone else
// uses for it (a public folder keeps its client-facing name in reports
// and in the Sieve result).
type target struct {
	u       model.User
	mailbox string
	where   string
	public  bool
}

// target resolves a fileinto destination. A name under the public
// prefix goes to the public namespace, which needs the post right (p)
// on that folder and an existing folder: a delivery does not create
// public folders. Without the right the message falls back to INBOX,
// the way a failed fileinto does.
func (r *run) target(name string) (target, error) {
	t, refused, err := r.resolveTarget(name)
	if err != nil {
		return target{}, err
	}

	if refused == nil {
		return t, nil
	}

	// A caller that named the mailbox itself gets the refusal; a Sieve
	// script gets INBOX, the way a failed fileinto does.
	if r.explicit {
		return target{}, refused
	}

	r.log.Warn("fileinto refused", "mailbox", name, "reason", refused.Error())

	return target{u: r.u, mailbox: inboxName, where: inboxName}, nil
}

// resolveTarget resolves a name without any fallback. refused says why
// the target cannot be used, nil when it can: a mailbox that is not
// there (ErrNoMailbox) or one this account may not post into
// (ErrNoPostRight).
func (r *run) resolveTarget(name string) (t target, refused, err error) {
	if p := r.svc.SharedPrefix; p != "" && strings.HasPrefix(name, p) {
		// A delivery is for one account, and this name belongs to
		// another one. Storing it here would create a mailbox of the
		// recipient that no client can open.
		return target{}, fmt.Errorf("%w: a mailbox of another account is no delivery target", ErrNoMailbox), nil
	}

	prefix := r.svc.PublicPrefix
	if prefix == "" || !strings.HasPrefix(name, prefix) {
		return r.ownTarget(name), nil, nil
	}

	folder := strings.TrimPrefix(name, prefix)

	pub, err := r.publicUser()
	if err != nil {
		return target{}, nil, err
	}

	acl, err := pub.GetACL(folder)

	switch {
	case errors.Is(err, model.ErrNoMailbox):
		return target{}, fmt.Errorf("%w: %s", ErrNoMailbox, name), nil
	case err != nil:
		return target{}, nil, err
	case !model.HasRight(model.EffectiveRights(acl, r.req.Account), 'p'):
		return target{}, fmt.Errorf("%w: no post right on %s", ErrNoPostRight, name), nil
	}

	return target{u: pub, mailbox: folder, where: name, public: true}, nil, nil
}

// ownTarget resolves a name in the recipient's own namespace, folded as
// the store folds it, so that two spellings of the same mailbox are one
// target and the result reports one name.
func (r *run) ownTarget(name string) target {
	name = canonicalMailbox(name)

	return target{u: r.u, mailbox: name, where: name}
}

// publicUser opens the public namespace once per delivery; it is closed
// with the delivery.
func (r *run) publicUser() (model.User, error) {
	if r.pub != nil {
		return r.pub, nil
	}

	pub, err := r.svc.Store.User(model.PublicNamespace)
	if err != nil {
		return nil, err
	}

	r.pub = pub
	r.opened = append(r.opened, pub)

	return pub, nil
}

// delivered is one completed fileinto of a multi-target delivery.
type delivered struct {
	target target
	meta   model.Meta
}

// rollback removes what a failed multi-target delivery stored; ok is
// false when something could not be removed (the intent then stays for
// the scrubber). Every message is removed where it lies, which for a
// public folder is not the recipient's namespace.
func (r *run) rollback(done []delivered) bool {
	ok := true

	for _, d := range done {
		if err := d.target.u.Expunge(d.target.mailbox, []model.Meta{d.meta}); err != nil {
			r.log.Error("delivery rollback failed", "mailbox", d.target.where, "uid", d.meta.UID, "err", err)

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
