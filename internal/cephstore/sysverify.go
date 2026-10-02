package cephstore

import (
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"syscall"

	"shardpost/internal/model"
)

// "shardpost system verify" (temp/12 §5.1, §5.9): without SourceOnly it
// compares the source of a migration (the old place, its quarantine,
// the original values of the poisoned heads) with the copy in the system
// pool, object by object; that holds only until the servers use the
// system pool. With SourceOnly it checks the old place after the
// migration: the store's own namespaces of the index pool may hold the
// fence, the poisoned heads (with the digest of M5) and the sweep's
// objects, nothing else; the quarantine must match m4_digest until
// cleanup deleted it.

// VerifySystem runs "shardpost system verify".
func (s *Store) VerifySystem(o SystemOptions) (*SystemReport, error) {
	return s.runTool("verify", o, func(t *sysTool) error {
		f, err := t.readFence()
		if err != nil {
			return err
		}

		if o.SourceOnly {
			return t.verifySource(f)
		}

		return t.verifyCopy(f)
	})
}

// verifyCopy compares the source of the migration of f with its copy.
func (t *sysTool) verifyCopy(f Fence) error {
	if (f.State != FenceMigrating || f.Values[fkDirection] != directionMig) && f.State != FenceDone {
		return fmt.Errorf("%w: no migration to verify (fence %s, run %s)", ErrSystemRefused, orNone(f), f.Run)
	}

	if err := t.systemPoolOf(f); err != nil {
		return err
	}

	rs, err := t.readRunState()
	if err != nil {
		return err
	}

	t.rep.Run = f.Run
	m := &migration{t: t, fence: f, run: f.Run, rs: rs}

	if err := m.list(); err != nil {
		return err
	}

	if err := m.m2(); err != nil {
		return err
	}

	var x mismatches

	if err := m.extraInTarget(&x); err != nil {
		return err
	}

	d := nsDigests{}

	err = eachParallel(t, m.q.items, func(w *ioCache, o *qObj) error { return m.verifyOne(w, o, d, &x) })
	if err != nil {
		return err
	}

	t.rep.Digests = d.sums()

	return x.err(t, "source and copy")
}

// extraInTarget reports objects of the system pool that the source does
// not have.
func (m *migration) extraInTarget(x *mismatches) error {
	target, err := m.t.movableIn(m.t.sysPool)
	if err != nil {
		return err
	}

	for ns, oids := range target {
		for _, oid := range oids {
			if m.q.by[objName(ns, oid)] == nil {
				x.add("%s: in the system pool but not in the source", objName(ns, oid))
			}
		}
	}

	return nil
}

// verifyOne compares one object of the source with its copy.
func (m *migration) verifyOne(w *ioCache, o *qObj, d nsDigests, x *mismatches) error {
	src, _, err := m.source(w, o)
	if err != nil {
		return err
	}

	want := src.digest(o.oid)

	x.mu.Lock()
	d.set(o.ns, o.oid, want)
	x.mu.Unlock()

	dst, err := w.at(slotDst, m.t.sysPool, o.ns)
	if err != nil {
		return err
	}

	im, err := readImage(dst, o.oid)

	switch {
	case errCode(err) == syscall.ENOENT:
		x.add("%s: missing in the system pool", o.name())
	case err != nil:
		return fmt.Errorf("read the copy of %s: %w", o.name(), err)
	case im.digest(o.oid) != want:
		x.add("%s: the copy differs from the source", o.name())
	}

	return nil
}

// orNone is the state of a fence, "none" without one.
func orNone(f Fence) string {
	if !f.Exists {
		return "none"
	}

	return f.State
}

// remnant is what the old place holds after a migration, as verify
// --source-only checks it.
type remnant struct {
	// surplus are the objects that must not be there (in the index
	// pool).
	surplus []objRef
	x       mismatches
}

