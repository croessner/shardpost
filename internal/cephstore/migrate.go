package cephstore

import (
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"syscall"
	"time"

	"github.com/ceph/go-ceph/rados"

	"shardpost/internal/model"
)

// Migration of the store's own namespaces into a system pool
// ("shardpost system migrate --to <pool>", temp/12 §5.3). The run state
// "system.migrate" in the system pool records the phase reached; every
// run takes up where the last one stopped:
//
//	M0  preconditions: the target, the fence, nothing running (no
//	    registration, lease, lock or watcher), under the tool lease
//	M1  the fence to "migrating" (servers refuse from here on), the run
//	    state
//	M2  one listing of the index pool: the source (the old place, and
//	    from M5 on its quarantine as well)
//	M3  the copy with a digest per object (only before M4)
//	M4  the check: the target holds exactly the source, digest by digest;
//	    m4_digest in the run state
//	M5  the quarantine: every old object moves to "<ns>.moved/<run>"
//	    of the index pool, every old index head is poisoned (format 1000,
//	    phase "moved:<pool id>") after its original values were saved
//	M6  the mark in the system pool: the commit
//	M7  the fence to "done", the report

// MigrateSystem migrates the store's own namespaces of the index pool
// into the system pool to (see above). With SystemOptions.Check it only
// checks the preconditions.
func (s *Store) MigrateSystem(to string, o SystemOptions) (*SystemReport, error) {
	return s.runTool("migrate", o, func(t *sysTool) error {
		if err := t.setSystemPool(to); err != nil {
			return err
		}

		if o.Check {
			return t.migrateCheck()
		}

		m := &migration{t: t}

		return t.finish(m.do())
	})
}

// migration is one run of the migration.
type migration struct {
	t     *sysTool
	fence Fence
	run   string
	// fresh is set for a new run (the fence is set in M1).
	fresh bool
	// wipe is the confirmed earlier run whose copies and quarantine are
	// deleted after M1.
	wipe string
	rs   runState
	src  listing
	q    *qset
	// shards is the digest object count, stored the digests read.
	shards int
	stored nsDigests
	sums   map[string]string
}

// migratePlan is what the fence becomes with the lease of a migration.
func (t *sysTool) migratePlan(f Fence) (map[string]string, error) {
	switch {
	case !f.Exists || f.State == FenceLeased || f.State == FenceReverted:
		return leasedValues(f, t.o.By, t.lease.now()), nil
	case f.State == FenceMigrating && f.Values[fkDirection] == directionMig && f.ToID == t.sysID:
		return maps.Clone(f.Values), nil
	case f.State == FenceDone && f.ToID == t.sysID:
		return maps.Clone(f.Values), nil
	case f.State == FenceMigrating:
		return nil, fmt.Errorf("%w: a %s of run %s (pool %q) is under way: continue it (\"shardpost system revert --run %s\", \"migrate --to %s\") or abort it",
			ErrSystemRefused, f.Values[fkDirection], f.Run, f.ToName, f.Run, f.ToName)
	default:
		return nil, fmt.Errorf("%w: the fence in the index pool has state %s (run %s, pool %q); a migration starts only without one, or after a revert or abort",
			ErrSystemRefused, f.State, f.Run, f.ToName)
	}
}

func (m *migration) do() error {
	t := m.t

	f, err := t.takeLease(t.migratePlan)
	if err != nil {
		return err
	}

	m.fence = f

	switch f.State {
	case FenceDone:
		return m.completeM7()
	case FenceMigrating:
		m.run = f.Run
	default:
		m.run, m.fresh = newGUID()[:12], true
	}

	t.rep.Run = m.run

	if err := m.check(); err != nil {
		return err
	}

	if err := m.m1(); err != nil {
		return err
	}

	return m.rest()
}

// rest runs M2 to M7.
func (m *migration) rest() error {
	steps := []func() error{m.m2, m.m3, m.m4, m.m5, m.m6, m.m7}

	for _, step := range steps {
		if err := step(); err != nil {
			return err
		}
	}

	return nil
}

// check runs M0 under the lease.
func (m *migration) check() error {
	t := m.t

	if err := m.checkTarget(); err != nil {
		return err
	}

	if err := t.probePool(t.guard); err != nil {
		return err
	}

	if err := m.list(); err != nil {
		return err
	}

	fs, err := m.checkSource(!t.o.Check)
	t.rep.Findings = append(t.rep.Findings, fs...)

	if err != nil {
		return err
	}

	return blocking(fs)
}

