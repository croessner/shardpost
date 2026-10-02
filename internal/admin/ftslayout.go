package admin

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"shardpost/internal/adminapi"
	"shardpost/internal/jobs"
	"shardpost/internal/model"
)

// Layout 3 of the full-text index (stage B14b): the cluster mark, the
// server registrations, the presence index and the layout record of an
// account.

// FTSLayoutStore is what the store offers for the layout administration.
type FTSLayoutStore interface {
	model.FTSLayoutMarked
	// FTSLayoutIndex opens the presence index of the accounts with a
	// layout 3 index; release closes it.
	FTSLayoutIndex() (model.SortedIndex, func(), error)
}

// Bounds of the layout operations.
const (
	defaultLayoutPage = 100
	maxLayoutPage     = 10000
	// disableListLimit bounds the accounts disable reports.
	disableListLimit = 1000
	// defaultDisableWait is how long disable waits before it reads the
	// presence index: a creator that read the mark just before it went
	// finishes its creation within it (N7).
	defaultDisableWait = 5 * time.Second
)

// layoutEnabled answers 503 when the layout administration is missing.
func (s *Server) layoutEnabled(w http.ResponseWriter) bool {
	if s.o.KV == nil || s.o.FTSLayouts == nil {
		writeError(w, http.StatusServiceUnavailable, adminapi.Temporary, "the full-text layout administration is not available")
		return false
	}

	return true
}

// GetFTSLayout3 reports the mark and the registrations.
func (s *Server) GetFTSLayout3(w http.ResponseWriter, r *http.Request) {
	if !require(w, r, RoleDiagnose) || !s.layoutEnabled(w) {
		return
	}

	st, err := s.layout3Status(time.Now())
	if err != nil {
		s.importError(w, r, err)
		return
	}

	writeJSON(w, http.StatusOK, st)
}

// layout3Status reads the mark and the registrations.
func (s *Server) layout3Status(now time.Time) (adminapi.FTSLayout3Status, error) {
	st := adminapi.FTSLayout3Status{Servers: []adminapi.ServerRegistration{}}

	b, err := s.o.KV.Get(model.FTSLayoutMarkKey)

	switch {
	case errors.Is(err, model.ErrNotFound):
	case err != nil:
		return st, err
	default:
		var m model.FTSLayoutMark
		if json.Unmarshal(b, &m) == nil {
			st.Enabled = true
			st.Mark = &adminapi.FTSLayout3Mark{EnabledAt: m.EnabledAt, Principal: optional(m.Principal), Pods: m.Pods, Forced: &m.Forced}
		}
	}

	servers, err := jobs.Servers(s.o.KV, now)
	if err != nil {
		return st, err
	}

	st.Servers = serverRegistrations(servers)

	return st, nil
}

// EnableFTSLayout3 sets the mark after the registrations check.
func (s *Server) EnableFTSLayout3(w http.ResponseWriter, r *http.Request) {
	if !require(w, r, jobs.RoleRepair) || !s.layoutEnabled(w) {
		return
	}

	var req adminapi.FTSLayout3Enable
	if err := decodeJSON(r, &req); err != nil || req.Pods < 1 {
		writeError(w, http.StatusBadRequest, adminapi.BadRequest, "pods (the number of running servers) is required")
		return
	}

	force := req.Force != nil && *req.Force
	auditf(r, "pods", req.Pods, "force", force)

	now := time.Now()

	servers, err := jobs.Servers(s.o.KV, now)
	if err != nil {
		s.importError(w, r, err)
		return
	}

	if err := jobs.Layout3Ready(servers, req.Pods); err != nil && !force {
		writeError(w, http.StatusConflict, adminapi.Conflict, err.Error())
		return
	}

	mark, _ := json.Marshal(model.FTSLayoutMark{EnabledAt: now.UTC(), Principal: principalOf(r.Context()).name, Pods: req.Pods, Forced: force})

	if err := s.o.KV.Update(model.FTSLayoutMarkKey, func([]byte) ([]byte, error) { return mark, nil }); err != nil {
		s.importError(w, r, err)
		return
	}

	st, err := s.layout3Status(now)
	if err != nil {
		s.importError(w, r, err)
		return
	}

	writeJSON(w, http.StatusOK, st)
}

// DisableFTSLayout3 removes the mark and reports the accounts that
// still have layout 3.
func (s *Server) DisableFTSLayout3(w http.ResponseWriter, r *http.Request) {
	if !require(w, r, jobs.RoleRepair) || !s.layoutEnabled(w) {
		return
	}

	auditf(r, "fts_layout3", "disable")

	if err := s.o.KV.Update(model.FTSLayoutMarkKey, func([]byte) ([]byte, error) { return nil, nil }); err != nil {
		s.importError(w, r, err)
		return
	}

	select {
	case <-r.Context().Done():
		return
	case <-time.After(s.o.LayoutDisableWait):
	}

	entries, next, err := s.layoutAccounts("", disableListLimit)
	if err != nil {
		s.importError(w, r, err)
		return
	}

	out := adminapi.FTSLayout3Disable{Accounts: make([]string, 0, len(entries))}
	for _, e := range entries {
		out.Accounts = append(out.Accounts, e.ID)
	}

	if next != "" {
		out.More = new(bool)
		*out.More = true
	}

	writeJSON(w, http.StatusOK, out)
}

// layoutAccounts reads a page of the presence index.
func (s *Server) layoutAccounts(after string, limit int) ([]model.IndexEntry, string, error) {
	ix, release, err := s.o.FTSLayouts.FTSLayoutIndex()
	if err != nil {
		return nil, "", err
	}
	defer release()

	return ix.List(after, limit)
}

