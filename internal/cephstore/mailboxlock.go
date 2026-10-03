package cephstore

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"syscall"
	"time"

	"shardpost/internal/model"
)

// The mailbox lock of mailbox_delete (stage B17, temp/16 §9): instead of
// the account's maintenance lock, the job locks only the mailboxes it
// is about to remove. The lock sits in the registry record of the
// mailbox (MailboxRec.Lock), so every path that reads the record (the
// lookup, the reservation CAS, the rename CAS) sees it at no extra cost,
// and an overview key per session names the locked mailboxes for the
// account-lock takers and the online recount:
//
//	"mblock:<session>"  {ids, names, exp, phase}
//
// The phase is locked (M1), unregistering (M5) or destroying (M6, M7:
// the records are gone, only the blobs and shards are still being
// removed; nothing is locked any more and account locks may be taken).
const regMailboxLockPrefix = "mblock:"

// errIDDeleting refuses a mailbox id whose deletion tombstone is alive.
var errIDDeleting = fmt.Errorf("%w: the id is being deleted", ErrMailboxExists)

// mailboxLockRec is the value of "mblock:<session>".
type mailboxLockRec struct {
	IDs   []string `json:"ids"`
	Names []string `json:"names"`
	Exp   int64    `json:"exp"`
	Phase string   `json:"phase"`
}

func mailboxLockKey(session string) string { return regMailboxLockPrefix + session }

// alive reports whether the overview record has not expired at now.
func (r *mailboxLockRec) alive(now time.Time) bool { return time.Unix(r.Exp, 0).After(now) }

// mailboxLockBlocks reports whether an overview value blocks account
// locks and online recounts: alive and not yet destroying.
func mailboxLockBlocks(v []byte, now time.Time) bool {
	var r mailboxLockRec

	return json.Unmarshal(v, &r) == nil && r.alive(now) && r.Phase != model.MailboxLockDestroying
}

// lockedRecord reports whether the registry record v is locked by a
// session other than session ("" any) at now.
func lockedRecord(v []byte, session string, now time.Time) bool {
	if len(v) == 0 {
		return false
	}

	var rec MailboxRec

	return json.Unmarshal(v, &rec) == nil && rec.Lock.Held(session, now)
}

// lockedMailbox reports whether the (looked up) mailbox is locked by a
// session other than session.
func lockedMailbox(mb *Mailbox, session string, now time.Time) bool {
	return mb != nil && mb.Lock.Held(session, now)
}

// ancestors returns the names above name ("A/B/C": "A", "A/B").
func ancestors(name string) []string {
	var out []string

	for i := 0; i < len(name); i++ {
		if name[i] == '/' {
			out = append(out, name[:i])
		}
	}

	return out
}

// lockedAncestor reports whether an ancestor of name (among the
// registry records all, by key) is locked.
func lockedAncestor(all map[string][]byte, name string, now time.Time) bool {
	for _, a := range ancestors(name) {
		if lockedRecord(all[regKey(a)], "", now) {
			return true
		}
	}

	return false
}

// ancestorKeys returns the registry keys of the ancestors of name.
func ancestorKeys(name string) []string {
	as := ancestors(name)
	keys := make([]string, 0, len(as))

	for _, a := range as {
		keys = append(keys, regKey(a))
	}

	return keys
}

var (
	_ model.MailboxLocker     = (*User)(nil)
	_ model.MailboxLockReader = (*User)(nil)
	_ model.OwnExpunger       = (*User)(nil)
)

// LockMailboxes implements model.MailboxLocker (M1): one CAS reads the
// account lock, the records of the targets, the overview keys and the
// scrub marks, and sets the lock in every record and the overview key.
func (u *User) LockMailboxes(session string, targets []model.MailboxLockTarget, ttl time.Duration) error {
	if session == "" || len(targets) == 0 || ttl <= 0 {
		return errors.New("mailbox lock: session, targets and ttl required")
	}

	now := time.Now()
	exp := now.Add(ttl)

	var refused error

	keys := []string{regImportLock}
	for _, t := range targets {
		keys = append(keys, regKey(t.Name))
	}

	err := casOmapPrefix(u, u.idx, oidRegistry, keys, []string{regMailboxLockPrefix, regScrubMarkPrefix},
		func(cur map[string][]byte, pages map[string]prefixPage) (map[string][]byte, []string) {
			if refused = lockBlockedBy(cur, pages, session, now); refused != nil {
				return nil, nil
			}

			rec := lockRecordOf(pages[regMailboxLockPrefix], session, exp)

			set := map[string][]byte{}

			for _, t := range targets {
				v, err := lockedTargetRecord(cur[regKey(t.Name)], t, session, exp, now)
				if err != nil {
					refused = err
					return nil, nil
				}

				set[regKey(t.Name)] = v

				if !slices.Contains(rec.IDs, t.ID) {
					rec.IDs = append(rec.IDs, t.ID)
					rec.Names = append(rec.Names, t.Name)
				}
			}

			set[mailboxLockKey(session)] = encode(rec)

			return set, nil
		})
	if err != nil {
		return fmt.Errorf("mailbox lock: %w", err)
	}

	u.invalidate()

	return refused
}

