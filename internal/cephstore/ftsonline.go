package cephstore

import (
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"shardpost/internal/metrics"
	"shardpost/internal/model"
)

// Online runs of fts_layout and fts_rebuild (stage B14i, temp/14): a
// reshard, a change of the identifier filter or a rebuild of the index
// without the account's maintenance lock. Deliveries, logins and
// sessions go on; users see at most a slightly slower search.
//
// The writers keep writing into the current generation only. While the
// layout record names an online run ("next" with "on"), the
// compare-and-swap of the id map that every indexing does anyway
// records the indexed GUIDs for the run: "w:<guid>" = "<tok>:<id>",
// and "w#" = "<tok>:<writes>" counts the writes of such keys
// (captureKeys). The run builds its target alone:
//
//	O1  without a lock: the cluster mark, the import lock, a foreign
//	    "next" or "old" (alive: busy; ended: cleared), the target and
//	    the mode (copy: the postings; reindex: the messages), the
//	    window (Admit).
//	O2  fence; the targets are created, then "next" = {target, tok, on,
//	    m}; from here on the writers record. The import lock is read
//	    again (an import that locked meanwhile wins: busy).
//	O3  every source posting object gets "!s" = frozen:<tok>, the soft
//	    freeze: a writer recorded for the run writes through it (once
//	    per operation it reads the record), one that read the record
//	    before O2 records itself first (registerBuild); a version
//	    before 0.51.0 waits as for an offline run.
//	O4  the snapshot of the id map after O3 (and, for reindex, the
//	    mailboxes projected to GUID, size and chunks).
//	O5  copy: P5 into the targets (no other writer there); reindex:
//	    every live message of the snapshot from its blob, with the
//	    target's text preparation, the ids of the snapshot and the fixed
//	    write (fixedWrite: every anomaly is errConversionLost).
//	O6  catch-up: the recorded pairs (GUID, id) not done yet, from their
//	    blobs into the target; once before the first switch the whole
//	    "i:" range against the snapshot (a writer of a version before
//	    0.51.0 does not record).
//	O7  fence; the switch: one compare-and-swap of the id map, only
//	    while "next" carries tok and "on" and "w#" is what the last
//	    catch-up round read before its listing. Otherwise more rounds;
//	    after fts.build_catchup_rounds a hold of new id allocations
//	    (fts.build_hold_ms), a last round and the switch.
//	O8  fence; the sources go, then the run's "w:" keys and "w#", then
//	    the "old" entry.
//
// A run that fails leaves "next" behind as an offline run does; its
// job, the runner's fallback, the jobs scrub or a lock taker removes it
// (ClearFTSLayoutNext, which also removes the run's "w:" keys). A run
// is never continued by another lease generation: the job that takes
// over clears it and starts again (R4.1).

// Keys of the id map an online run uses.
const (
	// buildKeyPrefix begins the record of an indexed GUID for an online
	// run: "w:<guid>" = "<tok>:<id>".
	buildKeyPrefix = "w:"
	// buildCountKey is "<tok>:<writes>", the number of writes of "w:"
	// keys for the run tok. "#" sorts before ":", so no scan of the "w:"
	// range reaches it.
	buildCountKey = "w#"

	// maxBuildHold bounds how long a writer waits for a run's hold,
	// whatever the record says.
	maxBuildHold = 5 * time.Second
)

// Phases of an online run (hook names; progress uses the names of
// onlineProgress).
const (
	phaseO2 = "O2"
	phaseO3 = "O3"
	phaseO5 = "O5"
	phaseO6 = "O6"
	phaseO7 = "O7"
	phaseO8 = "O8"
)

// Progress phases of an online run.
const (
	progressFreeze  = "O3"
	progressCatchup = "catchup"
	progressSwitch  = "switch"
	progressCleanup = "O8"
)

// Defaults of the online runs.
const (
	defaultCatchupRounds = 5
	defaultBuildHold     = 2 * time.Second
	defaultBatchDocs     = rebuildBatchDocs
	defaultBatchBytes    = rebuildBatchBytes
	// buildHolds is how often a run that does not converge holds new
	// allocations before it gives up.
	buildHolds = 3
	// buildReadAttempts are the attempts to read a live message.
	buildReadAttempts = 5
	// buildLayoutCheck is how often the copy reads the record to notice
	// a run that was taken over.
	buildLayoutCheck = 30 * time.Second
)

// buildCfg are the settings of the online runs.
type buildCfg struct {
	catchupRounds int
	hold          time.Duration
	noHold        bool
	batchDocs     int
	batchBytes    int
}

func (c buildCfg) withDefaults() buildCfg {
	if c.catchupRounds <= 0 {
		c.catchupRounds = defaultCatchupRounds
	}

	if c.hold <= 0 {
		c.hold = defaultBuildHold
	}

	c.hold = min(c.hold, maxBuildHold)

	if c.batchDocs <= 0 {
		c.batchDocs = defaultBatchDocs
	}

	if c.batchBytes <= 0 {
		c.batchBytes = defaultBatchBytes
	}

	return c
}

// errOnlineLayout refuses an online run that would convert between the
// layouts: those stay offline.
var errOnlineLayout = errors.New("fts layout: conversions between layout 2 and 3 run offline only")

// errOnlineBusy ends an online run without a change: another run is
// alive, or the account is locked.
var errOnlineBusy = errors.New("busy")

// errNotConverged: the switch found writes the last catch-up round did
// not see.
var errNotConverged = errors.New("not converged")

func buildKey(guid string) string { return buildKeyPrefix + guid }

// buildValue is the value of "w:<guid>" and "w#".
func buildValue(tok string, n uint64) []byte { return []byte(tok + ":" + strconv.FormatUint(n, 10)) }

// parseBuildValue splits a value of "w:<guid>" or "w#".
func parseBuildValue(v []byte) (tok string, n uint64, ok bool) {
	s := string(v)

	i := strings.LastIndexByte(s, ':')
	if i <= 0 {
		return "", 0, false
	}

	n, err := strconv.ParseUint(s[i+1:], 10, 64)
	if err != nil {
		return "", 0, false
	}

	return s[:i], n, true
}

