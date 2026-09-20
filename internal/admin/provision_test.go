package admin

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"testing"

	"shardpost/internal/adminapi"
	"shardpost/internal/model"
)

func etagOf(resp *http.Response) *string {
	e := resp.Header.Get("ETag")
	return &e
}

func TestProvisionNeedsRole(t *testing.T) {
	_, c := serveDiag(t, nil, RoleDiagnose)

	resp, _ := c.CreateMailboxWithResponse(context.Background(), "alice", adminapi.MailboxCreate{Name: "New"})
	if resp.StatusCode() != http.StatusForbidden {
		t.Fatalf("without role: %d", resp.StatusCode())
	}
}

func TestCreateMailbox(t *testing.T) {
	_, c := serveDiag(t, nil, RoleDiagnose, RoleProvision)
	ctx := context.Background()
	archive := []string{`\Archive`}

	first, _ := c.CreateMailboxWithResponse(ctx, "alice", adminapi.MailboxCreate{Name: "Projekte/Übersicht", SpecialUse: &archive})
	if first.StatusCode() != http.StatusCreated || first.JSON201 == nil || first.HTTPResponse.Header.Get("ETag") == "" {
		t.Fatalf("create: %d %v", first.StatusCode(), first.JSONDefault)
	}

	// Repeated (lost response): the same mailbox, 200.
	again, _ := c.CreateMailboxWithResponse(ctx, "alice", adminapi.MailboxCreate{Name: "Projekte/Übersicht", SpecialUse: &archive})
	if again.StatusCode() != http.StatusOK || again.JSON200.Id != first.JSON201.Id {
		t.Fatalf("repeat: %d", again.StatusCode())
	}

	if other, _ := c.CreateMailboxWithResponse(ctx, "alice", adminapi.MailboxCreate{Name: "Projekte/Übersicht"}); other.StatusCode() != http.StatusConflict {
		t.Fatalf("different special-use: %d", other.StatusCode())
	}

	lower := []string{`\archive`, `\ARCHIVE`}
	if resp, _ := c.CreateMailboxWithResponse(ctx, "alice", adminapi.MailboxCreate{Name: "Projekte/Übersicht", SpecialUse: &lower}); resp.StatusCode() != http.StatusOK {
		t.Errorf("special-use in another spelling: %d", resp.StatusCode())
	}

	unknown := []string{`\Frobnicate`}
	if resp, _ := c.CreateMailboxWithResponse(ctx, "alice", adminapi.MailboxCreate{Name: "Y", SpecialUse: &unknown}); resp.StatusCode() != http.StatusBadRequest {
		t.Errorf("unknown special-use: %d", resp.StatusCode())
	}

	for _, name := range []string{"", "Shared/bob/X", "Public/Team", "a//b", "trailing/", "bad\x01name"} {
		if resp, _ := c.CreateMailboxWithResponse(ctx, "alice", adminapi.MailboxCreate{Name: name}); resp.StatusCode() != http.StatusBadRequest {
			t.Errorf("%q: %d", name, resp.StatusCode())
		}
	}

	if resp, _ := c.CreateMailboxWithResponse(ctx, "ALICE@example.org", adminapi.MailboxCreate{Name: "X"}); resp.StatusCode() != http.StatusBadRequest {
		t.Errorf("alias: %d", resp.StatusCode())
	}
}

