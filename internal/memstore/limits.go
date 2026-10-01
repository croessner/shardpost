package memstore

import "shardpost/internal/model"

// SetLimits sets the per-account limits every user of the store is held
// to, as cephstore.Config.Limits does for the RADOS store. Call it before
// the store is used.
func (s *Store) SetLimits(l model.Limits) { s.limits = l }

// checkNewMailbox refuses a mailbox named name (which does not exist)
// at the mailbox or the name limit, as cephstore does: INBOX is never
// refused, and a name used before does not count again. u.mu is held.
func (u *User) checkNewMailbox(name string) error {
	l := u.store.limits

	if name == inboxName {
		return nil
	}

	if l.Mailboxes > 0 && len(u.boxes) >= l.Mailboxes {
		return model.LimitReached(model.LimitMailboxes, l.Mailboxes)
	}

	if l.MailboxNames > 0 && !u.usedNames[name] && len(u.usedNames) >= l.MailboxNames {
		return model.LimitReached(model.LimitMailboxNames, l.MailboxNames)
	}

	return nil
}

// useName remembers that a mailbox was created under name (the "uv:"
// record of the RADOS registry). u.mu is held.
func (u *User) useName(name string) {
	if u.usedNames == nil {
		u.usedNames = map[string]bool{}
	}

	u.usedNames[name] = true
}

// admit checks a delivery of size bytes with flags into b: the storage
// limit (with enforce) and the keyword limit. u.mu is held.
func (u *User) admit(b *box, size int, flags []string, enforce bool) error {
	if enforce && u.limit.Bytes > 0 && u.usage.Bytes+int64(size) > u.limit.Bytes {
		return model.ErrOverQuota
	}

	return u.checkKeywords(b, flags)
}

// checkKeywords refuses flags that would take b beyond the keyword
// limit. u.mu is held.
func (u *User) checkKeywords(b *box, flags []string) error {
	return model.CheckKeywords(b.keywords, flags, u.store.limits.KeywordsPerMailbox)
}

// checkMetadata refuses a change that adds entries beyond the limit:
// set and rm are the full keys it writes and removes, entries the entry
// name of each key in set (server-managed entries are never refused).
// u.mu is held.
func (u *User) checkMetadata(set map[string]string, rm []string, entries map[string]string) error {
	limit := u.store.limits.MetadataEntries
	if limit <= 0 {
		return nil
	}

	added, user := 0, false

	for k := range set {
		if _, ok := u.annot[k]; ok {
			continue
		}

		added++
		user = user || !model.ServerManagedEntry(entries[k])
	}

	if !user {
		return nil
	}

	n := len(u.annot)

	for _, k := range rm {
		if _, ok := u.annot[k]; ok {
			n--
		}
	}

	if n+added > limit {
		return model.LimitReached(model.LimitMetadata, limit)
	}

	return nil
}

// checkGrant refuses an ACL change on the mailbox name that would give
// identifier a pointer beyond the shared pointer limit; grant computes
// the new rights from the old ones. As in cephstore it runs before the
// change. u.mu is not held.
func (u *User) checkGrant(name, identifier string, grant func(old string) string) error {
	if u.store.limits.SharedPointers <= 0 {
		return nil
	}

	u.mu.Lock()

	b, canon, err := u.lookup(name)
	if err != nil {
		u.mu.Unlock()
		return nil // the change itself reports it
	}

	old := b.ACL[identifier]
	u.mu.Unlock()

	return u.checkShared(canon, identifier, old, grant(old))
}

// checkShared refuses a grant on the mailbox canon that gives identifier
// a pointer beyond the limit (old and rights are its rights before and
// after the change). u.mu is not held.
func (u *User) checkShared(canon, identifier, old, rights string) error {
	limit := u.store.limits.SharedPointers
	if limit <= 0 || old != "" || rights == "" || identifier == identAnyone || model.IsPublic(u.name) {
		return nil
	}

	g := u.store.user(identifier)

	g.mu.Lock()
	defer g.mu.Unlock()

	if _, ok := g.shared[u.name+"/"+canon]; ok {
		return nil
	}

	if len(g.shared) >= limit {
		return model.LimitReached(model.LimitSharedPointers, limit)
	}

	return nil
}
