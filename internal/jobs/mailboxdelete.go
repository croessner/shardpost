package jobs

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"shardpost/internal/metrics"
	"shardpost/internal/model"
)

// mailbox_delete deletes a mailbox with its children from a plan.
// Offline it holds the account's maintenance lock; with the mailbox lock
// (stage B17, temp/16 §9) it locks only the targets: M1 lock, M2 settle,
// M3 barrier, M4 drift check, M5 unregister, M6 unlock (phase
// destroying), M7 destroy. Users notice nothing but the targets: a
// write into one is refused, a session that has one selected is closed.

type mailboxDeleteParams struct {
	MailboxID string `json:"mailbox_id"`
	Online    *bool  `json:"online,omitempty"`
}

func parseMailboxDelete(params json.RawMessage) (mailboxDeleteParams, error) {
	var p mailboxDeleteParams

	if len(bytes.TrimSpace(params)) > 0 {
		if err := json.Unmarshal(params, &p); err != nil {
			return p, fmt.Errorf("%w: params: %v", ErrInvalidParams, err)
		}
	}

	if p.MailboxID == "" {
		return p, fmt.Errorf("%w: mailbox_id required", ErrInvalidParams)
	}

	return p, nil
}

type deleteTarget struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Messages int64  `json:"messages"`
	Bytes    int64  `json:"bytes"`
}

type deleteSummary struct {
	Mailboxes []deleteTarget `json:"mailboxes"`
	Messages  int64          `json:"messages"`
	Bytes     int64          `json:"bytes"`
	Online    bool           `json:"online,omitempty"`
}

// mailboxDelete is the kind.
type mailboxDelete struct{ kindBase }

// ExclusiveFor implements exclusiveFor: with the mailbox lock no account
// lock is taken.
func (k *mailboxDelete) ExclusiveFor(params json.RawMessage) bool {
	p, err := parseMailboxDelete(params)

	return err != nil || !isOnline(p.Online)
}

// normalizeParams implements paramsNormalizer: "online" (the mailbox
// lock) is decided when the plan is made.
func (k *mailboxDelete) normalizeParams(m *Manager, _ model.User, params json.RawMessage) (json.RawMessage, error) {
	return decideOnlineParams(m, params, parseMailboxDelete, func(p *mailboxDeleteParams) **bool { return &p.Online },
		m.mailboxLockMark(), m.cfg.MailboxDeleteOnlineAuto, model.ErrMailboxLockMark)
}

// deleteTargets returns the mailbox and its children (deepest first) and
// the fingerprint of their state.
func deleteTargets(u model.User, params json.RawMessage) (deleteSummary, string, error) {
	p, err := parseMailboxDelete(params)
	if err != nil {
		return deleteSummary{}, "", err
	}

	boxes, err := u.ListMailboxes()
	if err != nil {
		return deleteSummary{}, "", err
	}

	i := slices.IndexFunc(boxes, func(mb model.Mailbox) bool { return mb.ID == p.MailboxID })
	if i < 0 {
		return deleteSummary{}, "", model.ErrNoMailbox
	}

	root := boxes[i].Name
	if strings.EqualFold(root, "INBOX") {
		return deleteSummary{}, "", fmt.Errorf("%w: INBOX cannot be deleted (purge the account instead)", ErrInvalidParams)
	}

	s := deleteSummary{Online: isOnline(p.Online)}

	for _, mb := range boxes {
		if mb.Name == root || strings.HasPrefix(mb.Name, root+"/") {
			t, err := targetOf(u, mb)
			if err != nil {
				return deleteSummary{}, "", err
			}

			s.Mailboxes = append(s.Mailboxes, t)
			s.Messages += t.Messages
			s.Bytes += t.Bytes
		}
	}

	sort.Slice(s.Mailboxes, func(a, b int) bool { return len(s.Mailboxes[a].Name) > len(s.Mailboxes[b].Name) })

	fp, err := fingerprint(u, s.Mailboxes, s.Online)

	return s, fp, err
}

func targetOf(u model.User, mb model.Mailbox) (deleteTarget, error) {
	msgs, err := u.ListMessages(mb.ID)
	if err != nil {
		return deleteTarget{}, err
	}

	t := deleteTarget{ID: mb.ID, Name: mb.Name, Messages: int64(len(msgs))}
	for i := range msgs {
		t.Bytes += msgs[i].Size
	}

	return t, nil
}

