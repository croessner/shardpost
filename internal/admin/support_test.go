package admin

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"shardpost/internal/adminapi"
	"shardpost/internal/imapsieve"
	"shardpost/internal/model"
)

// testLearning is the learning queue of serveDiag (nil: none).
var testLearning LearningAdmin

// fakeLearning is an in-memory learning queue.
type fakeLearning struct {
	mu    sync.Mutex
	jobs  map[string]imapsieve.Job
	ready bool
}

func (f *fakeLearning) All() ([]imapsieve.Job, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	var out []imapsieve.Job
	for _, j := range f.jobs {
		out = append(out, j)
	}

	return out, nil
}

func (f *fakeLearning) Ready(imapsieve.Job) (bool, error) { return f.ready, nil }

func (f *fakeLearning) Revive(id string) (imapsieve.Job, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	j, ok := f.jobs[id]
	switch {
	case !ok:
		return j, imapsieve.ErrNoJob
	case !j.Dead:
		return j, imapsieve.ErrNotDead
	}

	j.Dead, j.Attempts, j.Next = false, 0, time.Now()
	j.Retries++
	f.jobs[id] = j

	return j, nil
}

// inboxState returns the INBOX id and UIDVALIDITY of alice.
func inboxState(t *testing.T, c *adminapi.ClientWithResponses) (string, int64) {
	t.Helper()

	id := mailboxID(t, c, "INBOX")

	resp, _ := c.GetMailboxWithResponse(context.Background(), "alice", id)
	if resp.JSON200 == nil {
		t.Fatalf("inbox: %d", resp.StatusCode())
	}

	return id, resp.JSON200.Mailbox.Uidvalidity
}

func TestMessagesList(t *testing.T) {
	_, c := serveDiag(t, nil, RoleDiagnose)
	ctx := context.Background()
	id, validity := inboxState(t, c)

	two := 2

	page, _ := c.ListMessagesWithResponse(ctx, "alice", id, &adminapi.ListMessagesParams{Limit: &two})
	if page.JSON200 == nil || len(page.JSON200.Items) != 2 || page.JSON200.NextUid == nil || page.JSON200.Items[1].Flags[0] != `\Seen` {
		t.Fatalf("list: %d %+v", page.StatusCode(), page.JSON200)
	}

	rest, _ := c.ListMessagesWithResponse(ctx, "alice", id, &adminapi.ListMessagesParams{AfterUid: page.JSON200.NextUid})
	if len(rest.JSON200.Items) != 1 || rest.JSON200.NextUid != nil {
		t.Fatalf("rest: %+v", rest.JSON200)
	}

	// Content needs its own role.
	if resp, _ := c.GetMessageWithResponse(ctx, "alice", id, 1, &adminapi.GetMessageParams{Uidvalidity: validity}); resp.StatusCode() != http.StatusForbidden {
		t.Fatalf("show without content role: %d", resp.StatusCode())
	}
}

func TestMessageShowExport(t *testing.T) {
	_, c := serveDiag(t, nil, RoleDiagnose, RoleContent)
	ctx := context.Background()
	id, validity := inboxState(t, c)

	show, _ := c.GetMessageWithResponse(ctx, "alice", id, 1, &adminapi.GetMessageParams{Uidvalidity: validity})
	if show.JSON200 == nil || !strings.Contains(show.JSON200.Header, "Subject: quota") || strings.Contains(show.JSON200.Header, "body") {
		t.Fatalf("show: %d %+v", show.StatusCode(), show.JSON200)
	}

	stale, _ := c.GetMessageWithResponse(ctx, "alice", id, 1, &adminapi.GetMessageParams{Uidvalidity: validity + 1})
	gone, _ := c.GetMessageWithResponse(ctx, "alice", id, 99, &adminapi.GetMessageParams{Uidvalidity: validity})

	if stale.StatusCode() != http.StatusPreconditionFailed || gone.StatusCode() != http.StatusNotFound {
		t.Fatalf("stale %d, gone %d", stale.StatusCode(), gone.StatusCode())
	}

	raw, err := c.ExportMessage(ctx, "alice", id, 1, &adminapi.ExportMessageParams{Uidvalidity: validity})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = raw.Body.Close() }()

	body, _ := io.ReadAll(raw.Body)
	if raw.Header.Get("Content-Type") != "message/rfc822" || string(body) != message {
		t.Fatalf("export: %s %q", raw.Header.Get("Content-Type"), body)
	}
}

