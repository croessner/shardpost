package cephstore

import (
	"fmt"
	"hash/fnv"
	"syscall"

	"shardpost/internal/model"
)

// A front-end index is a per-account map spread over a fixed number of
// omap objects in the index pool:
//
//	"ix/<space>/<nn>"    omap: the keys whose hash falls into shard nn
//
// The shard count is fixed for good: changing it would move every key.
// Thirty-two shards keep an account with a million messages at about
// thirty thousand keys per object, well below the size at which Ceph
// warns about large omap objects.
const indexShards = 32

var _ model.Index = (*User)(nil)

// indexOID is the object of one shard of an index.
func indexOID(space string, shard uint32) string { return fmt.Sprintf("ix/%s/%02d", space, shard) }

// indexShard is the shard a key lives in.
func indexShard(key string) uint32 {
	h := fnv.New32a()
	_, _ = h.Write([]byte(key))

	return h.Sum32() % indexShards
}

// byShard groups keys by the shard they live in.
func byShard(keys []string) map[uint32][]string {
	out := map[uint32][]string{}

	for _, k := range keys {
		s := indexShard(k)
		out[s] = append(out[s], k)
	}

	return out
}

// IndexGet implements model.Index.
func (u *User) IndexGet(space string, keys []string) (map[string][]byte, error) {
	if !model.ValidIndexSpace(space) {
		return nil, fmt.Errorf("index space %q", space)
	}

	out := map[string][]byte{}

	for shard, ks := range byShard(keys) {
		vals, err := getOmapKeys(u.idx, indexOID(space, shard), ks)
		if err != nil {
			return nil, fmt.Errorf("read index %s: %w", space, err)
		}

		for k, v := range vals {
			out[k] = v
		}
	}

	return out, nil
}

// IndexApply implements model.Index: one compare-and-swap per shard.
func (u *User) IndexApply(space string, keys []string, fn model.IndexFunc) error {
	if !model.ValidIndexSpace(space) {
		return fmt.Errorf("index space %q", space)
	}

	for shard, ks := range byShard(keys) {
		allowed := make(map[string]bool, len(ks))
		for _, k := range ks {
			allowed[k] = true
		}

		var outside bool

		err := casOmap(u, u.idx, indexOID(space, shard), ks, func(cur map[string][]byte) (map[string][]byte, []string) {
			set, rm := fn(ks, cur)

			outside = !within(allowed, set, rm)
			if outside {
				return nil, nil
			}

			return set, rm
		})
		if err != nil {
			return fmt.Errorf("update index %s: %w", space, err)
		}

		if outside {
			return model.ErrIndexKey
		}
	}

	return nil
}

// within reports whether every key an update touches is one it was
// given.
func within(allowed map[string]bool, set map[string][]byte, rm []string) bool {
	for k := range set {
		if !allowed[k] {
			return false
		}
	}

	for _, k := range rm {
		if !allowed[k] {
			return false
		}
	}

	return true
}

// IndexReset implements model.Index.
func (u *User) IndexReset(space string) error {
	if !model.ValidIndexSpace(space) {
		return fmt.Errorf("index space %q", space)
	}

	for shard := uint32(0); shard < indexShards; shard++ {
		if err := u.idx.Delete(indexOID(space, shard)); err != nil && errCode(err) != syscall.ENOENT {
			return fmt.Errorf("reset index %s: %w", space, err)
		}
	}

	return nil
}