// ListFTSLayout3Accounts lists the presence index.
func (s *Server) ListFTSLayout3Accounts(w http.ResponseWriter, r *http.Request, p adminapi.ListFTSLayout3AccountsParams) {
	if !requireAllAccounts(w, r, RoleDiagnose) || !s.layoutEnabled(w) {
		return
	}

	limit := defaultLayoutPage
	if p.Limit != nil {
		limit = *p.Limit
	}

	if limit < 1 || limit > maxLayoutPage {
		writeError(w, http.StatusBadRequest, adminapi.BadRequest, "limit must be 1 to 10000")
		return
	}

	entries, next, err := s.layoutAccounts(deref(p.After), limit)
	if err != nil {
		s.importError(w, r, err)
		return
	}

	page := adminapi.FTSLayoutAccountPage{Items: make([]adminapi.FTSLayoutAccount, 0, len(entries))}

	for _, e := range entries {
		item := adminapi.FTSLayoutAccount{Account: e.ID}

		var v model.FTSLayoutEntry
		if json.Unmarshal(e.Value, &v) == nil && !v.Since.IsZero() {
			item.Since = &v.Since
		}

		if p.Verify != nil && *p.Verify {
			s.verifyLayout(&item)
		}

		page.Items = append(page.Items, item)
	}

	page.Next = optional(next)

	writeJSON(w, http.StatusOK, page)
}

// verifyLayout reads the account's record: an entry whose account has no
// layout 3 index and no conversion to it is a mismatch.
func (s *Server) verifyLayout(item *adminapi.FTSLayoutAccount) {
	st, err := s.accountLayout(item.Account)
	if err != nil {
		item.Error = optional(err.Error())
		return
	}

	api := apiLayoutState(st)
	mismatch := st.Current.Layout != model.FTSLayout3 && (st.Next == nil || st.Next.Layout != model.FTSLayout3)
	item.Layout, item.Mismatch = &api, &mismatch
}

// accountLayout reads the layout record of an account.
func (s *Server) accountLayout(account string) (model.FTSLayoutState, error) {
	if s.o.Accounts == nil {
		return model.FTSLayoutState{}, errors.New("accounts cannot be opened")
	}

	u, err := s.o.Accounts.User(account)
	if err != nil {
		return model.FTSLayoutState{}, err
	}
	defer u.Close()

	lay, ok := u.(model.FTSLayouter)
	if !ok {
		return model.FTSLayoutState{}, errors.New("the store has no full-text layouts")
	}

	return lay.FTSLayout()
}

// GetFTSLayout reports the layout record of an account.
func (s *Server) GetFTSLayout(w http.ResponseWriter, r *http.Request, account adminapi.Account) {
	if !requireAccount(w, r, RoleDiagnose, account) {
		return
	}

	if !validAccount(account) {
		writeError(w, http.StatusBadRequest, adminapi.BadRequest, "invalid account")
		return
	}

	st, err := s.accountLayout(account)
	if err != nil {
		s.importError(w, r, err)
		return
	}

	writeJSON(w, http.StatusOK, apiLayoutState(st))
}

// ClearFTSLayout removes the unfinished conversion of an ended run.
func (s *Server) ClearFTSLayout(w http.ResponseWriter, r *http.Request, account adminapi.Account) {
	if !requireAccount(w, r, jobs.RoleRepair, account) || !s.layoutEnabled(w) {
		return
	}

	auditf(r, "account", account)

	if !validAccount(account) || s.o.Accounts == nil {
		writeError(w, http.StatusBadRequest, adminapi.BadRequest, "invalid account")
		return
	}

	u, err := s.o.Accounts.User(account)
	if err != nil {
		s.importError(w, r, err)
		return
	}
	defer u.Close()

	tok, err := jobs.ClearStaleLayout(s.o.KV, u, time.Now())
	if err != nil {
		s.importError(w, r, err)
		return
	}

	auditf(r, "cleared", tok)

	st, err := s.accountLayout(account)
	if err != nil {
		s.importError(w, r, err)
		return
	}

	writeJSON(w, http.StatusOK, adminapi.FTSLayoutClear{Cleared: tok, State: apiLayoutState(st)})
}

// apiLayoutState converts a layout record for the API.
func apiLayoutState(st model.FTSLayoutState) adminapi.FTSLayoutState {
	out := adminapi.FTSLayoutState{Current: apiShape(st.Current), NextTok: optional(st.NextTok)}

	if st.Next != nil {
		n := apiShape(*st.Next)
		out.Next = &n
	}

	if !st.NextAt.IsZero() {
		at := st.NextAt
		out.NextAt = &at
	}

	if len(st.Old) > 0 {
		old := make([]adminapi.FTSLayoutShape, 0, len(st.Old))
		for _, o := range st.Old {
			old = append(old, apiShape(o))
		}

		toks, at := append([]string{}, st.OldToks...), make([]string, 0, len(st.OldToks))

		for i := range st.OldToks {
			var beat string
			if i < len(st.OldAt) && !st.OldAt[i].IsZero() {
				beat = st.OldAt[i].UTC().Format(time.RFC3339)
			}

			at = append(at, beat)
		}

		out.Old, out.OldToks, out.OldAt = &old, &toks, &at
	}

	return out
}

func apiShape(s model.FTSLayoutShape) adminapi.FTSLayoutShape {
	out := adminapi.FTSLayoutShape{Layout: s.Layout, Gen: optional(s.Gen)}

	if s.S > 0 {
		out.S, out.D, out.H = &s.S, &s.D, &s.H
	}

	return out
}
