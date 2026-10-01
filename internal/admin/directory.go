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

// GetAccountDirectory shows the account's directory entry, whether its
// registry exists and the state of the directory.
func (s *Server) GetAccountDirectory(w http.ResponseWriter, r *http.Request, account adminapi.Account) {
	auditf(r, "account", account)

	if !requireAccount(w, r, RoleDiagnose, account) {
		return
	}

	if s.o.Directory == nil {
		writeError(w, http.StatusForbidden, adminapi.Forbidden, "the account directory is not enabled")
		return
	}

	if !validAccount(account) {
		writeError(w, http.StatusBadRequest, adminapi.BadRequest, "invalid account")
		return
	}

	st, err := directoryStatus(s.o.Directory, account)
	if err != nil {
		s.importError(w, r, err)
		return
	}

	writeJSON(w, http.StatusOK, st)
}

func directoryStatus(dir model.AccountDirectory, account string) (adminapi.DirectoryStatus, error) {
	out := adminapi.DirectoryStatus{Account: account}

	ix, release, err := dir.AccountIndex()
	if err != nil {
		return out, err
	}
	defer release()

	e, ok, err := model.GetAccountEntry(ix, account)
	if err != nil {
		return out, err
	}

	if ok {
		out.Found, out.Entry = true, directoryEntry(&e)
	}

	if out.Registry, err = dir.HasRegistry(account); err != nil {
		return out, err
	}

	h, err := ix.Head()
	if err != nil {
		return out, err
	}

	out.Shards, out.Phase = h.Shards, h.Phase

	if m := h.Markers[model.AccountBackfillMarker]; m != "" {
		out.Backfill = &m
	}

	return out, nil
}

// directoryOperation labels the paths below /directory.
func directoryOperation(parts []string) string {
	switch {
	case len(parts) == 0:
		return segDirectory
	case len(parts) == 1 && parts[0] == "repair":
		return "directory_repair"
	default:
		return opOther
	}
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
