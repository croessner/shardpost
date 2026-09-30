// Package postings encodes FTS posting lists as sorted document ids with
// varint deltas. A list of n ids costs about n bytes for dense ids
// instead of 16 bytes per raw GUID.
//
// Format (v2): 0x01, uvarint(count), count uvarint deltas (first delta is
// the first id itself), then one 0x00 pad byte if the length would be a
// multiple of 16. Legacy lists (concatenated 16-byte GUIDs) are exactly a
// multiple of 16 bytes long; the pad rule keeps the two formats apart
// without a migration of existing indexes: a legacy list is converted the
// first time it is written.
package postings

import (
	"encoding/binary"
	"sort"
)

const (
	version   = 0x01
	guidBytes = 16
)

// IsLegacy reports whether p is a legacy GUID list (or empty).
func IsLegacy(p []byte) bool { return len(p)%guidBytes == 0 }

// LegacyGUIDs splits a legacy list into its 16-byte GUIDs.
func LegacyGUIDs(p []byte) [][]byte {
	out := make([][]byte, 0, len(p)/guidBytes)
	for i := 0; i+guidBytes <= len(p); i += guidBytes {
		out = append(out, p[i:i+guidBytes])
	}

	return out
}

// Encode encodes ids (any order, duplicates removed).
func Encode(ids []uint64) []byte {
	if len(ids) == 0 {
		return nil
	}

	s := append([]uint64(nil), ids...)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })

	n := 0
	for i, v := range s {
		if i == 0 || v != s[n-1] {
			s[n] = v
			n++
		}
	}

	s = s[:n]
	buf := make([]byte, 0, 2+2*n)
	buf = append(buf, version)
	buf = binary.AppendUvarint(buf, uint64(n))

	prev := uint64(0)
	for _, v := range s {
		buf = binary.AppendUvarint(buf, v-prev)
		prev = v
	}

	if len(buf)%guidBytes == 0 {
		buf = append(buf, 0)
	}

	return buf
}

// Decode returns the ids of a v2 list; ok is false for a legacy list or
// a corrupt one.
func Decode(p []byte) (ids []uint64, ok bool) {
	if len(p) == 0 || IsLegacy(p) || p[0] != version {
		return nil, false
	}

	rest := p[1:]

	n, k := binary.Uvarint(rest)
	if k <= 0 {
		return nil, false
	}

	rest = rest[k:]
	ids = make([]uint64, 0, n)

	var v uint64

	for i := uint64(0); i < n; i++ {
		d, k := binary.Uvarint(rest)
		if k <= 0 {
			return nil, false
		}

		rest = rest[k:]
		v += d
		ids = append(ids, v)
	}

	return ids, true
}

// Add returns the list with id included.
func Add(p []byte, id uint64) []byte {
	ids, _ := Decode(p)
	for _, v := range ids {
		if v == id {
			return p
		}
	}

	return Encode(append(ids, id))
}

// AddAll returns the list with every id of add included. It decodes the
// list once, so adding many ids to a long list costs one pass, not one
// per id.
func AddAll(p []byte, add []uint64) []byte {
	if len(add) == 1 {
		return Add(p, add[0])
	}

	ids, _ := Decode(p)
	have := make(map[uint64]struct{}, len(ids)+len(add))

	for _, v := range ids {
		have[v] = struct{}{}
	}

	n := len(ids)

	for _, id := range add {
		if _, ok := have[id]; !ok {
			have[id] = struct{}{}
			ids = append(ids, id)
		}
	}

	if len(ids) == n {
		return p
	}

	return Encode(ids)
}

// Remove returns the list without id (nil when it becomes empty).
func Remove(p []byte, id uint64) []byte {
	ids, ok := Decode(p)
	if !ok {
		return p
	}

	out := ids[:0]
	for _, v := range ids {
		if v != id {
			out = append(out, v)
		}
	}

	return Encode(out)
}
