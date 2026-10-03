package cephstore

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// The scrub and the online recount (stage B16, temp/16 §4.8): a scrub
// that repairs entries or counters would break the recount's rule that
// no entry without the run's tag comes into being after its barrier. So
// a scrub sets a repair mark "sr:<holder>" in the registry before its
// first repair of that kind, in a CAS that reads the run key (and the
// mailbox locks of stage B17): a live run makes the scrub skip the
// account, and the recount's start CAS is busy while a mark lives. The
// mark is renewed by the scrub itself with the lease of whoever runs it
// (the fence); when a renewal fails the repairs stop. Repairs of
// reference counts, orphan blobs and settled intents need no mark: they
// touch neither entries nor counters.

// scrubMarkTTL is how long a repair mark lives without a renewal (the
// sweep's lease time); a variable for the tests.
var scrubMarkTTL = sweepLockTTL

// scrubMark is the state of a run's repair mark.
type scrubMarkState struct {
	mu      sync.Mutex
	holder  string
	set     bool
	skipped string // why the account is skipped ("" not)
	stopped string // why the repairs stopped ("" not)
	exp     atomic.Int64
	stop    chan struct{}
	done    chan struct{}
}

// mayRepair reports whether the run may repair class: allowed by the
// options, and, for the classes that write entries or counters, under a
// live repair mark.
func (sr *scrubRun) mayRepair(class string) bool {
	if !sr.o.Allowed(class) {
		return false
	}

	switch class {
	case RepairRefs, RepairOrphans, RepairUnknown:
		return true
	}

	return sr.ensureMark()
}

// ensureMark sets the repair mark once; false when the account is to be
// skipped (a recount or a mailbox lock is under way) or the mark was
// lost.
func (sr *scrubRun) ensureMark() bool {
	m := &sr.mark

	m.mu.Lock()
	defer m.mu.Unlock()

	if m.skipped != "" || m.stopped != "" {
		return false
	}

	if m.set {
		return sr.markValidLocked()
	}

	if err := sr.setMark(); err != nil {
		sr.rep.Errors = append(sr.rep.Errors, "repair mark: "+err.Error())
		m.stopped = "mark"

		return false
	}

	if m.skipped != "" {
		mh.scrubQuotaSkipped(m.skipped)
		sr.rep.OnlineSkipped = m.skipped
		sr.log.Info("scrub: repairs skipped", "reason", m.skipped)

		return false
	}

	m.set = true
	m.stop, m.done = make(chan struct{}), make(chan struct{})

	go sr.renewMark()

	if sr.o.afterMark != nil {
		sr.o.afterMark()
	}

	return true
}

// setMark is the CAS that sets the mark (m.mu held).
func (sr *scrubRun) setMark() error {
	m := &sr.mark
	now := time.Now()

	return casOmapPrefix(sr.u, sr.u.idx, oidRegistry, []string{regQuotaRun}, []string{regMailboxLockPrefix, regScrubMarkPrefix},
		func(cur map[string][]byte, pages map[string]prefixPage) (map[string][]byte, []string) {
			m.skipped = ""

			if run, ok := parseQuotaRun(cur[regQuotaRun]); ok && run.alive(now) {
				m.skipped = skipRecount
				return nil, nil
			}

			if _, blocked := mailboxLocksBlock(pages[regMailboxLockPrefix], "", now); blocked {
				m.skipped = skipMailboxLock
				return nil, nil
			}

			// Marks of holders that died are cleared along.
			var rm []string

			for k, v := range pages[regScrubMarkPrefix].vals {
				var old scrubMark
				if k != scrubMarkKey(m.holder) && (json.Unmarshal(v, &old) != nil || !time.Unix(old.Exp, 0).After(now)) {
					rm = append(rm, k)
				}
			}

			exp := now.Add(scrubMarkTTL)
			m.exp.Store(exp.Unix())

			return map[string][]byte{scrubMarkKey(m.holder): encode(scrubMark{Holder: m.holder, Exp: exp.Unix()})}, rm
		})
}

// markValid is the local check before a write inside a group of writes:
// the mark is good for more than two renewal intervals (no read).
func (sr *scrubRun) markValid() bool {
	m := &sr.mark

	m.mu.Lock()
	defer m.mu.Unlock()

	return m.set && sr.markValidLocked()
}

func (sr *scrubRun) markValidLocked() bool {
	m := &sr.mark
	if m.stopped != "" {
		return false
	}

	if time.Until(time.Unix(m.exp.Load(), 0)) > 2*scrubMarkTTL/3 {
		return true
	}

	sr.stopRepairs("mark expired")

	return false
}

// stopRepairs ends the repairs of the run (m.mu held).
func (sr *scrubRun) stopRepairs(reason string) {
	m := &sr.mark
	if m.stopped != "" {
		return
	}

	m.stopped = reason
	sr.rep.RepairsStopped = reason

	mh.scrubQuotaSkipped(skipMarkLost)
	sr.log.Warn("scrub: repairs stopped", "event", "scrub_repairs_stopped", "reason", reason)
}

