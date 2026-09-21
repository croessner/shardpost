package cephstore

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"shardpost/internal/model"
)

// The account change log, one object per account in the index pool:
//
//	"account"            omap:
//	                        "h:seq"      -> the account counter
//	                        "h:logstart" -> oldest sequence still kept
//	                        "p:<seq>"    -> announcement of a change in flight
//	                        "a:<seq>"    -> the recorded change
//
// See the package comment of model/account.go for why it exists and why
// it is written in two phases. What matters here: the announcement and
// the record are two writes to one object, the mailbox change between
// them is a write to another, and RADOS cannot make the three atomic.
// The order is therefore chosen so that a crash over-reports rather than
// under-reports: an announcement without a record makes a reader
// resynchronise one mailbox, while a record without the mailbox change
// would make it fetch something that did not change. Both are safe; a
// mailbox change without any trace in the log would not be, and that is
// the order this code never takes.
const (
	oidAccount        = "account"
	acctKeySeq        = "h:seq"
	acctKeyLogStart   = "h:logstart"
	acctEntryPrefix   = "a:"
	acctPendingPrefix = "p:"
	// DefaultAccountLogRetention bounds how many account change entries
	// an account keeps. A client that was away longer resynchronises.
	DefaultAccountLogRetention = 10000
	// acctPendingMaxAge is how long a reader waits for an announced
	// change to be recorded before it assumes the writer died. Too short
	// and a long COPY makes readers resynchronise a mailbox for nothing;
	// too long and one dead process stalls the account state for that
	// long. Both are recoverable, the first one cheaply.
	acctPendingMaxAge = 60 * time.Second
)

// acctRetention is the configured retention, or the default when the
// configuration leaves it out.
func acctRetention(n int) int {
	if n <= 0 {
		return DefaultAccountLogRetention
	}

	return n
}

func acctEntryKey(seq uint64) string   { return fmt.Sprintf("%s%020d", acctEntryPrefix, seq) }
func acctPendingKey(seq uint64) string { return fmt.Sprintf("%s%020d", acctPendingPrefix, seq) }

// acctSeqOf reads the sequence out of an entry or announcement key.
func acctSeqOf(key string) uint64 {
	if i := strings.IndexByte(key, ':'); i >= 0 {
		n, _ := strconv.ParseUint(key[i+1:], 10, 64)

		return n
	}

	return 0
}

// acctPending is the announcement of a change in flight: enough to name
// the mailbox that is being changed if the writer never comes back.
type acctPending struct {
	Kind    model.AccountChangeKind `json:"k"`
	Mailbox string                  `json:"m,omitempty"`
	Name    string                  `json:"n,omitempty"`
	Time    int64                   `json:"t"`
}

// acctSpec names one object a change is about to touch.
type acctSpec struct {
	kind    model.AccountChangeKind
	mailbox string
	name    string
}

// emailSpec announces a change to the messages of a mailbox.
func emailSpec(mailboxID string) acctSpec {
	return acctSpec{kind: model.KindEmail, mailbox: mailboxID}
}

// mailboxSpec announces a change to a mailbox itself.
func mailboxSpec(mailboxID, name string) acctSpec {
	return acctSpec{kind: model.KindMailbox, mailbox: mailboxID, name: name}
}

// beginKnownMailbox announces a change to the mailbox of that name, and
// nothing at all when no mailbox carries it. The announcement names the
// mailbox by name only: the id comes from the registry record the change
// itself rewrites, because the lookup here is served from a cache and a
// mailbox that was deleted and recreated elsewhere would otherwise be
// recorded under the id it used to have.
func (u *User) beginKnownMailbox(name string) (*acctTxn, error) {
	_, err := u.Lookup(name)
	if errors.Is(err, ErrNoMailbox) {
		return nil, nil
	}

	if err != nil {
		return nil, err
	}

	return u.beginAccount(acctSpec{kind: model.KindMailbox, name: name})
}

