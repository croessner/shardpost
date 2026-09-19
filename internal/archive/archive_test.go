package archive

import (
	"archive/tar"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"testing"
	"time"
)

func write(t *testing.T, failure error) []byte {
	t.Helper()

	var buf bytes.Buffer

	w, err := NewWriter(&buf, Header{Account: "alice", Created: time.Unix(1_800_000_000, 0)})
	if err != nil {
		t.Fatal(err)
	}

	steps := []error{
		w.Mailbox(Mailbox{ID: "0123abcd", Name: "INBOX", UIDValidity: 7, UIDNext: 3, HighestModSeq: 9}),
		w.Message("0123abcd", Message{UID: 1, GUID: "g1", Flags: []string{`\seen`}, ModSeq: 4}, []byte("Subject: a\r\n\r\na\r\n")),
		w.Message("0123abcd", Message{UID: 2, GUID: "g2", ModSeq: 9}, []byte("Subject: b\r\n\r\nb\r\n")),
		w.Sieve("main", `keep;`),
		w.Close("main", failure),
	}

	for _, err := range steps {
		if err != nil {
			t.Fatal(err)
		}
	}

	return buf.Bytes()
}

func readAll(b []byte) ([]*Entry, error) {
	r, err := NewReader(bytes.NewReader(b))
	if err != nil {
		return nil, err
	}

	var out []*Entry

	for {
		e, err := r.Next()
		if errors.Is(err, io.EOF) {
			return out, nil
		}

		if err != nil {
			return out, err
		}

		out = append(out, e)
	}
}

func TestRoundTrip(t *testing.T) {
	entries, err := readAll(write(t, nil))
	if err != nil {
		t.Fatal(err)
	}

	kinds := []Kind{KindMailbox, KindMessage, KindMessage, KindSieve, KindManifest}
	if len(entries) != len(kinds) {
		t.Fatalf("%d entries", len(entries))
	}

	for i, k := range kinds {
		if entries[i].Kind != k {
			t.Errorf("entry %d: kind %d, want %d", i, entries[i].Kind, k)
		}
	}

	if m := entries[1].Message; m.UID != 1 || m.Flags[0] != `\seen` || string(entries[1].Raw) != "Subject: a\r\n\r\na\r\n" {
		t.Errorf("message: %+v", m)
	}

	if man := entries[4].Manifest; man.Messages != 2 || man.ActiveSieve != "main" || len(man.Files) != 7 {
		t.Errorf("manifest: %+v", man)
	}
}

func TestTamperAndIncomplete(t *testing.T) {
	good := write(t, nil)

	tampered := bytes.Replace(good, []byte("Subject: b\r\n\r\nb"), []byte("Subject: B\r\n\r\nb"), 1)
	if _, err := readAll(tampered); !errors.Is(err, ErrChecksum) {
		t.Errorf("tampered message: %v", err)
	}

	if _, err := readAll(write(t, errors.New("disk full"))); !errors.Is(err, ErrIncomplete) {
		t.Errorf("incomplete export: %v", err)
	}

	if _, err := readAll(good[:len(good)/2]); err == nil {
		t.Error("truncated archive accepted")
	}

	if _, err := NewReader(bytes.NewReader([]byte("not a tar"))); !errors.Is(err, ErrFormat) {
		t.Errorf("garbage: %v", err)
	}
}

// crafted builds a tar with the given entries after a valid header.
func crafted(t *testing.T, names ...string) []byte {
	t.Helper()

	var buf bytes.Buffer

	tw := tar.NewWriter(&buf)

	for _, name := range append([]string{headerName}, names...) {
		body := []byte("x")
		if name == headerName {
			body, _ = json.Marshal(Header{Format: Format, Version: Version, Account: "a"})
		}

		_ = tw.WriteHeader(&tar.Header{Name: name, Mode: 0o600, Size: int64(len(body)), Typeflag: tar.TypeReg})
		_, _ = tw.Write(body)
	}

	_ = tw.Close()

	return buf.Bytes()
}

func TestCraftedArchives(t *testing.T) {
	for _, names := range [][]string{
		{"sieve/a.sieve", "sieve/a.sieve"},
		{"sieve/...sieve"},
		{"sieve/a\x01b.sieve"},
		{"mailboxes/../mailbox.json"},
		{"etc/passwd"},
	} {
		if _, err := readAll(crafted(t, names...)); !errors.Is(err, ErrFormat) {
			t.Errorf("%q: %v", names, err)
		}
	}
}