// migrateCheck is "migrate --check": M0 without the lease, reporting
// what runs instead of refusing it (the check before the processes are
// stopped).
func (t *sysTool) migrateCheck() error {
	f, err := t.readFence()
	if err != nil {
		return err
	}

	if _, err := t.checkPlanOnly(f); err != nil {
		return err
	}

	m := &migration{t: t, fence: f, run: f.Run, fresh: f.State != FenceMigrating}

	if f.State == FenceDone {
		t.rep.Done = true
		return nil
	}

	if err := m.checkTarget(); err != nil {
		return err
	}

	if err := t.probePool(nil); err != nil {
		return err
	}

	if err := m.list(); err != nil {
		return err
	}

	fs, err := m.checkSource(false)
	t.rep.Findings = append(t.rep.Findings, fs...)

	if err != nil {
		return err
	}

	return blocking(fs)
}

// checkPlanOnly is the decision of migratePlan without taking the
// lease.
func (t *sysTool) checkPlanOnly(f Fence) (map[string]string, error) {
	t.lease = &fenceLease{now: time.Now}
	defer func() { t.lease = nil }()

	return t.migratePlan(f)
}

// checkTarget is M0.1: the system pool holds nothing but what this run
// wrote, or (confirmed) the copies of an earlier run.
func (m *migration) checkTarget() error {
	t := m.t

	rs, err := t.readRunState()
	if err != nil {
		return err
	}

	mark, err := t.readMark()
	if err != nil {
		return err
	}

	m.rs = rs
	phase := rs.Values[rsPhase]

	if rs.Exists && (phase == PhaseAbort || phase == PhaseRevert) && rs.Values[rsRun] == m.run {
		return fmt.Errorf("%w: run %s is being taken back (phase %s): run \"shardpost system %s --run %s\" again",
			ErrSystemRefused, rs.Values[rsRun], phase, phase, rs.Values[rsRun])
	}

	if rs.Exists && rs.Values[rsRun] == m.run {
		if mark.Exists && (mark.Run != m.run || mark.State != MarkActive) {
			return fmt.Errorf("%w: system pool %q carries a mark (state %s, run %s) that is not this run's (%s)",
				ErrSystemRefused, t.sysPool, mark.State, mark.Run, m.run)
		}

		return nil
	}

	return m.checkEarlier(rs, mark)
}

// checkEarlier is M0.1 for a system pool without a run state of this
// run: it must be empty, or hold an earlier run that Confirm names.
func (m *migration) checkEarlier(rs runState, mark Mark) error {
	t := m.t

	var earlier []string

	if mark.Exists && mark.State == MarkRetired {
		earlier = append(earlier, mark.Run)
	}

	if rs.Exists {
		earlier = append(earlier, rs.Values[rsRun])
	}

	switch {
	case mark.Exists && mark.State != MarkRetired:
		return fmt.Errorf("%w: system pool %q carries a mark (state %s, origin %s, run %s): it is in use",
			ErrSystemRefused, t.sysPool, mark.State, mark.Origin, mark.Run)
	case len(earlier) > 0 && t.o.Confirm != "" && slices.Contains(earlier, t.o.Confirm):
		m.wipe = t.o.Confirm
		return nil
	case len(earlier) > 0:
		return fmt.Errorf("%w: system pool %q holds the copies of the earlier run %s; confirm that run (--confirm %s) to delete them and its quarantine",
			ErrSystemRefused, t.sysPool, earlier[0], earlier[0])
	}

	return t.checkEmpty()
}

// checkEmpty refuses a system pool with objects in the store's
// namespaces (a probe left over aside).
func (t *sysTool) checkEmpty() error {
	l, err := t.listPool(t.sysPool, func(ns, oid string) bool {
		return model.SystemNamespace(ns) && (ns != model.AdminNamespace || oid != probeOID)
	})
	if err != nil {
		return err
	}

	for _, ns := range model.SystemNamespaces() {
		if len(l[ns]) > 0 {
			return fmt.Errorf("%w: system pool %q is not empty: %d objects in %s, first %s",
				ErrSystemRefused, t.sysPool, len(l[ns]), ns, l[ns][0])
		}
	}

	return nil
}

// list lists the source: the movable objects of the index pool and the
// quarantine of this run (M2; the checks of M0 read it as well).
func (m *migration) list() error {
	quar := map[string]string{}
	for _, ns := range model.SystemNamespaces() {
		quar[quarantineNS(ns, m.run)] = ns
	}

	l, err := m.t.listPool(m.t.cfg.IndexPool, func(ns, oid string) bool {
		_, q := quar[ns]
		return q || movable(ns, oid)
	})
	if err != nil {
		return err
	}

	m.src = l

	return nil
}

