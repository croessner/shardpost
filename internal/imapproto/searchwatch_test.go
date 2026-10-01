package imapproto

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// TestSearchStopsWhenClientGoes: a search that reads many messages stops
// reading when its client closes the connection.
func TestSearchStopsWhenClientGoes(t *testing.T) {
	c, reads, _ := newCountingServer(t, 1)

	var msgs []string

	for i := 0; i < 300; i++ {
		msgs = append(msgs, fmt.Sprintf("Subject: m%d\r\n\r\nthe needle %d\r\n", i, i))
	}

	runSearches(t, c, "sg", msgs, nil)

	readDelay.Store(int64(5 * time.Millisecond))
	t.Cleanup(func() { readDelay.Store(0) })

	if _, err := fmt.Fprintf(c.conn, "s1 SEARCH BODY eedl\r\n"); err != nil {
		t.Fatal(err)
	}

	time.Sleep(100 * time.Millisecond)

	_ = c.conn.Close()

	time.Sleep(200 * time.Millisecond)

	n := reads.Load()

	time.Sleep(300 * time.Millisecond)

	if after := reads.Load(); after != n || n >= int64(len(msgs)) {
		t.Errorf("reads went on after the client left: %d, then %d of %d", n, after, len(msgs))
	}
}

// TestSearchKeepsPipelinedCommand: the watch for a client going away
// does not take a command the client sent after the SEARCH.
func TestSearchKeepsPipelinedCommand(t *testing.T) {
	c, _, _ := newCountingServer(t, 2)
	runSearches(t, c, "pc", []string{"Subject: a\r\n\r\nneedle\r\n", "Subject: b\r\n\r\nhay\r\n"}, nil)

	if _, err := fmt.Fprintf(c.conn, "p1 SEARCH BODY eedl\r\np2 NOOP\r\n"); err != nil {
		t.Fatal(err)
	}

	var lines []string

	for !has(lines, "p2 OK") {
		_ = c.conn.SetReadDeadline(time.Now().Add(5 * time.Second))

		l, err := c.br.ReadString('\n')
		if err != nil {
			t.Fatalf("after %v: %v", lines, err)
		}

		lines = append(lines, strings.TrimRight(l, "\r\n"))
	}

	if !has(lines, "* SEARCH 1") || !has(lines, "p1 OK") {
		t.Errorf("lines %v", lines)
	}
}
