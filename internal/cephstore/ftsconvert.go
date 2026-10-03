package cephstore

import (
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/ceph/go-ceph/rados"

	"shardpost/internal/metrics"
	"shardpost/internal/model"
	"shardpost/internal/postings"
)

// Conversions and reshards of the full-text index (stage B14b, job
// fts_layout and the import reshard endpoint).
//
// A run holds the account's maintenance lock and identifies itself by a
// token (model.FTSLayoutRequest.Tok). Its phases:
//
//	P2  fence; the layout record gets "next" = {target generation, tok}.
//	    A "next" of exactly this token is continued; any other is
//	    foreign: its targets are deleted and it is replaced. Sources a
//	    crashed run left in "old" are removed. 2→3 enters the account in
//	    the presence index first.
//	P3  every source posting object gets "!s" = frozen:<tok>; writers
//	    that meet it wait (§4.3).
//	P5  the targets are created with their markers (deleted first), the
//	    sources read page by page — markers and dead ids left out,
//	    legacy GUID lists converted — and written blindly in pieces of
//	    at most dictWriteChunk keys; the dictionary is derived from the
//	    terms, the header texts copied; then the counters. Fenced before
//	    every piece.
//	P6  fence; the record names the target as current (only while "next"
//	    still carries tok exactly) and the source in "old".
//	P7  fence; the sources are deleted (only while "old" carries tok),
//	    then the "old" entry is removed.
//
// A run that fails leaves "next" behind; the caller removes it with
// ClearFTSLayoutNext (the job's defer, the runner's fallback, the jobs
// scrub) or a lock taker does with ClearForeignFTSLayoutNext.

// ftsLayoutHook, when set (tests), is called after each phase; an error
// ends the run there as a crash would, without any clean-up.
var ftsLayoutHook func(account, phase string) error

// Phases of a conversion (hook names, progress).
const (
	phaseP2 = "P2"
	phaseP3 = "P3"
	phaseP5 = "P5"
	phaseP6 = "P6"
	phaseP7 = "P7"
)

// errConversionLost: the record no longer carries the run's token.
var errConversionLost = errors.New("the conversion was taken over or removed meanwhile")

func (u *User) hook(phase string) error {
	if ftsLayoutHook == nil {
		return nil
	}

	return ftsLayoutHook(u.name, phase)
}

// FTSLayoutDue implements model.FTSLayouter: the counters of the posting
// objects, the existence of the dictionary and header objects and the
// document count (S + D + H + 1 reads), and the shape they ask for.
func (u *User) FTSLayoutDue() (model.FTSLayoutShape, bool, error) {
	l, err := u.readLayout()
	if err != nil {
		return model.FTSLayoutShape{}, false, err
	}

	cur := curLoc(l, u.ftsShards)
	if !cur.v3() {
		return cur.shape(), false, nil
	}

	target, capped, err := u.reshardTarget(cur)
	if err != nil {
		return model.FTSLayoutShape{}, false, err
	}

	shape := target.shape()
	shape.Capped = capped

	return shape, target.s != cur.s || target.d != cur.d || target.h != cur.h, nil
}

// reshardTarget computes the shape the counters of a layout 3
// generation ask for (temp/13 §4.6): S′ and H′ within fts.shards_max
// from fts.shard_target_keys, D′ within fts.dict_shards_max from
// fts.dict_target_keys. capped names the kinds a cap cut; it is only
// reported, never counted (the writers count a generation at its cap).
func (u *User) reshardTarget(cur ftsLoc) (ftsLoc, []string, error) {
	terms, dict, docs, err := u.layoutCounts(cur)
	if err != nil {
		return ftsLoc{}, nil, err
	}

	z := u.ftsCfg.sizing
	s, c1 := z.count(terms)
	d, c2 := z.dictCount(dict)
	h, c3 := z.count(docs)

	var capped []string

	for _, c := range []struct {
		cut  bool
		kind string
	}{{c1, kindPostings}, {c2, kindDictionary}, {c3, kindHeaders}} {
		if c.cut {
			capped = append(capped, c.kind)
		}
	}

	return ftsLoc{f: model.FTSLayout3, s: s, d: d, h: h}, capped, nil
}

// layoutCounts reads the term and dictionary key counters of the posting
// objects, checks the generation marker of every other object, and
// reads the document count (S + D + H + 1 reads).
func (u *User) layoutCounts(cur ftsLoc) (terms, dict, docs int64, err error) {
	for _, oid := range cur.objects() {
		vals, err := u.genMarkers(cur, oid)
		if err != nil {
			return 0, 0, 0, err
		}

		terms += atoiMarker(vals[markTerms])
		dict += atoiMarker(vals[markDict])
	}

	vals, err := getOmapKeys(u.idx, oidFTSMap, []string{ftsCountKey})
	if err != nil {
		return 0, 0, 0, err
	}

	return terms, dict, atoiMarker(vals[ftsCountKey]), nil
}

