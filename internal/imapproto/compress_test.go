package imapproto

import (
	"bufio"
	"compress/flate"
	"encoding/hex"
	"io"
	"strings"
	"testing"
	"time"
)

// compressed switches the test client to COMPRESS=DEFLATE and returns a
// reader of the decompressed server output.
func compressed(t *testing.T, c *client) *bufio.Reader {
	t.Helper()

	c.cmd("LOGIN test test")

	if lines := c.cmd("COMPRESS DEFLATE"); !strings.Contains(lines[len(lines)-1], "OK") {
		t.Fatalf("compress: %v", lines)
	}

	return bufio.NewReader(flate.NewReader(c.br))
}

// expect reads decompressed lines until one starts with prefix, failing
// after a timeout (a stuck decompressor blocks).
func expect(t *testing.T, c *client, r *bufio.Reader, prefix string) {
	t.Helper()

	_ = c.conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	defer func() { _ = c.conn.SetReadDeadline(time.Time{}) }()

	for {
		line, err := r.ReadString('\n')
		if err != nil {
			t.Fatalf("waiting for %q: %v", prefix, err)
		}

		if strings.HasPrefix(line, prefix) {
			return
		}
	}
}

func TestCompressSyncFlush(t *testing.T) {
	c, _ := newTestServer(t)
	defer func() { _ = c.conn.Close() }()

	r := compressed(t, c)

	w, err := flate.NewWriter(c.conn, flate.DefaultCompression)
	if err != nil {
		t.Fatal(err)
	}

	for _, cmd := range []string{"z1 NOOP", "z2 SELECT INBOX"} {
		_, _ = io.WriteString(w, cmd+"\r\n")

		if err := w.Flush(); err != nil {
			t.Fatal(err)
		}

		expect(t, c, r, strings.Fields(cmd)[0]+" OK")
	}
}

// TestCompressPartialFlush sends commands the way iOS Mail does: raw
// deflate ended by Z_PARTIAL_FLUSH (bytes from Python zlib). Every command
// must be answered without further input.
func TestCompressPartialFlush(t *testing.T) {
	c, _ := newTestServer(t)
	defer func() { _ = c.conn.Close() }()

	r := compressed(t, c)

	for _, step := range []struct{ hex, tag string }{
		{"2a3152f0f3f70fe0e50208", "t2"},               // t2 NOOP
		{"a012630567c70047274f1fcf90485e2e8000", "t3"}, // t3 CAPABILITY
	} {
		data, _ := hex.DecodeString(step.hex)
		if _, err := c.conn.Write(data); err != nil {
			t.Fatal(err)
		}

		expect(t, c, r, step.tag+" OK")
	}
}
