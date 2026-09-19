package admin

import (
	"errors"
	"net/http"
	"sort"
	"strconv"

	"shardpost/internal/adminapi"
	"shardpost/internal/imapsieve"
)

// Learning queue diagnosis and retry (roadmap 8d).

// LearningAdmin is what the admin API needs from the learning queue.
type LearningAdmin interface {
	All() ([]imapsieve.Job, error)
	Ready(imapsieve.Job) (bool, error)
	Revive(id string) (imapsieve.Job, error)
}

// Learning job states.
const (
	learningPending   = "pending"
	learningDead      = "dead"
	learningDelivered = "delivered"
)

func learningState(j *imapsieve.Job) string {
	switch {
	case j.Delivered:
		return learningDelivered
	case j.Dead:
		return learningDead
	default:
		return learningPending
	}
}

func apiLearningJob(j *imapsieve.Job) adminapi.LearningJob {
	out := adminapi.LearningJob{Id: j.ID, Account: j.Owner, MailboxId: j.MailboxID, Program: j.Action,
		State: adminapi.LearningState(learningState(j)), Attempts: j.Attempts, Retries: j.Retries, Created: j.Created.UTC(),
		NextAttempt: j.Next.UTC(), Size: j.Size}

	if j.Prepared {
		out.Prepared = &j.Prepared
	}

	return out
}

// learningEnabled answers 503 without a learning queue.
func (s *Server) learningEnabled(w http.ResponseWriter) bool {
	if s.o.Learning == nil {
		writeError(w, http.StatusServiceUnavailable, adminapi.Temporary, "the learning queue is not available")
		return false
	}

	return true
}

// ListLearningJobs lists the learning queue by job id.
func (s *Server) ListLearningJobs(w http.ResponseWriter, r *http.Request, p adminapi.ListLearningJobsParams) {
	if !s.learningEnabled(w) || !require(w, r, RoleDiagnose) {
		return
	}

	account, state, limit, ok := jobListParams(w, r, adminapi.ListJobsParams{Account: p.Account, Limit: p.Limit})
	if !ok {
		return
	}

	if p.State != nil {
		state = string(*p.State)
	}

	all, err := s.o.Learning.All()
	if err != nil {
		s.importError(w, r, err)
		return
	}

	writeJSON(w, http.StatusOK, learningPage(all, account, state, deref(p.After), limit))
}

// learningPage returns the jobs after the id matching account and state,
// by id.
func learningPage(all []imapsieve.Job, account, state, after string, limit int) adminapi.LearningPage {
	sort.Slice(all, func(i, j int) bool { return all[i].ID < all[j].ID })

	page := adminapi.LearningPage{Items: []adminapi.LearningJob{}}

	for i := range all {
		j := &all[i]
		if j.ID <= after || (account != "" && j.Owner != account) || (state != "" && learningState(j) != state) {
			continue
		}

		if len(page.Items) == limit {
			page.Next = optional(page.Items[limit-1].Id)
			break
		}

		page.Items = append(page.Items, apiLearningJob(j))
	}

	return page
}

// learningJob finds a job in the client's scope; ok false means the
// response is written.
func (s *Server) learningJob(w http.ResponseWriter, r *http.Request, id, role string) (*imapsieve.Job, bool) {
	auditf(r, "learning_job", id)

	if !s.learningEnabled(w) || !require(w, r, role) {
		return nil, false
	}

	all, err := s.o.Learning.All()
	if err != nil {
		s.importError(w, r, err)
		return nil, false
	}

	for i := range all {
		if all[i].ID == id && principalOf(r.Context()).inScope(all[i].Owner) {
			auditf(r, "account", all[i].Owner)
			return &all[i], true
		}
	}

	writeError(w, http.StatusNotFound, adminapi.NoSuchJob, "no such learning job")

	return nil, false
}

// GetLearningJob shows one learning job.
func (s *Server) GetLearningJob(w http.ResponseWriter, r *http.Request, id adminapi.JobID) {
	if j, ok := s.learningJob(w, r, id, RoleDiagnose); ok {
		writeJSON(w, http.StatusOK, apiLearningJob(j))
	}
}

// RetryLearningJob makes a dead learning job due again. Learning
// programs are not idempotent: a job that was attempted before needs
// confirm_repeat, and a job whose message was never committed cannot be
// retried.
func (s *Server) RetryLearningJob(w http.ResponseWriter, r *http.Request, id adminapi.JobID) {
	var req adminapi.LearningRetry
	if err := decodeOptionalJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, adminapi.BadRequest, "invalid request")
		return
	}

	j, ok := s.learningJob(w, r, id, RoleRepair)
	if !ok {
		return
	}

	confirm := req.ConfirmRepeat != nil && *req.ConfirmRepeat
	auditf(r, "attempts", j.Attempts, "confirm_repeat", confirm)

	if msg := s.retryRefusal(j, confirm); msg != "" {
		writeError(w, http.StatusConflict, adminapi.Conflict, msg)
		return
	}

	revived, err := s.o.Learning.Revive(j.ID)

	switch {
	case errors.Is(err, imapsieve.ErrNoJob):
		writeError(w, http.StatusNotFound, adminapi.NoSuchJob, "no such learning job")
	case errors.Is(err, imapsieve.ErrNotDead):
		writeError(w, http.StatusConflict, adminapi.Conflict, err.Error())
	case err != nil:
		s.importError(w, r, err)
	default:
		writeJSON(w, http.StatusOK, apiLearningJob(&revived))
	}
}

// retryRefusal explains why a job cannot be retried ("" = it can).
func (s *Server) retryRefusal(j *imapsieve.Job, confirm bool) string {
	if !j.Dead {
		return imapsieve.ErrNotDead.Error()
	}

	if j.Prepared {
		if ready, err := s.o.Learning.Ready(*j); err != nil || !ready {
			return "the message was never committed (no receipt): there is nothing to learn"
		}
	}

	if (j.Attempts > 0 || j.Retries > 0) && !confirm {
		return "the program may have received this message already (" + strconv.Itoa(j.Attempts) +
			" attempts); learning is not idempotent, repeat with confirm_repeat"
	}

	return ""
}
