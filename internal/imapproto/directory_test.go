package imapproto

import (
	"testing"

	"shardpost/internal/memstore"
	"shardpost/internal/model"
)

// TestSetACLToPurgingAccount: a grant to an account whose purge already
// deleted its registry is refused with NO [UNAVAILABLE] and leaves the
// ACL as it was; otherwise it would pass to whoever gets the name next.
func TestSetACLToPurgingAccount(t *testing.T) {
	var store *memstore.Store

	alice, _ := twoUsersWith(t, func(o *Options) { store = o.Store.(*memstore.Store) })

	carol, _ := store.User("carol")
	if err := carol.CreateMailbox("INBOX", nil); err != nil {
		t.Fatal(err)
	}

	if err := carol.(model.DirectoryPurger).MarkPurging("job-1"); err != nil {
		t.Fatal(err)
	}

	if _, err := carol.(model.NamespacePurger).PurgeNamespace(func() error { return nil }); err != nil {
		t.Fatal(err)
	}

	alice.ok("CREATE Shared")

	mustHave(t, alice.cmd("SETACL Shared carol lr"), "NO [UNAVAILABLE]")

	lines := alice.ok("GETACL Shared")
	mustLack(t, lines, "carol")

	// Granting someone else still works.
	alice.ok("SETACL Shared bob lr")
}
