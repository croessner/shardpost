package cephstore

import (
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"

	"github.com/ceph/go-ceph/rados"

	"shardpost/internal/model"
)

// qObj is one object of the source of a migration.
type qObj struct {
	ns, oid string
	// orig: at the old place; quar: in the quarantine of the run;
	// poisoned: a head at the old place this run poisoned (its original
	// values are in the run state).
	orig, quar, poisoned bool
}

func (o *qObj) name() string { return objName(o.ns, o.oid) }

// qset is the source of a migration (Q).
type qset struct {
	items []*qObj
	by    map[string]*qObj
}

// poisonPhase is the phase a poisoned head carries.
func (t *sysTool) poisonPhase() string { return poisonPhasePfx + strconv.FormatInt(t.sysID, 10) }

// m2 builds the source from the listing: the old place and, once M5
// began, the quarantine; a poisoned head counts with its original
// values.
func (m *migration) m2() error {
	q := &qset{by: map[string]*qObj{}}

	add := func(ns, oid string) *qObj {
		o := q.by[objName(ns, oid)]
		if o == nil {
			o = &qObj{ns: ns, oid: oid}
			q.by[o.name()] = o
			q.items = append(q.items, o)
		}

		return o
	}

	for _, ns := range model.SystemNamespaces() {
		for _, oid := range m.src[ns] {
			add(ns, oid).orig = true
		}

		for _, oid := range m.src[quarantineNS(ns, m.run)] {
			add(ns, oid).quar = true
		}
	}

	slices.SortFunc(q.items, func(a, b *qObj) int { return strings.Compare(a.name(), b.name()) })

	if err := m.markPoisoned(q); err != nil {
		return err
	}

	m.q = q
	m.t.rep.Objects = len(q.items)

	return systemStep("M2")
}

// markPoisoned finds the heads at the old place that this run poisoned.
func (m *migration) markPoisoned(q *qset) error {
	t := m.t

	for _, ns := range model.SystemNamespaces() {
		c, err := t.openCtx(t.cfg.IndexPool, ns)
		if err != nil {
			return err
		}

		for _, oid := range m.src[ns] {
			if !isHead(oid) {
				continue
			}

			vals, _, err := getOmapKeysVer(c, oid, []string{headKeyPhase})
			if err != nil {
				c.Destroy()
				return fmt.Errorf("read %s: %w", objName(ns, oid), err)
			}

			q.by[objName(ns, oid)].poisoned = string(vals[headKeyPhase]) == t.poisonPhase()
		}

		c.Destroy()
	}

	return nil
}

// original is the image of a head as it was before this run poisoned it.
func (m *migration) original(name string) (*objImage, bool, error) {
	v, ok := m.rs.Values[rsHeadPrefix+name]
	if !ok {
		return nil, false, nil
	}

	var im objImage
	if err := json.Unmarshal([]byte(v), &im); err != nil {
		return nil, true, fmt.Errorf("%w: the original values of %s in the run state are unreadable", ErrSystemMismatch, name)
	}

	return &im, true, nil
}

// source reads an object of the source: a poisoned head from its
// original values, else from the old place, else from the quarantine.
func (m *migration) source(w *ioCache, o *qObj) (*objImage, *rados.IOContext, error) {
	if o.poisoned {
		im, ok, err := m.original(o.name())
		if err == nil && !ok {
			err = fmt.Errorf("%w: %s is poisoned but the run state lacks its original values", ErrSystemMismatch, o.name())
		}

		return im, nil, err
	}

	ns := o.ns
	if !o.orig {
		ns = quarantineNS(o.ns, m.run)
	}

	c, err := w.at(slotSrc, m.t.cfg.IndexPool, ns)
	if err != nil {
		return nil, nil, err
	}

	im, err := readImage(c, o.oid)
	if errCode(err) == syscall.ENOENT {
		return nil, nil, fmt.Errorf("%w: %s disappeared from the source", ErrSystemMismatch, o.name())
	}

	if err != nil {
		return nil, nil, fmt.Errorf("read %s: %w", o.name(), err)
	}

	return im, c, nil
}

