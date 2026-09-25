package model

import (
	"errors"
	"strings"
)

// Index is a per-account map that a front-end keeps for itself: data it
// derives from the store and can always rebuild from it. It carries no
// consistency obligation towards the rest of the store — a lost write
// costs a rebuild, never a message — and the store shards it, so that a
// map with a key per message does not become one object with a million
// keys.
//
// The interface is optional, like Blobs: a front-end that needs it asks
// for it.
type Index interface {
	// IndexGet returns the values of the given keys of space; keys that
	// are absent are missing from the result.
	IndexGet(space string, keys []string) (map[string][]byte, error)
	// IndexApply changes keys of space. It runs fn once for each shard the
	// keys fall into, handing it the keys of that shard and the current
	// values of those that exist; fn returns what to set and what to
	// remove. Each call is atomic over its keys, may be run again when
	// another writer got in between, and may only set or remove keys it
	// was handed.
	IndexApply(space string, keys []string, fn IndexFunc) error
	// IndexReset removes all of space.
	IndexReset(space string) error
}

// IndexFunc computes one shard's update of an IndexApply: keys are the
// keys of that shard the caller named, cur the current values of those
// of them that exist.
type IndexFunc func(keys []string, cur map[string][]byte) (set map[string][]byte, rm []string)

// ErrIndexKey is returned when an IndexApply function touches a key it
// was not given: that key may live in another shard, and a write to it
// would not be covered by the compare-and-swap.
var ErrIndexKey = errors.New("index update outside its keys")

// ValidIndexSpace reports whether space can name an index: a short word
// that cannot reach outside its own objects.
func ValidIndexSpace(space string) bool {
	return space != "" && len(space) <= 32 && !strings.ContainsAny(space, "/\x00")
}
