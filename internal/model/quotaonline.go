package model

import (
	"errors"
	"fmt"
	"time"
)

// Online quota recount (stage B16) and the mailbox lock of mailbox_delete
// (stage B17): the cluster marks, the errors, the store interfaces and
// the reports. temp/16 describes the design.

// QuotaOnlineMarkKey is the admin store record whose presence allows
// online runs of quota_recalc ("shardpostctl quota online enable"):
// recounts without the account's maintenance lock, whose writers capture
// every change of the usage while a run is under way. It is read without
// a cache by every plan that asks for an online run, so removing it
// makes new runs offline (or refused) at once. Its value is an
// FTSLayoutMark.
const QuotaOnlineMarkKey = "quota/online"

// MailboxLockMarkKey is the admin store record whose presence allows
// mailbox_delete to lock only its target mailboxes instead of the
// account ("shardpostctl mailbox lock enable"). Its value is an
// FTSLayoutMark.
const MailboxLockMarkKey = "mailbox/lock"

// Capabilities of the servers that know the two stages.
const (
	// CapQuotaOnlineRecount is the capability of a server whose writers
	// capture the usage changes an online recount needs (0.52.0).
	CapQuotaOnlineRecount = "quota_online_recount"
	// CapMailboxLock is the capability of a server whose writers honour
	// the mailbox lock of mailbox_delete (0.52.0).
	CapMailboxLock = "mailbox_lock"
)

var (
	// ErrQuotaOnlineMark refuses an online recount without the cluster mark.
	ErrQuotaOnlineMark = errors.New("online quota recounts are not enabled in this cluster (shardpostctl quota online enable)")
	// ErrMailboxLockMark refuses a mailbox_delete with the mailbox lock
	// without the cluster mark.
	ErrMailboxLockMark = errors.New("the mailbox lock of mailbox_delete is not enabled in this cluster (shardpostctl mailbox lock enable)")
	// ErrMailboxLocked is returned when a mailbox is locked by a
	// mailbox_delete job that is about to remove it.
	ErrMailboxLocked = errors.New("mailbox is being deleted")
	// ErrQuotaRecountLost ends an online recount whose run key another
	// writer removed (an offline recount, an import, a rename across the
	// quota rules, a capture limit): the counters are untouched and the
	// job is planned again.
	ErrQuotaRecountLost = errors.New("the online recount was invalidated by another writer")
)

// QuotaOnlineMarked is implemented by stores that read the cluster mark
// of online recounts (QuotaOnlineMarkKey).
type QuotaOnlineMarked interface {
	QuotaOnlineEnabled() (bool, error)
}

// MailboxLockMarked is implemented by stores that read the cluster mark
// of the mailbox lock (MailboxLockMarkKey).
type MailboxLockMarked interface {
	MailboxLockEnabled() (bool, error)
}

// Reasons of QuotaRecountBusyError.
const (
	BusyIntent     = "intent"      // a reservation of before the run is still open
	BusyMoveIntent = "move_intent" // a MOVE of before the run is still under way
	BusyImportLock = "import_lock" // an import or an exclusive job holds the account
	BusyMailboxLck = "mailbox_lock"
	BusyScrub      = "scrub"       // a scrub repairs the account
	BusyForeignRun = "foreign_run" // another online recount is alive
	BusySwitch     = "switch"      // the switch did not go through within the attempts
)

// QuotaRecountBusyError ends an online recount that could not start or
// switch safely: the job is postponed, not failed. Reason is one of the
// Busy* constants, Key the oldest intent waited for (if any).
type QuotaRecountBusyError struct {
	Reason string
	Key    string
}

func (e *QuotaRecountBusyError) Error() string {
	if e.Key != "" {
		return fmt.Sprintf("online recount busy: %s (%s)", e.Reason, e.Key)
	}

	return "online recount busy: " + e.Reason
}

// QuotaRecountRequest is one online recount (QuotaOnlineRecounter).
type QuotaRecountRequest struct {
	// Tok identifies the run: "job-<id>/<lease generation>".
	Tok string
	// Fence is called before every phase that writes; an error ends the
	// run.
	Fence func() error
	// Progress reports the phase and the progress inside it (optional).
	Progress func(phase string, done, total int)
	// Barrier bounds the wait for operations of before the run; Dead is
	// how old an intent may be before it counts as abandoned (both 0:
	// the store's settings).
	Barrier, Dead time.Duration
}

// QuotaRecountCaptures counts the captures of one run by kind.
type QuotaRecountCaptures struct {
	Reserve  int64 `json:"reserve"`
	Rollback int64 `json:"rollback"`
	Release  int64 `json:"release"`
	Mailbox  int64 `json:"mailbox"`
	Move     int64 `json:"move"`
}

// QuotaRecountReport is the result of a quota_recalc run.
type QuotaRecountReport struct {
	Counted Quota  `json:"counted"`
	Stored  Quota  `json:"stored"`
	Differs bool   `json:"differs"`
	Mode    string `json:"mode"` // offline, online
	// The following fields are set by online runs only.
	BarrierSeconds  float64              `json:"barrier_seconds,omitempty"`
	ListedMailboxes int                  `json:"listed_mailboxes,omitempty"`
	Captures        QuotaRecountCaptures `json:"captures"`
	CaptureKeys     int64                `json:"capture_keys,omitempty"`
	CaptureEntries  int64                `json:"capture_entries,omitempty"`
	SwitchRounds    int                  `json:"switch_rounds,omitempty"`
	HoldMS          int64                `json:"hold_ms,omitempty"`
	DeadIntents     int                  `json:"dead_intents,omitempty"`
	// Result is ok, busy or lost; Reason the busy reason, OldestKey the
	// oldest intent a busy barrier waited for.
	Result    string `json:"result,omitempty"`
	Reason    string `json:"reason,omitempty"`
	OldestKey string `json:"oldest_key,omitempty"`
}

