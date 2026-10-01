package jobs

import (
	"errors"
	"strings"

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

	return model.RepairAccountEntry(ix, account,
		func() (bool, error) { return dir.HasRegistry(account) },
		func(session string) (bool, error) { return PurgeRunning(kv, session) })
}

// KV returns the admin store of the job system.
func (m *Manager) KV() model.AdminKV { return m.st.kv }
