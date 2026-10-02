package cephstore

import (
	"fmt"
	"maps"
	"slices"
	"sync/atomic"
	"syscall"

	"github.com/ceph/go-ceph/rados"

	"shardpost/internal/model"
)

// "shardpost system abort --run <id>" (temp/12 §5.8): takes a migration
// back before the servers use the system pool. The first call records
// the phase it found as abort_from, together with phase "abort", in one
// write; every later call goes by abort_from, so that an abort that
// stopped is continued, not started again:
//
//	(a) abort_from M1 to M3, no run state, or an init ("initializing"):
//	    the source is unchanged; only the fence becomes "reverted" (and
//	    the mark of an init that stopped after writing it is retired)
//	(b) abort_from M4 to M6: the system pool must still have the digest
//	    of M4 (else a server used it: revert instead); the quarantine is
//	    copied back to the original names, the heads get their original
//	    values, surplus objects at the old place are refused (or deleted,
//	    confirmed), the old place is checked against m4_digest; then the
//	    mark is retired (if there is one) and the fence becomes
//	    "reverted". The quarantine stays until cleanup or the next run.
//
// With DeleteCopies (confirmed) the copies in the system pool go as
// well; else the next run deletes them (confirmed).

// AbortSystem runs "shardpost system abort".
func (s *Store) AbortSystem(o SystemOptions) (*SystemReport, error) {
	return s.runTakeBack("abort", o, func(t *sysTool) takeBack { return &abortion{t: t, run: o.Run} })
}

// takeBack is a command that takes a run back (abort, revert).
type takeBack interface {
	// precheck refuses before the lease what the command does not take,
	// or sets the report's Already.
	precheck(f Fence) error
	// do works under the lease.
	do() error
}

// runTakeBack runs the command mk makes.
func (s *Store) runTakeBack(command string, o SystemOptions, mk func(*sysTool) takeBack) (*SystemReport, error) {
	return s.runTool(command, o, func(t *sysTool) error {
		f, err := t.readFence()
		if err != nil {
			return err
		}

		tb := mk(t)

		if err := tb.precheck(f); err != nil || t.rep.Already {
			return err
		}

		return t.finish(tb.do())
	})
}

// abortion is one run of abort.
type abortion struct {
	t        *sysTool
	run      string
	rs       runState
	restored atomic.Int64
	// again: the run is aborted already (fence reverted); the abort
	// checks the old place again, repairs it from the quarantine and
	// deletes the copies with DeleteCopies.
	again bool
}

// abortable reports whether f is a fence abort takes: a migration or an
// init of the run that is not committed.
func (a *abortion) abortable(f Fence) bool {
	return f.Run == a.run && (f.State == FenceInitializing || (f.State == FenceMigrating && f.Values[fkDirection] == directionMig))
}

// precheck refuses what abort does not take before the lease.
func (a *abortion) precheck(f Fence) error {
	t := a.t
	t.rep.Run = a.run

	switch {
	case a.run == "":
		return fmt.Errorf("%w: abort needs --run <id> (the run of the fence)", ErrSystemRefused)
	case !a.confirmed():
		return fmt.Errorf("%w: --delete-copies and --delete-surplus need --confirm %s", ErrSystemRefused, a.run)
	case f.State == FenceReverted && f.Run == a.run:
		a.again = true
		return t.systemPoolOf(f)
	case f.State == FenceDone || f.State == FenceNative:
		return fmt.Errorf("%w: run %s is committed (fence %s): take it back with \"shardpost system revert --run %s --confirm %s\"",
			ErrSystemRefused, f.Run, f.State, f.Run, f.Run)
	case !a.abortable(f):
		return fmt.Errorf("%w: the fence (state %s, run %s) is not a migration or init of run %s", ErrSystemRefused, orNone(f), f.Run, a.run)
	}

	return t.systemPoolOf(f)
}

// confirmed reports whether the destructive options have their run
// confirmed.
func (a *abortion) confirmed() bool {
	return (!a.t.o.DeleteCopies && !a.t.o.DeleteSurplus) || a.t.o.Confirm == a.run
}

