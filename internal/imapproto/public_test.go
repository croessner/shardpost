package imapproto

import (
	"fmt"
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

	if strings.Contains(lines, "Shared/~public") {
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

// highestModSeq reads HIGHESTMODSEQ of a mailbox through STATUS.
func highestModSeq(t *testing.T, c *client, mailbox string) uint64 {
	t.Helper()

	lines := strings.Join(c.cmd("STATUS %s (HIGHESTMODSEQ)", mailbox), "\n")

	i := strings.Index(lines, "HIGHESTMODSEQ ")
	if i < 0 {
		t.Fatalf("no HIGHESTMODSEQ: %s", lines)
	}

	var n uint64
	if _, err := fmt.Sscan(strings.TrimLeft(lines[i+len("HIGHESTMODSEQ "):], " "), &n); err != nil {
		t.Fatalf("HIGHESTMODSEQ: %s", lines)
	}

	return n
}

// TestPublicPrivateSeenModSeq: a private \Seen is a change of the public
// mailbox like any other. Its MODSEQ comes from the mailbox's own
// counter, so it neither collides with the next shared change nor
// stalls a session: the STORE reports the new state, CHANGEDSINCE finds
// it, and a shared change made afterwards by somebody else still
// reaches the account that read first.
func TestPublicPrivateSeenModSeq(t *testing.T) {
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
		c.ok("ENABLE CONDSTORE")
		c.ok("SELECT Public/Team")
	}

	before := highestModSeq(t, alice, "Public/Team")

	// The STORE must report the message although nothing shared changed.
	lines := strings.Join(alice.cmd(`STORE 1 +FLAGS (\Seen)`), "\n")
	if !strings.Contains(lines, "FETCH") || !strings.Contains(lines, `\Seen`) {
		t.Fatalf("private store without a FETCH response: %s", lines)
	}

	after := highestModSeq(t, alice, "Public/Team")
	if after <= before {
		t.Fatalf("HIGHESTMODSEQ %d did not move past %d", after, before)
	}

	if lines := strings.Join(alice.cmd("FETCH 1 (FLAGS) (CHANGEDSINCE %d)", before), "\n"); !strings.Contains(lines, `\Seen`) {
		t.Errorf("CHANGEDSINCE %d does not report the private change: %s", before, lines)
	}

	// The number the private change took must not be handed out again:
	// bob's shared change has to reach alice.
	bob.ok(`STORE 1 +FLAGS (\Flagged)`)

	if lines := strings.Join(alice.cmd("NOOP"), "\n"); !strings.Contains(lines, `\Flagged`) {
		t.Errorf("shared change after a private one did not reach the other session: %s", lines)
	}
}

// TestPublicPrivateSeenOtherSession: a second session of the same
// account learns of a mark set by the first one, although the mark is
// in no shared record.
func TestPublicPrivateSeenOtherSession(t *testing.T) {
	first, store := newTestServer(t)
	defer func() { _ = first.conn.Close() }()

	publicFolder(t, store, "Team", map[string]string{"anyone": "lrswipkxte"})

	second, _ := newTestServerClient(t, first)
	defer func() { _ = second.conn.Close() }()

	for _, c := range []*client{first, second} {
		c.ok("LOGIN test test")
		c.ok("SELECT Public/Team")
	}

	first.ok(`STORE 1 +FLAGS (\Seen)`)

	if lines := strings.Join(second.cmd("NOOP"), "\n"); !strings.Contains(lines, `\Seen`) {
		t.Errorf("the account's other session did not learn of its own mark: %s", lines)
	}

	if lines := strings.Join(second.cmd("FETCH 1 (FLAGS)"), "\n"); !strings.Contains(lines, `\Seen`) {
		t.Errorf("snapshot of the other session: %s", lines)
	}
}

// TestPublicPrivateSeenUnchangedSince: a conditional STORE fails for a
// message whose private mark is newer than the client's MODSEQ, and
// leaves the mark as it is.
func TestPublicPrivateSeenUnchangedSince(t *testing.T) {
	c, store := newTestServer(t)
	defer func() { _ = c.conn.Close() }()

	publicFolder(t, store, "Team", map[string]string{"anyone": "lrswipkxte"})

	c.ok("LOGIN test test")
	c.ok("ENABLE CONDSTORE")
	c.ok("SELECT Public/Team")

	before := highestModSeq(t, c, "Public/Team")
	c.ok(`STORE 1 +FLAGS (\Seen)`)

	lines := strings.Join(c.cmd(`STORE 1 (UNCHANGEDSINCE %d) -FLAGS (\Seen)`, before), "\n")
	if !strings.Contains(lines, "MODIFIED 1") {
		t.Errorf("conditional store against an older MODSEQ: %s", lines)
	}

	if lines := strings.Join(c.cmd("FETCH 1 (FLAGS)"), "\n"); !strings.Contains(lines, `\Seen`) {
		t.Errorf("the refused store changed the mark anyway: %s", lines)
	}
}

// TestPublicPrivateSeenPruned: marks of messages that are gone do not
// pile up. Nobody can clean up another account's namespace, so each
// account does it when it opens the folder.
func TestPublicPrivateSeenPruned(t *testing.T) {
	c, store := newTestServer(t)
	defer func() { _ = c.conn.Close() }()

	pub := publicFolder(t, store, "Team", map[string]string{"anyone": "lrswipkxte"})

	c.ok("LOGIN test test")
	c.ok("SELECT Public/Team")
	c.ok(`STORE 1 +FLAGS (\Seen)`)
	c.ok("CLOSE")

	mb, err := pub.Lookup("Team")
	if err != nil {
		t.Fatal(err)
	}

	msgs, err := pub.ListMessages(mb.ID)
	if err != nil {
		t.Fatal(err)
	}

	if err := pub.Expunge("Team", msgs); err != nil {
		t.Fatal(err)
	}

	c.ok("SELECT Public/Team")

	u, err := store.User("test")
	if err != nil {
		t.Fatal(err)
	}

	marks, err := u.PrivateSeen(mb.ID, mb.UIDValidity)
	if err != nil {
		t.Fatal(err)
	}

	if len(marks) != 0 {
		t.Errorf("marks of expunged messages kept: %+v", marks)
	}
}

// TestPublicSharedSeenIsTheDefault: a message that arrives read — from
// a delivery, an APPEND or a migration — is read for everyone who has
// no mark of their own yet, and a shared flag set by somebody else does
// not change that. Only the account's own mark overrides it, and only
// for that account.
func TestPublicSharedSeenIsTheDefault(t *testing.T) {
	alice, store := newTestServer(t, func(o *Options, _ *memstore.Store) {
		o.Auth = auth.Static{"alice": "pw", "bob": "pw"}
	})
	defer func() { _ = alice.conn.Close() }()

	pub := publicFolder(t, store, "Team", map[string]string{"anyone": "lrswipkxte"})

	if _, err := pub.Deliver("Team", []byte("Subject: read\r\n\r\nalready read\r\n"), []string{flagSeenLower}, time.Now()); err != nil {
		t.Fatal(err)
	}

	bob, _ := newTestServerClient(t, alice)
	defer func() { _ = bob.conn.Close() }()

	alice.ok("LOGIN alice pw")
	bob.ok("LOGIN bob pw")

	for _, c := range []*client{alice, bob} {
		c.ok("SELECT Public/Team")
	}

	if lines := strings.Join(alice.cmd("FETCH 2 (FLAGS)"), "\n"); !strings.Contains(lines, `\Seen`) {
		t.Fatalf("a message delivered as read is unread without a mark: %s", lines)
	}

	// A shared flag from somebody else must not touch it.
	bob.ok(`STORE 2 +FLAGS (\Flagged)`)
	alice.ok("NOOP")

	if lines := strings.Join(alice.cmd("FETCH 2 (FLAGS)"), "\n"); !strings.Contains(lines, `\Seen`) {
		t.Errorf("another account's flag cleared the default: %s", lines)
	}

	// Alice's own mark decides for her, and only for her.
	alice.ok(`STORE 2 -FLAGS (\Seen)`)

	if lines := strings.Join(alice.cmd("FETCH 2 (FLAGS)"), "\n"); strings.Contains(lines, `\Seen`) {
		t.Errorf("alice's own mark was not applied: %s", lines)
	}

	if lines := strings.Join(bob.cmd(`STATUS Public/Team (UNSEEN)`), "\n"); !strings.Contains(lines, "UNSEEN 1") {
		t.Errorf("alice's mark changed bob's unread count: %s", lines)
	}
}

// TestPublicFetchBodyIsPrivate: the implicit \Seen of a FETCH belongs to
// the account like an explicit STORE does. Most clients fetch BODY[]
// rather than BODY.PEEK[], so writing the shared flag here would mark
// the message read for everyone who has no mark of their own.
func TestPublicFetchBodyIsPrivate(t *testing.T) {
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
		c.ok("SELECT Public/Team")
	}

	if lines := strings.Join(alice.cmd("FETCH 1 (BODY[])"), "\n"); !strings.Contains(lines, `\Seen`) {
		t.Fatalf("the reader's own mark was not set: %s", lines)
	}

	bob.ok("NOOP")

	if lines := strings.Join(bob.cmd("FETCH 1 (FLAGS)"), "\n"); strings.Contains(lines, `\Seen`) {
		t.Errorf("another account's BODY[] marked the message read: %s", lines)
	}

	if lines := strings.Join(bob.cmd("STATUS Public/Team (UNSEEN)"), "\n"); !strings.Contains(lines, "UNSEEN 1") {
		t.Errorf("unread count after another account's BODY[]: %s", lines)
	}
}

