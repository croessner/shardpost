package memstore

import (
	"errors"
	"sort"
	"strings"
	"time"

	"shardpost/internal/fts"
	"shardpost/internal/mailmeta"
	"shardpost/internal/model"
)

// importLock is the account import lock of a memstore user.
type importLock struct {
	session string
	expires time.Time
}

var _ model.Importer = (*User)(nil)

// ImportLock implements model.Importer.
func (u *User) ImportLock(session string, ttl time.Duration) (time.Time, error) {
	if session == "" || ttl <= 0 {
		return time.Time{}, errors.New("import lock: session and ttl required")
	}

	u.mu.Lock()
	defer u.mu.Unlock()

	now := time.Now()
	if u.imp != nil && u.imp.session != session && u.imp.expires.After(now) {
		return time.Time{}, model.ErrImportLocked
	}

	u.imp = &importLock{session: session, expires: now.Add(ttl)}

	return u.imp.expires, nil
}

// ImportRenew implements model.Importer.
func (u *User) ImportRenew(session string, ttl time.Duration) (time.Time, error) {
	u.mu.Lock()
	defer u.mu.Unlock()

	if u.imp == nil || u.imp.session != session {
		return time.Time{}, model.ErrImportNotHeld
	}

	u.imp.expires = time.Now().Add(ttl)

	return u.imp.expires, nil
}

// ImportUnlock implements model.Importer.
func (u *User) ImportUnlock(session string) error {
	u.mu.Lock()
	defer u.mu.Unlock()

	if u.imp != nil && u.imp.session == session {
		u.imp = nil
	}

	return nil
}

// ImportLocked implements model.Importer.
func (u *User) ImportLocked() (bool, error) {
	u.mu.Lock()
	defer u.mu.Unlock()

	return u.imp != nil && u.imp.expires.After(time.Now()), nil
}

// MailboxByID implements model.Importer.
func (u *User) MailboxByID(id string) (*model.Mailbox, error) {
	u.mu.Lock()
	defer u.mu.Unlock()

	for n, b := range u.boxes {
		if b.ID == id {
			return &model.Mailbox{Name: n, MailboxRec: b.rec()}, nil
		}
	}

	return nil, model.ErrNoMailbox
}

// ImportMailbox implements model.Importer.
func (u *User) ImportMailbox(spec model.ImportMailbox) (*model.Mailbox, error) {
	name := strings.Trim(spec.Name, model.Delim)
	if strings.EqualFold(name, inboxName) {
		name = inboxName
	}

	if name == "" || spec.UIDValidity == 0 {
		return nil, errors.New("import mailbox: name and uidvalidity required")
	}

	if err := u.importRename(spec.ID, name); err != nil {
		return nil, err
	}

	u.mu.Lock()
	defer u.mu.Unlock()

	b, ok := u.boxes[name]
	if ok && b.UIDValidity == spec.UIDValidity {
		b.Subscribed, b.SpecialUse = spec.Subscribed, spec.SpecialUse
		u.notePartial(model.KindMailbox, b, name)

		return &model.Mailbox{Name: name, MailboxRec: b.rec()}, nil
	}

	if ok {
		if err := u.dropForReplace(b, name, spec); err != nil {
			return nil, err
		}
	}

	nb := &box{MailboxRec: model.MailboxRec{ID: u.importID(spec.ID), UIDValidity: spec.UIDValidity, SpecialUse: spec.SpecialUse, Subscribed: spec.Subscribed},
		uidNext: 1, modseq: 1, vanished: map[uint64][]uint32{}}
	if ok {
		nb.ACL, nb.ACLGen = b.ACL, b.ACLGen
	}

	u.boxes[name] = nb
	delete(u.subMiss, name)
	u.notePartial(model.KindMailbox, nb, name)

	return &model.Mailbox{Name: name, MailboxRec: nb.rec()}, nil
}