func TestFlagsCopyMove(t *testing.T) {
	_, c := serveDiag(t, nil, RoleDiagnose, RoleSupport)
	ctx := context.Background()
	id, validity := inboxState(t, c)
	add, remove := []string{`\Flagged`, "$Label1"}, []string{`\SEEN`}

	res, _ := c.SetMessageFlagsWithResponse(ctx, "alice", id, adminapi.FlagRequest{Uidvalidity: validity, Uids: []int64{2, 99},
		Add: &add, Remove: &remove})
	if res.JSON200 == nil || len(res.JSON200.Updated) != 1 || len(res.JSON200.Missing) != 1 {
		t.Fatalf("flags: %d %+v", res.StatusCode(), res.JSON200)
	}

	if f := strings.Join(res.JSON200.Updated[0].Flags, " "); f != `\Flagged $label1` {
		t.Errorf("flags after change: %q", f)
	}

	recent := []string{`\Recent`}
	if bad, _ := c.SetMessageFlagsWithResponse(ctx, "alice", id, adminapi.FlagRequest{Uidvalidity: validity, Uids: []int64{1},
		Add: &recent}); bad.StatusCode() != http.StatusBadRequest {
		t.Errorf(`\Recent: %d`, bad.StatusCode())
	}

	dest := mailboxID(t, c, "A")
	move := true

	moved, _ := c.CopyMessagesWithResponse(ctx, "alice", id, adminapi.CopyRequest{Uidvalidity: validity, Uids: []int64{1, 3},
		DestinationId: dest, Move: &move})
	if moved.JSON200 == nil || len(moved.JSON200.Copied) != 2 || moved.JSON200.Copied[1].Uid != 3 {
		t.Fatalf("move: %d %+v", moved.StatusCode(), moved.JSON200)
	}

	left, _ := c.ListMessagesWithResponse(ctx, "alice", id, nil)
	if len(left.JSON200.Items) != 1 || left.JSON200.Items[0].Uid != 2 {
		t.Fatalf("left in INBOX: %+v", left.JSON200.Items)
	}

	if self, _ := c.CopyMessagesWithResponse(ctx, "alice", id, adminapi.CopyRequest{Uidvalidity: validity, Uids: []int64{2},
		DestinationId: id}); self.StatusCode() != http.StatusBadRequest {
		t.Errorf("copy onto itself: %d", self.StatusCode())
	}
}

func TestSearch(t *testing.T) {
	store, c := serveDiag(t, nil, RoleContent)
	ctx := context.Background()

	u, _ := store.User("alice")
	_, _ = u.Deliver("A", []byte("From: Carol <carol@example.org>\r\nSubject: Rechnung 42\r\n\r\ntext\r\n"), nil, time.Now())

	subject := "RECHNUNG"
	res, _ := c.SearchMessagesWithResponse(ctx, "alice", adminapi.MessageSearch{Subject: &subject})

	if res.JSON200 == nil || len(res.JSON200.Items) != 1 || res.JSON200.Items[0].Mailbox != "A" || res.JSON200.Items[0].Envelope == nil {
		t.Fatalf("subject: %d %+v", res.StatusCode(), res.JSON200)
	}

	// Paging over all mailboxes: 4 messages, 3 pages of 2 at most.
	var seen int

	var after *string

	for range 5 {
		limit := 2

		page, _ := c.SearchMessagesWithResponse(ctx, "alice", adminapi.MessageSearch{Limit: &limit, After: after})
		seen += len(page.JSON200.Items)

		if after = page.JSON200.Next; after == nil {
			break
		}
	}

	if seen != 4 {
		t.Fatalf("paged search saw %d messages", seen)
	}

	deleted := []string{`\Deleted`}
	if res, _ := c.SearchMessagesWithResponse(ctx, "alice", adminapi.MessageSearch{Flags: &deleted}); len(res.JSON200.Items) != 1 {
		t.Errorf(`\Deleted: %d`, len(res.JSON200.Items))
	}
}

