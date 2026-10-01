package cephstore

import (
	"fmt"
	"strings"
	"sync"
	"syscall"
	"unicode/utf8"

	"go.opentelemetry.io/otel/attribute"

	"shardpost/internal/fts"
)

// The candidate search of IMAP SEARCH (substring semantics) has to
// return every message that contains the value. Its reads go through
// candidateIndex, so that the tests can run it over an index in memory
// with the same key layout.

// The index version marker: the key ftsVersionKey of "ftsmap" holds
// ftsVersion when every message of the account was indexed by a version
// that prepares text as this one does (fts.Terms with window terms and
// markers, header names, charset conversion). A fresh index gets it
// with its first document id; RebuildFTS sets it when it is done.
const (
	ftsVersionKey = "v"
	ftsVersion    = "2"
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
	// current reports whether the index has the version marker.
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

// Whether the index has the version marker, cached per view.
const (
	ftsCurrentUnknown = iota
	ftsCurrentYes
	ftsCurrentNo
)

// ftsIndexCurrent reports whether the index has the version marker.
func (u *User) ftsIndexCurrent() (bool, error) {
	if u.ftsCurrent == ftsCurrentUnknown {
		vals, err := getOmapKeys(u.idx, oidFTSMap, []string{ftsVersionKey})
		if err != nil && errCode(err) != syscall.ENOENT {
			return false, err
		}

		u.ftsCurrent = ftsCurrentNo
		if string(vals[ftsVersionKey]) == ftsVersion {
			u.ftsCurrent = ftsCurrentYes
		}
	}

	return u.ftsCurrent == ftsCurrentYes, nil
}

// markFreshIndex adds the version marker to the first write of the id
// map: a fresh index is written by this version from the start. A
// rebuild sets it when it is done instead.
func (u *User) markFreshIndex(cur, set map[string][]byte) {
	if _, ok := cur["n"]; !ok && !u.ftsRebuilding {
		set[ftsVersionKey] = []byte(ftsVersion)
	}
}

// setFTSVersion sets the version marker.
func (u *User) setFTSVersion() error {
	return casOmap(u, u.idx, oidFTSMap, nil, func(_ map[string][]byte) (map[string][]byte, []string) {
		return map[string][]byte{ftsVersionKey: []byte(ftsVersion)}, nil
	})
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
// layout before 0.35.0. An index without the version marker was written
// by a version that prepared text differently (no window terms or
// markers, no header names, no charset conversion): there only whole
// ASCII words outside the header field narrow, which every version
// indexed alike.
func searchCandidates(ix candidateIndex, fields []string, query string) (map[string]bool, error) {
	current, err := ix.current()
	if err != nil {
		return nil, err
	}

	var result map[string]bool

	for _, sp := range fts.Spans(query) {
		if !current && !oldIndexAnswers(fields, sp) {
			continue
		}

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

// oldIndexAnswers reports whether an index without the version marker
// can narrow by a span: a whole ASCII word outside the header field.
func oldIndexAnswers(fields []string, sp fts.Span) bool {
	if sp.Match != fts.MatchWord {
		return false
	}

	for _, f := range fields {
		if f == fts.FieldHeader {
			return false
		}
	}

	for i := 0; i < len(sp.Token); i++ {
		if sp.Token[i] >= utf8.RuneSelf {
			return false
		}
	}

	return true
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
