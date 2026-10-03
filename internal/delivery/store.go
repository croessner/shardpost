package delivery

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"shardpost/internal/metrics"
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

	// where are the targets stored into so far, by namespace and
	// mailbox: a fileinto redirected to INBOX (a mailbox being deleted,
	// stage B17) and a later keep are one copy, in either order.
	where := map[string]bool{}

	fail := func(err error) ([]Stored, error) {
		if r.rollback(done) {
			_ = r.endIntents(intents) // nothing left to roll back
		}

		return nil, err
	}

	for i, f := range files {
		if where[targetKey(targets[i])] {
			continue
		}

		t, meta, err := r.storeOne(&intents, targets[i], raw, f, where)
		if err != nil {
			return fail(err)
		}

		if meta == nil {
			continue // redirected to a mailbox that holds the message already
		}

		where[targetKey(t)] = true
		done = append(done, delivered{t, *meta})
		r.log.Info("delivered", "user", user, "from", r.req.From, "mailbox", t.where, "uid", meta.UID, "guid", meta.GUID, "size", meta.Size)
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

// targetKey identifies a target by namespace and mailbox.
func targetKey(t target) string { return t.where + "\x00" + t.mailbox }

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
// the message (or the target's own edited copy) there. It returns the
// target the message went to: a mailbox a mailbox_delete locked between
// the resolution and the store (stage B17) refuses the message, which
// then goes to INBOX like any failed fileinto, under a new GUID that the
// intent names for INBOX, so that a rollback after a crash finds the
// copy where it lies. A nil meta says INBOX holds the message already.
func (r *run) storeOne(intents *[]intent, t target, raw []byte, f sieve.FileInto, where map[string]bool) (target, *model.Meta, error) {
	if !t.public && !t.ensured {
		if err := t.u.EnsureMailbox(t.mailbox, nil); err != nil {
			if model.MailboxLockedError(err) {
				return r.redirectLocked(intents, t, raw, f, where)
			}

			return t, nil, err
		}
	}

	guid := model.NewGUID()
	if err := t.u.RecordDelivery(intentOf(*intents, t.u), t.mailbox, guid); err != nil {
		if model.MailboxLockedError(err) {
			return r.redirectLocked(intents, t, raw, f, where)
		}

		return t, nil, err
	}

	meta, err := r.deliverTo(t, guid, raw, f)
	if model.MailboxLockedError(err) {
		return r.redirectLocked(intents, t, raw, f, where)
	}

	return t, meta, err
}

// deliverTo stores the message into the target under guid.
func (r *run) deliverTo(t target, guid string, raw []byte, f sieve.FileInto) (*model.Meta, error) {
	body := f.Raw
	if body == nil {
		body = raw
	}

	meta, err := t.u.DeliverGUID(t.mailbox, guid, body, f.Flags, r.now())
	if errors.Is(err, model.ErrLimit) {
		// The flags of a Sieve addflag would take the mailbox beyond
		// its keyword limit: the message is stored without the
		// keywords rather than not at all.
		r.log.Warn("keywords dropped at the keyword limit", "mailbox", t.where, "err", err)

		meta, err = t.u.DeliverGUID(t.mailbox, guid, body, model.SystemFlags(f.Flags), r.now())
	}

	return meta, err
}

// redirectLocked stores a message whose target is being deleted into INBOX
// instead (implicit keep): with its own intent entry, in a namespace
// that gets an intent of its own if the delivery had none there, and
// not twice when INBOX holds the message already. A caller that named
// the mailbox itself gets the refusal: the mailbox is gone in seconds.
func (r *run) redirectLocked(intents *[]intent, from target, raw []byte, f sieve.FileInto, where map[string]bool) (target, *model.Meta, error) {
	if r.explicit {
		return from, nil, ErrNoMailbox
	}

	metrics.DeliveryRedirected.WithLabelValues(metrics.ReasonLocked).Inc()
	r.log.Warn("fileinto redirected to INBOX, the mailbox is being deleted", "user", r.req.Account, "mailbox", from.where)

	inbox := target{u: r.u, mailbox: inboxName, where: inboxName}
	if where[targetKey(inbox)] {
		return inbox, nil, nil
	}

	if len(*intents) > 0 && intentOf(*intents, r.u) == "" {
		id, err := r.u.BeginDelivery()
		if err != nil {
			return inbox, nil, err
		}

		*intents = append(*intents, intent{u: r.u, id: id})
	}

	guid := model.NewGUID()
	if err := r.u.RecordDelivery(intentOf(*intents, r.u), inboxName, guid); err != nil {
		return inbox, nil, err
	}

	meta, err := r.deliverTo(inbox, guid, raw, f)

	return inbox, meta, err
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
	// ensured: target() already created the mailbox when it was missing.
	ensured bool
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
		t.ensured, refused, err = r.creatable(t)
		if err != nil {
			return target{}, err
		}
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

// creatable creates the own mailbox a Sieve fileinto names when it is
// missing, which it is refused when the account is at one of its mailbox
// limits (limits.mailboxes, limits.mailbox_names): refused is then that
// limit, and the message goes to INBOX like any other failed fileinto
// instead of the whole delivery failing. ensured reports that the
// mailbox exists now, so storeOne does not ensure it a second time. A
// mailbox the caller named itself is never created (checkExplicit), nor
// a public folder.
func (r *run) creatable(t target) (ensured bool, refused, err error) {
	if t.public || t.mailbox == inboxName {
		return false, lockedTarget(t), nil
	}

	if r.explicit {
		return false, nil, nil
	}

	err = t.u.EnsureMailbox(t.mailbox, nil)

	switch {
	case errors.Is(err, model.ErrLimit):
		return false, err, nil
	case model.MailboxLockedError(err):
		// The mailbox, or one above it, is being deleted (stage B17):
		// the message goes to INBOX like any other failed fileinto.
		return false, err, nil
	case err != nil:
		return false, nil, err
	}

	return true, lockedTarget(t), nil
}

// lockedTarget reports model.ErrMailboxLocked when the target's record
// (from the lookup cache, no extra read) carries a live mailbox lock.
func lockedTarget(t target) error {
	if t.mailbox == inboxName {
		return nil
	}

	mb, err := t.u.Lookup(t.mailbox)
	if err != nil || !mb.Lock.Held("", time.Now()) {
		return nil
	}

	return fmt.Errorf("%w: %s", model.ErrMailboxLocked, t.where)
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
		if err := expungeOwn(d.target.u, d.target.mailbox, []model.Meta{d.meta}); err != nil {
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

// expungeOwn removes entries this delivery wrote itself: without the
// mailbox lock check where the store has one (a copy stored before a
// mailbox_delete locked the mailbox must still be rolled back, or the
// client would deliver it again).
func expungeOwn(u model.User, mailbox string, metas []model.Meta) error {
	if o, ok := u.(model.OwnExpunger); ok {
		return o.ExpungeOwn(mailbox, metas)
	}

	return u.Expunge(mailbox, metas)
}
