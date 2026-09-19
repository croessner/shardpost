package inflate

import (
	"bytes"
	"compress/flate"
	"encoding/hex"
	"io"
	"math/rand/v2"
	"testing"
	"time"
)

// readWithin reads once from r and fails when that blocks longer than a
// second.
func readWithin(t *testing.T, r io.Reader) string {
	t.Helper()

	got := make(chan string, 1)

	go func() {
		buf := make([]byte, 1024)
		n, _ := r.Read(buf)

		got <- string(buf[:n])
	}()

	select {
	case s := <-got:
		return s
	case <-time.After(time.Second):
		t.Fatal("read blocked")
		return ""
	}
}

// TestPartialFlushIsHandedOut uses zlib output with Z_PARTIAL_FLUSH
// (Python zlib, raw deflate): each command must be readable without more
// input, which compress/flate does not do.
func TestPartialFlushIsHandedOut(t *testing.T) {
	pr, pw := io.Pipe()
	defer func() { _ = pw.Close() }()

	r := NewReader(pr)

	for _, tc := range []struct{ hex, want string }{
		{"2a3152f0f3f70fe0e50208", "t2 NOOP\r\n"},
		{"a012630567c70047274f1fcf90485e2e8000", "t3 CAPABILITY\r\n"},
	} {
		data, _ := hex.DecodeString(tc.hex)

		go func() { _, _ = pw.Write(data) }()

		if got := readWithin(t, r); got != tc.want {
			t.Fatalf("got %q, want %q", got, tc.want)
		}
	}
}

// TestRoundTrip checks the decompressor against compress/flate output of
// all levels, with sync flushes between writes (the IMAP pattern) and
// data larger than the window.
func TestRoundTrip(t *testing.T) {
	rng := rand.New(rand.NewPCG(1, 2))

	var plain bytes.Buffer
	for plain.Len() < 300<<10 {
		if rng.IntN(2) == 0 {
			plain.WriteString("* 17 FETCH (UID 4711 FLAGS (\\Seen) RFC822.SIZE 12345)\r\n")
		} else {
			b := make([]byte, rng.IntN(4096))
			for i := range b {
				b[i] = byte(rng.IntN(256))
			}

			plain.Write(b)
		}
	}

	for level := flate.HuffmanOnly; level <= flate.BestCompression; level++ {
		var z bytes.Buffer

		w, err := flate.NewWriter(&z, level)
		if err != nil {
			t.Fatal(err)
		}

		for p := plain.Bytes(); len(p) > 0; {
			n := min(len(p), 1+rng.IntN(20000))
			_, _ = w.Write(p[:n])
			_ = w.Flush()
			p = p[n:]
		}

		_ = w.Close()

		got, err := io.ReadAll(NewReader(&z))
		if err != nil || !bytes.Equal(got, plain.Bytes()) {
			t.Fatalf("level %d: err %v, equal %v", level, err, bytes.Equal(got, plain.Bytes()))
		}
	}
}

func TestCorruptInput(t *testing.T) {
	if _, err := io.ReadAll(NewReader(bytes.NewReader([]byte{0xff, 0xff, 0xff}))); err == nil {
		t.Fatal("corrupt input accepted")
	}
}
