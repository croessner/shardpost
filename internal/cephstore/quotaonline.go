package cephstore

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"shardpost/internal/model"
)

// The online quota recount (stage B16, temp/16 §4.4–§4.7): the run.
//
//	Q2 T0: one CAS sets the run key "qr" and an empty capture head
//	   "qd#" unless the account is busy (an import lock, a mailbox
//	   lock, a scrub that repairs, an import step, another run), and
//	   takes a snapshot R0 of the reservation intents ("bi:", "qi:").
//	Q3 barrier: wait until every intent of R0 is gone (or dead), then,
//	   per mailbox, until every MOVE intent that was there at the first
//	   read is gone (or dead). After that no entry without the run's
//	   tag comes into being (temp/16 §5.1, lemma 1).
//	Q4 listing: per counted mailbox, the header first (UIDNEXT and
//	   HIGHESTMODSEQ as of now), then the higher shards from the top,
//	   shard 0 last with its entries, its removal intents and the
//	   pending change records: an entry the run did not see on its
//	   shard counts when its "r:" says removed and not released (d and
//	   not f); one that is only announced is waited for while a live
//	   pending record names it.
//	Q5 fold: the captures so far are summed.
//	Q6 switch: one CAS replaces the counters by
//	   counted + arrivals - departures (deduplicated by mailbox and
//	   UID) and removes the run key and the captures.
//	Q7 report.
//
// Writers see the run key in every usage CAS and capture their change in
// the same write (quotacapture.go). The usage CAS is the only one that
// honours the hold of the switch; logins, flag writes and subscriptions
// never wait.

// recountHooks are test hooks: step is called with the name of a step
// of the run or of a writer and the mailbox id it concerns ("" none).
type recountHooks struct {
	step func(name, mailbox string)
}

// at calls the step hook if there is one.
func (u *User) at(step, mailbox string) {
	if u.hooks != nil && u.hooks.step != nil {
		u.hooks.step(step, mailbox)
	}
}

// Names of the hook steps.
const (
	hookAfterT0        = "after_t0"
	hookAfterBarrier   = "after_barrier"
	hookBeforeMailbox  = "before_mailbox"
	hookAfterHeader    = "after_header"
	hookAfterHigher    = "after_higher"
	hookAfterShard0    = "after_shard0"
	hookBeforeSwitch   = "before_switch"
	hookAfterSwitch    = "after_switch"
	hookAfterUnlink    = "after_unlink"
	hookAfterRelease   = "after_release"
	hookAfterFlag      = "after_flag"
	hookBeforeGate     = "before_gate"
	hookBeforeBarrier2 = "before_move_barrier"
)

// barrierPoll is how often the barrier looks again.
var barrierPoll = 2 * time.Second

// pendingPoll is how often the listing looks again at shard 0 while a
// pending change names an entry it has not seen.
var pendingPoll = 200 * time.Millisecond

var _ model.QuotaOnlineRecounter = (*User)(nil)

// quotaRecount is one online recount.
type quotaRecount struct {
	u    *User
	req  model.QuotaRecountRequest
	tok  string
	h    string
	cfg  QuotaRecountConfig
	rep  model.QuotaRecountReport
	t0   time.Time
	dead time.Duration
	// r0 are the reservation intents at T0; mailboxes the registered
	// mailboxes and unreleased tombstones the listing covers.
	r0        []string
	listed    []listedMailbox
	switching atomic.Bool
}

// listedMailbox is one mailbox of the listing with its unmarked entries
// (UID -> size).
type listedMailbox struct {
	id     string
	name   string
	counts bool
	tomb   bool
	seen   map[uint32]int64
}

// RecountQuotaOnline implements model.QuotaOnlineRecounter.
func (u *User) RecountQuotaOnline(req model.QuotaRecountRequest) (model.QuotaRecountReport, error) {
	if req.Tok == "" {
		return model.QuotaRecountReport{}, errors.New("online recount: token required")
	}

	r := &quotaRecount{u: u, req: req, tok: req.Tok, h: quotaRunHash(req.Tok), cfg: u.recount, dead: u.recount.DeadIntent}
	if req.Dead > 0 {
		r.dead = req.Dead
	}

	r.rep.Mode = model.QuotaRecountOnline

	u.recountRun.Store(r)
	defer u.recountRun.Store((*quotaRecount)(nil))

	err := r.run()

	var busy *model.QuotaRecountBusyError

	switch {
	case err == nil:
		r.rep.Result = model.QuotaRecountOK
		mh.recountRun(r.rep.Mode, r.rep.Result, "")
	case errors.As(err, &busy):
		r.rep.Result, r.rep.Reason, r.rep.OldestKey = model.QuotaRecountBusy, busy.Reason, busy.Key
		mh.recountRun(r.rep.Mode, r.rep.Result, busy.Reason)
	case errors.Is(err, model.ErrQuotaRecountLost):
		r.rep.Result = model.QuotaRecountLostRes
		mh.recountRun(r.rep.Mode, r.rep.Result, "")
	default:
		mh.recountRun(r.rep.Mode, "error", "")
	}

	return r.rep, err
}

