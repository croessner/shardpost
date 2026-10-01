package imapproto

import (
	"bufio"
	"errors"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	"shardpost/internal/auth"
	"shardpost/internal/memstore"
	"shardpost/internal/model"
)

// hubServer starts a server for alice and bob and returns it with a
// dialer that logs a user in.
func hubServer(t *testing.T, customize ...func(*Options)) (*Server, *memstore.Store, func(user string) *client) {
	t.Helper()

	store := memstore.New()
	opts := Options{Store: store, Auth: auth.Static{"alice": "a", "bob": "b"}, InsecureAuth: true, Autocreate: DefaultSpecialFolders,
		SharedPrefix: DefaultSharedPrefix}

	for _, fn := range customize {
		fn(&opts)
	}

	srv := New(opts)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	go func() { _ = srv.Serve(ln) }()

	t.Cleanup(func() { _ = srv.Close() })

	dial := func(user string) *client {
		nc, err := net.Dial("tcp", ln.Addr().String())
		if err != nil {
			t.Fatal(err)
		}

		c := &client{t: t, conn: nc, br: bufio.NewReader(nc), addr: ln.Addr().String()}
		if g, _ := c.br.ReadString('\n'); !strings.HasPrefix(g, "* OK") {
			t.Fatalf("greeting %q", g)
		}

		c.ok("LOGIN %s %s", user, user[:1])

		return c
	}

	return srv, store, dial
}

// idle starts IDLE and waits for the continuation.
func (c *client) idle(tag string) {
	c.t.Helper()

	if _, err := fmt.Fprintf(c.conn, "%s IDLE\r\n", tag); err != nil {
		c.t.Fatal(err)
	}

	if line, _ := c.br.ReadString('\n'); !strings.HasPrefix(line, "+ ") {
		c.t.Fatalf("IDLE: %q", line)
	}
}

// done ends IDLE.
func (c *client) done(tag string) {
	c.t.Helper()

	if _, err := fmt.Fprintf(c.conn, "DONE\r\n"); err != nil {
		c.t.Fatal(err)
	}

	for {
		line, err := c.br.ReadString('\n')
		if err != nil {
			c.t.Fatalf("DONE: %v", err)
		}

		if strings.HasPrefix(line, tag+" OK") {
			return
		}
	}
}

// waitHub polls until the hub watches n objects.
func waitHub(t *testing.T, srv *Server, n int) {
	t.Helper()

	deadline := time.Now().Add(2 * time.Second)
	for srv.hub.Size() != n {
		if time.Now().After(deadline) {
			t.Fatalf("hub watches %d objects, want %d", srv.hub.Size(), n)
		}

		time.Sleep(5 * time.Millisecond)
	}
}

// TestIdleSessionsShareOneWatch: sessions idling on the same mailbox
// share one watch, and each of them is told about a delivery.
func TestIdleSessionsShareOneWatch(t *testing.T) {
	srv, store, dial := hubServer(t)

	sessions := make([]*client, 5)
	for i := range sessions {
		sessions[i] = dial("alice")
		sessions[i].ok("SELECT INBOX")
		sessions[i].idle("i1")
	}

	waitHub(t, srv, 1)

	u, _ := store.User("alice")
	if _, err := u.Deliver("INBOX", []byte("Subject: wake\r\n\r\nx"), nil, time.Now()); err != nil {
		t.Fatal(err)
	}

	for i, c := range sessions {
		mustHave(t, c.unsolicited(300*time.Millisecond), "* 1 EXISTS")
		c.done("i1")

		if i == len(sessions)-2 && srv.hub.Size() != 1 {
			t.Fatal("watch closed while a session still idles")
		}
	}

	waitHub(t, srv, 0)
}

// TestNotifyFollowsTheAccountLog: NOTIFY over many mailboxes holds one
// watch (the account object) and reports the mailbox a change names.
func TestNotifyFollowsTheAccountLog(t *testing.T) {
	srv, _, dial := hubServer(t)
	a, b := dial("alice"), dial("alice")

	for _, name := range []string{"Work", "Lists", "Lists/Go", "Archive/2026"} {
		a.ok("CREATE %s", name)
	}

	a.ok("SELECT Work")
	a.ok(`NOTIFY SET STATUS (SELECTED (MessageNew (UID) MessageExpunge)) (PERSONAL (MessageNew MessageExpunge))`)

	waitHub(t, srv, 1)

	b.append("Lists/Go", "x@example.org", "one", "body")
	mustHave(t, a.unsolicited(time.Second), "* STATUS Lists/Go (MESSAGES 1 UIDNEXT 2")

	b.append("Work", "x@example.org", "two", "body")
	mustHave(t, a.unsolicited(time.Second), "* 1 EXISTS", "* 1 FETCH (UID 1)")

	// IDLE beside NOTIFY adds the mailbox watch.
	a.idle("i1")
	waitHub(t, srv, 2)
	a.done("i1")

	a.ok("NOTIFY NONE")
	waitHub(t, srv, 0)
}

