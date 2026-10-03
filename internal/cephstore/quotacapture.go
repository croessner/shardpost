package cephstore

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Online quota recount, the write side (stage B16, temp/16 §4.1–§4.3).
//
// While a run key "qr" stands in the registry, every compare-and-swap
// that changes the usage counters captures its change in the same write:
//
//	"qr"          {tok, h, t0, exp, hold}: the run; h is the first 16 hex
//	              digits of sha256(tok), the tag entries get
//	"qd#"         {tok, n, ne, pb, pm}: the number of "qd:" keys, the
//	              entries in them, and the net arrivals (bytes, messages)
//	"qd:<n>"      {h, m, e: [[uid, size, tagged], ...]}: the entries one
//	              release took out of mailbox m (at most ReleaseChunk
//	              per key; a larger release writes several keys)
//	"qm:<id>"     {h, t: [[uid, size], ...]}: the tagged entries a
//	              mailbox deletion released
//	"qi:<id>"     {t, k, m, n, b}: the intent of a COPY, a MOVE out of an
//	              ignored mailbox or a rename across the quota rules
//	              (what "bi:" is for a delivery)
//	"sr:<holder>" {holder, exp}: a scrub that repairs the account
//
// Arrivals count as a sum only: every entry that comes out of a captured
// reservation carries the run's hash in its index record (Meta.QuotaTag),
// and the run's listing skips such entries. Departures are captured with
// their identity (mailbox id, UID) and deduplicated by the run. A usage
// CAS that finds no live run writes exactly what it wrote before.
//
// Removal intents "r:<uid>" keep their place and their life (intents.go):
// three fields come to the value. d says the writer of the key removed
// the entry itself (the header CAS for shard 0, the commit of the pending
// change for higher shards), n is that writer's nonce, f that the usage
// of the entry was released (or that nothing was to release). The run's
// listing counts an entry it did not see on its shard when its r: has d
// and not f: removed, but still in the counters.
const (
	regQuotaRun           = "qr"
	regQuotaCaptureHead   = "qd#"
	regQuotaReleasePrefix = "qd:"
	regQuotaMailboxPrefix = "qm:"
	regQuotaIntentPrefix  = "qi:"
	regScrubMarkPrefix    = "sr:"
)

// Kinds of a "qi:" intent.
const (
	quotaIntentCopy   = "copy"
	quotaIntentMove   = "move"
	quotaIntentRename = "rename"
)

// quotaRun is the value of "qr".
type quotaRun struct {
	Tok  string `json:"tok"`
	H    string `json:"h"`
	T0   int64  `json:"t0"`
	Exp  int64  `json:"exp"`
	Hold int64  `json:"hold,omitempty"` // unix milliseconds
}

// alive reports whether the run has not expired at now.
func (r *quotaRun) alive(now time.Time) bool {
	return r != nil && r.Tok != "" && time.Unix(r.Exp, 0).After(now)
}

// quotaCaptureHead is the value of "qd#".
type quotaCaptureHead struct {
	Tok string `json:"tok"`
	N   int64  `json:"n"`
	NE  int64  `json:"ne"`
	PB  int64  `json:"pb"`
	PM  int64  `json:"pm"`
}

// quotaRelease is the value of "qd:<n>": the entries a release took out
// of mailbox M, each as [uid, size, tagged].
type quotaRelease struct {
	H string     `json:"h"`
	M string     `json:"m"`
	E [][3]int64 `json:"e"`
}

// quotaMailboxRelease is the value of "qm:<id>": the tagged entries a
// mailbox deletion released, each as [uid, size].
type quotaMailboxRelease struct {
	H string     `json:"h"`
	T [][2]int64 `json:"t"`
}

// quotaIntent is the value of "qi:<id>".
type quotaIntent struct {
	Time    int64  `json:"t"`
	Kind    string `json:"k"`
	Mailbox string `json:"m,omitempty"`
	Msgs    int64  `json:"n,omitempty"`
	Bytes   int64  `json:"b,omitempty"`
}

// scrubMark is the value of "sr:<holder>".
type scrubMark struct {
	Holder string `json:"holder"`
	Exp    int64  `json:"exp"`
}

