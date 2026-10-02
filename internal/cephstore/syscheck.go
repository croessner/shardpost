package cephstore

import (
	"encoding/json"
	"fmt"
	"strings"
	"syscall"
	"time"

	"shardpost/internal/model"
)

// The checks that nothing runs any more (temp/12 §5.2, points 3 to 6):
// before a migration or a revert, no process may use the store's own
// namespaces. They read the records the processes leave (registrations,
// the push worker's lease, job leases), the cls_locks and the watches
// where they are: at the place the migration reads from.

// Names of the checks in the findings.
const (
	checkRegistrations = "registrations"
	checkLeases        = "leases"
	checkLocks         = "locks"
	checkWatchers      = "watchers"
	checkTarget        = "target"
)

// watchedObjects are the objects every server and worker watches, by
// namespace (temp/12 §5.2.6).
var watchedObjects = [][2]string{
	{model.AdminNamespace, authFlushOID},
	{model.AdminNamespace, "ix.jobs/claimable/00"},
	{model.LearningNamespace, "ix.learning/00"},
	{model.AdminNamespace, "ix.submission/00"},
}

// watchTimeout is how long a notify waits for the watchers.
var watchTimeout = 2 * time.Second

// objectSource reads an object of the source of a check: where name is
// found (the old place or, for a migration that moved it, the
// quarantine).
type objectSource func(ns, oid string) (*objImage, error)

// Record keys the checks read (admin store keys with the "kv." prefix).
const (
	pushLeaseOID = "kv.jmap/push/lease"
	jobPrefix    = "kv.job/"
	serverPrefix = "kv." + model.ServersPrefix
)

// runningChecks runs the checks of registrations, leases and watchers on
// the admin namespace objects names (object names of "~admin"), read by
// src; pool is where the watched objects are. blocking makes their
// findings refuse (else they are only reported).
func (t *sysTool) runningChecks(names []string, src objectSource, pool string, blocking bool) ([]Finding, error) {
	now := time.Now()

	var out []Finding

	for _, oid := range names {
		fs, err := t.recordCheck(oid, src, now)
		if err != nil {
			return out, err
		}

		out = append(out, fs...)
	}

	ws, err := t.watcherCheck(pool)
	if err != nil {
		return out, err
	}

	out = append(out, ws...)

	for i := range out {
		out[i].Blocking = blocking
	}

	return out, nil
}

// recordCheck checks one record of the admin namespace.
func (t *sysTool) recordCheck(oid string, src objectSource, now time.Time) ([]Finding, error) {
	switch {
	case strings.HasPrefix(oid, serverPrefix):
		return t.registrationCheck(oid, src, now)
	case oid == pushLeaseOID:
		return leaseCheck(oid, src, now, func(b []byte) (time.Time, string) {
			var l struct {
				Holder string    `json:"holder"`
				Until  time.Time `json:"until"`
			}

			_ = json.Unmarshal(b, &l)

			return l.Until, "the JMAP push worker " + l.Holder
		})
	case strings.HasPrefix(oid, jobPrefix):
		return leaseCheck(oid, src, now, func(b []byte) (time.Time, string) {
			var j struct {
				State string `json:"state"`
				Lease struct {
					Owner string    `json:"owner"`
					Until time.Time `json:"until"`
				} `json:"lease"`
			}

			_ = json.Unmarshal(b, &j)

			if j.State != "running" && j.State != "cancel_requested" {
				return time.Time{}, ""
			}

			return j.Lease.Until, "job runner " + j.Lease.Owner
		})
	default:
		return nil, nil
	}
}

// readRecord reads a record through src (nil: gone).
func readRecord(oid string, src objectSource) ([]byte, error) {
	im, err := src(model.AdminNamespace, oid)
	if errCode(err) == syscall.ENOENT {
		return nil, nil
	}

	if err != nil {
		return nil, fmt.Errorf("read %s: %w", oid, err)
	}

	return im.Data, nil
}

// registrationCheck reports a registration of a process that runs, and
// one of a version before the system pool.
func (t *sysTool) registrationCheck(oid string, src objectSource, now time.Time) ([]Finding, error) {
	b, err := readRecord(oid, src)
	if err != nil || b == nil {
		return nil, err
	}

	var r model.ServerRecord
	if err := json.Unmarshal(b, &r); err != nil {
		return []Finding{{Check: checkRegistrations, Object: oid, Detail: "unreadable registration: " + err.Error()}}, nil
	}

	if !r.Running(now) {
		return nil, nil
	}

	role := r.Role
	if role == "" {
		role = model.RoleServe
	}

	f := Finding{Check: checkRegistrations, Object: oid,
		Detail: fmt.Sprintf("a %s process (version %s) registered %s ago and has not stopped", role, r.Version, now.Sub(r.Seen).Round(time.Second))}
	if !r.HasCap(model.CapSystemPool) {
		f.Detail += "; it does not know the system pool (a version before 0.47.0)"
	}

	if t.o.ForceRegistrations {
		f.Detail += "; ignored (--force-registrations)"

		t.log.Warn("running registration ignored", "event", "system_force_registration", "registration", oid, "role", role,
			"version", r.Version, "seen", r.Seen)

		t.rep.Findings = append(t.rep.Findings, f)

		return nil, nil
	}

	return []Finding{f}, nil
}

