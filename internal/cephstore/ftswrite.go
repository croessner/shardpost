package cephstore

import (
	"encoding/hex"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/ceph/go-ceph/rados"

	"shardpost/internal/fts"
	"shardpost/internal/metrics"
	"shardpost/internal/model"
	"shardpost/internal/postings"
)

// The write path of the full-text index in both layouts (stage B14b).
//
// An indexing (a delivery, an APPEND, an import batch, a rebuild batch)
// allocates the document ids in the id map, whose compare-and-swap also
// reads the layout record, and then writes in this order: the header
// texts, one compare-and-swap per posting object, the dictionary keys of
// the terms that were new. The header text goes first so that a
// conversion that freezes the posting objects after the header was
// written finds it in the source it copies; an indexing that meets the
// freeze is repeated as a whole in the generation the conversion
// switches to.
//
// Anomalies (ftsAnomaly) end the attempt; resolveAnomaly reads the
// layout record and decides. One indexing follows at most
// maxGenSwitches generation changes and waits at most once for a freeze
// (fts.frozen_wait_seconds, centrally: the posting goroutines only
// report it); beyond that it fails, which a delivery answers with a
// temporary failure.

// Bounds of one indexing or removal.
const (
	maxGenSwitches = 2
	maxAnomalies   = 8
	frozenPoll     = 250 * time.Millisecond
)

// ftsWrite is the state of one indexing or removal.
type ftsWrite struct {
	lay    *ftsLayout
	loc    ftsLoc
	wait   time.Duration
	gens   int
	waited bool
	// stale are the freezes no conversion names any more, by object:
	// the next compare-and-swap of the object lifts exactly that one.
	stale map[string]string
	// noRecord: a read of the layout record after an anomaly found none,
	// so missing layout 2 posting objects are created as before.
	noRecord bool
}

// follow makes the write go to the current generation of l.
func (w *ftsWrite) follow(l *ftsLayout, n int) {
	w.lay, w.loc, w.stale, w.noRecord = l, curLoc(l, n), nil, false
}

// ftsIndex adds the message's GUID to the posting list of every term and
// registers terms that are new for this user in the dictionary.
func (u *User) ftsIndex(guid string, raw []byte) error {
	return u.ftsIndexDocs([]ftsDoc{{guid: guid, raw: raw}})
}

// ftsIndexDocs indexes several messages at once. Indexing a message
// costs round trips — the id map, a header write, a compare-and-swap
// per posting object, a write per dictionary object its new terms touch
// — so a batch of an import that pays them once instead of once per
// message is much cheaper.
func (u *User) ftsIndexDocs(docs []ftsDoc) (err error) {
	if len(docs) == 0 {
		return nil
	}

	guids, size, err := docGUIDs(docs)
	if err != nil {
		return err
	}

	end := u.trace(spanFTSIndex, attrGUID(docs[0].guid), attrCount(len(docs)), attrBytes(size))
	defer func() { end(err) }()

	ids, lay, err := u.ensureDocIDs(guids)
	if err != nil {
		return err
	}

	docsOf, terms, headers := termDocs(docs, ids)

	w := &ftsWrite{wait: u.ftsCfg.frozenWait}
	w.follow(lay, u.ftsShards)

	return u.indexWith(w, docsOf, terms, headers)
}

// indexWith runs the attempts of one indexing within its budget.
func (u *User) indexWith(w *ftsWrite, docsOf map[string][]uint64, terms []string, headers map[string]string) error {
	for attempt := 0; ; attempt++ {
		err := u.indexInto(w, docsOf, terms, headers)

		an := asAnomaly(err)
		if an == nil {
			return err
		}

		if attempt >= maxAnomalies {
			return errFTSRetries
		}

		if err := u.resolveAnomaly(w, an); err != nil {
			return err
		}
	}
}

// indexInto writes the documents into the generation of w: header texts,
// postings, dictionary.
func (u *User) indexInto(w *ftsWrite, docsOf map[string][]uint64, terms []string, headers map[string]string) error {
	if err := u.putHeaderTextsAt(w.loc, headers); err != nil {
		return err
	}

	var (
		mu       sync.Mutex
		newTerms []string
	)

	add := func(oid string, terms []string) error {
		fresh, err := u.addPostings(w, oid, terms, docsOf)
		if err != nil {
			if asAnomaly(err) != nil {
				return err
			}

			return fmt.Errorf("fts index shard %s: %w", oid, err)
		}

		mu.Lock()

		newTerms = append(newTerms, fresh...)
		mu.Unlock()

		return nil
	}

	if err := parallel(groupBy(terms, w.loc.postingOID), add); err != nil {
		return err
	}

	_, err := u.dictApplyAt(w.loc, newTerms, true)

	return err
}

