package admin

import (
	"context"
	"crypto/x509"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"shardpost/internal/adminapi"
	"shardpost/internal/auth"
	"shardpost/internal/delivery"
	"shardpost/internal/jobs"
	"shardpost/internal/memstore"
	"shardpost/internal/sharenotice"
	"shardpost/internal/model"
)

// diagUsers are the static accounts of the diagnosis tests.
var diagUsers = auth.Static{"alice": "pw", "bob": "pw", "carol": "pw"}

// fillDiagStore gives alice INBOX with three messages (one seen, one
// deleted), mailboxes A to D and "Shared" (bob lr, anyone r).
func fillDiagStore(t *testing.T, store *memstore.Store) {
	t.Helper()

	u, _ := store.User("alice")

	for _, name := range []string{"INBOX", "A", "B", "C", "D", "Shared"} {
		if err := u.EnsureMailbox(name, nil); err != nil {
			t.Fatal(err)
		}
	}

	for _, flags := range [][]string{nil, {`\seen`}, {`\deleted`}} {
		if _, err := u.Deliver("INBOX", []byte(message), flags, time.Now()); err != nil {
			t.Fatal(err)
		}
	}

	if err := u.SetACL("Shared", "bob", "lr"); err != nil {
		t.Fatal(err)
	}

	if err := u.SetACL("Shared", "anyone", "r"); err != nil {
		t.Fatal(err)
	}
}

// serveDiag is an admin socket with the given roles on a filled memstore;
// wrap may replace the store the API sees.
func serveDiag(t *testing.T, wrap func(*memstore.Store) delivery.Store, roles ...string) (*memstore.Store, *adminapi.ClientWithResponses) {
	t.Helper()

	store := memstore.New()
	fillDiagStore(t, store)

	var accounts delivery.Store = store
	if wrap != nil {
		accounts = wrap(store)
	}

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	mgr := jobs.NewManager(memstore.NewAdminKV(), &jobs.Env{Accounts: accounts, Log: log},
		jobs.Config{Poll: 10 * time.Millisecond, LeaseTTL: time.Second}, "pod-1", jobs.Kinds(time.Hour)...)
	s := New(Options{Delivery: &delivery.Service{Store: store, Log: log}, Auth: diagUsers, Log: log, Version: "test",
		MaxMessageBytes: 4096, SocketRoles: roles, Accounts: accounts, Instance: "pod-1", StartedAt: time.Unix(1000, 0),
		ConfigFingerprint: "abc", StorePing: func() error { return nil }, Jobs: mgr, Learning: testLearning,
		SharedPrefix: "Shared/", PublicPrefix: "Public/", ShareNotices: &sharenotice.Recorder{Store: store}})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})

	go func() { mgr.Run(ctx); close(done) }()

	t.Cleanup(func() { cancel(); <-done })

	path := filepath.Join(socketDir(t), "admin.sock")

	ln, err := ListenUnix(path)
	if err != nil {
		t.Fatal(err)
	}

	srv := s.UnixServer()

	go func() { _ = srv.Serve(ln) }()

	t.Cleanup(func() { _ = srv.Close() })

	client, err := adminapi.NewSocketClient(path, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}

	return store, client
}

func TestDiagnoseNeedsRole(t *testing.T) {
	_, c := serveDiag(t, nil, RoleImport, RoleTrace)
	ctx := context.Background()

	srv, _ := c.GetServerStatusWithResponse(ctx)
	acc, _ := c.GetAccountWithResponse(ctx, "alice")
	list, _ := c.ListMailboxesWithResponse(ctx, "alice", nil)

	for name, code := range map[string]int{"server": srv.StatusCode(), "account": acc.StatusCode(), "mailboxes": list.StatusCode()} {
		if code != http.StatusForbidden {
			t.Errorf("%s without role: %d", name, code)
		}
	}
}

func TestServerStatus(t *testing.T) {
	_, c := serveDiag(t, nil, RoleDiagnose)

	resp, _ := c.GetServerStatusWithResponse(context.Background())
	if st := resp.JSON200; st == nil || st.Instance != "pod-1" || st.ConfigFingerprint != "abc" || !st.Store.Ok || st.Version != "test" {
		t.Fatalf("%d %+v", resp.StatusCode(), resp.JSON200)
	}
}