// waves are the objects in the order of the copy: records, index shards,
// index heads (a head names the shards a reader then looks for).
func (q *qset) waves() [3][]*qObj {
	var w [3][]*qObj

	for _, o := range q.items {
		switch {
		case !strings.HasPrefix(o.oid, "ix."):
			w[0] = append(w[0], o)
		case !isHead(o.oid):
			w[1] = append(w[1], o)
		default:
			w[2] = append(w[2], o)
		}
	}

	return w
}

// copyCounts counts what a copy did.
type copyCounts struct {
	copied, kept, replaced atomic.Int64
	bytes                  atomic.Int64
}

func (c *copyCounts) note(how string, size uint64) {
	switch how {
	case putCopied:
		c.copied.Add(1)
	case putKept:
		c.kept.Add(1)
	case putReplaced:
		c.replaced.Add(1)
	}

	c.bytes.Add(int64(size))
}

// m3 copies the source into the system pool with a digest per object
// (only before M4: from there on the copy is checked, not repeated).
func (m *migration) m3() error {
	t := m.t

	if phaseAtLeast(m.rs.Values[rsPhase], PhaseM4) {
		return nil
	}

	m.shards = runShards(m.rs)
	if m.shards == 0 {
		m.shards = digestShards(len(m.q.items))
		if err := m.setPhase(PhaseM1, map[string]string{rsDigestShards: strconv.Itoa(m.shards)}); err != nil {
			return err
		}
	}

	dw := t.newDigestWriter(m.shards)

	var cnt copyCounts

	for _, wave := range m.q.waves() {
		if err := eachParallel(t, wave, func(w *ioCache, o *qObj) error { return m.copyOne(w, o, dw, &cnt) }); err != nil {
			return err
		}

		if err := dw.flush(); err != nil {
			return err
		}
	}

	t.rep.Copied, t.rep.Kept, t.rep.Replaced = int(cnt.copied.Load()), int(cnt.kept.Load()), int(cnt.replaced.Load())
	t.rep.Bytes = cnt.bytes.Load()

	if err := m.setPhase(PhaseM3, map[string]string{rsObjects: strconv.Itoa(len(m.q.items)), rsBytes: strconv.FormatInt(t.rep.Bytes, 10)}); err != nil {
		return err
	}

	t.log.Info("system state copied", rsRun, m.run, rsObjects, len(m.q.items), "copied", t.rep.Copied, "kept", t.rep.Kept,
		"replaced", t.rep.Replaced)

	return systemStep(PhaseM3)
}

// copyOne copies one object into the system pool and records its
// digest; a held lock refuses (a process still uses it).
func (m *migration) copyOne(w *ioCache, o *qObj, dw *digestWriter, cnt *copyCounts) error {
	im, c, err := m.source(w, o)
	if err != nil {
		return err
	}

	if c != nil {
		fs, err := lockers(c, o.oid, o.name(), im)
		if err != nil {
			return err
		}

		if len(fs) > 0 {
			return fmt.Errorf("%w: %s %s", ErrSystemRefused, fs[0].Object, fs[0].Detail)
		}
	}

	d := im.digest(o.oid)

	dst, err := w.at(slotDst, m.t.sysPool, o.ns)
	if err != nil {
		return err
	}

	how, err := putImage(dst, o.oid, o.name(), im, d, true, m.t.guard)
	if err != nil {
		return err
	}

	cnt.note(how, im.Size)

	if err := dw.put(o.ns, o.oid, d); err != nil {
		return err
	}

	return systemStep("M3:object")
}

// mismatches collects what a check found different.
type mismatches struct {
	mu   sync.Mutex
	list []string
	n    int
}

func (x *mismatches) add(format string, args ...any) {
	x.mu.Lock()
	defer x.mu.Unlock()

	x.n++
	x.list = addListed(x.list, fmt.Sprintf(format, args...))
}

// err reports the mismatches (nil: none) into the report.
func (x *mismatches) err(t *sysTool, what string) error {
	if x.n == 0 {
		return nil
	}

	t.rep.Mismatches = append(t.rep.Mismatches, x.list...)

	return fmt.Errorf("%w: %s: %d differences, first %s", ErrSystemMismatch, what, x.n, x.list[0])
}