// run is Q2 to Q7; on every error but a lost run the run key and the
// captures of this token are cleared.
func (r *quotaRecount) run() (err error) {
	defer func() {
		if err != nil && !errors.Is(err, model.ErrQuotaRecountLost) {
			if cerr := r.u.ClearQuotaRecount(r.tok); cerr != nil {
				r.u.log.Warn("online recount: run key not cleared", "err", cerr)
			}
		}
	}()

	if err := r.fence(); err != nil {
		return err
	}

	if err := r.start(); err != nil {
		return err
	}

	r.u.at(hookAfterT0, "")

	if err := r.phase("barrier", r.barrier); err != nil {
		return err
	}

	r.u.at(hookAfterBarrier, "")

	if err := r.phase("list", r.list); err != nil {
		return err
	}

	if err := r.fence(); err != nil {
		return err
	}

	r.u.at(hookBeforeSwitch, "")

	if err := r.phase("switch", r.switchCounters); err != nil {
		return err
	}

	r.u.at(hookAfterSwitch, "")

	return nil
}

// fence calls the request's fence.
func (r *quotaRecount) fence() error {
	if r.req.Fence == nil {
		return nil
	}

	return r.req.Fence()
}

// progress reports the phase.
func (r *quotaRecount) progress(phase string, done, total int) {
	if r.req.Progress != nil {
		r.req.Progress(phase, done, total)
	}
}

// phase runs fn as the timed phase name.
func (r *quotaRecount) phase(name string, fn func() error) error {
	start := time.Now()
	err := fn()

	mh.recountPhase(name, time.Since(start))

	if name == "barrier" {
		r.rep.BarrierSeconds = time.Since(start).Seconds()
	}

	return err
}

// busy ends the run as busy.
func busy(reason, key string) error { return &model.QuotaRecountBusyError{Reason: reason, Key: key} }

// --- Q2 -----------------------------------------------------------------------

// start is the T0 CAS: it reads everything that makes the account busy
// in one op and sets the run key and the capture head; the rests of an
// earlier run go in the same write.
func (r *quotaRecount) start() error {
	var (
		reason, key string
		r0          []string
	)

	now := time.Now()
	exp := now.Add(2 * r.leaseTTL())

	err := casOmapPrefix(r.u, r.u.idx, oidRegistry,
		[]string{regQuotaRun, regQuotaCaptureHead, regImportLock},
		[]string{regMailboxLockPrefix, regScrubMarkPrefix, regImportBlobPrefix, regBlobPrefix, regQuotaIntentPrefix,
			regQuotaReleasePrefix, regQuotaMailboxPrefix},
		func(cur map[string][]byte, pages map[string]prefixPage) (map[string][]byte, []string) {
			reason, key = r.startBusy(cur, pages, now)
			if reason != "" {
				return nil, nil
			}

			var ok bool

			if r0, ok = intentSnapshot(pages); !ok {
				// More open intents than one page: the snapshot would
				// be incomplete, and so would the barrier.
				reason, key = model.BusyIntent, regBlobPrefix
				return nil, nil
			}

			rm := pageKeys(pages, regQuotaReleasePrefix, regQuotaMailboxPrefix)
			r.t0 = now

			set := map[string][]byte{
				regQuotaRun:         encode(quotaRun{Tok: r.tok, H: r.h, T0: now.Unix(), Exp: exp.Unix()}),
				regQuotaCaptureHead: encode(quotaCaptureHead{Tok: r.tok}),
			}

			return set, rm
		})
	if err != nil {
		return fmt.Errorf("online recount: start: %w", err)
	}

	if reason != "" {
		return busy(reason, key)
	}

	sort.Strings(r0)
	r.r0 = r0

	return nil
}

// intentSnapshot returns the keys of the reservation intents ("bi:",
// "qi:") of the pages; ok is false when a page was not all.
func intentSnapshot(pages map[string]prefixPage) ([]string, bool) {
	var r0 []string

	for _, p := range []string{regBlobPrefix, regQuotaIntentPrefix} {
		if pages[p].more {
			return nil, false
		}

		for k := range pages[p].vals {
			r0 = append(r0, k)
		}
	}

	return r0, true
}

// pageKeys returns the keys of the pages of the prefixes.
func pageKeys(pages map[string]prefixPage, prefixes ...string) []string {
	keys := []string{}

	for _, p := range prefixes {
		for k := range pages[p].vals {
			keys = append(keys, k)
		}
	}

	return keys
}

// startBusy reports what keeps the run from starting: a foreign live
// account lock, a mailbox lock before destroying, a live scrub mark, an
// import step or another live run ("" nothing).
func (r *quotaRecount) startBusy(cur map[string][]byte, pages map[string]prefixPage, now time.Time) (string, string) {
	if v, ok := cur[regImportLock]; ok {
		var l importLock
		if json.Unmarshal(v, &l) == nil && time.Unix(l.Expires, 0).After(now) {
			return model.BusyImportLock, l.Session
		}
	}

	if k, blocked := mailboxLocksBlock(pages[regMailboxLockPrefix], "", now); blocked {
		return model.BusyMailboxLck, k
	}

	for k, v := range pages[regScrubMarkPrefix].vals {
		var m scrubMark
		if json.Unmarshal(v, &m) == nil && time.Unix(m.Exp, 0).After(now) {
			return model.BusyScrub, k
		}
	}

	for k := range pages[regImportBlobPrefix].vals {
		return model.BusyImportLock, k
	}

	if run, ok := parseQuotaRun(cur[regQuotaRun]); ok && run.Tok != r.tok && run.alive(now) {
		return model.BusyForeignRun, run.Tok
	}

	return "", ""
}