// fingerprint hashes the identity and change state of the mailboxes.
// Online it leaves HIGHESTMODSEQ out: flag and keyword changes between
// the plan and the lock (which no lock prevents) cost no mail when the
// mailbox is deleted; what matters are arrivals (UIDNEXT), identity,
// name and the set of children, which the sorted lines carry.
func fingerprint(u model.User, targets []deleteTarget, online bool) (string, error) {
	lines := make([]string, 0, len(targets))

	for _, t := range targets {
		info, err := u.Info(t.Name)
		if err != nil {
			return "", err
		}

		if online {
			lines = append(lines, fmt.Sprintf("%s %s %d %d", info.ID, info.Name, info.UIDValidity, info.UIDNext))
		} else {
			lines = append(lines, fmt.Sprintf("%s %s %d %d %d", info.ID, info.Name, info.UIDValidity, info.UIDNext, info.HighestModSeq))
		}
	}

	sort.Strings(lines)
	sum := sha256.Sum256([]byte(strings.Join(lines, "\n")))

	return hex.EncodeToString(sum[:]), nil
}

func (k *mailboxDelete) Plan(_ context.Context, _ *Env, u model.User, _ string, params json.RawMessage) (any, string, error) {
	return deleteTargets(u, params)
}

// deleteCheckpoint is the resume state: the drift check passed, the
// targets unregistered and the targets destroyed (Deleted: the
// offline runs before 0.52.0).
type deleteCheckpoint struct {
	Checked      bool     `json:"checked,omitempty"`
	Unregistered []string `json:"unregistered,omitempty"`
	Deleted      []string `json:"deleted"`
}

// Results of a mailbox_delete run (deleteReport.Result and the metric).
const (
	deleteOK    = "ok"
	deleteBusy  = "busy"
	deleteDrift = "drift"
	deleteError = resultError
)

// deleteReport is the result of a run.
type deleteReport struct {
	Mode           string         `json:"mode"`
	Mailboxes      []deleteTarget `json:"mailboxes"`
	Messages       int64          `json:"messages"`
	Bytes          int64          `json:"bytes"`
	LockSeconds    float64        `json:"lock_seconds,omitempty"`
	BarrierSeconds float64        `json:"barrier_seconds,omitempty"`
	Result         string         `json:"result"`
	Reason         string         `json:"reason,omitempty"`
	Key            string         `json:"key,omitempty"`
}

// count adds a destroyed target.
func (r *deleteReport) count(t deleteTarget) {
	r.Mailboxes = append(r.Mailboxes, t)
	r.Messages += t.Messages
	r.Bytes += t.Bytes
}

func (k *mailboxDelete) Run(_ context.Context, run *Run) (any, error) {
	p, err := parseMailboxDelete(run.Job.Params)
	if err != nil {
		return nil, err
	}

	if isOnline(p.Online) {
		return k.runLocked(run)
	}

	return k.runOffline(run)
}

// plannedTargets returns the targets of the plan.
func plannedTargets(run *Run) []deleteTarget {
	var s deleteSummary

	_ = json.Unmarshal(run.Plan.Summary, &s)

	return s.Mailboxes
}

// runOffline is the run under the account lock: the drift check once,
// then every target by id (a target renamed and a mailbox created under
// its name between two attempts is left alone).
func (k *mailboxDelete) runOffline(run *Run) (any, error) {
	var cp deleteCheckpoint

	run.LoadCheckpoint(&cp)

	rep := deleteReport{Mode: model.QuotaRecountOffline, Result: deleteOK}

	if !cp.Checked {
		if err := checkDrift(run); err != nil {
			metrics.MailboxDeleteRuns.WithLabelValues(rep.Mode, deleteDrift).Inc()
			return nil, err
		}

		cp.Checked = true
	}

	ml, _ := run.User.(model.MailboxLocker)

	for _, t := range plannedTargets(run) {
		if slices.Contains(cp.Deleted, t.ID) {
			continue
		}

		if err := run.Fence(); err != nil {
			return cp, err
		}

		if err := deleteTarget1(run.User, ml, t); err != nil {
			return cp, fmt.Errorf("delete %q: %w", t.Name, err)
		}

		rep.count(t)
		cp.Deleted = append(cp.Deleted, t.ID)

		if err := run.Checkpoint(cp); err != nil {
			return cp, err
		}
	}

	metrics.MailboxDeleteRuns.WithLabelValues(rep.Mode, rep.Result).Inc()

	return rep, nil
}