// verifySource checks the old place (verify --source-only); surplus
// objects are deleted with DeleteSurplus and the confirmed run.
func (t *sysTool) verifySource(f Fence) error {
	if f.State != FenceDone && f.State != FenceNative {
		return fmt.Errorf("%w: verify --source-only needs a completed migration or init (fence %s, run %s)", ErrSystemRefused, orNone(f), f.Run)
	}

	t.rep.Run = f.Run

	if err := t.systemPoolOf(f); err != nil {
		return err
	}

	r, err := t.checkRemnant(f)
	if err != nil {
		return err
	}

	if len(r.surplus) > 0 && t.o.DeleteSurplus {
		if err := t.confirmed(f.Run, "deleting surplus objects"); err != nil {
			return err
		}

		if _, err := t.takeLease(keepPlan(f)); err != nil {
			return t.finish(err)
		}

		if err := t.finish(t.deleteSurplus(r.surplus)); err != nil {
			return err
		}

		r.surplus = nil
	}

	return r.result(t)
}

// result is the outcome of a remnant check: mismatches and surplus
// objects are ErrSystemMismatch.
func (r *remnant) result(t *sysTool) error {
	names := refNames(r.surplus)
	for _, s := range names {
		r.x.add("%s: must not be at the old place", s)
	}

	t.rep.Surplus = append(t.rep.Surplus, limited(names)...)

	return r.x.err(t, "the old place")
}

// limited is names cut to maxListed.
func limited(names []string) []string {
	var out []string
	for _, n := range names {
		out = addListed(out, n)
	}

	return out
}

// checkRemnant reads the old place of the migration or init of f.
func (t *sysTool) checkRemnant(f Fence) (*remnant, error) {
	r := &remnant{}

	heads, err := fenceHeads(f)
	if err != nil {
		return nil, err
	}

	t.rep.Heads = heads

	if r.surplus, err = t.beyondHeads(heads); err != nil {
		return nil, err
	}

	if err := t.checkHeads(f, heads, &r.x); err != nil {
		return nil, err
	}

	if f.State == FenceDone {
		if err := t.checkQuarantine(f, r); err != nil {
			return nil, err
		}
	}

	return r, nil
}

// fenceHeads are the poisoned heads the fence names.
func fenceHeads(f Fence) ([]string, error) {
	var heads []string

	if v := f.Values[fkHeads]; v != "" {
		if err := json.Unmarshal([]byte(v), &heads); err != nil {
			return nil, fmt.Errorf("%w: the fence's list of heads is unreadable", ErrSystemMismatch)
		}
	}

	return heads, nil
}

// beyondHeads lists what the old place holds besides the fence, the
// sweep's objects and the heads.
func (t *sysTool) beyondHeads(heads []string) ([]objRef, error) {
	old, err := t.listPool(t.cfg.IndexPool, func(ns, oid string) bool {
		return model.SystemNamespace(ns) && !isSweep(ns, oid) && (ns != model.AdminNamespace || oid != FenceOID)
	})
	if err != nil {
		return nil, err
	}

	var out []objRef

	for ns, oids := range old {
		for _, oid := range oids {
			if !slices.Contains(heads, objName(ns, oid)) {
				out = append(out, objRef{ns, oid})
			}
		}
	}

	return out, nil
}

// headsDigest is the digest of the heads at the old place (fenced_digest);
// a head that is missing is a mismatch.
func (t *sysTool) headsDigest(heads []string, x *mismatches) (string, error) {
	d := nsDigests{}

	for _, name := range heads {
		ns, oid := splitName(name)

		c, err := t.openCtx(t.cfg.IndexPool, ns)
		if err != nil {
			return "", err
		}

		im, err := readImage(c, oid)

		c.Destroy()

		switch {
		case errCode(err) == syscall.ENOENT:
			x.add("%s: the poisoned head is gone", name)
		case err != nil:
			return "", fmt.Errorf("read %s: %w", name, err)
		default:
			d.set(ns, oid, im.digest(oid))
		}
	}

	return d.sums()[digestTotal], nil
}

// checkHeads compares the poisoned heads with fenced_digest.
func (t *sysTool) checkHeads(f Fence, heads []string, x *mismatches) error {
	if f.State != FenceDone {
		return nil
	}

	got, err := t.headsDigest(heads, x)
	if err != nil {
		return err
	}

	if got != f.Values[fkFenced] {
		x.add("the poisoned heads: digest %s, the fence recorded %s", got, f.Values[fkFenced])
	}

	return nil
}