// leaseTTL is the time the run key lives without a renewal.
func (r *quotaRecount) leaseTTL() time.Duration {
	if r.req.Barrier > 0 {
		return max(r.req.Barrier, time.Minute)
	}

	return max(r.cfg.Barrier, time.Minute)
}

// --- Q3 -----------------------------------------------------------------------

// barrier waits for the operations of before T0 (temp/16 §4.4 Q3).
func (r *quotaRecount) barrier() error {
	limit := r.cfg.Barrier
	if r.req.Barrier > 0 {
		limit = r.req.Barrier
	}

	deadline := time.Now().Add(limit)

	// 1. The reservation intents of R0.
	if err := r.waitUntil(deadline, model.BusyIntent, r.openIntents); err != nil {
		return err
	}

	// 2. The MOVE intents on shard 0 of every mailbox, as of the first
	//    read of each.
	boxes, err := r.mailboxes()
	if err != nil {
		return err
	}

	r.u.at(hookBeforeBarrier2, "")

	snap := map[string]map[string]bool{} // mailbox id -> intent ids at the first read

	return r.waitUntil(deadline, model.BusyMoveIntent, func() (int, string, error) { return r.openMoves(boxes, snap) })
}

// waitUntil polls open until it reports nothing to wait for; after the
// deadline the run is busy with the reason and the oldest key.
func (r *quotaRecount) waitUntil(deadline time.Time, reason string, open func() (int, string, error)) error {
	for {
		if err := r.alive(); err != nil {
			return err
		}

		waiting, oldest, err := open()
		if err != nil {
			return err
		}

		if waiting == 0 {
			return nil
		}

		if time.Now().After(deadline) {
			return busy(reason, oldest)
		}

		r.progress("barrier", 0, waiting)
		time.Sleep(barrierPoll)
	}
}

// deadIntent counts an intent the run gave up waiting for.
func (r *quotaRecount) deadIntent() {
	r.rep.DeadIntents++

	mh.recountDeadIntent()
}

// openIntents reads the intents of R0 that are still there and younger
// than the dead-intent time; dead ones are counted and dropped from R0.
func (r *quotaRecount) openIntents() (int, string, error) {
	if len(r.r0) == 0 {
		return 0, "", nil
	}

	vals, err := getOmapKeys(r.u.idx, oidRegistry, r.r0)
	if err != nil {
		return 0, "", fmt.Errorf("online recount: barrier: %w", err)
	}

	now := time.Now()
	keep := r.r0[:0]

	var (
		oldestKey string
		oldest    time.Time
	)

	for _, k := range r.r0 {
		v, ok := vals[k]
		if !ok {
			continue
		}

		t := intentTime(v)
		if now.Sub(t) >= r.dead {
			r.deadIntent()
			continue
		}

		keep = append(keep, k)

		if oldestKey == "" || t.Before(oldest) {
			oldestKey, oldest = k, t
		}
	}

	r.r0 = keep

	return len(keep), oldestKey, nil
}

// openMoves reads the MOVE intents of the mailboxes; snap keeps what the
// first read of each found, and only those are waited for.
func (r *quotaRecount) openMoves(boxes []listedMailbox, snap map[string]map[string]bool) (int, string, error) {
	waiting := 0
	now := time.Now()

	var (
		oldestKey string
		oldest    time.Time
	)

	for _, b := range boxes {
		vals, err := r.u.idx.GetAllOmapValues(mboxOID(b.id), "", keyIntentPrefix, omapPageSize)
		if err != nil {
			if errCode(err) == syscall.ENOENT {
				continue
			}

			return 0, "", fmt.Errorf("online recount: move intents of %s: %w", b.name, err)
		}

		first, ok := snap[b.id]
		if !ok {
			first = make(map[string]bool, len(vals))
			for k := range vals {
				first[k] = true
			}

			snap[b.id] = first
		}

		for k := range first {
			v, ok := vals[k]
			if !ok {
				delete(first, k)
				continue
			}

			var in moveIntent

			_ = json.Unmarshal(v, &in)

			t := time.Unix(in.Started, 0)
			if now.Sub(t) >= r.dead {
				delete(first, k)
				r.deadIntent()

				continue
			}

			waiting++

			if oldestKey == "" || t.Before(oldest) {
				oldestKey, oldest = b.id+"/"+k, t
			}
		}
	}

	return waiting, oldestKey, nil
}