// TestPublicReplacingStoreKeepsSharedSeen: a replacing STORE by one
// account removes flags it does not name — but not the shared \Seen,
// which is the default of everyone who has no mark.
func TestPublicReplacingStoreKeepsSharedSeen(t *testing.T) {
	alice, store := newTestServer(t, func(o *Options, _ *memstore.Store) {
		o.Auth = auth.Static{"alice": "pw", "bob": "pw"}
	})
	defer func() { _ = alice.conn.Close() }()

	pub := publicFolder(t, store, "Team", map[string]string{"anyone": "lrswipkxte"})

	if _, err := pub.Deliver("Team", []byte("Subject: read\r\n\r\nalready read\r\n"), []string{flagSeenLower}, time.Now()); err != nil {
		t.Fatal(err)
	}

	bob, _ := newTestServerClient(t, alice)
	defer func() { _ = bob.conn.Close() }()

	alice.ok("LOGIN alice pw")
	bob.ok("LOGIN bob pw")

	for _, c := range []*client{alice, bob} {
		c.ok("SELECT Public/Team")
	}

	alice.ok(`STORE 2 FLAGS (\Flagged)`)
	bob.ok("NOOP")

	if lines := strings.Join(bob.cmd("FETCH 2 (FLAGS)"), "\n"); !strings.Contains(lines, `\Seen`) {
		t.Errorf("a replacing STORE cleared the shared default for everyone: %s", lines)
	}
}

