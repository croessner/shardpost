package admin

import (
	"net/http"
	"time"

	"shardpost/internal/jobs"
	"shardpost/internal/model"
)

// The cluster marks of the online quota recount (stage B16, temp/16
// §5.4) and of the mailbox lock of mailbox_delete (stage B17, §10.3).
// Both work like the mark of online full-text runs: an enable checks
// that every running server has the capability, since a server without
// it would not capture its usage changes (the counters a run switches to
// would be wrong) or would not see the lock (a mail could be deleted
// with the mailbox).

// GetQuotaOnline reports the mark and the registrations.
func (s *Server) GetQuotaOnline(w http.ResponseWriter, r *http.Request) {
	s.markGet(w, r, model.QuotaOnlineMarkKey)
}

// EnableQuotaOnline sets the mark after the registrations check.
func (s *Server) EnableQuotaOnline(w http.ResponseWriter, r *http.Request) {
	s.enableMark(w, r, model.QuotaOnlineMarkKey, model.CapQuotaOnlineRecount)
}

// DisableQuotaOnline removes the mark: new plans run offline.
func (s *Server) DisableQuotaOnline(w http.ResponseWriter, r *http.Request) {
	s.markDisable(w, r, model.QuotaOnlineMarkKey, "quota_online")
}

// GetMailboxLock reports the mark and the registrations.
func (s *Server) GetMailboxLock(w http.ResponseWriter, r *http.Request) {
	s.markGet(w, r, model.MailboxLockMarkKey)
}

// EnableMailboxLock sets the mark after the registrations check.
func (s *Server) EnableMailboxLock(w http.ResponseWriter, r *http.Request) {
	s.enableMark(w, r, model.MailboxLockMarkKey, model.CapMailboxLock)
}

// DisableMailboxLock removes the mark: new plans take the account lock.
func (s *Server) DisableMailboxLock(w http.ResponseWriter, r *http.Request) {
	s.markDisable(w, r, model.MailboxLockMarkKey, "mailbox_lock")
}

// markGet answers the status of a cluster mark (role diagnose).
func (s *Server) markGet(w http.ResponseWriter, r *http.Request, key string) {
	if !require(w, r, RoleDiagnose) || !s.layoutEnabled(w) {
		return
	}

	st, err := s.markStatus(key, time.Now())
	if err != nil {
		s.importError(w, r, err)
		return
	}

	writeJSON(w, http.StatusOK, st)
}

// markDisable removes a cluster mark (role repair) and answers its
// status; what is the audit action.
func (s *Server) markDisable(w http.ResponseWriter, r *http.Request, key, what string) {
	if !require(w, r, jobs.RoleRepair) || !s.layoutEnabled(w) {
		return
	}

	auditf(r, what, "disable")

	if err := s.o.KV.Update(key, func([]byte) ([]byte, error) { return nil, nil }); err != nil {
		s.importError(w, r, err)
		return
	}

	st, err := s.markStatus(key, time.Now())
	if err != nil {
		s.importError(w, r, err)
		return
	}

	writeJSON(w, http.StatusOK, st)
}