// checkDrift compares the account with the plan (M4).
func checkDrift(run *Run) error {
	_, fp, err := deleteTargets(run.User, run.Job.Params)
	if err != nil {
		return err
	}

	if fp != run.Plan.Fingerprint {
		return ErrDrift
	}

	return nil
}

// deleteTarget1 deletes one target offline: by id where the store can,
// by name otherwise.
func deleteTarget1(u model.User, ml model.MailboxLocker, t deleteTarget) error {
	if ml == nil {
		if err := u.DeleteMailbox(t.Name); err != nil && !errors.Is(err, model.ErrNoMailbox) {
			return err
		}

		return nil
	}

	if _, err := ml.UnregisterForDelete(t.ID, ""); err != nil {
		return err
	}

	_, _, err := ml.FinishDeletion(t.ID)

	return err
}

// lockedRun is the state of a run with the mailbox lock.
type lockedRun struct {
	run     *Run
	ml      model.MailboxLocker
	session string
	targets []deleteTarget
	cp      deleteCheckpoint
	rep     deleteReport
}

// runLocked is the run with the mailbox lock (M1 to M7).
func (k *mailboxDelete) runLocked(run *Run) (res any, err error) {
	ml, err := capability[model.MailboxLocker](run.User, "lock mailboxes")
	if err != nil {
		return nil, err
	}

	l := &lockedRun{run: run, ml: ml, session: lockSession(run.Job.ID), targets: plannedTargets(run),
		rep: deleteReport{Mode: model.QuotaRecountOnline, Result: deleteOK}}

	run.LoadCheckpoint(&l.cp)

	defer func() {
		metrics.MailboxDeleteRuns.WithLabelValues(l.rep.Mode, l.rep.Result).Inc()
	}()

	locks := make([]model.MailboxLockTarget, 0, len(l.targets))
	for _, t := range l.targets {
		if !slices.Contains(l.cp.Unregistered, t.ID) {
			locks = append(locks, model.MailboxLockTarget{ID: t.ID, Name: t.Name})
		}
	}

	if len(locks) > 0 {
		if err := l.lockPhase(locks); err != nil {
			return nil, err
		}
	}

	// M7: destroy (idempotent, from the tombstones).
	if err := l.destroyPhase(); err != nil {
		return l.cp, err
	}

	if err := ml.UnlockMailboxes(l.session); err != nil {
		run.Env.Log.Warn("mailbox_delete: lock record not removed", "err", err)
	}

	return l.rep, nil
}

// lockPhase is M1 to M6: lock, settle, barrier, drift check,
// unregister, unlock. A failure before the targets are unregistered
// gives the mailboxes back at once.
func (l *lockedRun) lockPhase(locks []model.MailboxLockTarget) (err error) {
	run, m, ml := l.run, l.run.m, l.ml
	lockedAt := m.now()

	// M1: lock. An account another job, an import or a scrub holds, or
	// a target another mailbox_delete locks, is tried again later (the
	// attempt does not count).
	_ = run.Progress("lock", 0, int64(len(l.targets)))

	switch err := ml.LockMailboxes(l.session, locks, 2*m.cfg.LeaseTTL); {
	case errors.Is(err, model.ErrImportLocked), errors.Is(err, model.ErrMailboxLocked):
		l.rep.Result = deleteBusy
		return &PostponeError{Until: m.now().Add(m.cfg.LeaseTTL), Reason: err.Error()}
	case errors.Is(err, model.ErrNoMailbox):
		l.rep.Result = deleteDrift
		return fmt.Errorf("%w: %v", ErrDrift, err)
	case err != nil:
		l.rep.Result = deleteError
		return err
	}

	run.KeepAlive(func() error { return ml.RenewMailboxLocks(l.session, 2*m.cfg.LeaseTTL) }, "mailbox lock lost")
	defer run.KeepAlive(nil, "")

	for _, t := range locks {
		ml.WakeSelected(t.ID)
	}

	defer func() {
		if err != nil {
			if uerr := ml.UnlockMailboxes(l.session); uerr != nil {
				run.Env.Log.Warn("mailbox_delete: mailboxes not unlocked", "err", uerr)
			}
		}
	}()

	if !l.cp.Checked {
		if err := l.settleAndCheck(locks); err != nil {
			return err
		}
	}

	if err := l.unregisterPhase(); err != nil {
		return err
	}

	// M6: the records are gone; the lock only says "destroying" now,
	// which keeps nobody out any more.
	if err := ml.SetMailboxLockPhase(l.session, model.MailboxLockDestroying); err != nil {
		run.Env.Log.Warn("mailbox_delete: lock phase not set", "err", err)
	}

	l.rep.LockSeconds = m.now().Sub(lockedAt).Seconds()
	metrics.MailboxLockSeconds.Observe(l.rep.LockSeconds)

	return nil
}