// resolveAnomaly reads the layout record after an anomaly and prepares
// the next attempt of w, or returns why there is none (§4.3):
//
//   - the record names another generation (or layout): the operation is
//     repeated there, as a whole, and nothing more is written into the
//     old one;
//   - a freeze no conversion names (no "next") is stale: the next
//     compare-and-swap of the object lifts it;
//   - a freeze of a running conversion is waited for, once per
//     operation, until the record moves on or the conversion goes away;
//   - a missing object of the named generation is an error (scrub
//     finding, repaired by fts_rebuild); in layout 2 without a record it
//     is created as before.
func (u *User) resolveAnomaly(w *ftsWrite, an *ftsAnomaly) error {
	l, err := u.readLayout()
	if err != nil {
		return err
	}

	if !curLoc(l, u.ftsShards).same(w.loc) {
		return u.followGen(w, l, "generation")
	}

	switch an.kind {
	case anomFrozen:
		if l == nil || l.Next == nil {
			if w.stale == nil {
				w.stale = map[string]string{}
			}

			w.lay, w.stale[an.oid] = l, an.state

			return nil
		}

		return u.waitFrozen(w)
	case anomMissing:
		if l == nil && !w.loc.v3() && !w.noRecord {
			w.lay, w.noRecord = nil, true // layout 2 without a record: created as before
			return nil
		}
	}

	metrics.FTSShardMissing.Inc()

	return fmt.Errorf("%w: %s", errFTSShardMissing, an.oid)
}

// followGen moves w to the current generation of l, within the budget.
func (u *User) followGen(w *ftsWrite, l *ftsLayout, reason string) error {
	w.gens++
	if w.gens > maxGenSwitches {
		return errFTSRetries
	}

	metrics.FTSLayoutRetries.WithLabelValues(reason).Inc()
	w.follow(l, u.ftsShards)

	return nil
}

// waitFrozen waits for the conversion that froze the generation of w:
// until the record names another generation (the operation goes on
// there), the conversion goes away (it goes on where it was) or
// w.wait has passed (temporary failure). It waits once per operation.
func (u *User) waitFrozen(w *ftsWrite) error {
	if w.waited || w.wait <= 0 {
		metrics.FTSFrozenWaits.WithLabelValues("timeout").Inc()
		return errFTSFrozen
	}

	w.waited = true
	deadline := time.Now().Add(w.wait)

	for {
		left := time.Until(deadline)
		if left <= 0 {
			metrics.FTSFrozenWaits.WithLabelValues("timeout").Inc()
			return errFTSFrozen
		}

		time.Sleep(min(frozenPoll, left))

		l, err := u.readLayout()
		if err != nil {
			return err
		}

		if !curLoc(l, u.ftsShards).same(w.loc) {
			metrics.FTSFrozenWaits.WithLabelValues("moved").Inc()
			return u.followGen(w, l, "frozen")
		}

		if l == nil || l.Next == nil {
			metrics.FTSFrozenWaits.WithLabelValues("thawed").Inc()

			w.lay = l

			return nil
		}
	}
}

// addPostings adds the ids of docsOf to the posting lists of the terms
// of one posting object and returns the terms that had no list yet. A
// term the read does not return counts as new and its list is written
// afresh, so the read must be complete: the terms go through in pieces
// of postingsPerCAS, as every other read by keys here does, rather than
// relying on the OSD to answer any number of keys at once.
func (u *User) addPostings(w *ftsWrite, oid string, terms []string, docsOf map[string][]uint64) ([]string, error) {
	var fresh []string

	for len(terms) > 0 {
		n := min(len(terms), postingsPerCAS)

		part, err := u.addPostingsPage(w, oid, terms[:n], docsOf)
		if err != nil {
			return nil, err
		}

		fresh = append(fresh, part...)
		terms = terms[n:]
	}

	return fresh, nil
}

