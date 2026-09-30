package cephstore

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"sort"
	"strconv"
	"syscall"
	"time"

	"github.com/ceph/go-ceph/rados"

	"shardpost/internal/mailmeta"
	"shardpost/internal/model"
)

// Account import with preserved identities (roadmap 8e).
//
// An import writes messages at their source UIDs with their source
// modseqs instead of allocating them, so IMAP clients that synchronised
// against the source only resynchronise incrementally after the switch.
// It runs under an account-wide import lock (registry key regImportLock)
// that keeps the protocol listeners out (they check ImportLocked when
// opening a user); the store functions below trust the caller to hold it.
//
// No change records are written for imported entries: no client knows
// the mailbox yet. Instead h:logstart is moved past h:modseq, so a client
// that resumes from a source modseq gets the full answer (every entry
// whose modseq is newer, every gap below UIDNEXT as VANISHED) rather than
// an empty change log.
//
// Crash consistency: blobs and FTS postings are written first, then the
// entries of higher shards, then shard 0 with the header (UIDNEXT,
// HIGHESTMODSEQ, keywords, shard 0 entries) in one CAS. A crash leaves
// orphan blobs (reaped by the scrubber) or entries of higher shards whose
// UIDs are not below UIDNEXT yet; a retry finds those, keeps them and
// completes the header.

// regImportLock is the registry key of the account's import lock.
const regImportLock = "import:lock"

// importLock is the value of regImportLock.
type importLock struct {
	Session string `json:"s"`
	Expires int64  `json:"exp"` // unix seconds
}

var _ model.Importer = (*User)(nil)

// ImportLock implements model.Importer.
func (u *User) ImportLock(session string, ttl time.Duration) (time.Time, error) {
	if session == "" || ttl <= 0 {
		return time.Time{}, errors.New("import lock: session and ttl required")
	}

	now := time.Now()
	exp := now.Add(ttl)

	var held bool

	err := casOmap(u, u.idx, oidRegistry, []string{regImportLock}, func(cur map[string][]byte) (map[string][]byte, []string) {
		held = false

		if v, ok := cur[regImportLock]; ok {
			var l importLock
			if json.Unmarshal(v, &l) == nil && l.Session != session && time.Unix(l.Expires, 0).After(now) {
				held = true
				return nil, nil
			}
		}

		b, _ := json.Marshal(importLock{Session: session, Expires: exp.Unix()})

		return map[string][]byte{regImportLock: b}, nil
	})
	if err != nil {
		return time.Time{}, fmt.Errorf("import lock: %w", err)
	}

	if held {
		return time.Time{}, model.ErrImportLocked
	}

	return time.Unix(exp.Unix(), 0), nil
}

// ImportRenew implements model.Importer.
func (u *User) ImportRenew(session string, ttl time.Duration) (time.Time, error) {
	exp := time.Now().Add(ttl)

	var held bool

	err := casOmap(u, u.idx, oidRegistry, []string{regImportLock}, func(cur map[string][]byte) (map[string][]byte, []string) {
		var l importLock
		if v, ok := cur[regImportLock]; !ok || json.Unmarshal(v, &l) != nil || l.Session != session {
			held = false
			return nil, nil
		}

		held = true
		b, _ := json.Marshal(importLock{Session: session, Expires: exp.Unix()})

		return map[string][]byte{regImportLock: b}, nil
	})
	if err != nil {
		return time.Time{}, fmt.Errorf("import lock: %w", err)
	}

	if !held {
		return time.Time{}, model.ErrImportNotHeld
	}

	return time.Unix(exp.Unix(), 0), nil
}

// ImportUnlock implements model.Importer.
func (u *User) ImportUnlock(session string) error {
	return casOmap(u, u.idx, oidRegistry, []string{regImportLock}, func(cur map[string][]byte) (map[string][]byte, []string) {
		var l importLock
		if v, ok := cur[regImportLock]; !ok || json.Unmarshal(v, &l) != nil || l.Session != session {
			return nil, nil
		}

		return nil, []string{regImportLock}
	})
}

// ImportLocked implements model.Importer.
func (u *User) ImportLocked() (bool, error) {
	vals, err := getOmapKeys(u.idx, oidRegistry, []string{regImportLock})
	if err != nil {
		return false, err
	}

	v, ok := vals[regImportLock]
	if !ok {
		return false, nil
	}

	var l importLock
	if json.Unmarshal(v, &l) != nil {
		return true, errors.New("import lock unreadable") // fail closed, visibly
	}

	return time.Unix(l.Expires, 0).After(time.Now()), nil
}

// MailboxByID implements model.Importer.
func (u *User) MailboxByID(id string) (*Mailbox, error) {
	all, err := u.ListMailboxes()
	if err != nil {
		return nil, err
	}

	for i := range all {
		if all[i].ID == id {
			return &all[i], nil
		}
	}

	return nil, ErrNoMailbox
}

