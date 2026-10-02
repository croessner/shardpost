package cephstore

import (
	"fmt"
	"time"
)

// "shardpost system cleanup --run <id> --confirm <id>" (temp/12 §5.9)
// deletes the quarantine of a migration once it is MinAge old and the old
// place checks out (verify --source-only); the fence and the poisoned
// heads stay for good. It also takes an aborted run (fence "reverted",
// run state phase "abort") and deletes only its quarantine. It records
// cleaned_at in the fence.

// CleanupSystem runs "shardpost system cleanup".
func (s *Store) CleanupSystem(o SystemOptions) (*SystemReport, error) {
	if o.MinAge <= 0 {
		o.MinAge = DefaultCleanupAge
	}

	return s.runTool("cleanup", o, func(t *sysTool) error {
		f, err := t.readFence()
		if err != nil {
			return err
		}

		t.rep.Run = o.Run

		if err := t.cleanupCheck(f); err != nil {
			return err
		}

		var r *remnant

		if f.State == FenceDone {
			if r, err = t.checkRemnant(f); err != nil {
				return err
			}

			if r.x.n > 0 || (len(r.surplus) > 0 && !o.DeleteSurplus) {
				return r.result(t)
			}
		}

		return t.finish(t.cleanup(f, r))
	})
}

// cleanupCheck refuses what cleanup does not take: another run, an
// unconfirmed one, one too young.
func (t *sysTool) cleanupCheck(f Fence) error {
	run := t.o.Run

	switch {
	case run == "" || t.o.Confirm != run:
		return fmt.Errorf("%w: cleanup needs --run <id> and --confirm <id> with the run of the fence", ErrSystemRefused)
	case f.Run != run || (f.State != FenceDone && f.State != FenceReverted):
		return fmt.Errorf("%w: the fence (state %s, run %s) is no completed or aborted migration of run %s", ErrSystemRefused, orNone(f), f.Run, run)
	}

	if err := t.systemPoolOf(f); err != nil {
		return err
	}

	if err := t.cleanupRun(f); err != nil {
		return err
	}

	at, err := time.Parse(time.RFC3339, f.At)
	if err != nil {
		return fmt.Errorf("%w: the fence has no readable time (%q)", ErrSystemRefused, f.At)
	}

	if age := time.Since(at); age < t.o.MinAge {
		return fmt.Errorf("%w: run %s ended %s ago, cleanup waits %s (--min-age)", ErrSystemRefused, run, age.Round(time.Minute), t.o.MinAge)
	}

	return nil
}

// cleanupRun checks the mark of a completed run and the run state of an
// aborted one.
func (t *sysTool) cleanupRun(f Fence) error {
	if f.State == FenceDone {
		m, err := t.readMark()
		if err != nil {
			return err
		}

		if !m.Exists || m.State != MarkActive || m.Run != f.Run {
			return fmt.Errorf("%w: the mark of system pool %q is not the active one of run %s", ErrSystemRefused, t.sysPool, f.Run)
		}

		return nil
	}

	rs, err := t.readRunState()
	if err != nil {
		return err
	}

	// An aborted init has no run state; a revert deleted its quarantine
	// itself.
	if rs.Exists && rs.Values[rsRun] == f.Run && rs.Values[rsPhase] != PhaseAbort {
		return fmt.Errorf("%w: the fence is reverted, but run %s was not aborted (phase %q): nothing to clean up",
			ErrSystemRefused, f.Run, rs.Values[rsPhase])
	}

	return t.abortedOldPlace(f, rs)
}

// abortedOldPlace checks the old place of a run aborted from M4 on
// against m4_digest: its quarantine is the last copy should the old place
// have changed since (a late write of a stopped tool, K3), so it goes
// only while the old place still has the digest of M4.
func (t *sysTool) abortedOldPlace(f Fence, rs runState) error {
	if rs.Values[rsRun] != f.Run || !phaseAtLeast(rs.Values[rsAbortFrom], PhaseM4) {
		return nil
	}

	m4, err := parseSums(rs.Values[rsM4Digest])
	if err != nil {
		return err
	}

	if err := t.checkOldPlace(m4); err != nil {
		return fmt.Errorf("%w; the quarantine is kept: run \"shardpost system abort --run %s\" again to repair the old place from it",
			err, f.Run)
	}

	return nil
}

// cleanup deletes the surplus (confirmed) and the quarantine under the
// lease and records cleaned_at.
func (t *sysTool) cleanup(f Fence, r *remnant) error {
	if _, err := t.takeLease(keepPlan(f)); err != nil {
		return err
	}

	if r != nil && len(r.surplus) > 0 {
		if err := t.deleteSurplus(r.surplus); err != nil {
			return err
		}
	}

	// From here on the quarantine may be partly gone: a cleanup that
	// stopped is taken up by verify and cleanup (checkQuarantine).
	if err := t.stampFence(f, fkCleaning, false); err != nil {
		return err
	}

	if err := systemStep("cleanup:marked"); err != nil {
		return err
	}

	n, err := t.deleteQuarantine(f.Run)
	t.rep.Deleted += n

	if err != nil {
		return err
	}

	if err := systemStep("cleanup:deleted"); err != nil {
		return err
	}

	if err := t.stampFence(f, fkCleanedAt, true); err != nil {
		return err
	}

	t.rep.Done = true
	t.log.Info("quarantine cleaned up", "event", "system_cleanup_done", rsRun, f.Run, rsObjects, n)

	return nil
}

// stampFence records the time under key in the fence of f (kept when it
// is set), under the lease; release gives the lease up with it.
func (t *sysTool) stampFence(f Fence, key string, release bool) error {
	return t.updateFence(func(vals map[string]string) error {
		if vals[pkRun] != f.Run || vals[pkState] != f.State {
			return fmt.Errorf("%w: the fence changed under the lease (state %s, run %s)", ErrSystemRefused, vals[pkState], vals[pkRun])
		}

		if vals[key] == "" {
			vals[key] = nowText()
		}

		return nil
	}, release)
}