func quotaReleaseKey(n int64) string    { return fmt.Sprintf("%s%012d", regQuotaReleasePrefix, n) }
func quotaMailboxKey(id string) string  { return regQuotaMailboxPrefix + id }
func quotaIntentKey(id string) string   { return regQuotaIntentPrefix + id }
func scrubMarkKey(holder string) string { return regScrubMarkPrefix + holder }
func quotaRunHash(tok string) string {
	s := sha256.Sum256([]byte(tok))
	return hex.EncodeToString(s[:8])
}
func newQuotaIntent(k, m string, q Quota) []byte {
	return encode(quotaIntent{Time: time.Now().Unix(), Kind: k, Mailbox: m, Msgs: q.Msgs, Bytes: q.Bytes})
}

// QuotaRecountConfig tunes the online recount (configuration quota.*).
type QuotaRecountConfig struct {
	// Barrier bounds the wait for operations of before the run.
	Barrier time.Duration
	// DeadIntent is how old an intent may be before it counts as
	// abandoned.
	DeadIntent time.Duration
	// SwitchAttempts are the switch calls (of casRetries rounds each)
	// before the run holds the usage writers.
	SwitchAttempts int
	// Hold is how long the usage writers wait at most while the run
	// switches (0: never).
	Hold time.Duration
	// MaxCaptureKeys and MaxCaptureEntries end a run whose captures
	// grow beyond them.
	MaxCaptureKeys, MaxCaptureEntries int64
	// ReleaseChunk is how many entries one "qd:" key holds.
	ReleaseChunk int
}

// Defaults of QuotaRecountConfig.
const (
	DefaultRecountBarrier        = 120 * time.Second
	DefaultRecountDeadIntent     = 600 * time.Second
	DefaultRecountSwitchAttempts = 10
	DefaultRecountHold           = 2 * time.Second
	DefaultRecountMaxKeys        = 1000
	DefaultRecountMaxEntries     = 1000000
	DefaultRecountReleaseChunk   = 10000
)

func (c QuotaRecountConfig) withDefaults() QuotaRecountConfig {
	c.Barrier = orDefault(c.Barrier, DefaultRecountBarrier)
	c.DeadIntent = orDefault(c.DeadIntent, DefaultRecountDeadIntent)

	if c.SwitchAttempts <= 0 {
		c.SwitchAttempts = DefaultRecountSwitchAttempts
	}

	if c.MaxCaptureKeys <= 0 {
		c.MaxCaptureKeys = DefaultRecountMaxKeys
	}

	if c.MaxCaptureEntries <= 0 {
		c.MaxCaptureEntries = DefaultRecountMaxEntries
	}

	if c.ReleaseChunk <= 0 {
		c.ReleaseChunk = DefaultRecountReleaseChunk
	}

	return c
}

// quotaKeys are the registry keys of the usage counters, the limit and
// the online recount: every usage CAS reads them together.
var quotaKeys = []string{regQuotaBytes, regQuotaMsgs, regQuotaLBytes, regQuotaLMsgs, regQuotaRun, regQuotaCaptureHead}

// captureState is what a usage CAS learned about the run from the keys
// it read: the run (nil: none) and its capture head.
type captureState struct {
	run  *quotaRun
	head quotaCaptureHead
	now  time.Time
}

// readCapture parses the run key and the capture head of cur.
func readCapture(cur map[string][]byte) captureState {
	st := captureState{now: time.Now()}

	var run quotaRun
	if v, ok := cur[regQuotaRun]; ok && json.Unmarshal(v, &run) == nil && run.alive(st.now) {
		st.run = &run

		if v, ok := cur[regQuotaCaptureHead]; ok {
			_ = json.Unmarshal(v, &st.head)
		}

		if st.head.Tok != run.Tok {
			st.head = quotaCaptureHead{Tok: run.Tok}
		}
	}

	return st
}

// live reports whether a run captures now.
func (st *captureState) live() bool { return st.run != nil }

// hash is the tag of the live run ("" without one).
func (st *captureState) hash() string {
	if st.run == nil {
		return ""
	}

	return st.run.H
}

// holdUntil is when the run's hold of the usage writers ends (zero: no
// hold).
func (st *captureState) holdUntil() time.Time {
	if st.run == nil || st.run.Hold == 0 {
		return time.Time{}
	}

	if t := time.UnixMilli(st.run.Hold); t.After(st.now) {
		return t
	}

	return time.Time{}
}

// arrive captures a reservation (or gives one back with negative q).
func (st *captureState) arrive(q Quota) {
	st.head.PB += q.Bytes
	st.head.PM += q.Msgs
}