func TestAccountStatus(t *testing.T) {
	store, c := serveDiag(t, nil, RoleDiagnose)
	ctx := context.Background()

	u, _ := store.User("alice")
	_ = u.(model.Tracer).SetTrace(model.TraceState{Until: time.Now().Add(time.Hour)})

	for _, tc := range []struct {
		account, lookup, canonical string
		exists                     bool
	}{
		{"alice", "canonical", "alice", true},
		{"ALICE@example.org", "alias", "alice", false},
		{"nobody", "unknown", "", false},
	} {
		resp, _ := c.GetAccountWithResponse(ctx, tc.account)

		st := resp.JSON200
		if st == nil || string(st.Lookup) != tc.lookup || deref(st.CanonicalAccount) != tc.canonical || st.StoreExists != tc.exists {
			t.Fatalf("%s: %d %+v", tc.account, resp.StatusCode(), st)
		}

		if tc.account == "alice" && (st.Mailboxes != 6 || st.Quota.UsageMessages != 3 || !st.TraceActive) {
			t.Fatalf("alice: %+v", st)
		}
	}
}

func TestMailboxPages(t *testing.T) {
	_, c := serveDiag(t, nil, RoleDiagnose)
	ctx := context.Background()

	var names []string

	params := &adminapi.ListMailboxesParams{Limit: ptr(4)}

	for pages := 0; ; pages++ {
		resp, _ := c.ListMailboxesWithResponse(ctx, "alice", params)
		if resp.JSON200 == nil || pages > 3 {
			t.Fatalf("page %d: %d", pages, resp.StatusCode())
		}

		for _, m := range resp.JSON200.Items {
			names = append(names, m.Name)
		}

		if resp.JSON200.Next == nil {
			break
		}

		params = &adminapi.ListMailboxesParams{Limit: ptr(4), After: resp.JSON200.Next}
	}

	if got := strings.Join(names, ","); got != "A,B,C,D,INBOX,Shared" {
		t.Fatalf("pages: %s", got)
	}
}

func TestMailboxPagesByNameEmptyAndLimits(t *testing.T) {
	_, c := serveDiag(t, nil, RoleDiagnose)
	ctx := context.Background()

	one, _ := c.ListMailboxesWithResponse(ctx, "alice", &adminapi.ListMailboxesParams{Name: ptr("Shared")})
	if one.JSON200 == nil || len(one.JSON200.Items) != 1 || one.JSON200.Items[0].AclEntries != 2 {
		t.Fatalf("by name: %+v", one.JSON200)
	}

	// Empty results: an account without mailboxes, an unknown name.
	for _, p := range []struct {
		account string
		params  *adminapi.ListMailboxesParams
	}{{"carol", nil}, {"alice", &adminapi.ListMailboxesParams{Name: ptr("Nope")}}} {
		resp, _ := c.ListMailboxesWithResponse(ctx, p.account, p.params)
		if resp.JSON200 == nil || len(resp.JSON200.Items) != 0 || resp.JSON200.Next != nil {
			t.Fatalf("empty %s: %d %+v", p.account, resp.StatusCode(), resp.JSON200)
		}
	}

	for _, limit := range []int{0, 1001} {
		if resp, _ := c.ListMailboxesWithResponse(ctx, "alice", &adminapi.ListMailboxesParams{Limit: ptr(limit)}); resp.StatusCode() != http.StatusBadRequest {
			t.Errorf("limit %d: %d", limit, resp.StatusCode())
		}
	}

	if resp, _ := c.ListMailboxesWithResponse(ctx, "ALICE@example.org", nil); resp.StatusCode() != http.StatusBadRequest {
		t.Errorf("alias: %d", resp.StatusCode())
	}
}

// mailboxID returns the id of an alice mailbox through the API.
func mailboxID(t *testing.T, c *adminapi.ClientWithResponses, name string) string {
	t.Helper()

	resp, _ := c.ListMailboxesWithResponse(context.Background(), "alice", &adminapi.ListMailboxesParams{Name: &name})
	if resp.JSON200 == nil || len(resp.JSON200.Items) != 1 {
		t.Fatalf("%s: %d", name, resp.StatusCode())
	}

	return resp.JSON200.Items[0].Id
}

