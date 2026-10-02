package cephstore

import (
	"bytes"
	"compress/flate"
	"io"

	"shardpost/internal/fts"
)

// The header store keeps fts.HeaderText of every indexed message — the
// header names and decoded values that IMAP SEARCH checks a header key
// against — so that a search over many messages checks headers with a
// few paged omap reads instead of reading every message. It belongs to
// the full-text index: in layout 2 "ftsh/<nn>", server.fts_shards
// objects, in layout 3 the H objects "ftx/<gen>/h<k>" of the generation;
// shard = fnv1a(GUID), key the GUID, value the text compressed with
// DEFLATE after a format byte. Keyed by GUID like the posting lists, so
// COPY and MOVE do not touch it; it is written before the postings
// (delivery, APPEND, import, JMAP, fts_rebuild: a conversion that
// freezes the postings afterwards finds it in its source) and removed
// with them (ftsRemove: expunge, purge, mailbox delete, the scrubber's
// orphans; DeleteFTS).
// A header text longer than fts.MaxHeaderTextBytes is not kept; such a
// message, and one indexed before the store existed, has its header
// read from the message. Like the index, it does not count against the
// quota.
const (
	headerPrefix = "ftsh/"
	// headerDeflate is the format byte of a DEFLATE-compressed text.
	headerDeflate = 1
)

// encodeHeaderText compresses a header text; false when it is too long
// to be kept.
func encodeHeaderText(text string) ([]byte, bool) {
	if len(text) > fts.MaxHeaderTextBytes {
		return nil, false
	}

	var buf bytes.Buffer

	buf.WriteByte(headerDeflate)

	w, _ := flate.NewWriter(&buf, flate.DefaultCompression)
	_, _ = w.Write([]byte(text))
	_ = w.Close()

	return buf.Bytes(), true
}

// decodeHeaderText reverses encodeHeaderText; false for a value it
// does not understand.
func decodeHeaderText(v []byte) (string, bool) {
	if len(v) == 0 || v[0] != headerDeflate {
		return "", false
	}

	r := flate.NewReader(bytes.NewReader(v[1:]))
	defer func() { _ = r.Close() }()

	b, err := io.ReadAll(io.LimitReader(r, fts.MaxHeaderTextBytes+1))
	if err != nil || len(b) > fts.MaxHeaderTextBytes {
		return "", false
	}

	return string(b), true
}