// genMarkers reads the markers of an object of generation cur; a
// missing object or one of another generation is an error.
func (u *User) genMarkers(cur ftsLoc, oid string) (map[string][]byte, error) {
	vals, err := getOmapKeys(u.idx, oid, []string{markGen, markTerms, markDict})
	if err != nil && errCode(err) != syscall.ENOENT {
		return nil, err
	}

	if string(vals[markGen]) != cur.g {
		metrics.FTSShardMissing.Inc()
		return nil, fmt.Errorf("%w: %s", errFTSShardMissing, oid)
	}

	return vals, nil
}

// ConvertFTSLayout implements model.FTSLayouter (see the phases above).
func (u *User) ConvertFTSLayout(req model.FTSLayoutRequest) (rep model.FTSLayoutReport, err error) {
	if req.Tok == "" {
		return rep, errors.New("fts layout: a run token is required")
	}

	req = withRequestDefaults(req)
	mode := "convert"

	defer func() {
		countRun(mode, rep, err)
		u.ftsForget()
	}()

	l, err := u.readLayout()
	if err != nil {
		return rep, err
	}

	src := curLoc(l, u.ftsShards)
	rep.From = src.shape()

	// A run of this token that got past P6 only has P7 left.
	if old := pastP6(l, req.Tok); old != nil {
		rep.Resumed, rep.To = true, src.shape()
		return rep, u.finishOld(req, genLoc(*old, u.ftsShards))
	}

	// Sources a crashed run of another token left after its P6 go first:
	// the run holds the account lock, and a run that finds nothing to do
	// (due_only) must not leave them for good.
	if l, err = u.dropForeignOld(req, l); err != nil {
		return rep, err
	}

	target, resume, err := u.conversionTarget(req, l, src)
	if err != nil || target == nil {
		rep.Noop, rep.Reason, rep.To = err == nil, "nothing to do", src.shape()
		return rep, err
	}

	if target.v3() && src.v3() {
		mode = "reshard"
	}

	rep.Resumed, rep.To = resume, target.shape()

	return rep, u.runPhases(req, &rep, l, src, *target, resume)
}

// runPhases runs P2 to P7.
func (u *User) runPhases(req model.FTSLayoutRequest, rep *model.FTSLayoutReport, l *ftsLayout, src, target ftsLoc, resume bool) error {
	if err := u.phase2(req, rep, l, src, target, resume); err != nil {
		return err
	}

	if err := u.phase3(req, src); err != nil {
		return err
	}

	stats, err := u.phase5(req, src, target)
	if err != nil {
		return err
	}

	rep.Terms, rep.Dict, rep.Headers, rep.Docs = stats.terms, stats.dict, stats.headers, stats.docs
	rep.Writes, rep.BufferKeys = stats.writes, stats.buffer

	if err := u.phase6(req, src, target, stats); err != nil {
		return err
	}

	return u.finishOld(req, src)
}

// withRequestDefaults fills in the optional functions of a request.
func withRequestDefaults(req model.FTSLayoutRequest) model.FTSLayoutRequest {
	if req.Fence == nil {
		req.Fence = func() error { return nil }
	}

	if req.Progress == nil {
		req.Progress = func(string, int, int) {}
	}

	return req
}

// countRun counts a run in imapceph_fts_layout_runs_total.
func countRun(mode string, rep model.FTSLayoutReport, err error) {
	switch {
	case err != nil:
		metrics.FTSLayoutRuns.WithLabelValues(mode, "error").Inc()
	case rep.Noop:
		metrics.FTSLayoutRuns.WithLabelValues("noop", "ok").Inc()
	default:
		metrics.FTSLayoutRuns.WithLabelValues(mode, "ok").Inc()
	}
}

// pastP6 returns the "old" entry of tok when the run of tok got past P6
// (no "next" any more), else nil.
func pastP6(l *ftsLayout, tok string) *ftsGen {
	if l == nil || l.Next != nil {
		return nil
	}

	if i := oldOf(l, tok); i >= 0 {
		return &l.Old[i]
	}

	return nil
}

// dropForeignOld removes the "old" entries of other runs (their sources
// and the entries) and returns the record after it.
func (u *User) dropForeignOld(req model.FTSLayoutRequest, l *ftsLayout) (*ftsLayout, error) {
	if l == nil || len(l.Old) == 0 {
		return l, nil
	}

	if err := req.Fence(); err != nil {
		return l, err
	}

	for _, o := range l.Old {
		if o.Tok == req.Tok {
			continue
		}

		if err := u.removeOld(o); err != nil {
			return l, err
		}
	}

	return u.readLayout()
}

