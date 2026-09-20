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
// The store holds the marks together with a counter, and a session
// reports the higher of that counter and the mailbox MODSEQ, so
// CONDSTORE and QRESYNC keep working for both kinds of change.

// loadPrivateSeen reads the account's marks for the selected public
// folder. It is a no-op for every other mailbox.
func (c *conn) loadPrivateSeen() error {
	if !c.mpublic || c.mbox == nil {
		c.pseen, c.pmodseq = nil, 0
		return nil
	}

	marks, modseq, err := c.user.PrivateSeen(c.mbox.ID, c.mbox.UIDValidity)
	if err != nil {
		return err
	}

	c.pseen, c.pmodseq = marks, modseq

	return nil
}

// privateOverlay replaces the shared \Seen of the messages by the
// account's own marks. Messages are addressed by UID, so the overlay
// survives a merge of fresh records.
func (c *conn) privateOverlay(msgs []model.Meta) {
	if !c.mpublic {
		return
	}

	for i := range msgs {
		mark := c.pseen[msgs[i].UID]
		msgs[i].Flags = withSeen(msgs[i].Flags, mark.Seen)
		// Private marks live in the shared number space, so one combined
		// MODSEQ is enough for CONDSTORE and QRESYNC.
		msgs[i].ModSeq = max(msgs[i].ModSeq, mark.ModSeq)
	}
}

// withSeen returns flags with \Seen set or removed, keeping everything
// else as it is.
func withSeen(flags []string, seen bool) []string {
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

// effectiveModSeq combines the mailbox MODSEQ with the counter of the
// account's private marks, so a private change is visible to CONDSTORE
// and QRESYNC without touching the shared state.
func (c *conn) effectiveModSeq(shared uint64) uint64 {
	if !c.mpublic {
		return shared
	}

	return max(shared, c.pmodseq)
}

// setPrivateSeen records the account's own \Seen change.
func (c *conn) setPrivateSeen(uids []uint32, seen bool) error {
	modseq, err := c.user.SetPrivateSeen(c.mbox.ID, c.mbox.UIDValidity, uids, seen, c.info.HighestModSeq)
	if err != nil {
		return err
	}

	if c.pseen == nil {
		c.pseen = map[uint32]model.PrivateMark{}
	}

	for _, uid := range uids {
		c.pseen[uid] = model.PrivateMark{Seen: seen, ModSeq: modseq}
	}

	c.pmodseq = modseq

	return nil
}

// publicUnseen counts the messages of a public folder that the account
// has not marked as read, for STATUS of a folder it has not selected.
func (c *conn) publicUnseen(mailboxID string, uidValidity uint32, msgs []model.Meta) (unseen int, high uint64, err error) {
	marks, high, err := c.user.PrivateSeen(mailboxID, uidValidity)
	if err != nil {
		return 0, 0, err
	}

	for i := range msgs {
		if !marks[msgs[i].UID].Seen {
			unseen++
		}
	}

	return unseen, high, nil
}

// splitPrivateSeen sends the \Seen part of a STORE in a public folder to
// the account's own marks and returns the operation for the shared
// flags; for every other mailbox it changes nothing.
func (c *conn) splitPrivateSeen(op string, flags []string, uids []uint32, apply func([]string) []string) (func([]string) []string, error) {
	if !c.mpublic {
		return apply, nil
	}

	if err := c.storePrivateSeen(op, flags, uids); err != nil {
		return nil, err
	}

	return withoutSeen(apply), nil
}

// storePrivateSeen applies the \Seen part of a STORE to the account's
// own marks. A replacing STORE decides by whether \Seen is in the new
// set; +FLAGS and -FLAGS only act when \Seen is named.
func (c *conn) storePrivateSeen(op string, flags []string, uids []uint32) error {
	named := model.HasFlag(flags, flagSeen)

	switch {
	case op == kwFlags:
		// The message keeps its other flags; \Seen follows the new set.
		if named {
			return c.setPrivateSeen(uids, true)
		}

		return c.setPrivateSeen(uids, false)
	case !named:
		return nil
	case op == kwAddFlags:
		return c.setPrivateSeen(uids, true)
	case op == kwDelFlags:
		return c.setPrivateSeen(uids, false)
	}

	return nil
}

// withoutSeen keeps \Seen out of the shared flags of a public folder:
// it is stored per account instead.
func withoutSeen(apply func([]string) []string) func([]string) []string {
	return func(old []string) []string {
		return withSeen(apply(withSeen(old, false)), false)
	}
}
