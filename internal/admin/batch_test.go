package admin

import (
	"context"
	"crypto/x509"
	"io"
	"log/slog"
	"net"
	"net/http"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"shardpost/internal/adminapi"
	"shardpost/internal/delivery"
	"shardpost/internal/jobs"
	"shardpost/internal/memstore"
	"shardpost/internal/model"
)

// registerAccounts gives each account an INBOX (a registry, so a
// directory entry) and marks the directory backfilled.
func registerAccounts(t *testing.T, store *memstore.Store, names ...string) {
	t.Helper()

	for _, name := range names {
		u, _ := store.User(name)
		if err := u.EnsureMailbox("INBOX", nil); err != nil {
			t.Fatal(err)
		}
	}

	ix, release, _ := store.AccountIndex()
	defer release()

	if err := ix.SetMarker(model.AccountBackfillMarker, `{"finished":"2026-10-01T00:00:00Z"}`); err != nil {
		t.Fatal(err)
	}
}

// listAllAccounts pages through the directory.
func listAllAccounts(t *testing.T, c *adminapi.ClientWithResponses, limit int, state string) []string {
	t.Helper()

	var (
		out    []string
		cursor *string
	)

	for range 100 {
		p := &adminapi.ListAccountsParams{Cursor: cursor, Limit: &limit}
		if state != "" {
			p.State = &state
		}

		resp, err := c.ListAccountsWithResponse(context.Background(), p)
		if err != nil || resp.JSON200 == nil {
			t.Fatalf("list: %v %d", err, resp.StatusCode())
		}

		if len(resp.JSON200.Items) > limit {
			t.Fatalf("page of %d items, limit %d", len(resp.JSON200.Items), limit)
		}

		for _, it := range resp.JSON200.Items {
			out = append(out, it.Account)
		}

		if resp.JSON200.Next == nil {
			slices.Sort(out)
			return out
		}

		cursor = resp.JSON200.Next
	}

	t.Fatal("listing does not end")

	return nil
}

func TestListAccounts(t *testing.T) {
	store, c := serveDiag(t, nil, RoleDiagnose)
	registerAccounts(t, store, "bob", "carol", "dave", "erin", "frank")

	want := []string{"alice", "bob", "carol", "dave", "erin", "frank"}

	if got := listAllAccounts(t, c, 2, ""); !slices.Equal(got, want) {
		t.Fatalf("accounts %v", got)
	}

	ix, release, _ := store.AccountIndex()
	_ = model.MarkAccountPurging(ix, "dave", "job-x", "", time.Now())

	release()

	if got := listAllAccounts(t, c, 1, model.AccountPurging); !slices.Equal(got, []string{"dave"}) {
		t.Fatalf("purging %v", got)
	}

	if got := listAllAccounts(t, c, 3, model.AccountActive); len(got) != len(want)-1 || slices.Contains(got, "dave") {
		t.Fatalf("active %v", got)
	}

	ctx := context.Background()
	bad, zero, state := "x:y", 0, "gone"

	for name, p := range map[string]*adminapi.ListAccountsParams{
		"cursor": {Cursor: &bad}, "limit": {Limit: &zero}, "state": {State: &state},
	} {
		if resp, _ := c.ListAccountsWithResponse(ctx, p); resp.StatusCode() != http.StatusBadRequest {
			t.Errorf("bad %s: %d", name, resp.StatusCode())
		}
	}
}

func TestListAccountsNeedsRole(t *testing.T) {
	_, c := serveDiag(t, nil, RoleRepair)

	if resp, _ := c.ListAccountsWithResponse(context.Background(), nil); resp.StatusCode() != http.StatusForbidden {
		t.Fatalf("without diagnose: %d", resp.StatusCode())
	}
}

// TestBatchPlanRefusals: a batch needs a backfilled directory, a kind
// that can run over all accounts and the role of that kind.
func TestBatchPlanRefusals(t *testing.T) {
	store, c := serveDiag(t, nil, RoleDiagnose)
	ctx := context.Background()

	// Without a completed directory backfill accounts could be missed.
	if resp, _ := c.CreateBatchPlanWithResponse(ctx, adminapi.BatchPlanRequest{Kind: jobs.KindIndexCheck}); resp.StatusCode() != http.StatusConflict {
		t.Fatalf("unmarked directory: %d", resp.StatusCode())
	}

	registerAccounts(t, store, "bob")

	for kind, want := range map[string]int{
		jobs.KindScrub: http.StatusForbidden, jobs.KindPurge: http.StatusForbidden, "nonsense": http.StatusBadRequest,
	} {
		if resp, _ := c.CreateBatchPlanWithResponse(ctx, adminapi.BatchPlanRequest{Kind: kind}); resp.StatusCode() != want {
			t.Errorf("%s: %d, want %d", kind, resp.StatusCode(), want)
		}
	}
}