// m4 checks the copy: the target holds exactly the source, and both
// match the digests of the copy; it records m4_digest.
func (m *migration) m4() error {
	t := m.t

	m.shards = runShards(m.rs)
	if m.shards == 0 {
		return fmt.Errorf("%w: the run state has no digests (phase %s)", ErrSystemMismatch, m.rs.Values[rsPhase])
	}

	stored, err := t.readDigests(m.shards)
	if err != nil {
		return err
	}

	m.stored = stored

	target, err := t.movableIn(t.sysPool)
	if err != nil {
		return err
	}

	var x mismatches

	m.compareNames(stored, target, &x)

	err = eachParallel(t, m.q.items, func(w *ioCache, o *qObj) error { return m.compareOne(w, o, &x) })
	if err != nil {
		return err
	}

	if err := x.err(t, "the copy in the system pool"); err != nil {
		return err
	}

	return m.recordM4()
}

// compareNames checks that source, digests and target name the same
// objects.
func (m *migration) compareNames(stored nsDigests, target listing, x *mismatches) {
	inTarget := target.set()

	for _, o := range m.q.items {
		if _, ok := stored[o.ns][o.oid]; !ok {
			x.add("%s: no digest of the copy", o.name())
		}

		if !inTarget[o.name()] {
			x.add("%s: missing in the system pool", o.name())
		}
	}

	for ns, ds := range stored {
		for oid := range ds {
			if m.q.by[objName(ns, oid)] == nil {
				x.add("%s: copied but no longer in the source", objName(ns, oid))
			}
		}
	}

	for ns, oids := range target {
		for _, oid := range oids {
			if m.q.by[objName(ns, oid)] == nil {
				x.add("%s: in the system pool but not in the source", objName(ns, oid))
			}
		}
	}
}

// compareOne compares source and target of one object with its digest.
func (m *migration) compareOne(w *ioCache, o *qObj, x *mismatches) error {
	want, ok := m.stored[o.ns][o.oid]
	if !ok {
		return nil
	}

	src, _, err := m.source(w, o)
	if err != nil {
		return err
	}

	if src.digest(o.oid) != want {
		x.add("%s: the source differs from its copy's digest", o.name())
	}

	dst, err := w.at(slotDst, m.t.sysPool, o.ns)
	if err != nil {
		return err
	}

	im, err := readImage(dst, o.oid)

	switch {
	case errCode(err) == syscall.ENOENT:
	case err != nil:
		return fmt.Errorf("read the copy of %s: %w", o.name(), err)
	case im.digest(o.oid) != want:
		x.add("%s: the copy differs from its digest", o.name())
	}

	return nil
}

// recordM4 records m4_digest; a run that recorded it before must arrive
// at the same digest.
func (m *migration) recordM4() error {
	t := m.t
	m.sums = m.stored.sums()
	t.rep.Digests = m.sums

	if old := m.rs.Values[rsM4Digest]; old != "" {
		prev, err := parseSums(old)
		if err != nil {
			return err
		}

		if prev[digestTotal] != m.sums[digestTotal] {
			t.rep.Mismatches = append(t.rep.Mismatches, "m4_digest: "+prev[digestTotal]+" recorded, "+m.sums[digestTotal]+" now")
			return fmt.Errorf("%w: the source no longer has the digest recorded in M4", ErrSystemMismatch)
		}

		return systemStep(PhaseM4)
	}

	if err := m.setPhase(PhaseM4, map[string]string{rsM4Digest: sumsText(m.sums)}); err != nil {
		return err
	}

	t.log.Info("copy checked", rsRun, m.run, "digest", m.sums[digestTotal])

	return systemStep(PhaseM4)
}

