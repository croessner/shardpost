package memstore

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"shardpost/internal/model"
	"shardpost/internal/quota"
)

// The online quota recount (stage B16) and the mailbox lock of
// mailbox_delete (stage B17) in the in-memory store: everything happens
// under the user's mutex, so a recount is one atomic count-and-set and
// needs no captures; the locks live in the mailbox records as in
// cephstore, and the same operations refuse a locked mailbox.

var (
	_ model.QuotaOnlineRecounter = (*User)(nil)
	_ model.MailboxLocker        = (*User)(nil)
	_ model.MailboxLockReader    = (*User)(nil)
	_ model.OwnExpunger          = (*User)(nil)
)

// SetQuotaRules sets the mailbox rules of the quota (cephstore.Config
// .QuotaRules): a mailbox a rule ignores counts nowhere.
func (s *Store) SetQuotaRules(p *quota.Policy) { s.rules = p }

// counts reports whether the messages of the mailbox name count against
// the quota. u.mu is held.
func (u *User) counts(name string, b *box) bool {
	if u.store.rules == nil {
		return true
	}

	return u.store.rules.Counts(&model.Mailbox{Name: name, MailboxRec: b.rec()})
}

// recount is the usage as the index says (u.mu held).
func (u *User) recount() model.Quota {
	var counted model.Quota

	for n, b := range u.boxes {
		if !u.counts(n, b) {
			continue
		}

		for _, m := range b.msgs {
			counted.Bytes += m.Size
			counted.Msgs++
		}
	}

	return counted
}

// RecountQuota implements model.QuotaRecounter, with the quota rules as
// cephstore applies them.
func (u *User) RecountQuota(apply bool) (model.Quota, model.Quota, error) {
	u.mu.Lock()
	defer u.mu.Unlock()

	counted, stored := u.recount(), u.usage
	if apply {
		u.usage = counted
	}

	return counted, stored, nil
}

// RecountQuotaOnline implements model.QuotaOnlineRecounter: under the
// mutex the count and the switch are one step, so the result is what
// cephstore's run arrives at.
func (u *User) RecountQuotaOnline(req model.QuotaRecountRequest) (model.QuotaRecountReport, error) {
	rep := model.QuotaRecountReport{Mode: model.QuotaRecountOnline}

	if req.Tok == "" {
		return rep, errors.New("online recount: token required")
	}

	if req.Fence != nil {
		if err := req.Fence(); err != nil {
			return rep, err
		}
	}

	u.mu.Lock()
	defer u.mu.Unlock()

	if u.imp != nil && u.imp.expires.After(time.Now()) {
		return rep, &model.QuotaRecountBusyError{Reason: model.BusyImportLock, Key: u.imp.session}
	}

	for session, l := range u.mlocks {
		if l.alive() && l.phase != model.MailboxLockDestroying {
			return rep, &model.QuotaRecountBusyError{Reason: model.BusyMailboxLck, Key: session}
		}
	}

	rep.Counted, rep.Stored = u.recount(), u.usage
	rep.Differs = rep.Counted != rep.Stored
	rep.ListedMailboxes = len(u.boxes)
	rep.Result = model.QuotaRecountOK
	u.usage = rep.Counted

	if req.Progress != nil {
		req.Progress("switch", 1, 1)
	}

	return rep, nil
}

// RenewQuotaRecount implements model.QuotaOnlineRecounter (nothing to
// renew: the run is one step).
func (u *User) RenewQuotaRecount(string, time.Duration) error { return nil }

// ClearQuotaRecount implements model.QuotaOnlineRecounter (nothing is
// captured).
func (u *User) ClearQuotaRecount(string) error { return nil }

// memLock is the account-wide record of a session's mailbox locks.
type memLock struct {
	ids, names []string
	exp        time.Time
	phase      string
}

func (l *memLock) alive() bool { return time.Now().Before(l.exp) }

// locked reports whether b is locked by a session other than session.
func lockedBox(b *box, session string) bool { return b.Lock.Held(session, time.Now()) }

// lockedAbove reports whether an ancestor of name is locked. u.mu is held.
func (u *User) lockedAbove(name string) bool {
	for i := 0; i < len(name); i++ {
		if name[i] != '/' {
			continue
		}

		if b, ok := u.boxes[name[:i]]; ok && lockedBox(b, "") {
			return true
		}
	}

	return false
}

// LockMailboxes implements model.MailboxLocker.
func (u *User) LockMailboxes(session string, targets []model.MailboxLockTarget, ttl time.Duration) error {
	if session == "" || len(targets) == 0 || ttl <= 0 {
		return errors.New("mailbox lock: session, targets and ttl required")
	}

	u.mu.Lock()
	defer u.mu.Unlock()

	now := time.Now()

	if u.imp != nil && u.imp.session != session && u.imp.expires.After(now) {
		return model.ErrImportLocked
	}

	if err := u.lockable(targets, session); err != nil {
		return err
	}

	if u.mlocks == nil {
		u.mlocks = map[string]*memLock{}
	}

	l := &memLock{exp: now.Add(ttl), phase: model.MailboxLockLocked}
	if old, ok := u.mlocks[session]; ok {
		l.phase, l.ids, l.names = old.phase, old.ids, old.names // a resumed run locks again
	}

	for _, t := range targets {
		u.boxes[t.Name].Lock = &model.MailboxLock{Session: session, Expires: l.exp.Unix()}

		if !slices.Contains(l.ids, t.ID) {
			l.ids, l.names = append(l.ids, t.ID), append(l.names, t.Name)
		}
	}

	u.mlocks[session] = l

	return nil
}