func TestMailboxDetailAndQuota(t *testing.T) {
	_, c := serveDiag(t, nil, RoleDiagnose)
	ctx := context.Background()

	resp, _ := c.GetMailboxWithResponse(ctx, "alice", mailboxID(t, c, "INBOX"))

	d := resp.JSON200
	if d == nil || d.Messages != 3 || d.Unseen != 2 || d.Deleted != 1 || d.SizeBytes != 3*int64(len(message)) || d.Mailbox.UidNext != 4 {
		t.Fatalf("%d %+v", resp.StatusCode(), d)
	}

	if resp, _ := c.GetMailboxWithResponse(ctx, "alice", "nope"); resp.StatusCode() != http.StatusNotFound {
		t.Fatalf("unknown id: %d", resp.StatusCode())
	}

	q, _ := c.GetQuotaWithResponse(ctx, "alice")
	if q.JSON200 == nil || q.JSON200.UsageMessages != 3 {
		t.Fatalf("quota: %+v", q.JSON200)
	}
}

func TestACLAndShared(t *testing.T) {
	_, c := serveDiag(t, nil, RoleDiagnose)
	ctx := context.Background()

	acl, _ := c.GetMailboxACLWithResponse(ctx, "alice", mailboxID(t, c, "Shared"))
	if a := acl.JSON200; a == nil || len(a.Entries) != 2 || a.Entries[0].Identifier != "anyone" || a.Entries[1].Rights != "lr" {
		t.Fatalf("acl: %+v", acl.JSON200)
	}

	shared, _ := c.ListSharedMailboxesWithResponse(ctx, "bob")
	if s := shared.JSON200; s == nil || len(s.Items) != 1 || s.Items[0].Owner != "alice" || s.Items[0].Mailbox != "Shared" {
		t.Fatalf("shared: %+v", shared.JSON200)
	}
}

func TestExplainACL(t *testing.T) {
	_, c := serveDiag(t, nil, RoleDiagnose)
	ctx := context.Background()
	id := mailboxID(t, c, "Shared")

	for _, tc := range []struct {
		principal, rights, finding string
		visible                    bool
	}{
		{"bob", "lr", "visible as Shared/alice/Shared", true},
		{"BOB@example.org", "lr", "is an alias of bob", true},
		{"carol", "r", "lack l", false},
		{"alice", model.AllRights, "owns the mailbox", true},
		{"nobody", "r", "unknown to Nauthilus", false},
	} {
		resp, _ := c.ExplainMailboxACLWithResponse(ctx, "alice", id, &adminapi.ExplainMailboxACLParams{Principal: tc.principal})

		e := resp.JSON200
		if e == nil || e.Rights != tc.rights || e.Visible != tc.visible || !strings.Contains(strings.Join(e.Findings, "\n"), tc.finding) {
			t.Fatalf("%s: %d %+v", tc.principal, resp.StatusCode(), e)
		}
	}
}

// TestExplainAnyoneWithoutPointer: rights only through anyone are listed
// by nobody (anyone writes no pointers).
func TestExplainAnyoneWithoutPointer(t *testing.T) {
	store, c := serveDiag(t, nil, RoleDiagnose)
	ctx := context.Background()

	u, _ := store.User("alice")
	if err := u.SetACL("Shared", "anyone", "lr"); err != nil {
		t.Fatal(err)
	}

	resp, _ := c.ExplainMailboxACLWithResponse(ctx, "alice", mailboxID(t, c, "Shared"), &adminapi.ExplainMailboxACLParams{Principal: "carol"})
	if e := resp.JSON200; e == nil || !e.Visible || e.Pointer.Present || !strings.Contains(strings.Join(e.Findings, "\n"), "only through anyone") {
		t.Fatalf("%+v", resp.JSON200)
	}
}

// readOnlyStore opens user views that fail the test on every write.
type readOnlyStore struct {
	t     *testing.T
	inner *memstore.Store
}

func (s readOnlyStore) User(name string) (model.User, error) {
	u, err := s.inner.User(name)
	if err != nil {
		return nil, err
	}

	return &readOnlyUser{User: u, t: s.t}, nil
}

// readOnlyUser forwards reads (including the optional pager and trace
// switch) and fails the test on writes.
type readOnlyUser struct {
	model.User
	t *testing.T
}

var errWrite = errors.New("write during diagnosis")

func (u *readOnlyUser) write(op string) error {
	u.t.Errorf("diagnosis called %s", op)
	return errWrite
}

func (u *readOnlyUser) ListMailboxesAfter(after string, limit int) ([]model.Mailbox, []string, error) {
	return u.User.(model.MailboxPager).ListMailboxesAfter(after, limit)
}

