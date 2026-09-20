package imapproto

import (
	"shardpost/internal/model"
)

// Private \Seen marks of public folders.
//
// A public folder belongs to nobody, so "read" is a statement about a
// person while "answered", "flagged" or a keyword is a statement about
// the folder. \Seen is therefore kept per account in that account's own
// namespace; every other flag stays where it is, shared by everyone.
//
// The marks do not carry a number space of their own. A change first
// allocates the next MODSEQ of the public mailbox (model.User.TouchModSeq)
// and writes the mark under it, so every session sees one monotonic
// MODSEQ per mailbox and CONDSTORE and QRESYNC keep working: the
// mailbox's HIGHESTMODSEQ advances, the change log names the UIDs, and
// the other sessions of the same account re-read their marks.

// maxPrune bounds how many stale marks one SELECT cleans up, so a folder
// with a long history does not turn a SELECT into a mass deletion.
const maxPrune = 1000

// loadPrivateSeen reads the account's marks for the selected public
// folder. It is a no-op for every other mailbox.
func (c *conn) loadPrivateSeen() error {
	if !c.mpublic || c.mbox == nil {
		c.pseen = nil
		return nil
	}

	marks, err := c.user.PrivateSeen(c.mbox.ID, c.mbox.UIDValidity)
	if err != nil {
		return err
	}

	c.pseen = marks

	return nil
}

// prunePrivateSeen drops marks whose message is gone. An EXPUNGE by one
// account cannot reach into another account's namespace, so every
// account cleans up its own marks when it opens the folder.
func (c *conn) prunePrivateSeen() {
	if !c.mpublic || len(c.pseen) == 0 {
		return
	}

	keep := make(map[uint32]bool, len(c.msgs))
	for i := range c.msgs {
		keep[c.msgs[i].UID] = true
	}

	stale := false

	for uid := range c.pseen {
		if !keep[uid] {
			delete(c.pseen, uid)

			stale = true
		}
	}

	if !stale {
		return
	}

	if err := c.user.PrunePrivateSeen(c.mbox.ID, c.mbox.UIDValidity, keep, maxPrune); err != nil {
		c.log.Warn("cleaning up private seen marks failed", "err", err)
	}
}

// privateOverlay replaces the shared \Seen of the messages by the
// account's own marks and lifts their MODSEQ to the one the mark was
// written under. Messages are addressed by UID, so the overlay survives
// a merge of fresh records.
//
// A message the account never touched keeps the shared flag: that is
// what a delivery, an APPEND or a migration left there, and it is the
// only sensible default for everyone who has not read the message yet.
// From the first mark on, the account's own state decides.
func (c *conn) privateOverlay(msgs []model.Meta) {
	if !c.mpublic {
		return
	}

	for i := range msgs {
		mark, ok := c.pseen[msgs[i].UID]
		if !ok {
			continue
		}

		msgs[i].Flags = withSeen(msgs[i].Flags, mark.Seen)
		// The mark was written under a MODSEQ of this mailbox, so the
		// higher of the two is the message's state for this account.
		msgs[i].ModSeq = max(msgs[i].ModSeq, mark.ModSeq)
	}
}

// withSeen returns flags with \Seen set or removed, keeping everything
// else as it is. Flags that already say what they should are returned
// untouched, which is the common case on a public folder.
func withSeen(flags []string, seen bool) []string {
	if model.HasFlag(flags, flagSeen) == seen {
		return flags
	}

	out := make([]string, 0, len(flags)+1)

	for _, f := range flags {
		if !model.HasFlag([]string{f}, flagSeen) {
			out = append(out, f)
		}
	}

	if seen {
		out = append(out, flagSeenLower)
	}

	return out
}

// publicUnseen counts the messages of a public folder that the account
// has not marked as read, for STATUS of a folder it has not selected.
func (c *conn) publicUnseen(mailboxID string, uidValidity uint32, msgs []model.Meta) (int, error) {
	marks, err := c.user.PrivateSeen(mailboxID, uidValidity)
	if err != nil {
		return 0, err
	}

	unseen := 0

	for i := range msgs {
		if !seenBy(marks, msgs[i]) {
			unseen++
		}
	}

	return unseen, nil
}

// seenBy reports whether the account has read the message: its own mark
// if it has one, the shared flag otherwise.
func seenBy(marks map[uint32]model.PrivateMark, m model.Meta) bool {
	if mark, ok := marks[m.UID]; ok {
		return mark.Seen
	}

	return model.HasFlag(m.Flags, flagSeen)
}