// TestNotifySharedSelectedThroughOwnerLog: a selected mailbox of another
// account is followed through the owner's account object.
func TestNotifySharedSelectedThroughOwnerLog(t *testing.T) {
	srv, _, dial := hubServer(t)
	alice, bob := dial("alice"), dial("bob")

	alice.ok("CREATE Team")
	alice.ok("SETACL Team bob lrs")

	bob.ok(`SELECT "Shared/alice/Team"`)
	bob.ok(`NOTIFY SET (SELECTED (MessageNew (UID) MessageExpunge))`)

	// bob's own account and alice's.
	waitHub(t, srv, 2)

	alice.append("Team", "x@example.org", "hello", "body")
	mustHave(t, bob.unsolicited(time.Second), "* 1 EXISTS", "* 1 FETCH (UID 1)")

	bob.ok("UNSELECT")
	waitHub(t, srv, 1)
}

// TestWatchLimitPerConnection: beyond its bound a connection polls
// instead of taking another watch.
func TestWatchLimitPerConnection(t *testing.T) {
	srv, store, dial := hubServer(t, func(o *Options) { o.MaxWatches = 1 })
	a := dial("alice")

	a.ok("SELECT INBOX")
	a.ok(`NOTIFY SET (PERSONAL (MessageNew MessageExpunge))`)
	waitHub(t, srv, 1)

	a.idle("i1")

	time.Sleep(50 * time.Millisecond)

	if srv.hub.Size() != 1 {
		t.Fatalf("IDLE took a watch beyond the limit: %d", srv.hub.Size())
	}

	// NOTIFY still reports through its watch.
	u, _ := store.User("alice")
	if _, err := u.Deliver("Drafts", []byte("Subject: x\r\n\r\nbody\r\n"), nil, time.Now()); err != nil {
		t.Fatal(err)
	}

	mustHave(t, a.unsolicited(300*time.Millisecond), "* STATUS Drafts (MESSAGES 1")
	a.done("i1")
}

// logUser is a user view whose account change log no longer covers the
// position asked for.
type logUser struct {
	model.User
	state uint64
}

func (u *logUser) AccountChanges(uint64, int) ([]model.AccountChange, bool, error) {
	return nil, false, nil
}
func (u *logUser) AccountState() (uint64, error) { return u.state, nil }

type brokenLogUser struct{ model.User }

func (brokenLogUser) AccountChanges(uint64, int) ([]model.AccountChange, bool, error) {
	return nil, false, errors.New("unreadable")
}

func (brokenLogUser) AccountState() (uint64, error) { return 0, errors.New("unreadable") }

// TestAccountFollowResync: when the log no longer covers the position,
// every mailbox is looked at and the position moves to the current
// state; a log entry without mailbox does the same.
func TestAccountFollowResync(t *testing.T) {
	f := &acctFollow{seq: 3}

	if ids, all := f.changed(&logUser{state: 99}); !all || ids != nil || f.seq != 99 {
		t.Fatalf("truncated log: %v %v seq %d", ids, all, f.seq)
	}

	if _, all := f.changed(brokenLogUser{}); !all || f.seq != 99 {
		t.Fatalf("unreadable log: %v seq %d", all, f.seq)
	}

	store := memstore.New()
	u, _ := store.User("alice")
	_ = u.EnsureMailbox("INBOX", nil)

	seq, _ := u.AccountState()
	f = &acctFollow{seq: seq}

	if _, err := u.Deliver("INBOX", []byte("Subject: x\r\n\r\nbody\r\n"), nil, time.Now()); err != nil {
		t.Fatal(err)
	}

	mb, _ := u.Lookup("INBOX")

	ids, all := f.changed(u)
	if all || !ids[mb.ID] || len(ids) != 1 || f.seq <= seq {
		t.Fatalf("delta %v %v seq %d", ids, all, f.seq)
	}
}
