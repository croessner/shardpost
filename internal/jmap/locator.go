package jmap

import (
	"encoding/json"
	"errors"
	"strconv"

	"shardpost/internal/mailmeta"
	"shardpost/internal/model"
)

// The locator answers the question JMAP asks and the store cannot: where
// is the message with this id. A JMAP Email id is the GUID — the identity
// IMAP reports as EMAILID, which stays the same when a message moves — but
// the store keys its entries by mailbox and UID and has no way back from
// a GUID.
//
// The locator is a derived index, kept in the store's front-end index
// space and built from the account change log, which already records
// every placement with its mailbox, UID and GUID. That keeps the IMAP
// write path exactly as it was: nothing there writes to the locator. The
// JMAP front-end folds the log into it before it reads, and builds it
// from a scan of the account the first time, or when the log has been
// trimmed past the point it had reached. It holds nothing the store does
// not: a lost write costs a rebuild, and every placement it hands out is
// checked against the mailbox before it is used.
//
//	"h:seq"        the account log sequence folded so far
//	"e:<guid>"     emailRec: the placements of the message, its thread
//	"t:<thread>"   threadRec: the messages of a conversation
const (
	locSpace     = "jmaploc"
	locWatermark = "h:seq"
	locEmail     = "e:"
	locThread    = "t:"
	// locPage is how many log entries one fold step reads.
	locPage = 1024
)

// place is where one copy of a message lives.
type place struct {
	Mailbox string `json:"m"`
	UID     uint32 `json:"u"`
}

// emailRec is the locator record of one message. Seq is the log sequence
// it reflects: a fold never applies an entry at or below it again, so two
// folds that run at once cannot move a record backwards.
type emailRec struct {
	Thread   string  `json:"t,omitempty"`
	Places   []place `json:"p,omitempty"`
	Received int64   `json:"r,omitempty"`
	Seq      uint64  `json:"s"`
}

// threadRec is the locator record of one conversation. It only grows: a
// message that is gone is filtered out when the thread is read, because
// a removal would race an addition from a fold that is still running,
// and an addition that is lost is a message missing from its thread.
type threadRec struct {
	Emails []string `json:"e,omitempty"`
}

// errNoIndex is what a store without front-end indexes gets.
var errNoIndex = errors.New("this store keeps no index for JMAP")

// locator is the index of one account.
type locator struct {
	u  model.User
	ix model.Index
	// allow limits what a lookup finds to the mailboxes the user may
	// read; nil allows every mailbox, which is the personal account. The
	// index itself is built and folded over the whole namespace — it is
	// derived data of the store, not an answer — and only what it hands
	// out is filtered: a message is found where the user may read it,
	// and only there.
	allow map[string]bool
	// overlay adjusts the records of one mailbox to what the user sees
	// (the private \Seen of a public folder); nil changes nothing.
	overlay func(mailbox string, metas []model.Meta) error
}

// newLocator opens the locator of the session's account.
func newLocator(u model.User) (*locator, error) {
	ix, ok := u.(model.Index)
	if !ok {
		return nil, errNoIndex
	}

	return &locator{u: u, ix: ix}, nil
}

// catchUp folds the account log into the locator until it covers the
// given state, and rebuilds it when the log cannot get it there.
func (l *locator) catchUp(state uint64) error {
	wm, built, err := l.watermark()
	if err != nil {
		return err
	}

	if !built {
		return l.rebuild()
	}

	for wm < state {
		entries, ok, err := l.u.AccountChanges(wm, locPage)
		if err != nil {
			return err
		}

		if !ok {
			return l.rebuild()
		}

		if len(entries) == 0 {
			return nil
		}

		if err := l.fold(entries); err != nil {
			return err
		}

		wm = entries[len(entries)-1].Seq
		if err := l.advance(wm); err != nil {
			return err
		}
	}

	return nil
}

// watermark reads how far the locator has folded, and whether it was
// ever built.
func (l *locator) watermark() (uint64, bool, error) {
	vals, err := l.ix.IndexGet(locSpace, []string{locWatermark})
	if err != nil {
		return 0, false, err
	}

	v, ok := vals[locWatermark]
	if !ok {
		return 0, false, nil
	}

	n, err := strconv.ParseUint(string(v), 10, 64)
	if err != nil {
		return 0, false, nil
	}

	return n, true, nil
}