// ImportMailbox implements model.Importer.
func (u *User) ImportMailbox(spec model.ImportMailbox) (*Mailbox, error) {
	name := canonical(spec.Name)
	if name == "" || spec.UIDValidity == 0 {
		return nil, errors.New("import mailbox: name and uidvalidity required")
	}

	if spec.ID != "" && model.ValidGUID(spec.ID) {
		if err := u.importRename(spec.ID, name); err != nil {
			return nil, err
		}
	}

	defer u.invalidate()

	u.invalidate()

	mb, err := u.Lookup(name)
	if err != nil && !errors.Is(err, ErrNoMailbox) {
		return nil, err
	}

	// A replacement retires the mailbox that was there under a new id,
	// so the old one is destroyed as far as a client is concerned.
	replace := err == nil && mb.UIDValidity != spec.UIDValidity

	return u.importMailboxLogged(name, mb, spec, replace, errors.Is(err, ErrNoMailbox))
}

// importMailboxLogged runs the step the import needs and records it in
// the account change log. The id of a mailbox that does not exist yet is
// chosen inside that step, so the announcement carries the name and the
// record fills the id in; an announcement whose writer dies without an
// id says only that the mailbox list changed, which is true.
func (u *User) importMailboxLogged(name string, mb *Mailbox, spec model.ImportMailbox, replace, fresh bool) (*Mailbox, error) {
	specs := []acctSpec{{kind: model.KindMailbox, name: name}}
	if replace {
		specs = append(specs, mailboxSpec(mb.ID, name))
	}

	acct, err := u.beginAccount(specs...)
	if err != nil {
		return nil, err
	}

	var imported *Mailbox

	switch {
	case fresh:
		imported, err = u.importCreate(name, spec)
	case replace:
		imported, err = u.importReplace(mb, spec)
	default:
		imported, err = u.importAdopt(mb, spec)
	}

	if err != nil {
		acct.partial()
		return nil, err
	}

	changes := []model.AccountChange{{Op: model.OpUpdated, Mailbox: imported.ID, Name: imported.Name, Partial: true}}
	if replace {
		changes = append(changes, model.AccountChange{Op: model.OpDestroyed})
	}

	acct.commit(changes...)

	return imported, nil
}

// importRename renames the mailbox with id to name if it exists under
// another name (the source renamed it between two runs).
func (u *User) importRename(id, name string) error {
	mb, err := u.MailboxByID(id)
	if errors.Is(err, ErrNoMailbox) || (err == nil && mb.Name == name) {
		return nil
	}

	if err != nil {
		return err
	}

	if mb.Name == inboxName {
		return model.ImportConflict("mailbox %s is INBOX here and cannot be renamed to %q", id, name)
	}

	if other, err := u.Lookup(name); err == nil && other.ID != id {
		return model.ImportConflict("rename %q to %q: the name is taken by another mailbox", mb.Name, name)
	}

	return u.RenameMailbox(mb.Name, name)
}

// importCreate creates a mailbox with the source UIDVALIDITY.
func (u *User) importCreate(name string, spec model.ImportMailbox) (*Mailbox, error) {
	rec := MailboxRec{ID: spec.ID, UIDValidity: spec.UIDValidity, SpecialUse: spec.SpecialUse, Subscribed: spec.Subscribed}
	if !model.ValidGUID(rec.ID) {
		rec.ID = newGUID()
	}

	size := u.importShardSize(spec)

	err := u.createIndexObjectSized(&rec, size)
	if errCode(err) == syscall.EEXIST {
		rec.ID = newGUID() // the preferred id is in use
		err = u.createIndexObjectSized(&rec, size)
	}

	if err != nil {
		return nil, fmt.Errorf("import mailbox %q: %w", name, err)
	}

	taken := false
	b, _ := json.Marshal(&rec)

	err = casOmap(u, u.idx, oidRegistry, []string{regKey(name), regSubMissing + name}, func(cur map[string][]byte) (map[string][]byte, []string) {
		if _, taken = cur[regKey(name)]; taken {
			return nil, nil
		}

		return map[string][]byte{regKey(name): b, regLastValidity + name: []byte(strconv.FormatUint(uint64(rec.UIDValidity), 10))},
			[]string{regSubMissing + name}
	})
	if err != nil || taken {
		_ = u.idx.Delete(mboxOID(rec.ID))

		if taken {
			return nil, model.ImportConflict("mailbox %q was created concurrently", name)
		}

		return nil, fmt.Errorf("register mailbox %q: %w", name, err)
	}

	return &Mailbox{Name: name, MailboxRec: rec}, nil
}

// importShardSize is the shard size of a mailbox the import creates.
func (u *User) importShardSize(spec model.ImportMailbox) uint32 {
	if spec.ShardSize > 0 {
		return spec.ShardSize
	}

	return u.shardSize
}