// release captures the entries a release took out of mailbox id: one or
// more "qd:" keys of at most chunk entries, added to set. It returns
// how many keys it wrote.
func (st *captureState) release(set map[string][]byte, id string, metas []Meta, chunk int) int {
	keys := 0

	for start := 0; start < len(metas); start += chunk {
		end := min(start+chunk, len(metas))

		rel := quotaRelease{H: st.run.H, M: id, E: make([][3]int64, 0, end-start)}
		for _, m := range metas[start:end] {
			tagged := int64(0)
			if m.QuotaTag == st.run.H {
				tagged = 1
			}

			rel.E = append(rel.E, [3]int64{int64(m.UID), m.Size, tagged})
		}

		st.head.N++
		st.head.NE += int64(end - start)
		set[quotaReleaseKey(st.head.N)] = encode(rel)
		keys++
	}

	return keys
}

// mailbox captures the tagged entries a mailbox deletion released.
func (st *captureState) mailbox(set map[string][]byte, id string, metas []Meta) {
	rel := quotaMailboxRelease{H: st.run.H, T: [][2]int64{}}

	for _, m := range metas {
		if m.QuotaTag == st.run.H {
			rel.T = append(rel.T, [2]int64{int64(m.UID), m.Size})
		}
	}

	set[quotaMailboxKey(id)] = encode(rel)
}

// finish adds the capture head to set, or ends the run (removes "qr")
// when the captures grew beyond the limits: the run notices and ends
// lost. It reports whether the run was ended.
func (st *captureState) finish(set map[string][]byte, rm []string, cfg QuotaRecountConfig) (map[string][]byte, []string, bool) {
	if st.run == nil {
		return set, rm, false
	}

	set[regQuotaCaptureHead] = encode(st.head)

	if st.head.N > cfg.MaxCaptureKeys || st.head.NE > cfg.MaxCaptureEntries {
		return set, append(rm, regQuotaRun), true
	}

	return set, rm, false
}

// usageChange is one change of the usage counters (applyUsage).
type usageChange struct {
	d      Quota
	notify bool
	// tag is the run hash of a reservation that is given back: only a
	// reservation the live run captured is uncaptured.
	tag string
	// mailbox and released are the entries a release took out of a
	// mailbox (captured with their identity while a run is live).
	mailbox  string
	released []Meta
	// tombstone marks the deletion tombstone of mailbox as released
	// ("dm:".q = 1) and captures the tagged entries under "qm:".
	tombstone bool
	// rm are registry keys the CAS removes along ("qi:" of a rename).
	rm []string
	// abort ends a live run: whoever overwrites the counters or books
	// a sum the run cannot see makes it lost.
	abort bool
	// arrivalsOnly changes the captured arrivals of the run tag by d
	// and leaves the counters alone (a gated MOVE whose entries turned
	// out expunged: nothing was reserved for them).
	arrivalsOnly bool
}

// applyUsage changes the usage counters in one compare-and-swap on the
// registry and captures the change for a live online recount. A change
// of nothing writes nothing, unless a run is live (an empty release is
// then captured) or a tombstone is to be marked. The run's hold is
// honoured: a round that meets it writes nothing, waits outside the
// CAS and runs again.
func (u *User) applyUsage(ch usageChange) error {
	var (
		before, after, limit Quota
		holdUntil            time.Time
		wrote                bool
	)

	keys := quotaKeys
	if ch.tombstone {
		keys = append(append([]string(nil), quotaKeys...), deleteKey(ch.mailbox))
	}

	for round := 0; round < 3; round++ {
		holdUntil = time.Time{}

		err := casOmap(u, u.idx, oidRegistry, keys, func(cur map[string][]byte) (map[string][]byte, []string) {
			before, limit = u.quotaState(cur)
			after = Quota{Bytes: max(before.Bytes+ch.d.Bytes, 0), Msgs: max(before.Msgs+ch.d.Msgs, 0)}
			st := readCapture(cur)
			wrote = false

			if h := st.holdUntil(); !h.IsZero() && !ch.abort {
				holdUntil = h
				return nil, nil
			}

			set, rm := u.usageWrite(ch, cur, &st, after)
			if len(set) == 0 && len(rm) == 0 {
				return nil, nil
			}

			wrote = true

			return set, rm
		})
		if err != nil {
			return err
		}

		if holdUntil.IsZero() {
			break
		}

		u.recountHeld(holdUntil)
	}

	if ch.notify && wrote {
		u.quotaChanged(before, after, limit)
	}

	return nil
}

