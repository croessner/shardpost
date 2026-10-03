package cephstore

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"syscall"

	"go.opentelemetry.io/otel/attribute"

	"shardpost/internal/fts"
	"shardpost/internal/model"
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
//
// Since 0.50.0 (B14h) the marker also says what a version before it must
// not trust (temp/13 §4.3), because it would miss messages: "3" for a
// generation whose dictionary has the identifier filter, "4" for one
// built without the signature fields of attached headers (with or
// without the filter). Such a version trusts only "2" and narrows
// nothing there (IMAP reads every message: slow, but right). This
// version trusts every one of them (knownVersion) with one exception:
// an index built with the text preparation "nosig" — "4", or a layout
// record with "ah" — only while fts.attached_headers is "nosig" too,
// since otherwise it lacks words the IMAP check looks for (ftsTrusted).
// The dictionary filter needs no such rule: the readers always read the
// start entries, and whether a generation has the filter is part of its
// search semantics, not of this server's configuration.
const (
	ftsVersionKey  = "v"
	ftsVersion     = "2"
	ftsVersionDict = "3"
	ftsVersionText = "4"
	ftsRebuildKey  = "r"
)

// versionFor is the version marker of a generation this version
// indexed: "4" with the text preparation "nosig", else "3" with the
// identifier filter, else "2".
func versionFor(loc ftsLoc) string {
	switch {
	case loc.ah == fts.AttachedHeadersNoSig:
		return ftsVersionText
	case loc.dx > 0:
		return ftsVersionDict
	default:
		return ftsVersion
	}
}

// knownVersion reports whether a version marker is one of this version.
func knownVersion(v []byte) bool {
	switch string(v) {
	case ftsVersion, ftsVersionDict, ftsVersionText:
		return true
	}

	return false
}

// versionText is the text preparation a generation was built with by its
// layout record (ah, "" for layout 2) and its version marker v: "nosig"
// when either says so. A version before 0.50.0 that rewrote the record
// dropped "ah"; the marker it left ("4") still says it.
func versionText(ah string, v []byte) string {
	if ah == fts.AttachedHeadersNoSig || string(v) == ftsVersionText {
		return fts.AttachedHeadersNoSig
	}

	return ""
}

// scanReq is one page of a key range scan: the keys after after that
// start with prefix, at most limit, each handed to fn; more is set by
// the read when the range goes on.
type scanReq struct {
	after, prefix string
	limit         uint64
	fn            func(key string)
	more          bool
}

// candidateIndex is what the candidate search reads.
type candidateIndex interface {
	// scan reads a page of each range of one object; ranges of the same
	// object go in one read.
	scan(oid string, reqs []*scanReq) error
	// postingsUnion returns the messages of the posting lists of terms.
	postingsUnion(terms []string) (map[string]bool, error)
	// legacyDictPresent reports whether the account still has the
	// dictionary layout before 0.35.0.
	legacyDictPresent() (bool, error)
	// current reports whether the index can be trusted (indexCurrent).
	current() (bool, error)
	// postingOIDs are the posting objects; dictOID is the dictionary
	// object of a trigram (injected, so that the tests run the search
	// over either layout's names, N12).
	postingOIDs() []string
	dictOID(tri string) string
}

// list is one page of one range of an object.
func list(ix candidateIndex, oid, after, prefix string, limit uint64, fn func(string)) (bool, error) {
	r := &scanReq{after: after, prefix: prefix, limit: limit, fn: fn}
	err := ix.scan(oid, []*scanReq{r})

	return r.more, err
}

// indexCurrent reports, from the keys ftsVersionKey and ftsRebuildKey of
// the id map, whether a layout 2 index can be trusted by a server with
// the default text preparation: the marker is there and no rebuild is
// running.
func indexCurrent(vals map[string][]byte) bool {
	return ftsTrusted(vals, ftsLoc{f: model.FTSLayout2}, "")
}