// addPostingsPage is addPostings for at most postingsPerCAS terms.
func (u *User) addPostingsPage(w *ftsWrite, oid string, terms []string, docsOf map[string][]uint64) ([]string, error) {
	var fresh []string

	err := u.casPostings(w, oid, terms, func(cur map[string][]byte, conv func([]byte) []byte) shardChange {
		fresh = fresh[:0]
		ch := shardChange{set: map[string][]byte{}}

		for _, t := range terms {
			p, ok := cur[t]
			if !ok {
				fresh = append(fresh, t)
				ch.terms++
				ch.dict += int64(dictKeysOf(t))
			}

			ch.set[t] = postings.AddAll(conv(p), docsOf[t])
		}

		return ch
	})

	return fresh, err
}

// shardChange is what one compare-and-swap of a posting object writes:
// the keys, and how the term and dictionary key counters change.
type shardChange struct {
	set         map[string][]byte
	rm          []string
	terms, dict int64
}

func (c shardChange) empty() bool { return len(c.set) == 0 && len(c.rm) == 0 }

// casPostings is casShard over posting lists with legacy conversion: fn
// receives the current lists and a converter that turns a legacy GUID
// list into the id encoding. The legacy check runs on the lists the
// compare-and-swap reads anyway: when they hold GUIDs without a known id,
// the round ends without a write, the ids are allocated and the
// compare-and-swap starts again with them (a list that gained further
// GUIDs in between repeats this). fn is called only when every GUID of
// cur has an id, so conv always converts completely. Only layout 2 has
// such lists; a conversion writes layout 3 in the id encoding.
func (u *User) casPostings(w *ftsWrite, oid string, terms []string, fn func(cur map[string][]byte, conv func([]byte) []byte) shardChange) error {
	known := map[string]uint64{}
	conv := legacyConverter(known)

	for attempt := 0; attempt < casRetries; attempt++ {
		var need []string

		err := u.casShard(w, oid, terms, func(cur map[string][]byte) (shardChange, bool) {
			if need = legacyGUIDsNeeded(cur, known); len(need) > 0 {
				return shardChange{}, false
			}

			return fn(cur, conv), true
		})
		if err != nil {
			return err
		}

		if len(need) == 0 {
			return nil
		}

		ids, _, err := u.ensureDocIDs(need)
		if err != nil {
			return err
		}

		for g, id := range ids {
			known[g] = id
		}
	}

	return fmt.Errorf("fts %s: too many concurrent writers", oid)
}

// postingMarkers are the markers a posting compare-and-swap reads.
func postingMarkers(loc ftsLoc) []string {
	if loc.v3() {
		return []string{markGen, markState, markTerms, markDict}
	}

	return []string{markState}
}

// casShard is the compare-and-swap of one posting object of the
// generation of w: it reads the terms and the object's markers, ends
// with an anomaly where §4.3 says so, and writes what fn computes —
// in layout 3 with the counters "!n" and "!dn", whose size may queue
// the account for a reshard. fn returning false ends the round without
// a write. A layout 3 object is never created here; a layout 2 object
// only when a read of the layout record found none.
func (u *User) casShard(w *ftsWrite, oid string, terms []string, fn func(cur map[string][]byte) (shardChange, bool)) (err error) {
	marks := postingMarkers(w.loc)
	keys := append(slices.Clone(terms), marks...)

	end := u.trace(spanOmapCAS, attrOID(oid), attrCount(len(keys)))
	defer func() { end(err) }()

	for attempt := 0; attempt < casRetries; attempt++ {
		done, err := u.casShardRound(w, oid, keys, marks, fn)
		if err != nil || done {
			return err
		}

		mh.casRetry("omap", casClass(oid))

		if attempt+1 < casRetries {
			time.Sleep(casBackoff(attempt))
		}
	}

	return fmt.Errorf("%s: too many concurrent writers", oid)
}

// casShardRound is one round of casShard; done is false after a round
// lost to another writer.
func (u *User) casShardRound(w *ftsWrite, oid string, keys, marks []string, fn func(cur map[string][]byte) (shardChange, bool)) (bool, error) {
	cur, ver, err := u.getKeysVer(u.idx, oid, keys)
	if err != nil {
		return true, err
	}

	lift, err := w.check(oid, ver, cur)
	if err != nil {
		return true, err
	}

	m := splitMarkers(cur, marks)

	ch, ok := fn(cur)
	if !ok || (ch.empty() && !lift) {
		return true, nil
	}

	set, rm, due := u.shardWrite(w, ch, m, lift)

	op := casWriteOp(ver, set, rm, nil)
	err = op.Operate(u.idx, oid, rados.OperationNoFlag)
	op.Release()

	switch errCode(err) {
	case 0:
		if lift {
			metrics.FTSStaleFreezeLifted.Inc()
		}

		if due {
			u.queueReshard(w.loc.g)
		}

		return true, nil
	case syscall.ERANGE, syscall.EOVERFLOW, syscall.EEXIST, syscall.ENOENT:
		return false, nil
	}

	return true, err
}