// ClearFTSLayoutOld implements model.FTSLayouter.
func (u *User) ClearFTSLayoutOld(tok string) (bool, error) {
	l, err := u.readLayout()
	if err != nil || l == nil {
		return false, err
	}

	i := oldOf(l, tok)
	if i < 0 {
		return false, nil
	}

	defer u.ftsForget()

	return true, u.removeOld(l.Old[i])
}

// oldOf returns the index of the "old" entry of tok, or -1.
func oldOf(l *ftsLayout, tok string) int {
	for i, o := range l.Old {
		if o.Tok == tok {
			return i
		}
	}

	return -1
}

// conversionTarget decides the target of a run (nil: nothing to do) and
// whether the run continues an unfinished one of its token.
func (u *User) conversionTarget(req model.FTSLayoutRequest, l *ftsLayout, src ftsLoc) (*ftsLoc, bool, error) {
	if l != nil && l.Next != nil && l.Next.Tok == req.Tok {
		t := genLoc(*l.Next, u.ftsShards)
		return &t, true, nil
	}

	if done, t, err := u.layout2Target(req, src); done || err != nil {
		return t, false, err
	}

	if !src.v3() {
		if err := u.requireMark(); err != nil {
			return nil, false, err
		}
	}

	t, err := u.layout3Target(req, src)
	if err != nil || (req.DueOnly && sameShape(t, src)) {
		return nil, false, err
	}

	return &t, false, nil
}

// sameShape reports whether a layout 3 target has the object counts of
// a layout 3 source.
func sameShape(t, src ftsLoc) bool {
	return src.v3() && t.s == src.s && t.d == src.d && t.h == src.h
}

// layout2Target decides a request for layout 2 (done): nothing to do for
// a layout 2 index, the layout 2 objects for a layout 3 one; an unknown
// layout is an error. A request for layout 3 is not done.
func (u *User) layout2Target(req model.FTSLayoutRequest, src ftsLoc) (bool, *ftsLoc, error) {
	layout := req.Layout
	if layout == 0 {
		layout = src.f
	}

	switch {
	case layout == model.FTSLayout2 && !src.v3():
		return true, nil, nil
	case layout == model.FTSLayout2:
		t := layout2Loc(u.ftsShards)
		return true, &t, nil
	case layout != model.FTSLayout3:
		return true, nil, fmt.Errorf("fts layout: unknown layout %d", layout)
	}

	return false, nil, nil
}

// requireMark refuses a conversion to layout 3 without the cluster mark.
func (u *User) requireMark() error {
	if u.store == nil {
		return model.ErrFTSLayoutMark
	}

	ok, err := u.store.FTSLayout3Enabled()
	if err == nil && !ok {
		err = model.ErrFTSLayoutMark
	}

	return err
}

// layout3Target is a new layout 3 generation of the shape the request,
// the counters (a reshard) or the number of documents (a conversion from
// layout 2) ask for.
func (u *User) layout3Target(req model.FTSLayoutRequest, src ftsLoc) (ftsLoc, error) {
	t := ftsLoc{f: model.FTSLayout3, g: newFTSGen()}

	switch {
	case req.Shape != nil:
		t.s, t.d, t.h = forcedCount(req.Shape.S), forcedCount(req.Shape.D), forcedCount(req.Shape.H)
	case req.Shards > 0:
		n := forcedCount(req.Shards)
		t.s, t.d, t.h = n, n, n
	case src.v3():
		shape, _, err := u.reshardTarget(src)
		if err != nil {
			return t, err
		}

		t.s, t.d, t.h = shape.s, shape.d, shape.h
	default:
		m, err := u.liveDocCount()
		if err != nil {
			return t, err
		}

		t.s, t.d, t.h = u.ftsCfg.sizing.shapeFor(m)
	}

	return t, nil
}

// forcedCount is a forced object count within 1 to 4096; the caps do
// not apply.
func forcedCount(n int) int { return min(max(n, 1), 4096) }

// liveDocCount counts the documents of the id map ("i:" keys).
func (u *User) liveDocCount() (int, error) {
	n := 0
	err := u.idMap(func(uint64, string) { n++ })

	return n, err
}

// idMap calls fn for every document of the id map.
func (u *User) idMap(fn func(id uint64, guid string)) error {
	after := ""

	for {
		vals, err := u.idx.GetOmapValues(oidFTSMap, after, "i:", omapPageSize)
		if err != nil {
			if errCode(err) == syscall.ENOENT {
				return nil
			}

			return fmt.Errorf("fts docids: %w", err)
		}

		if len(vals) == 0 {
			return nil
		}

		keys := make([]string, 0, len(vals))
		for k := range vals {
			keys = append(keys, k)
		}

		slices.Sort(keys)

		for _, k := range keys {
			id, err := strconv.ParseUint(strings.TrimPrefix(k, "i:"), 10, 64)
			if err == nil {
				fn(id, string(vals[k]))
			}
		}

		after = keys[len(keys)-1]

		if len(vals) < omapPageSize {
			return nil
		}
	}
}