// mailboxes lists the registered mailboxes and the unreleased deletion
// tombstones (q = 0) younger than the intent grace: what the listing
// covers.
func (r *quotaRecount) mailboxes() ([]listedMailbox, error) {
	boxes, err := r.u.ListMailboxes()
	if err != nil {
		return nil, err
	}

	out := make([]listedMailbox, 0, len(boxes))
	for i := range boxes {
		out = append(out, listedMailbox{id: boxes[i].ID, name: boxes[i].Name, counts: r.u.rules.Counts(&boxes[i])})
	}

	tombs, err := r.u.idx.GetAllOmapValues(oidRegistry, "", regDeletePrefix, omapPageSize)
	if err != nil && errCode(err) != syscall.ENOENT {
		return nil, fmt.Errorf("online recount: tombstones: %w", err)
	}

	cut := time.Now().Add(-r.u.intentGrace())

	for k, v := range tombs {
		if !tombstoneUnreleased(v) || !intentTime(v).After(cut) {
			continue
		}

		var t mailboxIntent

		_ = json.Unmarshal(v, &t)

		id := strings.TrimPrefix(k, regDeletePrefix)
		mb := Mailbox{Name: t.Name, MailboxRec: MailboxRec{ID: id}}

		out = append(out, listedMailbox{id: id, name: t.Name, counts: r.u.rules.Counts(&mb), tomb: true})
	}

	return out, nil
}

// alive reports ErrQuotaRecountLost when the run key is no longer this
// run's.
func (r *quotaRecount) alive() error {
	vals, err := getOmapKeys(r.u.idx, oidRegistry, []string{regQuotaRun})
	if err != nil {
		return err
	}

	if run, ok := parseQuotaRun(vals[regQuotaRun]); !ok || run.Tok != r.tok {
		return model.ErrQuotaRecountLost
	}

	return nil
}

// --- Q4 -----------------------------------------------------------------------

// list reads the unmarked entries of every counted mailbox.
func (r *quotaRecount) list() error {
	boxes, err := r.mailboxes()
	if err != nil {
		return err
	}

	r.listed = boxes
	r.rep.ListedMailboxes = len(boxes)

	for i := range r.listed {
		if err := r.fence(); err != nil {
			return err
		}

		b := &r.listed[i]
		if !b.counts {
			continue
		}

		r.u.at(hookBeforeMailbox, b.id)

		if err := r.listMailbox(b); err != nil {
			return err
		}

		r.progress("list", i+1, len(r.listed))
	}

	return nil
}

// listMailbox reads one mailbox: the header, the higher shards from the
// top, shard 0 last (temp/16 §4.4 Q4).
func (r *quotaRecount) listMailbox(b *listedMailbox) error {
	u := r.u
	b.seen = map[uint32]int64{}

	hv, ver, err := u.getKeysVer(u.idx, mboxOID(b.id), []string{keyUIDNext, keyRetired, keyShardSize, keyModSeq})
	if err != nil {
		return fmt.Errorf("online recount: header of %s: %w", b.name, err)
	}

	if ver == 0 {
		return nil // the object is gone: a finished deletion
	}

	uidNext := retiredUIDNext(hv)
	if uidNext == 0 {
		return nil
	}

	ss, _ := strconv.ParseUint(string(hv[keyShardSize]), 10, 32)
	if ss == 0 {
		ss = DefaultShardSize
	}

	modseq, _ := strconv.ParseUint(string(hv[keyModSeq]), 10, 64)

	u.at(hookAfterHeader, b.id)

	for s := shardOf(max(uidNext-1, 1), uint32(ss)); s >= 1; s-- {
		vals, err := u.idx.GetAllOmapValues(u.shardOID(b.id, s), "", keyMsgPrefix, omapPageSize)
		if err != nil && errCode(err) != syscall.ENOENT {
			return fmt.Errorf("online recount: shard %d of %s: %w", s, b.name, err)
		}

		r.noteEntries(b, vals, uidNext)
	}

	u.at(hookAfterHigher, b.id)

	vals, err := u.idx.GetAllOmapValues(mboxOID(b.id), "", keyMsgPrefix, omapPageSize)
	if err != nil && errCode(err) != syscall.ENOENT {
		return fmt.Errorf("online recount: shard 0 of %s: %w", b.name, err)
	}

	r.noteEntries(b, vals, uidNext)

	if err := r.noteRemovals(b, modseq); err != nil {
		return err
	}

	u.at(hookAfterShard0, b.id)

	return nil
}

// noteEntries adds the unmarked entries of a shard below uidNext.
func (r *quotaRecount) noteEntries(b *listedMailbox, vals map[string][]byte, uidNext uint32) {
	for _, v := range vals {
		var m Meta
		if json.Unmarshal(v, &m) != nil || m.UID >= uidNext || m.QuotaTag == r.h {
			continue
		}

		b.seen[m.UID] = m.Size
	}
}