// lockBlockedBy reports what keeps a session from locking mailboxes: a
// foreign live account lock or a live scrub mark (both postpone the
// job).
func lockBlockedBy(cur map[string][]byte, pages map[string]prefixPage, session string, now time.Time) error {
	if v, ok := cur[regImportLock]; ok {
		var l importLock
		if json.Unmarshal(v, &l) == nil && l.Session != session && time.Unix(l.Expires, 0).After(now) {
			return model.ErrImportLocked
		}
	}

	for k, v := range pages[regScrubMarkPrefix].vals {
		var m scrubMark
		if json.Unmarshal(v, &m) == nil && time.Unix(m.Exp, 0).After(now) {
			return fmt.Errorf("%w: a scrub is repairing the account (%s)", model.ErrImportLocked, k)
		}
	}

	return nil
}

// lockRecordOf is the overview record a session writes: a fresh one, or
// the one it has (a resumed run locks again; what it locked before
// stays in the record).
func lockRecordOf(page prefixPage, session string, exp time.Time) mailboxLockRec {
	rec := mailboxLockRec{Exp: exp.Unix(), Phase: model.MailboxLockLocked}

	var old mailboxLockRec
	if v, ok := page.vals[mailboxLockKey(session)]; ok && json.Unmarshal(v, &old) == nil && old.Phase != "" {
		rec.Phase, rec.IDs, rec.Names = old.Phase, old.IDs, old.Names
	}

	return rec
}

// lockedTargetRecord returns the record of a target with the session's
// lock; ErrNoMailbox when the name does not carry the planned id,
// ErrMailboxLocked when another session holds it.
func lockedTargetRecord(v []byte, t model.MailboxLockTarget, session string, exp, now time.Time) ([]byte, error) {
	var mb MailboxRec
	if v == nil || json.Unmarshal(v, &mb) != nil || mb.ID != t.ID {
		return nil, fmt.Errorf("%w: %q", ErrNoMailbox, t.Name)
	}

	if mb.Lock.Held(session, now) {
		return nil, ErrMailboxLocked
	}

	mb.Lock = &model.MailboxLock{Session: session, Expires: exp.Unix()}

	return encode(mb), nil
}

// lockRecord reads the overview record of session.
func (u *User) lockRecord(session string) (*mailboxLockRec, error) {
	vals, err := getOmapKeys(u.idx, oidRegistry, []string{mailboxLockKey(session)})
	if err != nil {
		return nil, err
	}

	v, ok := vals[mailboxLockKey(session)]
	if !ok {
		return nil, nil
	}

	var rec mailboxLockRec
	if err := json.Unmarshal(v, &rec); err != nil {
		return nil, fmt.Errorf("mailbox lock record: %w", err)
	}

	return &rec, nil
}

// RenewMailboxLocks implements model.MailboxLocker.
func (u *User) RenewMailboxLocks(session string, ttl time.Duration) error {
	return u.updateLocks(session, func(rec *mailboxLockRec, mb *MailboxRec, exp int64) {
		rec.Exp = exp

		if mb != nil && mb.Lock != nil && mb.Lock.Session == session {
			mb.Lock.Expires = exp
		}
	}, ttl)
}

// SetMailboxLockPhase implements model.MailboxLocker.
func (u *User) SetMailboxLockPhase(session, phase string) error {
	return u.updateLocks(session, func(rec *mailboxLockRec, _ *MailboxRec, _ int64) { rec.Phase = phase }, 0)
}