func (u *readOnlyUser) Trace() (model.TraceState, error) { return u.User.(model.Tracer).Trace() }
func (u *readOnlyUser) SetTrace(model.TraceState) error  { return u.write("SetTrace") }
func (u *readOnlyUser) CreateMailbox(string, []string) error {
	return u.write("CreateMailbox")
}
func (u *readOnlyUser) EnsureMailbox(string, []string) error { return u.write("EnsureMailbox") }
func (u *readOnlyUser) DeleteMailbox(string) error           { return u.write("DeleteMailbox") }
func (u *readOnlyUser) RenameMailbox(string, string) error   { return u.write("RenameMailbox") }
func (u *readOnlyUser) SetSubscribed(string, bool) error     { return u.write("SetSubscribed") }
func (u *readOnlyUser) Deliver(string, []byte, []string, time.Time) (*model.Meta, error) {
	return nil, u.write("Deliver")
}
func (u *readOnlyUser) DeliverGUID(string, string, []byte, []string, time.Time) (*model.Meta, error) {
	return nil, u.write("DeliverGUID")
}
func (u *readOnlyUser) SetFlags(string, []uint32, uint64, func([]string) []string) (*model.FlagsResult, error) {
	return nil, u.write("SetFlags")
}
func (u *readOnlyUser) Expunge(string, []model.Meta) error { return u.write("Expunge") }
func (u *readOnlyUser) Copy([]model.Meta, string) ([]model.Meta, error) {
	return nil, u.write("Copy")
}
func (u *readOnlyUser) Move(string, []model.Meta, string) ([]model.Meta, error) {
	return nil, u.write("Move")
}
func (u *readOnlyUser) SetQuotaLimit(model.Quota) error { return u.write("SetQuotaLimit") }
func (u *readOnlyUser) SetStorageLimit(int64) error     { return u.write("SetStorageLimit") }
func (u *readOnlyUser) MarkRecent(string, uint32) error { return u.write("MarkRecent") }
func (u *readOnlyUser) SetACL(string, string, string) error {
	return u.write("SetACL")
}
func (u *readOnlyUser) SetSharedSubscribed(string, string, bool) error {
	return u.write("SetSharedSubscribed")
}
func (u *readOnlyUser) RemoveSharedRef(model.SharedRef) error { return u.write("RemoveSharedRef") }
func (u *readOnlyUser) SetMetadata(string, string, map[string]*string) error {
	return u.write("SetMetadata")
}
func (u *readOnlyUser) SwapMetadata(string, string, string, string, *string) (bool, error) {
	return false, u.write("SwapMetadata")
}
func (u *readOnlyUser) BeginDelivery() (string, error)              { return "", u.write("BeginDelivery") }
func (u *readOnlyUser) RecordDelivery(string, string, string) error { return u.write("RecordDelivery") }
func (u *readOnlyUser) EndDelivery(string) error                    { return u.write("EndDelivery") }
func (u *readOnlyUser) PutScript(string, string) error              { return u.write("PutScript") }
func (u *readOnlyUser) DeleteScript(string) error                   { return u.write("DeleteScript") }
func (u *readOnlyUser) SetActiveScript(string) error                { return u.write("SetActiveScript") }
func (u *readOnlyUser) RenameScript(string, string) error           { return u.write("RenameScript") }

// TestDiagnoseDoesNotWrite runs every diagnosis on a store that fails
// the test on any write.
func TestDiagnoseDoesNotWrite(t *testing.T) {
	_, c := serveDiag(t, func(s *memstore.Store) delivery.Store { return readOnlyStore{t: t, inner: s} }, RoleDiagnose)
	ctx := context.Background()
	id := mailboxID(t, c, "Shared")
	inbox := mailboxID(t, c, "INBOX")

	codes := []int{}

	acc, _ := c.GetAccountWithResponse(ctx, "alice")
	list, _ := c.ListMailboxesWithResponse(ctx, "alice", &adminapi.ListMailboxesParams{Limit: ptr(2)})
	mb, _ := c.GetMailboxWithResponse(ctx, "alice", inbox)
	acl, _ := c.GetMailboxACLWithResponse(ctx, "alice", id)
	ex, _ := c.ExplainMailboxACLWithResponse(ctx, "alice", id, &adminapi.ExplainMailboxACLParams{Principal: "bob"})
	q, _ := c.GetQuotaWithResponse(ctx, "alice")
	sh, _ := c.ListSharedMailboxesWithResponse(ctx, "bob")

	codes = append(codes, acc.StatusCode(), list.StatusCode(), mb.StatusCode(), acl.StatusCode(), ex.StatusCode(), q.StatusCode(), sh.StatusCode())

	for i, code := range codes {
		if code != http.StatusOK {
			t.Errorf("request %d: %d", i, code)
		}
	}
}