// checkQuarantine compares the quarantine of the run with m4_digest; after
// cleanup it must be gone (its objects are surplus).
func (t *sysTool) checkQuarantine(f Fence, r *remnant) error {
	quar := map[string]string{}
	for _, ns := range model.SystemNamespaces() {
		quar[quarantineNS(ns, f.Run)] = ns
	}

	l, err := t.listPool(t.cfg.IndexPool, func(ns, _ string) bool { return isQuarantine(ns, f.Run) })
	if err != nil {
		return err
	}

	if f.Values[fkCleanedAt] != "" {
		for qns, oids := range l {
			for _, oid := range oids {
				r.surplus = append(r.surplus, objRef{qns, oid})
			}
		}

		return nil
	}

	got, err := t.digestListing(t.cfg.IndexPool, l, func(qns string) string { return quar[qns] })
	if err != nil {
		return err
	}

	rs, err := t.readRunState()
	if err != nil {
		return err
	}

	if f.Values[fkCleaning] != "" {
		return t.checkPartQuarantine(f, rs, got, r)
	}

	want, err := parseSums(rs.Values[rsM4Digest])
	if err != nil {
		return err
	}

	sums := got.sums()
	for _, ns := range model.SystemNamespaces() {
		if sums[ns] != want[ns] {
			r.x.add("%s: the quarantine has digest %s, M4 recorded %s", quarantineNS(ns, f.Run), sums[ns], want[ns])
		}
	}

	return nil
}

// checkPartQuarantine checks the quarantine of a cleanup that stopped:
// what is left of it must match the digests of the copy, object by
// object.
func (t *sysTool) checkPartQuarantine(f Fence, rs runState, got nsDigests, r *remnant) error {
	stored, err := t.readDigests(runShards(rs))
	if err != nil {
		return err
	}

	for ns, ds := range got {
		for oid, d := range ds {
			if stored[ns][oid] != d {
				r.x.add("%s: differs from the digest of its copy", objName(quarantineNS(ns, f.Run), oid))
			}
		}
	}

	return nil
}

// digestListing hashes every object of l in pool; as maps a listed
// namespace to the namespace the digests are kept under.
func (t *sysTool) digestListing(pool string, l listing, as func(string) string) (nsDigests, error) {
	type item struct{ ns, oid string }

	var items []item

	for _, ns := range slices.Sorted(maps.Keys(l)) {
		for _, oid := range l[ns] {
			items = append(items, item{ns, oid})
		}
	}

	d := nsDigests{}

	var x mismatches

	err := eachParallel(t, items, func(w *ioCache, it item) error {
		c, err := w.at(slotSrc, pool, it.ns)
		if err != nil {
			return err
		}

		im, err := readImage(c, it.oid)
		if errCode(err) == syscall.ENOENT {
			return nil
		}

		if err != nil {
			return fmt.Errorf("read %s: %w", objName(it.ns, it.oid), err)
		}

		x.mu.Lock()
		d.set(as(it.ns), it.oid, im.digest(it.oid))
		x.mu.Unlock()

		return nil
	})

	return d, err
}

// keepPlan is the lease plan of a command that works on the fence as it
// is: same state and run, else refused.
func keepPlan(want Fence) func(Fence) (map[string]string, error) {
	return func(f Fence) (map[string]string, error) {
		if f.State != want.State || f.Run != want.Run {
			return nil, fmt.Errorf("%w: the fence changed (state %s, run %s)", ErrSystemRefused, orNone(f), f.Run)
		}

		return maps.Clone(f.Values), nil
	}
}

// confirmed refuses a destructive step that Confirm does not name the
// run of.
func (t *sysTool) confirmed(run, what string) error {
	if t.o.Confirm != run {
		return fmt.Errorf("%w: %s needs --confirm %s", ErrSystemRefused, what, run)
	}

	return nil
}

// deleteSurplus deletes the surplus objects of the index pool under the
// lease and names them in the report.
func (t *sysTool) deleteSurplus(surplus []objRef) error {
	l := listing{}

	for _, o := range surplus {
		l[o.ns] = append(l[o.ns], o.oid)
	}

	names := refNames(surplus)

	n, err := t.deleteListed(t.cfg.IndexPool, l)
	t.rep.Deleted += n
	t.rep.Surplus = append(t.rep.Surplus, limited(names)...)

	if err != nil {
		return err
	}

	t.log.Warn("surplus objects deleted at the old place", "event", "system_surplus_deleted", rsObjects, n, "first", names[0])

	return systemStep("surplus")
}
