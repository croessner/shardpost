package imapproto

import (
	"bytes"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"shardpost/internal/memstore"
)

// TestSearchStopsWhenClientGoes: a search that reads many messages stops
// reading when its client closes the connection — also when that
// happens after the idle timeout the command loop armed has passed —
// and the session ends without a storage error.
func TestSearchStopsWhenClientGoes(t *testing.T) {
	var logs syncBuffer

	reads := &atomic.Int64{}

	c, _ := newTestServer(t, func(o *Options, store *memstore.Store) {
		o.Store = countingStore{inner: store, reads: reads, gone: &sync.Map{}}
		o.VerifyParallel = 1
		o.IdleTimeout = 300 * time.Millisecond
		o.Log = slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	})
	c.ok("LOGIN test test")

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

	// past the idle timeout of the command loop
	time.Sleep(600 * time.Millisecond)

	_ = c.conn.Close()

	time.Sleep(200 * time.Millisecond)

	n := reads.Load()

	time.Sleep(300 * time.Millisecond)

	if after := reads.Load(); after != n || n >= int64(len(msgs)) {
		t.Errorf("reads went on after the client left: %d, then %d of %d", n, after, len(msgs))
	}

	if out := logs.String(); strings.Contains(out, "storage error") || strings.Contains(out, "level=ERROR") {
		t.Errorf("the lost client was logged as an error:\n%s", out)
	}
}

// syncBuffer is a buffer for a logger that several goroutines write to.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.buf.String()
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
