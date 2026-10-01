package model

import (
	"errors"
	"fmt"
	"hash/fnv"
	"strconv"
	"strings"
)

// SortedIndex is a store-wide map from ids to small values, spread over
// a fixed number of shard objects so that no single object grows with
// the number of entries. It is the source for enumerations that must not
// list a pool (an object listing visits every placement group): the
// records it points to stay objects of their own.
//
// The index objects are "ix.<name>/<n>" plus a head object
// "ix.<name>/head" in a system namespace ("~accounts", "~admin",
// "~learning"); never under "kv.", which older servers read as admin
// records. The head carries the shard count, the format and the reshard
// phase. An id lives in shard IndexShard(id, shards); a listing returns
// the entries in (shard, id) order, which is not the order of the ids.
type SortedIndex interface {
	// Get returns the value of id or ErrNotFound.
	Get(id string) ([]byte, error)
	// Set stores the value of id (last writer wins).
	Set(id string, value []byte) error
	// Remove deletes id; an absent id is no error.
	Remove(id string) error
	// Update replaces the value of id with fn(current) atomically
	// (current is nil when absent; fn returning nil removes the entry,
	// returning current unchanged writes nothing). An error from fn
	// aborts without a write and is returned.
	Update(id string, fn func(cur []byte) ([]byte, error)) error
	// List returns up to limit entries after cursor ("" = from the
	// start) and the cursor of the next page ("" when the index is
	// exhausted).
	List(cursor string, limit int) ([]IndexEntry, string, error)
	// Head returns the head of the index (the defaults when no writer
	// created it yet).
	Head() (IndexHead, error)
	// SetMarker stores a named marker in the head (for example the
	// completion of a backfill).
	SetMarker(key, value string) error
}

// IndexEntry is one entry of a SortedIndex.
type IndexEntry struct {
	ID    string
	Value []byte
}

// IndexHead describes a SortedIndex.
type IndexHead struct {
	// Shards is the number of shard objects.
	Shards int
	// Format is the layout version of the index.
	Format int
	// Phase is IndexPhaseStable or "copying:<n>" while a reshard copies
	// the entries into n shards.
	Phase string
	// Markers are the named markers (SetMarker).
	Markers map[string]string
}

// Head phases and format.
const (
	IndexPhaseStable = "stable"
	IndexFormat      = 1
)

// Errors of a SortedIndex.
var (
	// ErrIndexReshard refuses a write while the index is being
	// resharded: this version does not write the new layout.
	ErrIndexReshard = errors.New("index is being resharded by a newer version")
	// ErrIndexFormat refuses an index whose format is newer than this
	// version understands.
	ErrIndexFormat = errors.New("index format not supported")
	// ErrIndexCursor rejects a malformed listing cursor.
	ErrIndexCursor = errors.New("invalid index cursor")
)

// IndexShard is the shard of id in an index of n shards: FNV-1a (32 bit)
// of the id modulo n. The function is part of the stored layout and must
// never change.
func IndexShard(id string, n int) int {
	if n <= 1 {
		return 0
	}

	h := fnv.New32a()
	_, _ = h.Write([]byte(id))

	return int(h.Sum32() % uint32(n))
}

// IndexCursor formats the listing position after id in shard.
func IndexCursor(shard int, id string) string { return strconv.Itoa(shard) + ":" + id }

// ParseIndexCursor splits a cursor into shard and id ("" = the start).
func ParseIndexCursor(cursor string) (int, string, error) {
	if cursor == "" {
		return 0, "", nil
	}

	s, id, ok := strings.Cut(cursor, ":")

	shard, err := strconv.Atoi(s)
	if !ok || err != nil || shard < 0 {
		return 0, "", fmt.Errorf("%w %q", ErrIndexCursor, cursor)
	}

	return shard, id, nil
}