// privateStore is what a STORE did to the account's own marks.
type privateStore struct {
	changed  []uint32 // marks that changed, now carrying the new MODSEQ
	modified []uint32 // untouched: the message changed after UNCHANGEDSINCE
}

// splitPrivateSeen sends the \Seen part of a STORE in a public folder to
// the account's own marks and returns the operation for the shared
// flags; for every other mailbox it changes nothing.
func (c *conn) splitPrivateSeen(a storeArgs, metas []model.Meta, apply func([]string) []string) (privateStore, func([]string) []string, error) {
	if !c.mpublic {
		return privateStore{}, apply, nil
	}

	seen, wanted := seenOperation(a.op, a.flags)
	if !wanted {
		return privateStore{}, keepSeen(apply), nil
	}

	ps, err := c.storePrivateSeen(seen, metas, a.unchangedSince)
	if err != nil {
		return privateStore{}, nil, err
	}

	return ps, keepSeen(apply), nil
}

// seenOperation reads the \Seen part out of a STORE: whether the mark is
// to be set or cleared, and whether the operation speaks about it at
// all. A replacing STORE always does — it removes what it does not name.
func seenOperation(op string, flags []string) (seen, wanted bool) {
	named := model.HasFlag(flags, flagSeen)

	switch {
	case op == kwFlags:
		return named, true
	case !named:
		return false, false
	case op == kwAddFlags:
		return true, true
	case op == kwDelFlags:
		return false, true
	}

	return false, false
}

// storePrivateSeen writes the account's own marks for the addressed
// messages. The current marks are read from the store rather than taken
// from the session, so a mark set by another session of the same account
// is not overwritten by a stale view; the MODSEQ comes from the public
// mailbox, so every session learns of the change through the change log.
func (c *conn) storePrivateSeen(seen bool, metas []model.Meta, unchangedSince uint64) (privateStore, error) {
	cur, err := c.user.PrivateSeen(c.mbox.ID, c.mbox.UIDValidity)
	if err != nil {
		return privateStore{}, err
	}

	var ps privateStore

	for _, m := range metas {
		// RFC 7162: the message must not have changed since, and for
		// this account that includes its own mark.
		if unchangedSince != 0 && max(m.ModSeq, cur[m.UID].ModSeq) > unchangedSince {
			ps.modified = append(ps.modified, m.UID)
			continue
		}

		if seenBy(cur, m) != seen {
			ps.changed = append(ps.changed, m.UID)
		}
	}

	c.pseen = cur

	if len(ps.changed) == 0 {
		return ps, nil
	}

	modseq, err := c.mu.TouchModSeq(c.mbox.Name, ps.changed, func(modseq uint64) error {
		return c.user.SetPrivateSeen(c.mbox.ID, c.mbox.UIDValidity, ps.changed, seen, modseq)
	})
	if err != nil {
		return privateStore{}, err
	}

	for _, uid := range ps.changed {
		c.pseen[uid] = model.PrivateMark{Seen: seen, ModSeq: modseq}
	}

	return ps, nil
}

// mergePrivate adds what only changed privately to the result of the
// shared STORE. The store reports a message it did not change — and a
// \Seen that is private never changes a shared record — so without this
// the client would see no FETCH for a message it just marked as read.
func (c *conn) mergePrivate(res *model.FlagsResult, ps privateStore, metas []model.Meta) {
	if !c.mpublic {
		return
	}

	res.Modified = append(res.Modified, ps.modified...)

	updated := make(map[uint32]bool, len(res.Updated))
	for _, m := range res.Updated {
		updated[m.UID] = true
	}

	for _, uid := range ps.changed {
		if updated[uid] {
			continue
		}

		for i := range metas {
			if metas[i].UID == uid {
				res.Updated = append(res.Updated, metas[i])
				break
			}
		}
	}
}

// keepSeen leaves the shared \Seen of a public folder exactly as it is:
// a STORE writes the account's own mark instead, and the shared flag
// stays the default for everyone who has no mark. A replacing STORE
// would otherwise clear it for all of them.
func keepSeen(apply func([]string) []string) func([]string) []string {
	return func(old []string) []string {
		return withSeen(apply(old), model.HasFlag(old, flagSeen))
	}
}