// lockable checks that every target is registered under its id and not
// locked by another session. u.mu is held.
func (u *User) lockable(targets []model.MailboxLockTarget, session string) error {
	for _, t := range targets {
		b, ok := u.boxes[t.Name]
		if !ok || b.ID != t.ID {
			return fmt.Errorf("%w: %q", model.ErrNoMailbox, t.Name)
		}

		if lockedBox(b, session) {
			return model.ErrMailboxLocked
		}
	}

	return nil
}

// RenewMailboxLocks implements model.MailboxLocker.
func (u *User) RenewMailboxLocks(session string, ttl time.Duration) error {
	u.mu.Lock()
	defer u.mu.Unlock()

	l, ok := u.mlocks[session]
	if !ok {
		return fmt.Errorf("mailbox lock of %s: %w", session, model.ErrLocked)
	}

	l.exp = time.Now().Add(ttl)

	for _, n := range l.names {
		if b, ok := u.boxes[n]; ok && b.Lock != nil && b.Lock.Session == session {
			b.Lock.Expires = l.exp.Unix()
		}
	}

	return nil
}

// SetMailboxLockPhase implements model.MailboxLocker.
func (u *User) SetMailboxLockPhase(session, phase string) error {
	u.mu.Lock()
	defer u.mu.Unlock()

	l, ok := u.mlocks[session]
	if !ok {
		return fmt.Errorf("mailbox lock of %s: %w", session, model.ErrLocked)
	}

	l.phase = phase

	return nil
}

// UnlockMailboxes implements model.MailboxLocker.
func (u *User) UnlockMailboxes(session string) error {
	u.mu.Lock()
	defer u.mu.Unlock()

	l, ok := u.mlocks[session]
	if !ok {
		return nil
	}

	for _, n := range l.names {
		if b, ok := u.boxes[n]; ok && b.Lock != nil && b.Lock.Session == session {
			b.Lock = nil
		}
	}

	delete(u.mlocks, session)

	return nil
}

// MailboxLockBarrier implements model.MailboxLocker: this store has no
// operations in flight.
func (u *User) MailboxLockBarrier([]model.MailboxLockTarget, time.Time, time.Duration) (string, string, error) {
	return "", "", nil
}

// UnregisterForDelete implements model.MailboxLocker: the mailbox goes
// at once (there is nothing to destroy later).
func (u *User) UnregisterForDelete(id, session string) (bool, error) {
	u.mu.Lock()

	var name string

	for n, b := range u.boxes {
		if b.ID == id {
			name = n
		}
	}

	if name == "" {
		u.mu.Unlock()
		return false, nil
	}

	b := u.boxes[name]

	switch {
	case session == "" && lockedBox(b, ""):
		u.mu.Unlock()
		return false, model.ErrMailboxLocked
	case session != "" && (b.Lock == nil || b.Lock.Session != session):
		u.mu.Unlock()
		return false, fmt.Errorf("%w: the mailbox lock of %s is gone", model.ErrLocked, session)
	}

	if l, ok := u.mlocks[session]; ok {
		l.phase = model.MailboxLockUnregistering
	}

	b.Lock = nil // the session's own lock goes with the record
	u.mu.Unlock()

	err := u.DeleteMailbox(name)

	return err == nil, err
}

// FinishDeletion implements model.MailboxLocker (nothing is left to do).
func (u *User) FinishDeletion(string) (int64, int64, error) { return 0, 0, nil }

// WakeSelected implements model.MailboxLocker: the watchers of the
// mailbox are woken.
func (u *User) WakeSelected(id string) {
	u.mu.Lock()
	defer u.mu.Unlock()

	if b := u.byID(id); b != nil {
		b.wake()
	}
}

// LockedMailbox implements model.MailboxLockReader.
func (u *User) LockedMailbox(name string) error {
	u.mu.Lock()
	defer u.mu.Unlock()

	if u.imp != nil && u.imp.expires.After(time.Now()) {
		return model.ErrImportLocked
	}

	b, _, err := u.lookup(name)
	if err != nil {
		return err
	}

	if lockedBox(b, "") {
		return model.ErrMailboxLocked
	}

	return nil
}

// ExpungeOwn implements model.OwnExpunger.
func (u *User) ExpungeOwn(name string, metas []model.Meta) error {
	u.mu.Lock()
	defer u.mu.Unlock()

	return u.expunge(name, metas, false)
}

// lockReason names a locked mailbox in an error.
func lockReason(name string) error {
	return fmt.Errorf("%w: %s", model.ErrMailboxLocked, strings.TrimSpace(name))
}