// acctTxn is a set of announced account changes waiting for their
// records. A change that touches two mailboxes (MOVE, a rename with a
// subtree) announces them together, so the log cannot end up holding one
// half of it.
type acctTxn struct {
	u     *User
	specs []acctSpec
	base  uint64 // the sequence of specs[0]; the rest follow it
}

// beginAccount announces the changes in the account change log. It is
// called after the preconditions of the change have passed and before
// the first write, so that everything the write may leave behind is
// covered by an announcement.
func (u *User) beginAccount(specs ...acctSpec) (*acctTxn, error) {
	if len(specs) == 0 {
		return nil, nil
	}

	now := time.Now().Unix()

	var base uint64

	err := casOmap(u, u.idx, oidAccount, []string{acctKeySeq}, func(cur map[string][]byte) (map[string][]byte, []string) {
		last, _ := strconv.ParseUint(string(cur[acctKeySeq]), 10, 64)
		base = last + 1
		set := map[string][]byte{acctKeySeq: []byte(strconv.FormatUint(last+uint64(len(specs)), 10))}

		for i, sp := range specs {
			b, _ := json.Marshal(&acctPending{Kind: sp.kind, Mailbox: sp.mailbox, Name: sp.name, Time: now})
			set[acctPendingKey(base+uint64(i))] = b
		}

		return set, nil
	})
	if err != nil {
		return nil, fmt.Errorf("announce account change: %w", err)
	}

	return &acctTxn{u: u, specs: specs, base: base}, nil
}

// commit records what the changes did and clears their announcements, in
// one write. One entry per announced spec, in the same order; a spec
// whose change did not happen is committed as a no-op entry, because the
// sequence is spent either way.
func (t *acctTxn) commit(cs ...model.AccountChange) {
	if t == nil {
		return
	}

	set := map[string][]byte{}
	rm := make([]string, 0, len(t.specs))

	for i, sp := range t.specs {
		var c model.AccountChange
		if i < len(cs) {
			c = cs[i]
		}

		c.Kind = sp.kind
		if c.Mailbox == "" {
			c.Mailbox = sp.mailbox
		}

		if c.Name == "" {
			c.Name = sp.name
		}

		if c.Op == "" {
			c.Op = model.OpUpdated
		}

		b, _ := json.Marshal(&c)
		set[acctEntryKey(t.base+uint64(i))] = b
		rm = append(rm, acctPendingKey(t.base+uint64(i)))
	}

	t.write(set, rm)
}

// write applies the records and clears the announcements. It is a CAS
// against the announcements themselves: a reader that gave up waiting
// (acctPendingMaxAge) has already turned them into Partial entries and
// handed the client a state above them, so overwriting those entries now
// would hide this change behind a token the client has already passed.
// In that case the change is recorded again under a fresh sequence.
func (t *acctTxn) write(set map[string][]byte, rm []string) {
	late := false

	err := casOmap(t.u, t.u.idx, oidAccount, rm, func(cur map[string][]byte) (map[string][]byte, []string) {
		late = false

		for _, k := range rm {
			if _, still := cur[k]; !still {
				late = true

				return nil, nil
			}
		}

		return set, rm
	})
	if err != nil {
		// The announcements stay; a reader turns them into Partial
		// entries, which costs one resynchronisation and loses nothing.
		t.u.log.Warn("account change not recorded", "seq", t.base, "err", err)

		return
	}

	if late {
		t.u.log.Warn("account change recorded late", "seq", t.base, "specs", len(t.specs))

		for _, sp := range t.specs {
			t.u.noteAccountChange(sp.kind, sp.mailbox, sp.name)
		}

		return
	}

	if (t.base+uint64(len(t.specs)))/256 != (t.base-1)/256 {
		t.u.background(func() { t.u.trimAccountLog() })
	}
}

