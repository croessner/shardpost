package cephstore

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"syscall"

	"go.opentelemetry.io/otel/attribute"

	"shardpost/internal/fts"
)

// The candidate search of IMAP SEARCH (substring semantics) has to
// return every message that contains the value. Its reads go through
// candidateIndex, so that the tests can run it over an index in memory
// with the same key layout.

// The index version marker. The key ftsVersionKey of "ftsmap" holds
// ftsVersion when every message of the account was indexed by a version
// that prepares text as this one does (fts.Terms with window terms and
// markers, header names, charset conversion, HTML text, file names). A
// fresh index gets it with its first document id; RebuildFTS sets it
// when it has indexed every message. ftsRebuildKey is set while a
// rebuild runs (right after the old index is deleted) and removed with
// the marker set in one write, so that a rebuild that stops midway — or
// a delivery into the empty index it leaves — never marks an index that
// misses messages. An index without the marker, or with ftsRebuildKey,
// narrows nothing: the old preparation (markup, undecoded words, no file
// names) cannot be trusted even for whole words.
const (
	ftsVersionKey = "v"
	ftsVersion    = "2"
	ftsRebuildKey = "r"
)

// omapLister lists the keys of an object that start with prefix and
// sort after after, at most limit, and reports whether there are more.
type omapLister func(oid, after, prefix string, limit uint64, fn func(key string)) (bool, error)

// candidateIndex is what the candidate search reads.
type candidateIndex interface {
	list(oid, after, prefix string, limit uint64, fn func(key string)) (bool, error)
	// postingsUnion returns the messages of the posting lists of terms.
	postingsUnion(terms []string) (map[string]bool, error)
	// legacyDictPresent reports whether the account still has the
	// dictionary layout before 0.35.0.
	legacyDictPresent() (bool, error)
	// current reports whether the index can be trusted (indexCurrent).
	current() (bool, error)
	shards() int
}

// radosIndex is the candidateIndex of a User.
type radosIndex struct{ u *User }

func (r radosIndex) list(oid, after, prefix string, limit uint64, fn func(string)) (bool, error) {
	return listOmapKeys(r.u.idx, oid, after, prefix, limit, fn)
}

func (r radosIndex) postingsUnion(terms []string) (map[string]bool, error) {
	return r.u.postingsUnion(terms)
}

func (r radosIndex) legacyDictPresent() (bool, error) { return r.u.hasLegacyDict() }
func (r radosIndex) current() (bool, error)           { return r.u.ftsIndexCurrent() }
func (r radosIndex) shards() int                      { return r.u.ftsShards }

func (u *User) lister() omapLister { return radosIndex{u}.list }

// Whether the index can be trusted, cached per view.
const (
	ftsCurrentUnknown = iota
	ftsCurrentYes
	ftsCurrentNo
)

// indexCurrent reports, from the keys ftsVersionKey and ftsRebuildKey of
// the id map, whether the index can be trusted: the marker is there and
// no rebuild is running.
func indexCurrent(vals map[string][]byte) bool {
	_, rebuilding := vals[ftsRebuildKey]

	return string(vals[ftsVersionKey]) == ftsVersion && !rebuilding
}

// ftsIndexCurrent reports whether the index can be trusted.
func (u *User) ftsIndexCurrent() (bool, error) {
	if u.ftsCurrent == ftsCurrentUnknown {
		vals, err := getOmapKeys(u.idx, oidFTSMap, []string{ftsVersionKey, ftsRebuildKey})
		if err != nil && errCode(err) != syscall.ENOENT {
			return false, err
		}

		u.ftsCurrent = ftsCurrentNo
		if indexCurrent(vals) {
			u.ftsCurrent = ftsCurrentYes
		}
	}

	return u.ftsCurrent == ftsCurrentYes, nil
}

// freshIndexKeys are the keys of the id map markFreshIndex looks at.
var freshIndexKeys = []string{"n", ftsVersionKey, ftsRebuildKey}

// markFreshIndex adds the version marker to the first write of the id
// map of an index this version writes from the start: there is no id
// yet ("n"), no rebuild is running, and fresh says that no posting
// shard existed — an account indexed before document ids existed has
// posting lists of GUIDs and no "n", and must not be marked.
func markFreshIndex(cur, set map[string][]byte, fresh bool) {
	_, hasN := cur["n"]
	_, rebuilding := cur[ftsRebuildKey]

	if !hasN && !rebuilding && fresh {
		set[ftsVersionKey] = []byte(ftsVersion)
	}
}

// Whether the index is fresh, as ensureDocIDs knows it in a round.
const (
	freshUnknown = iota
	freshYes
	freshNo
)

// freshByShards is the answer of ensureDocIDs for an id map without
// ids: fresh unless a posting shard exists.
func (u *User) freshByShards() (int, error) {
	exist, err := u.ftsPostingsExist()
	if err != nil {
		return freshUnknown, err
	}

	if exist {
		return freshNo, nil
	}

	return freshYes, nil
}

// ftsPostingsExist reports whether any posting shard object exists.
func (u *User) ftsPostingsExist() (bool, error) {
	for i := 0; i < u.ftsShards; i++ {
		if _, err := u.idx.Stat(fmt.Sprintf("%s%02d", ftsPrefix, i)); err == nil {
			return true, nil
		} else if errCode(err) != syscall.ENOENT {
			return false, err
		}
	}

	return false, nil
}

// errRebuildSuperseded is what a rebuild gets whose key another rebuild
// replaced: that one deleted what this one indexed, and it sets the
// marker when it is done.
var errRebuildSuperseded = errors.New("another rebuild of the index started meanwhile")

