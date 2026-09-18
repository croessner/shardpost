package pop3proto

import (
	"bufio"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	"shardpost/internal/auth"
	"shardpost/internal/memstore"
	"shardpost/internal/model"
)

type client struct {
	t  *testing.T
	nc net.Conn
	br *bufio.Reader
}

func (c *client) cmd(s string) string {
	c.t.Helper()

	if _, err := fmt.Fprintf(c.nc, "%s\r\n", s); err != nil {
		c.t.Fatal(err)
	}

	line, err := c.br.ReadString('\n')
	if err != nil {
		c.t.Fatal(err)
	}

	return strings.TrimRight(line, "\r\n")
}

func (c *client) multi() []string {
	c.t.Helper()

	var out []string

	for {
		line, err := c.br.ReadString('\n')
		if err != nil {
			c.t.Fatal(err)
		}

		line = strings.TrimRight(line, "\r\n")
		if line == "." {
			return out
		}

		out = append(out, strings.TrimPrefix(line, "."))
	}
}

func dial(t *testing.T, addr string) *client {
	nc, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}

	c := &client{t: t, nc: nc, br: bufio.NewReader(nc)}
	if g, _ := c.br.ReadString('\n'); !strings.HasPrefix(g, "+OK") {
		t.Fatal(g)
	}

	return c
}

// startSession starts a server over a memstore with two messages for
// user "test" and returns the listen address and the user view.
func startSession(t *testing.T) (string, model.User) {
	t.Helper()

	store := memstore.New()
	srv := New(Options{Store: store, Auth: auth.Static{"test": "test"}, InsecureAuth: true, MarkSeen: true})

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	go func() { _ = srv.Serve(ln) }()

	t.Cleanup(func() { _ = srv.Close() })

	u, err := store.User("test")
	if err != nil {
		t.Fatal(err)
	}

	if err := u.EnsureMailbox("INBOX", nil); err != nil {
		t.Fatal(err)
	}

	for _, raw := range []string{"Subject: one\r\n\r\nline1\r\n.dot\r\nline3\r\n", "Subject: two\r\n\r\nbody\r\n"} {
		if _, err := u.Deliver("INBOX", []byte(raw), nil, time.Time{}); err != nil {
			t.Fatal(err)
		}
	}

	return ln.Addr().String(), u
}

func TestSession(t *testing.T) {
	addr, u := startSession(t)

	c := dial(t, addr)
	if r := c.cmd("CAPA"); !strings.HasPrefix(r, "+OK") {
		t.Fatal(r)
	}

	c.multi()

	if r := c.cmd("PASS x"); !strings.HasPrefix(r, "-ERR") {
		t.Fatal(r)
	}

	c.cmd("USER test")

	if r := c.cmd("PASS test"); !strings.Contains(r, "2 messages") {
		t.Fatal(r)
	}
	// second session must be refused while the maildrop is locked
	c2 := dial(t, addr)
	c2.cmd("USER test")

	if r := c2.cmd("PASS test"); !strings.Contains(r, "[IN-USE]") {
		t.Fatal(r)
	}

	checkTransaction(t, c)

	// lock released, message 2 expunged, message 1 marked seen
	c3 := dial(t, addr)
	c3.cmd("USER test")

	if r := c3.cmd("PASS test"); !strings.Contains(r, "1 messages") {
		t.Fatal(r)
	}

	mb, _ := u.Lookup("INBOX")

	msgs, _ := u.ListMessages(mb.ID)
	if len(msgs) != 1 || len(msgs[0].Flags) != 1 || msgs[0].Flags[0] != `\seen` {
		t.Fatalf("%+v", msgs)
	}

	c3.cmd("QUIT")
}

// checkTransaction runs STAT, UIDL, TOP, RETR, DELE, RSET and QUIT on a
// logged-in session.
func checkTransaction(t *testing.T, c *client) {
	t.Helper()

	if r := c.cmd("STAT"); r != "+OK 2 58" {
		t.Fatal(r)
	}

	c.cmd("UIDL")

	if l := c.multi(); len(l) != 2 || !strings.HasPrefix(l[0], "1 ") {
		t.Fatal(l)
	}

	if r := c.cmd("TOP 1 1"); !strings.HasPrefix(r, "+OK") {
		t.Fatal(r)
	}

	if l := c.multi(); strings.Join(l, "|") != "Subject: one||line1" {
		t.Fatal(l)
	}

	c.cmd("RETR 1")

	if l := c.multi(); strings.Join(l, "|") != "Subject: one||line1|.dot|line3" {
		t.Fatal(l)
	}

	c.cmd("DELE 2")

	if r := c.cmd("STAT"); r != "+OK 1 36" {
		t.Fatal(r)
	}

	c.cmd("RSET")
	c.cmd("DELE 2")

	if r := c.cmd("QUIT"); !strings.HasPrefix(r, "+OK") {
		t.Fatal(r)
	}
}