// splitMarkers moves the markers out of cur.
func splitMarkers(cur map[string][]byte, marks []string) map[string][]byte {
	m := map[string][]byte{}

	for _, k := range marks {
		if v, ok := cur[k]; ok {
			m[k] = v
			delete(cur, k)
		}
	}

	return m
}

// shardWrite is what a posting compare-and-swap writes: the change, the
// lifted freeze and the counters of a layout 3 object; due reports
// counters that ask for a reshard.
func (u *User) shardWrite(w *ftsWrite, ch shardChange, m map[string][]byte, lift bool) (map[string][]byte, []string, bool) {
	set, rm := ch.set, ch.rm
	if set == nil {
		set = map[string][]byte{}
	}

	if lift && w.loc.v3() {
		set[markState] = []byte(stateLive)
	} else if lift {
		rm = append(rm, markState)
	}

	if !w.loc.v3() {
		return set, rm, false
	}

	n, dn := atoiMarker(m[markTerms])+ch.terms, atoiMarker(m[markDict])+ch.dict
	set[markTerms], set[markDict] = itoaMarker(n), itoaMarker(dn)

	return set, rm, ch.terms > 0 && u.reshardDue(w.loc, n, dn)
}

// check applies the anomaly rule to a posting object read at version
// ver; lift reports a stale freeze this write lifts.
func (w *ftsWrite) check(oid string, ver uint64, cur map[string][]byte) (lift bool, err error) {
	if ver == 0 && !w.loc.v3() {
		if w.noRecord {
			return false, nil // created as before
		}

		return false, &ftsAnomaly{kind: anomMissing, oid: oid}
	}

	an := checkMarkers(w.loc, oid, ver != 0, cur)
	if an == nil {
		return false, nil
	}

	if an.kind == anomFrozen && w.stale[oid] == an.state {
		return true, nil
	}

	return false, an
}

// --- header texts ---------------------------------------------------------------

// putHeaderTextsAt stores the header texts of the messages, by GUID. In
// layout 3 the write asserts that the object exists: a missing one is an
// anomaly.
func (u *User) putHeaderTextsAt(loc ftsLoc, texts map[string]string) error {
	byShard := map[string]map[string][]byte{}

	for guid, text := range texts {
		v, ok := encodeHeaderText(text)
		if !ok {
			continue
		}

		oid := loc.headerOID(guid)
		if byShard[oid] == nil {
			byShard[oid] = map[string][]byte{}
		}

		byShard[oid][guid] = v
	}

	groups := map[string][]string{}
	for oid := range byShard {
		groups[oid] = nil
	}

	return parallel(groups, func(oid string, _ []string) error {
		op := rados.CreateWriteOp()
		defer op.Release()

		if loc.v3() {
			op.AssertExists()
		}

		op.SetOmap(byShard[oid])

		err := op.Operate(u.idx, oid, rados.OperationNoFlag)
		if loc.v3() && errCode(err) == syscall.ENOENT {
			return &ftsAnomaly{kind: anomMissing, oid: oid}
		}

		if err != nil {
			return fmt.Errorf("fts header shard %s: %w", oid, err)
		}

		return nil
	})
}

// dropHeaderTextAt removes the header text of a message. A missing
// layout 2 object holds nothing; a missing layout 3 object is an anomaly.
func (u *User) dropHeaderTextAt(loc ftsLoc, guid string) error {
	op := rados.CreateWriteOp()
	defer op.Release()

	if loc.v3() {
		op.AssertExists()
	}

	op.RmOmapKeys([]string{guid})

	oid := loc.headerOID(guid)

	err := op.Operate(u.idx, oid, rados.OperationNoFlag)
	if errCode(err) == syscall.ENOENT {
		if loc.v3() {
			return &ftsAnomaly{kind: anomMissing, oid: oid}
		}

		return nil
	}

	return err
}

// --- dictionary -----------------------------------------------------------------