// advance moves the watermark forward, never back: a slower fold that
// finishes later must not undo a faster one.
func (l *locator) advance(seq uint64) error {
	return l.ix.IndexApply(locSpace, []string{locWatermark}, func(_ []string, cur map[string][]byte) (map[string][]byte, []string) {
		if have, err := strconv.ParseUint(string(cur[locWatermark]), 10, 64); err == nil && have >= seq {
			return nil, nil
		}

		return map[string][]byte{locWatermark: []byte(strconv.FormatUint(seq, 10))}, nil
	})
}

// placeOp is one change to the placements of a message, at the log
// sequence it was recorded at.
type placeOp struct {
	at     place
	seq    uint64
	remove bool
}

// fold applies a page of log entries. Placements are taken from the
// entries themselves; a Partial entry names a mailbox without listing
// its messages, and that mailbox is read instead.
func (l *locator) fold(entries []model.AccountChange) error {
	ops := map[string][]placeOp{} // guid -> ops, in log order
	fresh := map[string][]uint32{}
	seen := map[string]description{} // what a rescan has already read

	for i := range entries {
		e := &entries[i]
		if e.Kind != model.KindEmail {
			continue
		}

		if e.Partial && len(e.Msgs) == 0 {
			if err := l.rescan(e.Mailbox, e.Seq, ops, seen); err != nil {
				return err
			}

			continue
		}

		for _, m := range e.Msgs {
			switch e.Op {
			case model.OpCreated:
				ops[m.GUID] = append(ops[m.GUID], placeOp{at: place{e.Mailbox, m.UID}, seq: e.Seq})
				fresh[e.Mailbox] = append(fresh[e.Mailbox], m.UID)
			case model.OpDestroyed:
				ops[m.GUID] = append(ops[m.GUID], placeOp{at: place{e.Mailbox, m.UID}, seq: e.Seq, remove: true})
			}
		}
	}

	desc, err := l.describe(fresh)
	if err != nil {
		return err
	}

	for guid, d := range seen {
		desc[guid] = d
	}

	return l.applyOps(ops, desc)
}

// rescan reads a mailbox whose change the log did not enumerate and
// records every message in it as placed there. What it no longer holds is
// left alone: a placement that is gone is caught when it is used.
func (l *locator) rescan(mailbox string, seq uint64, ops map[string][]placeOp, seen map[string]description) error {
	metas, err := l.u.ListMessages(mailbox)
	if errors.Is(err, model.ErrNoMailbox) {
		return nil
	}

	if err != nil {
		return err
	}

	for i := range metas {
		m := &metas[i]
		ops[m.GUID] = append(ops[m.GUID], placeOp{at: place{mailbox, m.UID}, seq: seq})
		seen[m.GUID] = description{thread: mailmeta.ThreadID(m), received: m.InternalDate.Unix()}
	}

	return nil
}

// description is what a new message record needs besides its placements.
type description struct {
	thread   string
	received int64
}

// describe reads the thread and the arrival time of messages that a fold
// is about to place. A message that is gone again by now, or whose
// mailbox is, has no description, and its record is not given one it
// cannot have. Any other failure fails the fold, which runs again: a
// record placed without its thread would stay out of its thread for
// good, because nothing revisits a record that already has places.
func (l *locator) describe(fresh map[string][]uint32) (map[string]description, error) {
	out := map[string]description{}

	for mailbox, uids := range fresh {
		metas, err := l.u.GetMessages(mailbox, uids)
		if errors.Is(err, model.ErrNoMailbox) {
			continue
		}

		if err != nil {
			return nil, err
		}

		for i := range metas {
			out[metas[i].GUID] = description{thread: mailmeta.ThreadID(&metas[i]), received: metas[i].InternalDate.Unix()}
		}
	}

	return out, nil
}