func (a *abortion) do() error {
	t := a.t

	if a.again {
		return a.repeat()
	}

	f, err := t.takeLease(func(f Fence) (map[string]string, error) {
		if !a.abortable(f) {
			return nil, fmt.Errorf("%w: the fence changed (state %s, run %s)", ErrSystemRefused, orNone(f), f.Run)
		}

		return maps.Clone(f.Values), nil
	})
	if err != nil {
		return err
	}

	if f.State == FenceInitializing {
		return a.unchanged()
	}

	if a.rs, err = t.readRunState(); err != nil {
		return err
	}

	if !a.rs.Exists || a.rs.Values[rsRun] != a.run {
		return a.unchanged()
	}

	from, err := a.start()
	if err != nil {
		return err
	}

	switch from {
	case PhaseM1, PhaseM3:
		return a.unchanged()
	case PhaseM4, PhaseM5, PhaseM6:
		return a.restore()
	default:
		return fmt.Errorf("%w: run %s cannot be aborted from phase %s", ErrSystemRefused, a.run, from)
	}
}

// repeat is abort on a run it aborted before: an abort from M4 to M6
// checks the old place against m4_digest again and repairs it from the
// quarantine and the saved heads (a late write of a stopped tool, K3);
// then the copies go with DeleteCopies.
func (a *abortion) repeat() error {
	t := a.t

	if _, err := t.takeLease(keepPlan(Fence{State: FenceReverted, Run: a.run})); err != nil {
		return err
	}

	rs, err := t.readRunState()
	if err != nil {
		return err
	}

	a.rs = rs

	if from := rs.Values[rsAbortFrom]; rs.Values[rsRun] == a.run && rs.Values[rsPhase] == PhaseAbort && phaseAtLeast(from, PhaseM4) {
		if err := a.repair(); err != nil {
			return err
		}
	}

	if !t.o.DeleteCopies && t.rep.Restored == 0 && t.rep.Deleted == 0 {
		t.rep.Already = true
	}

	return a.deleteCopies()
}

// repair brings the old place of an aborted run back to m4_digest.
func (a *abortion) repair() error {
	t := a.t

	m4, err := parseSums(a.rs.Values[rsM4Digest])
	if err != nil {
		return err
	}

	stored, err := t.readDigests(runShards(a.rs))
	if err != nil {
		return err
	}

	if err := a.restoreAll(stored); err != nil {
		return err
	}

	if err := a.surplus(stored); err != nil {
		return err
	}

	if t.rep.Restored > 0 {
		t.log.Warn("the old place of an aborted run was repaired", "event", "system_abort_repaired", rsRun, a.run, "restored", t.rep.Restored)
	}

	return t.checkOldPlace(m4)
}

// start records abort_from and phase "abort" in one write, unless an
// earlier abort did; it returns abort_from.
func (a *abortion) start() (string, error) {
	if from := a.rs.Values[rsAbortFrom]; from != "" {
		a.t.log.Info("continuing an abort", rsRun, a.run, rsAbortFrom, from)
		return from, nil
	}

	from := a.rs.Values[rsPhase]

	if err := a.t.writeRunState(map[string]string{rsAbortFrom: from, rsPhase: PhaseAbort}, false); err != nil {
		return "", err
	}

	return from, systemStep("abort:start")
}

// unchanged is case (a), and the end of case (b): the source is as it
// was. A mark an init wrote before it stopped is retired with the fence
// (else it would be a mark without a fence). The copies go after the
// fence is reverted: an abort that stopped while deleting them is taken
// up by running it again with DeleteCopies.
func (a *abortion) unchanged() error {
	if err := a.t.retireMark(a.run); err != nil {
		return err
	}

	if err := a.revertFence(); err != nil {
		return err
	}

	return a.deleteCopies()
}

// deleteCopies deletes the copies in the system pool (DeleteCopies). The
// run state with its digests and the mark stay as the record of the
// run (a repeated abort repairs from them): the next run confirms it and
// deletes them together with the run's quarantine.
func (a *abortion) deleteCopies() error {
	t := a.t

	if !t.o.DeleteCopies {
		return nil
	}

	objs, err := listAllOf(t.conn, t.sysPool, func(ns, oid string) bool {
		return model.SystemNamespace(ns) && !placementObject(ns, oid)
	})
	if err != nil {
		return err
	}

	if err := systemStep("abort:copies"); err != nil {
		return err
	}

	n, err := deleteAllOf(t.conn, t.sysPool, objs, t.guard)
	t.rep.Deleted += n

	if err == nil {
		t.log.Info("copies deleted", rsRun, a.run, rsObjects, n)
	}

	return err
}