func TestSieveAdmin(t *testing.T) {
	_, c := serveDiag(t, nil, RoleDiagnose, RoleContent, RoleSupport)
	ctx := context.Background()
	activate := true

	bad, _ := c.PutSieveScriptWithResponse(ctx, "alice", "main", nil, adminapi.SievePut{Script: "if frobnicate { stop; }"})
	if bad.StatusCode() != http.StatusBadRequest {
		t.Fatalf("broken script: %d", bad.StatusCode())
	}

	script := `require ["fileinto", "vacation"]; if header :contains "subject" "quota" { fileinto "A"; redirect "x@example.org"; vacation "away"; }`

	put, _ := c.PutSieveScriptWithResponse(ctx, "alice", "main", nil, adminapi.SievePut{Script: script, Activate: &activate})
	if put.JSON200 == nil || !put.JSON200.Active {
		t.Fatalf("put: %d %v", put.StatusCode(), put.JSONDefault)
	}

	star := adminapi.PutSieveScriptParamsIfNoneMatch("*")
	if again, _ := c.PutSieveScriptWithResponse(ctx, "alice", "main", &adminapi.PutSieveScriptParams{IfNoneMatch: &star},
		adminapi.SievePut{Script: script}); again.StatusCode() != http.StatusPreconditionFailed {
		t.Errorf("create-only over an existing script: %d", again.StatusCode())
	}

	stale := `"s-0000000000000000"`
	if del, _ := c.DeleteSieveScriptWithResponse(ctx, "alice", "main", &adminapi.DeleteSieveScriptParams{IfMatch: &stale}); del.StatusCode() != http.StatusPreconditionFailed {
		t.Errorf("stale delete: %d", del.StatusCode())
	}

	if del, _ := c.DeleteSieveScriptWithResponse(ctx, "alice", "main", nil); del.StatusCode() != http.StatusConflict {
		t.Errorf("delete active: %d", del.StatusCode())
	}

	checkDryRun(t, c)

	if list, _ := c.SetActiveSieveScriptWithResponse(ctx, "alice", adminapi.SieveActivate{Name: ""}); list.JSON200 == nil || list.JSON200.Active != nil {
		t.Fatalf("deactivate: %d", list.StatusCode())
	}
}

// checkDryRun runs the active script over INBOX uid 1: every side effect
// is reported as suppressed.
func checkDryRun(t *testing.T, c *adminapi.ClientWithResponses) {
	t.Helper()

	ctx := context.Background()
	id, validity := inboxState(t, c)

	check, _ := c.CheckSieveWithResponse(ctx, "alice", adminapi.SieveCheck{Message: &adminapi.SieveMessageRef{MailboxId: id,
		Uidvalidity: validity, Uid: 1}})
	if check.JSON200 == nil || !check.JSON200.Valid || check.JSON200.Actions == nil {
		t.Fatalf("check: %d %+v", check.StatusCode(), check.JSON200)
	}

	a := check.JSON200.Actions
	if len(a.Fileinto) != 1 || a.Fileinto[0].Mailbox != "A" || len(a.Redirect) != 1 || a.Vacation == nil || len(a.Suppressed) != 2 {
		t.Fatalf("actions: %+v", a)
	}
}

func TestSieveCheckWithoutStoreNeedsNoContentRole(t *testing.T) {
	_, c := serveDiag(t, nil, RoleDiagnose)
	ctx := context.Background()
	script := `require "fileinto"; fileinto "B";`
	raw := "Subject: x\r\n\r\nx\r\n"

	res, _ := c.CheckSieveWithResponse(ctx, "alice", adminapi.SieveCheck{Script: &script, Raw: &raw})
	if res.JSON200 == nil || !res.JSON200.Valid || res.JSON200.Actions.Fileinto[0].Mailbox != "B" || res.JSON200.Actions.Keep {
		t.Fatalf("check: %d %+v", res.StatusCode(), res.JSON200)
	}

	broken := "fileinto"
	if res, _ := c.CheckSieveWithResponse(ctx, "alice", adminapi.SieveCheck{Script: &broken}); res.JSON200 == nil || res.JSON200.Valid {
		t.Fatalf("broken: %d", res.StatusCode())
	}

	if res, _ := c.CheckSieveWithResponse(ctx, "alice", adminapi.SieveCheck{}); res.StatusCode() != http.StatusForbidden {
		t.Fatalf("stored script without content role: %d", res.StatusCode())
	}
}