// applyOps writes the placement changes and the thread memberships they
// imply.
func (l *locator) applyOps(ops map[string][]placeOp, desc map[string]description) error {
	if len(ops) == 0 {
		return nil
	}

	keys := make([]string, 0, len(ops))
	for guid := range ops {
		keys = append(keys, locEmail+guid)
	}

	// Thread memberships are collected while the records are written. A
	// shard's function may run more than once, and it may add a message
	// to its thread from an attempt that did not win; that is harmless,
	// because a message's thread never changes and adding twice is adding
	// once.
	threads := map[string][]string{} // thread -> guids to add

	err := l.ix.IndexApply(locSpace, keys, func(shard []string, cur map[string][]byte) (map[string][]byte, []string) {
		set := map[string][]byte{}

		for _, key := range shard {
			guid := key[len(locEmail):]
			rec := decodeEmailRec(cur[key])
			applyPlaceOps(&rec, ops[guid], desc[guid])

			if rec.Thread != "" && len(rec.Places) > 0 {
				threads[rec.Thread] = append(threads[rec.Thread], guid)
			}

			set[key] = encode(&rec)
		}

		return set, nil
	})
	if err != nil {
		return err
	}

	return l.addToThreads(threads)
}

// applyPlaceOps applies the ops newer than the record, in log order.
func applyPlaceOps(rec *emailRec, ops []placeOp, d description) {
	if rec.Thread == "" && d.thread != "" {
		rec.Thread, rec.Received = d.thread, d.received
	}

	for _, op := range ops {
		if op.seq <= rec.Seq {
			continue
		}

		if op.remove {
			rec.Places = without(rec.Places, op.at)
		} else if !hasPlace(rec.Places, op.at) {
			rec.Places = append(rec.Places, op.at)
		}

		rec.Seq = op.seq
	}
}

// hasPlace reports whether the list holds p.
func hasPlace(list []place, p place) bool {
	for _, x := range list {
		if x == p {
			return true
		}
	}

	return false
}

// without is the list without p.
func without(list []place, p place) []place {
	out := list[:0:0]

	for _, x := range list {
		if x != p {
			out = append(out, x)
		}
	}

	return out
}

// addToThreads records messages as members of their threads. Adding is
// all it does, and adding twice is adding once.
func (l *locator) addToThreads(threads map[string][]string) error {
	if len(threads) == 0 {
		return nil
	}

	keys := make([]string, 0, len(threads))
	for t := range threads {
		keys = append(keys, locThread+t)
	}

	return l.ix.IndexApply(locSpace, keys, func(shard []string, cur map[string][]byte) (map[string][]byte, []string) {
		set := map[string][]byte{}

		for _, key := range shard {
			rec := decodeThreadRec(cur[key])
			changed := false

			for _, guid := range threads[key[len(locThread):]] {
				if !containsString(rec.Emails, guid) {
					rec.Emails = append(rec.Emails, guid)
					changed = true
				}
			}

			if changed {
				set[key] = encode(&rec)
			}
		}

		return set, nil
	})
}

// containsString reports whether the list holds s.
func containsString(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}

	return false
}

// decodeEmailRec reads a record; a damaged one reads as empty, which a
// rebuild or the next fold fills in again.
func decodeEmailRec(b []byte) emailRec {
	var rec emailRec
	if len(b) > 0 {
		_ = json.Unmarshal(b, &rec)
	}

	return rec
}

// decodeThreadRec reads a thread record.
func decodeThreadRec(b []byte) threadRec {
	var rec threadRec
	if len(b) > 0 {
		_ = json.Unmarshal(b, &rec)
	}

	return rec
}

// encode writes a record.
func encode(v any) []byte {
	b, _ := json.Marshal(v)

	return b
}

// rebuildChunk bounds how many records one index write carries during a
// rebuild, so that an account with a million messages is not one omap
// write of a million keys.
const rebuildChunk = 4096