func TestRenameAndSubscribe(t *testing.T) {
	store, c := serveDiag(t, nil, RoleDiagnose, RoleProvision)
	ctx := context.Background()

	u, _ := store.User("alice")
	_ = u.EnsureMailbox("A/Sub", nil)

	id := mailboxID(t, c, "A")

	cur, _ := c.GetMailboxWithResponse(ctx, "alice", id)
	stale := `"m-0000000000000000"`

	if resp, _ := c.UpdateMailboxWithResponse(ctx, "alice", id, &adminapi.UpdateMailboxParams{IfMatch: &stale},
		adminapi.MailboxUpdate{Name: ptr("Z")}); resp.StatusCode() != http.StatusPreconditionFailed {
		t.Fatalf("stale If-Match: %d", resp.StatusCode())
	}

	resp, _ := c.UpdateMailboxWithResponse(ctx, "alice", id, &adminapi.UpdateMailboxParams{IfMatch: etagOf(cur.HTTPResponse)},
		adminapi.MailboxUpdate{Name: ptr("Z"), Subscribed: ptr(false)})
	if resp.JSON200 == nil || resp.JSON200.Name != "Z" || resp.JSON200.Id != id || resp.JSON200.Subscribed {
		t.Fatalf("rename: %d %+v %v", resp.StatusCode(), resp.JSON200, resp.JSONDefault)
	}

	if _, err := u.Lookup("Z/Sub"); err != nil {
		t.Fatalf("child not renamed: %v", err)
	}

	// Repeated (lost response): renaming to the current name is a no-op.
	if again, _ := c.UpdateMailboxWithResponse(ctx, "alice", id, nil, adminapi.MailboxUpdate{Name: ptr("Z")}); again.StatusCode() != http.StatusOK {
		t.Fatalf("repeat: %d", again.StatusCode())
	}

	for name, body := range map[string]struct {
		id   string
		name string
	}{"inbox": {mailboxID(t, c, "INBOX"), "Old"}, "target exists": {id, "B"}} {
		if resp, _ := c.UpdateMailboxWithResponse(ctx, "alice", body.id, nil, adminapi.MailboxUpdate{Name: &body.name}); resp.StatusCode() != http.StatusConflict {
			t.Errorf("%s: %d", name, resp.StatusCode())
		}
	}

	bad := "garbage"
	if resp, _ := c.UpdateMailboxWithResponse(ctx, "alice", id, &adminapi.UpdateMailboxParams{IfMatch: &bad},
		adminapi.MailboxUpdate{Name: ptr("Shared/x")}); resp.StatusCode() != http.StatusBadRequest {
		t.Errorf("malformed If-Match: %d", resp.StatusCode())
	}

	if resp, _ := c.UpdateMailboxWithResponse(ctx, "alice", id, nil, adminapi.MailboxUpdate{Name: ptr("Shared/x")}); resp.StatusCode() != http.StatusBadRequest {
		t.Errorf("invalid target name: %d", resp.StatusCode())
	}
}

// sharedPointer reports whether bob holds a pointer to a shared mailbox.
func sharedPointer(t *testing.T, c *adminapi.ClientWithResponses) bool {
	t.Helper()

	resp, _ := c.ListSharedMailboxesWithResponse(context.Background(), "bob")

	return resp.JSON200 != nil && len(resp.JSON200.Items) == 1
}

func TestACLChanges(t *testing.T) {
	_, c := serveDiag(t, nil, RoleDiagnose, RoleProvision)
	ctx := context.Background()
	id := mailboxID(t, c, "Shared")

	set := func(identifier, rights string, mode adminapi.ACLChangeMode, ifMatch *string) *adminapi.SetACLEntryResponse {
		resp, _ := c.SetACLEntryWithResponse(ctx, "alice", id, identifier, &adminapi.SetACLEntryParams{IfMatch: ifMatch},
			adminapi.ACLChange{Rights: rights, Mode: &mode})

		return resp
	}

	add := set("bob", "w", adminapi.Add, nil)
	if add.JSON200 == nil || entry(add.JSON200, "bob") != "lrw" {
		t.Fatalf("add: %d %+v", add.StatusCode(), add.JSON200)
	}

	stale := `"acl-0"`
	if resp := set("bob", "k", adminapi.Add, &stale); resp.StatusCode() != http.StatusPreconditionFailed {
		t.Fatalf("stale If-Match: %d", resp.StatusCode())
	}

	rm := set("bob", "rw", adminapi.Remove, etagOf(add.HTTPResponse))
	if rm.JSON200 == nil || entry(rm.JSON200, "bob") != "l" || !sharedPointer(t, c) {
		t.Fatalf("remove: %d %+v", rm.StatusCode(), rm.JSON200)
	}

	del, _ := c.DeleteACLEntryWithResponse(ctx, "alice", id, "bob", nil)
	if del.JSON200 == nil || entry(del.JSON200, "bob") != "" || sharedPointer(t, c) {
		t.Fatalf("delete: %d %+v", del.StatusCode(), del.JSON200)
	}
}