// partial records that the mailboxes changed without saying how. It is
// what a failed change commits when it cannot rule out that it wrote
// something: the reader resynchronises those mailboxes.
func (t *acctTxn) partial() {
	if t == nil {
		return
	}

	cs := make([]model.AccountChange, len(t.specs))
	for i := range cs {
		cs[i] = model.AccountChange{Op: model.OpUpdated, Partial: true}
	}

	t.commit(cs...)
}

// settle closes a transaction whose write either failed (a Partial
// entry, because it may have left something behind), changed nothing (no
// entry), or succeeded (one updated entry naming the mailbox the change
// actually found).
func (t *acctTxn) settle(err error, changed bool, mailboxID string) {
	switch {
	case err != nil:
		t.partial()
	case !changed:
		t.withdraw()
	default:
		t.commit(model.AccountChange{Op: model.OpUpdated, Mailbox: mailboxID})
	}
}

// withdraw removes announcements of changes that provably wrote nothing.
// A failure leaves them behind, which a reader resolves into Partial
// entries: one resynchronisation too many, never one too few.
func (t *acctTxn) withdraw() {
	if t == nil {
		return
	}

	rm := make([]string, 0, len(t.specs))
	for i := range t.specs {
		rm = append(rm, acctPendingKey(t.base+uint64(i)))
	}

	if err := t.u.idx.RmOmapKeys(oidAccount, rm); err != nil {
		t.u.log.Warn("account announcement not withdrawn", "seq", t.base, "err", err)
	}
}

// notePartial announces a change and records it as Partial in one go:
// the mailbox is named, its messages are not, and a reader
// resynchronises it instead of applying a delta it was never given. It
// is what the bulk paths use — the scrubber's repairs, the account
// import — where enumerating what changed would mean listing a whole
// mailbox. Announce first, work, then call this; see beginPartial.
func (u *User) beginPartial(kind model.AccountChangeKind, mailboxID, name string) (*acctTxn, error) {
	return u.beginAccount(acctSpec{kind: kind, mailbox: mailboxID, name: name})
}

// noteAccountChange records a Partial change that has already happened,
// without an announcement. It is the recovery path of a commit that came
// too late (see acctTxn.write); nothing else may use it, because an
// entry written after the change can be lost by a crash in between.
func (u *User) noteAccountChange(kind model.AccountChangeKind, mailboxID, name string) {
	c := model.AccountChange{Kind: kind, Op: model.OpUpdated, Mailbox: mailboxID, Name: name, Partial: true}
	b, _ := json.Marshal(&c)

	err := casOmap(u, u.idx, oidAccount, []string{acctKeySeq}, func(cur map[string][]byte) (map[string][]byte, []string) {
		last, _ := strconv.ParseUint(string(cur[acctKeySeq]), 10, 64)

		return map[string][]byte{
			acctKeySeq:             []byte(strconv.FormatUint(last+1, 10)),
			acctEntryKey(last + 1): b,
		}, nil
	})
	if err != nil {
		u.log.Warn("account change not recorded", "mailbox", mailboxID, "err", err)
	}
}

// emailChange is the record of a change to the given messages at modseq.
func emailChange(op model.AccountOp, modseq uint64, metas []Meta) model.AccountChange {
	c := model.AccountChange{Op: op, ModSeq: modseq, Msgs: make([]model.EmailRef, 0, len(metas))}
	for i := range metas {
		c.Msgs = append(c.Msgs, model.EmailRef{UID: metas[i].UID, GUID: metas[i].GUID})

		if metas[i].ModSeq > c.ModSeq {
			c.ModSeq = metas[i].ModSeq
		}
	}

	return c
}