// TestDiagnoseScopeAndCertificates: an mTLS client limited to alice may
// read alice but neither bob nor bob as the principal of an explanation,
// nor switch bob's trace; an expired certificate fails the handshake.
func TestDiagnoseScopeAndCertificates(t *testing.T) {
	old := reloadInterval
	reloadInterval = 0

	t.Cleanup(func() { reloadInterval = old })

	ca := newTestCA(t)
	caFile := filepath.Join(ca.dir, "ca.crt")
	opsCert, opsKey := ca.issue(t, "ops", "ops", 20, x509.ExtKeyUsageClientAuth)
	oldCert, oldKey := ca.issueValid(t, "old", "ops", 21, time.Now().Add(-48*time.Hour), time.Now().Add(-24*time.Hour))
	url, mgr := serveScoped(t, ca, caFile)

	ctx := context.Background()

	c, err := adminapi.NewTLSClient(url, opsCert, opsKey, caFile, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}

	if resp, _ := c.GetAccountWithResponse(ctx, "alice"); resp.StatusCode() != http.StatusOK {
		t.Fatalf("alice in scope: %d", resp.StatusCode())
	}

	id := ""
	if resp, _ := c.ListMailboxesWithResponse(ctx, "alice", &adminapi.ListMailboxesParams{Name: ptr("Shared")}); resp.JSON200 != nil && len(resp.JSON200.Items) == 1 {
		id = resp.JSON200.Items[0].Id
	}

	upper, _ := c.GetAccountWithResponse(ctx, "Alice")
	aliasOfBob, _ := c.GetAccountWithResponse(ctx, "bob@alice.org") // in scope, canonical bob is not

	if upper.StatusCode() != http.StatusForbidden || aliasOfBob.StatusCode() != http.StatusForbidden {
		t.Errorf("upper-case or alias of an account outside the scope: %d %d", upper.StatusCode(), aliasOfBob.StatusCode())
	}

	bob, _ := c.GetAccountWithResponse(ctx, "bob")
	ex, _ := c.ExplainMailboxACLWithResponse(ctx, "alice", id, &adminapi.ExplainMailboxACLParams{Principal: "bob"})
	tr, _ := c.SetTraceWithResponse(ctx, "bob", adminapi.TraceRequest{})
	// alice shares with bob: purging alice would change bob.
	purge, _ := c.CreatePlanWithResponse(ctx, "alice", adminapi.PlanRequest{Kind: adminapi.PlanRequestKind(jobs.KindPurge)})
	list, _ := c.ListJobsWithResponse(ctx, nil)

	for name, code := range map[string]int{"account bob": bob.StatusCode(), "explain bob": ex.StatusCode(), "trace bob": tr.StatusCode(),
		"purge touching bob": purge.StatusCode(), "jobs without account": list.StatusCode()} {
		if code != http.StatusForbidden {
			t.Errorf("%s out of scope: %d", name, code)
		}
	}

	checkScopeAtApply(t, c, mgr)

	expired, err := adminapi.NewTLSClient(url, oldCert, oldKey, caFile, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := expired.GetServerStatusWithResponse(ctx); err == nil {
		t.Fatal("expired client certificate accepted")
	}
}

func TestDiagnoseOperationLabels(t *testing.T) {
	for path, want := range map[string]string{
		"/admin/v1/server":                                      "server",
		"/admin/v1/accounts/a@b":                                "account",
		"/admin/v1/accounts/a@b/quota":                          "quota",
		"/admin/v1/accounts/a@b/shared":                         "shared",
		"/admin/v1/accounts/a@b/trace":                          "trace",
		"/admin/v1/accounts/a@b/mailboxes":                      "mailboxes",
		"/admin/v1/accounts/a@b/mailboxes/x":                    "mailbox",
		"/admin/v1/accounts/a@b/mailboxes/x/acl":                "acl",
		"/admin/v1/accounts/a@b/mailboxes/x/acl/explain":        "acl_explain",
		"/admin/v1/accounts/a@b/mailboxes/x/acl/bob":            "acl_entry",
		"/admin/v1/accounts/a@b/import/mailboxes/x/messages":    "import_messages",
		"/admin/v1/accounts/a@b/mailboxes/x/acl/explain/deeper": opOther,
	} {
		if got := operation(path); got != want {
			t.Errorf("%s: %q, want %q", path, got, want)
		}
	}
}

// serveScoped serves the filled store over mTLS for the client "ops",
// limited to alice, and returns the URL.
func serveScoped(t *testing.T, ca *testCA, caFile string) (string, *jobs.Manager) {
	t.Helper()

	store := memstore.New()
	fillDiagStore(t, store)

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	mgr := jobs.NewManager(memstore.NewAdminKV(), &jobs.Env{Accounts: store, Log: log}, jobs.Config{}, "pod-1", jobs.Kinds(time.Hour)...)
	s := New(Options{Delivery: &delivery.Service{Store: store, Log: log}, Auth: diagUsers, Log: log, Version: "test",
		MaxMessageBytes: 4096, Accounts: store, Clients: map[string][]string{"ops": {RoleDiagnose, RoleTrace, RoleDelete}},
		ClientScopes: map[string][]string{"OPS": {"alice", "*@alice.org"}}, Jobs: mgr})

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

	return "https://" + ln.Addr().String(), mgr
}

// checkScopeAtApply: a plan made by an unscoped client that changes bob
// cannot be applied by a client limited to alice; bob's jobs do not exist
// for it.
func checkScopeAtApply(t *testing.T, c *adminapi.ClientWithResponses, mgr *jobs.Manager) {
	t.Helper()

	ctx := context.Background()

	purge, _ := mgr.Kind(jobs.KindPurge)

	plan, err := mgr.CreatePlan(ctx, purge, "alice", nil, "root", nil)
	if err != nil {
		t.Fatal(err)
	}

	if resp, _ := c.SubmitJobWithResponse(ctx, nil, adminapi.JobRequest{PlanId: &plan.ID}); resp.StatusCode() != http.StatusForbidden {
		t.Errorf("apply of a plan touching bob: %d", resp.StatusCode())
	}

	check, _ := mgr.Kind(jobs.KindIndexCheck)

	j, _, err := mgr.Submit(check, "bob", nil, "", "root", "")
	if err != nil {
		t.Fatal(err)
	}

	if resp, _ := c.GetJobWithResponse(ctx, j.ID); resp.StatusCode() != http.StatusNotFound {
		t.Errorf("job of bob: %d", resp.StatusCode())
	}
}

// brokenEntryStore adds an unreadable registry entry "B0" between B and C.
type brokenEntryStore struct{ inner *memstore.Store }

func (s brokenEntryStore) User(name string) (model.User, error) {
	u, err := s.inner.User(name)
	if err != nil {
		return nil, err
	}

	return &brokenEntryUser{User: u}, nil
}

type brokenEntryUser struct{ model.User }

func (u *brokenEntryUser) ListMailboxesAfter(after string, limit int) ([]model.Mailbox, []string, error) {
	all, err := u.ListMailboxes()
	if err != nil {
		return nil, nil, err
	}

	var boxes []model.Mailbox

	var scanned []string

	for _, name := range []string{"A", "B", "B0", "C", "D", "INBOX", "Shared"} {
		if name <= after || len(scanned) == limit {
			continue
		}

		scanned = append(scanned, name)

		for _, mb := range all {
			if mb.Name == name {
				boxes = append(boxes, mb)
			}
		}
	}

	return boxes, scanned, nil
}

// TestMailboxPagesPastBrokenEntry: an unreadable registry entry neither
// ends the listing nor hides a mailbox.
func TestMailboxPagesPastBrokenEntry(t *testing.T) {
	_, c := serveDiag(t, func(s *memstore.Store) delivery.Store { return brokenEntryStore{inner: s} }, RoleDiagnose)
	ctx := context.Background()

	var names []string

	params := &adminapi.ListMailboxesParams{Limit: ptr(3)}

	for pages := 0; pages < 5; pages++ {
		resp, _ := c.ListMailboxesWithResponse(ctx, "alice", params)
		if resp.JSON200 == nil {
			t.Fatalf("page %d: %d", pages, resp.StatusCode())
		}

		for _, m := range resp.JSON200.Items {
			names = append(names, m.Name)
		}

		if resp.JSON200.Next == nil {
			break
		}

		params = &adminapi.ListMailboxesParams{Limit: ptr(3), After: resp.JSON200.Next}
	}

	if got := strings.Join(names, ","); got != "A,B,C,D,INBOX,Shared" {
		t.Fatalf("pages: %s", got)
	}
}
