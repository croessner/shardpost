package imapproto

import (
	"strings"
	"testing"
	"time"

	"shardpost/internal/auth"
	"shardpost/internal/memstore"
	"shardpost/internal/model"
)

// publicFolder creates a folder in the public namespace with the given
// ACL entries and one message.
func publicFolder(t *testing.T, store *memstore.Store, name string, acl map[string]string) model.User {
	t.Helper()

	pub, err := store.User(model.PublicNamespace)
	if err != nil {
		t.Fatal(err)
	}

	if err := pub.EnsureMailbox(name, nil); err != nil {
		t.Fatal(err)
	}

	for id, rights := range acl {
		if err := pub.SetACL(name, id, rights); err != nil {
			t.Fatal(err)
		}
	}

	if _, err := pub.Deliver(name, []byte("Subject: announcement\r\n\r\nread me\r\n"), nil, time.Now()); err != nil {
		t.Fatal(err)
	}

	return pub
}

// TestPublicNamespaceVisibility: a public folder is listed for an
// account that has the lookup right and stays invisible without it.
func TestPublicNamespaceVisibility(t *testing.T) {
	c, store := newTestServer(t)
	defer func() { _ = c.conn.Close() }()

	publicFolder(t, store, "Announcements", map[string]string{"anyone": "lr"})
	publicFolder(t, store, "Board", map[string]string{"boss@example.org": "lrswipkxte"})

	c.ok("LOGIN test test")

	lines := strings.Join(c.cmd(`LIST "" "*"`), "\n")
	if !strings.Contains(lines, `"/" Public/Announcements`) {
		t.Errorf("public folder not listed:\n%s", lines)
	}

	if strings.Contains(lines, "Board") {
		t.Errorf("folder without rights listed:\n%s", lines)
	}

	ns := strings.Join(c.cmd("NAMESPACE"), "\n")
	if !strings.Contains(ns, `(("Public/" "/"))`) {
		t.Errorf("NAMESPACE lacks the public namespace: %s", ns)
	}
}

// TestPublicFolderRights: reading is allowed with r, appending needs i,
// and a folder without rights does not exist for the account.
func TestPublicFolderRights(t *testing.T) {
	c, store := newTestServer(t)
	defer func() { _ = c.conn.Close() }()

	publicFolder(t, store, "Announcements", map[string]string{"anyone": "lr"})
	publicFolder(t, store, "Board", map[string]string{"boss@example.org": "lrswipkxte"})

	c.ok("LOGIN test test")
	c.ok("SELECT Public/Announcements")

	if lines := strings.Join(c.cmd("FETCH 1 (BODY[HEADER.FIELDS (SUBJECT)])"), "\n"); !strings.Contains(lines, "announcement") {
		t.Errorf("message not readable: %s", lines)
	}

	if lines := strings.Join(c.cmd(`APPEND Public/Announcements {5+}`+"\r\nhello"), "\n"); !strings.Contains(lines, "NOPERM") {
		t.Errorf("APPEND without the insert right: %s", lines)
	}

	if lines := strings.Join(c.cmd("SELECT Public/Board"), "\n"); !strings.Contains(lines, "NONEXISTENT") {
		t.Errorf("a folder without rights must not exist: %s", lines)
	}
}

// TestPublicSubscription: subscriptions belong to the account, not to
// the folder, and survive a rename of the folder.
func TestPublicSubscription(t *testing.T) {
	c, store := newTestServer(t)
	defer func() { _ = c.conn.Close() }()

	pub := publicFolder(t, store, "Announcements", map[string]string{"anyone": "lr"})

	c.ok("LOGIN test test")
	c.ok("SUBSCRIBE Public/Announcements")

	if lines := strings.Join(c.cmd(`LSUB "" "*"`), "\n"); !strings.Contains(lines, `"/" Public/Announcements`) {
		t.Fatalf("not subscribed: %s", lines)
	}

	if err := pub.RenameMailbox("Announcements", "News"); err != nil {
		t.Fatal(err)
	}

	if lines := strings.Join(c.cmd(`LSUB "" "*"`), "\n"); !strings.Contains(lines, `"/" Public/News`) {
		t.Errorf("subscription did not follow the rename: %s", lines)
	}

	c.ok("UNSUBSCRIBE Public/News")

	if lines := strings.Join(c.cmd(`LSUB "" "*"`), "\n"); strings.Contains(lines, "Public/News") {
		t.Errorf("still subscribed: %s", lines)
	}
}