// AccountState returns the account's change counter: the sequence of the
// newest entry a reader may rely on. A change that is announced but not
// recorded holds the counter below itself, so a client that stores this
// number cannot skip over a change that is still being written.
func (u *User) AccountState() (uint64, error) {
	vals, _, err := u.getKeysVer(u.idx, oidAccount, []string{acctKeySeq})
	if err != nil {
		return 0, fmt.Errorf("read account state: %w", err)
	}

	seq, _ := strconv.ParseUint(string(vals[acctKeySeq]), 10, 64)

	// The counter is read before the announcements, so a change that
	// appears in between lowers the answer rather than raising it: a
	// client is told too little, never too much.
	floor, err := u.acctFloor()
	if err != nil {
		return 0, err
	}

	if floor > 0 && floor <= seq {
		return floor - 1, nil
	}

	return seq, nil
}

// acctFloor returns the lowest sequence that is announced but not
// recorded; everything below it is settled and 0 means all of it is.
// Announcements older than acctPendingMaxAge are resolved on the way,
// because their writer is not coming back.
func (u *User) acctFloor() (uint64, error) {
	pend, err := u.acctPending()
	if err != nil {
		return 0, err
	}

	cutoff := time.Now().Add(-acctPendingMaxAge).Unix()

	for _, seq := range sortedSeqs(pend) {
		var p acctPending
		if json.Unmarshal(pend[acctPendingKey(seq)], &p) == nil && p.Time > cutoff {
			return seq, nil
		}

		if err := u.resolveAnnouncement(seq, p); err != nil {
			return 0, err
		}
	}

	return 0, nil
}

// acctPending reads the announcements of the account, oldest first. A
// page is enough: the answer is the lowest one, and a hundred
// announcements in flight at once is already more than any account does.
func (u *User) acctPending() (map[string][]byte, error) {
	out := map[string][]byte{}

	err := u.idx.ListOmapValues(oidAccount, "", acctPendingPrefix, 128,
		func(key string, value []byte) { out[key] = append([]byte(nil), value...) })
	if err != nil && errCode(err) != syscall.ENOENT {
		return nil, fmt.Errorf("read account announcements: %w", err)
	}

	return out, nil
}

// sortedSeqs returns the sequences of the given keys in order.
func sortedSeqs(vals map[string][]byte) []uint64 {
	out := make([]uint64, 0, len(vals))
	for k := range vals {
		out = append(out, acctSeqOf(k))
	}

	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })

	return out
}

// resolveAnnouncement turns an abandoned announcement into a Partial
// entry, unless its writer recorded it after all. The write is a CAS
// against both keys so that a writer that is just finishing wins: its
// record is the better answer.
func (u *User) resolveAnnouncement(seq uint64, p acctPending) error {
	entry := model.AccountChange{Kind: p.Kind, Op: model.OpUpdated, Mailbox: p.Mailbox, Name: p.Name, Partial: true}
	if entry.Kind == "" {
		entry.Kind = model.KindMailbox
	}

	b, _ := json.Marshal(&entry)
	keys := []string{acctEntryKey(seq), acctPendingKey(seq)}

	err := casOmap(u, u.idx, oidAccount, keys, func(cur map[string][]byte) (map[string][]byte, []string) {
		if _, done := cur[acctEntryKey(seq)]; done {
			return nil, []string{acctPendingKey(seq)}
		}

		if _, still := cur[acctPendingKey(seq)]; !still {
			return nil, nil
		}

		return map[string][]byte{acctEntryKey(seq): b}, []string{acctPendingKey(seq)}
	})
	if err != nil {
		return fmt.Errorf("resolve account announcement %d: %w", seq, err)
	}

	u.log.Warn("account change recovered without its details", "seq", seq, "mailbox", p.Mailbox)

	return nil
}