// usageWrite is what one round of applyUsage writes: the counters, the
// tombstone, and the captures of a live run (or its end).
func (u *User) usageWrite(ch usageChange, cur map[string][]byte, st *captureState, after Quota) (map[string][]byte, []string) {
	set := map[string][]byte{}

	rm := append([]string(nil), ch.rm...)

	if ch.d != (Quota{}) && !ch.arrivalsOnly {
		set = usageValues(after)
	}

	if ch.tombstone {
		if v, ok := cur[deleteKey(ch.mailbox)]; ok {
			set[deleteKey(ch.mailbox)] = tombstoneReleased(v)
		}
	}

	if ch.abort {
		return set, append(rm, regQuotaRun, regQuotaCaptureHead)
	}

	if !st.live() {
		return set, rm
	}

	u.captureChange(ch, st, set)

	set, rm, aborted := st.finish(set, rm, u.recount)
	if aborted {
		mh.recountCapture("abort")
	}

	return set, rm
}

// captureChange captures the change for the live run.
func (u *User) captureChange(ch usageChange, st *captureState, set map[string][]byte) {
	switch {
	case ch.tag != "" && ch.tag == st.hash():
		st.arrive(ch.d)
		mh.recountCapture("rollback")
	case ch.tag != "":
		// a reservation of before the run, or of another one
	case ch.tombstone:
		st.mailbox(set, ch.mailbox, ch.released)
		mh.recountCapture("mailbox")
	case ch.mailbox != "":
		st.release(set, ch.mailbox, ch.released, u.recount.ReleaseChunk)
		mh.recountCapture("release")
	}
}

// recountHeld waits for the hold of an online recount's switch.
func (u *User) recountHeld(until time.Time) {
	d := time.Until(until)
	if d <= 0 {
		return
	}

	mh.recountHold(d)
	time.Sleep(d)
}

// tombstoneReleased returns the tombstone value v with q = 1.
func tombstoneReleased(v []byte) []byte {
	var t mailboxIntent

	_ = json.Unmarshal(v, &t)

	one := 1
	t.Released = &one

	return encode(t)
}

// tombstoneUnreleased reports whether a tombstone value says its usage
// is still to be released (q = 0; a value without q, written before
// 0.52.0, is not listed).
func tombstoneUnreleased(v []byte) bool {
	var t mailboxIntent

	return json.Unmarshal(v, &t) == nil && t.Released != nil && *t.Released == 0
}

// --- the removal intent fields -----------------------------------------------

// removalValue is the "r:" value of an entry this removal removed
// (removed) or announced, with its nonce and whether the usage is
// (or needs not be) released.
func removalValue(now int64, m *Meta, removed bool, nonce string, released bool) []byte {
	return encode(removeIntent{Time: now, blobRef: blobRef{GUID: m.GUID, Size: m.Size, Chunks: m.Chunks},
		Removed: removed, Nonce: nonce, Released: released})
}

// refreshRemoval returns an existing "r:" value with a fresh time and
// everything else as it is: a renewed announcement must not take d, n
// or f away from the session that removed the entry.
func refreshRemoval(v []byte, now int64) ([]byte, bool) {
	var ri removeIntent
	if json.Unmarshal(v, &ri) != nil || ri.GUID == "" {
		return nil, false
	}

	ri.Time = now

	return encode(ri), true
}

// flagReleased writes f = true (with d = true and a fresh time) on the
// "r:" keys of the entries this removal (nonce) removed: the one extra
// write a release costs, directly after the usage CAS and before the
// blob references go (+1 W per release).
func (u *User) flagReleased(mb *Mailbox, removed []Meta, nonce string) {
	if len(removed) == 0 {
		return
	}

	now := time.Now().Unix()
	set := make(map[string][]byte, len(removed))

	for i := range removed {
		set[removeKey(removed[i].UID)] = removalValue(now, &removed[i], true, nonce, true)
	}

	if err := setKeysExisting(u.idx, mboxOID(mb.ID), set); err != nil {
		u.log.Warn("release flag not written", "mailbox", mb.Name, "err", err)
	}
}

// --- qi: intents ------------------------------------------------------------

// parseQuotaRun parses a "qr" value.
func parseQuotaRun(v []byte) (*quotaRun, bool) {
	var run quotaRun
	if json.Unmarshal(v, &run) != nil || run.Tok == "" {
		return nil, false
	}

	return &run, true
}

// quotaReleaseIndex is the n of a "qd:<n>" key.
func quotaReleaseIndex(key string) int64 {
	n, _ := strconv.ParseInt(strings.TrimPrefix(key, regQuotaReleasePrefix), 10, 64)
	return n
}