// importAdopt keeps an existing mailbox with the source UIDVALIDITY and
// brings subscription and special-use in line with the source.
func (u *User) importAdopt(mb *Mailbox, spec model.ImportMailbox) (*Mailbox, error) {
	out := *mb

	err := casOmap(u, u.idx, oidRegistry, []string{regKey(mb.Name)}, func(cur map[string][]byte) (map[string][]byte, []string) {
		var rec MailboxRec
		if v, ok := cur[regKey(mb.Name)]; !ok || json.Unmarshal(v, &rec) != nil || rec.ID != mb.ID {
			return nil, nil
		}

		if rec.Subscribed == spec.Subscribed && equalFlags(rec.SpecialUse, spec.SpecialUse) {
			out.MailboxRec = rec
			return nil, nil
		}

		rec.Subscribed, rec.SpecialUse = spec.Subscribed, spec.SpecialUse
		out.MailboxRec = rec
		b, _ := json.Marshal(&rec)

		return map[string][]byte{regKey(mb.Name): b}, nil
	})

	return &out, err
}

// importReplace replaces an existing mailbox whose UIDVALIDITY differs
// from the source; only an empty one may go (typically an INBOX created
// by a first login before the import). The old index object is retired
// first (UIDNEXT 0 reads as "no mailbox"), guarded by the header state
// that was checked empty, so a message linked in between is refused
// instead of lost.
func (u *User) importReplace(mb *Mailbox, spec model.ImportMailbox) (*Mailbox, error) {
	info, err := u.retireEmpty(mb, spec)
	if err != nil {
		return nil, err
	}

	rec := mb.MailboxRec
	rec.ID, rec.UIDValidity, rec.SpecialUse, rec.Subscribed = spec.ID, spec.UIDValidity, spec.SpecialUse, spec.Subscribed

	if !model.ValidGUID(rec.ID) || rec.ID == mb.ID {
		rec.ID = newGUID()
	}

	size := u.importShardSize(spec)

	if err := u.createIndexObjectSized(&rec, size); err != nil {
		rec.ID = newGUID()
		if err := u.createIndexObjectSized(&rec, size); err != nil {
			u.unretire(mb, info)
			return nil, fmt.Errorf("import mailbox %q: %w", mb.Name, err)
		}
	}

	if err := u.swapRecord(mb, rec); err != nil {
		_ = u.idx.Delete(mboxOID(rec.ID))
		u.unretire(mb, info)

		return nil, err
	}

	if err := u.deleteShards(mb, info, mb.Name); err != nil {
		u.log.Warn("import: old index objects not removed", "mailbox", mb.Name, "err", err)
	}

	return &Mailbox{Name: mb.Name, MailboxRec: rec}, nil
}

// retireEmpty retires the index object of an empty mailbox (UIDNEXT 0)
// and returns the header it had. An object an interrupted earlier
// replacement already retired is taken as it is, so the replacement
// resumes instead of failing on the unreadable header forever.
func (u *User) retireEmpty(mb *Mailbox, spec model.ImportMailbox) (*MailboxInfo, error) {
	if info, ok := u.retired(mb); ok {
		return info, nil
	}

	info, _, err := u.readHeader(mb)
	if err != nil {
		return nil, err
	}

	msgs, err := u.ListMessages(mb.ID)
	if err != nil {
		return nil, err
	}

	if len(msgs) > 0 && !spec.Replace {
		return nil, model.ImportConflict("mailbox %q has UIDVALIDITY %d (source %d) and %d messages", mb.Name, mb.UIDValidity, spec.UIDValidity, len(msgs))
	}

	if len(msgs) > 0 {
		if err := u.Expunge(mb.Name, msgs); err != nil {
			return nil, err
		}

		if info, _, err = u.readHeader(mb); err != nil {
			return nil, err
		}
	}

	_, _, err = u.casHeader(mb, func(cur *MailboxInfo, _ uint64) (*headerChange, error) {
		if cur.UIDNext != info.UIDNext || cur.HighestModSeq != info.HighestModSeq {
			return nil, model.ImportConflict("mailbox %q changed while it was replaced", mb.Name)
		}

		return &headerChange{set: map[string][]byte{keyUIDNext: []byte("0")}}, nil
	})

	return info, err
}

// retired reports whether the index object of mb exists with UIDNEXT 0
// (retired by a replacement); info then describes it for deleteShards
// (it was empty: shard 0 is all there is to delete, higher empty shards
// are reaped by the scrubber as unknown objects).
func (u *User) retired(mb *Mailbox) (*MailboxInfo, bool) {
	hv, ver, err := u.getKeysVer(u.idx, mboxOID(mb.ID), []string{keyUIDNext, keyShardSize})
	if err != nil || ver == 0 {
		return nil, false
	}

	if n, _ := strconv.ParseUint(string(hv[keyUIDNext]), 10, 32); n != 0 {
		return nil, false
	}

	ss, _ := strconv.ParseUint(string(hv[keyShardSize]), 10, 32)

	return &MailboxInfo{Mailbox: *mb, UIDNext: 1, ShardSize: uint32(max(ss, 1))}, true
}