// updateLocks rewrites the overview record of session and the locks of
// its mailboxes that are still registered under their names with fn;
// ttl > 0 renews them for ttl. An overview record that is gone means
// the lock is lost.
func (u *User) updateLocks(session string, fn func(rec *mailboxLockRec, mb *MailboxRec, exp int64), ttl time.Duration) error {
	rec, err := u.lockRecord(session)
	if err != nil {
		return err
	}

	if rec == nil {
		return fmt.Errorf("mailbox lock of %s: %w", session, model.ErrLocked)
	}

	keys := []string{mailboxLockKey(session)}
	for _, n := range rec.Names {
		keys = append(keys, regKey(n))
	}

	lost := false

	err = casOmap(u, u.idx, oidRegistry, keys, func(cur map[string][]byte) (map[string][]byte, []string) {
		var r mailboxLockRec

		v, ok := cur[mailboxLockKey(session)]
		if lost = !ok || json.Unmarshal(v, &r) != nil; lost {
			return nil, nil
		}

		exp := r.Exp
		if ttl > 0 {
			exp = time.Now().Add(ttl).Unix()
		}

		fn(&r, nil, exp)

		set := lockedRecords(cur, &r, session, func(mb *MailboxRec) { fn(&r, mb, exp) })
		set[mailboxLockKey(session)] = encode(r)

		return set, nil
	})
	if err != nil {
		return fmt.Errorf("mailbox lock: %w", err)
	}

	if lost {
		return fmt.Errorf("mailbox lock of %s: %w", session, model.ErrLocked)
	}

	return nil
}

// lockedRecords applies fn to every record of the session's lock record
// that is still registered under its name with the session's lock, and
// returns the records to write.
func lockedRecords(cur map[string][]byte, r *mailboxLockRec, session string, fn func(mb *MailboxRec)) map[string][]byte {
	set := map[string][]byte{}

	for i, n := range r.Names {
		var mb MailboxRec

		v, ok := cur[regKey(n)]
		if !ok || json.Unmarshal(v, &mb) != nil || mb.ID != r.IDs[i] || mb.Lock == nil || mb.Lock.Session != session {
			continue // unregistered already, or the lock went
		}

		fn(&mb)
		set[regKey(n)] = encode(mb)
	}

	return set
}

// UnlockMailboxes implements model.MailboxLocker.
func (u *User) UnlockMailboxes(session string) error {
	rec, err := u.lockRecord(session)
	if err != nil {
		return err
	}

	if rec == nil {
		return nil
	}

	keys := []string{mailboxLockKey(session)}
	for _, n := range rec.Names {
		keys = append(keys, regKey(n))
	}

	err = casOmap(u, u.idx, oidRegistry, keys, func(cur map[string][]byte) (map[string][]byte, []string) {
		set := map[string][]byte{}

		for _, n := range rec.Names {
			var mb MailboxRec

			v, ok := cur[regKey(n)]
			if !ok || json.Unmarshal(v, &mb) != nil || mb.Lock == nil || mb.Lock.Session != session {
				continue
			}

			mb.Lock = nil
			set[regKey(n)] = encode(mb)
		}

		if _, ok := cur[mailboxLockKey(session)]; !ok && len(set) == 0 {
			return nil, nil
		}

		return set, []string{mailboxLockKey(session)}
	})

	u.invalidate()

	if err != nil {
		return fmt.Errorf("mailbox unlock: %w", err)
	}

	return nil
}

// MailboxLockBarrier implements model.MailboxLocker (M3): it waits,
// live, until no reservation intent names a target and no MOVE intent
// points at one or sits on one. Reservation intents older than dead are
// abandoned (a reservation does not link any more); MOVE intents have
// no dead time: the scrub may still complete one into a target.
func (u *User) MailboxLockBarrier(targets []model.MailboxLockTarget, deadline time.Time, dead time.Duration) (string, string, error) {
	ids := make(map[string]bool, len(targets))
	for _, t := range targets {
		ids[t.ID] = true
	}

	for {
		reason, key, err := u.lockBarrierOnce(ids, dead)
		if err != nil || reason == "" {
			return reason, key, err
		}

		if time.Now().After(deadline) {
			return reason, key, nil
		}

		time.Sleep(barrierPoll)
	}
}

// lockBarrierOnce is one look of MailboxLockBarrier: the reason and key
// it still waits for ("" none).
func (u *User) lockBarrierOnce(ids map[string]bool, dead time.Duration) (string, string, error) {
	if key, err := u.openReservationInto(ids, dead); err != nil || key != "" {
		return model.BusyIntent, key, err
	}

	boxes, err := u.ListMailboxes()
	if err != nil {
		return "", "", err
	}

	for i := range boxes {
		intents, err := u.pendingIntents(boxes[i].ID)
		if err != nil {
			return "", "", fmt.Errorf("mailbox lock barrier: %w", err)
		}

		for _, in := range intents {
			if ids[in.Dest] || ids[boxes[i].ID] {
				return model.BusyMoveIntent, boxes[i].ID + "/" + in.ID, nil
			}
		}
	}

	return "", "", nil
}