// onlineNext is the "next" of an online run of a layout 3 record (nil:
// none).
func onlineNext(l *ftsLayout) *ftsGen {
	if l == nil || l.F != model.FTSLayout3 || l.Next == nil || !l.Next.On || l.Next.F != model.FTSLayout3 {
		return nil
	}

	return l.Next
}

// holding reports whether the run holds new allocations at now.
func holding(run *ftsGen, now time.Time) bool {
	return run.Hold > 0 && now.Before(time.UnixMilli(run.Hold))
}

// captureKeys are the keys that record the GUIDs for the run tok in the
// id map cur (read with "w:<guid>" and "w#"): "w:<guid>" =
// "<tok>:<id>" for every GUID whose key carries another value (a GUID
// indexed again with a new id is recorded again, R2.4), and "w#"
// counting the writes; nil when every GUID is recorded already. idOf
// gives the id of a GUID (false: none, nothing to record).
func captureKeys(cur map[string][]byte, tok string, guids []string, idOf func(string) (uint64, bool)) (map[string][]byte, int) {
	var set map[string][]byte

	for _, g := range guids {
		id, ok := idOf(g)
		if !ok {
			continue
		}

		want := buildValue(tok, id)
		if string(cur[buildKey(g)]) == string(want) {
			continue
		}

		if set == nil {
			set = map[string][]byte{}
		}

		set[buildKey(g)] = want
	}

	if len(set) == 0 {
		return nil, 0
	}

	n := uint64(0)
	if t, c, ok := parseBuildValue(cur[buildCountKey]); ok && t == tok {
		n = c
	}

	set[buildCountKey] = buildValue(tok, n+uint64(len(set)))

	return set, len(set) - 1
}

// registerBuild records the documents of the indexing w for the online
// run the record names (a writer that read the record before the run's
// O2 and met its freeze, §4.3): one compare-and-swap of the id map with
// the record, "w#" and the documents' keys, only while the record names
// an online run of the generation of w. A hold of the run is waited for
// once, outside the compare-and-swap. It returns the record its last
// round read; w.built is set when it recorded.
func (u *User) registerBuild(w *ftsWrite) (*ftsLayout, error) {
	guids := slices.Sorted(maps.Keys(w.ids))

	keys := make([]string, 0, 2*len(guids)+2)
	for _, g := range guids {
		keys = append(keys, docKey(g), buildKey(g))
	}

	keys = append(keys, ftsLayoutKey, buildCountKey)

	for {
		var r registerRound

		err := casOmap(u, u.idx, oidFTSMap, keys, func(cur map[string][]byte) (map[string][]byte, []string) {
			r = registerRound{}
			return u.registerSet(w, guids, cur, &r), nil
		})

		switch {
		case err != nil:
			return nil, fmt.Errorf("fts docids: %w", err)
		case r.perr != nil:
			return nil, r.perr
		case !r.hold.IsZero():
			w.held = true

			time.Sleep(min(time.Until(r.hold), maxBuildHold))

			continue
		}

		if run := onlineNext(r.l); run != nil && curLoc(r.l, u.ftsShards).same(w.loc) {
			w.built = run.Tok

			if r.n > 0 {
				metrics.FTSBuildRegistered.WithLabelValues("crossing").Add(float64(r.n))
			}
		}

		return r.l, nil
	}
}

// registerRound is what a round of registerBuild found.
type registerRound struct {
	l    *ftsLayout
	perr error
	hold time.Time
	n    int
}

// registerSet is the compare-and-swap function of registerBuild: the
// keys that record the GUIDs with the ids they have now (a GUID removed
// meanwhile has none), nothing while the run holds or when the record
// names no online run of the generation of w.
func (u *User) registerSet(w *ftsWrite, guids []string, cur map[string][]byte, r *registerRound) map[string][]byte {
	if r.l, r.perr = parseLayout(cur[ftsLayoutKey]); r.perr != nil || !curLoc(r.l, u.ftsShards).same(w.loc) {
		return nil
	}

	run := onlineNext(r.l)
	if run == nil {
		return nil
	}

	set, n := captureKeys(cur, run.Tok, guids, idsIn(cur))
	if set != nil && !w.held && holding(run, time.Now()) {
		r.hold = time.UnixMilli(run.Hold)
		return nil
	}

	r.n = n

	return set
}

// idsIn gives the ids of the GUIDs as the id map cur (read with their
// "g:" keys) has them.
func idsIn(cur map[string][]byte) func(string) (uint64, bool) {
	return func(g string) (uint64, bool) {
		v, ok := cur[docKey(g)]
		if !ok {
			return 0, false
		}

		id, err := strconv.ParseUint(string(v), 10, 64)

		return id, err == nil
	}
}

// eachBuildPage calls fn for every page of the "w:" keys of the id map
// (none for an absent id map).
func (u *User) eachBuildPage(fn func(page map[string][]byte) error) error {
	after := ""

	for {
		page, err := u.idx.GetOmapValues(oidFTSMap, after, buildKeyPrefix, omapPageSize)
		if errCode(err) == syscall.ENOENT {
			return nil
		}

		if err != nil {
			return fmt.Errorf("fts build keys: %w", err)
		}

		if err := fn(page); err != nil {
			return err
		}

		if len(page) < omapPageSize {
			return nil
		}

		for k := range page {
			after = max(after, k)
		}
	}
}

// dropBuildKeys removes the "w:" keys and "w#" whose token match
// accepts, page by page, each page in a compare-and-swap with the
// record: only keys whose token is not the one of the record's "next"
// in the state read go (§5.6), so that a run that starts meanwhile and
// records the same GUID keeps its key. It returns how many it removed.
func (u *User) dropBuildKeys(match func(tok string) bool) (int, error) {
	removed := 0
	first := true

	err := u.eachBuildPage(func(page map[string][]byte) error {
		var cand []string

		for k, v := range page {
			if t, _, ok := parseBuildValue(v); !ok || match(t) {
				cand = append(cand, k)
			}
		}

		n, err := u.dropBuildPage(cand, match)
		removed += n
		first = false

		return err
	})
	if err != nil || !first {
		return removed, err
	}

	// no "w:" key: "w#" alone
	n, err := u.dropBuildPage(nil, match)

	return removed + n, err
}

