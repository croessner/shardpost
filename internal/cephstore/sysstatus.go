package cephstore

import (
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"
	"syscall"
	"time"

	"shardpost/internal/model"
)

// "shardpost system status [--deep]" reads the placement: the fence, the
// mark and the run state, and the registrations where the admin store
// lives (the system pool once the fence says so). With Deep it counts
// the objects of the store's namespaces in both pools and of every
// quarantine. It only reads.

// SystemStatus runs "shardpost system status".
func (s *Store) SystemStatus(o SystemOptions) (*SystemReport, error) {
	return s.runTool("status", o, func(t *sysTool) error {
		f, err := t.readFence()
		if err != nil {
			return err
		}

		t.rep.Run = f.Run

		if err := t.statusSystemPool(f); err != nil {
			return err
		}

		admin := t.cfg.IndexPool
		if t.sysPool != "" && (f.State == FenceDone || f.State == FenceNative) {
			admin = t.sysPool
		}

		if err := t.statusRegistrations(admin); err != nil {
			return err
		}

		if o.Deep {
			return t.statusDeep()
		}

		return nil
	})
}

// statusSystemPool finds the system pool (the fence's, else the
// configured one) and reads its mark and run state.
func (t *sysTool) statusSystemPool(f Fence) error {
	var err error

	switch {
	case f.Exists && f.State != FenceLeased && f.ToID >= 0:
		err = t.systemPoolOf(f)
	case t.cfg.SystemPool != "":
		err = t.setSystemPool(t.cfg.SystemPool)
	default:
		return nil
	}

	if err != nil {
		t.rep.Findings = append(t.rep.Findings, Finding{Check: checkTarget, Detail: err.Error()})
		return nil
	}

	if _, err := t.readMark(); err != nil {
		return err
	}

	rs, err := t.readRunState()
	if err == nil {
		t.rep.Phase = rs.Values[rsPhase]
	}

	return err
}

// statusRegistrations lists the registrations of pool.
func (t *sysTool) statusRegistrations(pool string) error {
	l, err := t.listPool(pool, func(ns, oid string) bool {
		return ns == model.AdminNamespace && strings.HasPrefix(oid, serverPrefix)
	})
	if err != nil {
		return err
	}

	c, err := t.openCtx(pool, model.AdminNamespace)
	if err != nil {
		return err
	}
	defer c.Destroy()

	now := time.Now()

	for _, oid := range l[model.AdminNamespace] {
		im, err := readImage(c, oid)
		if errCode(err) == syscall.ENOENT {
			continue
		}

		if err != nil {
			return fmt.Errorf("read %s: %w", oid, err)
		}

		reg := Registration{Instance: strings.TrimPrefix(oid, serverPrefix)}
		if json.Unmarshal(im.Data, &reg.ServerRecord) != nil {
			continue
		}

		reg.Running = reg.ServerRecord.Running(now)
		t.rep.Registrations = append(t.rep.Registrations, reg)
	}

	return nil
}

// statusDeep counts the objects of the store's namespaces and of the
// quarantines in the index pool, and of the store's namespaces in the
// system pool.
func (t *sysTool) statusDeep() error {
	pools := []string{t.cfg.IndexPool}
	if t.sysPool != "" {
		pools = append(pools, t.sysPool)
	}

	for _, pool := range pools {
		l, err := t.listPool(pool, func(ns, _ string) bool {
			return model.SystemNamespace(ns) || strings.Contains(ns, ".moved/")
		})
		if err != nil {
			return err
		}

		for _, ns := range slices.Sorted(maps.Keys(l)) {
			t.rep.Namespaces = append(t.rep.Namespaces, NamespaceCount{Pool: pool, Namespace: ns, Objects: len(l[ns])})
		}
	}

	return nil
}
