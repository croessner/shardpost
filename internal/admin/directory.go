package admin

import (
	"net/http"

	"shardpost/internal/adminapi"
	"shardpost/internal/jobs"
	"shardpost/internal/model"
)

// RepairAccountDirectory applies the repair rules of the account
// directory to one account (jobs.RepairAccountEntry).
func (s *Server) RepairAccountDirectory(w http.ResponseWriter, r *http.Request, account adminapi.Account) {
	auditf(r, "account", account)

	if !requireAccount(w, r, jobs.RoleRepair, account) {
		return
	}

	if s.o.Directory == nil || s.o.Jobs == nil {
		writeError(w, http.StatusForbidden, adminapi.Forbidden, "the account directory is not enabled")
		return
	}

	if !validAccount(account) {
		writeError(w, http.StatusBadRequest, adminapi.BadRequest, "invalid account")
		return
	}

	rep, err := jobs.RepairAccountEntry(s.o.Jobs.KV(), s.o.Directory, account)
	if err != nil {
		s.importError(w, r, err)
		return
	}

	auditf(r, "action", rep.Action)
	writeJSON(w, http.StatusOK, directoryRepair(rep))
}

// directoryRepair is the API view of a repair.
func directoryRepair(rep model.DirectoryRepair) adminapi.DirectoryRepair {
	return adminapi.DirectoryRepair{
		Account: rep.Account, Action: adminapi.DirectoryRepairAction(rep.Action), Found: rep.Found,
		Before: directoryEntry(rep.Before), After: directoryEntry(rep.After),
	}
}

func directoryEntry(e *model.AccountEntry) *adminapi.DirectoryEntry {
	if e == nil {
		return nil
	}

	out := &adminapi.DirectoryEntry{Created: e.Created.UTC(), State: adminapi.DirectoryEntryState(e.State)}

	if e.Session != "" {
		out.Session = &e.Session
	}

	if e.Cluster != "" {
		out.Cluster = &e.Cluster
	}

	return out
}
