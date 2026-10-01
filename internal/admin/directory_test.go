package admin

import (
	"context"
	"net/http"
	"testing"

	"shardpost/internal/adminapi"
	"shardpost/internal/model"
)

func TestRepairAccountDirectory(t *testing.T) {
	store, c := serveDiag(t, nil, RoleDiagnose, "repair")
	ctx := context.Background()

	// A purge that is not running left "gone" purging without registry.
	gone, _ := store.User("gone")
	_ = gone.EnsureMailbox("INBOX", nil)
	_ = gone.(model.DirectoryPurger).MarkPurging("job-20261001T000000Z-000000000000")
	_, _ = gone.(model.NamespacePurger).PurgeNamespace(func() error { return nil })

	resp, err := c.RepairAccountDirectoryWithResponse(ctx, "gone")
	if err != nil || resp.JSON200 == nil {
		t.Fatalf("repair: %v %d %v", err, resp.StatusCode(), resp.JSONDefault)
	}

	rep := resp.JSON200
	if rep.Action != adminapi.Removed || !rep.Found || rep.Before == nil || rep.Before.State != adminapi.Purging || rep.After != nil {
		t.Fatalf("repair %+v", rep)
	}

	// Again: nothing left.
	resp, _ = c.RepairAccountDirectoryWithResponse(ctx, "gone")
	if resp.JSON200 == nil || resp.JSON200.Action != adminapi.None || resp.JSON200.Found {
		t.Fatalf("second repair %d %+v", resp.StatusCode(), resp.JSON200)
	}

	if got := operation("/admin/v1/accounts/gone/directory/repair"); got != "directory_repair" {
		t.Errorf("operation label %q", got)
	}
}

// TestGrantToPurgingAccount: the admin ACL change names the purge, not
// an import, and writes nothing.
func TestGrantToPurgingAccount(t *testing.T) {
	store, c := serveDiag(t, nil, RoleDiagnose, RoleProvision)
	id := mailboxID(t, c, "A")

	carol, _ := store.User("carol")
	_ = carol.EnsureMailbox("INBOX", nil)
	_ = carol.(model.DirectoryPurger).MarkPurging("job-1")
	_, _ = carol.(model.NamespacePurger).PurgeNamespace(func() error { return nil })

	resp, err := c.SetACLEntryWithResponse(context.Background(), "alice", id, "carol", nil, adminapi.ACLChange{Rights: "lr"})
	if err != nil || resp.StatusCode() != http.StatusConflict || resp.JSONDefault == nil || resp.JSONDefault.Code != adminapi.AccountPurging {
		t.Fatalf("grant: %v %d %+v", err, resp.StatusCode(), resp.JSONDefault)
	}

	alice, _ := store.User("alice")
	if acl, _ := alice.GetACL("A"); acl["carol"] != "" {
		t.Fatalf("ACL written: %v", acl)
	}
}

func TestRepairAccountDirectoryNeedsRole(t *testing.T) {
	_, c := serveDiag(t, nil, RoleDiagnose)

	resp, err := c.RepairAccountDirectoryWithResponse(context.Background(), "alice")
	if err != nil || resp.StatusCode() != http.StatusForbidden {
		t.Fatalf("without role: %v %d", err, resp.StatusCode())
	}
}