// dictApplyAt adds the terms to the trigram dictionary of loc or removes
// them and returns the number of entries.
//
// The posting lists say which terms are new or gone, and the dictionary
// follows in a second step. A term that one writer removes while another
// adds it again can therefore end up with postings and without its
// dictionary keys (it is then found by the exact word and its stems
// only, until a conversion, ConvertFTSDict or a rebuild lists it again);
// a key without postings costs a lookup that finds nothing.
//
// In layout 3 the writes assert that the objects exist. A missing one of
// another generation than the record names is ignored: the terms are in
// the source a conversion derives the new dictionary from (§4.3 step 4).
func (u *User) dictApplyAt(loc ftsLoc, terms []string, add bool) (n int, err error) {
	byShard, n := dictEntriesAt(loc, terms)
	if n == 0 {
		return 0, nil
	}

	end := u.trace(spanFTSDict, attrCount(n))
	defer func() { end(err) }()

	var (
		mu      sync.Mutex
		missing string
	)

	err = parallelN(byShard, dictWriters, func(oid string, keys []string) error {
		for len(keys) > 0 {
			n := min(len(keys), dictWriteChunk)

			err := dictWrite(u.idx, oid, keys[:n], add, loc.v3())
			if loc.v3() && errCode(err) == syscall.ENOENT {
				mu.Lock()
				missing = oid
				mu.Unlock()

				return nil
			}

			if err != nil {
				return fmt.Errorf("fts dictionary shard %s: %w", oid, err)
			}

			keys = keys[n:]
		}

		return nil
	})
	if err != nil || missing == "" {
		return n, err
	}

	l, err := u.readLayout()
	if err != nil {
		return n, err
	}

	if !curLoc(l, u.ftsShards).same(loc) {
		return n, nil
	}

	metrics.FTSShardMissing.Inc()

	return n, fmt.Errorf("%w: %s", errFTSShardMissing, missing)
}

// dictEntriesAt returns the dictionary keys of the terms by object of
// loc, and their number. Terms of fields without substring matching
// have none.
func dictEntriesAt(loc ftsLoc, terms []string) (map[string][]string, int) {
	byShard := map[string][]string{}
	n := 0

	for _, t := range terms {
		field, tok, _ := strings.Cut(t, ":")
		if !fts.Substring(field) {
			continue
		}

		for _, tri := range fts.Trigrams(tok) {
			oid := loc.dictOID(tri)
			byShard[oid] = append(byShard[oid], tri+dictSep+t)
			n++
		}
	}

	return byShard, n
}

// dictEntries is dictEntriesAt for layout 2.
func dictEntries(terms []string) (map[string][]string, int) {
	return dictEntriesAt(layout2Loc(DefaultFTSShards), terms)
}

// dictWrite sets or removes dictionary keys of one object. Neither
// depends on what the object holds, so there is nothing to read first
// and nothing to retry. With exists the object must exist (layout 3).
func dictWrite(ioctx *rados.IOContext, oid string, keys []string, add, exists bool) error {
	op := rados.CreateWriteOp()
	defer op.Release()

	if exists {
		op.AssertExists()
	}

	if add {
		set := make(map[string][]byte, len(keys))
		for _, k := range keys {
			set[k] = nil
		}

		op.SetOmap(set)
	} else {
		op.RmOmapKeys(keys)
	}

	err := op.Operate(ioctx, oid, rados.OperationNoFlag)
	if !add && !exists && errCode(err) == syscall.ENOENT {
		return nil
	}

	return err
}

// --- document ids -----------------------------------------------------------

func docKey(guid string) string { return "g:" + guid }
func idKey(id uint64) string    { return "i:" + strconv.FormatUint(id, 10) }

// ftsMapKeys are the keys of the id map an allocation reads besides the
// documents: the next id, the version markers, the rebuild key, the
// layout record and the document count.
var ftsMapKeys = []string{"n", ftsVersionKey, ftsRebuildKey, ftsLayoutKey, ftsVersionKey3, ftsCountKey}