// m5 moves the old objects into the quarantine and poisons the old
// heads (temp/12 §5.3 M5); fenced_digest is the digest of the poisoned
// heads.
func (m *migration) m5() error {
	t := m.t

	if phaseAtLeast(m.rs.Values[rsPhase], PhaseM5) && m.rs.Values[rsFenced] != "" {
		t.rep.FencedDigest = m.rs.Values[rsFenced]
		return nil
	}

	if err := m.setPhase(PhaseM5, nil); err != nil {
		return err
	}

	if err := systemStep("M5:start"); err != nil {
		return err
	}

	waves := m.q.waves()

	var moved atomic.Int64

	err := eachParallel(t, append(waves[0], waves[1]...), func(w *ioCache, o *qObj) error { return m.quarantineOne(w, o, &moved) })
	if err != nil {
		return err
	}

	t.rep.Quarantined = int(moved.Load())

	w := t.newIOCache()
	defer w.close()

	for _, o := range waves[2] {
		if err := m.poisonOne(w, o); err != nil {
			return err
		}
	}

	return m.fenceHeads(waves[2])
}

// quarantineOne moves one object (not a head) into the quarantine: the
// copy is checked against the digest before the original goes.
func (m *migration) quarantineOne(w *ioCache, o *qObj, moved *atomic.Int64) error {
	t := m.t
	want := m.stored[o.ns][o.oid]

	qc, err := w.at(slotDst, t.cfg.IndexPool, quarantineNS(o.ns, m.run))
	if err != nil {
		return err
	}

	if o.orig {
		oc, err := w.at(slotSrc, t.cfg.IndexPool, o.ns)
		if err != nil {
			return err
		}

		im, err := readImage(oc, o.oid)

		switch {
		case errCode(err) == syscall.ENOENT:
		case err != nil:
			return fmt.Errorf("read %s: %w", o.name(), err)
		default:
			return m.moveOne(oc, qc, o, im, want, moved)
		}
	}

	return checkQuarantined(qc, o, want, "in the quarantine")
}

// moveOne copies im into the quarantine, checks the copy and deletes the
// original.
func (m *migration) moveOne(oc, qc *rados.IOContext, o *qObj, im *objImage, want string, moved *atomic.Int64) error {
	t := m.t

	if im.digest(o.oid) != want {
		return fmt.Errorf("%w: %s changed since it was checked (M4)", ErrSystemMismatch, o.name())
	}

	if _, err := putImage(qc, o.oid, o.name(), im, want, true, t.guard); err != nil {
		return err
	}

	if err := checkQuarantined(qc, o, want, "in the quarantine"); err != nil {
		return err
	}

	if err := t.deleteObject(oc, o.oid); err != nil {
		return err
	}

	moved.Add(1)

	return systemStep("M5:object")
}

// checkQuarantined checks that the copy of o on c has the digest want.
func checkQuarantined(c *rados.IOContext, o *qObj, want, where string) error {
	im, err := readImage(c, o.oid)
	if errCode(err) == syscall.ENOENT {
		return fmt.Errorf("%w: %s is neither at the old place nor %s", ErrSystemMismatch, o.name(), where)
	}

	if err != nil {
		return fmt.Errorf("read %s %s: %w", o.name(), where, err)
	}

	if im.digest(o.oid) != want {
		return fmt.Errorf("%w: the copy of %s %s differs from its digest", ErrSystemMismatch, o.name(), where)
	}

	return nil
}

// poisonOne copies a head into the quarantine, saves its original values
// in the run state and poisons it, in that order (temp/12 §5.3 M5 (b)).
func (m *migration) poisonOne(w *ioCache, o *qObj) error {
	t := m.t
	want := m.stored[o.ns][o.oid]

	qc, err := w.at(slotDst, t.cfg.IndexPool, quarantineNS(o.ns, m.run))
	if err != nil {
		return err
	}

	if o.poisoned {
		return checkQuarantined(qc, o, want, "in the quarantine")
	}

	oc, err := w.at(slotSrc, t.cfg.IndexPool, o.ns)
	if err != nil {
		return err
	}

	// The version before the read: the head is poisoned only if nothing
	// wrote it since the content was checked and saved.
	_, ver, err := getOmapKeysVer(oc, o.oid, []string{pkFormat})
	if err != nil {
		return fmt.Errorf("read %s: %w", o.name(), err)
	}

	src, err := readImage(oc, o.oid)
	if errCode(err) == syscall.ENOENT || (err == nil && ver == 0) {
		return fmt.Errorf("%w: the head %s is gone from the old place", ErrSystemMismatch, o.name())
	}

	if err != nil {
		return fmt.Errorf("read %s: %w", o.name(), err)
	}

	if err := m.saveHead(qc, o, src, want); err != nil {
		return err
	}

	if err := checkQuarantined(qc, o, want, "in the quarantine"); err != nil {
		return err
	}

	if err := m.poison(oc, o, ver); err != nil {
		return err
	}

	t.rep.Poisoned++

	return systemStep("M5:head")
}

