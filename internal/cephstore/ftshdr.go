package cephstore

import (
	"bytes"
	"compress/flate"
	"fmt"
	"io"
	"sync"
	"syscall"

	"github.com/ceph/go-ceph/rados"

	"shardpost/internal/fts"
)

// The header store keeps fts.HeaderText of every indexed message — the
// header names and decoded values that IMAP SEARCH checks a header key
// against — so that a search over many messages checks headers with a
// few paged omap reads instead of reading every message. It belongs to
// the full-text index: "ftsh/<nn>", server.fts_shards objects, shard =
// fnv1a(GUID), key the GUID, value the text compressed with DEFLATE
// after a format byte. Keyed by GUID like the posting lists, so COPY
// and MOVE do not touch it; it is written with the postings (delivery,
// APPEND, import, JMAP, fts_rebuild) and removed with them (ftsRemove:
// expunge, purge, mailbox delete, the scrubber's orphans; DeleteFTS).
// A header text longer than fts.MaxHeaderTextBytes is not kept; such a
// message, and one indexed before the store existed, has its header
// read from the message. Like the index, it does not count against the
// quota.
const (
	headerPrefix = "ftsh/"
	// headerDeflate is the format byte of a DEFLATE-compressed text.
	headerDeflate = 1
)

func (u *User) headerShardOID(guid string) string {
	return fmt.Sprintf("%s%02d", headerPrefix, shardNo(guid, u.ftsShards))
}

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

// putHeaderTexts stores the header texts of the messages, by GUID.
func (u *User) putHeaderTexts(texts map[string]string) error {
	byShard := map[string]map[string][]byte{}

	for guid, text := range texts {
		v, ok := encodeHeaderText(text)
		if !ok {
			continue
		}

		oid := u.headerShardOID(guid)
		if byShard[oid] == nil {
			byShard[oid] = map[string][]byte{}
		}

		byShard[oid][guid] = v
	}

	groups := map[string][]string{}
	for oid := range byShard {
		groups[oid] = nil
	}

	return parallel(groups, func(oid string, _ []string) error {
		op := rados.CreateWriteOp()
		defer op.Release()

		op.SetOmap(byShard[oid])

		if err := op.Operate(u.idx, oid, rados.OperationNoFlag); err != nil {
			return fmt.Errorf("fts header shard %s: %w", oid, err)
		}

		return nil
	})
}

// dropHeaderText removes the header text of a message.
func (u *User) dropHeaderText(guid string) error {
	op := rados.CreateWriteOp()
	defer op.Release()

	op.RmOmapKeys([]string{guid})

	err := op.Operate(u.idx, u.headerShardOID(guid), rados.OperationNoFlag)
	if errCode(err) == syscall.ENOENT {
		return nil
	}

	return err
}

// HeaderTexts implements model.User: the stored header texts of the
// GUIDs that have one, read a page of keys per shard at a time.
func (u *User) HeaderTexts(guids []string) (map[string]string, error) {
	groups := groupBy(guids, u.headerShardOID)

	var (
		mu  sync.Mutex
		out = make(map[string]string, len(guids))
	)

	err := parallel(groups, func(oid string, keys []string) error {
		for i := 0; i < len(keys); i += omapPageSize {
			vals, err := getOmapKeys(u.idx, oid, keys[i:min(i+omapPageSize, len(keys))])
			if err != nil {
				if errCode(err) == syscall.ENOENT {
					return nil
				}

				return fmt.Errorf("fts header shard %s: %w", oid, err)
			}

			mu.Lock()

			for g, v := range vals {
				if text, ok := decodeHeaderText(v); ok {
					out[g] = text
				}
			}

			mu.Unlock()
		}

		return nil
	})

	return out, err
}

// headerOIDs are the objects of the header store.
func (u *User) headerOIDs() []string {
	oids := make([]string, 0, u.ftsShards)
	for i := 0; i < u.ftsShards; i++ {
		oids = append(oids, fmt.Sprintf("%s%02d", headerPrefix, i))
	}

	return oids
}