// phase2 records the target in the layout record (§4.6 P2).
func (u *User) phase2(req model.FTSLayoutRequest, rep *model.FTSLayoutReport, l *ftsLayout, src, target ftsLoc, resume bool) error {
	req.Progress(phaseP2, 0, 1)

	if err := req.Fence(); err != nil {
		return err
	}

	if resume {
		return u.hook(phaseP2)
	}

	foreign, err := u.clearForRun(l)
	if err != nil {
		return err
	}

	if foreign != nil {
		rep.Cleared = foreign.Tok
	}

	if target.v3() && !src.v3() {
		if err := u.markLayout3(); err != nil {
			return err
		}
	}

	next := &ftsGen{F: target.f, G: target.g, S: target.s, D: target.d, H: target.h, Tok: req.Tok}
	if !strings.HasPrefix(req.Tok, "job-") {
		next.At = time.Now().Unix() // the import reshard endpoint's heartbeat
	}

	if err := u.setNext(foreign, next); err != nil {
		return err
	}

	return u.hook(phaseP2)
}

// clearForRun prepares the record for a new run: the targets of a
// foreign unfinished conversion go (they are named; it is replaced in
// the same write as the new one is recorded), and so do sources a
// crashed run left in "old". A run of the same job with another lease
// generation is foreign too (R4.1): it may still be writing.
func (u *User) clearForRun(l *ftsLayout) (*ftsGen, error) {
	if l == nil {
		return nil, nil
	}

	var foreign *ftsGen

	if l.Next != nil {
		n := *l.Next
		foreign = &n

		if err := u.deleteObjects(targetObjects(n, u.ftsShards)); err != nil {
			return nil, err
		}
	}

	for _, o := range l.Old {
		if err := u.removeOld(o); err != nil {
			return nil, err
		}
	}

	return foreign, nil
}

// setNext records next in the record while its "next" is still foreign.
func (u *User) setNext(foreign, next *ftsGen) error {
	var lost bool

	err := casOmap(u, u.idx, oidFTSMap, []string{ftsLayoutKey}, func(cur map[string][]byte) (map[string][]byte, []string) {
		c, perr := parseLayout(cur[ftsLayoutKey])
		if lost = perr != nil || !sameNext(c, foreign); lost {
			return nil, nil
		}

		if c == nil {
			c = &ftsLayout{F: model.FTSLayout2}
		}

		c.Next = next

		return map[string][]byte{ftsLayoutKey: c.encode()}, nil
	})
	if err == nil && lost {
		err = errConversionLost
	}

	return err
}

// sameNext reports whether the record's "next" is still want (nil: none).
func sameNext(l *ftsLayout, want *ftsGen) bool {
	switch {
	case l == nil || l.Next == nil:
		return want == nil
	case want == nil:
		return false
	default:
		return l.Next.Tok == want.Tok && l.Next.G == want.G && l.Next.F == want.F
	}
}

// targetObjects are the objects of a conversion target.
func targetObjects(g ftsGen, n int) []string {
	return genLoc(g, n).objects()
}

// removeOld deletes the sources a run left in "old" and removes the
// entry (P7 of a crashed run).
func (u *User) removeOld(o ftsGen) error {
	if err := u.deleteObjects(genLoc(o, u.ftsShards).objects()); err != nil {
		return err
	}

	return u.dropOld(o.Tok, o.F == model.FTSLayout2)
}

// dropOld removes the "old" entry of tok; lg marks the layout 2 objects
// gone.
func (u *User) dropOld(tok string, lg bool) error {
	return casOmap(u, u.idx, oidFTSMap, []string{ftsLayoutKey}, func(cur map[string][]byte) (map[string][]byte, []string) {
		c, err := parseLayout(cur[ftsLayoutKey])
		if err != nil || c == nil {
			return nil, nil
		}

		i := oldOf(c, tok)
		if i < 0 {
			return nil, nil
		}

		c.Old = slices.Delete(c.Old, i, i+1)
		c.LG = c.LG || lg

		return map[string][]byte{ftsLayoutKey: c.encode()}, nil
	})
}