// leaseCheck reports a lease that is still valid; until returns its end
// and holder (zero: no lease to check).
func leaseCheck(oid string, src objectSource, now time.Time, until func([]byte) (time.Time, string)) ([]Finding, error) {
	b, err := readRecord(oid, src)
	if err != nil || b == nil {
		return nil, err
	}

	end, holder := until(b)
	if !end.After(now) {
		return nil, nil
	}

	return []Finding{{Check: checkLeases, Object: oid,
		Detail: fmt.Sprintf("%s holds a lease until %s (%s from now)", holder, end.UTC().Format(time.RFC3339), end.Sub(now).Round(time.Second))}}, nil
}

// watcherCheck notifies the watched objects of pool: any watcher that
// answers or times out is a process that still runs.
func (t *sysTool) watcherCheck(pool string) ([]Finding, error) {
	var out []Finding

	for _, w := range watchedObjects {
		c, err := t.openCtx(pool, w[0])
		if err != nil {
			return out, err
		}

		acks, timeouts, err := c.NotifyWithTimeout(w[1], nil, watchTimeout)

		c.Destroy()

		if errCode(err) == syscall.ENOENT {
			continue
		}

		if err != nil && len(acks) == 0 && len(timeouts) == 0 {
			return out, fmt.Errorf("notify %s/%s: %w", w[0], w[1], err)
		}

		for _, a := range acks {
			out = append(out, Finding{Check: checkWatchers, Object: objName(w[0], w[1]),
				Detail: fmt.Sprintf("the watch of client.%d (cookie %d) answered", a.NotifierID, a.WatcherID)})
		}

		for _, to := range timeouts {
			out = append(out, Finding{Check: checkWatchers, Object: objName(w[0], w[1]),
				Detail: fmt.Sprintf("the watch of client.%d (cookie %d) did not answer within %s", to.NotifierID, to.WatcherID, watchTimeout)})
		}
	}

	return out, nil
}

// lockCandidate reports whether oid of ns is one of the store's lock
// objects (temp/12 §1.2): the queue and backfill locks "ix.<name>/lock"
// and "ix.submission/p<n>/lock", the learning worker's lock.
func lockCandidate(ns, oid string) bool {
	return strings.HasSuffix(oid, "/lock") || (ns == model.LearningNamespace && oid == learningWorkerOID)
}

// lockCheck reports the cls_locks held on the lock objects names of
// pool (namespace/object names), and on the sweep's lock of the index
// pool.
func (t *sysTool) lockCheck(pool string, names []string) ([]Finding, error) {
	names = append(names, objName(model.AccountsNamespace, sweepLockOID))

	var out []Finding

	for _, name := range names {
		ns, oid := splitName(name)

		p := pool
		if isSweep(ns, oid) {
			p = t.cfg.IndexPool
		}

		fs, err := t.objectLocks(p, ns, oid)
		if err != nil {
			return out, err
		}

		out = append(out, fs...)
	}

	return out, nil
}

// objectLocks reports the held locks of one object.
func (t *sysTool) objectLocks(pool, ns, oid string) ([]Finding, error) {
	c, err := t.openCtx(pool, ns)
	if err != nil {
		return nil, err
	}
	defer c.Destroy()

	im := &objImage{}

	err = im.readXattrs(c, oid)
	if errCode(err) == syscall.ENOENT {
		return nil, nil
	}

	if err != nil {
		return nil, fmt.Errorf("xattrs of %s: %w", objName(ns, oid), err)
	}

	return lockers(c, oid, objName(ns, oid), im)
}

// blocking returns ErrSystemRefused naming the blocking findings.
func blocking(fs []Finding) error {
	var b []string

	for _, f := range fs {
		if f.Blocking {
			b = append(b, f.Check+" "+f.Object+": "+f.Detail)
		}
	}

	if len(b) == 0 {
		return nil
	}

	return fmt.Errorf("%w: %d checks failed, first %s (after the processes stopped, leases and registrations run out within three minutes: run it again)",
		ErrSystemRefused, len(b), b[0])
}

// probePool probes the system pool (probeSystemPool) after guard (nil:
// none).
func (t *sysTool) probePool(guard func() error) error {
	if guard == nil {
		guard = func() error { return nil }
	}

	return probeSystemPool(t.conn, t.sysPool, guard, ErrSystemRefused)
}