// swapRecord replaces the registry record of mb (still pointing at mb's
// id) with rec.
func (u *User) swapRecord(mb *Mailbox, rec MailboxRec) error {
	swapped := false
	b, _ := json.Marshal(&rec)

	err := casOmap(u, u.idx, oidRegistry, []string{regKey(mb.Name)}, func(cur map[string][]byte) (map[string][]byte, []string) {
		var old MailboxRec

		v, ok := cur[regKey(mb.Name)]
		swapped = ok && json.Unmarshal(v, &old) == nil && old.ID == mb.ID

		if !swapped {
			return nil, nil
		}

		return map[string][]byte{regKey(mb.Name): b, regLastValidity + mb.Name: []byte(strconv.FormatUint(uint64(rec.UIDValidity), 10))}, nil
	})
	if err == nil && !swapped {
		err = model.ImportConflict("mailbox %q changed while it was replaced", mb.Name)
	}

	return err
}

// unretire undoes the retirement of an index object after a failed
// replacement (best effort: logged, the mailbox reads as missing until
// repaired).
func (u *User) unretire(mb *Mailbox, info *MailboxInfo) {
	err := casOmap(u, u.idx, mboxOID(mb.ID), []string{keyUIDNext}, func(map[string][]byte) (map[string][]byte, []string) {
		return map[string][]byte{keyUIDNext: []byte(strconv.FormatUint(uint64(info.UIDNext), 10))}, nil
	})
	if err != nil {
		u.log.Error("import: mailbox left retired", "mailbox", mb.Name, "err", err)
	}
}

// preparedImport is a message whose blob exists and whose entry is still
// to be linked.
type preparedImport struct {
	meta   Meta
	shared bool // references a blob that existed before
}

// ImportMessages implements model.Importer.
func (u *User) ImportMessages(id string, msgs []model.ImportMessage) (*model.ImportResult, error) {
	if err := checkImportBatch(msgs); err != nil {
		return nil, err
	}

	mb, err := u.MailboxByID(id)
	if err != nil {
		return nil, err
	}

	info, _, err := u.readHeader(mb)
	if err != nil {
		return nil, err
	}

	fillModSeqs(msgs, info.HighestModSeq)

	res := &model.ImportResult{}

	todo, err := u.newImports(mb, info, msgs, res)
	if err != nil {
		return nil, err
	}

	var size Quota
	for i := range todo {
		size.Bytes += int64(len(todo[i].Raw))
		size.Msgs++
	}

	resv, err := u.reserve(mb, size)
	if err != nil {
		return nil, err
	}

	prepared, err := u.prepareImport(todo)
	if err != nil {
		resv.settle(Quota{})
		return nil, err
	}

	acct, err := u.beginPartial(model.KindEmail, mb.ID, mb.Name)
	if err != nil {
		resv.settle(Quota{})

		return nil, err
	}

	linked, err := u.linkImported(mb, info.ShardSize, prepared, msgs)
	resv.settle(countLinked(prepared, linked, res))

	acct.partial()

	if err != nil {
		return nil, err
	}

	return res, nil
}

// newImports returns the messages whose UIDs are not taken yet; a UID
// that holds a message of the same size counts as existing (a resumed
// batch), one with another message is a conflict.
func (u *User) newImports(mb *Mailbox, info *MailboxInfo, msgs []model.ImportMessage, res *model.ImportResult) ([]model.ImportMessage, error) {
	uids := make([]uint32, len(msgs))
	for i := range msgs {
		uids[i] = msgs[i].UID
	}

	existing, err := u.getEntries(mb.ID, info.ShardSize, uids)
	if err != nil {
		return nil, err
	}

	have := make(map[uint32]Meta, len(existing))
	for _, m := range existing {
		have[m.UID] = m
	}

	var (
		todo  []model.ImportMessage
		stale []Meta
	)

	for i := range msgs {
		e, ok := have[msgs[i].UID]

		switch {
		case !ok:
			todo = append(todo, msgs[i])
		case e.Size != int64(len(msgs[i].Raw)):
			return nil, model.ImportConflict("%s uid %d holds a different message (%d bytes, source %d)", mb.Name, e.UID, e.Size, len(msgs[i].Raw))
		case !u.blobExists(e.GUID):
			// Left by an interrupted step whose blob the scrubber
			// reaped meanwhile: store the message again.
			stale = append(stale, e)
			todo = append(todo, msgs[i])
		default:
			res.Existing++
		}
	}

	return todo, u.dropEntries(mb, info.ShardSize, stale)
}