// ensureDocIDs returns the ids of the GUIDs, allocating missing ones in
// one compare-and-swap on the map object, and the layout record that
// compare-and-swap read.
//
// The first ids of an index this version writes from the start get the
// version marker (markFreshIndex). Whether the index is fresh is decided
// inside the compare-and-swap: an id map with "n" or a layout record is
// not, and only one without them (and without ids seen by the view,
// ftsHasIDs) needs the posting shards looked up. The compare-and-swap
// function does no I/O, so it ends that round without a write, the
// shards are looked up and the round is repeated with the answer.
//
// A fresh index gets layout 3 when fts.layout is 3 and the cluster mark
// is set (read now, §4.2): the account is entered in the presence index,
// the objects of a new generation are created with their markers, and
// the compare-and-swap that allocates the first ids writes the layout
// record. A writer that loses that race to another creator deletes its
// objects and follows the record that won; the presence entry belongs to
// the account and stays.
func (u *User) ensureDocIDs(guids []string) (map[string]uint64, *ftsLayout, error) {
	// Deduplicate: a GUID named twice must not get two ids.
	guids = slices.Compact(slices.Sorted(slices.Values(guids)))

	keys := make([]string, 0, len(guids)+len(ftsMapKeys))
	for _, g := range guids {
		keys = append(keys, docKey(g))
	}

	keys = append(keys, ftsMapKeys...)

	a := &docAlloc{u: u, guids: guids, out: map[string]uint64{}, fresh: freshUnknown, want3: u.ftsCfg.layout == model.FTSLayout3}
	if u.ftsHasIDs.Load() {
		a.fresh = freshNo
	}

	defer a.dropGen()

	for attempt := 0; attempt < casRetries; attempt++ {
		if err := casOmap(u, u.idx, oidFTSMap, keys, a.round); err != nil {
			return nil, nil, fmt.Errorf("fts docids: %w", err)
		}

		if a.perr != nil {
			return nil, nil, a.perr
		}

		done, err := a.next()
		if err != nil {
			return nil, nil, err
		}

		if done {
			return a.out, a.lay, nil
		}
	}

	return nil, nil, fmt.Errorf("fts docids: %s: too many concurrent writers", oidFTSMap)
}

// docAlloc is one allocation of document ids (ensureDocIDs).
type docAlloc struct {
	u     *User
	guids []string
	out   map[string]uint64
	fresh int
	want3 bool
	// gen is a prepared layout 3 generation that no record names yet.
	gen *ftsLoc

	// what the last round found
	lay                            *ftsLayout
	perr                           error
	lookup, create, installed, due bool
}

// round is the compare-and-swap function of the id map.
func (a *docAlloc) round(cur map[string][]byte) (map[string][]byte, []string) {
	a.lookup, a.create, a.installed, a.due = false, false, false, false

	if a.lay, a.perr = parseLayout(cur[ftsLayoutKey]); a.perr != nil {
		return nil, nil
	}

	set, next := assignDocIDs(cur, a.guids, a.out)
	if len(set) == 0 {
		return nil, nil
	}

	added := int64(len(set) / 2)

	if _, hasN := cur["n"]; a.lay == nil && !hasN && !a.first(cur, set, added) {
		return nil, nil
	}

	set["n"] = []byte(strconv.FormatUint(next, 10))

	switch {
	case a.lay == nil:
		markFreshIndex(cur, set, a.fresh == freshYes)
	case a.lay.F == model.FTSLayout3 && !a.installed:
		c := atoiMarker(cur[ftsCountKey]) + added
		set[ftsCountKey] = itoaMarker(c)
		a.due = float64(c)/float64(max(a.lay.H, 1)) > reshardFactor*float64(a.u.ftsCfg.sizing.target)
	}

	return set, nil
}

// first handles the first ids of an index; false ends the round without
// a write (the shards are looked up, or a generation is prepared).
func (a *docAlloc) first(cur, set map[string][]byte, added int64) bool {
	switch {
	case a.fresh == freshUnknown:
		a.lookup = true
		return false
	case a.fresh == freshYes && a.want3 && a.gen == nil:
		a.create = true
		return false
	case a.fresh == freshYes && a.want3:
		a.lay, a.installed = installGen(cur, set, *a.gen, added), true
	}

	return true
}

// next does the I/O a round asked for; true when the allocation is done.
func (a *docAlloc) next() (bool, error) {
	var err error

	switch {
	case a.lookup:
		if a.fresh, err = a.u.freshByShards(); err != nil {
			return false, fmt.Errorf("fts docids: %w", err)
		}

		return false, nil
	case a.create:
		a.gen, a.want3, err = a.u.prepareGen()
		return false, err
	}

	if a.installed {
		a.gen = nil // the record names it now
	}

	a.u.ftsHasIDs.Store(true)

	if a.due {
		a.u.queueReshard(a.lay.G)
	}

	return true, nil
}

