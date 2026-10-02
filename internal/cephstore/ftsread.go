package cephstore

import (
	"encoding/hex"
	"fmt"
	"strings"
	"sync"
	"syscall"

	"github.com/ceph/go-ceph/rados"
	"go.opentelemetry.io/otel/attribute"

	"shardpost/internal/fts"
	"shardpost/internal/metrics"
	"shardpost/internal/postings"
)

// The read path of the full-text index in both layouts (stage B14b).
//
// A view reads the layout record once, with the trust markers, and
// caches it (ftsState). Every read of an index object also reads its
// markers in the same operation. A layout 3 object that is missing or
// carries another generation, and a frozen object of a generation the
// record no longer names, make the search read the record again: in
// another generation it starts over (at most twice), in the same one it
// fails (the scrub reports the object, fts_rebuild repairs it). A
// frozen object of the current generation is read on: its state is
// complete. In layout 2 a missing posting object is empty only while
// the record still says layout 2; readers never write.

// ftsReadState is what a view's searches know of the index.
type ftsReadState struct {
	lay     *ftsLayout
	loc     ftsLoc
	current bool
}

// ftsState returns the cached layout of the view, reading it (with the
// trust markers, one read) when there is none.
func (u *User) ftsState() (*ftsReadState, error) {
	u.ftsMu.Lock()
	defer u.ftsMu.Unlock()

	if u.ftsRS != nil {
		return u.ftsRS, nil
	}

	vals, err := getOmapKeys(u.idx, oidFTSMap, []string{ftsVersionKey, ftsRebuildKey, ftsVersionKey3, ftsLayoutKey})
	if err != nil && errCode(err) != syscall.ENOENT {
		return nil, err
	}

	lay, err := parseLayout(vals[ftsLayoutKey])
	if err != nil {
		return nil, err
	}

	loc := curLoc(lay, u.ftsShards)
	u.ftsRS = &ftsReadState{lay: lay, loc: loc, current: indexCurrentIn(vals, loc.v3())}

	return u.ftsRS, nil
}

// ftsForget drops the cached layout of the view.
func (u *User) ftsForget() {
	u.ftsMu.Lock()
	u.ftsRS = nil
	u.ftsMu.Unlock()
}

// ftsRead runs a search over the view's layout and follows a change of
// generation (§4.5).
func (u *User) ftsRead(fn func(ix *radosIndex) error) error {
	for attempt := 0; ; attempt++ {
		rs, err := u.ftsState()
		if err != nil {
			return err
		}

		err = fn(&radosIndex{u: u, rs: rs})

		an := asAnomaly(err)
		if an == nil {
			return err
		}

		u.ftsForget()

		now, err := u.ftsState()
		if err != nil {
			return err
		}

		if now.loc.same(rs.loc) && an.kind != anomMoved {
			metrics.FTSShardMissing.Inc()
			return fmt.Errorf("%w: %s", errFTSShardMissing, an.oid)
		}

		if attempt >= maxGenSwitches {
			return errFTSRetries
		}

		metrics.FTSLayoutRetries.WithLabelValues("generation").Inc()
	}
}

// radosIndex is the candidateIndex of a User over one generation.
type radosIndex struct {
	u  *User
	rs *ftsReadState

	// once: a search reads the record again at most once to confirm
	// that a frozen or (layout 2) missing posting object belongs to the
	// generation it reads.
	once    sync.Once
	movedTo error
}

var _ candidateIndex = (*radosIndex)(nil)

// confirm reads the record once per search: an anomaly when it names
// another generation than the one the search reads.
func (r *radosIndex) confirm() error {
	r.once.Do(func() {
		l, err := r.u.readLayout()
		if err != nil {
			r.movedTo = err
			return
		}

		if !curLoc(l, r.u.ftsShards).same(r.rs.loc) {
			r.movedTo = &ftsAnomaly{kind: anomMoved}
		}
	})

	return r.movedTo
}

// readMarkers are the markers a read of an object of kind kind checks.
func (r *radosIndex) readMarkers(posting bool) []string {
	switch {
	case r.rs.loc.v3() && posting:
		return []string{markGen, markState}
	case r.rs.loc.v3():
		return []string{markGen}
	case posting:
		return []string{markState}
	default:
		return nil
	}
}

