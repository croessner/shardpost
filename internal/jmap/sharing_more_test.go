package jmap

import (
	"testing"
)

// TestSharingMailboxQueryAccount: Mailbox/query answers for the shared
// account it was asked about.
func TestSharingMailboxQueryAccount(t *testing.T) {
	f := newSharing(t)

	res := f.mail(t, "Mailbox/query", `{"accountId":"alice"}`)
	if res["accountId"] != "alice" || len(res["ids"].([]any)) != 5 {
		t.Errorf("Mailbox/query in alice: %#v", res)
	}
}