// rebuild writes the locator from a scan of the account. The state is
// read first: every change after it is folded on top of the scan by the
// next catch-up, and replaying a placement the scan already saw changes
// nothing — adding a place twice is adding it once, and a UID is never
// reused, so a removal cannot take away something that came back. A
// record a fold has already moved past the state is left alone.
func (l *locator) rebuild() error {
	state, err := l.u.AccountState()
	if err != nil {
		return err
	}

	recs, err := l.scan()
	if err != nil {
		return err
	}

	guids := make([]string, 0, len(recs))
	for guid := range recs {
		guids = append(guids, guid)
	}

	for start := 0; start < len(guids); start += rebuildChunk {
		chunk := guids[start:min(start+rebuildChunk, len(guids))]
		if err := l.writeScanned(chunk, recs, state); err != nil {
			return err
		}
	}

	return l.advance(state)
}

// scan reads the placements of every message of the account.
func (l *locator) scan() (map[string]*emailRec, error) {
	boxes, err := l.u.ListMailboxes()
	if err != nil {
		return nil, err
	}

	recs := map[string]*emailRec{}

	for _, mb := range boxes {
		metas, err := l.u.ListMessages(mb.ID)
		if errors.Is(err, model.ErrNoMailbox) {
			continue
		}

		if err != nil {
			return nil, err
		}

		for i := range metas {
			m := &metas[i]

			rec := recs[m.GUID]
			if rec == nil {
				rec = &emailRec{Thread: mailmeta.ThreadID(m), Received: m.InternalDate.Unix()}
				recs[m.GUID] = rec
			}

			rec.Places = append(rec.Places, place{Mailbox: mb.ID, UID: m.UID})
		}
	}

	return recs, nil
}

// writeScanned writes one chunk of scanned records at the given state.
func (l *locator) writeScanned(guids []string, recs map[string]*emailRec, state uint64) error {
	keys := make([]string, len(guids))
	for i, guid := range guids {
		keys[i] = locEmail + guid
	}

	threads := map[string][]string{}

	err := l.ix.IndexApply(locSpace, keys, func(shard []string, cur map[string][]byte) (map[string][]byte, []string) {
		set := map[string][]byte{}

		for _, key := range shard {
			if have := decodeEmailRec(cur[key]); have.Seq >= state && len(cur[key]) > 0 {
				continue
			}

			rec := *recs[key[len(locEmail):]]
			rec.Seq = state
			set[key] = encode(&rec)
		}

		return set, nil
	})
	if err != nil {
		return err
	}

	for _, guid := range guids {
		threads[recs[guid].Thread] = append(threads[recs[guid].Thread], guid)
	}

	return l.addToThreads(threads)
}

// found is what the locator knows of one message after checking every
// place it had for it against the mailboxes.
type found struct {
	metas  []model.Meta // one per place that still holds the message
	places []place      // where each of metas lives, in the same order
	// raw is the message itself for one that is not stored — a blob
	// Email/parse was asked about — and nil for every stored one.
	raw   []byte
	boxes map[string]bool
}

// resolve looks messages up by GUID and checks every place the locator
// names for them. A place that no longer holds the message is dropped
// from the result and from the locator: it can only have got there by a
// change the log did not enumerate, and a UID is never reused, so what
// is not there now will not be there again.
func (l *locator) resolve(guids []string) (map[string]*found, error) {
	keys := make([]string, len(guids))
	for i, guid := range guids {
		keys[i] = locEmail + guid
	}

	vals, err := l.ix.IndexGet(locSpace, keys)
	if err != nil {
		return nil, err
	}

	wanted := map[string]map[uint32]string{} // mailbox -> uid -> guid

	for key, v := range vals {
		for _, p := range decodeEmailRec(v).Places {
			if wanted[p.Mailbox] == nil {
				wanted[p.Mailbox] = map[uint32]string{}
			}

			wanted[p.Mailbox][p.UID] = key[len(locEmail):]
		}
	}

	out, stale, err := l.check(wanted)
	if err != nil {
		return nil, err
	}

	l.heal(stale)

	return out, nil
}

