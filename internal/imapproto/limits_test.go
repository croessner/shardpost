package imapproto

import (
	"testing"

	"shardpost/internal/memstore"
	"shardpost/internal/model"
)

// limitServer starts a server whose store holds accounts to l, with no
// autocreated mailboxes besides INBOX, and logs in.
func limitServer(t *testing.T, l model.Limits) *client {
	t.Helper()

	c, _ := newTestServer(t, func(o *Options, s *memstore.Store) {
		o.Autocreate = nil

		s.SetLimits(l)
	})
	c.ok("LOGIN test test")

	return c
}

// TestMailboxLimitIMAP: CREATE beyond limits.mailboxes is NO [LIMIT];
// a deletion makes room again, RENAME is not a creation.
func TestMailboxLimitIMAP(t *testing.T) {
	c := limitServer(t, model.Limits{Mailboxes: 3})

	c.ok("CREATE a")
	c.ok("CREATE b")
	mustHave(t, c.cmd("CREATE c"), "NO [LIMIT]")
	c.ok("RENAME b b2")
	c.ok("DELETE a")
	c.ok("CREATE c")
	mustHave(t, c.cmd("CREATE d"), "NO [LIMIT]")
	// An existing mailbox is still answered as one.
	mustHave(t, c.cmd("CREATE c"), "ALREADYEXISTS")
}

// TestMailboxNameLimitIMAP: names used before may be used again at
// limits.mailbox_names, new ones are NO [LIMIT].
func TestMailboxNameLimitIMAP(t *testing.T) {
	c := limitServer(t, model.Limits{MailboxNames: 3}) // INBOX is one of them

	c.ok("CREATE a")
	c.ok("CREATE b")
	c.ok("DELETE a")
	mustHave(t, c.cmd("CREATE c"), "NO [LIMIT]")
	c.ok("CREATE a")
	// RENAME does not use up a name.
	c.ok("RENAME a renamed")
}

// TestKeywordLimitIMAP: STORE, APPEND and COPY that would take a mailbox
// beyond limits.keywords_per_mailbox are NO [LIMIT]; known keywords and
// system flags always pass.
func TestKeywordLimitIMAP(t *testing.T) {
	c := limitServer(t, model.Limits{KeywordsPerMailbox: 2})

	msg := "Subject: x\r\n\r\nbody\r\n"
	c.ok("APPEND INBOX ($a) {%d+}\r\n%s", len(msg), msg)
	mustHave(t, c.cmd("APPEND INBOX ($b $c) {%d+}\r\n%s", len(msg), msg), "NO [LIMIT]")

	c.ok("SELECT INBOX")
	c.ok("STORE 1 +FLAGS ($b)")
	mustHave(t, c.cmd("STORE 1 +FLAGS ($c)"), "NO [LIMIT]")
	c.ok("STORE 1 +FLAGS ($A \\Flagged)")
	c.ok("STORE 1 FLAGS ($a $b \\Seen)")
	c.ok("STORE 1 -FLAGS ($a $b)")

	c.ok("CREATE Other")
	c.ok("STORE 1 +FLAGS ($a $b)")
	c.ok("COPY 1 Other")

	c.ok("CREATE Third")
	c.ok("APPEND Third ($x $y) {%d+}\r\n%s", len(msg), msg)
	mustHave(t, c.cmd("COPY 1 Third"), "NO [LIMIT]")
	mustHave(t, c.cmd("MOVE 1 Third"), "NO [LIMIT]")
	mustHave(t, c.ok("STATUS Third (MESSAGES)"), "MESSAGES 1")
}

// TestMetadataLimitIMAP: SETMETADATA beyond limits.metadata_entries is
// NO [METADATA TOOMANY]; replacing and removing entries always works.
func TestMetadataLimitIMAP(t *testing.T) {
	c := limitServer(t, model.Limits{MetadataEntries: 2})

	c.ok(`SETMETADATA "" (/private/a "1" /private/b "2")`)
	mustHave(t, c.cmd(`SETMETADATA "" (/private/c "3")`), "NO [METADATA TOOMANY]")
	c.ok(`SETMETADATA "" (/private/a "one")`)
	c.ok(`SETMETADATA "" (/private/a NIL /private/c "3")`)
	mustHave(t, c.ok(`GETMETADATA "" (/private/a /private/b /private/c)`), `/private/c "3"`)
}

// TestSharedPointerLimitIMAP: a SETACL that would share one mailbox more
// with a grantee than limits.shared_pointers is NO [LIMIT] and leaves the
// ACL alone; changing the rights of an existing grant always works.
func TestSharedPointerLimitIMAP(t *testing.T) {
	alice, bob := twoUsersWith(t, func(o *Options) {
		o.Store.(*memstore.Store).SetLimits(model.Limits{SharedPointers: 1})
	})

	alice.ok("CREATE A")
	alice.ok("CREATE B")
	alice.ok("SETACL A bob lr")
	alice.ok("SETACL A bob lrs")
	mustHave(t, alice.cmd("SETACL B bob lr"), "NO [LIMIT]")
	mustLack(t, alice.ok("GETACL B"), "bob")
	// anyone has no pointer.
	alice.ok("SETACL B anyone lr")

	mustHave(t, bob.ok(`LIST "" "Shared/*"`), "Shared/alice/A")

	alice.ok("DELETEACL A bob")
	alice.ok("SETACL B bob lr")
}