// noteRemovals reads the removal intents and the live pending change
// records of shard 0 (after the entries): an intent with d and without
// f counts; one without d that a live pending record names is read
// again until it has d, the record is committed or the record expired.
func (r *quotaRecount) noteRemovals(b *listedMailbox, modseq uint64) error {
	u := r.u

	from, err := u.pendingScanStart(b.id, modseq)
	if err != nil && !errors.Is(err, ErrNoMailbox) {
		return fmt.Errorf("online recount: pending records of %s: %w", b.name, err)
	}

	deadline := time.Now().Add(pendingMaxAge)

	for {
		announced, err := r.noteIntents(b)
		if err != nil {
			return err
		}

		if len(announced) == 0 || time.Now().After(deadline) {
			return nil
		}

		pending, err := u.livePending(b.id, from)
		if err != nil {
			return fmt.Errorf("online recount: pending records of %s: %w", b.name, err)
		}

		if !anyPending(announced, pending) {
			// No live record names one: the announcements are stale,
			// or their removal was committed between the two reads;
			// one more read of the intents tells (d is then set).
			_, err := r.noteIntents(b)

			return err
		}

		time.Sleep(pendingPoll)
	}
}

// noteIntents reads the removal intents of shard 0 once: an intent with
// d and without f of an entry not seen counts; the UIDs only announced
// (no d) are returned.
func (r *quotaRecount) noteIntents(b *listedMailbox) (map[uint32]bool, error) {
	removals, err := r.u.removeIntents(b.id)
	if err != nil {
		return nil, fmt.Errorf("online recount: removal intents of %s: %w", b.name, err)
	}

	announced := map[uint32]bool{}

	for k, ri := range removals {
		uid := uidOfKey(strings.Replace(k, keyRemovePrefix, keyMsgPrefix, 1))
		if _, ok := b.seen[uid]; ok {
			continue
		}

		switch {
		case ri.Removed && !ri.Released:
			b.seen[uid] = ri.Size
		case !ri.Removed && ri.GUID != "":
			announced[uid] = true
		}
	}

	return announced, nil
}

// anyPending reports whether a live pending record names one of the
// announced UIDs.
func anyPending(announced, pending map[uint32]bool) bool {
	for uid := range announced {
		if pending[uid] {
			return true
		}
	}

	return false
}

// livePending returns the UIDs the live pending change records of a
// mailbox name, from the change record from on.
func (u *User) livePending(id string, from uint64) (map[uint32]bool, error) {
	vals, err := u.idx.GetAllOmapValues(mboxOID(id), chgKey(from), keyChgPrefix, omapPageSize)
	if err != nil {
		if errCode(err) == syscall.ENOENT {
			return nil, nil
		}

		return nil, err
	}

	out := map[uint32]bool{}

	for _, v := range vals {
		var rec changeRec
		if json.Unmarshal(v, &rec) != nil || !rec.Pending || time.Since(time.Unix(rec.Time, 0)) >= pendingMaxAge {
			continue
		}

		for _, uid := range rec.UIDs {
			out[uid] = true
		}
	}

	return out, nil
}

// --- Q5, Q6 -------------------------------------------------------------------

// departures are the captured departures of a run: by mailbox the UIDs
// released (deduplicated), the sum of the tagged ones, and the
// mailboxes released as a whole.
type departures struct {
	byBox  map[string]map[uint32]bool
	tagged Quota
	seen   map[string]bool // mailbox id "/" uid of the tagged ones
	boxes  map[string]bool
}

func newDepartures() *departures {
	return &departures{byBox: map[string]map[uint32]bool{}, seen: map[string]bool{}, boxes: map[string]bool{}}
}

// add takes the entries of one "qd:" value.
func (d *departures) add(rel quotaRelease) {
	uids := d.byBox[rel.M]
	if uids == nil {
		uids = map[uint32]bool{}
		d.byBox[rel.M] = uids
	}

	for _, e := range rel.E {
		uid := uint32(e[0])
		uids[uid] = true

		if e[2] == 1 {
			d.tag(rel.M, uid, e[1])
		}
	}
}

// addMailbox takes one "qm:" value.
func (d *departures) addMailbox(id string, rel quotaMailboxRelease) {
	d.boxes[id] = true

	for _, e := range rel.T {
		d.tag(id, uint32(e[0]), e[1])
	}
}

// tag counts a tagged departure once.
func (d *departures) tag(id string, uid uint32, size int64) {
	k := id + "/" + strconv.FormatUint(uint64(uid), 10)
	if d.seen[k] {
		return
	}

	d.seen[k] = true
	d.tagged.Bytes += size
	d.tagged.Msgs++
}

// clone copies the departures (the switch adds the fresh captures of a
// round to a copy of the folded ones).
func (d *departures) clone() *departures {
	c := newDepartures()
	c.tagged = d.tagged

	for id, uids := range d.byBox {
		m := make(map[uint32]bool, len(uids))
		for uid := range uids {
			m[uid] = true
		}

		c.byBox[id] = m
	}

	for k := range d.seen {
		c.seen[k] = true
	}

	for k := range d.boxes {
		c.boxes[k] = true
	}

	return c
}

// total is E for the listing with the departures and the arrivals.
func (r *quotaRecount) total(d *departures, head quotaCaptureHead) Quota {
	var e Quota

	for i := range r.listed {
		b := &r.listed[i]
		if !b.counts || d.boxes[b.id] {
			continue
		}

		gone := d.byBox[b.id]

		for uid, size := range b.seen {
			if gone[uid] {
				continue
			}

			e.Bytes += size
			e.Msgs++
		}
	}

	e.Bytes += head.PB - d.tagged.Bytes
	e.Msgs += head.PM - d.tagged.Msgs

	return Quota{Bytes: max(e.Bytes, 0), Msgs: max(e.Msgs, 0)}
}

