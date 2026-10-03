package cephstore

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"syscall"

	"github.com/ceph/go-ceph/rados"

	"shardpost/internal/metrics"
	"shardpost/internal/model"
)

// The scrub's look at the layout of the full-text index (stage B14b,
// §5.4). It reports, without repairing them (the repair is fts_rebuild):
//
//   - fts_mixed_layout: a layout 3 account whose layout 2 objects should
//     be gone has one (fts/00) — a server before 0.45.0 wrote into it,
//     which only a wrong way back makes possible;
//   - fts_shard_missing: an object of the current generation is missing
//     or carries another generation;
//   - fts_version_drift: the version marker does not fit the generation
//     (B14h, temp/13 §5.3), see versionDrift; the run repairs it where
//     the right marker is known, unless it only reports;
//   - fts_build_leftover: records of online runs ("w:" keys, "w#") the
//     layout record no longer names (B14i), see checkBuildLeftover;
//     removed with the repair class uncovered.
//
// It keeps the presence index ix.fts_layout in step with the record: an
// account with layout 3 (or a conversion to it) is entered, one without
// is removed (unless the run only reports).

// scrubFTSLayout runs the checks of the layout.
func (sr *scrubRun) scrubFTSLayout() {
	u := sr.u

	// the record, the version markers and the first page of the records
	// of online runs in one read
	cur, build, more, err := u.readLayoutState()
	if err != nil {
		sr.rep.Errors = append(sr.rep.Errors, fmt.Sprintf("fts layout record: %v", err))
		return
	}

	l, err := parseLayout(cur[ftsLayoutKey])
	if err != nil {
		sr.rep.Errors = append(sr.rep.Errors, fmt.Sprintf("fts layout record: %v", err))
		return
	}

	if ftsLayout3(l) {
		sr.checkGeneration(curLoc(l, u.ftsShards))
		sr.checkMixed(l)
	}

	sr.checkVersion(cur)
	sr.checkBuildLeftover(l, cur, build, more)

	if sr.o.DryRun || (sr.o.Fence != nil && sr.o.Fence() != nil) {
		return
	}

	if err := sr.presence(recordWants3(l), recordProps(l, u.ftsShards)); err != nil {
		sr.rep.Errors = append(sr.rep.Errors, fmt.Sprintf("fts layout index: %v", err))
	}
}

// checkGeneration counts the objects of the generation that are missing
// or carry another generation.
func (sr *scrubRun) checkGeneration(loc ftsLoc) {
	for _, oid := range loc.objects() {
		vals, err := getOmapKeys(sr.u.idx, oid, []string{markGen})
		if err != nil && errCode(err) != syscall.ENOENT {
			sr.rep.Errors = append(sr.rep.Errors, fmt.Sprintf("fts object %s: %v", oid, err))
			continue
		}

		if string(vals[markGen]) != loc.g {
			sr.rep.FTSShardsMissing++

			metrics.FTSShardMissing.Inc()
			sr.log.Warn("scrub: full-text object of the current generation missing, repair with fts_rebuild", "oid", oid)
		}
	}
}

// checkMixed looks for layout 2 objects in an account whose layout 2
// objects are gone: the first posting, header and dictionary object (one
// stat each), which a server before 0.45.0 or a layout 2 writer that
// slept through the conversion would have written.
func (sr *scrubRun) checkMixed(l *ftsLayout) {
	if !l.LG {
		return
	}

	l2 := layout2Loc(sr.u.ftsShards)

	for _, oid := range []string{l2.postingOIDn(0), l2.headerOIDn(0), l2.dictOIDn(0)} {
		switch _, err := sr.u.idx.Stat(oid); {
		case err == nil:
			sr.rep.FTSMixedLayout++
			sr.log.Warn("scrub: layout 2 objects in a layout 3 full-text index, repair with fts_rebuild", "oid", oid)

			return
		case errCode(err) != syscall.ENOENT:
			sr.rep.Errors = append(sr.rep.Errors, fmt.Sprintf("fts object %s: %v", oid, err))
		}
	}
}

// Kinds of version drift.
const (
	driftNone    = iota
	driftFixable // a marker whose correct value is known: set
	driftReport  // "4" in a record without "ah": reported only
)

