package admin

import (
	"errors"
	"net/http"
	"sort"

	"shardpost/internal/adminapi"
	"shardpost/internal/sendqueue"
)

// Delayed JMAP submissions (scaling plan B10): list and cancel the
// pending submissions of an account.

// SubmissionAdmin is what the admin API needs from the delayed
// submission queue (sendqueue.Service).
type SubmissionAdmin interface {
	List(account string) ([]*sendqueue.Record, error)
	Cancel(account, id, by string) (*sendqueue.Record, error)
}

// submissionsEnabled answers 503 without a submission queue.
func (s *Server) submissionsEnabled(w http.ResponseWriter) bool {
	if s.o.Submissions == nil {
		writeError(w, http.StatusServiceUnavailable, adminapi.Temporary, "the submission queue is not available (admin.kv_index off)")
		return false
	}

	return true
}

// ListSubmissions lists an account's delayed submissions.
func (s *Server) ListSubmissions(w http.ResponseWriter, r *http.Request, account adminapi.Account) {
	auditf(r, "account", account)

	if !requireAccount(w, r, RoleDiagnose, account) || !s.submissionsEnabled(w) {
		return
	}

	if !validAccount(account) {
		writeError(w, http.StatusBadRequest, adminapi.BadRequest, "invalid account")
		return
	}

	list, err := s.o.Submissions.List(account)
	if err != nil {
		s.importError(w, r, err)
		return
	}

	sort.Slice(list, func(i, j int) bool {
		if !list[i].Release.Equal(list[j].Release) {
			return list[i].Release.Before(list[j].Release)
		}

		return list[i].ID < list[j].ID
	})

	out := adminapi.SubmissionList{Items: make([]adminapi.Submission, 0, len(list))}
	for _, rec := range list {
		out.Items = append(out.Items, apiSubmission(rec))
	}

	writeJSON(w, http.StatusOK, out)
}

// CancelSubmission cancels a pending delayed submission.
func (s *Server) CancelSubmission(w http.ResponseWriter, r *http.Request, account adminapi.Account, id adminapi.SubmissionID) {
	auditf(r, "account", account, "submission", id)

	if !requireAccount(w, r, RoleSupport, account) || !s.submissionsEnabled(w) {
		return
	}

	if !validAccount(account) || !sendqueue.ValidID(id) {
		writeError(w, http.StatusBadRequest, adminapi.BadRequest, "invalid account or submission id")
		return
	}

	rec, err := s.o.Submissions.Cancel(account, id, "admin:"+principalOf(r.Context()).name)

	var np *sendqueue.NotPendingError

	switch {
	case errors.Is(err, sendqueue.ErrNotFound):
		writeError(w, http.StatusNotFound, adminapi.NoSuchSubmission, "no such delayed submission (it may have ended)")
	case errors.As(err, &np):
		writeError(w, http.StatusConflict, adminapi.Conflict, err.Error())
	case err != nil:
		s.importError(w, r, err)
	default:
		writeJSON(w, http.StatusOK, apiSubmission(rec))
	}
}

// apiSubmission is a record as the API shows it.
func apiSubmission(rec *sendqueue.Record) adminapi.Submission {
	out := adminapi.Submission{Id: rec.ID, Account: rec.Account, State: adminapi.SubmissionState(rec.State),
		Release: rec.Release.UTC(), Due: rec.Due.UTC(), Created: rec.Created.UTC(), Attempts: rec.Attempts, Size: rec.Size,
		MailFrom: rec.MailFrom, RcptTo: append([]string{}, rec.RcptTo...)}

	if !rec.SendingSince.IsZero() {
		t := rec.SendingSince.UTC()
		out.SendingSince = &t
	}

	if !rec.Finished.IsZero() {
		t := rec.Finished.UTC()
		out.Finished = &t
	}

	if rec.Unknown {
		out.Unknown = &rec.Unknown
	}

	out.Reason = optional(rec.Reason)
	out.CanceledBy = optional(rec.CanceledBy)

	return out
}
