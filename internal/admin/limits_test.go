package admin

import (
	"context"
	"net/http"
	"testing"

	"shardpost/internal/adminapi"
	"shardpost/internal/model"
)

// TestCreateMailboxAtLimit: a mailbox beyond limits.mailboxes is 409
// limit_exceeded, not a server error.
func TestCreateMailboxAtLimit(t *testing.T) {
	store, c := serveDiag(t, nil, RoleDiagnose, RoleProvision)
	ctx := context.Background()

	u, err := store.User("alice")
	if err != nil {
		t.Fatal(err)
	}

	have, err := u.ListMailboxes()
	if err != nil {
		t.Fatal(err)
	}

	store.SetLimits(model.Limits{Mailboxes: len(have) + 1})

	if resp, _ := c.CreateMailboxWithResponse(ctx, "alice", adminapi.MailboxCreate{Name: "One"}); resp.StatusCode() != http.StatusCreated {
		t.Fatalf("below the limit: %d", resp.StatusCode())
	}

	resp, _ := c.CreateMailboxWithResponse(ctx, "alice", adminapi.MailboxCreate{Name: "Two"})
	if resp.StatusCode() != http.StatusConflict || resp.JSONDefault == nil || resp.JSONDefault.Code != adminapi.LimitExceeded {
		t.Fatalf("at the limit: %d %v", resp.StatusCode(), resp.JSONDefault)
	}
}