// checkSource runs M0.3 to M0.6 on the source: registrations, leases,
// watchers and locks, refusing only when strict. "--check" runs before
// the processes stop, so the worker locks of the running servers are
// reported there like their leases and registrations, not refused.
func (m *migration) checkSource(strict bool) ([]Finding, error) {
	t := m.t

	var admin, locks []string

	for _, ns := range model.SystemNamespaces() {
		for _, list := range [][]string{m.src[ns], m.src[quarantineNS(ns, m.run)]} {
			for _, oid := range list {
				if ns == model.AdminNamespace {
					admin = append(admin, oid)
				}

				if lockCandidate(ns, oid) {
					locks = append(locks, objName(ns, oid))
				}
			}
		}
	}

	fs, err := t.runningChecks(admin, m.sourceImage, t.cfg.IndexPool, strict)
	if err != nil {
		return fs, err
	}

	ls, err := t.lockCheckSource(locks, m.run)

	for i := range ls {
		ls[i].Blocking = strict
	}

	return append(fs, ls...), err
}

// lockCheckSource checks the locks of the source objects names, at the
// old place or in the quarantine of run.
func (t *sysTool) lockCheckSource(names []string, run string) ([]Finding, error) {
	var out []Finding

	for _, name := range slices.Compact(slices.Sorted(slices.Values(names))) {
		ns, oid := splitName(name)

		fs, err := t.objectLocks(t.cfg.IndexPool, ns, oid)
		if err != nil {
			return out, err
		}

		qs, err := t.objectLocks(t.cfg.IndexPool, quarantineNS(ns, run), oid)
		if err != nil {
			return out, err
		}

		out = append(append(out, fs...), qs...)
	}

	sw, err := t.lockCheck(t.cfg.IndexPool, nil)

	return append(out, sw...), err
}

// sourceImage reads an object of the source: at the old place, else in
// the quarantine.
func (m *migration) sourceImage(ns, oid string) (*objImage, error) {
	for _, n := range []string{ns, quarantineNS(ns, m.run)} {
		c, err := m.t.openCtx(m.t.cfg.IndexPool, n)
		if err != nil {
			return nil, err
		}

		im, err := readImage(c, oid)

		c.Destroy()

		if errCode(err) != syscall.ENOENT {
			return im, err
		}
	}

	return nil, &rados.OperationError{OpError: syscall.ENOENT}
}

// m1 sets the fence to "migrating" for a new run, deletes a confirmed
// earlier run, and creates the run state.
func (m *migration) m1() error {
	t := m.t

	if m.fresh {
		if err := m.startFence(); err != nil {
			return err
		}

		if err := systemStep("fence"); err != nil {
			return err
		}
	}

	if m.wipe != "" {
		if err := m.clearEarlier(); err != nil {
			return err
		}
	}

	if !m.rs.Exists || m.rs.Values[rsRun] != m.run {
		vals := map[string]string{rsRun: m.run, rsPhase: PhaseM1, rsToID: strconv.FormatInt(t.sysID, 10),
			rsBy: t.o.By, rsAt: nowText()}

		if err := t.writeRunState(vals, true); err != nil {
			return err
		}

		m.rs = runState{Exists: true, Values: vals}
	}

	t.rep.Phase = m.rs.Values[rsPhase]

	return systemStep(PhaseM1)
}

// startFence sets the fence of a new run to "migrating".
func (m *migration) startFence() error {
	t := m.t

	return t.updateFence(func(vals map[string]string) error {
		if s := vals[pkState]; s != FenceLeased && s != FenceReverted {
			return fmt.Errorf("%w: the fence changed under the lease (state %s)", ErrSystemRefused, s)
		}

		clear(vals)
		maps.Copy(vals, map[string]string{pkState: FenceMigrating, fkDirection: directionMig,
			pkToID: strconv.FormatInt(t.sysID, 10), pkToName: t.sysPool, pkIndexID: strconv.FormatInt(t.indexID, 10),
			pkFSID: t.fsid, pkRun: m.run, pkAt: nowText(), pkBy: t.o.By})

		return nil
	}, false)
}

// clearEarlier deletes the copies of the confirmed earlier run in the
// system pool and its quarantine in the index pool.
func (m *migration) clearEarlier() error {
	n, err := m.t.clearSystemNamespaces(m.wipe)
	m.t.rep.Deleted += n

	if err != nil {
		return err
	}

	m.rs = runState{}

	return systemStep("wipe")
}

