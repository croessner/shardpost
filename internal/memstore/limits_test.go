package memstore

import (
	"errors"
	"testing"
	"time"

	"shardpost/internal/model"
)

func wantLimit(t *testing.T, err error, kind model.LimitKind) {
	t.Helper()

	if !errors.Is(err, model.ErrLimit) || model.LimitKindOf(err) != kind {
		t.Fatalf("got %v, want the %s limit", err, kind)
	}
}

func must(t *testing.T, err error) {
	t.Helper()

	if err != nil {
		t.Fatal(err)
	}
}

// limitedUser is a user of a store with limits l.
func limitedUser(t *testing.T, l model.Limits) model.User {
	t.Helper()

	s := New()
	s.SetLimits(l)

	u, err := s.User("alice")
	must(t, err)

	return u
}

// TestMailboxLimits: the mailbox and the name limit, as cephstore keeps
// them: INBOX counts, a deletion makes room, a name used before is
// never refused.
func TestMailboxLimits(t *testing.T) {
	u := limitedUser(t, model.Limits{Mailboxes: 3, MailboxNames: 4})

	for _, n := range []string{"INBOX", "a", "b"} {
		must(t, u.CreateMailbox(n, nil))
	}

	wantLimit(t, u.CreateMailbox("c", nil), model.LimitMailboxes)
	must(t, u.DeleteMailbox("a"))
	must(t, u.CreateMailbox("c", nil)) // the fourth name
	must(t, u.DeleteMailbox("c"))
	wantLimit(t, u.CreateMailbox("d", nil), model.LimitMailboxNames)
	must(t, u.CreateMailbox("a", nil)) // used before
}

// TestKeywordLimit: deliveries and flag changes that add keywords beyond
// the limit are refused; known keywords (in any case) and system flags
// pass.
func TestKeywordLimit(t *testing.T) {
	u := limitedUser(t, model.Limits{KeywordsPerMailbox: 2})
	must(t, u.CreateMailbox("INBOX", nil))

	_, err := u.Deliver("INBOX", []byte("Subject: x\r\n\r\n"), []string{"$a", `\Seen`}, time.Now())
	must(t, err)

	_, err = u.Deliver("INBOX", []byte("Subject: y\r\n\r\n"), []string{"$b", "$c"}, time.Now())
	wantLimit(t, err, model.LimitKeywords)

	_, err = u.SetFlags("INBOX", []uint32{1}, 0, func(old []string) []string { return append(old, "$b", "$c") })
	wantLimit(t, err, model.LimitKeywords)

	_, err = u.SetFlags("INBOX", []uint32{1}, 0, func(old []string) []string { return append(old, "$A", `\Flagged`) })
	must(t, err)
}

// TestMetadataLimit: new entries beyond the limit are refused, a change
// that removes as many as it adds passes, server-managed entries always
// do.
func TestMetadataLimit(t *testing.T) {
	u := limitedUser(t, model.Limits{MetadataEntries: 1})
	one := "1"

	must(t, u.SetMetadata("", "alice", map[string]*string{"/private/a": &one}))
	wantLimit(t, u.SetMetadata("", "alice", map[string]*string{"/private/b": &one}), model.LimitMetadata)
	must(t, u.SetMetadata("", "alice", map[string]*string{"/private/a": nil, "/private/b": &one}))

	if ok, err := u.SwapMetadata("", "alice", model.ServerManagedPrefix+"x", "", &one); err != nil || !ok {
		t.Fatalf("server-managed entry: %v %v", ok, err)
	}

	_, err := u.SwapMetadata("", "alice", "/private/c", "", &one)
	wantLimit(t, err, model.LimitMetadata)
}

// TestSharedPointerLimit: a grant that adds a pointer beyond the
// grantee's limit is refused; changing a grant, "anyone" and removals
// pass.
func TestSharedPointerLimit(t *testing.T) {
	u := limitedUser(t, model.Limits{SharedPointers: 1})

	for _, n := range []string{"a", "b"} {
		must(t, u.CreateMailbox(n, nil))
	}

	must(t, u.SetACL("a", "bob", "lr"))
	must(t, u.SetACL("a", "bob", "lrs"))
	wantLimit(t, u.SetACL("b", "bob", "lr"), model.LimitSharedPointers)
	must(t, u.SetACL("b", "anyone", "lr"))
	must(t, u.SetACL("b", "bob", ""))
}
