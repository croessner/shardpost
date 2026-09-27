// Package sharenotice keeps the share notifications of an account (RFC
// 9670 3): one record per change of the rights the account holds on a
// mailbox of another namespace, written by whoever changed the ACL — an
// IMAP SETACL, the admin API, a JMAP Mailbox/set of shareWith — and read
// by JMAP as ShareNotification objects.
//
// The notices of an account live in one private server METADATA entry of
// the account's own namespace, below model.ServerManagedPrefix, so no
// IMAP client can write it. The entry is changed with SwapMetadata, one
// atomic step per change, and it is bounded: a number of notices and an
// age (Limits), and a size that fits one metadata value. A new notice
// about a mailbox the account already has a notice for replaces it
// (RFC 9670 6.3): the old rights of the older one and the new rights of
// the change, or nothing at all when the two are the same again.
//
// Writing a notice is best effort. The ACL is what counts, and a notice
// that cannot be written is logged, never the reason an ACL change fails.
package sharenotice

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"shardpost/internal/model"
)

// Entry is the private server METADATA entry the notices are kept in.
const Entry = model.ServerManagedPrefix + "sharenotifications"

// identAnyone is the RFC 4314 identifier of everybody.
const identAnyone = "anyone"

// Limits bound what one account keeps.
type Limits struct {
	// Max is the number of notices an account keeps; a new one beyond it
	// removes the oldest (RFC 9670 3).
	Max int
	// MaxAge is how long a notice is kept.
	MaxAge time.Duration
}

// The bounds a configuration that says nothing gets, and the most notices
// an account may keep whatever the configuration says: the record has to
// fit one metadata value.
const (
	DefaultMax    = 50
	DefaultMaxAge = 30 * 24 * time.Hour
	HardMax       = 200
)

// maxRecord keeps the stored record below the 64 KiB a metadata value may
// hold, with room to spare; the oldest notices go first when it would
// not fit.
const maxRecord = 56 << 10

// maxName bounds the mailbox name a notice keeps, in bytes.
const maxName = 512

// goneFactor is how many destructions the record remembers per notice it
// may keep, for ShareNotification/changes; a state older than the oldest
// one remembered cannot be answered with a delta.
const goneFactor = 4

// swapTries bounds the attempts of one change against concurrent writers.
const swapTries = 8

// WithDefaults fills in the bounds left at zero and caps Max.
func (l Limits) WithDefaults() Limits {
	if l.Max <= 0 {
		l.Max = DefaultMax
	}

	l.Max = min(l.Max, HardMax)

	if l.MaxAge <= 0 {
		l.MaxAge = DefaultMaxAge
	}

	return l
}

// Notice is one change of the rights an account holds on a mailbox.
type Notice struct {
	ID string `json:"id"`
	// Seq is the sequence of the record the notice was created at.
	Seq uint64 `json:"seq"`
	// Created is when, in Unix seconds.
	Created int64 `json:"created"`
	// By is the account that changed the ACL; "" is an administrator.
	By string `json:"by,omitempty"`
	// Owner is the namespace of the mailbox: its owner's account, or
	// model.PublicNamespace.
	Owner     string `json:"owner"`
	MailboxID string `json:"mailbox"`
	// Name is the mailbox name at the time, as the owner's namespace
	// names it.
	Name string `json:"name"`
	// Old and New are the account's rights (RFC 4314) before and after,
	// the union of its own entry and "anyone"'s; "" is none.
	Old string `json:"old"`
	New string `json:"new"`
}

// Gone is a notice that was destroyed — dismissed by the client, or
// dropped for the bounds — at Seq; Born is the sequence it was created at.
type Gone struct {
	ID   string `json:"id"`
	Seq  uint64 `json:"seq"`
	Born uint64 `json:"born"`
}

// Log is the record of an account: its notices, oldest first, and the
// destructions a /changes still has to report.
type Log struct {
	// Seq counts every creation and destruction; it is the state.
	Seq uint64 `json:"seq"`
	// Floor is the oldest state a delta can be computed from: the
	// destructions before it are forgotten.
	Floor   uint64   `json:"floor,omitempty"`
	Notices []Notice `json:"notices,omitempty"`
	Gone    []Gone   `json:"gone,omitempty"`
}

// read decodes the record of an account; an absent or unreadable entry is
// an empty record. raw is what the entry holds, for the swap.
func read(u model.User, account string) (*Log, string, error) {
	md, err := u.GetMetadata("", account)
	if err != nil {
		return nil, "", err
	}

	raw := md[Entry]
	l := &Log{}

	if raw != "" {
		if json.Unmarshal([]byte(raw), l) != nil {
			// A record this code cannot read is replaced rather than
			// blocking every notice after it.
			l = &Log{}
		}
	}

	return l, raw, nil
}

