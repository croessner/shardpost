package cephstore

import (
		"fmt"
	"maps"
	"strconv"
	"sync/atomic"

	"shardpost/internal/model"
)

// "shardpost system revert --run <id> --confirm <id>" (temp/12 §5.8):
// takes a committed migration or init back, in a maintenance window. The
// system pool is the truth:
//
//  1. nothing may run (the checks of M0, on the system pool); the fence
//     becomes "migrating" with direction "revert" (servers refuse from
//     here on), the run state phase "revert";
//  2. every object of the store's namespaces in the system pool (but the
//     placement objects) is copied to its name in the index pool; one
//     that differs there is overwritten, the poisoned heads included;
//  3. objects in the index pool that the system pool does not have are
//     refused, or deleted with DeleteSurplus;
//  4. the index pool is checked against a fresh digest of the system
//     pool;
//  5. the quarantine of the run is deleted;
//  6. the mark is retired and the fence becomes "reverted".
//
// A revert that stopped is continued by running it again (a fence
// "migrating" with direction "revert" of the run is taken up). The
// copies stay in the system pool, retired, until a new run deletes them.

// RevertSystem runs "shardpost system revert".
func (s *Store) RevertSystem(o SystemOptions) (*SystemReport, error) {
	return s.runTakeBack("revert", o, func(t *sysTool) takeBack { return &reversal{t: t, run: o.Run} })
}

// reversal is one run of revert.
type reversal struct {
	t      *sysTool
	run    string
	copied atomic.Int64
	kept   atomic.Int64
}

// revertible reports whether f is a fence revert takes.
func (r *reversal) revertible(f Fence) bool {
	return f.Run == r.run && (f.State == FenceDone || f.State == FenceNative ||
		(f.State == FenceMigrating && f.Values[fkDirection] == directionBack))
}

func (r *reversal) precheck(f Fence) error {
	t := r.t
	t.rep.Run = r.run

	switch {
	case r.run == "" || t.o.Confirm != r.run:
		return fmt.Errorf("%w: revert needs --run <id> and --confirm <id> with the run of the fence", ErrSystemRefused)
	case f.State == FenceReverted && f.Run == r.run:
		t.rep.Already = true
		return nil
	case !r.revertible(f):
		return fmt.Errorf("%w: the fence (state %s, run %s) is no committed migration or init of run %s (before the commit: abort)",
			ErrSystemRefused, orNone(f), f.Run, r.run)
	}

	return t.systemPoolOf(f)
}

func (r *reversal) do() error {
	t := r.t

	f, err := t.takeLease(func(f Fence) (map[string]string, error) {
		if !r.revertible(f) {
			return nil, fmt.Errorf("%w: the fence changed (state %s, run %s)", ErrSystemRefused, orNone(f), f.Run)
		}

		return maps.Clone(f.Values), nil
	})
	if err != nil {
		return err
	}

	sys, err := t.movableIn(t.sysPool)
	if err != nil {
		return err
	}

	if err := r.checkIdle(sys); err != nil {
		return err
	}

	if err := r.start(f); err != nil {
		return err
	}

	for _, step := range []func(listing) error{r.copyBack, r.surplus, r.compare} {
		if err := step(sys); err != nil {
			return err
		}
	}

	return r.finishRevert()
}

// checkIdle runs the checks of M0 on the system pool: no registration,
// lease, lock or watcher of a running process.
func (r *reversal) checkIdle(sys listing) error {
	t := r.t

	var locks []string

	for _, ns := range model.SystemNamespaces() {
		for _, oid := range sys[ns] {
			if lockCandidate(ns, oid) {
				locks = append(locks, objName(ns, oid))
			}
		}
	}

	src := func(ns, oid string) (*objImage, error) {
		c, err := t.openCtx(t.sysPool, ns)
		if err != nil {
			return nil, err
		}
		defer c.Destroy()

		return readImage(c, oid)
	}

	fs, err := t.runningChecks(sys[model.AdminNamespace], src, t.sysPool, true)
	if err == nil {
		var ls []Finding

		ls, err = t.lockCheck(t.sysPool, locks)
		fs = append(fs, ls...)
	}

	t.rep.Findings = append(t.rep.Findings, fs...)

	if err != nil {
		return err
	}

	return blocking(fs)
}