// TestBatchPlanAndApply: a batch over all accounts runs as an
// account-less job of the kind batch.
func TestBatchPlanAndApply(t *testing.T) {
	store, c := serveDiag(t, nil, RoleDiagnose)
	ctx := context.Background()

	registerAccounts(t, store, "bob", "carol")

	resp, _ := c.CreateBatchPlanWithResponse(ctx, adminapi.BatchPlanRequest{Kind: jobs.KindIndexCheck})

	plan := resp.JSON201
	if plan == nil || plan.Kind != jobs.KindBatch || plan.Account != "" || plan.Summary["kind"] != jobs.KindIndexCheck {
		t.Fatalf("plan %d %+v", resp.StatusCode(), plan)
	}

	sub, _ := c.SubmitJobWithResponse(ctx, nil, adminapi.JobRequest{PlanId: &plan.Id})
	if sub.JSON202 == nil {
		t.Fatalf("apply %d", sub.StatusCode())
	}

	j := waitJob(t, c, sub.JSON202.Id)

	if j.State != adminapi.Succeeded || j.Kind != jobs.KindBatch || j.Account != "" || j.Result == nil || (*j.Result)["done"] != float64(3) {
		t.Fatalf("job %+v result %v", j, j.Result)
	}
}

// waitJob polls a job until it ended.
func waitJob(t *testing.T, c *adminapi.ClientWithResponses, id string) *adminapi.Job {
	t.Helper()

	for range 500 {
		resp, _ := c.GetJobWithResponse(context.Background(), id)
		if j := resp.JSON200; j != nil && (j.State == adminapi.Succeeded || j.State == adminapi.Failed ||
			j.State == adminapi.PartiallyFailed || j.State == adminapi.Cancelled) {
			return j
		}

		time.Sleep(10 * time.Millisecond)
	}

	t.Fatalf("job %s does not end", id)

	return nil
}

// serveAllAccounts serves a store over mTLS for the clients "ops" (no
// account scope, no scope of all accounts), "fleet" (all accounts) and
// "tenant" (limited to alice).
func serveAllAccounts(t *testing.T, ca *testCA, caFile string) string {
	t.Helper()

	store := memstore.New()
	fillDiagStore(t, store)
	registerAccounts(t, store, "bob")

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	mgr := jobs.NewManager(memstore.NewAdminKV(), &jobs.Env{Accounts: store, Directory: store, Log: log}, jobs.Config{}, "pod-1",
		jobs.Kinds(time.Hour)...)
	roles := []string{RoleDiagnose, RoleRepair}
	s := New(Options{Delivery: &delivery.Service{Store: store, Log: log}, Auth: diagUsers, Log: log, Version: "test",
		MaxMessageBytes: 4096, Accounts: store, Directory: store, Jobs: mgr,
		Clients:            map[string][]string{"ops": roles, "fleet": roles, "tenant": roles},
		ClientScopes:       map[string][]string{"tenant": {"alice"}},
		AllAccountsClients: []string{"Fleet"}})

	srvCert, srvKey := ca.issue(t, "server", "shardpost", 10, x509.ExtKeyUsageServerAuth)

	srv, err := s.TLSServer(srvCert, srvKey, caFile)
	if err != nil {
		t.Fatal(err)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	go func() { _ = srv.ServeTLS(ln, "", "") }()

	t.Cleanup(func() { _ = srv.Close() })

	return "https://" + ln.Addr().String()
}

// TestAllAccountsScope: listing the directory and batches over every
// account need the scope of all accounts, which only clients configured
// with it have.
func TestAllAccountsScope(t *testing.T) {
	old := reloadInterval
	reloadInterval = 0

	t.Cleanup(func() { reloadInterval = old })

	ca := newTestCA(t)
	caFile := filepath.Join(ca.dir, "ca.crt")
	url := serveAllAccounts(t, ca, caFile)
	ctx := context.Background()

	for i, tc := range []struct {
		name string
		want int
	}{{"ops", http.StatusForbidden}, {"tenant", http.StatusForbidden}, {"fleet", http.StatusOK}} {
		cert, key := ca.issue(t, tc.name, tc.name, int64(30+i), x509.ExtKeyUsageClientAuth)

		c, err := adminapi.NewTLSClient(url, cert, key, caFile, 5*time.Second)
		if err != nil {
			t.Fatal(err)
		}

		list, _ := c.ListAccountsWithResponse(ctx, nil)
		plan, _ := c.CreateBatchPlanWithResponse(ctx, adminapi.BatchPlanRequest{Kind: jobs.KindScrub})

		wantPlan := tc.want
		if wantPlan == http.StatusOK {
			wantPlan = http.StatusCreated
		}

		if list.StatusCode() != tc.want || plan.StatusCode() != wantPlan {
			t.Errorf("%s: list %d, plan %d", tc.name, list.StatusCode(), plan.StatusCode())
		}
	}
}