// Values of QuotaRecountReport.Mode and Result.
const (
	QuotaRecountOffline = "offline"
	QuotaRecountOnline  = "online"
	QuotaRecountOK      = "ok"
	QuotaRecountBusy    = "busy"
	QuotaRecountLostRes = "lost"
)

// QuotaOnlineRecounter is implemented by user views that recount the
// usage without the account's maintenance lock (stage B16).
type QuotaOnlineRecounter interface {
	// RecountQuotaOnline runs the online recount; a
	// *QuotaRecountBusyError or ErrQuotaRecountLost means nothing was
	// changed.
	RecountQuotaOnline(req QuotaRecountRequest) (QuotaRecountReport, error)
	// RenewQuotaRecount extends the run key of tok (the job's
	// heartbeat); an error means the run is lost.
	RenewQuotaRecount(tok string, ttl time.Duration) error
	// ClearQuotaRecount removes the run key of tok (or an expired one;
	// "" any) with its captures, in version-checked writes only.
	ClearQuotaRecount(tok string) error
}

// MailboxLock is the lock a mailbox_delete job holds on a mailbox it is
// about to remove (stage B17): the job's lock session and the expiry.
type MailboxLock struct {
	Session string `json:"s"`
	Expires int64  `json:"exp"` // unix seconds
}

// Held reports whether the lock is alive at now and not session's own.
func (l *MailboxLock) Held(session string, now time.Time) bool {
	return l != nil && l.Session != session && time.Unix(l.Expires, 0).After(now)
}

// Phases of a mailbox lock (the account-wide overview key).
const (
	MailboxLockLocked        = "locked"
	MailboxLockUnregistering = "unregistering"
	MailboxLockDestroying    = "destroying"
)

// MailboxLockTarget is one mailbox a mailbox_delete job locks.
type MailboxLockTarget struct {
	ID   string
	Name string
}

// MailboxLockState is what a store reports about a session's lock.
type MailboxLockState struct {
	Phase   string
	Expires time.Time
	IDs     []string
}

// MailboxLocker is implemented by user views that lock single mailboxes
// for mailbox_delete (stage B17).
type MailboxLocker interface {
	// LockMailboxes locks the targets for session (or renews the
	// session's own lock on them) for ttl. ErrNoMailbox when a target is
	// not registered under its id, ErrMailboxLocked when another session
	// holds one, ErrImportLocked when the account is locked.
	LockMailboxes(session string, targets []MailboxLockTarget, ttl time.Duration) error
	// RenewMailboxLocks extends the session's locks; an error means they
	// are lost.
	RenewMailboxLocks(session string, ttl time.Duration) error
	// SetMailboxLockPhase records the phase of the session's lock.
	SetMailboxLockPhase(session, phase string) error
	// UnlockMailboxes removes the session's locks and its overview key.
	UnlockMailboxes(session string) error
	// MailboxLockBarrier waits until no operation that started before
	// the lock can still write into a target: no reservation names one,
	// no MOVE intent points at one or sits on one. It returns the
	// reason and key it still waits for when deadline passes.
	MailboxLockBarrier(targets []MailboxLockTarget, deadline time.Time, dead time.Duration) (reason, key string, err error)
	// UnregisterForDelete removes the registry entry of the mailbox id
	// (checked against session's lock; "" needs no lock) and retires its
	// header; the messages and the tombstone stay for FinishDeletion.
	// done is false when the mailbox was already gone.
	UnregisterForDelete(id, session string) (done bool, err error)
	// FinishDeletion releases the blobs and shards of an unregistered
	// mailbox from its tombstone; it ends when the tombstone is gone.
	FinishDeletion(id string) (msgs, bytes int64, err error)
	// WakeSelected wakes the sessions that have the mailbox selected
	// (best effort).
	WakeSelected(id string)
}

// MailboxLockReader is implemented by user views that report the lock
// state of a mailbox with one read: ErrMailboxLocked when a
// mailbox_delete locks it, ErrNoMailbox when it is not registered,
// ErrImportLocked when the account is locked, nil otherwise. A transport
// error is returned as such.
type MailboxLockReader interface {
	LockedMailbox(name string) error
}

// OwnExpunger is implemented by user views whose Expunge checks the
// mailbox lock: ExpungeOwn removes entries this caller wrote itself
// without the check (the rollback of a delivery, the scrub).
type OwnExpunger interface {
	ExpungeOwn(name string, metas []Meta) error
}

// MailboxLockedError reports whether err says that a mailbox is locked
// by a mailbox_delete.
func MailboxLockedError(err error) bool { return errors.Is(err, ErrMailboxLocked) }

// Selected re-checks the selected mailbox name of a running session at
// most every LockCheckInterval: it returns ErrMailboxLocked when a
// mailbox_delete locks it, ErrNoMailbox when the registry no longer has
// it (both authoritative, read from the registry), nil otherwise and on
// every transient error.
func (p *LockProbe) Selected(u User, name string) error {
	now := time.Now()
	if u == nil || name == "" || now.Before(p.next) {
		return nil
	}

	p.next = now.Add(LockCheckInterval)

	return SelectedMailboxState(u, name)
}

// SelectedMailboxState reads the state of a selected mailbox now (see
// LockProbe.Selected); it returns only authoritative answers.
func SelectedMailboxState(u User, name string) error {
	r, ok := u.(MailboxLockReader)
	if !ok {
		return nil
	}

	switch err := r.LockedMailbox(name); {
	case errors.Is(err, ErrMailboxLocked), errors.Is(err, ErrNoMailbox), errors.Is(err, ErrImportLocked):
		return err
	default:
		return nil
	}
}