// TestPublicQresyncReportsOwnMark: a client that reconnects with
// QRESYNC and its last MODSEQ is told about its own mark, which is in
// no shared record.
func TestPublicQresyncReportsOwnMark(t *testing.T) {
	c, store := newTestServer(t)
	defer func() { _ = c.conn.Close() }()

	publicFolder(t, store, "Team", map[string]string{"anyone": "lrswipkxte"})

	c.ok("LOGIN test test")
	c.ok("ENABLE QRESYNC")
	c.ok("SELECT Public/Team")

	before := highestModSeq(t, c, "Public/Team")
	c.ok(`STORE 1 +FLAGS (\Seen)`)
	c.ok("CLOSE")

	pub, err := store.User(model.PublicNamespace)
	if err != nil {
		t.Fatal(err)
	}

	mb, err := pub.Lookup("Team")
	if err != nil {
		t.Fatal(err)
	}

	found := false

	for _, line := range c.cmd("SELECT Public/Team (QRESYNC (%d %d))", mb.UIDValidity, before) {
		if strings.Contains(line, "FETCH") && strings.Contains(line, "UID 1") && strings.Contains(line, `\Seen`) {
			found = true
		}
	}

	if !found {
		t.Errorf("QRESYNC since %d does not report the account's own mark", before)
	}
}

// TestPublicUnchangedSinceWithoutSeen: a conditional STORE that does
// not mention \Seen is still about the message, and the MODSEQ it is
// compared against is the account's own — a mark set meanwhile has to
// make it fail.
func TestPublicUnchangedSinceWithoutSeen(t *testing.T) {
	first, store := newTestServer(t)
	defer func() { _ = first.conn.Close() }()

	publicFolder(t, store, "Team", map[string]string{"anyone": "lrswipkxte"})

	second, _ := newTestServerClient(t, first)
	defer func() { _ = second.conn.Close() }()

	for _, c := range []*client{first, second} {
		c.ok("LOGIN test test")
		c.ok("ENABLE CONDSTORE")
		c.ok("SELECT Public/Team")
	}

	before := highestModSeq(t, second, "Public/Team")

	first.ok(`STORE 1 +FLAGS (\Seen)`)

	lines := strings.Join(second.cmd(`STORE 1 (UNCHANGEDSINCE %d) +FLAGS (\Flagged)`, before), "\n")
	if !strings.Contains(lines, "MODIFIED 1") {
		t.Errorf("a mark set meanwhile did not make the conditional store fail: %s", lines)
	}

	if lines := strings.Join(first.cmd("FETCH 1 (FLAGS)"), "\n"); strings.Contains(lines, `\Flagged`) {
		t.Errorf("the refused store set the shared flag anyway: %s", lines)
	}
}

// TestPublicFetchBatchesMarks: a FETCH over a whole public folder
// writes the account's marks in one step. A mark takes a MODSEQ of the
// mailbox and announces a change, so one per message would announce as
// many changes as the folder has messages.
func TestPublicFetchBatchesMarks(t *testing.T) {
	c, store := newTestServer(t)
	defer func() { _ = c.conn.Close() }()

	pub := publicFolder(t, store, "Team", map[string]string{"anyone": "lrswipkxte"})

	for i := 0; i < 4; i++ {
		if _, err := pub.Deliver("Team", []byte("Subject: more\r\n\r\ntext\r\n"), nil, time.Now()); err != nil {
			t.Fatal(err)
		}
	}

	c.ok("LOGIN test test")
	c.ok("ENABLE CONDSTORE")
	c.ok("SELECT Public/Team")

	before := highestModSeq(t, c, "Public/Team")

	lines := strings.Join(c.cmd("FETCH 1:* (BODY[])"), "\n")
	if strings.Count(lines, `\Seen`) != 5 {
		t.Errorf("not every message was marked read: %s", lines)
	}

	if after := highestModSeq(t, c, "Public/Team"); after != before+1 {
		t.Errorf("HIGHESTMODSEQ went from %d to %d, want one step for the whole fetch", before, after)
	}

	if lines := strings.Join(c.cmd("STATUS Public/Team (UNSEEN)"), "\n"); !strings.Contains(lines, "UNSEEN 0") {
		t.Errorf("unread after reading everything: %s", lines)
	}
}