// dropEntries removes entries without touching their blobs (entries of
// higher shards only need this: shard 0 entries are replaced in the
// header write).
func (u *User) dropEntries(mb *Mailbox, shardSize uint32, metas []Meta) error {
	byShard := map[string][]string{}

	for _, m := range metas {
		oid := u.shardOID(mb.ID, shardOf(m.UID, shardSize))
		if oid != mboxOID(mb.ID) {
			byShard[oid] = append(byShard[oid], uidKey(m.UID))
		}
	}

	for oid, keys := range byShard {
		if err := u.writeEntries(oid, nil, keys, false); err != nil {
			return fmt.Errorf("drop stale entries %s: %w", oid, err)
		}
	}

	return nil
}

// countLinked adds the linked entries to res and returns their usage.
func countLinked(prepared []preparedImport, linked []bool, res *model.ImportResult) Quota {
	var used Quota

	for i := range prepared {
		if !linked[i] {
			continue
		}

		used.Bytes += prepared[i].meta.Size
		used.Msgs++
		res.Imported++
		res.Bytes += prepared[i].meta.Size

		if prepared[i].shared {
			res.Shared++
		}
	}

	return used
}

// fillModSeqs gives messages without a source modseq (a source without
// CONDSTORE) one above the mailbox's highest, so they count as changed
// for clients of this server.
func fillModSeqs(msgs []model.ImportMessage, highest uint64) {
	for i := range msgs {
		if msgs[i].ModSeq == 0 {
			msgs[i].ModSeq = highest + 1
		}
	}
}

// checkImportBatch validates a batch: ascending UIDs, sizes, flags, ids.
func checkImportBatch(msgs []model.ImportMessage) error {
	var last uint32

	for i := range msgs {
		m := &msgs[i]

		switch {
		case m.UID == 0 || m.UID <= last:
			return fmt.Errorf("import: uids must be ascending and non-zero (uid %d after %d)", m.UID, last)
		case int64(len(m.Raw)) > MaxMessageLen:
			return ErrTooLarge
		case m.POP3UIDL != "" && !model.ValidPOP3UIDL(m.POP3UIDL):
			return fmt.Errorf("import: invalid pop3 uidl for uid %d", m.UID)
		}

		if err := checkFlags(m.UID, m.Flags); err != nil {
			return err
		}

		last = m.UID
	}

	return nil
}

// checkFlags validates the flags of uid.
func checkFlags(uid uint32, flags []string) error {
	for _, f := range flags {
		if !model.ValidFlag(f) {
			return fmt.Errorf("import: invalid flag %q for uid %d", f, uid)
		}
	}

	return nil
}

// prepareImport writes (or shares) the blobs and FTS postings of the
// messages and builds their entries; on error everything it did is
// undone. The new blobs are indexed together, once for the batch (see
// ftsIndexDocs).
func (u *User) prepareImport(msgs []model.ImportMessage) ([]preparedImport, error) {
	out := make([]preparedImport, 0, len(msgs))

	var docs []ftsDoc

	for i := range msgs {
		p, err := u.prepareOne(&msgs[i])
		if err != nil {
			u.unprepare(out)
			return nil, err
		}

		out = append(out, p)

		if !p.shared {
			docs = append(docs, ftsDoc{guid: p.meta.GUID, raw: msgs[i].Raw})
		}
	}

	if err := u.ftsIndexDocs(docs); err != nil {
		u.unprepare(out)
		return nil, err
	}

	return out, nil
}

// prepareOne stores one message's blob and builds its entry.
func (u *User) prepareOne(m *model.ImportMessage) (preparedImport, error) {
	guid, shared, err := u.importBlob(m)
	if err != nil {
		return preparedImport{}, err
	}

	env, refs, preview := mailmeta.Extract(m.Raw)
	idate, sdate := m.InternalDate, m.SaveDate

	if idate.IsZero() {
		idate = time.Now()
	}

	if sdate.IsZero() {
		sdate = time.Now()
	}

	return preparedImport{shared: shared, meta: Meta{UID: m.UID, GUID: guid, Size: int64(len(m.Raw)), Chunks: u.chunkCount(int64(len(m.Raw))),
		Flags: append([]string(nil), m.Flags...), InternalDate: idate.UTC(), SaveDate: sdate.UTC(), ModSeq: m.ModSeq,
		Envelope: env, References: refs, Preview: preview, POP3UIDL: m.POP3UIDL}}, nil
}

// importBlob stores the blob of m: a source GUID whose blob already
// exists with the same size is shared (one more reference), otherwise a
// new blob is written under the source GUID (or a fresh one); the caller
// indexes it.
func (u *User) importBlob(m *model.ImportMessage) (guid string, shared bool, err error) {
	guid = m.GUID
	if !model.ValidGUID(guid) {
		guid = newGUID()
	}

	ok, err := u.blobMatches(guid, int64(len(m.Raw)))

	switch {
	case err != nil:
		return "", false, err
	case ok:
		_, err := u.blobRef(guid, +1)
		if err == nil {
			return guid, true, nil
		}

		if !errors.Is(err, ErrGone) {
			return "", false, err
		}

		guid = newGUID() // being deleted: store a copy
	case u.blobExists(guid):
		guid = newGUID() // same id, different content: do not share
	}

	if _, err := u.writeBlob(guid, m.Raw); err != nil {
		// A chunk of an interrupted earlier write may occupy the
		// source id: fall back to a fresh one.
		guid = newGUID()
		if _, err := u.writeBlob(guid, m.Raw); err != nil {
			return "", false, fmt.Errorf("write blob: %w", err)
		}
	}

	return guid, false, nil
}