// m6 writes the mark: the commit.
func (m *migration) m6() error {
	t := m.t

	vals, err := m.markValues()
	if err != nil {
		return err
	}

	c, err := t.openCtx(t.sysPool, model.AdminNamespace)
	if err != nil {
		return err
	}
	defer c.Destroy()

	err = operate(c, MarkOID, t.guard, func(op *rados.WriteOp) {
		op.Create(rados.CreateExclusive)
		op.SetOmap(toBytes(vals))
	})

	switch {
	case errCode(err) == syscall.EEXIST:
		mark, err := t.readMark()
		if err != nil {
			return err
		}

		if mark.State != MarkActive || mark.Run != m.run || mark.Origin != OriginMigrated || mark.IndexID != t.indexID || mark.SystemID != t.sysID {
			return fmt.Errorf("%w: system pool %q carries a mark of another run (state %s, run %s)", ErrSystemRefused, t.sysPool, mark.State, mark.Run)
		}
	case err != nil:
		return fmt.Errorf("write the mark: %w", err)
	default:
		t.log.Info("mark written", rsRun, m.run)
	}

	if err := m.setPhase(PhaseM6, nil); err != nil {
		return err
	}

	return systemStep(PhaseM6)
}

// markValues are the values of the mark of this run.
func (m *migration) markValues() (map[string]string, error) {
	t := m.t
	id := func(n int64) string { return strconv.FormatInt(n, 10) }

	return map[string]string{pkFormat: strconv.Itoa(MarkFormat), pkState: MarkActive, pkOrigin: OriginMigrated,
		pkIndexID: id(t.indexID), pkIndexName: t.cfg.IndexPool, pkDataID: id(t.dataID), pkDataName: t.cfg.DataPool,
		pkSystemID: id(t.sysID), pkFSID: t.fsid, pkRun: m.run, pkAt: nowText(), pkObjects: strconv.Itoa(len(m.q.items)),
		pkDigest: m.sums[digestTotal], pkPools: "{}"}, nil
}

// setPhase records phase (unless the run is beyond it) with vals.
func (m *migration) setPhase(phase string, vals map[string]string) error {
	if vals == nil {
		vals = map[string]string{}
	}

	if !phaseAtLeast(m.rs.Values[rsPhase], phase) {
		vals[rsPhase] = phase
	}

	if len(vals) == 0 {
		return nil
	}

	if err := m.t.writeRunState(vals, false); err != nil {
		return err
	}

	maps.Copy(m.rs.Values, vals)
	m.t.rep.Phase = m.rs.Values[rsPhase]

	return nil
}

// m7 sets the fence to "done" and records the report.
func (m *migration) m7() error {
	t := m.t
	heads, fenced := m.rs.Values[rsHeads], m.rs.Values[rsFenced]

	err := t.updateFence(func(vals map[string]string) error {
		if vals[pkState] != FenceMigrating || vals[pkRun] != m.run || vals[fkDirection] != directionMig {
			return fmt.Errorf("%w: the fence changed under the lease (state %s, run %s)", ErrSystemRefused, vals[pkState], vals[pkRun])
		}

		vals[pkState], vals[pkAt], vals[fkFenced], vals[fkHeads] = FenceDone, nowText(), fenced, heads

		return nil
	}, false)
	if err != nil {
		return err
	}

	if err := systemStep("done"); err != nil {
		return err
	}

	return m.record()
}

// record writes the report and phase M7 into the run state.
func (m *migration) record() error {
	t := m.t
	t.rep.Done, t.rep.Phase = true, PhaseM7

	b, err := json.Marshal(t.rep)
	if err != nil {
		return err
	}

	if err := m.setPhase(PhaseM7, map[string]string{rsReport: string(b)}); err != nil {
		return err
	}

	t.log.Info("system state migrated", "event", "system_migrate_done", rsRun, m.run, "system_pool", t.sysPool)

	return systemStep(PhaseM7)
}

// completeM7 finishes a run whose fence is "done" already (R3.9): only
// the run state can be behind.
func (m *migration) completeM7() error {
	t := m.t
	m.run = m.fence.Run
	t.rep.Run = m.run

	rs, err := t.readRunState()
	if err != nil {
		return err
	}

	if !rs.Exists || rs.Values[rsRun] != m.run {
		return fmt.Errorf("%w: the fence is done (run %s) but the run state of system pool %q names run %q",
			ErrSystemRefused, m.run, t.sysPool, rs.Values[rsRun])
	}

	m.rs = rs

	if rs.Values[rsPhase] == PhaseM7 {
		t.rep.Already, t.rep.Done, t.rep.Phase = true, true, PhaseM7
		return nil
	}

	t.rep.FencedDigest = rs.Values[rsFenced]

	return m.record()
}