// saveHead copies a head into the quarantine and saves its original
// values, unless that was done (then the head must still be the one
// saved, R3.10).
func (m *migration) saveHead(qc *rados.IOContext, o *qObj, src *objImage, want string) error {
	saved, ok, err := m.original(o.name())
	if err != nil {
		return err
	}

	if ok {
		if src.digest(o.oid) != saved.digest(o.oid) {
			return fmt.Errorf("%w: the head %s differs from the original values saved in the run state", ErrSystemMismatch, o.name())
		}

		return nil
	}

	if src.digest(o.oid) != want {
		return fmt.Errorf("%w: %s changed since it was checked (M4)", ErrSystemMismatch, o.name())
	}

	// A head's quarantine copy is never overwritten.
	if _, err := putImage(qc, o.oid, o.name(), src, want, false, m.t.guard); err != nil {
		return err
	}

	if err := systemStep("M5:head-copied"); err != nil {
		return err
	}

	b, err := json.Marshal(src)
	if err != nil {
		return err
	}

	if err := m.t.writeRunState(map[string]string{rsHeadPrefix + o.name(): string(b)}, false); err != nil {
		return err
	}

	m.rs.Values[rsHeadPrefix+o.name()] = string(b)

	return systemStep("M5:head-saved")
}

// poison sets format 1000 and phase "moved:<pool id>" on the head at the
// old place, at version ver (read before its content was checked).
func (m *migration) poison(oc *rados.IOContext, o *qObj, ver uint64) error {
	err := operate(oc, o.oid, m.t.guard, func(op *rados.WriteOp) {
		op.AssertVersion(ver)
		op.SetOmap(map[string][]byte{pkFormat: []byte(strconv.Itoa(poisonFormat)), headKeyPhase: []byte(m.t.poisonPhase())})
	})

	switch errCode(err) {
	case 0:
		if err != nil {
			return err
		}

		o.poisoned = true

		return nil
	case syscall.ERANGE, syscall.EOVERFLOW:
		return fmt.Errorf("%w: %s was written while it was poisoned", ErrSystemMismatch, o.name())
	default:
		return fmt.Errorf("poison %s: %w", o.name(), err)
	}
}

// fenceHeads computes the digest of the poisoned heads and records it
// with their names: M5 is complete.
func (m *migration) fenceHeads(heads []*qObj) error {
	t := m.t
	d := nsDigests{}
	names := []string{}

	for _, o := range heads {
		c, err := t.openCtx(t.cfg.IndexPool, o.ns)
		if err != nil {
			return err
		}

		im, err := readImage(c, o.oid)

		c.Destroy()

		if err != nil {
			return fmt.Errorf("read the poisoned head %s: %w", o.name(), err)
		}

		d.set(o.ns, o.oid, im.digest(o.oid))
		names = append(names, o.name())
	}

	b, err := json.Marshal(names)
	if err != nil {
		return err
	}

	fenced := d.sums()[digestTotal]
	t.rep.FencedDigest, t.rep.Heads = fenced, names

	if err := m.setPhase(PhaseM5, map[string]string{rsFenced: fenced, rsHeads: string(b)}); err != nil {
		return err
	}

	t.log.Info("old place quarantined and fenced", rsRun, m.run, "quarantined", t.rep.Quarantined, "heads", len(names))

	return systemStep(PhaseM5)
}
