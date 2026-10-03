package cephstore

import (
	"context"
	"fmt"
	"sync"
	"syscall"

	"github.com/ceph/go-ceph/rados"

	"shardpost/internal/fts/census"
	"shardpost/internal/model"
)

// Statistics of an account's index (temp/13 §7.1). The default reads
// the counters of the posting objects and the document count: S + 1
// reads, no lock. Exact counts the keys of every object page by page
// (librados has no key count), which on a large account is thousands of
// reads alongside the deliveries: the counts are only roughly
// consistent. Classes reads the terms of the posting objects; the admin
// API allows it only for test accounts. Nothing here prints a term.

// Bounds of the exact count's parallelism.
const (
	defaultStatsParallel = 2
	maxStatsParallel     = 16
)

var _ model.FTSStatser = (*User)(nil)

// FTSStats implements model.FTSStatser.
func (u *User) FTSStats(req model.FTSStatsRequest) (model.FTSStats, error) {
	ctx := req.Context
	if ctx == nil {
		ctx = context.Background()
	}

	l, err := u.readLayout()
	if err != nil {
		return model.FTSStats{}, err
	}

	cur := curLoc(l, u.ftsShards)
	st := model.FTSStats{Shape: cur.shape(), Exact: req.Exact, Objects: []model.FTSObjectStats{}}

	if cur.v3() {
		vals, err := getOmapKeys(u.idx, oidFTSMap, []string{ftsCountKey})
		if err != nil && errCode(err) != syscall.ENOENT {
			return st, err
		}

		st.Docs = atoiMarker(vals[ftsCountKey])
	}

	switch {
	case req.Exact:
		err = u.statsExact(ctx, &st, cur, req.Parallel)
	case cur.v3():
		err = u.statsCounters(&st, cur)
	}

	if err != nil {
		return st, err
	}

	if req.Classes {
		if st.Classes, err = u.statsClasses(ctx, cur); err != nil {
			return st, err
		}
	}

	return st, nil
}

// statsCounters reads "!n" and "!dn" of every posting object.
func (u *User) statsCounters(st *model.FTSStats, cur ftsLoc) error {
	for _, oid := range cur.postingOIDs() {
		vals, err := u.genMarkers(cur, oid)
		if err != nil {
			return err
		}

		n, dn := atoiMarker(vals[markTerms]), atoiMarker(vals[markDict])
		st.Terms += n
		st.DictKeys += dn
		st.Objects = append(st.Objects, model.FTSObjectStats{Object: oid, Kind: model.FTSObjectPostings, Keys: n, DictKeys: dn})
	}

	return nil
}

// statsKinds are the objects of a generation with their kinds, in order
// (layout 2 with the dictionary before 0.35.0).
func statsKinds(cur ftsLoc) []model.FTSObjectStats {
	var out []model.FTSObjectStats

	add := func(kind string, oids []string) {
		for _, oid := range oids {
			out = append(out, model.FTSObjectStats{Object: oid, Kind: kind})
		}
	}

	add(model.FTSObjectPostings, cur.postingOIDs())
	add(model.FTSObjectDictionary, cur.dictOIDs())
	add(model.FTSObjectHeaders, cur.headerOIDs())

	if !cur.v3() {
		add(model.FTSObjectLegacy, oidsOf(cur.s, func(i int) string { return fmt.Sprintf("%s%02d", legacyDictPrefix, i) }))
	}

	return out
}

// statsExact counts the keys of every object, parallel at a time; for
// layout 2 it counts the documents of the id map as well.
func (u *User) statsExact(ctx context.Context, st *model.FTSStats, cur ftsLoc, parallel int) error {
	objs := statsKinds(cur)
	groups := make(map[string][]string, len(objs))

	for _, o := range objs {
		groups[o.Object] = nil
	}

	var (
		mu     sync.Mutex
		counts = make(map[string]int64, len(objs))
	)

	n := parallel
	if n <= 0 {
		n = defaultStatsParallel
	}

	err := parallelN(groups, min(n, maxStatsParallel), func(oid string, _ []string) error {
		var keys int64

		err := u.eachKey(ctx, oid, func(string) { keys++ })

		mu.Lock()
		counts[oid] = keys
		mu.Unlock()

		return err
	})
	if err != nil {
		return err
	}

	for _, o := range objs {
		o.Keys = counts[o.Object]

		switch o.Kind {
		case model.FTSObjectPostings:
			st.Terms += o.Keys
		case model.FTSObjectDictionary:
			st.DictKeys += o.Keys
		case model.FTSObjectHeaders:
			st.HeaderKeys += o.Keys
		}

		st.Objects = append(st.Objects, o)
	}

	if !cur.v3() {
		docs, err := u.liveDocCount()
		if err != nil {
			return err
		}

		st.Docs = int64(docs)
	}

	return nil
}

// statsClasses classifies the terms of the posting objects.
func (u *User) statsClasses(ctx context.Context, cur ftsLoc) (*model.FTSTermClasses, error) {
	c := census.New()

	for _, oid := range cur.postingOIDs() {
		if err := u.eachKey(ctx, oid, c.Add); err != nil {
			return nil, err
		}
	}

	return c.Result(), nil
}

// eachKey calls fn for every key of an object but the markers, a page
// at a time (an absent object has none); it stops when ctx ends.
func (u *User) eachKey(ctx context.Context, oid string, fn func(key string)) error {
	after := ""

	for {
		if err := ctx.Err(); err != nil {
			return err
		}

		op := rados.CreateReadOp()
		step := op.GetOmapValues(after, "", omapPageSize)

		if err := op.Operate(u.idx, oid, rados.OperationNoFlag); err != nil {
			op.Release()

			if errCode(err) == syscall.ENOENT {
				return nil
			}

			return fmt.Errorf("read %s: %w", oid, err)
		}

		var n int

		for {
			kv, err := step.Next()
			if err != nil {
				op.Release()
				return err
			}

			if kv == nil {
				break
			}

			n++
			after = kv.Key

			if !isMarker(kv.Key) {
				fn(after)
			}
		}

		more := step.More()

		op.Release()

		if !more || n == 0 {
			return nil
		}
	}
}