// versionDrift is the drift of the version marker of the id map cur,
// the key it is in and, for a fixable one, the value it gets:
//
//   - "3" in layout 2, or in a layout 3 generation without properties:
//     a version before 0.50.0 converted a filtered generation and
//     derived the whole dictionary; it gets "2";
//   - "2" in a generation with properties: a version before 0.50.0 gave
//     a filtered or "nosig" generation that it found empty its first
//     ids (markFreshIndex3), so a version before 0.50.0 would trust it
//     and miss identifiers; it gets the marker of the generation
//     (versionFor: its writers wrote a superset since);
//   - "4" in a record without "ah": only reported.
func versionDrift(cur map[string][]byte, n int) (kind int, key, value string) {
	l, err := parseLayout(cur[ftsLayoutKey])
	if err != nil {
		return driftNone, "", ""
	}

	if !ftsLayout3(l) {
		if string(cur[ftsVersionKey]) == ftsVersionDict {
			return driftFixable, ftsVersionKey, ftsVersion
		}

		return driftNone, "", ""
	}

	loc := curLoc(l, n)

	switch v := string(cur[ftsVersionKey3]); {
	case v == ftsVersionDict && !loc.hasProps():
		return driftFixable, ftsVersionKey3, ftsVersion
	case v == ftsVersion && loc.hasProps():
		return driftFixable, ftsVersionKey3, versionFor(loc)
	case v == ftsVersionText && loc.ah == "":
		return driftReport, ftsVersionKey3, ""
	}

	return driftNone, "", ""
}

// versionKeys are the keys of the id map the scrub's look at the
// layout reads, once.
var versionKeys = []string{ftsLayoutKey, ftsVersionKey, ftsVersionKey3}

// readLayoutState reads the keys of versionKeys and "w#" and the first
// page of the "w:" keys of the id map in one read op (an account
// without an online run has an empty range there); more is set when
// the range goes on.
func (u *User) readLayoutState() (map[string][]byte, map[string][]byte, bool, error) {
	op := rados.CreateReadOp()
	defer op.Release()

	kstep := op.GetOmapValuesByKeys(append(slices.Clone(versionKeys), buildCountKey))
	wstep := op.GetOmapValues("", buildKeyPrefix, omapPageSize)

	if err := op.Operate(u.idx, oidFTSMap, rados.OperationNoFlag); err != nil {
		if errCode(err) == syscall.ENOENT {
			return map[string][]byte{}, map[string][]byte{}, false, nil
		}

		return nil, nil, false, err
	}

	cur, err := stepValues(kstep)
	if err != nil {
		return nil, nil, false, err
	}

	build := map[string][]byte{}

	for {
		kv, err := wstep.Next()
		if err != nil {
			return nil, nil, false, err
		}

		if kv == nil {
			break
		}

		build[kv.Key] = append([]byte(nil), kv.Value...)
	}

	return cur, build, wstep.More(), nil
}

// checkBuildLeftover reports the records of online runs that the record
// l no longer names (fts_build_leftover, temp/14 §5.6): "w:" keys and
// "w#" whose token is not the one of l's "next", left by a run that
// crashed or was cleared by a version before 0.51.0. Unless the run only
// reports, or the repair class uncovered is not selected, they go, each
// page in a compare-and-swap with the record that keeps the keys of the
// run "next" names in the state read: a run that starts meanwhile and
// records the same GUID keeps its keys (a blind removal would make it
// miss its switch).
func (sr *scrubRun) checkBuildLeftover(l *ftsLayout, cur, build map[string][]byte, more bool) {
	u := sr.u
	next := nextTok(l)

	n := foreignBuildKeys(build, next)

	if v, ok := cur[buildCountKey]; ok {
		n += foreignBuildKeys(map[string][]byte{buildCountKey: v}, next)
	}

	if more {
		// a long range (a crashed run of a busy account): count all of it
		c, err := u.buildLeftover(next)
		if err != nil {
			sr.rep.Errors = append(sr.rep.Errors, fmt.Sprintf("fts build keys: %v", err))
			return
		}

		n = c
	}

	if n == 0 {
		return
	}

	sr.rep.FTSBuildLeftover += n

	sr.log.Warn("scrub: records of an online full-text run that the layout record no longer names", "keys", n,
		"repair", sr.o.Allowed(RepairUncovered))

	if !sr.o.Allowed(RepairUncovered) {
		return
	}

	if _, err := u.dropBuildKeys(func(string) bool { return true }); err != nil {
		sr.rep.Errors = append(sr.rep.Errors, fmt.Sprintf("fts build keys: %v", err))
	}
}

