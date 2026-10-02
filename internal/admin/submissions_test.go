package admin

import (
	"context"
	"net/http"
	"testing"
	"time"

	"shardpost/internal/adminapi"
	"shardpost/internal/memstore"
	"shardpost/internal/sendqueue"
)

// testSubmissions is the submission queue of serveDiag (nil: none).
var testSubmissions SubmissionAdmin

// withSubmissions gives serveDiag a submission queue with two pending
// submissions of alice.
func withSubmissions(t *testing.T) []*sendqueue.Record {
	t.Helper()

	store := memstore.New()

	q, err := sendqueue.New(sendqueue.Options{KV: memstore.NewAdminKV(), Accounts: store})
	if err != nil {
		t.Fatal(err)
	}

	u, err := store.User("alice")
	if err != nil {
		t.Fatal(err)
	}

	var recs []*sendqueue.Record

	for i, id := range []string{"S00000000000000000000000b", "S00000000000000000000000a"} {
		rec, err := q.Schedule(u, sendqueue.Spec{Account: "alice", ID: id, Release: time.Now().Add(time.Duration(i+1) * time.Hour),
			MailFrom: "alice@example.org", RcptTo: []string{"bob@example.net"}, Raw: []byte("x\r\n")})
		if err != nil {
			t.Fatal(err)
		}

		recs = append(recs, rec)
	}

	testSubmissions = q

	t.Cleanup(func() { testSubmissions = nil })

	return recs
}

// TestSubmissions: listing needs diagnose, canceling support; a cancel
// turns the submission canceled and a second one finds nothing.
func TestSubmissions(t *testing.T) {
	recs := withSubmissions(t)
	_, c := serveDiag(t, nil, RoleDiagnose, RoleSupport)
	ctx := context.Background()

	checkSubmissionList(t, c, recs)

	res, err := c.CancelSubmissionWithResponse(ctx, "alice", recs[0].ID)
	if err != nil || res.JSON200 == nil || res.JSON200.State != adminapi.SubmissionCanceled ||
		res.JSON200.CanceledBy == nil || *res.JSON200.CanceledBy == "" {
		t.Fatalf("cancel: %v %s", err, res.Body)
	}

	res, _ = c.CancelSubmissionWithResponse(ctx, "alice", recs[0].ID)
	if res.StatusCode() != http.StatusNotFound || res.JSONDefault.Code != adminapi.NoSuchSubmission {
		t.Errorf("second cancel %d %s", res.StatusCode(), res.Body)
	}

	if list, _ := c.ListSubmissionsWithResponse(ctx, "alice"); len(list.JSON200.Items) != 1 {
		t.Errorf("after the cancel %s", list.Body)
	}

	res, _ = c.CancelSubmissionWithResponse(ctx, "alice", "nope")
	if res.StatusCode() != http.StatusBadRequest {
		t.Errorf("a bad id %d", res.StatusCode())
	}
}

// checkSubmissionList lists alice's two submissions, by release.
func checkSubmissionList(t *testing.T, c *adminapi.ClientWithResponses, recs []*sendqueue.Record) {
	t.Helper()

	list, err := c.ListSubmissionsWithResponse(context.Background(), "alice")
	if err != nil || list.JSON200 == nil || len(list.JSON200.Items) != 2 {
		t.Fatalf("list: %v %s", err, list.Body)
	}

	if first := list.JSON200.Items[0]; first.Id != recs[0].ID || first.State != adminapi.SubmissionPending {
		t.Errorf("first %#v", first)
	}
}

// TestListAllSubmissions: every record, paged by key.
func TestListAllSubmissions(t *testing.T) {
	withSubmissions(t)
	_, c := serveDiag(t, nil, RoleDiagnose)
	ctx := context.Background()
	limit := 1

	first, err := c.ListAllSubmissionsWithResponse(ctx, &adminapi.ListAllSubmissionsParams{Limit: &limit})
	if err != nil || first.JSON200 == nil || len(first.JSON200.Items) != 1 || first.JSON200.Next == nil {
		t.Fatalf("first page: %v %s", err, first.Body)
	}

	rest, err := c.ListAllSubmissionsWithResponse(ctx, &adminapi.ListAllSubmissionsParams{After: first.JSON200.Next})
	if err != nil || rest.JSON200 == nil || len(rest.JSON200.Items) != 1 || rest.JSON200.Next != nil ||
		rest.JSON200.Items[0].Id == first.JSON200.Items[0].Id {
		t.Fatalf("second page: %v %s", err, rest.Body)
	}
}

// TestSubmissionsRoles: without support no cancel; without a queue 503.
func TestSubmissionsRoles(t *testing.T) {
	recs := withSubmissions(t)
	_, c := serveDiag(t, nil, RoleDiagnose)
	ctx := context.Background()

	if res, _ := c.CancelSubmissionWithResponse(ctx, "alice", recs[0].ID); res.StatusCode() != http.StatusForbidden {
		t.Errorf("cancel without support %d", res.StatusCode())
	}

	testSubmissions = nil
	_, c = serveDiag(t, nil, RoleDiagnose)

	if res, _ := c.ListSubmissionsWithResponse(ctx, "alice"); res.StatusCode() != http.StatusServiceUnavailable {
		t.Errorf("list without a queue %d", res.StatusCode())
	}
}