// switchCounters folds the captures and replaces the counters in one
// CAS (temp/16 §4.4 Q5, Q6, §4.6).
func (r *quotaRecount) switchCounters() error {
	folded, upTo, err := r.fold()
	if err != nil {
		return err
	}

	r.switching.Store(true)
	defer r.switching.Store(false)

	var lastErr error

	for attempt := 0; attempt < r.cfg.SwitchAttempts; attempt++ {
		switch err := r.switchOnce(folded, upTo); {
		case err == nil:
			return nil
		case !isCASContention(err):
			return err
		default:
			lastErr = err
		}
	}

	if r.cfg.Hold <= 0 {
		return busy(model.BusySwitch, lastErr.Error())
	}

	// The hold: the usage writers wait, everybody else goes on.
	until := time.Now().Add(r.cfg.Hold)
	if err := r.setHold(until); err != nil {
		return err
	}

	r.rep.HoldMS = r.cfg.Hold.Milliseconds()

	for time.Now().Before(until) {
		switch err := r.switchOnce(folded, upTo); {
		case err == nil:
			return nil
		case !isCASContention(err):
			return err
		}
	}

	return busy(model.BusySwitch, "the switch did not go through during the hold")
}

// isCASContention reports a CAS that lost every round.
func isCASContention(err error) bool {
	return err != nil && strings.Contains(err.Error(), "too many concurrent writers")
}

// setHold sets the hold on the run key.
func (r *quotaRecount) setHold(until time.Time) error {
	lost := false

	err := casOmap(r.u, r.u.idx, oidRegistry, []string{regQuotaRun}, func(cur map[string][]byte) (map[string][]byte, []string) {
		run, ok := parseQuotaRun(cur[regQuotaRun])
		if lost = !ok || run.Tok != r.tok; lost {
			return nil, nil
		}

		run.Hold = until.UnixMilli()

		return map[string][]byte{regQuotaRun: encode(*run)}, nil
	})
	if err != nil {
		return err
	}

	if lost {
		return model.ErrQuotaRecountLost
	}

	return nil
}

// fold reads the captures written so far and sums them; upTo is the
// highest "qd:" index folded.
func (r *quotaRecount) fold() (*departures, int64, error) {
	start := time.Now()
	defer func() { mh.recountPhase("fold", time.Since(start)) }()

	d := newDepartures()

	vals, err := r.u.idx.GetAllOmapValues(oidRegistry, "", regQuotaReleasePrefix, omapPageSize)
	if err != nil && errCode(err) != syscall.ENOENT {
		return nil, 0, fmt.Errorf("online recount: fold: %w", err)
	}

	var upTo int64

	for k, v := range vals {
		var rel quotaRelease
		if json.Unmarshal(v, &rel) != nil || rel.H != r.h {
			continue
		}

		d.add(rel)

		upTo = max(upTo, quotaReleaseIndex(k))
	}

	return d, upTo, nil
}

// switchOnce is one switch CAS: it reads the counters, the capture head
// and the captures after upTo under one version and writes E.
func (r *quotaRecount) switchOnce(folded *departures, upTo int64) error {
	var (
		lost          bool
		stored, limit Quota
		e             Quota
	)

	err := casOmapPrefix(r.u, r.u.idx, oidRegistry, quotaKeys,
		[]string{regQuotaReleasePrefix, regQuotaMailboxPrefix, regMboxPrefix, regDeletePrefix},
		func(cur map[string][]byte, pages map[string]prefixPage) (map[string][]byte, []string) {
			lost = false

			if run, ok := parseQuotaRun(cur[regQuotaRun]); !ok || run.Tok != r.tok {
				lost = true
				return nil, nil
			}

			head := r.captureHead(cur)

			d, rm, ok := r.freshCaptures(folded, upTo, pages)
			if !ok || !r.countsUnchanged(pages[regMboxPrefix], pages[regDeletePrefix], d) {
				lost = true
				return nil, nil
			}

			stored, limit = r.u.quotaState(cur)
			e = r.total(d, head)
			r.rep.SwitchRounds++

			return usageValues(e), append(rm, regQuotaRun, regQuotaCaptureHead)
		})
	if err != nil {
		return err
	}

	if lost {
		return model.ErrQuotaRecountLost
	}

	r.rep.Counted, r.rep.Stored, r.rep.Differs = e, stored, e != stored
	mh.recountDrift(absDiff(e.Bytes, stored.Bytes), absDiff(e.Msgs, stored.Msgs))
	r.u.quotaChanged(stored, e, limit)

	return nil
}

// captureHead reads the capture head of this run (empty for another
// token) and notes its counts in the report.
func (r *quotaRecount) captureHead(cur map[string][]byte) quotaCaptureHead {
	var head quotaCaptureHead

	_ = json.Unmarshal(cur[regQuotaCaptureHead], &head)

	if head.Tok != r.tok {
		head = quotaCaptureHead{}
	}

	r.rep.CaptureKeys, r.rep.CaptureEntries = head.N, head.NE

	return head
}