// dropBuildPage removes the keys of one page (and "w#") in a
// compare-and-swap with the record.
func (u *User) dropBuildPage(cand []string, match func(tok string) bool) (int, error) {
	keys := append(slices.Clone(cand), buildCountKey, ftsLayoutKey)
	n := 0

	err := casOmap(u, u.idx, oidFTSMap, keys, func(cur map[string][]byte) (map[string][]byte, []string) {
		n = 0

		l, perr := parseLayout(cur[ftsLayoutKey])
		if perr != nil {
			return nil, nil
		}

		next := nextTok(l)

		var rm []string

		for _, k := range append(slices.Clone(cand), buildCountKey) {
			v, ok := cur[k]
			if !ok {
				continue
			}

			if t, _, okv := parseBuildValue(v); okv && (t == next || !match(t)) {
				continue
			}

			rm = append(rm, k)
		}

		n = len(rm)

		return nil, rm
	})

	return n, err
}

// nextTok is the token of the record's "next" ("" for none).
func nextTok(l *ftsLayout) string {
	if l == nil || l.Next == nil {
		return ""
	}

	return l.Next.Tok
}

// foreignBuildKeys counts the keys of vals ("w:" keys or "w#") whose
// token is not next.
func foreignBuildKeys(vals map[string][]byte, next string) int {
	n := 0

	for _, v := range vals {
		if t, _, ok := parseBuildValue(v); !ok || t != next {
			n++
		}
	}

	return n
}

// buildLeftover counts the "w:" keys and "w#" of runs other than the
// one the record names (the scrub's finding fts_build_leftover).
func (u *User) buildLeftover(next string) (int, error) {
	vals, err := getOmapKeys(u.idx, oidFTSMap, []string{buildCountKey})
	if err != nil && errCode(err) != syscall.ENOENT {
		return 0, err
	}

	n := foreignBuildKeys(vals, next)

	err = u.eachBuildPage(func(page map[string][]byte) error {
		n += foreignBuildKeys(page, next)
		return nil
	})

	return n, err
}

// --- the run ---------------------------------------------------------------------

// blobLoc is where the blob of a GUID is: its size and chunks.
type blobLoc struct {
	size   int64
	chunks int
}

// buildPair is one recorded document.
type buildPair struct {
	guid string
	id   uint64
}

// onlineRun is one online run.
type onlineRun struct {
	u    *User
	req  model.FTSLayoutRequest
	rep  *model.FTSLayoutReport
	mode string

	src, target ftsLoc

	// the snapshot of O4
	live   map[uint64]bool
	byGUID map[string]uint64
	// proj maps the GUIDs of the mailboxes to their blobs (reindex).
	proj map[string]blobLoc
	// done are the pairs indexed or found dead.
	done map[buildPair]bool

	// checked is when fence last looked at the record (Unix nanoseconds).
	checked atomic.Int64
}

// convertOnline is ConvertFTSLayout for an online request.
func (u *User) convertOnline(req model.FTSLayoutRequest) (rep model.FTSLayoutReport, err error) {
	req = withRequestDefaults(req)
	rep.Online = true

	r := &onlineRun{u: u, req: req, rep: &rep, done: map[buildPair]bool{}}

	defer func() {
		if errors.Is(err, errOnlineBusy) {
			rep.Deferred, rep.Reason, err = model.FTSDeferredBusy, "another run of the account is alive, or it is locked", nil
		}

		countOnline(rep, err)
		u.ftsForget()
	}()

	err = r.run()

	return rep, err
}

// countOnline counts a run in imapceph_fts_build_runs_total.
func countOnline(rep model.FTSLayoutReport, err error) {
	mode, result := rep.Mode, "ok"
	if mode == "" {
		mode = model.FTSModeCopy
	}

	switch {
	case errors.Is(err, errConversionLost):
		result = "lost"
	case err != nil:
		result = "error"
	case rep.Deferred == model.FTSDeferredBusy:
		result = "busy"
	case rep.Deferred != "":
		result = "deferred"
	case rep.Unsupported:
		result = "unsupported"
	case rep.Noop:
		result = "noop"
	}

	metrics.FTSBuildRuns.WithLabelValues(mode, result).Inc()
}

// run is O1 to O8.
func (r *onlineRun) run() error {
	u, req := r.u, r.req

	if err := u.requireOnlineMark(); err != nil {
		return err
	}

	if req.Layout == model.FTSLayout2 {
		return errOnlineLayout
	}

	l, err := u.readLayout()
	if err != nil {
		return err
	}

	src := curLoc(l, u.ftsShards)
	r.rep.From = src.shape()

	// A run of this token that switched has only O8 left.
	if old := pastP6(l, req.Tok); old != nil {
		r.rep.Resumed, r.rep.To = true, src.shape()
		r.src = genLoc(*old, u.ftsShards)

		return r.finish()
	}

	if !src.v3() {
		r.rep.Unsupported, r.rep.To = true, src.shape()
		r.rep.Reason = "the account has no layout 3 index; layout 2 indexes are converted offline"

		return nil
	}

	if l, err = r.clearForeign(l); err != nil {
		return err
	}

	r.src = curLoc(l, u.ftsShards)

	target, err := r.targetOf()
	if err != nil || target == nil {
		r.rep.Noop, r.rep.Reason, r.rep.To = err == nil, reasonNothing, r.src.shape()
		return err
	}

	r.target, r.rep.Mode, r.rep.To = *target, r.mode, target.shape()

	if ok, err := r.admit(); err != nil || !ok {
		return err
	}

	return r.phases()
}

