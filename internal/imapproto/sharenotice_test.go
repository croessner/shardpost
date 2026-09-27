package imapproto

import (
	"testing"
	"time"

	"shardpost/internal/memstore"
	"shardpost/internal/sharenotice"
)

// TestSetACLRecordsShareNotice: SETACL and DELETEACL tell the grantee of
// the change (RFC 9670 3), with the account that made it; a change of
// "anyone" tells nobody.
func TestSetACLRecordsShareNotice(t *testing.T) {
	store := memstore.New()
	rec := &sharenotice.Recorder{Store: store}

	alice, _ := twoUsersWith(t, func(o *Options) {
		o.Store = store
		o.ShareNotices = rec
	})

	alice.ok(`CREATE Team`)
	alice.ok(`SETACL Team bob lr`)
	alice.ok(`SETACL Team bob +w`)
	alice.ok(`SETACL Team anyone l`)

	// The grant and the change coalesce into one notice.
	n := bobNotice(t, store)
	if n.Owner != "alice" || n.Name != "Team" || n.Old != "" || n.New != "lrw" || n.By != "alice" || n.MailboxID == "" {
		t.Errorf("the notice is %+v", n)
	}

	alice.ok(`DELETEACL Team bob`)

	// Revoked back to what anyone holds: bob still has l, and the notice
	// says so.
	if n := bobNotice(t, store); n.New != "l" {
		t.Errorf("after DELETEACL: %+v", n)
	}
}

// bobNotice is bob's one notice.
func bobNotice(t *testing.T, store *memstore.Store) sharenotice.Notice {
	t.Helper()

	bob, err := store.User("bob")
	if err != nil {
		t.Fatal(err)
	}
	defer bob.Close()

	l, err := sharenotice.Load(bob, "bob", sharenotice.Limits{}, time.Now())
	if err != nil || len(l.Notices) != 1 {
		t.Fatalf("bob's notices: %+v %v", l, err)
	}

	return l.Notices[0]
}
