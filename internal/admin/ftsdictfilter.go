package admin

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"shardpost/internal/adminapi"
	"shardpost/internal/jobs"
	"shardpost/internal/model"
)

// The cluster mark of the dictionary filter (stage B14h, temp/13 §5.3):
// it allows index generations with the identifier filter and with the
// text preparation "nosig", which a version before 0.50.0 must not
// search.

// GetFTSDictFilter reports the mark and the registrations.
func (s *Server) GetFTSDictFilter(w http.ResponseWriter, r *http.Request) {
	if !require(w, r, RoleDiagnose) || !s.layoutEnabled(w) {
		return
	}

	st, err := s.markStatus(model.FTSDictFilterMarkKey, time.Now())
	if err != nil {
		s.importError(w, r, err)
		return
	}

	writeJSON(w, http.StatusOK, st)
}

// EnableFTSDictFilter sets the mark after the registrations check.
func (s *Server) EnableFTSDictFilter(w http.ResponseWriter, r *http.Request) {
	s.enableMark(w, r, model.FTSDictFilterMarkKey, model.CapFTSDictFilter)
}

// DisableFTSDictFilter removes the mark and reports the accounts whose
// presence entry says that a generation of theirs has (or is about to
// get) the filter or the text preparation "nosig" (Props): it reads the
// presence index only, as the disable of layout 3 does.
func (s *Server) DisableFTSDictFilter(w http.ResponseWriter, r *http.Request) {
	if !require(w, r, jobs.RoleRepair) || !s.layoutEnabled(w) {
		return
	}

	auditf(r, "fts_dictfilter", "disable")

	if err := s.o.KV.Update(model.FTSDictFilterMarkKey, func([]byte) ([]byte, error) { return nil, nil }); err != nil {
		s.importError(w, r, err)
		return
	}

	select {
	case <-r.Context().Done():
		return
	case <-time.After(s.o.LayoutDisableWait):
	}

	accounts, more, err := s.propsAccounts(r.Context(), disableListLimit)
	if err != nil {
		s.importError(w, r, err)
		return
	}

	out := adminapi.FTSLayout3Disable{Accounts: accounts}
	if more {
		out.More = &more
	}

	writeJSON(w, http.StatusOK, out)
}

// propsAccounts pages through the presence index and returns, in its
// order, at most limit accounts whose entry has Props (or cannot be
// read), and whether there are more.
func (s *Server) propsAccounts(ctx context.Context, limit int) ([]string, bool, error) {
	out := []string{}
	after := ""

	for {
		entries, next, err := s.layoutAccounts(after, maxLayoutPage)
		if err != nil {
			return nil, false, err
		}

		for _, e := range entries {
			var v model.FTSLayoutEntry
			if json.Unmarshal(e.Value, &v) == nil && v.Props.IsZero() {
				continue
			}

			if len(out) == limit {
				return out, true, nil
			}

			out = append(out, e.ID)
		}

		if err := ctx.Err(); err != nil {
			return nil, false, err
		}

		if next == "" {
			return out, false, nil
		}

		after = next
	}
}