// update applies fn to the record in one atomic step and retries against
// a concurrent writer; fn reports whether it changed anything.
func update(u model.User, account string, fn func(l *Log) bool) (*Log, error) {
	for range swapTries {
		l, raw, err := read(u, account)
		if err != nil {
			return nil, err
		}

		if !fn(l) {
			return l, nil
		}

		b, err := json.Marshal(l)
		if err != nil {
			return nil, err
		}

		next := string(b)

		swapped, err := u.SwapMetadata("", account, Entry, raw, &next)
		if err != nil {
			return nil, err
		}

		if swapped {
			return l, nil
		}
	}

	return nil, errors.New("share notifications changed concurrently too often")
}

// destroy removes the notice at index i and remembers it.
func (l *Log) destroy(i int) {
	l.Seq++
	l.Gone = append(l.Gone, Gone{ID: l.Notices[i].ID, Seq: l.Seq, Born: l.Notices[i].Seq})
	l.Notices = append(l.Notices[:i], l.Notices[i+1:]...)
}

// prune applies the bounds and reports whether anything changed.
func (l *Log) prune(lim Limits, now time.Time) bool {
	changed := false

	for len(l.Notices) > 0 && (len(l.Notices) > lim.Max || expired(&l.Notices[0], lim, now)) {
		l.destroy(0)

		changed = true
	}

	// The notices are appended in time order, but a clock that went back
	// is no reason to keep an expired one.
	for i := 0; i < len(l.Notices); {
		if expired(&l.Notices[i], lim, now) {
			l.destroy(i)

			changed = true

			continue
		}

		i++
	}

	if keep := goneFactor * lim.Max; len(l.Gone) > keep {
		cut := len(l.Gone) - keep
		l.Floor = max(l.Floor, l.Gone[cut-1].Seq)
		l.Gone = append([]Gone(nil), l.Gone[cut:]...)
		changed = true
	}

	for len(l.Notices) > 0 && encodedSize(l) > maxRecord {
		l.destroy(0)

		changed = true
	}

	for len(l.Gone) > 0 && encodedSize(l) > maxRecord {
		l.Floor = max(l.Floor, l.Gone[0].Seq)
		l.Gone = l.Gone[1:]
		changed = true
	}

	return changed
}

// expired reports whether a notice is older than the limits allow.
func expired(n *Notice, lim Limits, now time.Time) bool {
	return now.Sub(time.Unix(n.Created, 0)) > lim.MaxAge
}

// encodedSize is the size of the stored record.
func encodedSize(l *Log) int {
	b, _ := json.Marshal(l)

	return len(b)
}

// add records a notice: it replaces one about the same mailbox, keeping
// that one's old rights, and is dropped when the rights are the same
// again (RFC 9670 6.3).
func (l *Log) add(n Notice) {
	for i := range l.Notices {
		if l.Notices[i].Owner == n.Owner && l.Notices[i].MailboxID == n.MailboxID {
			n.Old = l.Notices[i].Old
			l.destroy(i)

			break
		}
	}

	if n.Old == n.New {
		return
	}

	l.Seq++
	n.Seq = l.Seq
	n.ID = newID(l.Seq)
	l.Notices = append(l.Notices, n)
}

// newID is the id of the notice created at seq: the sequence, which is
// unique within the record, and a random part, so that a record that was
// removed and started again does not hand out an id a client still holds.
func newID(seq uint64) string {
	var b [3]byte

	_, _ = rand.Read(b[:])

	return "n" + strconv.FormatUint(seq, 36) + hex.EncodeToString(b[:])
}

// Record adds a notice to the record of account in its own namespace u.
func Record(u model.User, account string, n Notice, lim Limits, now time.Time) error {
	lim = lim.WithDefaults()
	n.Created = now.Unix()
	n.Name = truncate(n.Name, maxName)

	_, err := update(u, account, func(l *Log) bool {
		l.add(n)
		l.prune(lim, now)

		return true
	})

	return err
}

// truncate cuts s to at most n bytes without splitting a character.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}

	s = s[:n]
	for len(s) > 0 && !utf8.ValidString(s) {
		s = s[:len(s)-1]
	}

	return s
}

// Load reads the record of an account with the bounds applied. Notices
// that have expired are removed from the stored record as well, so the
// state a client is told moves with what it sees; if that write fails the
// record is served pruned all the same, and the next write catches up.
func Load(u model.User, account string, lim Limits, now time.Time) (*Log, error) {
	lim = lim.WithDefaults()

	l, _, err := read(u, account)
	if err != nil {
		return nil, err
	}

	probe := *l
	probe.Notices = append([]Notice(nil), l.Notices...)
	probe.Gone = append([]Gone(nil), l.Gone...)

	if !probe.prune(lim, now) {
		return l, nil
	}

	stored, err := update(u, account, func(l *Log) bool { return l.prune(lim, now) })
	if err != nil {
		return &probe, nil //nolint:nilerr // served pruned; the next write stores it
	}

	return stored, nil
}

// State reads the state of an account's notices as it is stored, without
// pruning: push compares it to see whether anything changed, and leaves
// the pruning to the reads that apply the account's bounds.
func State(u model.User, account string) (uint64, error) {
	l, _, err := read(u, account)
	if err != nil {
		return 0, err
	}

	return l.Seq, nil
}