// check applies the reader rule to an object read: nil to read on (an
// absent layout 2 object as empty).
func (r *radosIndex) check(oid string, posting, exists bool, marks map[string][]byte) error {
	if !r.rs.loc.v3() {
		switch {
		case !exists && posting && r.rs.lay != nil:
			// a conversion recorded: the object may be gone with it
			return r.confirm()
		case !exists:
			// no record: empty, as before 0.45.0 (a conversion locks the
			// account, which ends the sessions that cached this)
			return nil
		}
	}

	an := checkMarkers(r.rs.loc, oid, exists, marks)
	if an == nil {
		return nil
	}

	if an.kind == anomFrozen {
		return r.confirm()
	}

	return an
}

// isPosting reports whether oid is a posting object of the generation.
func (r *radosIndex) isPosting(oid string) bool {
	if r.rs.loc.v3() {
		return strings.HasPrefix(oid, ftxPrefix+r.rs.loc.g+"/p")
	}

	return strings.HasPrefix(oid, ftsPrefix)
}

// scan implements candidateIndex: one read op with a step per range and
// one for the markers.
func (r *radosIndex) scan(oid string, reqs []*scanReq) error {
	op := rados.CreateReadOp()
	defer op.Release()

	steps := make([]*rados.GetOmapStep, len(reqs))
	for i, q := range reqs {
		steps[i] = op.GetOmapValues(q.after, q.prefix, q.limit)
	}

	posting := r.isPosting(oid)
	mk := r.readMarkers(posting)

	var mstep *rados.ReadOpOmapGetValsByKeysStep
	if len(mk) > 0 {
		mstep = op.GetOmapValuesByKeys(mk)
	}

	if err := op.Operate(r.u.idx, oid, rados.OperationNoFlag); err != nil {
		if errCode(err) == syscall.ENOENT {
			return r.check(oid, posting, false, nil)
		}

		return fmt.Errorf("fts object %s: %w", oid, err)
	}

	marks, err := stepValues(mstep)
	if err != nil {
		return err
	}

	if err := r.check(oid, posting, true, marks); err != nil {
		return err
	}

	for i, q := range reqs {
		for {
			kv, err := steps[i].Next()
			if err != nil {
				return err
			}

			if kv == nil {
				break
			}

			if !isMarker(kv.Key) {
				q.fn(kv.Key)
			}
		}

		q.more = steps[i].More()
	}

	return nil
}

// stepValues collects the values of a by-keys step (nil step: none).
func stepValues(step *rados.ReadOpOmapGetValsByKeysStep) (map[string][]byte, error) {
	out := map[string][]byte{}
	if step == nil {
		return out, nil
	}

	for {
		kv, err := step.Next()
		if err != nil {
			return nil, err
		}

		if kv == nil {
			return out, nil
		}

		out[kv.Key] = append([]byte(nil), kv.Value...)
	}
}

// readKeys reads keys of an object of the generation with its markers.
func (r *radosIndex) readKeys(oid string, keys []string, posting bool) (map[string][]byte, error) {
	mk := r.readMarkers(posting)

	op := rados.CreateReadOp()
	defer op.Release()

	step := op.GetOmapValuesByKeys(append(append([]string(nil), keys...), mk...))

	if err := op.Operate(r.u.idx, oid, rados.OperationNoFlag); err != nil {
		if errCode(err) == syscall.ENOENT {
			return map[string][]byte{}, r.check(oid, posting, false, nil)
		}

		return nil, fmt.Errorf("fts object %s: %w", oid, err)
	}

	vals, err := stepValues(step)
	if err != nil {
		return nil, err
	}

	marks := map[string][]byte{}

	for _, k := range mk {
		if v, ok := vals[k]; ok {
			marks[k] = v
			delete(vals, k)
		}
	}

	return vals, r.check(oid, posting, true, marks)
}

func (r *radosIndex) postingOIDs() []string     { return r.rs.loc.postingOIDs() }
func (r *radosIndex) dictOID(tri string) string { return r.rs.loc.dictOID(tri) }
func (r *radosIndex) current() (bool, error)    { return r.rs.current, nil }

func (r *radosIndex) legacyDictPresent() (bool, error) {
	if r.rs.loc.v3() {
		return false, nil
	}

	return r.u.hasLegacyDict()
}