// dropForReplace accounts the messages of a mailbox that is replaced;
// a non-empty one needs spec.Replace. u.mu is held.
func (u *User) dropForReplace(b *box, name string, spec model.ImportMailbox) error {
	if len(b.msgs) > 0 && !spec.Replace {
		return model.ImportConflict("mailbox %q has UIDVALIDITY %d (source %d) and messages", name, b.UIDValidity, spec.UIDValidity)
	}

	for _, m := range b.msgs {
		u.usage.Bytes -= m.Size
		u.usage.Msgs--
	}

	return nil
}

// importRename renames the mailbox with id to name (the source renamed
// it between two runs).
func (u *User) importRename(id, name string) error {
	if !model.ValidGUID(id) {
		return nil
	}

	mb, err := u.MailboxByID(id)

	switch {
	case errors.Is(err, model.ErrNoMailbox) || (err == nil && mb.Name == name):
		return nil
	case err != nil:
		return err
	}

	if mb.Name == inboxName {
		return model.ImportConflict("mailbox %s is INBOX here", id)
	}

	if other, err := u.Lookup(name); err == nil && other.ID != id {
		return model.ImportConflict("rename %q to %q: the name is taken", mb.Name, name)
	}

	return u.RenameMailbox(mb.Name, name)
}

// importID is id if it is valid and unused, otherwise a fresh id; u.mu
// is held.
func (u *User) importID(id string) string {
	if model.ValidGUID(id) && !u.idTaken(id) {
		return id
	}

	u.guidN++

	return strings.Repeat("0", 24) + hex8(u.guidN)
}

func (u *User) idTaken(id string) bool {
	for _, b := range u.boxes {
		if b.ID == id {
			return true
		}
	}

	return false
}

func hex8(n int) string {
	const digits = "0123456789abcdef"

	out := make([]byte, 8)
	for i := 7; i >= 0; i-- {
		out[i] = digits[n&0xf]
		n >>= 4
	}

	return string(out)
}

// ImportMessages implements model.Importer.
func (u *User) ImportMessages(id string, msgs []model.ImportMessage) (*model.ImportResult, error) {
	if err := checkBatch(msgs); err != nil {
		return nil, err
	}

	u.mu.Lock()
	defer u.mu.Unlock()

	b := u.byID(id)
	if b == nil {
		return nil, model.ErrNoMailbox
	}

	have := map[uint32]int64{}
	for _, m := range b.msgs {
		have[m.UID] = m.Size
	}

	res := &model.ImportResult{}

	var modseq uint64

	for i := range msgs {
		m := &msgs[i]
		if m.ModSeq == 0 {
			m.ModSeq = b.modseq + 1
		}

		modseq = max(modseq, m.ModSeq)

		if size, ok := have[m.UID]; ok {
			if size != int64(len(m.Raw)) {
				return nil, model.ImportConflict("uid %d holds a different message", m.UID)
			}

			res.Existing++

			continue
		}

		u.importOne(b, m, res)
	}

	sort.Slice(b.msgs, func(i, j int) bool { return b.msgs[i].UID < b.msgs[j].UID })
	b.importHeader(msgs[len(msgs)-1].UID+1, modseq)
	u.notePartial(model.KindEmail, b, "")

	return res, nil
}

// checkBatch validates an import batch like cephstore does.
func checkBatch(msgs []model.ImportMessage) error {
	var last uint32

	for i := range msgs {
		if msgs[i].UID == 0 || msgs[i].UID <= last {
			return errors.New("import: uids must be ascending and non-zero")
		}

		last = msgs[i].UID

		if msgs[i].POP3UIDL != "" && !model.ValidPOP3UIDL(msgs[i].POP3UIDL) {
			return errors.New("import: invalid pop3 uidl")
		}

		if !validFlags(msgs[i].Flags) {
			return errors.New("import: invalid flag")
		}
	}

	if len(msgs) == 0 {
		return errors.New("import: no messages")
	}

	return nil
}

func validFlags(flags []string) bool {
	for _, f := range flags {
		if !model.ValidFlag(f) {
			return false
		}
	}

	return true
}