// requireOnlineMark refuses an online run without the cluster mark
// (read now, O1(a)).
func (u *User) requireOnlineMark() error {
	if u.store == nil {
		return model.ErrFTSOnlineMark
	}

	ok, err := u.store.FTSOnlineEnabled()
	if err == nil && !ok {
		err = model.ErrFTSOnlineMark
	}

	return err
}

// alive reports whether the run tok may still be working.
func (r *onlineRun) alive(tok string, at int64) (bool, error) {
	if r.req.ForeignAlive == nil {
		return false, nil
	}

	var t time.Time
	if at > 0 {
		t = time.Unix(at, 0)
	}

	return r.req.ForeignAlive(tok, t)
}

// clearForeign is O1(b) to O1(d): busy while the account is locked or
// another run is alive; the "next" and "old" of runs that ended go (and
// their "w:" keys). It returns the record after it.
func (r *onlineRun) clearForeign(l *ftsLayout) (*ftsLayout, error) {
	u := r.u

	switch locked, err := u.ImportLocked(); {
	case err != nil:
		return l, err
	case locked:
		return l, errOnlineBusy
	}

	if l.Next != nil {
		if err := r.clearEnded(*l.Next, false); err != nil {
			return l, err
		}

		r.rep.Cleared = l.Next.Tok
	}

	for _, o := range l.Old {
		if err := r.clearEnded(o, true); err != nil {
			return l, err
		}
	}

	return u.readLayout()
}

// clearEnded removes the "next" (old: an "old" entry) of the run g when
// it has ended, with its "w:" keys; errOnlineBusy while it may still be
// working (an "old" entry: a run in its O8, R2.6).
func (r *onlineRun) clearEnded(g ftsGen, old bool) error {
	switch alive, err := r.alive(g.Tok, g.At); {
	case err != nil:
		return err
	case alive && (old || g.Tok != r.req.Tok):
		return errOnlineBusy
	}

	if err := r.req.Fence(); err != nil {
		return err
	}

	if !old {
		_, err := r.u.ClearFTSLayoutNext(g.Tok)
		return err
	}

	if err := r.u.removeOld(g); err != nil {
		return err
	}

	_, err := r.u.dropBuildKeys(func(t string) bool { return t == g.Tok })

	return err
}

// targetOf is O1(e): the target and the mode; nil when there is nothing
// to do (due_only).
func (r *onlineRun) targetOf() (*ftsLoc, error) {
	u, req, src := r.u, r.req, r.src

	if !req.Rebuild {
		t, err := u.layout3Target(req, src)
		if err != nil || (req.DueOnly && sameShape(t, src) && t.sameProps(src)) {
			return nil, err
		}

		// layout3Target keeps the text preparation of the postings
		// (sourceText): the copy is exact.
		r.mode = model.FTSModeCopy

		return &t, nil
	}

	dx, ah, err := u.genProps()
	if err != nil {
		return nil, err
	}

	shape, _, err := u.reshardTarget(src, dx, runSample(src))
	if err != nil {
		return nil, err
	}

	t := ftsLoc{f: model.FTSLayout3, g: newFTSGen(), s: shape.s, d: shape.d, h: shape.h, dx: dx, ah: ah}
	r.mode = model.FTSModeReindex

	return &t, nil
}

// admit is O1(f): the window of a run the worker or a batch planned.
func (r *onlineRun) admit() (bool, error) {
	if r.req.Admit == nil {
		return true, nil
	}

	vals, err := getOmapKeys(r.u.idx, oidFTSMap, []string{ftsCountKey})
	if err != nil {
		return false, err
	}

	switch err := r.req.Admit(r.mode, atoiMarker(vals[ftsCountKey])); {
	case errors.Is(err, model.ErrFTSWindowTooSmall):
		r.rep.Deferred, r.rep.Reason = model.FTSDeferredTooSmall, err.Error()
		return false, nil
	case errors.Is(err, model.ErrFTSBuildWindow):
		r.rep.Deferred, r.rep.Reason = model.FTSDeferredWindow, err.Error()
		return false, nil
	case err != nil:
		return false, err
	}

	return true, nil
}

// phases runs O2 to O8.
func (r *onlineRun) phases() error {
	steps := []struct {
		name string
		fn   func() error
	}{
		{phaseO2, r.record}, {phaseO3, r.freeze}, {phaseO5, r.build}, {phaseO6, r.switchOver}, {phaseO8, r.finish},
	}

	for _, s := range steps {
		start := time.Now()

		if err := s.fn(); err != nil {
			return err
		}

		metrics.FTSBuildPhase.WithLabelValues(s.name).Observe(time.Since(start).Seconds())
	}

	return nil
}

// fence is the request's fence and, every buildLayoutCheck, a look at
// the record: a run whose "next" is gone or carries another token was
// taken over.
func (r *onlineRun) fence() error {
	if err := r.req.Fence(); err != nil {
		return err
	}

	// the writers of the copy fence at the same time: one of them looks
	last := r.checked.Load()
	if time.Since(time.Unix(0, last)) < buildLayoutCheck || !r.checked.CompareAndSwap(last, time.Now().UnixNano()) {
		return nil
	}

	return r.mine()
}

// mine checks that the record names this run.
func (r *onlineRun) mine() error {
	l, err := r.u.readLayout()
	if err != nil {
		return err
	}

	if run := onlineNext(l); run == nil || run.Tok != r.req.Tok {
		return errConversionLost
	}

	return nil
}

// record is O2.
func (r *onlineRun) record() error {
	u, req := r.u, r.req
	req.Progress(phaseO2, 0, 1)

	if err := req.Fence(); err != nil {
		return err
	}

	// Props before a target with properties is created (and named).
	if r.target.hasProps() {
		if err := u.markLayoutProps(true); err != nil {
			return err
		}
	}

	if err := u.createGen(r.target); err != nil {
		_ = u.deleteObjects(r.target.objects())
		return err
	}

	next := r.target.gen()
	next.Tok, next.On, next.M = req.Tok, true, r.mode

	if err := u.setNext(nil, &next); err != nil {
		return r.notRecorded(next, err)
	}

	u.ftsForget()

	if err := u.hook(phaseO2); err != nil {
		return err
	}

	return r.importAfterRecord()
}

