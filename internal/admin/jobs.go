package admin

import (
	"encoding/json"
	"errors"
	"net/http"

	"shardpost/internal/adminapi"
	"shardpost/internal/jobs"
	"shardpost/internal/model"
)

// Maintenance jobs (roadmap 8c): plans, jobs, status, cancel.

// Roles of the job kinds.
const (
	RoleRepair = jobs.RoleRepair
	RoleDelete = jobs.RoleDelete
)

// Job list page sizes.
const (
	defaultJobPage = 50
	maxJobPage     = 500
)

// errScope refuses a plan that changes accounts outside the client scope.
var errScope = errors.New("the plan changes accounts outside the client's scope")

// jobsEnabled answers 503 when this server has no job system.
func (s *Server) jobsEnabled(w http.ResponseWriter) bool {
	if s.o.Jobs == nil {
		writeError(w, http.StatusServiceUnavailable, adminapi.Temporary, "jobs are not enabled")
		return false
	}

	return true
}

// jobError maps a failed job operation to the error document.
func (s *Server) jobError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, jobs.ErrNotFound):
		writeError(w, http.StatusNotFound, adminapi.NoSuchJob, err.Error())
	case errors.Is(err, jobs.ErrPlanExpired), errors.Is(err, jobs.ErrPlanUsed), errors.Is(err, jobs.ErrIdempotency),
		errors.Is(err, jobs.ErrNotCancelled):
		writeError(w, http.StatusConflict, adminapi.Conflict, err.Error())
	case errors.Is(err, jobs.ErrUnknownKind), errors.Is(err, jobs.ErrNeedsPlan), errors.Is(err, jobs.ErrNoPlan),
		errors.Is(err, jobs.ErrInvalidParams):
		writeError(w, http.StatusBadRequest, adminapi.BadRequest, err.Error())
	case errors.Is(err, errScope):
		writeError(w, http.StatusForbidden, adminapi.Forbidden, err.Error())
	case errors.Is(err, jobs.ErrDirectoryNotReady), errors.Is(err, model.ErrFTSLayoutMark), errors.Is(err, model.ErrFTSDictLayout3),
		errors.Is(err, model.ErrFTSDictFilterMark):
		writeError(w, http.StatusConflict, adminapi.Conflict, err.Error())
	default:
		s.importError(w, r, err)
	}
}

// allowTouched refuses other accounts outside the client scope.
func (p *principal) allowTouched(touched []string) error {
	for _, a := range touched {
		if !p.inScope(a) {
			return errScope
		}
	}

	return nil
}

// CreatePlan computes and stores a plan.
func (s *Server) CreatePlan(w http.ResponseWriter, r *http.Request, account adminapi.Account) {
	auditf(r, "account", account)

	if !s.jobsEnabled(w) {
		return
	}

	var req adminapi.PlanRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, adminapi.BadRequest, "invalid request")
		return
	}

	kind, err := s.o.Jobs.Kind(string(req.Kind))
	if err != nil {
		s.jobError(w, r, err)
		return
	}

	auditf(r, "kind", kind.Name())

	u, ok := s.openAccount(w, r, kind.Role(), account, true)
	if !ok {
		return
	}

	u.Close()

	params, _ := json.Marshal(req.Params)
	if req.Params == nil {
		params = nil
	}

	p := principalOf(r.Context())

	plan, err := s.o.Jobs.CreatePlan(r.Context(), kind, account, params, p.name, p.allowTouched)
	if err != nil {
		s.jobError(w, r, err)
		return
	}

	auditf(r, "plan", plan.ID)
	writeJSON(w, http.StatusCreated, apiPlan(plan))
}

// CreateBatchPlan plans a batch: a kind over every account of the
// account directory. It needs the role of that kind and the scope of
// all accounts.
func (s *Server) CreateBatchPlan(w http.ResponseWriter, r *http.Request) {
	if !s.jobsEnabled(w) {
		return
	}

	var req adminapi.BatchPlanRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, adminapi.BadRequest, "invalid request")
		return
	}

	kind, err := s.o.Jobs.Kind(string(req.Kind))
	if err != nil {
		s.jobError(w, r, err)
		return
	}

	auditf(r, "kind", jobs.KindBatch, "runs", kind.Name())

	if !requireAllAccounts(w, r, kind.Role()) {
		return
	}

	p := jobs.BatchParams{Kind: kind.Name(), SkipIdle: req.SkipIdle != nil && *req.SkipIdle}
	if req.Params != nil {
		p.Params, _ = json.Marshal(req.Params)
	}

	plan, err := s.o.Jobs.CreateBatchPlan(p, principalOf(r.Context()).name)
	if err != nil {
		s.jobError(w, r, err)
		return
	}

	auditf(r, "plan", plan.ID)
	writeJSON(w, http.StatusCreated, apiPlan(plan))
}