// TestPublicGrantWritesNoPointer: a right granted to an account must not
// write a share pointer into that account's registry, or the folder
// would be listed twice and a folder open to many accounts would cost a
// write per account.
func TestPublicGrantWritesNoPointer(t *testing.T) {
	c, store := newTestServer(t)
	defer func() { _ = c.conn.Close() }()

	publicFolder(t, store, "Team", map[string]string{"test": "lrswipkxte"})

	u, err := store.User("test")
	if err != nil {
		t.Fatal(err)
	}

	refs, err := u.ListShared()
	if err != nil {
		t.Fatal(err)
	}

	if len(refs) != 0 {
		t.Fatalf("share pointers written for a public folder: %+v", refs)
	}

	c.ok("LOGIN test test")

	lines := strings.Join(c.cmd(`LIST "" "*"`), "\n")
	if strings.Count(lines, "Team") != 1 || !strings.Contains(lines, `"/" Public/Team`) {
		t.Fatalf("public folder not listed exactly once:\n%s", lines)
	}

	if strings.Contains(lines, "Other Users/~public") {
		t.Errorf("public folder listed as a shared one:\n%s", lines)
	}
}

// TestPublicPrivateSeen: two accounts read the same public folder and
// each sees its own \Seen, while every other flag is shared. The
// combined MODSEQ keeps CONDSTORE usable.
func TestPublicPrivateSeen(t *testing.T) {
	alice, store := newTestServer(t, func(o *Options, _ *memstore.Store) {
		o.Auth = auth.Static{"alice": "pw", "bob": "pw"}
	})
	defer func() { _ = alice.conn.Close() }()

	publicFolder(t, store, "Team", map[string]string{"anyone": "lrswipkxte"})

	bob, _ := newTestServerClient(t, alice)
	defer func() { _ = bob.conn.Close() }()

	alice.ok("LOGIN alice pw")
	bob.ok("LOGIN bob pw")

	for _, c := range []*client{alice, bob} {
		if lines := strings.Join(c.cmd("SELECT Public/Team"), "\n"); !strings.Contains(lines, "1 EXISTS") {
			t.Fatalf("select: %s", lines)
		}
	}

	// Alice reads the message and flags it for the team.
	alice.ok(`STORE 1 +FLAGS (\Seen \Flagged)`)

	if lines := strings.Join(alice.cmd("FETCH 1 (FLAGS)"), "\n"); !strings.Contains(lines, `\Seen`) || !strings.Contains(lines, `\Flagged`) {
		t.Fatalf("alice after store: %s", lines)
	}

	// Bob sees the shared flag but not Alice's reading state.
	bob.ok("NOOP")

	lines := strings.Join(bob.cmd("FETCH 1 (FLAGS)"), "\n")
	if !strings.Contains(lines, `\Flagged`) {
		t.Errorf("shared flag missing for bob: %s", lines)
	}

	if strings.Contains(lines, `\Seen`) {
		t.Errorf("alice's reading state leaked to bob: %s", lines)
	}

	if lines := strings.Join(bob.cmd(`STATUS Public/Team (UNSEEN)`), "\n"); !strings.Contains(lines, "UNSEEN 1") {
		t.Errorf("bob's unseen count: %s", lines)
	}

	if lines := strings.Join(alice.cmd(`STATUS Public/Team (UNSEEN)`), "\n"); !strings.Contains(lines, "UNSEEN 0") {
		t.Errorf("alice's unseen count: %s", lines)
	}

	// Bob marks it read for himself; Alice keeps her own state.
	bob.ok(`STORE 1 +FLAGS (\Seen)`)
	alice.ok("NOOP")

	if lines := strings.Join(alice.cmd("FETCH 1 (FLAGS)"), "\n"); !strings.Contains(lines, `\Seen`) {
		t.Errorf("alice lost her reading state: %s", lines)
	}

	// Unread again for Alice only.
	alice.ok(`STORE 1 -FLAGS (\Seen)`)

	if lines := strings.Join(alice.cmd("FETCH 1 (FLAGS)"), "\n"); strings.Contains(lines, `\Seen`) {
		t.Errorf("alice still seen: %s", lines)
	}

	if lines := strings.Join(bob.cmd(`STATUS Public/Team (UNSEEN)`), "\n"); !strings.Contains(lines, "UNSEEN 0") {
		t.Errorf("bob's state changed with alice's: %s", lines)
	}
}