// notRecorded undoes O2 after its "next" could not be recorded: the
// targets go, Props follows the current generation again; another run
// that recorded itself meanwhile makes this one busy.
func (r *onlineRun) notRecorded(next ftsGen, err error) error {
	u := r.u

	_ = u.deleteObjects(r.target.objects())

	if l, lerr := u.readLayout(); lerr == nil && ftsLayout3(l) {
		_ = u.presenceAfterClear(next, l)
	}

	if errors.Is(err, errConversionLost) {
		return errOnlineBusy
	}

	return err
}

// importAfterRecord reads the import lock again after O2: an import that
// locked between O1 and O2 has the account (R2.9); the run's "next"
// goes and the run is busy.
func (r *onlineRun) importAfterRecord() error {
	switch locked, err := r.u.ImportLocked(); {
	case err != nil:
		return err
	case !locked:
		return nil
	}

	if _, err := r.u.ClearFTSLayoutNext(r.req.Tok); err != nil {
		return err
	}

	return errOnlineBusy
}

// freeze is O3: the soft freeze of every source posting object, each
// after a look at the record (a run that was taken over stops before it
// overwrites the freeze of the run that took over).
func (r *onlineRun) freeze() error {
	u, req, src := r.u, r.req, r.src
	state := []byte(frozenPrefix + req.Tok)

	if err := req.Fence(); err != nil {
		return err
	}

	for i, oid := range src.postingOIDs() {
		req.Progress(progressFreeze, i, src.s)

		if err := r.mine(); err != nil {
			return err
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

	return u.hook(phaseO3)
}

// build is O4 and O5.
func (r *onlineRun) build() error {
	u := r.u

	live, byGUID, _, err := u.liveDocs()
	if err != nil {
		return err
	}

	r.live, r.byGUID = live, byGUID
	r.rep.Docs = int64(len(live))

	if r.mode == model.FTSModeCopy {
		err = r.copyPostings()
	} else {
		err = r.reindex()
	}

	if err != nil {
		return err
	}

	return u.hook(phaseO5)
}

// copyPostings is O5 of the mode copy: P5 into the targets, which only
// this run writes.
func (r *onlineRun) copyPostings() error {
	u, target := r.u, r.target
	st := &convertStats{docs: int64(len(r.live))}

	req := r.req
	req.Fence = r.fence
	req.Progress = func(_ string, done, total int) { r.req.Progress(model.FTSModeCopy, done, total) }

	w := u.newPieceWriter(target, r.fence)
	defer w.stop()

	c := &copier{u: u, target: target, live: r.live, byGUID: r.byGUID, st: st, terms: map[string]int64{}, dicts: map[string]int64{},
		posts: newChunkWriter(w), dict: newChunkWriter(w), heads: newChunkWriter(w)}

	if err := c.postings(req, r.src); err != nil {
		return err
	}

	if err := c.headers(r.src); err != nil {
		return err
	}

	err := w.finish()
	st.writes, st.buffer = w.writes.Load(), w.peak

	addDictKeyMetrics(c.triKeys, c.startKeys)

	if err != nil {
		return err
	}

	if err := u.writeCounters(req, target, c.terms, c.dicts); err != nil {
		return err
	}

	r.rep.Terms, r.rep.Dict, r.rep.Headers = st.terms, st.dict, st.headers
	r.rep.Writes, r.rep.BufferKeys = st.writes, st.buffer

	return nil
}

// reindex is O5 of the mode reindex: every live message of the snapshot
// from its blob into the target, then the messages of the mailboxes
// without an id through the writers (they are recorded and caught up).
func (r *onlineRun) reindex() error {
	u := r.u

	proj, err := u.blobProjection()
	if err != nil {
		return err
	}

	r.proj = proj

	pairs := make([]buildPair, 0, len(r.byGUID))
	for g, id := range r.byGUID {
		pairs = append(pairs, buildPair{g, id})
	}

	slices.SortFunc(pairs, func(a, b buildPair) int { return compareID(a.id, b.id) })

	if err := r.indexPairs(pairs, model.FTSModeReindex); err != nil {
		return err
	}

	return r.indexUnknown()
}

func compareID(a, b uint64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}

	return 0
}

// indexUnknown indexes the messages of the mailboxes that have no id
// (index gaps, an offline rebuild that stopped): through the writers,
// whose id allocation records them for the catch-up; the source is
// repaired on the way.
func (r *onlineRun) indexUnknown() error {
	u := r.u

	var (
		docs []ftsDoc
		size int
	)

	flush := func() error {
		if len(docs) == 0 {
			return nil
		}

		if err := r.fence(); err != nil {
			return err
		}

		if err := u.ftsIndexDocs(docs); err != nil {
			return fmt.Errorf("index: %w", err)
		}

		docs, size = docs[:0], 0

		return nil
	}

	for _, g := range slices.Sorted(maps.Keys(r.proj)) {
		if _, ok := r.byGUID[g]; ok {
			continue
		}

		ref := r.proj[g]

		raw, err := u.readBlob(&Meta{GUID: g, Size: ref.size, Chunks: ref.chunks})
		if err != nil {
			// gone meanwhile, or unreadable: the source misses it as well
			u.log.Warn("fts online rebuild: message without id not readable", "guid", g, "err", err)
			continue
		}

		docs = append(docs, ftsDoc{guid: g, raw: raw})
		size += len(raw)

		if len(docs) >= u.ftsCfg.build.batchDocs || size >= u.ftsCfg.build.batchBytes {
			if err := flush(); err != nil {
				return err
			}
		}
	}

	return flush()
}

// indexPairs indexes documents from their blobs into the target with the
// fixed write, in batches; pairs found dead count as done.
func (r *onlineRun) indexPairs(pairs []buildPair, phase string) error {
	b := &fixedBatch{r: r, ids: map[string]uint64{}}

	for i, p := range pairs {
		r.req.Progress(phase, i, len(pairs))

		if r.done[p] {
			continue
		}

		raw, ok, err := r.readDoc(p)
		if err != nil {
			return err
		}

		if !ok {
			r.done[p] = true
			continue
		}

		if err := b.add(p, raw); err != nil {
			return err
		}
	}

	if err := b.flush(); err != nil {
		return err
	}

	r.req.Progress(phase, len(pairs), len(pairs))

	return nil
}

// fixedBatch is a batch of documents an online run indexes into its
// target (indexPairs).
type fixedBatch struct {
	r    *onlineRun
	docs []ftsDoc
	ids  map[string]uint64
	size int
	next []buildPair
}

// add adds a document and writes the batch when it is full. The same
// GUID with another id goes into the next batch (the older id is dead:
// ids are never reused).
func (b *fixedBatch) add(p buildPair, raw []byte) error {
	if _, dup := b.ids[p.guid]; dup {
		if err := b.flush(); err != nil {
			return err
		}
	}

	b.docs = append(b.docs, ftsDoc{guid: p.guid, raw: raw})
	b.ids[p.guid] = p.id
	b.next = append(b.next, p)
	b.size += len(raw)

	if cfg := b.r.u.ftsCfg.build; len(b.docs) >= cfg.batchDocs || b.size >= cfg.batchBytes {
		return b.flush()
	}

	return nil
}

// flush writes the batch, fenced, and marks its pairs done.
func (b *fixedBatch) flush() error {
	if len(b.docs) == 0 {
		return nil
	}

	r := b.r

	if err := r.fence(); err != nil {
		return err
	}

	if err := r.u.indexFixed(r.target, b.docs, b.ids); err != nil {
		return err
	}

	for _, p := range b.next {
		r.done[p] = true
	}

	r.rep.Indexed += int64(len(b.docs))
	b.docs, b.ids, b.size, b.next = b.docs[:0], map[string]uint64{}, 0, b.next[:0]

	return nil
}

// indexFixed indexes the documents into loc with the given ids and the
// fixed write of an online run.
func (u *User) indexFixed(loc ftsLoc, docs []ftsDoc, ids map[string]uint64) error {
	w := &ftsWrite{fixed: true, loc: loc}

	return u.indexWith(w, &ftsBatch{docs: docs, ids: ids})
}

// readDoc reads the blob of a document (§4.7b): false when the document
// is dead (its GUID has no id any more, or another one) or an orphan
// (no blob and no mailbox entry; counted and, with drop_orphan_ids,
// dropped). A live message that cannot be read is tried
// buildReadAttempts times; then the run fails, or with skip_unreadable
// skips it.
func (r *onlineRun) readDoc(p buildPair) ([]byte, bool, error) {
	u := r.u

	var last error

	for attempt := 0; attempt < buildReadAttempts; attempt++ {
		raw, inProj, err := r.readOnce(p)
		if err == nil {
			return raw, true, nil
		}

		last = err

		live, err := r.liveID(p)
		if err != nil {
			return nil, false, err
		}

		if !live {
			return nil, false, nil // removed meanwhile: done, no attempt
		}

		if !inProj && errors.Is(last, model.ErrNoBlob) {
			return nil, false, r.orphan(p)
		}

		if attempt+1 < buildReadAttempts {
			time.Sleep(time.Duration(100<<attempt) * time.Millisecond)
		}
	}

	if r.req.SkipUnreadable {
		r.rep.Unreadable++

		u.log.Warn("fts online run: live message not readable, skipped", "guid", p.guid, "err", last)

		return nil, false, nil
	}

	return nil, false, fmt.Errorf("read %s: %w", p.guid, last)
}

// readOnce reads the blob of a document: its size and chunks from the
// projection of the mailboxes (inProj), or else from the blob itself.
func (r *onlineRun) readOnce(p buildPair) (raw []byte, inProj bool, err error) {
	ref, inProj := r.proj[p.guid]
	if !inProj {
		size, err := r.u.BlobSize(p.guid)
		if err != nil {
			return nil, false, err
		}

		ref = blobLoc{size: size, chunks: r.u.chunkCount(size)}
	}

	raw, err = r.u.readBlob(&Meta{GUID: p.guid, Size: ref.size, Chunks: ref.chunks})

	return raw, inProj, err
}

// liveID reports whether the GUID still has the id of the pair.
func (r *onlineRun) liveID(p buildPair) (bool, error) {
	vals, err := getOmapKeys(r.u.idx, oidFTSMap, []string{docKey(p.guid)})
	if err != nil {
		return false, err
	}

	v, ok := vals[docKey(p.guid)]

	return ok && string(v) == strconv.FormatUint(p.id, 10), nil
}

// orphan handles an id without a blob and without a mailbox entry (an id
// whose release failed, R2.3).
func (r *onlineRun) orphan(p buildPair) error {
	r.rep.OrphanIDs++
	r.u.log.Warn("fts online run: id without blob and mailbox entry", "guid", p.guid, "id", p.id, "drop", r.req.DropOrphanIDs)

	if !r.req.DropOrphanIDs {
		return nil
	}

	return r.u.dropDocIDOf(p.guid, p.id)
}

// dropDocIDOf is dropDocID only while the GUID still has the id.
func (u *User) dropDocIDOf(guid string, id uint64) error {
	return casOmap(u, u.idx, oidFTSMap, []string{docKey(guid), ftsLayoutKey, ftsCountKey}, func(cur map[string][]byte) (map[string][]byte, []string) {
		if string(cur[docKey(guid)]) != strconv.FormatUint(id, 10) {
			return nil, nil
		}

		rm := []string{docKey(guid), idKey(id)}

		if lay, _ := parseLayout(cur[ftsLayoutKey]); lay == nil || lay.F != model.FTSLayout3 {
			return nil, rm
		}

		return map[string][]byte{ftsCountKey: itoaMarker(atoiMarker(cur[ftsCountKey]) - 1)}, rm
	})
}

// blobProjection lists the mailboxes a page at a time and keeps of every
// message only its GUID, size and chunks (§4.7 step 2: no whole Meta of
// every mailbox in memory).
func (u *User) blobProjection() (map[string]blobLoc, error) {
	boxes, err := u.ListMailboxes()
	if err != nil {
		return nil, err
	}

	out := map[string]blobLoc{}

	for i := range boxes {
		after := uint32(0)

		for {
			page, err := u.ListMessagesAfter(boxes[i].ID, after, omapPageSize)
			if errors.Is(err, ErrNoMailbox) {
				break
			}

			if err != nil {
				return nil, fmt.Errorf("mailbox %q: %w", boxes[i].Name, err)
			}

			if len(page) == 0 {
				break
			}

			for j := range page {
				m := &page[j]
				out[m.GUID] = blobLoc{size: m.Size, chunks: max(m.Chunks, 1)}
				after = max(after, m.UID)
			}
		}
	}

	return out, nil
}

// --- catch-up and switch -----------------------------------------------------------

// switchOver is O6 and O7.
func (r *onlineRun) switchOver() error {
	cfg := r.u.ftsCfg.build

	// a first round, then once, before the first switch, the whole id
	// map against the snapshot (§4.8 step 5)
	if _, _, err := r.catchup(); err != nil {
		return err
	}

	if err := r.compareIDs(); err != nil {
		return err
	}

	for holds := 0; ; holds++ {
		if done, err := r.converge(); err != nil || done {
			return err
		}

		if cfg.noHold || holds >= buildHolds {
			return fmt.Errorf("fts online run: no switch after %d catch-up rounds and %d holds: the account gets too many deliveries; "+
				"run it again later or raise fts.build_hold_ms", r.rep.Rounds, holds)
		}

		if done, err := r.holdAndSwitch(); err != nil || done {
			return err
		}
	}
}

// converge runs catch-up rounds (more while they find new documents)
// and switch tries, fts.build_catchup_rounds of them; true when it
// switched.
func (r *onlineRun) converge() (bool, error) {
	rounds := r.u.ftsCfg.build.catchupRounds

	for tries := 0; tries < rounds; tries++ {
		count, found, err := r.catchup()
		if err != nil {
			return false, err
		}

		if found > 0 && tries+1 < rounds {
			continue
		}

		switch err := r.trySwitch(count); {
		case err == nil:
			return true, r.switched()
		case !errors.Is(err, errNotConverged):
			return false, err
		}
	}

	return false, nil
}

// switched ends O7.
func (r *onlineRun) switched() error {
	metrics.FTSBuildCatchupRounds.Observe(float64(r.rep.Rounds))

	return r.u.hook(phaseO7)
}

// holdAndSwitch holds new id allocations, catches up once more and
// tries the switch; true when it switched. A failed try lifts the hold.
func (r *onlineRun) holdAndSwitch() (bool, error) {
	u := r.u
	hold := u.ftsCfg.build.hold
	start := time.Now()

	if err := r.setHold(start.Add(hold)); err != nil {
		return false, err
	}

	defer func() {
		r.rep.HoldMS += time.Since(start).Milliseconds()
		metrics.FTSBuildHold.Observe(time.Since(start).Seconds())
	}()

	count, _, err := r.catchup()
	if err == nil {
		err = r.trySwitch(count)
	}

	switch {
	case err == nil:
		return true, r.switched()
	case !errors.Is(err, errNotConverged):
		return false, err
	}

	return false, r.setHold(time.Time{})
}

// setHold sets (zero: removes) the hold of the run's "next".
func (r *onlineRun) setHold(until time.Time) error {
	if err := r.req.Fence(); err != nil {
		return err
	}

	var lost bool

	err := casOmap(r.u, r.u.idx, oidFTSMap, []string{ftsLayoutKey}, func(cur map[string][]byte) (map[string][]byte, []string) {
		c, perr := parseLayout(cur[ftsLayoutKey])
		if lost = perr != nil || onlineNext(c) == nil || c.Next.Tok != r.req.Tok; lost {
			return nil, nil
		}

		c.Next.Hold = 0
		if !until.IsZero() {
			c.Next.Hold = until.UnixMilli()
		}

		return map[string][]byte{ftsLayoutKey: c.encode()}, nil
	})
	if err == nil && lost {
		err = errConversionLost
	}

	return err
}

// catchup is one round of O6: it reads "w#" (count), then every "w:" key
// of the run, and indexes the pairs not done yet; found is their number.
func (r *onlineRun) catchup() (count []byte, found int, err error) {
	u, tok := r.u, r.req.Tok
	r.rep.Rounds++
	r.req.Progress(progressCatchup, r.rep.Rounds, 0)

	if err := r.req.Fence(); err != nil {
		return nil, 0, err
	}

	if err := r.mine(); err != nil {
		return nil, 0, err
	}

	vals, err := getOmapKeys(u.idx, oidFTSMap, []string{buildCountKey})
	if err != nil {
		return nil, 0, err
	}

	count = vals[buildCountKey]

	pairs, err := r.pendingPairs(tok)
	if err != nil {
		return nil, 0, err
	}

	before := r.rep.Indexed

	if err := r.indexPairs(pairs, progressCatchup); err != nil {
		return nil, 0, err
	}

	r.rep.CaughtUp += r.rep.Indexed - before

	return count, len(pairs), u.hook(phaseO6)
}

// pendingPairs lists the pairs recorded for the run tok that are not
// done yet, by id.
func (r *onlineRun) pendingPairs(tok string) ([]buildPair, error) {
	var pairs []buildPair

	err := r.u.eachBuildPage(func(page map[string][]byte) error {
		for k, v := range page {
			if t, id, ok := parseBuildValue(v); ok && t == tok {
				if p := (buildPair{strings.TrimPrefix(k, buildKeyPrefix), id}); !r.done[p] {
					pairs = append(pairs, p)
				}
			}
		}

		return nil
	})

	slices.SortFunc(pairs, func(a, b buildPair) int { return compareID(a.id, b.id) })

	return pairs, err
}

// compareIDs reads every "i:" key and indexes the live documents that
// are neither in the snapshot nor recorded for the run: those of a
// writer of a version before 0.51.0 that never met the freeze (a
// message without terms, §5.4). It also counts the snapshot's documents
// that died since (dead_ids).
func (r *onlineRun) compareIDs() error {
	var extra []buildPair

	now := map[uint64]bool{}

	err := r.u.idMap(func(id uint64, guid string) {
		now[id] = true

		if !r.live[id] {
			extra = append(extra, buildPair{guid, id})
		}
	})
	if err != nil {
		return err
	}

	for id := range r.live {
		if !now[id] {
			r.rep.DeadIDs++
		}
	}

	if len(extra) == 0 {
		return nil
	}

	recorded, err := r.recordsOf(extra)
	if err != nil {
		return err
	}

	var todo []buildPair

	for _, p := range extra {
		if string(recorded[buildKey(p.guid)]) == string(buildValue(r.req.Tok, p.id)) || r.done[p] {
			continue
		}

		todo = append(todo, p)
	}

	if len(todo) > 0 {
		r.u.log.Info("fts online run: documents of writers that did not record them caught up", "count", len(todo))
	}

	return r.indexPairs(todo, progressCatchup)
}

// recordsOf reads the "w:" keys of the GUIDs of pairs, a page of keys
// at a time.
func (r *onlineRun) recordsOf(pairs []buildPair) (map[string][]byte, error) {
	recorded := map[string][]byte{}

	for page := range slices.Chunk(pairs, omapPageSize) {
		keys := make([]string, 0, len(page))
		for _, p := range page {
			keys = append(keys, buildKey(p.guid))
		}

		vals, err := getOmapKeys(r.u.idx, oidFTSMap, keys)
		if err != nil {
			return nil, err
		}

		maps.Copy(recorded, vals)
	}

	return recorded, nil
}

// trySwitch is O7: the switch compare-and-swap, only while "next"
// carries the run's token and "on" and "w#" is count (what the last
// catch-up round read before its listing: every write it counts is in
// that listing, or was replaced by a later write of the same GUID, whose
// earlier id is dead then). errNotConverged when "w#" moved on.
func (r *onlineRun) trySwitch(count []byte) error {
	u, req := r.u, r.req
	req.Progress(progressSwitch, 0, 1)

	if err := req.Fence(); err != nil {
		return err
	}

	var lost, moved bool

	err := casOmap(u, u.idx, oidFTSMap, []string{ftsLayoutKey, buildCountKey, ftsVersionKey, ftsVersionKey3, ftsRebuildKey, ftsCountKey, "n"},
		func(cur map[string][]byte) (map[string][]byte, []string) {
			c, perr := parseLayout(cur[ftsLayoutKey])
			if lost = perr != nil || c == nil || c.Next == nil || c.Next.Tok != req.Tok || !c.Next.On; lost {
				return nil, nil
			}

			if moved = string(cur[buildCountKey]) != string(count); moved {
				return nil, nil
			}

			return switchOnline(cur, c, r.src, r.target, req.Tok, r.mode)
		})

	switch {
	case err != nil:
		return err
	case lost:
		return errConversionLost
	case moved:
		return errNotConverged
	}

	u.ftsForget()

	if err := u.markLayoutProps(r.target.hasProps()); err != nil {
		// a Props too many after a switch to a target without them: the
		// scrub clears it
		u.log.Warn("fts layout index: properties not recorded", "err", err)
	}

	req.Progress(progressSwitch, 1, 1)

	return nil
}

// switchOnline is the switch of an online run: the target becomes
// current, the source goes to "old" with the token until O8; the
// document count stays (dropDocID keeps it for both generations). The
// version marker: a copy keeps the rule of switchTo3 (a trusted source
// gives the target's marker, an untrusted one none); a rebuild indexed
// every message of the mailboxes, so it sets the target's marker and
// removes the rebuild key whatever the source had (it heals an offline
// rebuild that stopped midway).
func switchOnline(cur map[string][]byte, c *ftsLayout, src, target ftsLoc, tok, mode string) (map[string][]byte, []string) {
	set, rm := map[string][]byte{}, []string(nil)

	switch {
	case mode == model.FTSModeReindex:
		set[ftsVersionKey3] = []byte(versionFor(target))

		rm = append(rm, ftsRebuildKey)
	case knownVersion(cur[ftsVersionKey3]):
		set[ftsVersionKey3] = []byte(versionFor(target))
	default:
		rm = append(rm, ftsVersionKey3)
	}

	old := src.gen()
	old.Tok = tok

	nl := &ftsLayout{F: model.FTSLayout3, G: target.g, S: target.s, D: target.d, H: target.h, Dict: true,
		LG: c.LG, Old: append(c.Old, old), DX: newDX(target.dx), AH: target.ah}

	set[ftsLayoutKey] = nl.encode()

	return set, rm
}

// finish is O8: the sources go (while "old" names them for this run),
// then the run's "w:" keys and "w#", then the "old" entry. Sources
// another run removed already are fine.
func (r *onlineRun) finish() error {
	u, req := r.u, r.req
	req.Progress(progressCleanup, 0, 1)

	if err := req.Fence(); err != nil {
		return err
	}

	l, err := u.readLayout()
	if err != nil {
		return err
	}

	listed := l != nil && oldOf(l, req.Tok) >= 0

	if listed {
		if err := u.deleteObjects(r.src.objects()); err != nil {
			return err
		}
	}

	if _, err := u.dropBuildKeys(func(t string) bool { return t == req.Tok }); err != nil {
		return err
	}

	if listed {
		if err := u.dropOld(req.Tok, false); err != nil {
			return err
		}
	}

	req.Progress(progressCleanup, 1, 1)

	return u.hook(phaseO8)
}