// openReservationInto returns the key of a reservation intent ("bi:",
// "qi:") younger than dead that names one of the mailboxes ("" none).
func (u *User) openReservationInto(ids map[string]bool, dead time.Duration) (string, error) {
	now := time.Now()

	for _, p := range []string{regBlobPrefix, regQuotaIntentPrefix} {
		vals, err := u.idx.GetAllOmapValues(oidRegistry, "", p, omapPageSize)
		if err != nil && errCode(err) != syscall.ENOENT {
			return "", fmt.Errorf("mailbox lock barrier: %w", err)
		}

		for k, v := range vals {
			var in struct {
				Time    int64  `json:"t"`
				Mailbox string `json:"m"`
			}

			if json.Unmarshal(v, &in) == nil && ids[in.Mailbox] && now.Sub(time.Unix(in.Time, 0)) < dead {
				return k, nil
			}
		}
	}

	return "", nil
}

// UnregisterForDelete implements model.MailboxLocker (M5): the front
// part of DeleteMailbox for the mailbox id, by id, checked against the
// session's lock; the header is retired at once, so no stale lookup can
// allocate a UID in it. The messages stay for FinishDeletion.
func (u *User) UnregisterForDelete(id, session string) (bool, error) {
	mb, err := u.MailboxByID(id)
	if errors.Is(err, ErrNoMailbox) {
		return false, nil
	}

	if err != nil {
		return false, err
	}

	lease, err := u.lockLearning(mb.ID)
	if err != nil {
		return false, err
	}
	defer lease.Release()

	u.invalidate()

	info, metas, err := u.readForDelete(mb)
	if err != nil {
		return false, err
	}

	if err := u.preserveLearningReceipts(mb); err != nil {
		return false, err
	}

	if lease.Lost() {
		return false, ErrLocked
	}

	snap, lost, err := u.unregisterLoggedAs(mb, lease, metas, info, session)
	if err != nil {
		return false, fmt.Errorf("unregister mailbox %q: %w", mb.Name, err)
	}

	u.invalidate()

	if lost {
		return false, nil
	}

	u.dropSharedRefs(snap.ACL, mb.Name)

	if _, _, err := u.retireDeleted(mb, info); err != nil {
		u.log.Warn("delete: header not retired yet, FinishDeletion retires it", "mailbox", mb.Name, "err", err)
	}

	return true, nil
}

// FinishDeletion implements model.MailboxLocker (M7): the back part of
// DeleteMailbox from the tombstone. The tombstone's time is renewed
// first (its q as it is), so that the scrub does not take the deletion
// for abandoned while it runs.
func (u *User) FinishDeletion(id string) (int64, int64, error) {
	vals, err := getOmapKeys(u.idx, oidRegistry, []string{deleteKey(id)})
	if err != nil {
		return 0, 0, err
	}

	v, ok := vals[deleteKey(id)]
	if !ok {
		return 0, 0, nil
	}

	var tomb mailboxIntent
	if err := json.Unmarshal(v, &tomb); err != nil {
		return 0, 0, fmt.Errorf("tombstone of %s: %w", id, err)
	}

	info, err := u.deletedRange(id, tomb)
	if err != nil {
		return 0, 0, err
	}

	mb := &Mailbox{Name: tomb.Name, MailboxRec: MailboxRec{ID: id}}
	info.Mailbox = *mb

	u.keepTombstone(mb, info, tomb.Name)

	q, err := u.destroyUnregistered(mb, info, nil, tomb.Name)

	return q.Msgs, q.Bytes, err
}

// WakeSelected implements model.MailboxLocker: the sessions that have
// the mailbox selected (its watchers) and the NOTIFY sessions are woken.
func (u *User) WakeSelected(id string) {
	u.notify(id)
	u.notifyAccount()
}

// LockedMailbox implements model.MailboxLockReader: one read of the
// account lock and the record.
func (u *User) LockedMailbox(name string) error {
	name = canonical(name)

	vals, err := getOmapKeys(u.idx, oidRegistry, []string{regImportLock, regKey(name)})
	if err != nil {
		return err
	}

	now := time.Now()

	if v, ok := vals[regImportLock]; ok {
		var l importLock
		if json.Unmarshal(v, &l) == nil && time.Unix(l.Expires, 0).After(now) {
			return model.ErrImportLocked
		}
	}

	v, ok := vals[regKey(name)]
	if !ok {
		return ErrNoMailbox
	}

	if lockedRecord(v, "", now) {
		return ErrMailboxLocked
	}

	return nil
}

// mailboxLocksBlock reports whether a live mailbox lock of another
// session (phase before destroying) is in the overview page.
func mailboxLocksBlock(page prefixPage, session string, now time.Time) (string, bool) {
	for k, v := range page.vals {
		if strings.TrimPrefix(k, regMailboxLockPrefix) != session && mailboxLockBlocks(v, now) {
			return k, true
		}
	}

	return "", false
}
