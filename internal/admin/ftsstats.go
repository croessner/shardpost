package admin

import (
	"net/http"
	"strings"

	"shardpost/internal/adminapi"
	"shardpost/internal/model"
)

// Key counts of an account's full-text index (temp/13 §7.1): counters
// by default, every key on request, the classes of the terms only for
// the test accounts of fts.stats_test_accounts. No term leaves the
// server.

// maxStatsParallel bounds the parallel objects of an exact count.
const maxStatsParallel = 16

// GetFTSStats reports the key counts of an account's index.
func (s *Server) GetFTSStats(w http.ResponseWriter, r *http.Request, account adminapi.Account, p adminapi.GetFTSStatsParams) {
	if !requireAccount(w, r, RoleDiagnose, account) {
		return
	}

	req, ok := s.statsRequest(w, r, account, p)
	if !ok {
		return
	}

	u, err := s.o.Accounts.User(account)
	if err != nil {
		s.importError(w, r, err)
		return
	}
	defer u.Close()

	st, ok := u.(model.FTSStatser)
	if !ok {
		writeError(w, http.StatusServiceUnavailable, adminapi.Temporary, "the store has no full-text statistics")
		return
	}

	stats, err := st.FTSStats(req)
	if err != nil {
		s.importError(w, r, err)
		return
	}

	writeJSON(w, http.StatusOK, stats)
}

// statsRequest checks the account and the parameters (an error is
// answered).
func (s *Server) statsRequest(w http.ResponseWriter, r *http.Request, account string, p adminapi.GetFTSStatsParams) (model.FTSStatsRequest, bool) {
	req := model.FTSStatsRequest{Exact: p.Exact != nil && *p.Exact, Classes: p.Classes != nil && *p.Classes, Context: r.Context()}

	switch {
	case !validAccount(account) || s.o.Accounts == nil:
		writeError(w, http.StatusBadRequest, adminapi.BadRequest, "invalid account")
		return req, false
	case p.Parallel != nil && (*p.Parallel < 1 || *p.Parallel > maxStatsParallel):
		writeError(w, http.StatusBadRequest, adminapi.BadRequest, "parallel must be 1 to 16")
		return req, false
	case req.Classes && (s.o.FTSStatsTestAccount == nil || !s.o.FTSStatsTestAccount(strings.ToLower(account))):
		writeError(w, http.StatusForbidden, adminapi.Forbidden, model.ErrFTSStatsClasses.Error())
		return req, false
	}

	if p.Parallel != nil {
		req.Parallel = *p.Parallel
	}

	return req, true
}
