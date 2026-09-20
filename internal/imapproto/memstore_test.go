package imapproto

import (
	"bufio"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	"shardpost/internal/auth"
	"shardpost/internal/memstore"
)

// --- test harness ----------------------------------------------------------

type client struct {
	t    *testing.T
	conn net.Conn
	br   *bufio.Reader
	n    int
	addr string
}

func newTestServer(t *testing.T, customize ...func(*Options, *memstore.Store)) (*client, *memstore.Store) {
	t.Helper()

	store := memstore.New()

	opts := Options{Store: store, Auth: auth.Static{"test": "test"}, InsecureAuth: true, Autocreate: DefaultSpecialFolders, QuotaSelfService: true}
	for _, fn := range customize {
		fn(&opts, store)
	}

	srv := New(opts)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	go func() { _ = srv.Serve(ln) }()

	t.Cleanup(func() { _ = srv.Close() })

	nc, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}

	c := &client{t: t, conn: nc, br: bufio.NewReader(nc)}

	greeting, _ := c.br.ReadString('\n')
	if !strings.HasPrefix(greeting, "* OK [CAPABILITY") {
		t.Fatalf("greeting: %q", greeting)
	}

	c.addr = ln.Addr().String()

	return c, store
}

// cmd sends a command and returns all response lines up to the tagged one.
func (c *client) cmd(format string, args ...any) []string {
	c.t.Helper()
	c.n++
	tag := fmt.Sprintf("t%d", c.n)

	if _, err := fmt.Fprintf(c.conn, tag+" "+format+"\r\n", args...); err != nil {
		c.t.Fatalf("write: %v", err)
	}

	var lines []string

	for {
		line, err := c.br.ReadString('\n')
		if err != nil {
			c.t.Fatalf("read: %v (after %v)", err, lines)
		}

		line = strings.TrimRight(line, "\r\n")
		// literals: read their bytes and append inline, repeatedly
		for {
			i := strings.LastIndex(line, "{")
			if i < 0 || !strings.HasSuffix(line, "}") {
				break
			}

			var n int
			if _, err := fmt.Sscanf(line[i:], "{%d}", &n); err != nil {
				break
			}

			buf := make([]byte, n)
			if _, err := readFull(c.br, buf); err != nil {
				c.t.Fatal(err)
			}

			rest, _ := c.br.ReadString('\n')
			line = line[:i] + string(buf) + strings.TrimRight(rest, "\r\n")
		}

		lines = append(lines, line)
		if strings.HasPrefix(line, tag+" ") {
			return lines
		}
	}
}

func readFull(r *bufio.Reader, buf []byte) (int, error) {
	n := 0
	for n < len(buf) {
		m, err := r.Read(buf[n:])

		n += m
		if err != nil {
			return n, err
		}
	}

	return n, nil
}

func (c *client) ok(format string, args ...any) []string {
	c.t.Helper()
	lines := c.cmd(format, args...)

	last := lines[len(lines)-1]
	if !strings.Contains(last, " OK") {
		c.t.Fatalf("%s -> %v", fmt.Sprintf(format, args...), lines)
	}

	return lines
}

func has(lines []string, sub string) bool {
	for _, l := range lines {
		if strings.Contains(l, sub) {
			return true
		}
	}

	return false
}

func find(lines []string, sub string) string {
	for _, l := range lines {
		if strings.Contains(l, sub) {
			return l
		}
	}

	return ""
}

func (c *client) append(mailbox, from, subject, body string, extra ...string) {
	c.t.Helper()

	id := strings.ReplaceAll(strings.ReplaceAll(subject, " ", "-"), ":", "")
	msg := fmt.Sprintf("From: %s\r\nTo: test@example.org\r\nSubject: %s\r\nDate: Mon, 14 Sep 2026 10:00:00 +0000\r\nMessage-ID: <%s@example.org>\r\n%sContent-Type: text/plain\r\n\r\n%s\r\n",
		from, subject, id, strings.Join(extra, ""), body)
	c.ok("APPEND %s {%d+}\r\n%s", mailbox, len(msg), msg)
}

// --- tests -----------------------------------------------------------------

