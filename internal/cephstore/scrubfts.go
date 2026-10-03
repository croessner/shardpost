package cephstore

import (
	"encoding/json"
	"errors"
	"fmt"
	"syscall"

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
//     (B14h, temp/13 §5.3). A version before 0.50.0 that converted a
//     generation with the identifier filter derived the whole
//     dictionary but kept the marker "3": back to layout 2 it copied it
//     to "v", in a reshard it left it in "V" and dropped the filter from
//     the record. Both get "2" (the run repairs them unless it only
//     reports). A marker "4" whose record lost "ah" the same way is only
//     reported: the readers take "nosig" from the marker, and the next
//     fts_layout writes the record anew.
//
// It keeps the presence index ix.fts_layout in step with the record: an
// account with layout 3 (or a conversion to it) is entered, one without
// is removed (unless the run only reports).

// scrubFTSLayout runs the checks of the layout.
func (sr *scrubRun) scrubFTSLayout() {
	u := sr.u

	l, err := u.readLayout()
	if err != nil {
		sr.rep.Errors = append(sr.rep.Errors, fmt.Sprintf("fts layout record: %v", err))
		return
	}

	if ftsLayout3(l) {
		sr.checkGeneration(curLoc(l, u.ftsShards))
		sr.checkMixed(l)
	}

	sr.checkVersion()

	if sr.o.DryRun || (sr.o.Fence != nil && sr.o.Fence() != nil) {
		return
	}

	want := ftsLayout3(l) || (l != nil && l.Next != nil && l.Next.F == model.FTSLayout3)

	if err := sr.presence(want); err != nil {
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
	driftFixable // "3" where the dictionary is whole: becomes "2"
	driftReport  // "4" in a record without "ah": reported only
)

// versionDrift is the drift of the version marker of the id map cur and
// the key it is in.
func versionDrift(cur map[string][]byte, n int) (int, string) {
	l, err := parseLayout(cur[ftsLayoutKey])
	if err != nil {
		return driftNone, ""
	}

	if !ftsLayout3(l) {
		if string(cur[ftsVersionKey]) == ftsVersionDict {
			return driftFixable, ftsVersionKey
		}

		return driftNone, ""
	}

	loc := curLoc(l, n)

	switch v := string(cur[ftsVersionKey3]); {
	case v == ftsVersionDict && loc.dx == 0 && loc.ah == "":
		return driftFixable, ftsVersionKey3
	case v == ftsVersionText && loc.ah == "":
		return driftReport, ftsVersionKey3
	}

	return driftNone, ""
}

// checkVersion reports (and unless the run only reports, corrects) a
// version marker that does not fit the generation.
func (sr *scrubRun) checkVersion() {
	u := sr.u
	keys := []string{ftsLayoutKey, ftsVersionKey, ftsVersionKey3}

	cur, err := getOmapKeys(u.idx, oidFTSMap, keys)
	if err != nil {
		if errCode(err) != syscall.ENOENT {
			sr.rep.Errors = append(sr.rep.Errors, fmt.Sprintf("fts version marker: %v", err))
		}

		return
	}

	kind, key := versionDrift(cur, u.ftsShards)
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

	sr.log.Warn("scrub: full-text version marker 3 of a dictionary without the identifier filter "+
		"(a version before 0.50.0 converted it)", "key", key, "repair", !sr.o.DryRun)

	if sr.o.DryRun || (sr.o.Fence != nil && sr.o.Fence() != nil) {
		return
	}

	err = casOmap(u, u.idx, oidFTSMap, keys, func(cur map[string][]byte) (map[string][]byte, []string) {
		if k, at := versionDrift(cur, u.ftsShards); k != driftFixable || at != key {
			return nil, nil
		}

		return map[string][]byte{key: []byte(ftsVersion)}, nil
	})
	if err != nil {
		sr.rep.Errors = append(sr.rep.Errors, fmt.Sprintf("fts version marker: %v", err))
		return
	}

	u.ftsForget()
}

// presence enters the account in the presence index or removes it.
func (sr *scrubRun) presence(want bool) error {
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
		return u.markLayout3()
	case !want && have && entry.Since.Before(sr.cutoff):
		// An entry younger than the grace may belong to an index being
		// created right now (the entry comes before the objects).
		sr.rep.FTSPresenceFixed++
		return u.unmarkLayout3()
	}

	return nil
}