// freshCaptures adds the captures written after the fold to a copy of
// the folded departures and returns the keys of all captures (foreign
// ones included, they go with the switch); ok is false when the rest of
// a page could not be read.
func (r *quotaRecount) freshCaptures(folded *departures, upTo int64, pages map[string]prefixPage) (*departures, []string, bool) {
	d := folded.clone()

	var rm []string

	releases, ok := r.allOf(pages[regQuotaReleasePrefix], regQuotaReleasePrefix)
	if !ok {
		return nil, nil, false
	}

	for k, v := range releases {
		rm = append(rm, k)

		var rel quotaRelease
		if json.Unmarshal(v, &rel) == nil && rel.H == r.h && quotaReleaseIndex(k) > upTo {
			d.add(rel)
		}
	}

	mailboxes, ok := r.allOf(pages[regQuotaMailboxPrefix], regQuotaMailboxPrefix)
	if !ok {
		return nil, nil, false
	}

	for k, v := range mailboxes {
		rm = append(rm, k)

		var rel quotaMailboxRelease
		if json.Unmarshal(v, &rel) == nil && rel.H == r.h {
			d.addMailbox(strings.TrimPrefix(k, regQuotaMailboxPrefix), rel)
		}
	}

	return d, rm, true
}

// allOf returns the values of a prefix: the page, completed by a read
// of the rest when the page was not all (the version check covers it).
func (r *quotaRecount) allOf(page prefixPage, prefix string) (map[string][]byte, bool) {
	if !page.more {
		return page.vals, true
	}

	vals, err := r.u.idx.GetAllOmapValues(oidRegistry, "", prefix, omapPageSize)
	if err != nil {
		return nil, false
	}

	return vals, true
}

// countsUnchanged checks that every listed mailbox is still counted as
// it was: by its record (found by id), else by its tombstone or its
// release as a whole; with more records than one page a mailbox whose
// record is not in the page is taken as unchanged.
func (r *quotaRecount) countsUnchanged(recs, tombs prefixPage, d *departures) bool {
	byID := map[string]Mailbox{}

	for k, v := range recs.vals {
		var rec MailboxRec
		if json.Unmarshal(v, &rec) == nil {
			byID[rec.ID] = Mailbox{Name: strings.TrimPrefix(k, regMboxPrefix), MailboxRec: rec}
		}
	}

	for i := range r.listed {
		b := &r.listed[i]

		if mb, ok := byID[b.id]; ok {
			if r.u.rules.Counts(&mb) != b.counts {
				return false
			}

			continue
		}

		if _, ok := tombs.vals[deleteKey(b.id)]; ok || d.boxes[b.id] || recs.more || tombs.more {
			continue
		}

		return false
	}

	return true
}

func absDiff(a, b int64) int64 {
	if a > b {
		return a - b
	}

	return b - a
}

// --- renewal and clean-up -----------------------------------------------------

// RenewQuotaRecount implements model.QuotaOnlineRecounter. While the run
// switches the renewal writes nothing: it would only make the switch
// lose its rounds.
func (u *User) RenewQuotaRecount(tok string, ttl time.Duration) error {
	if r, _ := u.recountRun.Load().(*quotaRecount); r != nil && r.tok == tok && r.switching.Load() {
		return nil
	}

	lost := false
	exp := time.Now().Add(ttl)

	err := casOmap(u, u.idx, oidRegistry, []string{regQuotaRun}, func(cur map[string][]byte) (map[string][]byte, []string) {
		run, ok := parseQuotaRun(cur[regQuotaRun])
		if lost = !ok || run.Tok != tok; lost {
			return nil, nil
		}

		run.Exp = exp.Unix()

		return map[string][]byte{regQuotaRun: encode(*run)}, nil
	})
	if err != nil {
		return err
	}

	if lost {
		return model.ErrQuotaRecountLost
	}

	return nil
}

// ClearQuotaRecount implements model.QuotaOnlineRecounter: the run key
// of tok (or an expired one) and its captures go in version-checked
// writes only, each of which checks that the captures do not belong to
// another live run (temp/16 §4.7). tok "" clears any run: for the
// holder of the account lock and the scrub under its repair mark, whom
// no run may outlive.
func (u *User) ClearQuotaRecount(tok string) error {
	for page := 0; page < 1024; page++ {
		more, wrote := false, false

		err := casOmapPrefix(u, u.idx, oidRegistry, []string{regQuotaRun, regQuotaCaptureHead},
			[]string{regQuotaReleasePrefix, regQuotaMailboxPrefix},
			func(cur map[string][]byte, pages map[string]prefixPage) (map[string][]byte, []string) {
				more, wrote = false, false

				if run, ok := parseQuotaRun(cur[regQuotaRun]); ok && run.alive(time.Now()) && run.Tok != tok && tok != "" {
					return nil, nil // another run's: not ours to clear
				}

				rm := recountCaptureKeys(cur, pages, &more)
				wrote = len(rm) > 0

				return nil, rm
			})
		if err != nil {
			return fmt.Errorf("clear online recount: %w", err)
		}

		if !more || !wrote {
			return nil
		}

		// Further pages: the run key is gone now, so a new run may start
		// meanwhile; the check above then finds its live key and stops.
	}

	return errors.New("clear online recount: too many pages")
}

