package admin

import (
	"net/http"
	"time"

	"shardpost/internal/jobs"
	"shardpost/internal/model"
)

// The cluster mark of online runs of the full-text index (stage B14i,
// temp/14 §5.4): it allows fts_layout and fts_rebuild without the
// account's maintenance lock, whose writers record what they index
// meanwhile. A server before 0.51.0 does not, and waits at the run's
// freeze; so every server must have the capability first.

// GetFTSOnline reports the mark and the registrations.
func (s *Server) GetFTSOnline(w http.ResponseWriter, r *http.Request) {
	if !require(w, r, RoleDiagnose) || !s.layoutEnabled(w) {
		return
	}

	st, err := s.markStatus(model.FTSOnlineMarkKey, time.Now())
	if err != nil {
		s.importError(w, r, err)
		return
	}

	writeJSON(w, http.StatusOK, st)
}

// EnableFTSOnline sets the mark after the registrations check.
func (s *Server) EnableFTSOnline(w http.ResponseWriter, r *http.Request) {
	s.enableMark(w, r, model.FTSOnlineMarkKey, model.CapFTSOnlineBuild)
}

// DisableFTSOnline removes the mark: new plans run offline. Runs that
// started go on (their accounts show next_online in the layout state).
func (s *Server) DisableFTSOnline(w http.ResponseWriter, r *http.Request) {
	if !require(w, r, jobs.RoleRepair) || !s.layoutEnabled(w) {
		return
	}

	auditf(r, "fts_online", "disable")

	if err := s.o.KV.Update(model.FTSOnlineMarkKey, func([]byte) ([]byte, error) { return nil, nil }); err != nil {
		s.importError(w, r, err)
		return
	}

	st, err := s.markStatus(model.FTSOnlineMarkKey, time.Now())
	if err != nil {
		s.importError(w, r, err)
		return
	}

	writeJSON(w, http.StatusOK, st)
}