// blobMatches reports whether the blob guid exists complete with the
// given size: every chunk has its expected size and there is no further
// chunk (an interrupted parallel chunk write can leave some chunks).
func (u *User) blobMatches(guid string, size int64) (bool, error) {
	chunks := u.chunkCount(size)

	for n := 0; n < chunks; n++ {
		st, err := u.data.Stat(chunkOID(guid, n))
		if errCode(err) == syscall.ENOENT {
			return false, nil
		}

		if err != nil {
			return false, err
		}

		if want := min(u.chunkSize, size-int64(n)*u.chunkSize); int64(st.Size) != want {
			return false, nil
		}
	}

	_, err := u.data.Stat(chunkOID(guid, chunks))

	return errCode(err) == syscall.ENOENT, nil // a longer (or unreadable) blob is not the same
}

// blobExists reports whether chunk 0 of guid exists.
func (u *User) blobExists(guid string) bool {
	_, err := u.data.Stat(chunkOID(guid, 0))
	return err == nil
}

// unprepare gives back what prepareImport did for entries that were not
// linked.
func (u *User) unprepare(ps []preparedImport) {
	for i := range ps {
		if ps[i].shared {
			if _, err := u.blobRef(ps[i].meta.GUID, -1); err != nil {
				u.log.Warn("import: blob reference not returned", "guid", ps[i].meta.GUID, "err", err)
			}

			continue
		}

		_ = u.ftsRemove(&ps[i].meta)
		u.deleteBlob(ps[i].meta.GUID, ps[i].meta.Chunks)
	}
}

// importEntries are the encoded entries of a batch by shard object, with
// the batch indexes of each shard.
type importEntries struct {
	shard0 map[string][]byte
	higher map[string]map[string][]byte
	index  map[string][]int
	flags  []string
}

// encodeEntries groups the entries of ps by shard.
func (u *User) encodeEntries(mb *Mailbox, shardSize uint32, ps []preparedImport) (*importEntries, error) {
	e := &importEntries{shard0: map[string][]byte{}, higher: map[string]map[string][]byte{}, index: map[string][]int{}}

	for i := range ps {
		b, err := json.Marshal(&ps[i].meta)
		if err != nil {
			return nil, err
		}

		oid := u.shardOID(mb.ID, shardOf(ps[i].meta.UID, shardSize))
		key := uidKey(ps[i].meta.UID)

		switch {
		case oid == mboxOID(mb.ID):
			e.shard0[key] = b
		case e.higher[oid] == nil:
			e.higher[oid] = map[string][]byte{key: b}
		default:
			e.higher[oid][key] = b
		}

		e.index[oid] = append(e.index[oid], i)
		e.flags = append(e.flags, ps[i].meta.Flags...)
	}

	return e, nil
}

// linkImported writes the entries of higher shards, then shard 0 with the
// header. It reports per prepared message whether its entry is durable;
// the blobs of entries that are certainly not written are given back.
// UIDNEXT and HIGHESTMODSEQ cover the whole batch (all), including UIDs
// that already existed (the entries of an interrupted earlier attempt).
func (u *User) linkImported(mb *Mailbox, shardSize uint32, ps []preparedImport, all []model.ImportMessage) ([]bool, error) {
	linked := make([]bool, len(ps))

	e, err := u.encodeEntries(mb, shardSize, ps)
	if err != nil {
		u.unprepare(ps)
		return linked, err
	}

	for oid, set := range e.higher {
		if err := u.writeEntries(oid, set, nil, false); err != nil {
			// The write of this shard may or may not have happened: its
			// blobs stay (a retry finds the entries, or the scrubber
			// reaps them). Nothing of the shards not written yet and of
			// shard 0 is referenced: give those back.
			u.unprepare(unlinked(ps, e.index, linked, oid))

			return linked, fmt.Errorf("import entries %s: %w", oid, err)
		}

		for _, i := range e.index[oid] {
			linked[i] = true
		}
	}

	var (
		uidNext uint32
		modseq  uint64
	)

	flags := e.flags

	for i := range all {
		uidNext = max(uidNext, all[i].UID+1)
		modseq = max(modseq, all[i].ModSeq)
		flags = append(flags, all[i].Flags...)
	}

	_, _, err = u.casHeader(mb, func(info *MailboxInfo, _ uint64) (*headerChange, error) {
		set := maps.Clone(e.shard0)
		importHeader(set, info, uidNext, modseq)
		setMergedKeywords(set, info.Keywords, flags)

		return &headerChange{set: set}, nil
	})

	shard0 := e.index[mboxOID(mb.ID)]

	if err != nil {
		failed := make([]preparedImport, 0, len(shard0))
		for _, i := range shard0 {
			failed = append(failed, ps[i])
		}

		u.unprepare(failed)

		return linked, err
	}

	for _, i := range shard0 {
		linked[i] = true
	}

	return linked, nil
}