// ftsTrusted reports whether the index of generation loc can be trusted
// by a server whose fts.attached_headers is readerAH ("" or "nosig"):
// the version marker of its layout (ftsVersionKey3 in layout 3) is one
// this version knows, no rebuild is running, and the generation's text
// preparation (versionText) has every word the server's IMAP check
// looks for.
func ftsTrusted(vals map[string][]byte, loc ftsLoc, readerAH string) bool {
	_, rebuilding := vals[ftsRebuildKey]

	key := ftsVersionKey
	if loc.v3() {
		key = ftsVersionKey3
	}

	v := vals[key]
	if !knownVersion(v) || rebuilding {
		return false
	}

	return versionText(loc.ah, v) == "" || readerAH == fts.AttachedHeadersNoSig
}

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
// It decides the layout of the new index (§4.2): with fts.layout 3 and
// the cluster mark a generation sized for m messages is created and
// named in the same write, and the account entered in the presence
// index first; otherwise the index is layout 2 and the account leaves
// the presence index. The caller deleted the old index (DeleteFTS).
func (u *User) startFTSRebuild(m int) (string, error) {
	u.ftsForget()

	token := newGUID()

	want3, err := u.layout3Wanted()
	if err != nil {
		return "", err
	}

	set := map[string][]byte{ftsRebuildKey: []byte(token)}

	var gen *ftsLoc

	if want3 {
		dx, ah, err := u.genProps()
		if err != nil {
			return "", err
		}

		s, d, h := u.ftsCfg.sizing.shapeFor(m)
		loc := ftsLoc{f: model.FTSLayout3, g: newFTSGen(), s: s, d: d, h: h, dx: dx, ah: ah}

		if err := u.markLayout3(); err != nil {
			return "", err
		}

		// the old index is gone (DeleteFTS): Props follows the new one
		if err := u.markLayoutProps(loc.hasProps()); err != nil {
			return "", err
		}

		if err := u.createGen(loc); err != nil {
			_ = u.deleteObjects(loc.objects())
			return "", err
		}

		gen = &loc
		l := newLayoutRecord(loc)
		set[ftsLayoutKey], set[ftsCountKey] = l.encode(), []byte("0")
	} else if err := u.unmarkLayout3(); err != nil {
		return "", err
	}

	err = casOmap(u, u.idx, oidFTSMap, nil, func(_ map[string][]byte) (map[string][]byte, []string) {
		return set, nil
	})
	if err != nil && gen != nil {
		_ = u.deleteObjects(gen.objects())
	}

	return token, err
}

// finishFTSRebuild sets the version marker of the layout and removes
// ftsRebuildKey in one write — only while the key still holds the token
// of this run.
func (u *User) finishFTSRebuild(token string) error {
	defer u.ftsForget()

	var superseded bool

	err := casOmap(u, u.idx, oidFTSMap, []string{ftsRebuildKey, ftsLayoutKey}, func(cur map[string][]byte) (map[string][]byte, []string) {
		if superseded = !rebuildOwned(cur, token); superseded {
			return nil, nil
		}

		// Layout 2 has neither filter nor text preparation of its own: a
		// rebuild into it indexes with the default preparation.
		key, v := ftsVersionKey, ftsVersion
		if l, _ := parseLayout(cur[ftsLayoutKey]); l != nil && l.F == model.FTSLayout3 {
			key, v = ftsVersionKey3, versionFor(curLoc(l, u.ftsShards))
		}

		return map[string][]byte{key: []byte(v)}, []string{ftsRebuildKey}
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

	err = u.ftsRead(func(ix *radosIndex) error {
		hits, err = searchCandidates(ix, fields, query)
		return err
	})

	return hits, err
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

			found, complete, err = dictMatches(ix, f, sp.Token, fts.Trigrams(sp.Token))
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
	for _, oid := range ix.postingOIDs() {
		shards[oid] = nil
	}

	err := parallel(shards, func(oid string, _ []string) error {
		var (
			after string
			found []string
		)

		for more := true; more; {
			var err error

			more, err = list(ix, oid, after, prefix, omapPageSize, func(key string) {
				after = key
				found = append(found, key)
			})
			if err != nil {
				return err
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