func TestLoginAndList(t *testing.T) {
	c, _ := newTestServer(t)

	lines := c.cmd("LOGIN test wrong")
	mustHave(t, lines, "AUTHENTICATIONFAILED")

	lines = c.ok("LOGIN test test")
	if !has(lines, "CONDSTORE") || !has(lines, "QRESYNC") || !has(lines, "OBJECTID") {
		t.Fatalf("capabilities: %v", lines)
	}

	lines = c.ok(`LIST "" "*" RETURN (SPECIAL-USE)`)
	mustHave(t, lines, `\Trash) "/" Trash`, `"/" INBOX`)

	c.ok("CREATE Projects/2026/Q3")

	lines = c.ok(`LIST "" "Projects*"`)
	mustHave(t, lines, `(\Noselect \HasChildren) "/" Projects`)

	c.ok("RENAME Projects Work")
	c.ok("SUBSCRIBE Work/2026/Q3")

	lines = c.ok(`LIST (SUBSCRIBED) "" "*"`)
	if !has(lines, `Work/2026/Q3`) || has(lines, `Drafts`) == false {
		t.Fatal(lines)
	}

	c.ok(`STATUS INBOX (MESSAGES UIDNEXT HIGHESTMODSEQ MAILBOXID)`)

	lines = c.ok(`NAMESPACE`)
	mustHave(t, lines, `NAMESPACE (("" "/")) (("Other Users/" "/")) (("Public/" "/"))`)

	lines = c.ok(`ID ("name" "test")`)
	mustHave(t, lines, `* ID (`)
}

func TestFetchSearchStore(t *testing.T) {
	c, _ := newTestServer(t)
	c.ok("LOGIN test test")
	c.append("INBOX", "alice@example.org", "Invoice 4711", "Please find the invoices attached.")
	c.append("INBOX", "bob@example.org", "Lunch", "Pizza or sushi?")

	lines := c.ok("SELECT INBOX (CONDSTORE)")
	mustHave(t, lines, "* 2 EXISTS", "HIGHESTMODSEQ", "MAILBOXID")

	checkFetchItems(t, c)

	lines = c.ok("FETCH 1 FLAGS")
	if !has(lines, `FLAGS (\Seen`) {
		t.Fatal("BODY[TEXT] must set \\Seen:", lines)
	}

	lines = c.ok("SEARCH RETURN (SAVE COUNT) SUBJECT invoic")
	mustHave(t, lines, `ESEARCH (TAG "t`, "COUNT 1")

	lines = c.ok("UID FETCH $ (UID)")
	mustHave(t, lines, "UID 1")

	lines = c.ok("SEARCH SUBJECT x") // too short to index: brute force on the Subject line only
	if !has(lines, "* SEARCH\r") && find(lines, "* SEARCH") != "* SEARCH" {
		t.Fatal(lines)
	}

	lines = c.ok("SEARCH OR FROM bob NOT SUBJECT invoice")
	mustHave(t, lines, "* SEARCH 2")

	lines = c.ok("SEARCH MODSEQ 1")
	mustHave(t, lines, "(MODSEQ ")

	lines = c.ok(`STORE 2 +FLAGS ($Forwarded)`)
	mustHave(t, lines, `FLAGS ($Forwarded) MODSEQ (`)
	// UNCHANGEDSINCE: message 2 was just changed, so an old modseq must fail
	lines = c.ok(`UID STORE 2 (UNCHANGEDSINCE 1) +FLAGS (\Flagged)`)
	mustHave(t, lines, "MODIFIED 2")

	lines = c.ok(`SORT (REVERSE SUBJECT) UTF-8 ALL`)
	mustHave(t, lines, "* SORT 2 1")

	lines = c.ok(`THREAD ORDEREDSUBJECT UTF-8 ALL`)
	mustHave(t, lines, "* THREAD (1)(2)")

	lines = c.ok("GETQUOTAROOT INBOX")
	mustHave(t, lines, `* QUOTAROOT INBOX ""`)

	c.ok(`SETQUOTA "" (STORAGE 1024)`)

	lines = c.ok(`GETQUOTA ""`)
	mustHave(t, lines, "STORAGE 1 1024")
}