// phase3 freezes every source posting object with the run's token; a
// freeze of another run is overwritten. A missing layout 2 object is
// created frozen (writers would otherwise create it behind the freeze);
// a missing layout 3 object is an error.
func (u *User) phase3(req model.FTSLayoutRequest, src ftsLoc) error {
	state := []byte(frozenPrefix + req.Tok)

	for i, oid := range src.postingOIDs() {
		req.Progress(phaseP3, i, src.s)

		if !src.v3() {
			err := casOmap(u, u.idx, oid, []string{markState}, func(map[string][]byte) (map[string][]byte, []string) {
				return map[string][]byte{markState: state}, nil
			})
			if err != nil {
				return fmt.Errorf("freeze %s: %w", oid, err)
			}

			continue
		}

		var gone, foreign bool

		err := u.casExisting(oid, []string{markGen}, func(cur map[string][]byte) (map[string][]byte, []string) {
			if foreign = string(cur[markGen]) != src.g; foreign {
				return nil, nil
			}

			return map[string][]byte{markState: state}, nil
		}, &gone)
		if err != nil {
			return fmt.Errorf("freeze %s: %w", oid, err)
		}

		if gone || foreign {
			metrics.FTSShardMissing.Inc()
			return fmt.Errorf("%w: %s", errFTSShardMissing, oid)
		}
	}

	return u.hook(phaseP3)
}

// convertStats count what P5 wrote.
type convertStats struct {
	terms, dict, headers, docs int64
	// writes are the writes of the copy, buffer the most keys it held.
	writes int64
	buffer int
	// nextID is the id map's next id when P5 read the documents (P6
	// counts the documents indexed after it).
	nextID uint64
}

// phase5 builds the target from the frozen sources.
func (u *User) phase5(req model.FTSLayoutRequest, src, target ftsLoc) (convertStats, error) {
	var st convertStats

	if err := req.Fence(); err != nil {
		return st, err
	}

	// Targets afresh: what a run before left there goes.
	if err := u.deleteObjects(target.objects()); err != nil {
		return st, err
	}

	if target.v3() {
		if err := u.createGen(target); err != nil {
			return st, err
		}
	}

	live, byGUID, next, err := u.liveDocs()
	if err != nil {
		return st, err
	}

	st.docs, st.nextID = int64(len(live)), next

	w := u.newPieceWriter(target, req.Fence)
	defer w.stop()

	c := &copier{u: u, target: target, live: live, byGUID: byGUID, st: &st, terms: map[string]int64{}, dicts: map[string]int64{},
		posts: newChunkWriter(w), dict: newChunkWriter(w), heads: newChunkWriter(w)}

	if err := c.postings(req, src); err != nil {
		return st, err
	}

	if err := c.headers(src); err != nil {
		return st, err
	}

	err = w.finish()
	st.writes, st.buffer = w.writes.Load(), w.peak

	if err != nil {
		return st, err
	}

	if target.v3() {
		if err := u.writeCounters(req, target, c.terms, c.dicts); err != nil {
			return st, err
		}
	}

	shardGauge("postings", c.posts.sizes)
	shardGauge("dictionary", c.dict.sizes)
	shardGauge("headers", c.heads.sizes)

	return st, u.hook(phaseP5)
}

// copier is the copy of P5.
type copier struct {
	u      *User
	target ftsLoc
	live   map[uint64]bool
	byGUID map[string]uint64
	st     *convertStats
	// terms and dicts count the terms and dictionary keys of each target
	// posting object ("!n", "!dn").
	terms, dicts       map[string]int64
	posts, dict, heads *chunkWriter
}

// postings copies the posting lists of every source object and derives
// the dictionary from their terms.
func (c *copier) postings(req model.FTSLayoutRequest, src ftsLoc) error {
	for i, oid := range src.postingOIDs() {
		req.Progress(phaseP5, i, src.s)

		if err := c.u.copyPostings(oid, c.live, c.byGUID, c.term); err != nil {
			return err
		}
	}

	return nil
}

// term writes one term's list and dictionary keys.
func (c *copier) term(term string, list []byte) error {
	toid := c.target.postingOID(term)
	c.terms[toid]++
	c.st.terms++

	if err := c.posts.add(toid, term, list); err != nil {
		return err
	}

	byShard, n := dictEntriesAt(c.target, []string{term})
	c.dicts[toid] += int64(n)
	c.st.dict += int64(n)

	for doid, keys := range byShard {
		for _, k := range keys {
			if err := c.dict.add(doid, k, nil); err != nil {
				return err
			}
		}
	}

	return nil
}

// headers copies the header texts of the live documents.
func (c *copier) headers(src ftsLoc) error {
	for _, oid := range src.headerOIDs() {
		err := c.u.scanObject(oid, func(guid string, v []byte) error {
			if _, ok := c.byGUID[guid]; !ok {
				return nil
			}

			c.st.headers++

			return c.heads.add(c.target.headerOID(guid), guid, v)
		})
		if err != nil {
			return err
		}
	}

	return nil
}

// shardGauge records the largest object of a kind.
func shardGauge(kind string, sizes map[string]int64) {
	var top int64
	for _, n := range sizes {
		top = max(top, n)
	}

	metrics.FTSShardKeys.WithLabelValues(kind).Set(float64(top))
}