// renewMark renews the mark every third of its life while the fence
// holds; a failure stops the repairs.
func (sr *scrubRun) renewMark() {
	m := &sr.mark
	defer close(m.done)

	t := time.NewTicker(scrubMarkTTL / 3)
	defer t.Stop()

	for {
		select {
		case <-m.stop:
			return
		case <-t.C:
		}

		if sr.o.Fence != nil {
			if err := sr.o.Fence(); err != nil {
				m.mu.Lock()
				sr.stopRepairs("lease lost: " + err.Error())
				m.mu.Unlock()

				return
			}
		}

		exp := time.Now().Add(scrubMarkTTL)

		lost := false

		err := casOmap(sr.u, sr.u.idx, oidRegistry, []string{scrubMarkKey(m.holder)}, func(cur map[string][]byte) (map[string][]byte, []string) {
			if _, ok := cur[scrubMarkKey(m.holder)]; !ok {
				lost = true
				return nil, nil
			}

			return map[string][]byte{scrubMarkKey(m.holder): encode(scrubMark{Holder: m.holder, Exp: exp.Unix()})}, nil
		})

		if err != nil || lost {
			m.mu.Lock()
			sr.stopRepairs(fmt.Sprintf("mark renewal failed: %v (lost %v)", err, lost))
			m.mu.Unlock()

			return
		}

		m.exp.Store(exp.Unix())
	}
}

// endMark removes the mark at the end of the run.
func (sr *scrubRun) endMark() {
	m := &sr.mark

	m.mu.Lock()
	set := m.set
	m.mu.Unlock()

	if !set {
		return
	}

	close(m.stop)
	<-m.done

	if err := rmKeys(sr.u.idx, oidRegistry, []string{scrubMarkKey(m.holder)}); err != nil {
		sr.log.Warn("scrub: repair mark not removed", "err", err)
	}
}

// Reasons a scrub skips an account or stops (OnlineSkipped,
// imapceph_scrub_quota_skipped_total).
const (
	skipRecount     = "qr"
	skipMailboxLock = "mblock"
	skipMarkLost    = "sr"
)

// heldOnline reports whether an online recount is under way or a
// mailbox_delete locks mailboxes (or the registry could not be read):
// the scrub skips the account then.
func (u *User) heldOnline(rep *ScrubReport, log *slog.Logger) bool {
	reason, err := u.onlineBusy(log)
	if err != nil {
		log.Warn("scrub: registry not readable, skipped", "err", err)
		rep.Errors = append(rep.Errors, "registry: "+err.Error())

		return true
	}

	if reason == "" {
		return false
	}

	mh.scrubQuotaSkipped(reason)
	rep.OnlineSkipped = reason

	return true
}

// onlineBusy reports why a scrub skips the account before it reads
// anything: an online recount is under way or a mailbox_delete locks
// mailboxes ("" neither).
func (u *User) onlineBusy(log *slog.Logger) (string, error) {
	cur, pages, _, err := u.getKeysPrefix(u.idx, oidRegistry, []string{regQuotaRun}, []string{regMailboxLockPrefix})
	if err != nil {
		return "", err
	}

	now := time.Now()

	if run, ok := parseQuotaRun(cur[regQuotaRun]); ok && run.alive(now) {
		log.Info("scrub: online quota recount in progress, skipped", "run", run.Tok)
		return skipRecount, nil
	}

	if k, blocked := mailboxLocksBlock(pages[regMailboxLockPrefix], "", now); blocked {
		log.Info("scrub: mailbox_delete holds mailboxes locked, skipped", "lock", strings.TrimPrefix(k, regMailboxLockPrefix))
		return skipMailboxLock, nil
	}

	return "", nil
}

// scrubHolder is the holder of a run's repair mark.
func scrubHolder(o ScrubOptions) string {
	if o.Holder != "" {
		return o.Holder
	}

	return "scrub-" + newGUID()[:8]
}

// quotaLeftover is step 5b of the repairs: captures of an online recount
// without a live run key (the run died and nothing cleared them). They
// are cleared under the same check as ClearQuotaRecount.
func (sr *scrubRun) quotaLeftover() {
	left, err := sr.u.quotaCaptureLeftover()
	if err != nil {
		sr.rep.Errors = append(sr.rep.Errors, "quota captures: "+err.Error())
		return
	}

	if !left {
		return
	}

	scrubFinding("quota_capture_leftover")

	sr.rep.QuotaCaptureLeftover = true
	sr.log.Warn("captures of an online quota recount without a run")

	if sr.mayRepair(RepairQuota) {
		if err := sr.u.ClearQuotaRecount(""); err != nil {
			sr.rep.Errors = append(sr.rep.Errors, err.Error())
		}
	}
}