// checkVersion reports (and unless the run only reports, corrects) a
// version marker of the id map cur that does not fit the generation.
func (sr *scrubRun) checkVersion(cur map[string][]byte) {
	u := sr.u

	kind, key, value := versionDrift(cur, u.ftsShards)
	if kind == driftNone {
		return
	}

	sr.rep.FTSVersionDrift++

	metrics.FTSVersionDrift.Inc()

	if kind == driftReport {
		sr.log.Warn("scrub: full-text version marker of the text preparation nosig without it in the layout record "+
			"(a version before 0.50.0 rewrote it); fts_layout writes the record anew", "key", key)

		return
	}

	sr.log.Warn("scrub: full-text version marker does not fit the generation (a version before 0.50.0 wrote it)",
		"key", key, "marker", string(cur[key]), "fit", value, "repair", !sr.o.DryRun)

	if sr.o.DryRun || (sr.o.Fence != nil && sr.o.Fence() != nil) {
		return
	}

	err := casOmap(u, u.idx, oidFTSMap, versionKeys, func(now map[string][]byte) (map[string][]byte, []string) {
		if k, at, v := versionDrift(now, u.ftsShards); k != driftFixable || at != key || v != value {
			return nil, nil
		}

		return map[string][]byte{key: []byte(value)}, nil
	})
	if err != nil {
		sr.rep.Errors = append(sr.rep.Errors, fmt.Sprintf("fts version marker: %v", err))
		return
	}

	u.ftsForget()
}

// recordWants3 reports whether a record has layout 3 or a conversion to
// it (the account belongs into the presence index).
func recordWants3(l *ftsLayout) bool {
	return ftsLayout3(l) || (l != nil && l.Next != nil && l.Next.F == model.FTSLayout3)
}

// recordProps reports whether the current or next generation of a
// record has properties (the presence entry's Props).
func recordProps(l *ftsLayout, n int) bool {
	return curLoc(l, n).hasProps() || (l != nil && l.Next != nil && genLoc(*l.Next, n).hasProps())
}

// presence enters the account in the presence index or removes it, and
// keeps the entry's Props in step with the generations (props: the
// current or next one has properties).
func (sr *scrubRun) presence(want, props bool) error {
	u := sr.u
	if u.store == nil {
		return nil
	}

	ix, release, err := u.store.FTSLayoutIndex()
	if err != nil {
		return err
	}
	defer release()

	v, err := ix.Get(u.name)
	have := err == nil

	var entry model.FTSLayoutEntry

	_ = json.Unmarshal(v, &entry)

	switch {
	case err != nil && !errors.Is(err, model.ErrNotFound):
		return err
	case want && !have:
		sr.rep.FTSPresenceFixed++

		if err := u.markLayout3(); err != nil {
			return err
		}

		return u.markLayoutProps(props)
	case !want && have && entry.Since.Before(sr.cutoff):
		// An entry younger than the grace may belong to an index being
		// created right now (the entry comes before the objects).
		sr.rep.FTSPresenceFixed++
		return u.unmarkLayout3()
	case want:
		return sr.presenceProps(entry, props)
	}

	return nil
}

// presenceProps keeps the Props of an existing presence entry in step
// with the record (props).
func (sr *scrubRun) presenceProps(entry model.FTSLayoutEntry, props bool) error {
	switch {
	case props && entry.Props.IsZero():
		sr.rep.FTSPresenceFixed++
		return sr.u.markLayoutProps(true)
	case !props && !entry.Props.IsZero() && entry.Props.Before(sr.cutoff):
		// Props younger than the grace may belong to a generation being
		// created right now (Props comes before the objects).
		sr.rep.FTSPresenceFixed++
		return sr.u.markLayoutProps(false)
	}

	return nil
}