func TestLearningRetry(t *testing.T) {
	q := &fakeLearning{jobs: map[string]imapsieve.Job{
		"a1": {ID: "a1", Owner: "alice", Action: "spam", Dead: true, Attempts: 3},
		"a2": {ID: "a2", Owner: "alice", Action: "spam"},
		"b1": {ID: "b1", Owner: "bob", Action: "ham", Dead: true, Prepared: true},
	}}
	testLearning = q

	t.Cleanup(func() { testLearning = nil })

	_, c := serveDiag(t, nil, RoleDiagnose, RoleRepair)
	ctx := context.Background()
	dead := adminapi.LearningState(learningDead)

	list, _ := c.ListLearningJobsWithResponse(ctx, &adminapi.ListLearningJobsParams{State: &dead})
	if list.JSON200 == nil || len(list.JSON200.Items) != 2 {
		t.Fatalf("dead jobs: %d %+v", list.StatusCode(), list.JSON200)
	}

	for _, tc := range []struct {
		id      string
		confirm bool
		want    int
	}{
		{"a2", true, http.StatusConflict},  // not dead
		{"b1", true, http.StatusConflict},  // never committed
		{"a1", false, http.StatusConflict}, // attempted, not confirmed
		{"a1", true, http.StatusOK},
		{"zz", true, http.StatusNotFound},
	} {
		resp, _ := c.RetryLearningJobWithResponse(ctx, tc.id, adminapi.LearningRetry{ConfirmRepeat: &tc.confirm})
		if resp.StatusCode() != tc.want {
			t.Errorf("%s confirm %v: %d, want %d", tc.id, tc.confirm, resp.StatusCode(), tc.want)
		}
	}

	if j, _ := c.GetLearningJobWithResponse(ctx, "a1"); j.JSON200 == nil || j.JSON200.State != learningPending || j.JSON200.Retries != 1 {
		t.Fatalf("after retry: %+v", j.JSON200)
	}
}

// TestPublicNamespaceAdministration: public folders are provisioned and
// granted like any account, without a user database entry, and they are
// not a delivery target.
func TestPublicNamespaceAdministration(t *testing.T) {
	_, c := serveDiag(t, nil, RoleDiagnose, RoleProvision, RoleDeliver)
	ctx := context.Background()
	pub := model.PublicNamespace

	created, _ := c.CreateMailboxWithResponse(ctx, pub, adminapi.MailboxCreate{Name: "Announcements"})
	if created.StatusCode() != http.StatusCreated {
		t.Fatalf("create: %d %v", created.StatusCode(), created.JSONDefault)
	}

	id := created.JSON201.Id

	acl, _ := c.SetACLEntryWithResponse(ctx, pub, id, "anyone", nil, adminapi.ACLChange{Rights: "lr"})
	if acl.JSON200 == nil || acl.JSON200.Entries[0].Rights != "lr" {
		t.Fatalf("acl: %d %+v", acl.StatusCode(), acl.JSON200)
	}

	if list, _ := c.ListMailboxesWithResponse(ctx, pub, nil); list.JSON200 == nil || len(list.JSON200.Items) != 1 {
		t.Fatalf("list: %d", list.StatusCode())
	}

	if acct, _ := c.GetAccountWithResponse(ctx, pub); acct.JSON200 == nil || !acct.JSON200.StoreExists {
		t.Fatalf("account show: %d %+v", acct.StatusCode(), acct.JSON200)
	}

	body := strings.NewReader(message)
	if resp, _ := c.DeliverWithBodyWithResponse(ctx, &adminapi.DeliverParams{Recipient: pub}, "message/rfc822", body); resp.StatusCode() != http.StatusBadRequest {
		t.Errorf("delivery to the public namespace: %d", resp.StatusCode())
	}
}