// writeCounters sets "!n" and "!dn" of the target posting objects.
func (u *User) writeCounters(req model.FTSLayoutRequest, target ftsLoc, terms, dicts map[string]int64) error {
	if err := req.Fence(); err != nil {
		return err
	}

	for _, oid := range target.postingOIDs() {
		op := rados.CreateWriteOp()
		op.AssertExists()
		op.SetOmap(map[string][]byte{markTerms: itoaMarker(terms[oid]), markDict: itoaMarker(dicts[oid])})
		err := op.Operate(u.idx, oid, rados.OperationNoFlag)
		op.Release()

		if err != nil {
			return fmt.Errorf("counters %s: %w", oid, err)
		}
	}

	return nil
}

// liveDocs reads the id map: the live ids, the id of every live GUID and
// the next id.
func (u *User) liveDocs() (map[uint64]bool, map[string]uint64, uint64, error) {
	live, byGUID := map[uint64]bool{}, map[string]uint64{}

	err := u.idMap(func(id uint64, guid string) {
		live[id] = true
		byGUID[guid] = id
	})
	if err != nil {
		return nil, nil, 0, err
	}

	vals, err := getOmapKeys(u.idx, oidFTSMap, []string{"n"})
	if err != nil && errCode(err) != syscall.ENOENT {
		return nil, nil, 0, err
	}

	next, _ := strconv.ParseUint(string(vals["n"]), 10, 64)

	return live, byGUID, next, nil
}

// scanObject calls fn for every key of an object but the markers (an
// absent object has none).
func (u *User) scanObject(oid string, fn func(key string, v []byte) error) error {
	after := ""

	for {
		op := rados.CreateReadOp()
		step := op.GetOmapValues(after, "", omapPageSize)

		err := op.Operate(u.idx, oid, rados.OperationNoFlag)
		if err != nil {
			op.Release()

			if errCode(err) == syscall.ENOENT {
				return nil
			}

			return fmt.Errorf("read %s: %w", oid, err)
		}

		var kvs []*rados.OmapKeyValue

		for {
			kv, err := step.Next()
			if err != nil {
				op.Release()
				return err
			}

			if kv == nil {
				break
			}

			kvs = append(kvs, &rados.OmapKeyValue{Key: kv.Key, Value: append([]byte(nil), kv.Value...)})
		}

		more := step.More()

		op.Release()

		for _, kv := range kvs {
			after = kv.Key

			if isMarker(kv.Key) {
				continue
			}

			if err := fn(kv.Key, kv.Value); err != nil {
				return err
			}
		}

		if !more || len(kvs) == 0 {
			return nil
		}
	}
}

// copyPostings hands every posting list of a source object to fn, with
// the dead ids left out and legacy GUID lists converted; lists that end
// up empty are dropped. A legacy GUID without an id gets one (as a
// delivery that met the list would give it).
func (u *User) copyPostings(oid string, live map[uint64]bool, byGUID map[string]uint64, fn func(term string, list []byte) error) error {
	return u.scanObject(oid, func(term string, v []byte) error {
		var ids []uint64

		if len(v) > 0 && postings.IsLegacy(v) {
			var err error
			if ids, err = u.legacyIDs(v, live, byGUID); err != nil {
				return err
			}
		} else {
			list, _ := postings.Decode(v)
			for _, id := range list {
				if live[id] {
					ids = append(ids, id)
				}
			}
		}

		if len(ids) == 0 {
			return nil
		}

		slices.Sort(ids)

		return fn(term, postings.Encode(slices.Compact(ids)))
	})
}

// legacyIDs returns the ids of the GUIDs of a legacy list, allocating
// the missing ones.
func (u *User) legacyIDs(v []byte, live map[uint64]bool, byGUID map[string]uint64) ([]uint64, error) {
	var (
		ids  []uint64
		need []string
	)

	for _, g := range postings.LegacyGUIDs(v) {
		gs := hex.EncodeToString(g)
		if id, ok := byGUID[gs]; ok {
			ids = append(ids, id)
		} else {
			need = append(need, gs)
		}
	}

	if len(need) == 0 {
		return ids, nil
	}

	out := map[string]uint64{}

	keys := make([]string, 0, len(need)+1)
	for _, g := range need {
		keys = append(keys, docKey(g))
	}

	err := casOmap(u, u.idx, oidFTSMap, append(keys, "n"), func(cur map[string][]byte) (map[string][]byte, []string) {
		set, next := assignDocIDs(cur, need, out)
		if len(set) == 0 {
			return nil, nil
		}

		set["n"] = []byte(strconv.FormatUint(next, 10))

		return set, nil
	})
	if err != nil {
		return nil, fmt.Errorf("fts docids: %w", err)
	}

	for g, id := range out {
		byGUID[g], live[id] = id, true
		ids = append(ids, id)
	}

	return ids, nil
}