// check reads the places a lookup names, mailbox by mailbox.
func (l *locator) check(wanted map[string]map[uint32]string) (map[string]*found, map[string][]place, error) {
	out := map[string]*found{}
	stale := map[string][]place{}

	for mailbox, uids := range wanted {
		if l.allow != nil && !l.allow[mailbox] {
			// Not stale: the message is there, the user may not read it.
			continue
		}

		metas, err := l.read(mailbox, uids)
		if err != nil {
			return nil, nil, err
		}

		there := map[uint32]model.Meta{}
		for _, m := range metas {
			there[m.UID] = m
		}

		for uid, guid := range uids {
			m, ok := there[uid]
			if !ok || m.GUID != guid {
				stale[guid] = append(stale[guid], place{Mailbox: mailbox, UID: uid})
				continue
			}

			f := out[guid]
			if f == nil {
				f = &found{boxes: map[string]bool{}}
				out[guid] = f
			}

			f.metas = append(f.metas, m)
			f.places = append(f.places, place{Mailbox: mailbox, UID: uid})
			f.boxes[mailbox] = true
		}
	}

	return out, stale, nil
}

// read reads the records of the given UIDs of one mailbox, as the user
// sees them; a mailbox that is gone has none.
func (l *locator) read(mailbox string, uids map[uint32]string) ([]model.Meta, error) {
	list := make([]uint32, 0, len(uids))
	for uid := range uids {
		list = append(list, uid)
	}

	metas, err := l.u.GetMessages(mailbox, list)
	if err != nil && !errors.Is(err, model.ErrNoMailbox) {
		return nil, err
	}

	if l.overlay != nil {
		if err := l.overlay(mailbox, metas); err != nil {
			return nil, err
		}
	}

	return metas, nil
}

// heal drops places that turned out not to hold their message. It is a
// repair, not a requirement — a failure leaves the place to be checked
// and dropped again next time.
func (l *locator) heal(stale map[string][]place) {
	if len(stale) == 0 {
		return
	}

	keys := make([]string, 0, len(stale))
	for guid := range stale {
		keys = append(keys, locEmail+guid)
	}

	_ = l.ix.IndexApply(locSpace, keys, func(shard []string, cur map[string][]byte) (map[string][]byte, []string) {
		set := map[string][]byte{}

		for _, key := range shard {
			v, ok := cur[key]
			if !ok {
				continue
			}

			rec := decodeEmailRec(v)
			for _, p := range stale[key[len(locEmail):]] {
				rec.Places = without(rec.Places, p)
			}

			set[key] = encode(&rec)
		}

		return set, nil
	})
}

// threadMembers returns the messages of the given threads that still
// have a place, with their arrival times, for Thread/get.
func (l *locator) threadMembers(threads []string) (map[string][]member, error) {
	keys := make([]string, len(threads))
	for i, t := range threads {
		keys[i] = locThread + t
	}

	vals, err := l.ix.IndexGet(locSpace, keys)
	if err != nil {
		return nil, err
	}

	candidates := map[string][]string{} // thread -> guids
	all := []string{}

	for key, v := range vals {
		t := key[len(locThread):]
		candidates[t] = decodeThreadRec(v).Emails
		all = append(all, candidates[t]...)
	}

	recs, err := l.emailRecs(all)
	if err != nil {
		return nil, err
	}

	// A member counts only where one of its places still holds it — the
	// same check a message lookup makes — so that a thread does not list a
	// message Email/get would call not found.
	live, err := l.resolve(all)
	if err != nil {
		return nil, err
	}

	out := map[string][]member{}

	for t, guids := range candidates {
		for _, guid := range guids {
			rec, ok := recs[guid]
			if _, there := live[guid]; ok && there && rec.Thread == t {
				out[t] = append(out[t], member{guid: guid, received: rec.Received})
			}
		}
	}

	return out, nil
}

// member is one message of a thread.
type member struct {
	guid     string
	received int64
}

// emailRecs reads the locator records of the given messages.
func (l *locator) emailRecs(guids []string) (map[string]emailRec, error) {
	keys := make([]string, len(guids))
	for i, guid := range guids {
		keys[i] = locEmail + guid
	}

	vals, err := l.ix.IndexGet(locSpace, keys)
	if err != nil {
		return nil, err
	}

	out := make(map[string]emailRec, len(vals))

	for key, v := range vals {
		out[key[len(locEmail):]] = decodeEmailRec(v)
	}

	return out, nil
}