// postingsUnion returns the union of the posting lists of the terms, as
// GUIDs (ids are resolved through the map object; legacy lists carry
// GUIDs directly).
func (r *radosIndex) postingsUnion(terms []string) (map[string]bool, error) {
	out := map[string]bool{}
	if len(terms) == 0 {
		return out, nil
	}

	ids := map[uint64]bool{}

	var mu sync.Mutex

	err := parallel(groupBy(terms, r.rs.loc.postingOID), func(oid string, ts []string) error {
		for len(ts) > 0 {
			page := ts[:min(len(ts), postingsPerCAS)]
			ts = ts[len(page):]

			vals, err := r.readKeys(oid, page, true)
			if err != nil {
				return err
			}

			mu.Lock()

			for _, v := range vals {
				if len(v) > 0 && postings.IsLegacy(v) {
					for _, g := range postings.LegacyGUIDs(v) {
						out[hex.EncodeToString(g)] = true
					}

					continue
				}

				list, _ := postings.Decode(v)
				for _, id := range list {
					ids[id] = true
				}
			}

			mu.Unlock()
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	resolved, err := r.u.resolveDocIDs(ids)
	if err != nil {
		return nil, err
	}

	for g := range resolved {
		out[g] = true
	}

	return out, nil
}

// resolveDocIDs maps ids back to GUIDs; an id that was freed (a dead id
// a removal could not take out of every list) has none.
func (u *User) resolveDocIDs(ids map[uint64]bool) (map[string]bool, error) {
	out := map[string]bool{}
	if len(ids) == 0 {
		return out, nil
	}

	keys := make([]string, 0, len(ids))
	for id := range ids {
		keys = append(keys, idKey(id))
	}

	for i := 0; i < len(keys); i += omapPageSize {
		vals, err := getOmapKeys(u.idx, oidFTSMap, keys[i:min(i+omapPageSize, len(keys))])
		if err != nil {
			return nil, fmt.Errorf("fts docids: %w", err)
		}

		for _, v := range vals {
			out[string(v)] = true
		}
	}

	return out, nil
}

// --- word search (JMAP, IMAP word mode) -------------------------------------------

// SearchField answers an IMAP text criterion for one field: every token of
// the query must occur in the field as substring of a term (RFC 3501
// substring semantics within a token), its stem, or exact match. Result:
// the set of matching GUIDs.
func (u *User) SearchField(field, query string) (map[string]bool, error) {
	return u.SearchFields([]string{field}, query)
}

// SearchFields is SearchField over several fields (OR between fields, AND
// between query tokens). Used for TEXT = headers or body.
func (u *User) SearchFields(fields []string, query string) (hits map[string]bool, err error) {
	end := u.trace(spanFTSSearch, attribute.StringSlice("store.fields", fields))
	defer func() { end(err) }()

	toks := fts.Tokenize(query)
	if len(toks) == 0 {
		return nil, nil // nothing to constrain on
	}

	err = u.ftsRead(func(ix *radosIndex) error {
		hits, err = ix.searchFields(fields, toks)
		return err
	})

	return hits, err
}

func (r *radosIndex) searchFields(fields, toks []string) (map[string]bool, error) {
	var result map[string]bool

	for _, tok := range toks {
		var terms []string

		for _, f := range fields {
			c, err := r.candidateTerms(f, tok)
			if err != nil {
				return nil, err
			}

			terms = append(terms, c...)
		}

		hits, err := r.postingsUnion(terms)
		if err != nil {
			return nil, err
		}

		if result == nil {
			result = hits
		} else {
			for g := range result {
				if !hits[g] {
					delete(result, g)
				}
			}
		}

		if len(result) == 0 {
			break
		}
	}

	return result, nil
}

// candidateTerms returns the index terms of field that a query token
// matches: exact, stemmed, and substring via the trigram dictionary.
func (r *radosIndex) candidateTerms(field, tok string) ([]string, error) {
	set := map[string]struct{}{}
	for _, t := range fts.Expand(tok) {
		set[field+":"+t] = struct{}{}
	}

	tris := fts.Trigrams(tok)
	sub := []string{field}

	if len(tris) == 0 || !fts.Substring(field) {
		// Too short for the dictionary, or a field that is not in it:
		// prefix scan on the sorted omap.
		terms, err := r.prefixTermsN(field+":"+tok, prefixScanMax)
		if err != nil {
			return nil, err
		}

		for _, t := range terms {
			set[t] = struct{}{}
		}

		sub = nil
		if len(tris) > 0 {
			sub = fts.HeaderSubstringFields
		}
	}

	for _, f := range sub {
		if err := r.substringTerms(set, f, tok, tris); err != nil {
			return nil, err
		}
	}

	return keys(set), nil
}

// substringTerms adds the terms of field that contain tok to set.
func (r *radosIndex) substringTerms(set map[string]struct{}, field, tok string, tris []string) error {
	terms, _, err := dictMatches(r, field, tok, tris)
	if err != nil {
		return err
	}

	for _, t := range terms {
		set[t] = struct{}{}
	}

	return r.legacyDictMatches(set, field, tok, tris)
}

// prefixTermsN lists terms starting with prefix across all posting
// objects, at most perShard per object (the fallbacks of short tokens
// and of fields outside the dictionary; the capped-trigram fallback of
// the old dictionary asks for a page, the field's vocabulary).
func (r *radosIndex) prefixTermsN(prefix string, perShard uint64) ([]string, error) {
	var (
		mu  sync.Mutex
		out []string
	)

	shards := map[string][]string{}
	for _, oid := range r.postingOIDs() {
		shards[oid] = nil
	}

	err := parallel(shards, func(oid string, _ []string) error {
		var found []string

		if _, err := list(r, oid, "", prefix, perShard, func(t string) { found = append(found, t) }); err != nil {
			return err
		}

		mu.Lock()

		out = append(out, found...)

		mu.Unlock()

		return nil
	})

	return out, err
}

// --- substring dictionary ---------------------------------------------------------

// newDictScan is the scan of the range of trigram tri for field in the
// dictionary object oid.
func newDictScan(oid, tri, field string) *dictScan {
	return &dictScan{oid: oid, prefix: tri + dictSep + field + ":", skip: len(tri) + len(dictSep)}
}

// dictMatches returns the terms of field that contain tok, at most about
// maxCandidates of them, and whether they are all of them.
//
// Every such term is listed under each trigram of tok, so the key range
// of any one trigram, read to its end and filtered, is the whole answer.
// Which range is the short one is not known beforehand: all of them are
// read a page at a time until the first is exhausted, which costs at
// most the shortest range times the number of trigrams. That is one
// round for a token with any trigram that is not among the most common
// ones. The ranges of one dictionary object go in one read, so a round
// costs one read per object the trigrams fall into (one with D = 1).
func dictMatches(ix candidateIndex, field, tok string, tris []string) ([]string, bool, error) {
	scans := make([]*dictScan, len(tris))
	for i, tri := range tris {
		scans[i] = newDictScan(ix.dictOID(tri), tri, field)
	}

	for {
		byOID := map[string][]*dictScan{}
		for _, s := range scans {
			byOID[s.oid] = append(byOID[s.oid], s)
		}

		groups := make(map[string][]string, len(byOID))
		for oid := range byOID {
			groups[oid] = nil
		}

		err := parallel(groups, func(oid string, _ []string) error {
			group := byOID[oid]
			reqs := make([]*scanReq, len(group))

			for i, s := range group {
				reqs[i] = &scanReq{after: s.after, prefix: s.prefix, limit: omapPageSize, fn: func(key string) { s.take(key, tok) }}
			}

			if err := ix.scan(oid, reqs); err != nil {
				if asAnomaly(err) != nil {
					return err
				}

				return fmt.Errorf("fts dictionary shard %s: %w", oid, err)
			}

			for i, s := range group {
				s.done = !reqs[i].more
			}

			return nil
		})
		if err != nil {
			return nil, false, err
		}

		if s := finishedScan(scans); s != nil {
			return s.found, s.done, nil
		}
	}
}

// listOmapKeys calls fn for up to limit keys of the object that start
// with prefix and sort after the key after, and reports whether there
// are more. A missing object has none.
func listOmapKeys(ioctx *rados.IOContext, oid, after, prefix string, limit uint64, fn func(key string)) (bool, error) {
	op := rados.CreateReadOp()
	defer op.Release()

	step := op.GetOmapValues(after, prefix, limit)
	if err := op.Operate(ioctx, oid, rados.OperationNoFlag); err != nil {
		if errCode(err) == syscall.ENOENT {
			return false, nil
		}

		return false, err
	}

	for {
		kv, err := step.Next()
		if err != nil {
			return false, err
		}

		if kv == nil {
			return step.More(), nil
		}

		fn(kv.Key)
	}
}

// hasLegacyDict reports whether the account still has objects of the
// old dictionary layout. It is looked up once per view: a converted
// account must not pay sixteen reads of objects that are gone for every
// token of every query. A layout 3 account has none (no stats).
func (u *User) hasLegacyDict() (bool, error) {
	if u.legacyDict == legacyDictUnknown {
		n, err := u.LegacyFTSDict()
		if err != nil {
			return false, err
		}

		u.legacyDict = legacyDictAbsent
		if n > 0 {
			u.legacyDict = legacyDictPresent
		}
	}

	return u.legacyDict == legacyDictPresent, nil
}

// legacyDictMatches adds the terms of field that contain tok and are
// listed in the old dictionary layout to set. An account without the
// old objects has none.
func (r *radosIndex) legacyDictMatches(set map[string]struct{}, field, tok string, tris []string) error {
	legacy, err := r.legacyDictPresent()
	if err != nil || !legacy {
		return err
	}

	inter, capped, err := r.u.legacyDictIntersect(tris)
	if err != nil {
		return err
	}

	if inter == nil && capped > 0 {
		// Every trigram of the token is too common to be selective: scan
		// the field's terms instead (bounded by the user's vocabulary).
		terms, err := r.prefixTermsN(field+":", omapPageSize)
		if err != nil {
			return err
		}

		inter = map[string]struct{}{}
		for _, t := range terms {
			inter[t] = struct{}{}
		}
	}

	addSubstringTerms(set, inter, field, tok)

	return nil
}

// legacyDictIntersect intersects the term lists of the trigrams in the
// old dictionary layout. Capped trigrams are skipped and counted; inter
// stays nil if every trigram was capped.
func (u *User) legacyDictIntersect(tris []string) (map[string]struct{}, int, error) {
	var (
		mu    sync.Mutex
		inter map[string]struct{}
	)

	capped := 0

	err := parallel(groupBy(tris, u.legacyDictShardOID), func(oid string, tris []string) error {
		vals, err := getOmapKeys(u.idx, oid, tris)
		if err != nil {
			return fmt.Errorf("fts dictionary shard %s: %w", oid, err)
		}

		mu.Lock()
		defer mu.Unlock()

		for _, tri := range tris {
			if string(vals[tri]) == legacyDictCapped {
				capped++
				continue
			}

			list := splitTerms(vals[tri])
			if inter == nil {
				inter = list
				continue
			}

			for t := range inter {
				if _, ok := list[t]; !ok {
					delete(inter, t)
				}
			}
		}

		return nil
	})

	return inter, capped, err
}

// --- header texts -----------------------------------------------------------------

// HeaderTexts implements model.User: the stored header texts of the
// GUIDs that have one, read a page of keys per object at a time.
func (u *User) HeaderTexts(guids []string) (map[string]string, error) {
	var out map[string]string

	err := u.ftsRead(func(r *radosIndex) error {
		var err error

		out, err = r.headerTexts(guids)

		return err
	})

	return out, err
}

func (r *radosIndex) headerTexts(guids []string) (map[string]string, error) {
	groups := groupBy(guids, r.rs.loc.headerOID)

	var (
		mu  sync.Mutex
		out = make(map[string]string, len(guids))
	)

	err := parallel(groups, func(oid string, keys []string) error {
		for i := 0; i < len(keys); i += omapPageSize {
			vals, err := r.readKeys(oid, keys[i:min(i+omapPageSize, len(keys))], false)
			if err != nil {
				return err
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

// --- deleting -----------------------------------------------------------------------

// DeleteFTS removes every generation the layout record names and the
// objects of layout 2 (both dictionary layouts, the header store), the
// id map with the markers last.
func (u *User) DeleteFTS() error {
	lay, err := u.readLayout()
	if err != nil {
		return err
	}

	oids := layout2Loc(u.ftsShards).objects()

	for _, loc := range nameGens(lay, u.ftsShards) {
		if loc.v3() {
			oids = append(oids, loc.objects()...)
		}
	}

	if err := u.deleteObjects(oids); err != nil {
		return err
	}

	defer u.ftsForget()

	if err := u.idx.Delete(oidFTSMap); err != nil && errCode(err) != syscall.ENOENT {
		return err
	}

	return nil
}