// dropGen deletes a prepared generation that lost to another creator.
func (a *docAlloc) dropGen() {
	if a.gen == nil {
		return
	}

	if err := a.u.deleteObjects(a.gen.objects()); err != nil {
		a.u.log.Warn("fts generation of a lost creation not removed", "gen", a.gen.g, "err", err)
	}
}

// installGen writes the layout record of a prepared generation into the
// first allocation of a fresh index (with the layout 3 version marker
// unless a rebuild is running) and returns it.
func installGen(cur, set map[string][]byte, g ftsLoc, added int64) *ftsLayout {
	l := &ftsLayout{F: model.FTSLayout3, G: g.g, S: g.s, D: g.d, H: g.h, Dict: true, LG: true}
	set[ftsLayoutKey] = l.encode()
	set[ftsCountKey] = itoaMarker(added)

	if _, rebuilding := cur[ftsRebuildKey]; !rebuilding {
		set[ftsVersionKey3] = []byte(ftsVersion)
	}

	return l
}

// prepareGen creates a generation for a fresh index if layout 3 is
// allowed now; nil and false when the index stays in layout 2.
func (u *User) prepareGen() (*ftsLoc, bool, error) {
	ok, err := u.layout3Wanted()
	if err != nil || !ok {
		return nil, false, err
	}

	s, d, h := u.ftsCfg.sizing.shapeFor(u.ftsHint)
	loc := ftsLoc{f: model.FTSLayout3, g: newFTSGen(), s: s, d: d, h: h}

	// Presence entry first (a completeness index): an account with
	// layout 3 objects is always in it.
	if err := u.markLayout3(); err != nil {
		return nil, false, err
	}

	if err := u.createGen(loc); err != nil {
		_ = u.deleteObjects(loc.objects())
		return nil, false, err
	}

	return &loc, true, nil
}

// assignDocIDs fills out with the ids of the GUIDs in the id map cur
// and gives the others the next free ones; it returns the keys to set
// for them (nil: all had ids) and the next free id after them.
func assignDocIDs(cur map[string][]byte, guids []string, out map[string]uint64) (map[string][]byte, uint64) {
	clear(out)

	next, _ := strconv.ParseUint(string(cur["n"]), 10, 64)
	if next == 0 {
		next = 1
	}

	var set map[string][]byte

	for _, g := range guids {
		if v, ok := cur[docKey(g)]; ok {
			out[g], _ = strconv.ParseUint(string(v), 10, 64)
			continue
		}

		if set == nil {
			set = map[string][]byte{}
		}

		out[g] = next
		set[docKey(g)] = []byte(strconv.FormatUint(next, 10))
		set[idKey(next)] = []byte(g)
		next++
	}

	return set, next
}

// docIDOf returns the id of a GUID (ok=false: never indexed with ids)
// and the layout record, in one read.
func (u *User) docIDOf(guid string) (uint64, bool, *ftsLayout, error) {
	vals, err := getOmapKeys(u.idx, oidFTSMap, []string{docKey(guid), ftsLayoutKey})
	if err != nil {
		if errCode(err) == syscall.ENOENT {
			return 0, false, nil, nil
		}

		return 0, false, nil, err
	}

	lay, err := parseLayout(vals[ftsLayoutKey])
	if err != nil {
		return 0, false, nil, err
	}

	v, ok := vals[docKey(guid)]
	if !ok {
		return 0, false, lay, nil
	}

	id, _ := strconv.ParseUint(string(v), 10, 64)

	return id, true, lay, nil
}

// dropDocID frees the id of a GUID; in layout 3 the document count goes
// down with it.
func (u *User) dropDocID(guid string, id uint64) error {
	return casOmap(u, u.idx, oidFTSMap, []string{docKey(guid), ftsLayoutKey, ftsCountKey}, func(cur map[string][]byte) (map[string][]byte, []string) {
		rm := []string{docKey(guid), idKey(id)}

		lay, _ := parseLayout(cur[ftsLayoutKey])
		if _, ok := cur[docKey(guid)]; !ok || lay == nil || lay.F != model.FTSLayout3 {
			return nil, rm
		}

		return map[string][]byte{ftsCountKey: itoaMarker(atoiMarker(cur[ftsCountKey]) - 1)}, rm
	})
}

// --- removal --------------------------------------------------------------------

// ftsRemove deletes the message from every posting list of the message,
// drops terms that became empty from the dictionary and frees the
// document id, waiting for a conversion as an indexing does.
func (u *User) ftsRemove(m *Meta) error { return u.ftsRemoveWait(m, u.ftsCfg.frozenWait) }

