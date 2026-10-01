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

func TestRepairAccountDirectoryNeedsRole(t *testing.T) {
	_, c := serveDiag(t, nil, RoleDiagnose)

	resp, err := c.RepairAccountDirectoryWithResponse(context.Background(), "alice")
	if err != nil || resp.StatusCode() != http.StatusForbidden {
		t.Fatalf("without role: %v %d", err, resp.StatusCode())
	}
}