func TestACLChangeRefusals(t *testing.T) {
	_, c := serveDiag(t, nil, RoleDiagnose, RoleProvision)
	ctx := context.Background()
	id := mailboxID(t, c, "Shared")

	set := func(identifier, rights string, ifMatch *string) *adminapi.SetACLEntryResponse {
		mode := adminapi.Replace
		resp, _ := c.SetACLEntryWithResponse(ctx, "alice", id, identifier, &adminapi.SetACLEntryParams{IfMatch: ifMatch},
			adminapi.ACLChange{Rights: rights, Mode: &mode})

		return resp
	}

	for name, tc := range map[string]struct {
		identifier, rights string
		status             int
	}{
		"alias":        {"BOB@example.org", "lr", http.StatusBadRequest},
		"unknown":      {"nobody", "lr", http.StatusNotFound},
		"owner":        {"alice", "lr", http.StatusBadRequest},
		"bad rights":   {"bob", "lz", http.StatusBadRequest},
		"anyone works": {"anyone", "lr", http.StatusOK},
	} {
		if resp := set(tc.identifier, tc.rights, nil); resp.StatusCode() != tc.status {
			t.Errorf("%s: %d %v", name, resp.StatusCode(), resp.JSONDefault)
		}
	}

	bad := "garbage"
	if resp := set("bob", "l", &bad); resp.StatusCode() != http.StatusBadRequest {
		t.Errorf("malformed If-Match: %d", resp.StatusCode())
	}
}

// TestConcurrentACLAdds: relative changes from many clients at once are
// all kept (computed in one versioned step on the server).
func TestConcurrentACLAdds(t *testing.T) {
	store, c := serveDiag(t, nil, RoleDiagnose, RoleProvision)
	id := mailboxID(t, c, "Shared")

	var wg sync.WaitGroup

	for _, r := range model.AllRights {
		wg.Add(1)

		go func(right string) {
			defer wg.Done()

			mode := adminapi.Add
			if resp, _ := c.SetACLEntryWithResponse(context.Background(), "alice", id, "carol", nil,
				adminapi.ACLChange{Rights: right, Mode: &mode}); resp == nil || resp.StatusCode() != http.StatusOK {
				t.Errorf("add %s failed", right)
			}
		}(string(r))
	}

	wg.Wait()

	u, _ := store.User("alice")
	if acl, _ := u.GetACL("Shared"); acl["carol"] != model.AllRights {
		t.Fatalf("lost updates: %q", acl["carol"])
	}
}

func entry(acl *adminapi.MailboxACL, identifier string) string {
	for _, e := range acl.Entries {
		if strings.EqualFold(e.Identifier, identifier) {
			return e.Rights
		}
	}

	return ""
}

// TestReplaceRepairsPointer: replacing with the same rights writes a
// missing pointer (the documented repair after a failed pointer write).
func TestReplaceRepairsPointer(t *testing.T) {
	store, c := serveDiag(t, nil, RoleDiagnose, RoleProvision)

	bob, _ := store.User("bob")
	if err := bob.RemoveSharedRef(model.SharedRef{Owner: "alice", Mailbox: "Shared"}); err != nil || sharedPointer(t, c) {
		t.Fatalf("pointer not removed: %v", err)
	}

	mode := adminapi.Replace
	if resp, _ := c.SetACLEntryWithResponse(context.Background(), "alice", mailboxID(t, c, "Shared"), "bob", nil,
		adminapi.ACLChange{Rights: "lr", Mode: &mode}); resp.StatusCode() != http.StatusOK || !sharedPointer(t, c) {
		t.Fatalf("pointer not repaired: %d", resp.StatusCode())
	}
}