// phase6 switches the record to the target (only while "next" carries
// the run's token exactly).
func (u *User) phase6(req model.FTSLayoutRequest, src, target ftsLoc, st convertStats) error {
	req.Progress(phaseP6, 0, 1)

	if err := req.Fence(); err != nil {
		return err
	}

	var lost bool

	err := casOmap(u, u.idx, oidFTSMap, []string{ftsLayoutKey, ftsVersionKey, ftsVersionKey3, ftsCountKey, "n"},
		func(cur map[string][]byte) (map[string][]byte, []string) {
			c, perr := parseLayout(cur[ftsLayoutKey])
			if lost = perr != nil || c == nil || c.Next == nil || c.Next.Tok != req.Tok; lost {
				return nil, nil
			}

			if !target.v3() {
				return switchTo2(cur)
			}

			return switchTo3(cur, c, src, target, req.Tok, st)
		})
	if err != nil {
		return err
	}

	if lost {
		return errConversionLost
	}

	if !target.v3() {
		if err := u.unmarkLayout3(); err != nil {
			return err
		}
	}

	return u.hook(phaseP6)
}

// switchTo2 is the switch of 3→2: the record goes, the version marker
// moves back.
func switchTo2(cur map[string][]byte) (map[string][]byte, []string) {
	set, rm := moveKey(cur, ftsVersionKey3, ftsVersionKey)

	return set, append(rm, ftsLayoutKey, ftsCountKey)
}

// switchTo3 is the switch to a layout 3 target: it becomes current, the
// source is kept in "old" with the run's token (and the heartbeat of an
// import reshard, which goes on renewing it there) until P7, the version
// marker moves to ftsVersionKey3 (2→3) and the document count is set:
// what P5 copied and the ids allocated since.
func switchTo3(cur map[string][]byte, c *ftsLayout, src, target ftsLoc, tok string, st convertStats) (map[string][]byte, []string) {
	set, rm := map[string][]byte{}, []string(nil)
	if !src.v3() {
		set, rm = moveKey(cur, ftsVersionKey, ftsVersionKey3)
	}

	old := ftsGen{F: src.f, G: src.g, S: src.s, D: src.d, H: src.h, Tok: tok}
	if c.Next != nil && c.Next.Tok == tok {
		old.At = c.Next.At
	}

	nl := &ftsLayout{F: model.FTSLayout3, G: target.g, S: target.s, D: target.d, H: target.h, Dict: true,
		LG: c.LG && src.v3(), Old: append(c.Old, old)}

	docs := st.docs
	if next, _ := strconv.ParseUint(string(cur["n"]), 10, 64); next > st.nextID {
		docs += int64(next - st.nextID)
	}

	set[ftsLayoutKey], set[ftsCountKey] = nl.encode(), itoaMarker(docs)

	return set, rm
}

// moveKey moves the value of key from to key to (removing to when from
// is absent).
func moveKey(cur map[string][]byte, from, to string) (map[string][]byte, []string) {
	if v, ok := cur[from]; ok {
		return map[string][]byte{to: v}, []string{from}
	}

	return map[string][]byte{}, []string{from, to}
}

// finishOld is P7: the sources go (only while the record still lists
// them for this run; after a conversion back to layout 2 nothing names
// them any more), then the "old" entry.
func (u *User) finishOld(req model.FTSLayoutRequest, src ftsLoc) error {
	req.Progress(phaseP7, 0, 1)

	if err := req.Fence(); err != nil {
		return err
	}

	l, err := u.readLayout()
	if err != nil {
		return err
	}

	switch {
	case l == nil:
		// 3→2 switched: the record is gone with the generation.
	case oldOf(l, req.Tok) < 0:
		return errConversionLost
	}

	if err := u.deleteObjects(src.objects()); err != nil {
		return err
	}

	if l != nil {
		if err := u.dropOld(req.Tok, !src.v3()); err != nil {
			return err
		}
	}

	return u.hook(phaseP7)
}

// --- clearing an unfinished conversion ---------------------------------------------

// ClearFTSLayoutNext implements model.FTSLayouter.
func (u *User) ClearFTSLayoutNext(tok string) (bool, error) {
	cleared, err := u.clearNext(func(t string) bool { return t == tok })

	return cleared != "", err
}

// ClearForeignFTSLayoutNext implements model.FTSLayouter.
func (u *User) ClearForeignFTSLayoutNext(own string) (string, error) {
	return u.clearNext(func(t string) bool { return t != own })
}