// checkFetchItems checks the metadata and body items of FETCH on message 1.
func checkFetchItems(t *testing.T, c *client) {
	t.Helper()

	lines := c.ok("FETCH 1 (UID FLAGS ENVELOPE RFC822.SIZE INTERNALDATE MODSEQ EMAILID THREADID PREVIEW SAVEDATE)")

	l := find(lines, "* 1 FETCH")
	for _, want := range []string{"UID 1", `"Invoice 4711"`, `"alice" "example.org"`, "MODSEQ (", "EMAILID (", "THREADID (T", `PREVIEW "Please find`} {
		if !strings.Contains(l, want) {
			t.Fatalf("missing %q in %s", want, l)
		}
	}

	lines = c.ok("FETCH 1 (BODY.PEEK[HEADER.FIELDS (SUBJECT)] BODY[TEXT]<0.5> BODYSTRUCTURE)")

	l = find(lines, "* 1 FETCH")
	if !strings.Contains(l, "BODY[HEADER.FIELDS (SUBJECT)] Subject: Invoice 4711") || !strings.Contains(l, "BODY[TEXT]<0> Pleas") || !strings.Contains(l, `("text" "plain"`) {
		t.Fatal(l)
	}
}

func TestQresyncAndMove(t *testing.T) {
	c, _ := newTestServer(t)
	c.ok("LOGIN test test")
	c.ok("ENABLE QRESYNC")
	c.append("INBOX", "a@example.org", "one", "first")
	c.append("INBOX", "b@example.org", "two", "second")
	c.append("INBOX", "c@example.org", "three", "third")
	lines := c.ok("SELECT INBOX")
	validity := find(lines, "UIDVALIDITY")

	var v uint32
	if _, err := fmt.Sscanf(validity, "* OK [UIDVALIDITY %d]", &v); err != nil {
		t.Fatal(err)
	}

	var ms uint64
	if _, err := fmt.Sscanf(find(lines, "HIGHESTMODSEQ"), "* OK [HIGHESTMODSEQ %d]", &ms); err != nil {
		t.Fatal(err)
	}

	lines = c.ok("UID MOVE 2 Trash")
	mustHave(t, lines, "COPYUID", "* VANISHED 2")

	c.ok(`STORE 1 +FLAGS (\Flagged)`)
	c.ok("CLOSE")

	lines = c.ok("SELECT INBOX (QRESYNC (%d %d))", v, ms)
	mustHave(t, lines, "* VANISHED (EARLIER) 2", `FLAGS (\Flagged) MODSEQ`)

	lines = c.ok("UID FETCH 1:* (FLAGS) (CHANGEDSINCE %d VANISHED)", ms)
	if !has(lines, "* VANISHED (EARLIER) 2") || !has(lines, "UID 1 FLAGS") || has(lines, "UID 3 FLAGS") {
		t.Fatal(lines)
	}

	lines = c.ok(`THREAD REFERENCES UTF-8 ALL`)
	mustHave(t, lines, "* THREAD")
}

func TestThreadReferences(t *testing.T) {
	c, _ := newTestServer(t)
	c.ok("LOGIN test test")
	c.append("INBOX", "a@example.org", "root", "start")
	c.append("INBOX", "b@example.org", "Re: root", "reply", "References: <root@example.org>\r\n")
	c.append("INBOX", "c@example.org", "other", "unrelated")
	c.append("INBOX", "d@example.org", "Re: root", "reply2", "In-Reply-To: <Re-root@example.org>\r\nReferences: <root@example.org> <Re-root@example.org>\r\n")
	c.ok("SELECT INBOX")

	lines := c.ok("THREAD REFERENCES UTF-8 ALL")
	mustHave(t, lines, "* THREAD (1 2 4)(3)")

	lines = c.ok("FETCH 1:4 THREADID")

	t1, t2 := find(lines, "* 1 FETCH"), find(lines, "* 2 FETCH")
	if t1[strings.Index(t1, "THREADID"):] != t2[strings.Index(t2, "THREADID"):] {
		t.Fatalf("thread ids differ: %s / %s", t1, t2)
	}
}

