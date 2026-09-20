package imapproto

import (
	"strings"
	"testing"
	"time"

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
