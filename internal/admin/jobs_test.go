package admin

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"shardpost/internal/adminapi"
	"shardpost/internal/jobs"
	"shardpost/internal/memstore"
	"shardpost/internal/model"
)

// waitJob polls the API until the job ends.
func waitJobAPI(t *testing.T, c *adminapi.ClientWithResponses, id string) *adminapi.Job {
	t.Helper()

	deadline := time.Now().Add(5 * time.Second)

	for time.Now().Before(deadline) {
		resp, _ := c.GetJobWithResponse(context.Background(), id)
		if j := resp.JSON200; j != nil {
			switch j.State {
			case adminapi.Succeeded, adminapi.PartiallyFailed, adminapi.Failed, adminapi.Cancelled:
				return j
			}
		}

		time.Sleep(10 * time.Millisecond)
	}

	t.Fatalf("job %s did not end", id)

	return nil
}

func TestPlanApplyMailboxDelete(t *testing.T) {
	store, c := serveDiag(t, nil, RoleDiagnose, RoleDelete)
	ctx := context.Background()
	id := mailboxID(t, c, "A")

	params := map[string]any{"mailbox_id": id}

	plan, _ := c.CreatePlanWithResponse(ctx, "alice", adminapi.PlanRequest{Kind: adminapi.PlanRequestKind(jobs.KindMailboxDelete), Params: &params})
	if plan.JSON201 == nil || plan.JSON201.Summary["mailboxes"] == nil {
		t.Fatalf("plan: %d %v", plan.StatusCode(), plan.JSONDefault)
	}

	key := "apply-once-123"

	resp, _ := c.SubmitJobWithResponse(ctx, &adminapi.SubmitJobParams{IdempotencyKey: &key}, adminapi.JobRequest{PlanId: &plan.JSON201.Id})
	if resp.JSON202 == nil {
		t.Fatalf("apply: %d %v", resp.StatusCode(), resp.JSONDefault)
	}

	// The same request again: the same job, 200.
	again, _ := c.SubmitJobWithResponse(ctx, &adminapi.SubmitJobParams{IdempotencyKey: &key}, adminapi.JobRequest{PlanId: &plan.JSON201.Id})
	if again.JSON200 == nil || again.JSON200.Id != resp.JSON202.Id {
		t.Fatalf("repeat: %d %v", again.StatusCode(), again.JSONDefault)
	}

	// Without the key the plan is used up.
	if used, _ := c.SubmitJobWithResponse(ctx, nil, adminapi.JobRequest{PlanId: &plan.JSON201.Id}); used.StatusCode() != http.StatusConflict {
		t.Fatalf("second apply: %d", used.StatusCode())
	}

	checkDeleted(t, c, store, resp.JSON202.Id)
}

// checkDeleted waits for the delete job and checks its effects.
func checkDeleted(t *testing.T, c *adminapi.ClientWithResponses, store *memstore.Store, jobID string) {
	t.Helper()

	ctx := context.Background()

	if j := waitJobAPI(t, c, jobID); j.State != adminapi.Succeeded || deref(j.Server) != "pod-1" {
		t.Fatalf("job: %+v", j)
	}

	u, _ := store.User("alice")
	if _, err := u.Lookup("A"); err == nil {
		t.Fatal("mailbox still there")
	}

	list, _ := c.ListJobsWithResponse(ctx, &adminapi.ListJobsParams{Account: ptr("alice")})
	if list.JSON200 == nil || len(list.JSON200.Items) != 1 {
		t.Fatalf("list: %+v", list.JSON200)
	}

	if cancel, _ := c.CancelJobWithResponse(ctx, jobID); cancel.StatusCode() != http.StatusConflict {
		t.Fatalf("cancel finished: %d", cancel.StatusCode())
	}
}

func TestJobRolesAndErrors(t *testing.T) {
	_, c := serveDiag(t, nil, RoleDiagnose)
	ctx := context.Background()

	// Deleting needs the role delete.
	if resp, _ := c.CreatePlanWithResponse(ctx, "alice", adminapi.PlanRequest{Kind: adminapi.PlanRequestKind(jobs.KindPurge)}); resp.StatusCode() != http.StatusForbidden {
		t.Fatalf("purge without role: %d", resp.StatusCode())
	}

	for name, req := range map[string]adminapi.PlanRequest{
		"unknown kind":  {Kind: "frobnicate"},
		"check planned": {Kind: adminapi.PlanRequestKind(jobs.KindIndexCheck)},
	} {
		if resp, _ := c.CreatePlanWithResponse(ctx, "alice", req); resp.StatusCode() != http.StatusBadRequest && resp.StatusCode() != http.StatusForbidden {
			t.Errorf("%s: %d", name, resp.StatusCode())
		}
	}

	kind := adminapi.IndexCheck

	resp, _ := c.SubmitJobWithResponse(ctx, nil, adminapi.JobRequest{Kind: &kind, Account: ptr("alice")})
	if resp.JSON202 == nil {
		t.Fatalf("check: %d %v", resp.StatusCode(), resp.JSONDefault)
	}

	if j := waitJobAPI(t, c, resp.JSON202.Id); j.State != adminapi.Succeeded || j.Result == nil {
		t.Fatalf("check: %+v", j)
	}

	if missing, _ := c.GetJobWithResponse(ctx, "nope"); missing.StatusCode() != http.StatusNotFound {
		t.Fatalf("unknown job: %d", missing.StatusCode())
	}

	if bad, _ := c.SubmitJobWithResponse(ctx, nil, adminapi.JobRequest{}); bad.StatusCode() != http.StatusBadRequest {
		t.Fatalf("empty request: %d", bad.StatusCode())
	}
}

// TestJobErrorLayoutRefusals: a plan refused for the layout (fts_layout 3
// without the cluster mark, fts_dict_convert on a layout 3 account) is a
// conflict the caller can act on, not a temporary failure.
func TestJobErrorLayoutRefusals(t *testing.T) {
	s := &Server{}

	for _, err := range []error{model.ErrFTSLayoutMark, fmt.Errorf("plan: %w", model.ErrFTSDictLayout3)} {
		rec := httptest.NewRecorder()
		s.jobError(rec, httptest.NewRequest(http.MethodPost, "/admin/v1/accounts/alice/plans", nil), err)

		if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), string(adminapi.Conflict)) {
			t.Errorf("%v: %d %s", err, rec.Code, rec.Body.String())
		}
	}
}