// settleAndCheck is M2 to M4: the settle time (IMAP sessions see the
// lock within registryTTL and the lock probe interval), the barrier and
// the drift check, once.
func (l *lockedRun) settleAndCheck(locks []model.MailboxLockTarget) error {
	run, m := l.run, l.run.m

	time.Sleep(m.cfg.Settle)

	_ = run.Progress("barrier", 0, int64(len(l.targets)))

	start := m.now()

	reason, key, err := l.ml.MailboxLockBarrier(locks, m.now().Add(m.cfg.MailboxLockBarrier), m.deadIntent())
	if err != nil {
		l.rep.Result = deleteError
		return err
	}

	l.rep.BarrierSeconds = m.now().Sub(start).Seconds()

	if reason != "" {
		l.rep.Result, l.rep.Reason, l.rep.Key = deleteBusy, reason, key

		return &PostponeError{Until: m.now().Add(m.cfg.LeaseTTL), Reason: fmt.Sprintf("mailbox lock barrier: %s (%s)", reason, key)}
	}

	_ = run.Progress("check", 0, int64(len(l.targets)))

	if err := checkDrift(run); err != nil {
		if errors.Is(err, ErrDrift) {
			l.rep.Result = deleteDrift
		} else {
			l.rep.Result = deleteError
		}

		return err
	}

	l.cp.Checked = true

	return run.Checkpoint(l.cp)
}

// unregisterPhase is M5: the targets are unregistered, deepest first.
func (l *lockedRun) unregisterPhase() error {
	run := l.run

	for i, t := range l.targets {
		if slices.Contains(l.cp.Unregistered, t.ID) {
			continue
		}

		if err := run.Fence(); err != nil {
			return err
		}

		if _, err := l.ml.UnregisterForDelete(t.ID, l.session); err != nil {
			l.rep.Result = deleteError
			return fmt.Errorf("unregister %q: %w", t.Name, err)
		}

		l.cp.Unregistered = append(l.cp.Unregistered, t.ID)

		if err := run.Checkpoint(l.cp); err != nil {
			return err
		}

		_ = run.Progress("unregister", int64(i+1), int64(len(l.targets)))
	}

	return nil
}

// destroyPhase is M7: blobs and shards of every target go, from its
// tombstone.
func (l *lockedRun) destroyPhase() error {
	run := l.run

	for i, t := range l.targets {
		if slices.Contains(l.cp.Deleted, t.ID) {
			continue
		}

		if err := run.Fence(); err != nil {
			return err
		}

		msgs, bytes, err := l.ml.FinishDeletion(t.ID)
		if err != nil {
			l.rep.Result = deleteError
			return fmt.Errorf("destroy %q: %w", t.Name, err)
		}

		t.Messages, t.Bytes = msgs, bytes
		l.rep.count(t)
		l.cp.Deleted = append(l.cp.Deleted, t.ID)

		if err := run.Checkpoint(l.cp); err != nil {
			return err
		}

		_ = run.Progress("destroy", int64(i+1), int64(len(l.targets)))
	}

	return nil
}

// deadIntent is how old a reservation intent may be before the barriers
// take it for abandoned (quota.recount_dead_intent_seconds).
func (m *Manager) deadIntent() time.Duration {
	if m.cfg.DeadIntent > 0 {
		return m.cfg.DeadIntent
	}

	return 600 * time.Second
}