// recountCaptureKeys returns the run key, the capture head and the capture keys
// of the pages; more reports a page that was not all.
func recountCaptureKeys(cur map[string][]byte, pages map[string]prefixPage, more *bool) []string {
	rm := []string{}

	for _, k := range []string{regQuotaRun, regQuotaCaptureHead} {
		if _, ok := cur[k]; ok {
			rm = append(rm, k)
		}
	}

	for _, p := range pages {
		*more = *more || p.more

		for k := range p.vals {
			rm = append(rm, k)
		}
	}

	return rm
}

// quotaCaptureLeftover reports captures without a live run key (the
// scrub's finding quota_capture_leftover).
func (u *User) quotaCaptureLeftover() (bool, error) {
	cur, pages, _, err := u.getKeysPrefix(u.idx, oidRegistry, []string{regQuotaRun, regQuotaCaptureHead},
		[]string{regQuotaReleasePrefix, regQuotaMailboxPrefix})
	if err != nil {
		return false, err
	}

	if run, ok := parseQuotaRun(cur[regQuotaRun]); ok && run.alive(time.Now()) {
		return false, nil
	}

	_, head := cur[regQuotaCaptureHead]

	return head || len(pages[regQuotaReleasePrefix].vals) > 0 || len(pages[regQuotaMailboxPrefix].vals) > 0, nil
}

// --- the MOVE gate ------------------------------------------------------------

// moveGate is the gate of a MOVE between two counted mailboxes, after
// its intent is written and before its first link (temp/16 §4.2): one
// read of the run key and the destination's record; while a run is
// live, a CAS that captures the batch as arrivals and its source
// entries as departures, so that the destination entries carry the
// run's tag and the source entries are taken out whether the listing
// saw them or not. It returns the tag the destination entries get.
func (u *User) moveGate(smb, dmb *Mailbox, metas []Meta) (string, error) {
	u.at(hookBeforeGate, smb.ID)

	vals, err := getOmapKeys(u.idx, oidRegistry, []string{regQuotaRun, regKey(dmb.Name)})
	if err != nil {
		return "", fmt.Errorf("move gate: %w", err)
	}

	if lockedRecord(vals[regKey(dmb.Name)], "", time.Now()) {
		return "", ErrMailboxLocked
	}

	if run, ok := parseQuotaRun(vals[regQuotaRun]); !ok || !run.alive(time.Now()) {
		return "", nil
	}

	var (
		tag       string
		locked    bool
		holdUntil time.Time
	)

	keys := append(append([]string(nil), quotaKeys...), regKey(dmb.Name))

	for round := 0; round < 3; round++ {
		holdUntil = time.Time{}

		err := casOmap(u, u.idx, oidRegistry, keys, func(cur map[string][]byte) (map[string][]byte, []string) {
			tag, locked, holdUntil = gateRound(cur, dmb.Name)
			if tag == "" {
				return nil, nil
			}

			return u.gateCapture(cur, smb.ID, metas)
		})
		if err != nil {
			return "", fmt.Errorf("move gate: %w", err)
		}

		if holdUntil.IsZero() {
			break
		}

		u.recountHeld(holdUntil)
	}

	if locked {
		return "", ErrMailboxLocked
	}

	if tag != "" {
		mh.recountCapture("move")
	}

	return tag, nil
}

// gateCapture is the write of the gate CAS: the batch as arrivals, the
// source entries as departures.
func (u *User) gateCapture(cur map[string][]byte, src string, metas []Meta) (map[string][]byte, []string) {
	set := map[string][]byte{}
	st := readCapture(cur)
	st.arrive(sizeOf(metas))
	st.release(set, src, metas, u.recount.ReleaseChunk)

	set, rm, _ := st.finish(set, nil, u.recount)

	return set, rm
}

// gateRound decides one round of the gate CAS: the tag of the live run
// ("" none, or nothing to write), whether the destination is locked and
// the hold to wait for.
func gateRound(cur map[string][]byte, dest string) (string, bool, time.Time) {
	if lockedRecord(cur[regKey(dest)], "", time.Now()) {
		return "", true, time.Time{}
	}

	st := readCapture(cur)
	if !st.live() {
		return "", false, time.Time{}
	}

	if h := st.holdUntil(); !h.IsZero() {
		return "", false, h
	}

	return st.hash(), false, time.Time{}
}

// moveGateSkipped takes the entries of a gated MOVE that turned out
// expunged (ErrGone) out of the arrivals again (they are not linked).
func (u *User) moveGateSkipped(tag string, skipped []Meta) {
	q := sizeOf(skipped)

	if err := u.applyUsage(usageChange{tag: tag, d: Quota{Bytes: -q.Bytes, Msgs: -q.Msgs}, arrivalsOnly: true}); err != nil {
		u.log.Warn("move gate: correction failed", "err", err)
	}
}