// ftsRemoveWait is ftsRemove with the wait for a frozen index: 0 for the
// callers without the account lock that must not wait (the undo of a
// delivery, the upload expiry, the scrubber's orphans).
//
// It re-tokenises the blob, which is cheaper than storing a per-message
// term list. The order (§4.4): the id and the layout record in one read,
// the header text (it needs no blob), the posting lists, the dictionary
// keys of terms that became empty; the id is freed in any case, also
// when the blob cannot be read or a posting object is frozen or missing:
// the lists that keep the id then hold a dead one, which resolveDocIDs
// filters out and a conversion or rebuild leaves out.
func (u *User) ftsRemoveWait(m *Meta, wait time.Duration) (err error) {
	id, hasID, lay, err := u.docIDOf(m.GUID)
	if err != nil {
		return err
	}

	if hasID {
		defer func() {
			if derr := u.dropDocID(m.GUID, id); derr != nil {
				u.log.Warn("fts docid release failed", "guid", m.GUID, "err", derr)
			}
		}()
	}

	raw, rerr := u.readBlob(m)

	var terms []string
	if rerr == nil {
		terms = fts.Terms(raw)
	}

	w := &ftsWrite{wait: wait}
	w.follow(lay, u.ftsShards)

	if err := u.removeWith(w, m.GUID, id, hasID, terms); err != nil {
		return err
	}

	return rerr
}

// removeWith runs the attempts of one removal within its budget.
func (u *User) removeWith(w *ftsWrite, guid string, id uint64, hasID bool, terms []string) error {
	g, _ := hex.DecodeString(guid)

	for attempt := 0; ; attempt++ {
		err := u.removeFrom(w, guid, g, id, hasID, terms)

		an := asAnomaly(err)
		if an == nil {
			return err
		}

		if attempt >= maxAnomalies {
			return errFTSRetries
		}

		if err := u.resolveAnomaly(w, an); err != nil {
			return err
		}
	}
}

// removeFrom removes a message from the generation of w: header text,
// postings, dictionary keys of the terms that became empty.
func (u *User) removeFrom(w *ftsWrite, guid string, g []byte, id uint64, hasID bool, terms []string) error {
	// first: it needs no blob, and a stale header text would be checked
	// for a GUID that a later message cannot have
	if err := u.dropHeaderTextAt(w.loc, guid); err != nil {
		if asAnomaly(err) != nil {
			return err
		}

		u.log.Warn("fts header text removal failed", "guid", guid, "err", err)
	}

	if len(terms) == 0 {
		return nil
	}

	var (
		mu   sync.Mutex
		gone []string
	)

	err := parallel(groupBy(terms, w.loc.postingOID), func(oid string, terms []string) error {
		empty, err := u.ftsRemoveShard(w, oid, terms, g, id, hasID)
		if err != nil {
			return err
		}

		mu.Lock()

		gone = append(gone, empty...)
		mu.Unlock()

		return nil
	})
	if err != nil {
		return err
	}

	_, err = u.dictApplyAt(w.loc, gone, false)

	return err
}

// ftsRemoveShard removes the message from the posting lists of terms on
// one posting object and returns the terms whose lists became empty.
func (u *User) ftsRemoveShard(w *ftsWrite, oid string, terms []string, g []byte, id uint64, hasID bool) ([]string, error) {
	var empty []string

	for len(terms) > 0 {
		page := terms[:min(len(terms), postingsPerCAS)]
		terms = terms[len(page):]

		var part []string

		err := u.casShard(w, oid, page, func(cur map[string][]byte) (shardChange, bool) {
			part = part[:0]
			ch := shardChange{set: map[string][]byte{}}

			for _, t := range page {
				p := removeFromPosting(cur[t], g, id, hasID)

				if _, had := cur[t]; had && len(p) == 0 {
					ch.rm = append(ch.rm, t)
					ch.terms--
					ch.dict -= int64(dictKeysOf(t))
					part = append(part, t)
				} else if len(p) > 0 && string(p) != string(cur[t]) {
					ch.set[t] = p
				}
			}

			return ch, true
		})
		if err != nil {
			if asAnomaly(err) != nil {
				return nil, err
			}

			return nil, fmt.Errorf("fts remove shard %s: %w", oid, err)
		}

		empty = append(empty, part...)
	}

	return empty, nil
}