// AccountChanges returns the entries after since, oldest first, at most
// limit of them. A full page means there may be more: the caller
// continues from the Seq of the last entry. ok is false when the log no
// longer covers since, and the caller has to resynchronise.
//
// The result stops before the first change that is announced but not
// recorded, for the same reason AccountState does.
func (u *User) AccountChanges(since uint64, limit int) (changes []model.AccountChange, ok bool, err error) {
	if limit <= 0 {
		limit = DefaultAccountLogRetention
	}

	seq, logStart, err := u.acctBounds()
	if err != nil {
		return nil, false, err
	}

	if since > seq || since+1 < logStart {
		return nil, false, nil
	}

	// The counter is read before the announcements and the entries after
	// them, so every sequence this call may return was already allocated
	// when the announcements were read: an entry above that snapshot
	// could have been announced after the floor was taken, and returning
	// it would carry the client's token over a change still in flight.
	floor, err := u.acctFloor()
	if err != nil {
		return nil, false, err
	}

	changes, err = u.acctEntries(since, ceiling(floor, seq+1), limit)
	if err != nil {
		return nil, false, err
	}

	// A trim that ran during the listing may have removed entries the
	// caller still needed; the mailbox change log reads its own start
	// afterwards for the same reason.
	if _, after, err := u.acctBounds(); err != nil {
		return nil, false, err
	} else if since+1 < after {
		return nil, false, nil
	}

	return changes, true, nil
}

// ceiling is the lower of the floor and the snapshot, treating 0 as
// "no announcement in flight".
func ceiling(floor, snapshot uint64) uint64 {
	if floor == 0 || floor > snapshot {
		return snapshot
	}

	return floor
}

// acctBounds reads the account counter and the start of the log.
func (u *User) acctBounds() (seq, logStart uint64, err error) {
	vals, _, err := u.getKeysVer(u.idx, oidAccount, []string{acctKeySeq, acctKeyLogStart})
	if err != nil {
		return 0, 0, fmt.Errorf("read account log: %w", err)
	}

	seq, _ = strconv.ParseUint(string(vals[acctKeySeq]), 10, 64)

	logStart, _ = strconv.ParseUint(string(vals[acctKeyLogStart]), 10, 64)
	if logStart == 0 {
		logStart = 1
	}

	return seq, logStart, nil
}

// acctEntries reads at most limit recorded entries after since and
// below bound.
func (u *User) acctEntries(since, bound uint64, limit int) ([]model.AccountChange, error) {
	out := make([]model.AccountChange, 0, limit)

	err := u.idx.ListOmapValues(oidAccount, acctEntryKey(since), acctEntryPrefix, int64(limit),
		func(key string, value []byte) {
			seq := acctSeqOf(key)
			if seq >= bound {
				return
			}

			var c model.AccountChange
			if json.Unmarshal(value, &c) != nil {
				return
			}

			c.Seq = seq
			out = append(out, c)
		})
	if err != nil && errCode(err) != syscall.ENOENT {
		return nil, fmt.Errorf("read account log: %w", err)
	}

	sort.Slice(out, func(i, j int) bool { return out[i].Seq < out[j].Seq })

	return out, nil
}

// trimAccountLog keeps the account log bounded and moves h:logstart
// forward, never backward: a slower concurrent trim must not undo a
// newer one, so the write is a CAS against the current value.
func (u *User) trimAccountLog() {
	retention := u.acctRetention

	all, err := u.idx.GetAllOmapValues(oidAccount, "", acctEntryPrefix, omapPageSize)
	if err != nil || len(all) <= retention {
		return
	}

	keys := make([]string, 0, len(all))
	for k := range all {
		keys = append(keys, k)
	}

	sort.Strings(keys)

	drop := keys[:len(keys)-retention]
	start := acctSeqOf(keys[len(keys)-retention])

	_ = casOmap(u, u.idx, oidAccount, []string{acctKeyLogStart}, func(cur map[string][]byte) (map[string][]byte, []string) {
		have, _ := strconv.ParseUint(string(cur[acctKeyLogStart]), 10, 64)
		if have >= start {
			return nil, nil
		}

		return map[string][]byte{acctKeyLogStart: []byte(strconv.FormatUint(start, 10))}, drop
	})
}
