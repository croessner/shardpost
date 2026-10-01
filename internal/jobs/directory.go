package jobs

import (
	"errors"
	"strings"
	"time"

	"shardpost/internal/model"
)

// The account directory and the purge job (scaling plan B4): purge
// marks the account purging under its lock session before it deletes the
// namespace (purgeObjects) and removes the entry at the end
// (purgeDirectory). An entry left purging by a purge that failed or was
// cancelled is repaired here.

// PurgeRunning reports whether the job behind a maintenance lock session
// is a purge that has not ended (a takeover may still continue it). An
// unknown session or job counts as not running.
func PurgeRunning(kv model.AdminKV, session string) (bool, error) {
	id, ok := strings.CutPrefix(session, "job-")
	if !ok || lockSession(id) != session {
		return false, nil
	}

	j, err := store{kv}.getJob(id)
	if errors.Is(err, ErrNotFound) {
		return false, nil
	}

	if err != nil {
		return false, err
	}

	return j.Kind == KindPurge && !j.Terminal(), nil
}

// RepairAccountEntry applies the repair rules of the account directory
// to one account (model.RepairAccountEntry), with the job store telling
// whether a purge still runs.
func RepairAccountEntry(kv model.AdminKV, dir model.AccountDirectory, account string) (model.DirectoryRepair, error) {
	ix, release, err := dir.AccountIndex()
	if err != nil {
		return model.DirectoryRepair{Account: account, Action: model.RepairNone}, err
	}
	defer release()

	res, err := model.RepairAccountEntry(ix, account, model.RepairChecks{
		HasRegistry:  func() (bool, error) { return dir.HasRegistry(account) },
		PurgeRunning: func(session string) (bool, error) { return PurgeRunning(kv, session) },
		Now:          time.Now(),
	})

	ForgetActivated(dir, res)

	return res, err
}

// ForgetActivated drops the purge record of an account a repair activated
// again or whose stale entry it removed: such an account is no purged
// one, and without an entry a left-over record would let the sweep take
// its namespace for debris (best effort; the sweep still requires the
// registry to be missing).
func ForgetActivated(dir any, res model.DirectoryRepair) {
	f, ok := dir.(model.PurgedForgetter)
	if ok && (res.Action == model.RepairActivated || res.Action == model.RepairStale) {
		_ = f.ForgetPurged(res.Account)
	}
}

// ErrLeftPurging refuses an exclusive job on an account that an ended
// purge left purging: nothing can take its lock until the entry is
// repaired.
var ErrLeftPurging = errors.New("the account was left purging by an ended purge job; run shardpostctl account repair")

// leftPurging reports whether the account's lock is refused because an
// ended purge of another session left it purging (and its registry is
// gone): waiting for the lock would not help.
func (m *Manager) leftPurging(u model.User, jobID string) bool {
	dp, ok := u.(model.DirectoryPurger)
	if !ok {
		return false
	}

	session, purging, err := dp.PurgingSession()
	if err != nil || !purging || session == lockSession(jobID) {
		return false
	}

	running, err := PurgeRunning(m.st.kv, session)

	return err == nil && !running
}

// KV returns the admin store of the job system.
func (m *Manager) KV() model.AdminKV { return m.st.kv }