// GetPlan returns a plan.
func (s *Server) GetPlan(w http.ResponseWriter, r *http.Request, id string) {
	auditf(r, "plan", id)

	if !s.jobsEnabled(w) || !require(w, r, RoleDiagnose) {
		return
	}

	plan, err := s.o.Jobs.GetPlan(id)
	if err != nil {
		s.jobError(w, r, err)
		return
	}

	if !principalOf(r.Context()).inScope(plan.Account) {
		s.jobError(w, r, jobs.ErrNotFound)
		return
	}

	writeJSON(w, http.StatusOK, apiPlan(plan))
}

// SubmitJob applies a plan or starts a check.
func (s *Server) SubmitJob(w http.ResponseWriter, r *http.Request, params adminapi.SubmitJobParams) {
	if !s.jobsEnabled(w) {
		return
	}

	var req adminapi.JobRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, adminapi.BadRequest, "invalid request")
		return
	}

	kind, account, planID, ok := s.jobTarget(w, r, req)
	if !ok {
		return
	}

	j, created, err := s.o.Jobs.Submit(kind, account, nil, planID, principalOf(r.Context()).name, deref(params.IdempotencyKey))
	if err != nil {
		s.jobError(w, r, err)
		return
	}

	auditf(r, "job", j.ID, "kind", j.Kind, "plan", planID)

	status := http.StatusAccepted
	if !created {
		status = http.StatusOK
	}

	writeJSON(w, status, apiJob(j))
}

// jobTarget resolves kind, account and plan of a submit and checks role
// and scope; ok false means the response is written.
func (s *Server) jobTarget(w http.ResponseWriter, r *http.Request, req adminapi.JobRequest) (jobs.Kind, string, string, bool) {
	if planID := deref(req.PlanId); planID != "" {
		plan, err := s.o.Jobs.GetPlan(planID)
		if err != nil {
			s.jobError(w, r, err)
			return nil, "", "", false
		}

		kind, err := s.o.Jobs.Kind(plan.Kind)
		if err != nil {
			s.jobError(w, r, err)
			return nil, "", "", false
		}

		auditf(r, "account", plan.Account)

		if !s.requireJobScope(w, r, kind, plan.Params, plan.Account) {
			return nil, "", "", false
		}

		// The scope may differ from the one the plan was made under.
		if err := principalOf(r.Context()).allowTouched(plan.Touched); err != nil {
			s.jobError(w, r, err)
			return nil, "", "", false
		}

		return kind, plan.Account, planID, true
	}

	if req.Kind == nil || req.Account == nil {
		writeError(w, http.StatusBadRequest, adminapi.BadRequest, "plan_id, or kind and account required")
		return nil, "", "", false
	}

	kind, err := s.o.Jobs.Kind(string(*req.Kind))
	if err != nil {
		s.jobError(w, r, err)
		return nil, "", "", false
	}

	u, ok := s.openAccount(w, r, kind.Role(), *req.Account, true)
	if !ok {
		return nil, "", "", false
	}

	u.Close()

	return kind, *req.Account, "", true
}

// ListJobs lists jobs.
func (s *Server) ListJobs(w http.ResponseWriter, r *http.Request, p adminapi.ListJobsParams) {
	if !s.jobsEnabled(w) || !require(w, r, RoleDiagnose) {
		return
	}

	account, state, limit, ok := jobListParams(w, r, p)
	if !ok {
		return
	}

	list, next, err := s.o.Jobs.List(account, state, deref(p.After), limit)
	if err != nil {
		s.jobError(w, r, err)
		return
	}

	page := adminapi.JobPage{Items: []adminapi.Job{}, Next: optional(next)}
	for i := range list {
		page.Items = append(page.Items, apiJob(&list[i]))
	}

	writeJSON(w, http.StatusOK, page)
}

// jobListParams checks the scope and the paging of a job listing; ok
// false means the response is written.
func jobListParams(w http.ResponseWriter, r *http.Request, p adminapi.ListJobsParams) (account, state string, limit int, ok bool) {
	account, limit = deref(p.Account), defaultJobPage
	principal := principalOf(r.Context())

	if p.Limit != nil {
		limit = *p.Limit
	}

	if p.State != nil {
		state = string(*p.State)
	}

	switch {
	case account == "" && len(principal.accounts) > 0:
		writeError(w, http.StatusForbidden, adminapi.Forbidden, "a client with an account scope must name an account")
	case account != "" && !principal.inScope(account):
		writeError(w, http.StatusForbidden, adminapi.Forbidden, "account outside the client's scope")
	case limit < 1 || limit > maxJobPage:
		writeError(w, http.StatusBadRequest, adminapi.BadRequest, "limit must be 1 to 500")
	default:
		return account, state, limit, true
	}

	return "", "", 0, false
}

