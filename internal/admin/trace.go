package admin

import (
	"net/http"
	"time"

	"shardpost/internal/adminapi"
	"shardpost/internal/model"
)

// RoleTrace may switch the protocol trace of accounts on and off. The
// trace logs protocol lines of the account (never literal contents), so
// it is a role of its own.
const RoleTrace = "trace"

// beginTrace checks role and account and opens the account's trace
// switch; ok false means the response is written. The caller closes u.
// The account must be canonical: sessions trace the canonical account,
// so a switch under an alias would never take effect (and its status
// would mislead).
func (s *Server) beginTrace(w http.ResponseWriter, r *http.Request, account string) (model.User, model.Tracer, bool) {
	auditf(r, "account", account)

	if !requireAccount(w, r, RoleTrace, account) {
		return nil, nil, false
	}

	if s.o.Accounts == nil {
		writeError(w, http.StatusForbidden, adminapi.Forbidden, "trace is not enabled")
		return nil, nil, false
	}

	if !validAccount(account) {
		writeError(w, http.StatusBadRequest, adminapi.BadRequest, "invalid account")
		return nil, nil, false
	}

	if !s.canonical(w, r, account) {
		return nil, nil, false
	}

	u, err := s.o.Accounts.User(account)
	if err != nil {
		s.importError(w, r, err)
		return nil, nil, false
	}

	t, ok := u.(model.Tracer)
	if !ok {
		u.Close()
		writeError(w, http.StatusInternalServerError, adminapi.Internal, "the store cannot trace")

		return nil, nil, false
	}

	return u, t, true
}

// GetTrace reports the trace switch of an account.
func (s *Server) GetTrace(w http.ResponseWriter, r *http.Request, account adminapi.Account) {
	u, t, ok := s.beginTrace(w, r, account)
	if !ok {
		return
	}
	defer u.Close()

	st, err := t.Trace()
	if err != nil {
		s.importError(w, r, err)
		return
	}

	writeJSON(w, http.StatusOK, traceStatus(account, st, time.Now()))
}

// SetTrace switches the trace of an account on.
func (s *Server) SetTrace(w http.ResponseWriter, r *http.Request, account adminapi.Account) {
	u, t, ok := s.beginTrace(w, r, account)
	if !ok {
		return
	}
	defer u.Close()

	var req adminapi.TraceRequest
	if err := decodeOptionalJSON(r, &req); err != nil || (req.DurationSeconds != nil && *req.DurationSeconds < 1) {
		writeError(w, http.StatusBadRequest, adminapi.BadRequest, "invalid request, duration_seconds must be positive")
		return
	}

	d := s.o.TraceDefaultDuration

	if req.DurationSeconds != nil {
		// Capped before the multiplication, which could overflow.
		d = time.Duration(min(int64(*req.DurationSeconds), int64(s.o.TraceMaxDuration/time.Second))) * time.Second
	}

	now := time.Now()
	st := model.TraceState{Until: now.Add(min(d, s.o.TraceMaxDuration)), By: principalOf(r.Context()).name, SetAt: now}

	if err := t.SetTrace(st); err != nil {
		s.importError(w, r, err)
		return
	}

	auditf(r, "trace_until", st.Until.UTC().Format(time.RFC3339))
	writeJSON(w, http.StatusOK, traceStatus(account, st, now))
}

// ClearTrace switches the trace of an account off.
func (s *Server) ClearTrace(w http.ResponseWriter, r *http.Request, account adminapi.Account) {
	u, t, ok := s.beginTrace(w, r, account)
	if !ok {
		return
	}
	defer u.Close()

	if err := t.SetTrace(model.TraceState{}); err != nil {
		s.importError(w, r, err)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// traceStatus is the API view of a switch.
func traceStatus(account string, st model.TraceState, now time.Time) adminapi.TraceStatus {
	out := adminapi.TraceStatus{Account: account, Active: st.Active(now)}
	if !out.Active {
		return out
	}

	until := st.Until.UTC()
	out.Until = &until

	if st.By != "" {
		out.SetBy = &st.By
	}

	if !st.SetAt.IsZero() {
		at := st.SetAt.UTC()
		out.SetAt = &at
	}

	return out
}
