package memstore

import (
	"hash/fnv"

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

// emailDestroyedAll records that every message of a deleted mailbox is
// gone, the way the RADOS store does: as an ordinary entry listing them.
func (u *User) emailDestroyedAll(b *box) {
	c := model.AccountChange{Kind: model.KindEmail, Op: model.OpDestroyed, Mailbox: b.ID,
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

// IndexGet implements model.Index.
func (u *User) IndexGet(space string, keys []string) (map[string][]byte, error) {
	u.mu.Lock()
	defer u.mu.Unlock()

	out := map[string][]byte{}

	for _, k := range keys {
		if v, ok := u.indexes[space][k]; ok {
			out[k] = append([]byte(nil), v...)
		}
	}

	return out, nil
}

// indexShards is the shard count of the RADOS store's indexes. The
// in-memory index hands its function one shard at a time as well, so that
// a caller that writes outside the keys it was handed fails here, in a
// test, and not only against a cluster.
const indexShards = 32

// indexShard is the shard a key lives in, by the same hash the RADOS
// store uses.
func indexShard(key string) uint32 {
	h := fnv.New32a()
	_, _ = h.Write([]byte(key))

	return h.Sum32() % indexShards
}

// IndexApply implements model.Index: under one mutex, but one shard at a
// time, as the RADOS store does it.
func (u *User) IndexApply(space string, keys []string, fn model.IndexFunc) error {
	u.mu.Lock()
	defer u.mu.Unlock()

	if u.indexes == nil {
		u.indexes = map[string]map[string][]byte{}
	}

	if u.indexes[space] == nil {
		u.indexes[space] = map[string][]byte{}
	}

	groups := map[uint32][]string{}
	for _, k := range keys {
		groups[indexShard(k)] = append(groups[indexShard(k)], k)
	}

	for _, ks := range groups {
		if err := u.applyShard(space, ks, fn); err != nil {
			return err
		}
	}

	return nil
}

// applyShard runs an index function over the keys of one shard.
func (u *User) applyShard(space string, keys []string, fn model.IndexFunc) error {
	allowed := map[string]bool{}
	cur := map[string][]byte{}

	for _, k := range keys {
		allowed[k] = true

		if v, ok := u.indexes[space][k]; ok {
			cur[k] = append([]byte(nil), v...)
		}
	}

	set, rm := fn(keys, cur)

	for k := range set {
		if !allowed[k] {
			return model.ErrIndexKey
		}
	}

	for _, k := range rm {
		if !allowed[k] {
			return model.ErrIndexKey
		}
	}

	for k, v := range set {
		u.indexes[space][k] = append([]byte(nil), v...)
	}

	for _, k := range rm {
		delete(u.indexes[space], k)
	}

	return nil
}

// IndexReset implements model.Index.
func (u *User) IndexReset(space string) error {
	u.mu.Lock()
	defer u.mu.Unlock()

	delete(u.indexes, space)

	return nil
}