// start sets the fence to "migrating" (direction revert) and the run
// state to phase "revert".
func (r *reversal) start(f Fence) error {
	t := r.t

	rs, err := t.readRunState()
	if err != nil {
		return err
	}

	if rs.Exists && rs.Values[rsRun] != r.run {
		return fmt.Errorf("%w: the run state of system pool %q names run %s, not %s", ErrSystemRefused, t.sysPool, rs.Values[rsRun], r.run)
	}

	if f.State != FenceMigrating {
		err := t.updateFence(func(vals map[string]string) error {
			if vals[pkRun] != r.run || (vals[pkState] != FenceDone && vals[pkState] != FenceNative) {
				return fmt.Errorf("%w: the fence changed under the lease (state %s, run %s)", ErrSystemRefused, vals[pkState], vals[pkRun])
			}

			vals[pkState], vals[fkDirection], vals[pkAt] = FenceMigrating, directionBack, nowText()

			return nil
		}, false)
		if err != nil {
			return err
		}

		if err := systemStep("revert:fence"); err != nil {
			return err
		}
	}

	vals := map[string]string{rsPhase: PhaseRevert, rsRevertedAt: nowText()}
	if !rs.Exists {
		maps.Copy(vals, map[string]string{rsRun: r.run, rsToID: strconv.FormatInt(t.sysID, 10), rsBy: t.o.By, rsAt: nowText()})
	}

	if err := t.writeRunState(vals, !rs.Exists); err != nil {
		return err
	}

	t.rep.Phase = PhaseRevert

	return nil
}

// copyBack copies the system pool to the original names in the index
// pool; what differs there is overwritten.
func (r *reversal) copyBack(sys listing) error {
	t := r.t

	type item struct{ ns, oid string }

	var items []item

	for _, ns := range model.SystemNamespaces() {
		for _, oid := range sys[ns] {
			items = append(items, item{ns, oid})
		}
	}

	err := eachParallel(t, items, func(w *ioCache, it item) error {
		src, err := w.at(slotSrc, t.sysPool, it.ns)
		if err != nil {
			return err
		}

		im, err := readImage(src, it.oid)
		if err != nil {
			return fmt.Errorf("read %s in the system pool: %w", objName(it.ns, it.oid), err)
		}

		dst, err := w.at(slotDst, t.cfg.IndexPool, it.ns)
		if err != nil {
			return err
		}

		how, err := putImage(dst, it.oid, objName(it.ns, it.oid), im, im.digest(it.oid), true, t.guard)
		if err != nil {
			return err
		}

		if how == putKept {
			r.kept.Add(1)
		} else {
			r.copied.Add(1)
		}

		return systemStep("revert:object")
	})

	t.rep.Objects, t.rep.Copied, t.rep.Kept = len(items), int(r.copied.Load()), int(r.kept.Load())

	return err
}

// surplus refuses (or deletes) what the index pool has beyond the system
// pool: records of servers before 0.47.0, heads deleted after the
// migration.
func (r *reversal) surplus(sys listing) error {
	old, err := r.t.movableIn(r.t.cfg.IndexPool)
	if err != nil {
		return err
	}

	var extra []string

	in := sys.set()

	for ns, oids := range old {
		for _, oid := range oids {
			if !in[objName(ns, oid)] {
				extra = append(extra, objName(ns, oid))
			}
		}
	}

	return r.t.handleSurplus(extra)
}

// compare checks the index pool against a fresh digest of the system
// pool.
func (r *reversal) compare(sys listing) error {
	t := r.t

	want, err := t.digestListing(t.sysPool, sys, func(ns string) string { return ns })
	if err != nil {
		return err
	}

	old, err := t.movableIn(t.cfg.IndexPool)
	if err != nil {
		return err
	}

	got, err := t.digestListing(t.cfg.IndexPool, old, func(ns string) string { return ns })
	if err != nil {
		return err
	}

	var x mismatches

	for _, ns := range model.SystemNamespaces() {
		for oid, d := range want[ns] {
			if got[ns][oid] != d {
				x.add("%s: differs from the system pool after the copy", objName(ns, oid))
			}
		}
	}

	t.rep.Digests = want.sums()

	if got.sums()[digestTotal] != want.sums()[digestTotal] && x.n == 0 {
		x.add("the index pool has digest %s, the system pool %s", got.sums()[digestTotal], want.sums()[digestTotal])
	}

	return x.err(t, "the index pool after the revert")
}

// finishRevert deletes the quarantine, retires the mark and sets the
// fence to "reverted".
func (r *reversal) finishRevert() error {
	t := r.t

	n, err := t.deleteQuarantine(r.run)
	t.rep.Deleted += n

	if err != nil {
		return err
	}

	if err := t.retireMark(r.run); err != nil {
		return err
	}

	if err := systemStep("revert:retired"); err != nil {
		return err
	}

	err = t.updateFence(func(vals map[string]string) error {
		if vals[pkRun] != r.run || vals[pkState] != FenceMigrating || vals[fkDirection] != directionBack {
			return fmt.Errorf("%w: the fence changed under the lease (state %s, run %s)", ErrSystemRefused, vals[pkState], vals[pkRun])
		}

		vals[pkState], vals[pkAt] = FenceReverted, nowText()

		return nil
	}, true)
	if err != nil {
		return err
	}

	t.rep.Done = true
	t.log.Info("system state reverted to the index pool", "event", "system_revert_done", rsRun, r.run)

	return nil
}
