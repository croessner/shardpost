package admin

import (
	"context"
	"net/http"
	"sync"
	"time"

	"shardpost/internal/adminapi"
	"shardpost/internal/jobs"
	"shardpost/internal/model"
)

// The cluster mark of the dictionary filter (stage B14h, temp/13 §5.3):
// it allows index generations with the identifier filter and with the
// text preparation "nosig", which a version before 0.50.0 must not
// search.

// dictFilterScanParallel bounds the layout records disable reads at a
// time.
const dictFilterScanParallel = 16

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
// index still has a generation with the filter or the text preparation
// "nosig": every account of the layout 3 presence index, one read of
// its layout record each.
func (s *Server) DisableFTSDictFilter(w http.ResponseWriter, r *http.Request) {
	if !requireAllAccounts(w, r, jobs.RoleRepair) || !s.layoutEnabled(w) {
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

	accounts, more, err := s.filteredAccounts(r.Context(), disableListLimit)
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

// filteredAccounts reads the layout record of every account of the
// presence index and returns, sorted as the index is, at most limit of
// those whose current or next generation has the identifier filter or
// the text preparation "nosig", and whether there are more. An account
// whose record cannot be read is listed too: it may have one.
func (s *Server) filteredAccounts(ctx context.Context, limit int) ([]string, bool, error) {
	out := []string{}
	after := ""

	for {
		entries, next, err := s.layoutAccounts(after, defaultLayoutPage*10)
		if err != nil {
			return nil, false, err
		}

		hit := make([]bool, len(entries))
		sem := make(chan struct{}, dictFilterScanParallel)

		var wg sync.WaitGroup

		for i, e := range entries {
			wg.Add(1)

			sem <- struct{}{}

			go func() {
				defer wg.Done()
				defer func() { <-sem }()

				st, err := s.accountLayout(e.ID)
				hit[i] = err != nil || layoutFiltered(st)
			}()
		}

		wg.Wait()

		for i, e := range entries {
			if !hit[i] {
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

// layoutFiltered reports whether the current or next generation of a
// layout record has the identifier filter or the text preparation
// "nosig".
func layoutFiltered(st model.FTSLayoutState) bool {
	props := func(sh model.FTSLayoutShape) bool { return sh.DictIDMinRunes > 0 || sh.AttachedHeaders != "" }

	return props(st.Current) || (st.Next != nil && props(*st.Next))
}
