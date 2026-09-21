package memstore

import (
	"shardpost/internal/model"
)

// The account change log of the in-memory store. It has the same
// contract as the one in the RADOS store (see model/account.go) but
// none of its machinery: a change and its record are made under the same
// mutex, so there is never an announcement without a record and never a
// Partial entry except where the RADOS store writes one too.
//
// The entries have to match the ones the RADOS store produces for the
// same operation — same kind, same operation, same order — or the tests
// that drive an IMAP session against this store are evidence for
// nothing.

// accountLogRetention bounds the log, as the RADOS store bounds its own.
// It is a variable so a test can reach the truncated case.
var accountLogRetention = 10000

// accountLog is the per-account change log.
type accountLog struct {
	entries  []model.AccountChange
	seq      uint64
	logStart uint64 // sequence of the oldest entry still kept
}

// record appends one entry under the next sequence. The caller holds the
// user's mutex.
func (l *accountLog) record(c model.AccountChange) {
	l.seq++
	c.Seq = l.seq
	l.entries = append(l.entries, c)

	if len(l.entries) > accountLogRetention {
		l.entries = l.entries[len(l.entries)-accountLogRetention:]
		l.logStart = l.entries[0].Seq
	}
}

// mailboxChange records a change to a mailbox itself.
func (u *User) mailboxChange(op model.AccountOp, b *box, name string) {
	u.acct.record(model.AccountChange{Kind: model.KindMailbox, Op: op, Mailbox: b.ID, Name: name})
}

// emailChange records a change to the messages of a mailbox. An empty
// set of messages is still an entry: the RADOS store spends a sequence
// on it too, and a reader that sees one simply has nothing to fetch.
func (u *User) emailChange(op model.AccountOp, b *box, modseq uint64, metas []model.Meta) {
	c := model.AccountChange{Kind: model.KindEmail, Op: op, Mailbox: b.ID, ModSeq: modseq,
		Msgs: make([]model.EmailRef, 0, len(metas))}

	for i := range metas {
		c.Msgs = append(c.Msgs, model.EmailRef{UID: metas[i].UID, GUID: metas[i].GUID})

		if metas[i].ModSeq > c.ModSeq {
			c.ModSeq = metas[i].ModSeq
		}
	}

	u.acct.record(c)
}

// emailDestroyedPartial records that every message of a mailbox is gone
// without standing behind the list: the RADOS store reads the list
// before the mailbox is closed for writing, so a delivery can slip in
// after it, and the two stores must say the same thing.
func (u *User) emailDestroyedPartial(b *box) {
	c := model.AccountChange{Kind: model.KindEmail, Op: model.OpDestroyed, Mailbox: b.ID, Partial: true,
		Msgs: make([]model.EmailRef, 0, len(b.msgs))}

	for _, m := range b.msgs {
		c.Msgs = append(c.Msgs, model.EmailRef{UID: m.UID, GUID: m.GUID})
	}

	u.acct.record(c)
}

// notePartial records a change without its details, the way the RADOS
// store does for the paths that import rather than serve a client: a
// reader resynchronises the mailbox instead of applying a delta it was
// never given. The caller holds the user's mutex.
func (u *User) notePartial(kind model.AccountChangeKind, b *box, name string) {
	u.acct.record(model.AccountChange{Kind: kind, Op: model.OpUpdated, Mailbox: b.ID, Name: name, Partial: true})
}

// AccountState implements model.User.
func (u *User) AccountState() (uint64, error) {
	u.mu.Lock()
	defer u.mu.Unlock()

	return u.acct.seq, nil
}

// AccountChanges implements model.User.
func (u *User) AccountChanges(since uint64, limit int) ([]model.AccountChange, bool, error) {
	u.mu.Lock()
	defer u.mu.Unlock()

	if limit <= 0 {
		limit = accountLogRetention
	}

	if since > u.acct.seq || since+1 < u.acct.logStart {
		return nil, false, nil
	}

	out := make([]model.AccountChange, 0, limit)

	for _, c := range u.acct.entries {
		if c.Seq <= since {
			continue
		}

		if len(out) == limit {
			break
		}

		out = append(out, c)
	}

	return out, true, nil
}

// PutBlob implements model.Blobs.
func (u *User) PutBlob(raw []byte) (string, error) {
	guid := u.newGUID()

	u.mu.Lock()
	defer u.mu.Unlock()

	u.blobs[guid] = append([]byte(nil), raw...)

	return guid, nil
}

// BlobSize implements model.Blobs.
func (u *User) BlobSize(guid string) (int64, error) {
	u.mu.Lock()
	defer u.mu.Unlock()

	raw, ok := u.blobs[guid]
	if !ok {
		return 0, model.ErrNoBlob
	}

	return int64(len(raw)), nil
}

// ReadBlob implements model.Blobs.
func (u *User) ReadBlob(guid string, _, offset, length int64) ([]byte, error) {
	u.mu.Lock()
	defer u.mu.Unlock()

	raw, ok := u.blobs[guid]
	if !ok {
		return nil, model.ErrNoBlob
	}

	lo := max(offset, 0)
	if lo > int64(len(raw)) {
		lo = int64(len(raw))
	}

	hi := int64(len(raw))
	if length >= 0 {
		hi = min(lo+length, int64(len(raw)))
	}

	return append([]byte(nil), raw[lo:hi]...), nil
}