// clearNext removes the record's "next" when match accepts its token:
// the record first (a freeze without "next" is stale for every writer),
// then the freezes of exactly that token are lifted, then the targets
// deleted. A run that still lives fails at its next fence, at the
// deleted targets (AssertExists) or at the token check of P6.
func (u *User) clearNext(match func(tok string) bool) (string, error) {
	l, err := u.readLayout()
	if err != nil || l == nil || l.Next == nil || !match(l.Next.Tok) {
		return "", err
	}

	next := *l.Next
	src := curLoc(l, u.ftsShards)

	rest, removed, err := u.dropNext(next)
	if err != nil || !removed {
		return "", err
	}

	u.ftsForget()

	if err := u.thaw(src, next.Tok); err != nil {
		return next.Tok, err
	}

	if err := u.deleteObjects(targetObjects(next, u.ftsShards)); err != nil {
		return next.Tok, err
	}

	// An account that got its presence entry for a conversion that did
	// not happen leaves the presence index again.
	if next.F == model.FTSLayout3 && !ftsLayout3(rest) {
		if err := u.unmarkLayout3(); err != nil {
			return next.Tok, err
		}
	}

	return next.Tok, nil
}

// ftsLayout3 reports whether a record names a layout 3 generation as
// current.
func ftsLayout3(l *ftsLayout) bool { return l != nil && l.F == model.FTSLayout3 }

// dropNext removes "next" from the record while it is still next;
// removed is false when it changed meanwhile. A layout 2 record without
// anything else goes as a whole.
func (u *User) dropNext(next ftsGen) (rest *ftsLayout, removed bool, err error) {
	err = casOmap(u, u.idx, oidFTSMap, []string{ftsLayoutKey}, func(cur map[string][]byte) (map[string][]byte, []string) {
		c, perr := parseLayout(cur[ftsLayoutKey])
		if removed = perr == nil && c != nil && c.Next != nil && c.Next.Tok == next.Tok && c.Next.G == next.G; !removed {
			return nil, nil
		}

		c.Next, rest = nil, c

		if c.F == model.FTSLayout2 && len(c.Old) == 0 {
			return nil, []string{ftsLayoutKey}
		}

		return map[string][]byte{ftsLayoutKey: c.encode()}, nil
	})

	return rest, removed, err
}

// thaw lifts the freezes of exactly tok on the posting objects of src: a
// compare-and-swap on the value read, so the freeze of another run is
// never lifted (R5.1).
func (u *User) thaw(src ftsLoc, tok string) error {
	want := frozenPrefix + tok

	for _, oid := range src.postingOIDs() {
		var gone bool

		err := u.casExisting(oid, []string{markState}, func(cur map[string][]byte) (map[string][]byte, []string) {
			if string(cur[markState]) != want {
				return nil, nil
			}

			if src.v3() {
				return map[string][]byte{markState: []byte(stateLive)}, nil
			}

			return nil, []string{markState}
		}, &gone)
		if err != nil {
			return fmt.Errorf("thaw %s: %w", oid, err)
		}
	}

	return nil
}

// casExisting is casOmap that never creates the object: an absent one
// is left alone (gone is set).
func (u *User) casExisting(oid string, keys []string, fn func(cur map[string][]byte) (map[string][]byte, []string), gone *bool) error {
	for attempt := 0; attempt < casRetries; attempt++ {
		cur, ver, err := u.getKeysVer(u.idx, oid, keys)
		if err != nil {
			return err
		}

		if ver == 0 {
			*gone = true
			return nil
		}

		set, rm := fn(cur)
		if len(set) == 0 && len(rm) == 0 {
			return nil
		}

		op := casWriteOp(ver, set, rm, nil)
		err = op.Operate(u.idx, oid, rados.OperationNoFlag)
		op.Release()

		switch errCode(err) {
		case 0:
			return nil
		case syscall.ERANGE, syscall.EOVERFLOW, syscall.ENOENT:
			time.Sleep(casBackoff(attempt))
			continue
		}

		return err
	}

	return fmt.Errorf("%s: too many concurrent writers", oid)
}

// HeartbeatFTSLayoutNext records a heartbeat of the run tok in its
// "next" (the import reshard endpoint, §4.6), after the switch in its
// "old" entry until P7, so that a clear of stale runs does not take a
// working run for ended; false when the record carries no entry of tok.
func (u *User) HeartbeatFTSLayoutNext(tok string, at time.Time) (bool, error) {
	var ok bool

	err := casOmap(u, u.idx, oidFTSMap, []string{ftsLayoutKey}, func(cur map[string][]byte) (map[string][]byte, []string) {
		c, perr := parseLayout(cur[ftsLayoutKey])
		if perr != nil || c == nil {
			ok = false
			return nil, nil
		}

		switch i := oldOf(c, tok); {
		case c.Next != nil && c.Next.Tok == tok:
			c.Next.At, ok = at.Unix(), true
		case i >= 0:
			c.Old[i].At, ok = at.Unix(), true
		default:
			ok = false
			return nil, nil
		}

		return map[string][]byte{ftsLayoutKey: c.encode()}, nil
	})

	return ok, err
}

var _ model.FTSLayouter = (*User)(nil)