// Destroy removes the notices of the given ids and returns the ones it
// removed; an id that is not there is left out of it.
func Destroy(u model.User, account string, ids []string, lim Limits, now time.Time) (*Log, []string, error) {
	lim = lim.WithDefaults()

	var destroyed []string

	l, err := update(u, account, func(l *Log) bool {
		destroyed = destroyed[:0]

		for _, id := range ids {
			for i := range l.Notices {
				if l.Notices[i].ID == id {
					l.destroy(i)

					destroyed = append(destroyed, id)

					break
				}
			}
		}

		pruned := l.prune(lim, now)

		return len(destroyed) > 0 || pruned
	})
	if err != nil {
		return nil, nil, err
	}

	return l, destroyed, nil
}

// Changes is the delta of the record since a state (RFC 8620 5.2): the
// notices created and the ones destroyed after it, at most max events,
// and the state it reaches. ok is false when the record no longer
// reaches back to since, or since is a state it never had.
func (l *Log) Changes(since uint64, maxEvents int) (created, destroyed []string, newState uint64, more, ok bool) {
	if since > l.Seq || since < l.Floor {
		return nil, nil, 0, false, false
	}

	type event struct {
		seq  uint64
		id   string
		gone bool
	}

	var events []event

	for i := range l.Notices {
		if l.Notices[i].Seq > since {
			events = append(events, event{seq: l.Notices[i].Seq, id: l.Notices[i].ID})
		}
	}

	for _, g := range l.Gone {
		// One that came and went after since is news to nobody.
		if g.Seq > since && g.Born <= since {
			events = append(events, event{seq: g.Seq, id: g.ID, gone: true})
		}
	}

	sort.Slice(events, func(i, j int) bool { return events[i].seq < events[j].seq })

	created, destroyed = []string{}, []string{}
	newState = l.Seq

	if len(events) > maxEvents {
		events, more = events[:maxEvents], true
		newState = events[len(events)-1].seq
	}

	for _, e := range events {
		if e.gone {
			destroyed = append(destroyed, e.id)
		} else {
			created = append(created, e.id)
		}
	}

	return created, destroyed, newState, more, true
}

// Opener opens the namespace of an account.
type Opener interface {
	User(name string) (model.User, error)
}

// Change is one ACL change a Recorder is told of.
type Change struct {
	// Owner is the namespace of the mailbox (an account, or
	// model.PublicNamespace), MailboxID and Mailbox the mailbox.
	Owner, MailboxID, Mailbox string
	// Grantee is the identifier whose entry changed.
	Grantee string
	// By is the account that made the change, "" an administrator.
	By string
	// Old and New are the grantee's rights before and after: its own
	// entry united with "anyone"'s (see Effective).
	Old, New string
}

// Recorder writes the notices of ACL changes into the grantees'
// namespaces. A nil Recorder records nothing.
type Recorder struct {
	Store  Opener
	Limits Limits
	Log    *slog.Logger
	// Now is the clock; nil is time.Now.
	Now func() time.Time
}

// Effective is the rights an account holds through its own entry and the
// "anyone" entry, in canonical order (model.EffectiveRights).
func Effective(own, anyone string) string {
	return model.EffectiveRights(map[string]string{"x": own, identAnyone: anyone}, "x")
}

// Notifies reports whether a change of an identifier's entry is one a
// notice is written for: an account's, not "anyone"'s (RFC 9670 3 lets a
// server leave out the changes of a group), nor a negative or reserved
// identifier.
func Notifies(identifier string) bool {
	return model.ValidACLIdentifier(identifier) && identifier != identAnyone &&
		!strings.HasPrefix(identifier, "-") && !strings.HasPrefix(identifier, "~")
}

// Record writes the notice of one change, best effort: a failure is
// logged and nothing else. A change that leaves the grantee's rights as
// they were, and one the grantee made itself, write nothing.
func (r *Recorder) Record(c Change) {
	if r == nil || r.Store == nil {
		return
	}

	old, _ := model.NormalizeRights(c.Old)
	now, _ := model.NormalizeRights(c.New)

	if old == now || !Notifies(c.Grantee) || c.Grantee == c.Owner || c.Grantee == c.By {
		return
	}

	log := r.Log
	if log == nil {
		log = slog.Default()
	}

	g, err := r.Store.User(c.Grantee)
	if err != nil {
		log.Warn("share notification not written", "grantee", c.Grantee, "owner", c.Owner, "err", err)

		return
	}
	defer g.Close()

	clock := time.Now
	if r.Now != nil {
		clock = r.Now
	}

	n := Notice{By: c.By, Owner: c.Owner, MailboxID: c.MailboxID, Name: c.Mailbox, Old: old, New: now}

	if err := Record(g, c.Grantee, n, r.Limits, clock()); err != nil {
		log.Warn("share notification not written", "grantee", c.Grantee, "owner", c.Owner, "err", err)
	}
}