// unlinked returns the prepared messages that are neither linked nor on
// the shard failed (whose write outcome is unknown).
func unlinked(ps []preparedImport, index map[string][]int, linked []bool, failed string) []preparedImport {
	uncertain := map[int]bool{}
	for _, i := range index[failed] {
		uncertain[i] = true
	}

	var out []preparedImport

	for i := range ps {
		if !linked[i] && !uncertain[i] {
			out = append(out, ps[i])
		}
	}

	return out
}

// importHeader sets the header keys of an import step: UIDNEXT and
// HIGHESTMODSEQ at least uidNext and modseq, \Recent watermark at
// UIDNEXT (imported messages are not new), log start past HIGHESTMODSEQ.
func importHeader(set map[string][]byte, info *MailboxInfo, uidNext uint32, modseq uint64) {
	uidNext = max(uidNext, info.UIDNext)
	modseq = max(modseq, info.HighestModSeq)

	set[keyUIDNext] = []byte(strconv.FormatUint(uint64(uidNext), 10))
	set[keyModSeq] = []byte(strconv.FormatUint(modseq, 10))
	set[keyLogStart] = []byte(strconv.FormatUint(max(modseq+1, info.LogStart), 10))
	set[keyRecent] = []byte(strconv.FormatUint(uint64(max(uidNext, info.RecentFrom)), 10))
}

// ImportChanges implements model.Importer.
func (u *User) ImportChanges(id string, flags []model.ImportFlags, expunge []uint32) error {
	mb, err := u.MailboxByID(id)
	if err != nil {
		return err
	}

	for _, f := range flags {
		if err := checkFlags(f.UID, f.Flags); err != nil {
			return err
		}
	}

	info, _, err := u.readHeader(mb)
	if err != nil {
		return err
	}

	// Flags without a source modseq get one above everything, including
	// the modseq the expunge below consumes.
	fresh := info.HighestModSeq + 1

	acct, err := u.beginPartial(model.KindEmail, mb.ID, mb.Name)
	if err != nil {
		return err
	}
	defer acct.partial()

	if len(expunge) > 0 {
		fresh++
	}

	for i := range flags {
		if flags[i].ModSeq == 0 {
			flags[i].ModSeq = fresh
		}
	}

	if err := u.importExpunge(mb, info, expunge); err != nil {
		return err
	}

	modseq, all, err := u.importFlags(mb, info, flags)
	if err != nil {
		return err
	}

	_, _, err = u.casHeader(mb, func(info *MailboxInfo, _ uint64) (*headerChange, error) {
		set := map[string][]byte{}
		importHeader(set, info, 0, modseq)
		setMergedKeywords(set, info.Keywords, all)

		return &headerChange{set: set}, nil
	})

	return err
}

// importExpunge removes the entries of uids (like EXPUNGE).
func (u *User) importExpunge(mb *Mailbox, info *MailboxInfo, uids []uint32) error {
	if len(uids) == 0 {
		return nil
	}

	metas, err := u.getEntries(mb.ID, info.ShardSize, uids)
	if err != nil {
		return err
	}

	return u.Expunge(mb.Name, metas)
}

// importFlags rewrites the flags and modseqs of existing entries, shard
// by shard; it returns the highest modseq and all flags set.
func (u *User) importFlags(mb *Mailbox, info *MailboxInfo, flags []model.ImportFlags) (uint64, []string, error) {
	var (
		modseq uint64
		all    []string
	)

	byShard := map[string]map[string]model.ImportFlags{}

	for _, f := range flags {
		oid := u.shardOID(mb.ID, shardOf(f.UID, info.ShardSize))
		if byShard[oid] == nil {
			byShard[oid] = map[string]model.ImportFlags{}
		}

		byShard[oid][uidKey(f.UID)] = f
		modseq = max(modseq, f.ModSeq)
		all = append(all, f.Flags...)
	}

	for oid, want := range byShard {
		keys := slices.Sorted(maps.Keys(want))

		err := u.applyEntries(oid, keys, func(cur map[string]Meta) map[string]Meta {
			out := map[string]Meta{}

			for k, m := range cur {
				f := want[k]
				if equalFlags(m.Flags, f.Flags) && m.ModSeq == f.ModSeq {
					continue
				}

				m.Flags, m.ModSeq = append([]string(nil), f.Flags...), f.ModSeq
				out[k] = m
			}

			return out
		})
		if err != nil {
			return 0, nil, fmt.Errorf("import flags %s: %w", oid, err)
		}
	}

	return modseq, all, nil
}