// startFTSRebuild sets ftsRebuildKey to a token of this run and returns
// it; the index is not trusted until finishFTSRebuild with the token.
func (u *User) startFTSRebuild() (string, error) {
	u.ftsCurrent = ftsCurrentUnknown
	token := newGUID()

	return token, casOmap(u, u.idx, oidFTSMap, nil, func(_ map[string][]byte) (map[string][]byte, []string) {
		return map[string][]byte{ftsRebuildKey: []byte(token)}, nil
	})
}

// finishFTSRebuild sets the version marker and removes ftsRebuildKey in
// one write — only while the key still holds the token of this run.
func (u *User) finishFTSRebuild(token string) error {
	u.ftsCurrent = ftsCurrentUnknown

	var superseded bool

	err := casOmap(u, u.idx, oidFTSMap, []string{ftsRebuildKey}, func(cur map[string][]byte) (map[string][]byte, []string) {
		if superseded = !rebuildOwned(cur, token); superseded {
			return nil, nil
		}

		return map[string][]byte{ftsVersionKey: []byte(ftsVersion)}, []string{ftsRebuildKey}
	})
	if err == nil && superseded {
		err = errRebuildSuperseded
	}

	return err
}

// rebuildOwned reports whether the rebuild key holds token.
func rebuildOwned(cur map[string][]byte, token string) bool {
	v, ok := cur[ftsRebuildKey]

	return ok && string(v) == token
}

// SearchCandidates returns a superset of the messages whose text in one
// of fields contains query as a substring (IMAP SEARCH semantics), or
// nil when the index cannot narrow them. The caller checks every
// candidate against the message.
func (u *User) SearchCandidates(fields []string, query string) (hits map[string]bool, err error) {
	end := u.trace(spanFTSSearch, attribute.StringSlice("store.fields", fields))
	defer func() { end(err) }()

	return searchCandidates(radosIndex{u}, fields, query)
}

// searchCandidates intersects the candidates of the words of the query
// (fts.Spans) that the index can look up completely; a word it cannot
// leaves the set as it is, and with no such word the result is nil.
//
// A word is looked up by how it must occur: a whole word by its term, a
// word's start by a scan of the posting shards for keys with that
// prefix, an inner part through the substring dictionary — the last two
// with the messages of the markers it needs (fts.Markers). Incomplete,
// and so not narrowing, are: an inner part in a field without the
// dictionary or of fewer than three characters, a scan or dictionary
// answer of more than maxCandidates terms, an account on the dictionary
// layout before 0.35.0. An index that cannot be trusted (see
// ftsVersionKey) narrows nothing.
func searchCandidates(ix candidateIndex, fields []string, query string) (map[string]bool, error) {
	current, err := ix.current()
	if err != nil || !current {
		return nil, err
	}

	var result map[string]bool

	for _, sp := range fts.Spans(query) {
		terms, ok, err := spanTerms(ix, fields, sp)
		if err != nil {
			return nil, err
		}

		if !ok {
			continue
		}

		hits, err := ix.postingsUnion(terms)
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

// spanTerms returns every term of fields that can hold the span, and
// the markers it needs; false when the index cannot list them all.
func spanTerms(ix candidateIndex, fields []string, sp fts.Span) ([]string, bool, error) {
	var terms []string

	for _, f := range fields {
		if !fts.SpanIndexed(f, sp) {
			return nil, false, nil
		}

		var (
			found    []string
			complete bool
			err      error
		)

		switch sp.Match {
		case fts.MatchWord:
			terms = append(terms, f+":"+sp.Token)

			continue
		case fts.MatchPrefix:
			// The posting shards hold every term, the dictionary only
			// those of fields that have one: a prefix is looked up there.
			found, complete, err = prefixKeys(ix, f+":"+sp.Token)
		default:
			legacy, lerr := ix.legacyDictPresent()
			if lerr != nil || legacy {
				return nil, false, lerr
			}

			found, complete, err = dictMatches(ix.list, f, sp.Token, fts.Trigrams(sp.Token))
		}

		if err != nil || !complete {
			return nil, false, err
		}

		for _, t := range found {
			if _, tok, _ := strings.Cut(t, ":"); fts.SpanTerm(sp, tok) {
				terms = append(terms, t)
			}
		}

		for _, m := range fts.Markers(sp) {
			terms = append(terms, f+":"+m)
		}
	}

	return terms, true, nil
}

// prefixKeys lists the terms that start with prefix across the posting
// shards, keys only, and whether that is all of them: false when there
// are more than maxCandidates. Every term has its key in exactly one
// posting shard, so the scan of all shards to their end is complete.
func prefixKeys(ix candidateIndex, prefix string) ([]string, bool, error) {
	var (
		mu       sync.Mutex
		out      []string
		complete = true
	)

	shards := map[string][]string{}
	for i := 0; i < ix.shards(); i++ {
		shards[fmt.Sprintf("%s%02d", ftsPrefix, i)] = nil
	}

	err := parallel(shards, func(oid string, _ []string) error {
		var (
			after string
			found []string
		)

		for more := true; more; {
			var err error

			more, err = ix.list(oid, after, prefix, omapPageSize, func(key string) {
				after = key
				found = append(found, key)
			})
			if err != nil {
				return fmt.Errorf("fts shard %s: %w", oid, err)
			}

			if more && len(found) >= maxCandidates {
				mu.Lock()
				complete = false
				mu.Unlock()

				break
			}
		}

		mu.Lock()
		defer mu.Unlock()

		out = append(out, found...)

		return nil
	})

	return out, complete && len(out) <= maxCandidates, err
}