func TestIdleAndExamine(t *testing.T) {
	c, store := newTestServer(t)
	c.ok("LOGIN test test")
	c.append("INBOX", "a@example.org", "one", "first")
	c.ok("EXAMINE INBOX")

	lines := c.cmd(`STORE 1 +FLAGS (\Seen)`)
	mustHave(t, lines, "NOPERM")

	c.ok("SELECT INBOX")

	if _, err := fmt.Fprintf(c.conn, "i1 IDLE\r\n"); err != nil {
		t.Fatal(err)
	}

	line, _ := c.br.ReadString('\n')
	if !strings.HasPrefix(line, "+ ") {
		t.Fatal(line)
	}

	u, _ := store.User("test")
	if _, err := u.Deliver("INBOX", []byte("Subject: wake\r\n\r\nx"), nil, time.Time{}); err != nil {
		t.Fatal(err)
	}

	if err := c.conn.SetReadDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatal(err)
	}

	line, err := c.br.ReadString('\n')
	if err != nil || !strings.Contains(line, "* 2 EXISTS") {
		t.Fatalf("expected EXISTS, got %q %v", line, err)
	}

	if line, _ = c.br.ReadString('\n'); !strings.Contains(line, "RECENT") {
		t.Fatalf("expected RECENT, got %q", line)
	}

	if _, err := fmt.Fprintf(c.conn, "DONE\r\n"); err != nil {
		t.Fatal(err)
	}

	line, _ = c.br.ReadString('\n')
	if !strings.HasPrefix(line, "i1 OK") {
		t.Fatal(line)
	}
}

func TestLiteralsAndSyntax(t *testing.T) {
	c, _ := newTestServer(t)
	c.ok("LOGIN {4}\r\ntest {4}\r\ntest")

	m1, m2 := "Subject: one\r\n\r\nx", "Subject: two\r\n\r\ny"

	lines := c.ok("APPEND Drafts (\\Draft) {%d+}\r\n%s {%d+}\r\n%s", len(m1), m1, len(m2), m2)
	if !has(lines, "APPENDUID") || !strings.Contains(lines[len(lines)-1], " 1:2]") {
		t.Fatal(lines)
	}

	c.ok("SELECT Drafts")

	lines = c.ok("FETCH 2 (BINARY.SIZE[1] BINARY.PEEK[1])")
	mustHave(t, lines, "BINARY.SIZE[1] 1", "BINARY[1] ~y")

	c.ok("UNSELECT")

	lines = c.cmd("FETCH 1 FLAGS")
	mustHave(t, lines, "BAD")

	c.ok("SELECT INBOX")

	lines = c.cmd("FETCH 1 (NOPE)")
	mustHave(t, lines, "BAD")

	lines = c.cmd("SEARCH FROM")
	mustHave(t, lines, "BAD")

	if got := formatSet([]uint32{5, 1, 2, 3, 9, 10}); got != "1:3,5,9:10" {
		t.Fatal(got)
	}
}

// newTestServerClient opens a second connection to the server of c.
func newTestServerClient(t *testing.T, c *client) (*client, *memstore.Store) {
	t.Helper()

	nc, err := net.Dial("tcp", c.addr)
	if err != nil {
		t.Fatal(err)
	}

	d := &client{t: t, conn: nc, br: bufio.NewReader(nc), addr: c.addr}
	if g, _ := d.br.ReadString('\n'); !strings.HasPrefix(g, "* OK [CAPABILITY") {
		t.Fatalf("greeting: %q", g)
	}

	return d, nil
}

// mustHave fails the test unless lines contain every one of subs.
func mustHave(t *testing.T, lines []string, subs ...string) {
	t.Helper()

	for _, sub := range subs {
		if !has(lines, sub) {
			t.Fatal(lines)
		}
	}
}

// mustLack fails the test if lines contain any of subs.
func mustLack(t *testing.T, lines []string, subs ...string) {
	t.Helper()

	for _, sub := range subs {
		if has(lines, sub) {
			t.Fatal(lines)
		}
	}
}