// revertFence sets the fence to "reverted" (the lease is given up at the
// end of the command).
func (a *abortion) revertFence() error {
	err := a.t.updateFence(func(vals map[string]string) error {
		if vals[pkRun] != a.run || (vals[pkState] != FenceMigrating && vals[pkState] != FenceInitializing) {
			return fmt.Errorf("%w: the fence changed under the lease (state %s, run %s)", ErrSystemRefused, vals[pkState], vals[pkRun])
		}

		vals[pkState], vals[pkAt] = FenceReverted, nowText()

		return nil
	}, false)
	if err != nil {
		return err
	}

	a.t.rep.Done = true
	a.t.log.Info("migration aborted", "event", "system_abort_done", rsRun, a.run)

	return nil
}

// restore is case (b).
func (a *abortion) restore() error {
	t := a.t

	m4, err := parseSums(a.rs.Values[rsM4Digest])
	if err != nil {
		return err
	}

	if err := a.checkTarget(m4); err != nil {
		return err
	}

	stored, err := t.readDigests(runShards(a.rs))
	if err != nil {
		return err
	}

	if err := a.restoreAll(stored); err != nil {
		return err
	}

	if err := a.surplus(stored); err != nil {
		return err
	}

	if err := t.checkOldPlace(m4); err != nil {
		return err
	}

	if err := t.retireMark(a.run); err != nil {
		return err
	}

	if err := systemStep("abort:retired"); err != nil {
		return err
	}

	return a.unchanged()
}

// checkTarget refuses a system pool that changed since M4 (a server used
// it: only revert takes that back).
func (a *abortion) checkTarget(m4 map[string]string) error {
	t := a.t

	target, err := t.movableIn(t.sysPool)
	if err != nil {
		return err
	}

	d, err := t.digestListing(t.sysPool, target, func(ns string) string { return ns })
	if err != nil {
		return err
	}

	if got := d.sums()[digestTotal]; got != m4[digestTotal] {
		t.rep.Mismatches = append(t.rep.Mismatches, "system pool: digest "+got+", M4 recorded "+m4[digestTotal])

		return fmt.Errorf("%w: the system pool changed since M4 (a server used it?): take the run back with \"shardpost system revert\"",
			ErrSystemMismatch)
	}

	return nil
}

// restoreItem is one object to restore.
type restoreItem struct{ ns, oid, want string }

// restoreAll brings every object of the source back to its original name.
func (a *abortion) restoreAll(stored nsDigests) error {
	var items []restoreItem

	for _, ns := range model.SystemNamespaces() {
		for _, oid := range slices.Sorted(maps.Keys(stored[ns])) {
			items = append(items, restoreItem{ns, oid, stored[ns][oid]})
		}
	}

	m := &migration{t: a.t, run: a.run, rs: a.rs}

	err := eachParallel(a.t, items, func(w *ioCache, it restoreItem) error { return a.restoreOne(w, m, it) })
	a.t.rep.Restored = int(a.restored.Load())

	return err
}

// restoreOne restores one object: kept when the old place has it with
// its digest, else from the saved original values (a head) or the
// quarantine copy.
func (a *abortion) restoreOne(w *ioCache, m *migration, it restoreItem) error {
	t := a.t
	name := objName(it.ns, it.oid)

	oc, err := w.at(slotDst, t.cfg.IndexPool, it.ns)
	if err != nil {
		return err
	}

	if have, err := readImage(oc, it.oid); err == nil && have.digest(it.oid) == it.want {
		return nil
	} else if err != nil && errCode(err) != syscall.ENOENT {
		return fmt.Errorf("read %s: %w", name, err)
	}

	im, err := a.restoreSource(w, m, it)
	if err != nil {
		return err
	}

	if _, err := putImage(oc, it.oid, name, im, it.want, true, t.guard); err != nil {
		return err
	}

	a.restored.Add(1)

	return systemStep("abort:object")
}

// restoreSource is what an object is restored from: the original values
// of a head, else the quarantine copy, either with the digest of M4.
func (a *abortion) restoreSource(w *ioCache, m *migration, it restoreItem) (*objImage, error) {
	name := objName(it.ns, it.oid)

	im, ok, err := m.original(name)
	if err != nil {
		return nil, err
	}

	if !ok {
		if im, err = a.copyOf(w, it); err != nil {
			return nil, err
		}
	}

	if im.digest(it.oid) != it.want {
		return nil, fmt.Errorf("%w: the saved copy of %s differs from its digest", ErrSystemMismatch, name)
	}

	return im, nil
}