// importOne stores one message (sharing the blob of an identical GUID);
// u.mu is held.
func (u *User) importOne(b *box, m *model.ImportMessage, res *model.ImportResult) {
	guid := m.GUID
	if old, ok := u.blobs[guid]; model.ValidGUID(guid) && ok && len(old) == len(m.Raw) {
		res.Shared++
	} else {
		if !model.ValidGUID(guid) || ok {
			u.guidN++
			guid = strings.Repeat("0", 24) + hex8(u.guidN)
		}

		u.blobs[guid] = m.Raw
		u.index(guid, m.Raw)
	}

	env, refs, preview := mailmeta.Extract(m.Raw)
	b.msgs = append(b.msgs, model.Meta{UID: m.UID, GUID: guid, Size: int64(len(m.Raw)), Chunks: 1, Flags: append([]string(nil), m.Flags...),
		InternalDate: m.InternalDate, SaveDate: m.SaveDate, ModSeq: m.ModSeq, Envelope: env, References: refs, Preview: preview, POP3UIDL: m.POP3UIDL})
	b.addKeywords(m.Flags)
	u.usage.Bytes += int64(len(m.Raw))
	u.usage.Msgs++
	res.Imported++
	res.Bytes += int64(len(m.Raw))
}

// index adds the search terms of raw under guid; u.mu is held.
func (u *User) index(guid string, raw []byte) {
	for _, t := range fts.Terms(raw) {
		if u.terms[t] == nil {
			u.terms[t] = map[string]bool{}
		}

		u.terms[t][guid] = true
	}
}

// importHeader is cephstore's importHeader for a memstore box.
func (b *box) importHeader(uidNext uint32, modseq uint64) {
	b.uidNext = max(b.uidNext, uidNext)
	b.modseq = max(b.modseq, modseq)
	b.logStart = max(b.logStart, b.modseq+1)
	b.recentFrom = max(b.recentFrom, b.uidNext)
}

// ImportChanges implements model.Importer.
func (u *User) ImportChanges(id string, flags []model.ImportFlags, expunge []uint32) error {
	u.mu.Lock()
	defer u.mu.Unlock()

	b := u.byID(id)
	if b == nil {
		return model.ErrNoMailbox
	}

	if len(expunge) > 0 {
		var gone []model.Meta

		for _, m := range b.msgs {
			if containsUID(expunge, m.UID) {
				gone = append(gone, m)
			}
		}

		if len(gone) > 0 {
			u.remove(b, gone)

			for _, m := range gone {
				u.usage.Bytes -= m.Size
				u.usage.Msgs--
			}
		}
	}

	var modseq uint64

	fresh := b.modseq + 1

	for _, f := range flags {
		if f.ModSeq == 0 {
			f.ModSeq = fresh
		}

		for i := range b.msgs {
			if b.msgs[i].UID == f.UID {
				b.msgs[i].Flags, b.msgs[i].ModSeq = append([]string(nil), f.Flags...), f.ModSeq
				b.addKeywords(f.Flags)
			}
		}

		modseq = max(modseq, f.ModSeq)
	}

	b.importHeader(0, modseq)
	u.notePartial(model.KindEmail, b, "")

	return nil
}

// ImportFinalize implements model.Importer.
func (u *User) ImportFinalize(id string, uidNext uint32, highestModSeq uint64) (*model.MailboxInfo, error) {
	u.mu.Lock()

	b := u.byID(id)
	if b == nil {
		u.mu.Unlock()
		return nil, model.ErrNoMailbox
	}

	b.importHeader(uidNext, highestModSeq)
	u.notePartial(model.KindEmail, b, "")

	name := ""

	for n, x := range u.boxes {
		if x == b {
			name = n
		}
	}
	u.mu.Unlock()

	return u.Info(name)
}

// ListMessagesAfter implements model.Importer.
func (u *User) ListMessagesAfter(id string, after uint32, limit int) ([]model.Meta, error) {
	u.mu.Lock()
	defer u.mu.Unlock()

	b := u.byID(id)
	if b == nil {
		return nil, model.ErrNoMailbox
	}

	var out []model.Meta

	for _, m := range b.msgs {
		if m.UID > after && len(out) < limit {
			out = append(out, m)
		}
	}

	return out, nil
}