// ImportFinalize implements model.Importer.
func (u *User) ImportFinalize(id string, uidNext uint32, highestModSeq uint64) (*MailboxInfo, error) {
	mb, err := u.MailboxByID(id)
	if err != nil {
		return nil, err
	}

	// One entry for the whole finalised mailbox: a JMAP client that was
	// connected before the import resynchronises it rather than being
	// handed a delta of thousands of messages.
	acct, err := u.beginPartial(model.KindEmail, mb.ID, mb.Name)
	if err != nil {
		return nil, err
	}

	_, _, err = u.casHeader(mb, func(info *MailboxInfo, _ uint64) (*headerChange, error) {
		set := map[string][]byte{}
		importHeader(set, info, uidNext, highestModSeq)

		return &headerChange{set: set}, nil
	})

	acct.partial()

	if err != nil {
		return nil, err
	}

	// casHeader reports the header as read; report the written state.
	info, _, err := u.readHeader(mb)
	if err != nil {
		return nil, err
	}

	return info, u.ensureShards(mb, info)
}

// ensureShards creates the missing shard objects below UIDNEXT. Source
// UIDs can leave whole shards empty; the scrubber takes a missing
// intermediate shard for a lost one and then repairs nothing.
func (u *User) ensureShards(mb *Mailbox, info *MailboxInfo) error {
	last := shardOf(max(info.UIDNext-1, 1), info.ShardSize)

	for s := uint32(1); s < last; s++ {
		op := rados.CreateWriteOp()
		op.Create(rados.CreateIdempotent)
		err := op.Operate(u.idx, u.shardOID(mb.ID, s), rados.OperationNoFlag)
		op.Release()

		if err != nil {
			return fmt.Errorf("create shard %d of %q: %w", s, mb.Name, err)
		}
	}

	return nil
}

// ListMessagesAfter implements model.Importer: it reads the shards from
// the one of after+1 on, in order, until limit records are found.
func (u *User) ListMessagesAfter(id string, after uint32, limit int) ([]Meta, error) {
	uidNext, shardSize, err := u.entryRange(id)
	if err != nil || limit <= 0 || uidNext <= uint64(after)+1 {
		return nil, err
	}

	last := shardOf(uint32(uidNext-1), shardSize)

	var out []Meta

	for s := shardOf(after+1, shardSize); s <= last && len(out) < limit; s++ {
		page, err := u.entriesAfter(u.shardOID(id, s), after, uidNext, limit-len(out))
		if err != nil {
			return nil, fmt.Errorf("list %s: %w", id, err)
		}

		out = append(out, page...)
	}

	return out, nil
}

// entryRange reads UIDNEXT and the shard size of a mailbox.
func (u *User) entryRange(id string) (uint64, uint32, error) {
	hv, ver, err := u.getKeysVer(u.idx, mboxOID(id), []string{keyUIDNext, keyShardSize})
	if err != nil {
		return 0, 0, fmt.Errorf("list %s: %w", id, err)
	}

	n, _ := strconv.ParseUint(string(hv[keyUIDNext]), 10, 32)
	if ver == 0 || n == 0 {
		return 0, 0, ErrNoMailbox
	}

	ss, _ := strconv.ParseUint(string(hv[keyShardSize]), 10, 32)
	if ss == 0 {
		ss = DefaultShardSize
	}

	return n, uint32(ss), nil
}

// entriesAfter reads up to limit entries of one shard with a UID above
// after and below uidNext, in UID order. One omap read returns at most
// what the OSD allows per request (osd_max_omap_entries_per_request,
// 1024 by default) however much is asked for, so the shard is read in
// pages until a page comes back short.
func (u *User) entriesAfter(oid string, after uint32, uidNext uint64, limit int) ([]Meta, error) {
	var out []Meta

	for start := after; len(out) < limit; {
		want := min(limit-len(out), omapPageSize)

		vals, err := u.idx.GetOmapValues(oid, uidKey(start), keyMsgPrefix, int64(want))
		if err != nil {
			if errCode(err) == syscall.ENOENT {
				break
			}

			return nil, err
		}

		page := make([]Meta, 0, len(vals))

		for k, v := range vals {
			var m Meta
			if err := json.Unmarshal(v, &m); err != nil {
				return nil, fmt.Errorf("corrupt index record %s in %s", k, oid)
			}

			start = max(start, m.UID)

			if m.UID > after && uint64(m.UID) < uidNext {
				page = append(page, m)
			}
		}

		sort.Slice(page, func(i, j int) bool { return page[i].UID < page[j].UID })
		out = append(out, page...)

		if len(vals) < want {
			break
		}
	}

	return out, nil
}