// scopedJob loads a job and checks the role and the client scope.
func (s *Server) scopedJob(w http.ResponseWriter, r *http.Request, id string, role func(*jobs.Job) string) (*jobs.Job, bool) {
	auditf(r, "job", id)

	if !s.jobsEnabled(w) {
		return nil, false
	}

	j, err := s.o.Jobs.Get(id)
	if err != nil {
		if !require(w, r, RoleDiagnose) {
			return nil, false
		}

		s.jobError(w, r, err)

		return nil, false
	}

	// A job outside the scope does not exist for this client.
	if !principalOf(r.Context()).inScope(j.Account) {
		s.jobError(w, r, jobs.ErrNotFound)
		return nil, false
	}

	if j.Account == "" && role(j) != RoleDiagnose {
		// Changing a batch takes the scope of all accounts.
		return j, requireAllAccounts(w, r, role(j))
	}

	return j, requireAccount(w, r, role(j), j.Account)
}

// requireJobScope checks the role a job of the kind needs and the client
// scope: the account, or for a batch the scope of all accounts.
func (s *Server) requireJobScope(w http.ResponseWriter, r *http.Request, kind jobs.Kind, params json.RawMessage, account string) bool {
	role := s.o.Jobs.RoleOf(kind, params)

	if jobs.IsBatch(kind) {
		return requireAllAccounts(w, r, role)
	}

	return requireAccount(w, r, role, account)
}

// GetJob returns a job.
func (s *Server) GetJob(w http.ResponseWriter, r *http.Request, id adminapi.JobID) {
	j, ok := s.scopedJob(w, r, id, func(*jobs.Job) string { return RoleDiagnose })
	if ok {
		writeJSON(w, http.StatusOK, apiJob(j))
	}
}

// CancelJob asks a job to stop.
func (s *Server) CancelJob(w http.ResponseWriter, r *http.Request, id adminapi.JobID) {
	j, ok := s.scopedJob(w, r, id, func(j *jobs.Job) string {
		if k, err := s.o.Jobs.Kind(j.Kind); err == nil {
			return s.o.Jobs.RoleOf(k, j.Params)
		}

		return RoleDelete
	})
	if !ok {
		return
	}

	j, err := s.o.Jobs.Cancel(j.ID)
	if err != nil {
		s.jobError(w, r, err)
		return
	}

	writeJSON(w, http.StatusOK, apiJob(j))
}

// apiJob is the API view of a job (without its checkpoint).
func apiJob(j *jobs.Job) adminapi.Job {
	out := adminapi.Job{Id: j.ID, Kind: j.Kind, Account: j.Account, State: adminapi.JobState(j.State), Principal: j.Principal,
		Created: j.Created.UTC(), Attempts: j.Attempts, Errors: j.Errors,
		Progress: adminapi.JobProgress{Phase: optional(j.Progress.Phase), Done: j.Progress.Done, Total: j.Progress.Total}}

	if out.Errors == nil {
		out.Errors = []string{}
	}

	out.PlanId = optional(j.PlanID)
	out.Server = optional(j.Lease.Owner)
	out.Result = object(j.Result)

	if !j.Started.IsZero() {
		t := j.Started.UTC()
		out.Started = &t
	}

	if !j.Finished.IsZero() {
		t := j.Finished.UTC()
		out.Finished = &t
	}

	return out
}

// apiPlan is the API view of a plan.
func apiPlan(p *jobs.Plan) adminapi.Plan {
	out := adminapi.Plan{Id: p.ID, Kind: p.Kind, Account: p.Account, Principal: p.Principal, Created: p.Created.UTC(),
		Expires: p.Expires.UTC(), AppliedJob: optional(p.AppliedJob), Params: object(p.Params), Summary: map[string]any{}}

	if s := object(p.Summary); s != nil {
		out.Summary = *s
	}

	return out
}

// object decodes a JSON object ("value" wraps anything else); nil for
// none.
func object(raw json.RawMessage) *map[string]any {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}

	var m map[string]any
	if json.Unmarshal(raw, &m) == nil {
		return &m
	}

	var v any

	_ = json.Unmarshal(raw, &v)

	return &map[string]any{"value": v}
}

// optional returns a pointer to s, nil for "".
func optional(s string) *string {
	if s == "" {
		return nil
	}

	return &s
}
