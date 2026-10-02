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
//     or carries another generation.
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

// checkMixed looks for a layout 2 object in an account whose layout 2
// objects are gone (one stat of fts/00).
func (sr *scrubRun) checkMixed(l *ftsLayout) {
	if !l.LG {
		return
	}

	oid := layout2Loc(sr.u.ftsShards).postingOIDn(0)

	switch _, err := sr.u.idx.Stat(oid); {
	case err == nil:
		sr.rep.FTSMixedLayout++
		sr.log.Warn("scrub: layout 2 objects in a layout 3 full-text index, repair with fts_rebuild", "oid", oid)
	case errCode(err) != syscall.ENOENT:
		sr.rep.Errors = append(sr.rep.Errors, fmt.Sprintf("fts object %s: %v", oid, err))
	}
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