// copyOf reads the quarantine copy of an object, else its copy in the
// system pool: checkTarget found that one unchanged since M4, and an
// object that changed at the old place before M5 moved it has no other.
func (a *abortion) copyOf(w *ioCache, it restoreItem) (*objImage, error) {
	name := objName(it.ns, it.oid)

	for _, src := range [][2]string{{a.t.cfg.IndexPool, quarantineNS(it.ns, a.run)}, {a.t.sysPool, it.ns}} {
		c, err := w.at(slotSrc+src[0], src[0], src[1])
		if err != nil {
			return nil, err
		}

		im, err := readImage(c, it.oid)

		switch {
		case err == nil && im.digest(it.oid) == it.want:
			return im, nil
		case err != nil && errCode(err) != syscall.ENOENT:
			return nil, fmt.Errorf("read the copy of %s: %w", name, err)
		}
	}

	return nil, fmt.Errorf("%w: %s has no copy with its digest of M4 (old place, quarantine, system pool)", ErrSystemMismatch, name)
}

// surplus refuses objects at the old place that are not in the source,
// or deletes them (DeleteSurplus, confirmed).
func (a *abortion) surplus(stored nsDigests) error {
	t := a.t

	old, err := t.movableIn(t.cfg.IndexPool)
	if err != nil {
		return err
	}

	var extra []objRef

	for ns, oids := range old {
		for _, oid := range oids {
			if _, ok := stored[ns][oid]; !ok {
				extra = append(extra, objRef{ns, oid})
			}
		}
	}

	return t.handleSurplus(extra)
}

// handleSurplus deletes surplus objects with DeleteSurplus (the command
// checked the confirmation), else refuses naming them.
func (t *sysTool) handleSurplus(extra []objRef) error {
	if len(extra) == 0 {
		return nil
	}

	if !t.o.DeleteSurplus {
		names := refNames(extra)
		t.rep.Surplus = append(t.rep.Surplus, limited(names)...)

		return fmt.Errorf("%w: %d objects at the old place are not part of the run (first %s): find out which process wrote them, then delete them with --delete-surplus --confirm %s",
			ErrSystemRefused, len(extra), names[0], t.rep.Run)
	}

	return t.deleteSurplus(extra)
}

// checkOldPlace compares the old place with m4_digest.
func (t *sysTool) checkOldPlace(m4 map[string]string) error {
	old, err := t.movableIn(t.cfg.IndexPool)
	if err != nil {
		return err
	}

	d, err := t.digestListing(t.cfg.IndexPool, old, func(ns string) string { return ns })
	if err != nil {
		return err
	}

	var x mismatches

	sums := d.sums()
	for _, ns := range model.SystemNamespaces() {
		if sums[ns] != m4[ns] {
			x.add("%s: digest %s at the old place, M4 recorded %s", ns, sums[ns], m4[ns])
		}
	}

	return x.err(t, "the restored old place")
}

// retireMark retires the mark of run in the system pool, if there is
// one.
func (t *sysTool) retireMark(run string) error {
	c, err := t.openCtx(t.sysPool, model.AdminNamespace)
	if err != nil {
		return err
	}
	defer c.Destroy()

	vals, ver, err := getOmapKeysVer(c, MarkOID, markKeys)
	if err != nil {
		return fmt.Errorf("read the mark: %w", err)
	}

	m := markOf(vals, ver)

	switch {
	case !m.Exists || m.State == MarkRetired:
		return nil
	case m.Run != run:
		return fmt.Errorf("%w: the mark of system pool %q belongs to run %s, not %s", ErrSystemRefused, t.sysPool, m.Run, run)
	}

	err = operate(c, MarkOID, t.guard, func(op *rados.WriteOp) {
		op.AssertVersion(ver)
		op.SetOmap(map[string][]byte{pkState: []byte(MarkRetired), "retired_at": []byte(nowText())})
	})
	if err != nil {
		return fmt.Errorf("retire the mark: %w", err)
	}

	t.rep.Mark = mergedSummary(m.Values, map[string]string{pkState: MarkRetired})
	t.log.Info("mark retired", rsRun, run)

	return nil
}
