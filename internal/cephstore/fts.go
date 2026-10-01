package cephstore

import (
	"encoding/hex"
	"fmt"
	"hash/fnv"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"

	"github.com/ceph/go-ceph/rados"
	"go.opentelemetry.io/otel/attribute"

	"shardpost/internal/fts"
	"shardpost/internal/postings"
)

// Full-text index ("flatcurve on RADOS")
//
// Per user there are N shard objects "fts/<shard>" in the index pool. Each
// shard's omap maps a term ("s:invoice", "b:hello") to its posting list:
// the document ids of all messages containing the term, sorted and
// delta-varint encoded (package postings; about one byte per posting
// instead of a 16-byte GUID). The object "ftsmap" maps GUID <-> document
// id ("g:<guid>" -> id, "i:<id>" -> guid, "n" -> next id); an id is
// allocated once per blob when it is indexed and freed when the blob is
// unindexed. Lists written before this encoding (raw GUIDs) are still
// read and are converted the first time they are written.
// The shard of a term is fnv1a(term) mod N, so every posting list has one
// home and an omap page never has to grow beyond what RocksDB handles well.
//
// Postings reference blobs (GUIDs via ids), not (mailbox, UID): COPY and
// MOVE never touch the index, and a blob is unindexed exactly when its
// last reference goes away. Updates are compare-and-swap on the shard object like everything
// else in this store, and the shards are written in parallel, so several
// pods may index concurrently.
//
// Stemming: Terms() indexes every token together with its Snowball stems,
// and queries are expanded the same way.
//
// Substrings: a second set of shard objects "ftst/<shard>" is the term
// dictionary. Its omap has one key per (trigram, term) pair,
// "<trigram>:<term>" (for example "voi:s:invoice"), with an empty value;
// the shard is fnv1a(trigram) mod dictShards, so the terms of a trigram
// are one key range of one object. A new term costs one key per trigram
// of its token, written without reading anything: the keys are set, and
// setting one twice changes nothing, so the update needs no
// compare-and-swap either. What a delivery writes therefore depends on
// the message, not on the size of the vocabulary.
//
// A substring query scans the key range "<trigram>:<field>:" of the
// token's trigrams, a page of each at a time, until the first range is
// exhausted; the terms of that range which contain the token are the
// answer, and only their postings are read. No message is ever read for
// a search. This is the RADOS counterpart of Xapian's term database that
// flatcurve uses for wildcard matching.
//
// The terms of the field "h" (every header value) are not in the
// dictionary, see fts.Substring.
//
// Until 0.34.6 the dictionary was "ftsd/<shard>" with one key per trigram
// and the newline separated list of all its terms as the value, which
// was read and rewritten whole for every new term. Nothing writes that
// layout any more; it is still read, in addition to the new one, until
// ConvertFTSDict has rebuilt the dictionary of the account from its
// posting lists and removed the old objects.
const (
	ftsPrefix        = "fts/"
	dictPrefix       = "ftst/"
	legacyDictPrefix = "ftsd/"
	oidFTSMap        = "ftsmap"
	DefaultFTSShards = 16
	guidBytes        = 16
	maxCandidates    = 4096 // terms per query token after substring filtering
	prefixScanMax    = 64   // terms per shard for the < 3 rune prefix fallback
	// dictShards is the number of dictionary objects of an account. It
	// keeps an object below the key count at which Ceph warns about large
	// omap objects for a dictionary of tens of millions of pairs, and it is
	// fixed for good: changing it would move every key.
	dictShards = 256
	dictSep    = ":"
	// dictWriteChunk bounds the keys of one dictionary write, dictWriters
	// the writes in flight; together they bound what an update of any size
	// holds in the client library at a time.
	dictWriteChunk = 2048
	dictWriters    = 8
	// legacyDictCapped is the value the old layout stored in place of a
	// term list of more than 2000 terms; such a trigram is skipped.
	legacyDictCapped = "\x00capped"
)

func shardNo(s string, n int) int {
	h := fnv.New32a()
	h.Write([]byte(s))

	return int(h.Sum32() % uint32(n))
}

func (u *User) ftsShardOID(term string) string {
	return fmt.Sprintf("%s%02d", ftsPrefix, shardNo(term, u.ftsShards))
}

func dictShardOID(tri string) string {
	return fmt.Sprintf("%s%03d", dictPrefix, shardNo(tri, dictShards))
}

func (u *User) legacyDictShardOID(tri string) string {
	return fmt.Sprintf("%s%02d", legacyDictPrefix, shardNo(tri, u.ftsShards))
}

func groupBy(items []string, oidOf func(string) string) map[string][]string {
	m := map[string][]string{}

	for _, it := range items {
		oid := oidOf(it)
		m[oid] = append(m[oid], it)
	}

	return m
}

// parallel runs fn for every group concurrently and returns the first error.
func parallel(groups map[string][]string, fn func(oid string, items []string) error) error {
	return parallelN(groups, len(groups), fn)
}

// parallelN is parallel with at most n groups in flight.
func parallelN(groups map[string][]string, n int, fn func(oid string, items []string) error) error {
	var wg sync.WaitGroup

	errs := make(chan error, len(groups))
	sem := make(chan struct{}, max(n, 1))

	for oid, items := range groups {
		wg.Add(1)

		sem <- struct{}{}

		go func(oid string, items []string) {
			defer wg.Done()
			defer func() { <-sem }()

			if err := fn(oid, items); err != nil {
				errs <- err
			}
		}(oid, items)
	}

	wg.Wait()
	close(errs)

	return <-errs
}

// ftsDoc is a stored message to index: its blob id and its text.
type ftsDoc struct {
	guid string
	raw  []byte
}

// ftsIndex adds the message's GUID to the posting list of every term and
// registers terms that are new for this user in the dictionary.
func (u *User) ftsIndex(guid string, raw []byte) error {
	return u.ftsIndexDocs([]ftsDoc{{guid: guid, raw: raw}})
}

// ftsIndexDocs indexes several messages at once. Indexing a message
// costs round trips — the id map, a read and a compare-and-swap per
// posting shard, a write per dictionary shard its new terms touch — so a
// batch of an import that pays them once instead of once per message is
// much cheaper.
func (u *User) ftsIndexDocs(docs []ftsDoc) (err error) {
	if len(docs) == 0 {
		return nil
	}

	guids, size, err := docGUIDs(docs)
	if err != nil {
		return err
	}

	end := u.trace(spanFTSIndex, attrGUID(docs[0].guid), attrCount(len(docs)), attrBytes(size))
	defer func() { end(err) }()

	ids, err := u.ensureDocIDs(guids)
	if err != nil {
		return err
	}

	docsOf, terms := termDocs(docs, ids)

	var (
		mu       sync.Mutex
		newTerms []string
	)

	add := func(oid string, terms []string) error {
		fresh, err := u.addPostings(oid, terms, docsOf)
		if err != nil {
			return fmt.Errorf("fts index shard %s: %w", oid, err)
		}

		mu.Lock()

		newTerms = append(newTerms, fresh...)
		mu.Unlock()

		return nil
	}

	if err := parallel(groupBy(terms, u.ftsShardOID), add); err != nil {
		return err
	}

	return u.dictUpdate(newTerms, true)
}

// addPostings adds the ids of docsOf to the posting lists of the terms
// of one shard and returns the terms that had no list yet. A term the
// read does not return counts as new and its list is written afresh, so
// the read must be complete: the terms go through in pieces of
// omapPageSize, as every other read by keys here does (resolveDocIDs),
// rather than relying on the OSD to answer any number of keys at once.
func (u *User) addPostings(oid string, terms []string, docsOf map[string][]uint64) ([]string, error) {
	var fresh []string

	for len(terms) > 0 {
		n := min(len(terms), omapPageSize)

		part, err := u.addPostingsPage(oid, terms[:n], docsOf)
		if err != nil {
			return nil, err
		}

		fresh = append(fresh, part...)
		terms = terms[n:]
	}

	return fresh, nil
}

// addPostingsPage is addPostings for at most omapPageSize terms.
func (u *User) addPostingsPage(oid string, terms []string, docsOf map[string][]uint64) ([]string, error) {
	var fresh []string

	err := u.casPostings(oid, terms, func(cur map[string][]byte, conv func([]byte) ([]byte, bool)) (map[string][]byte, []string, bool) {
		fresh = fresh[:0]
		set := map[string][]byte{}

		for _, t := range terms {
			p, ok := cur[t]
			if !ok {
				fresh = append(fresh, t)
			}

			if p, ok = conv(p); !ok {
				return nil, nil, false
			}

			set[t] = postings.AddAll(p, docsOf[t])
		}

		return set, nil, true
	})

	return fresh, err
}

// docGUIDs returns the blob ids of the messages and their total size; an
// id that is not a GUID is an error.
func docGUIDs(docs []ftsDoc) (guids []string, size int, err error) {
	for _, d := range docs {
		if g, err := hex.DecodeString(d.guid); err != nil || len(g) != guidBytes {
			return nil, 0, fmt.Errorf("bad guid %q", d.guid)
		}

		size += len(d.raw)
		guids = append(guids, d.guid)
	}

	return guids, size, nil
}

// termDocs tokenizes the messages and returns, for every term, the ids
// of the messages that contain it, and the terms.
func termDocs(docs []ftsDoc, ids map[string]uint64) (map[string][]uint64, []string) {
	docsOf := map[string][]uint64{}

	for _, d := range docs {
		for _, t := range fts.Terms(d.raw) {
			docsOf[t] = append(docsOf[t], ids[d.guid])
		}
	}

	terms := make([]string, 0, len(docsOf))
	for t := range docsOf {
		terms = append(terms, t)
	}

	return docsOf, terms
}

// casPostings is casOmap over posting lists with legacy conversion: fn
// receives the current lists and a converter that turns a legacy GUID
// list into the id encoding. Ids for GUIDs of legacy lists are allocated
// before the write; if a list gained GUIDs unknown to us in between, fn
// returns ok=false and the attempt is repeated with the new ids.
func (u *User) casPostings(oid string, terms []string, fn func(cur map[string][]byte, conv func([]byte) ([]byte, bool)) (map[string][]byte, []string, bool)) error {
	known := map[string]uint64{}

	for attempt := 0; attempt < casRetries; attempt++ {
		cur, err := getOmapKeys(u.idx, oid, terms)
		if err != nil && errCode(err) != syscall.ENOENT {
			return err
		}

		need := legacyGUIDsNeeded(cur, known)

		if len(need) > 0 {
			ids, err := u.ensureDocIDs(need)
			if err != nil {
				return err
			}

			for g, id := range ids {
				known[g] = id
			}
		}

		conv := legacyConverter(known)
		retry := false

		err = casOmap(u, u.idx, oid, terms, func(cur map[string][]byte) (map[string][]byte, []string) {
			set, rm, ok := fn(cur, conv)
			if !ok {
				retry = true
				return nil, nil
			}

			return set, rm
		})
		if err != nil {
			return err
		}

		if !retry {
			return nil
		}
	}

	return fmt.Errorf("fts %s: too many concurrent writers", oid)
}

// legacyGUIDsNeeded returns the GUIDs (hex) in legacy posting lists of cur
// that have no document id in known yet, each once.
func legacyGUIDsNeeded(cur map[string][]byte, known map[string]uint64) []string {
	var need []string

	needSet := map[string]bool{}

	for _, v := range cur {
		if len(v) > 0 && postings.IsLegacy(v) {
			for _, g := range postings.LegacyGUIDs(v) {
				gs := hex.EncodeToString(g)
				if _, ok := known[gs]; !ok && !needSet[gs] {
					needSet[gs] = true
					need = append(need, gs)
				}
			}
		}
	}

	return need
}

// legacyConverter returns the converter casPostings hands to its fn: it
// turns a legacy GUID list into the id encoding using known, and reports
// false if a GUID has no id in known.
func legacyConverter(known map[string]uint64) func([]byte) ([]byte, bool) {
	return func(p []byte) ([]byte, bool) {
		if len(p) == 0 || !postings.IsLegacy(p) {
			return p, true
		}

		var ids []uint64

		for _, g := range postings.LegacyGUIDs(p) {
			id, ok := known[hex.EncodeToString(g)]
			if !ok {
				return nil, false
			}

			ids = append(ids, id)
		}

		return postings.Encode(ids), true
	}
}

// ftsRemove deletes the message from every posting list of the message,
// drops terms that became empty from the dictionary and frees the
// document id. It re-tokenises the blob, which is cheaper than storing a
// per-message term list.
func (u *User) ftsRemove(m *Meta) error {
	raw, err := u.readBlob(m)
	if err != nil {
		return err
	}

	g, _ := hex.DecodeString(m.GUID)

	id, hasID, err := u.docIDOf(m.GUID)
	if err != nil {
		return err
	}

	var (
		mu   sync.Mutex
		gone []string
	)

	err = parallel(groupBy(fts.Terms(raw), u.ftsShardOID), func(oid string, terms []string) error {
		empty, err := u.ftsRemoveShard(oid, terms, g, id, hasID)
		if err != nil {
			return err
		}

		mu.Lock()

		gone = append(gone, empty...)
		mu.Unlock()

		return nil
	})
	if err != nil {
		return err
	}

	if hasID {
		if err := u.dropDocID(m.GUID, id); err != nil {
			u.log.Warn("fts docid release failed", "guid", m.GUID, "err", err)
		}
	}

	return u.dictUpdate(gone, false)
}

// ftsRemoveShard removes the message from the posting lists of terms on
// one FTS shard and returns the terms whose lists became empty.
func (u *User) ftsRemoveShard(oid string, terms []string, g []byte, id uint64, hasID bool) ([]string, error) {
	var empty []string

	err := casOmap(u, u.idx, oid, terms, func(cur map[string][]byte) (map[string][]byte, []string) {
		empty = empty[:0]
		set := map[string][]byte{}

		var rm []string

		for _, t := range terms {
			p := removeFromPosting(cur[t], g, id, hasID)

			if len(p) == 0 {
				rm = append(rm, t)
				empty = append(empty, t)
			} else if string(p) != string(cur[t]) {
				set[t] = p
			}
		}

		return set, rm
	})
	if err != nil {
		return nil, fmt.Errorf("fts remove shard %s: %w", oid, err)
	}

	return empty, nil
}

// removeFromPosting removes a message from one posting list: by GUID g
// from a legacy list, else by document id (if the message has one).
func removeFromPosting(p, g []byte, id uint64, hasID bool) []byte {
	if len(p) > 0 && postings.IsLegacy(p) {
		p = removeLegacyPosting(p, g)
	} else if hasID {
		p = postings.Remove(p, id)
	}

	return p
}

// --- document ids -----------------------------------------------------------

func docKey(guid string) string { return "g:" + guid }
func idKey(id uint64) string    { return "i:" + strconv.FormatUint(id, 10) }

// ensureDocIDs returns the ids of the GUIDs, allocating missing ones in
// one compare-and-swap on the map object.
func (u *User) ensureDocIDs(guids []string) (map[string]uint64, error) {
	// Deduplicate: a GUID named twice must not get two ids.
	seen := map[string]bool{}

	uniq := guids[:0:0]
	for _, g := range guids {
		if !seen[g] {
			seen[g] = true
			uniq = append(uniq, g)
		}
	}

	guids = uniq

	keys := make([]string, 0, len(guids)+1)
	for _, g := range guids {
		keys = append(keys, docKey(g))
	}

	keys = append(keys, "n")
	out := map[string]uint64{}

	err := casOmap(u, u.idx, oidFTSMap, keys, func(cur map[string][]byte) (map[string][]byte, []string) {
		for k := range out {
			delete(out, k)
		}

		next, _ := strconv.ParseUint(string(cur["n"]), 10, 64)
		if next == 0 {
			next = 1
		}

		set := map[string][]byte{}

		for _, g := range guids {
			if v, ok := cur[docKey(g)]; ok {
				out[g], _ = strconv.ParseUint(string(v), 10, 64)
				continue
			}

			out[g] = next
			set[docKey(g)] = []byte(strconv.FormatUint(next, 10))
			set[idKey(next)] = []byte(g)
			next++
		}

		if len(set) == 0 {
			return nil, nil
		}

		set["n"] = []byte(strconv.FormatUint(next, 10))

		return set, nil
	})
	if err != nil {
		return nil, fmt.Errorf("fts docids: %w", err)
	}

	return out, nil
}

// docIDOf returns the id of a GUID (ok=false: never indexed with ids).
func (u *User) docIDOf(guid string) (uint64, bool, error) {
	vals, err := getOmapKeys(u.idx, oidFTSMap, []string{docKey(guid)})
	if err != nil {
		if errCode(err) == syscall.ENOENT {
			return 0, false, nil
		}

		return 0, false, err
	}

	v, ok := vals[docKey(guid)]
	if !ok {
		return 0, false, nil
	}

	id, _ := strconv.ParseUint(string(v), 10, 64)

	return id, true, nil
}

func (u *User) dropDocID(guid string, id uint64) error {
	return casOmap(u, u.idx, oidFTSMap, nil, func(_ map[string][]byte) (map[string][]byte, []string) {
		return nil, []string{docKey(guid), idKey(id)}
	})
}

// resolveDocIDs maps ids back to GUIDs.
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

// dictUpdate adds the terms to the trigram dictionary or removes them.
//
// The posting lists say which terms are new or gone, and the dictionary
// follows in a second step. A term that one writer removes while another
// adds it again can therefore end up with postings and without its
// dictionary keys (it is then found by the exact word and its stems
// only, until ConvertFTSDict or a rebuild lists it again); a key without
// postings costs a lookup that finds nothing.
func (u *User) dictUpdate(terms []string, add bool) error {
	_, err := u.dictApply(terms, add)

	return err
}

// dictApply is dictUpdate that also returns the number of entries.
func (u *User) dictApply(terms []string, add bool) (n int, err error) {
	byShard, n := dictEntries(terms)
	if n == 0 {
		return 0, nil
	}

	end := u.trace(spanFTSDict, attrCount(n))
	defer func() { end(err) }()

	return n, parallelN(byShard, dictWriters, func(oid string, keys []string) error {
		for len(keys) > 0 {
			n := min(len(keys), dictWriteChunk)

			if err := dictWrite(u.idx, oid, keys[:n], add); err != nil {
				return fmt.Errorf("fts dictionary shard %s: %w", oid, err)
			}

			keys = keys[n:]
		}

		return nil
	})
}

// dictEntries returns the dictionary keys of the terms by shard object,
// and their number. Terms of fields without substring matching have none.
func dictEntries(terms []string) (map[string][]string, int) {
	byShard := map[string][]string{}
	n := 0

	for _, t := range terms {
		field, tok, _ := strings.Cut(t, ":")
		if !fts.Substring(field) {
			continue
		}

		for _, tri := range fts.Trigrams(tok) {
			oid := dictShardOID(tri)
			byShard[oid] = append(byShard[oid], tri+dictSep+t)
			n++
		}
	}

	return byShard, n
}

// dictWrite sets or removes dictionary keys of one shard object. Neither
// depends on what the object holds, so there is nothing to read first
// and nothing to retry.
func dictWrite(ioctx *rados.IOContext, oid string, keys []string, add bool) error {
	op := rados.CreateWriteOp()
	defer op.Release()

	if add {
		set := make(map[string][]byte, len(keys))
		for _, k := range keys {
			set[k] = nil
		}

		op.SetOmap(set)
	} else {
		op.RmOmapKeys(keys)
	}

	err := op.Operate(ioctx, oid, rados.OperationNoFlag)
	if !add && errCode(err) == syscall.ENOENT {
		return nil
	}

	return err
}

// splitTerms returns the terms of a term list of the old dictionary
// layout.
func splitTerms(v []byte) map[string]struct{} {
	out := map[string]struct{}{}
	if len(v) == 0 {
		return out
	}

	for _, t := range strings.Split(string(v), "\n") {
		if t != "" {
			out[t] = struct{}{}
		}
	}

	return out
}

// --- queries ----------------------------------------------------------------

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

	var result map[string]bool

	for _, tok := range toks {
		var terms []string

		for _, f := range fields {
			c, err := u.candidateTerms(f, tok)
			if err != nil {
				return nil, err
			}

			terms = append(terms, c...)
		}

		hits, err := u.postingsUnion(terms)
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

// SearchCandidates returns a superset of the messages whose text in one
// of fields contains query as a substring (IMAP SEARCH semantics), or
// nil when the index cannot narrow them. Every word of the query that
// the index can look up completely narrows the set (fts.Spans): a whole
// word by its term, a word's start by a bounded key scan, an inner part
// through the substring dictionary, each with the messages that have a
// word too long for a term. A word it cannot look up completely — an
// inner part of a word in a field without the dictionary or of fewer
// than three characters, a scan that reaches its bound, a dictionary
// answer that reaches maxCandidates, an account still on the dictionary
// layout before 0.35.0 — leaves the set as it is. The caller checks
// every candidate against the message.
func (u *User) SearchCandidates(fields []string, query string) (hits map[string]bool, err error) {
	end := u.trace(spanFTSSearch, attribute.StringSlice("store.fields", fields))
	defer func() { end(err) }()

	var result map[string]bool

	for _, sp := range fts.Spans(query) {
		terms, ok, err := u.spanTerms(fields, sp)
		if err != nil {
			return nil, err
		}

		if !ok {
			continue
		}

		hits, err := u.postingsUnion(terms)
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

// spanTerms returns every term of fields that can hold the span; false
// when the index cannot list them all.
func (u *User) spanTerms(fields []string, sp fts.Span) ([]string, bool, error) {
	var terms []string

	for _, f := range fields {
		if !fts.SpanIndexed(f, sp) {
			return nil, false, nil
		}

		if sp.Match == fts.MatchWord {
			terms = append(terms, f+":"+sp.Token)

			continue
		}

		var (
			found    []string
			complete bool
			err      error
		)

		if tris := fts.Trigrams(sp.Token); fts.Substring(f) && len(tris) > 0 {
			legacy, lerr := u.hasLegacyDict()
			if lerr != nil || legacy {
				return nil, false, lerr
			}

			found, complete, err = u.dictMatches(f, sp.Token, tris)
		} else {
			found, complete, err = u.prefixKeys(f + ":" + sp.Token)
		}

		if err != nil || !complete {
			return nil, false, err
		}

		for _, t := range found {
			if _, tok, _ := strings.Cut(t, ":"); fts.SpanTerm(sp, tok) {
				terms = append(terms, t)
			}
		}

		terms = append(terms, f+":"+fts.LongWord)
	}

	return terms, true, nil
}

// prefixKeys lists the terms that start with prefix across the posting
// shards, keys only, at most maxCandidates of them, and whether they
// are all of them.
func (u *User) prefixKeys(prefix string) ([]string, bool, error) {
	var (
		mu       sync.Mutex
		out      []string
		complete = true
	)

	shards := map[string][]string{}
	for i := 0; i < u.ftsShards; i++ {
		shards[fmt.Sprintf("%s%02d", ftsPrefix, i)] = nil
	}

	err := parallel(shards, func(oid string, _ []string) error {
		var (
			after string
			found []string
		)

		for more := true; more; {
			var err error

			more, err = listOmapKeys(u.idx, oid, after, prefix, omapPageSize, func(key string) {
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

// candidateTerms returns the index terms of field that a query token
// matches: exact, stemmed, and substring via the trigram dictionary.
func (u *User) candidateTerms(field, tok string) ([]string, error) {
	set := map[string]struct{}{}
	for _, t := range fts.Expand(tok) {
		set[field+":"+t] = struct{}{}
	}

	tris := fts.Trigrams(tok)
	sub := []string{field}

	if len(tris) == 0 || !fts.Substring(field) {
		// Too short for the dictionary, or a field that is not in it:
		// prefix scan on the sorted omap.
		terms, err := u.prefixTerms(field + ":" + tok)
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
		if err := u.substringTerms(set, f, tok, tris); err != nil {
			return nil, err
		}
	}

	return keys(set), nil
}

// substringTerms adds the terms of field that contain tok to set.
func (u *User) substringTerms(set map[string]struct{}, field, tok string, tris []string) error {
	terms, _, err := u.dictMatches(field, tok, tris)
	if err != nil {
		return err
	}

	for _, t := range terms {
		set[t] = struct{}{}
	}

	return u.legacyDictMatches(set, field, tok, tris)
}

// dictScan is the scan of one trigram's key range for the terms of a
// field that contain a token.
type dictScan struct {
	oid    string
	prefix string // "<trigram>:<field>:"
	skip   int    // length of "<trigram>:", what precedes the term in a key
	after  string
	found  []string
	done   bool
}

func newDictScan(tri, field string) *dictScan {
	return &dictScan{oid: dictShardOID(tri), prefix: tri + dictSep + field + ":", skip: len(tri) + len(dictSep)}
}

// take looks at the next key of the range.
func (s *dictScan) take(key, tok string) {
	s.after = key

	if strings.Contains(key[len(s.prefix):], tok) {
		s.found = append(s.found, key[s.skip:])
	}
}

// page reads the next page of the range.
func (s *dictScan) page(ioctx *rados.IOContext, tok string) error {
	more, err := listOmapKeys(ioctx, s.oid, s.after, s.prefix, omapPageSize, func(key string) { s.take(key, tok) })
	if err != nil {
		return fmt.Errorf("fts dictionary shard %s: %w", s.oid, err)
	}

	s.done = !more

	return nil
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
// ones.
func (u *User) dictMatches(field, tok string, tris []string) ([]string, bool, error) {
	scans := make([]*dictScan, len(tris))
	for i, tri := range tris {
		scans[i] = newDictScan(tri, field)
	}

	for {
		var wg sync.WaitGroup

		errs := make([]error, len(scans))

		for i, s := range scans {
			wg.Add(1)

			go func() {
				defer wg.Done()

				errs[i] = s.page(u.idx, tok)
			}()
		}

		wg.Wait()

		for _, err := range errs {
			if err != nil {
				return nil, false, err
			}
		}

		if s := finishedScan(scans); s != nil {
			return s.found, s.done, nil
		}
	}
}

// finishedScan returns a scan that has the answer: one that reached the
// end of its range, else one that has collected maxCandidates terms.
func finishedScan(scans []*dictScan) *dictScan {
	var full *dictScan

	for _, s := range scans {
		if s.done {
			return s
		}

		if len(s.found) >= maxCandidates {
			full = s
		}
	}

	return full
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

// Whether the account still has objects of the old dictionary layout.
const (
	legacyDictUnknown = iota
	legacyDictPresent
	legacyDictAbsent
)

// hasLegacyDict reports whether the account still has objects of the
// old dictionary layout. It is looked up once per view: a converted
// account must not pay sixteen reads of objects that are gone for every
// token of every query.
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
func (u *User) legacyDictMatches(set map[string]struct{}, field, tok string, tris []string) error {
	legacy, err := u.hasLegacyDict()
	if err != nil || !legacy {
		return err
	}

	inter, capped, err := u.legacyDictIntersect(tris)
	if err != nil {
		return err
	}

	if inter == nil && capped > 0 {
		// Every trigram of the token is too common to be selective: scan
		// the field's terms instead (bounded by the user's vocabulary).
		terms, err := u.prefixTermsN(field+":", omapPageSize)
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

// addSubstringTerms adds the terms of inter that belong to field and
// contain tok to set, at most maxCandidates of them.
func addSubstringTerms(set, inter map[string]struct{}, field, tok string) {
	prefix := field + ":"
	n := 0

	for t := range inter {
		if strings.HasPrefix(t, prefix) && strings.Contains(t[len(prefix):], tok) {
			set[t] = struct{}{}

			if n++; n >= maxCandidates {
				break
			}
		}
	}
}

// prefixTerms lists terms starting with prefix across all index shards
// (at most prefixScanMax per shard for the fallbacks of short tokens and
// of fields outside the dictionary, unbounded for the capped-trigram
// fallback of the old dictionary, which needs the field's vocabulary).
func (u *User) prefixTerms(prefix string) ([]string, error) {
	return u.prefixTermsN(prefix, prefixScanMax)
}

func (u *User) prefixTermsN(prefix string, perShard int64) ([]string, error) {
	var (
		mu  sync.Mutex
		out []string
	)

	shards := map[string][]string{}
	for i := 0; i < u.ftsShards; i++ {
		shards[fmt.Sprintf("%s%02d", ftsPrefix, i)] = nil
	}

	err := parallel(shards, func(oid string, _ []string) error {
		vals, err := u.idx.GetAllOmapValues(oid, "", prefix, perShard)
		if err != nil {
			if errCode(err) == syscall.ENOENT {
				return nil
			}

			return err
		}

		mu.Lock()
		for t := range vals {
			out = append(out, t)
		}
		mu.Unlock()

		return nil
	})

	return out, err
}

// postingsUnion returns the union of the posting lists of the terms, as
// GUIDs (ids are resolved through the map object; legacy lists carry
// GUIDs directly).
func (u *User) postingsUnion(terms []string) (map[string]bool, error) {
	out := map[string]bool{}
	if len(terms) == 0 {
		return out, nil
	}

	ids := map[uint64]bool{}

	var mu sync.Mutex

	err := parallel(groupBy(terms, u.ftsShardOID), func(oid string, ts []string) error {
		vals, err := getOmapKeys(u.idx, oid, ts)
		if err != nil {
			return fmt.Errorf("fts shard %s: %w", oid, err)
		}

		mu.Lock()
		defer mu.Unlock()

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

		return nil
	})
	if err != nil {
		return nil, err
	}

	resolved, err := u.resolveDocIDs(ids)
	if err != nil {
		return nil, err
	}

	for g := range resolved {
		out[g] = true
	}

	return out, nil
}

func keys(set map[string]struct{}) []string {
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}

	sort.Strings(out)

	return out
}

// DeleteFTS removes all index and dictionary shard objects (of both
// dictionary layouts) and the id map.
func (u *User) DeleteFTS() error {
	oids := u.legacyDictOIDs()

	for i := 0; i < u.ftsShards; i++ {
		oids = append(oids, fmt.Sprintf("%s%02d", ftsPrefix, i))
	}

	for i := 0; i < dictShards; i++ {
		oids = append(oids, fmt.Sprintf("%s%03d", dictPrefix, i))
	}

	for _, oid := range append(oids, oidFTSMap) {
		if err := u.idx.Delete(oid); err != nil && errCode(err) != syscall.ENOENT {
			return err
		}
	}

	return nil
}

// legacyDictOIDs are the objects of the old dictionary layout.
func (u *User) legacyDictOIDs() []string {
	oids := make([]string, 0, u.ftsShards)
	for i := 0; i < u.ftsShards; i++ {
		oids = append(oids, fmt.Sprintf("%s%02d", legacyDictPrefix, i))
	}

	return oids
}

// removeLegacyPosting removes a GUID from a legacy (raw GUID) list.
func removeLegacyPosting(p, g []byte) []byte {
	out := make([]byte, 0, len(p))
	for i := 0; i+guidBytes <= len(p); i += guidBytes {
		if string(p[i:i+guidBytes]) != string(g) {
			out = append(out, p[i:i+guidBytes]...)
		}
	}

	return out
}

// --- generic omap compare-and-swap ------------------------------------------

// getOmapKeysVer reads the given keys and returns the object version (0 if
// the object does not exist). The context must not be shared with other
// concurrent operations; use User.getKeysVer.
func getOmapKeysVer(ioctx *rados.IOContext, oid string, keys []string) (map[string][]byte, uint64, error) {
	op := rados.CreateReadOp()
	defer op.Release()

	step := op.GetOmapValuesByKeys(keys)
	if err := op.Operate(ioctx, oid, rados.OperationNoFlag); err != nil {
		if errCode(err) == syscall.ENOENT {
			return map[string][]byte{}, 0, nil
		}

		return nil, 0, err
	}

	ver, err := ioctx.GetLastVersion()
	if err != nil {
		return nil, 0, err
	}

	out := map[string][]byte{}

	for {
		kv, err := step.Next()
		if err != nil {
			return nil, 0, err
		}

		if kv == nil {
			break
		}

		out[kv.Key] = append([]byte(nil), kv.Value...)
	}

	return out, ver, nil
}

func getOmapKeys(ioctx *rados.IOContext, oid string, keys []string) (map[string][]byte, error) {
	v, _, err := getOmapKeysVer(ioctx, oid, keys)
	return v, err
}

// casOmap reads keys, lets fn compute changes and writes them back guarded
// by the object version. A missing object is created exclusively; a
// concurrent creator or writer triggers a retry.
func casOmap(u *User, ioctx *rados.IOContext, oid string, keys []string, fn func(cur map[string][]byte) (set map[string][]byte, rm []string)) (err error) {
	return casOmapOp(u, ioctx, oid, keys, func(cur map[string][]byte) (map[string][]byte, []string, func(*rados.WriteOp)) {
		set, rm := fn(cur)

		return set, rm, nil
	})
}

// casOmapOp is casOmap whose change may add further steps to the same
// write op (an xattr), which then take effect with the omap change or
// not at all. fn runs after the version was read, so what it reads of the
// object besides the keys is covered by the version check.
func casOmapOp(u *User, ioctx *rados.IOContext, oid string, keys []string,
	fn func(cur map[string][]byte) (set map[string][]byte, rm []string, extra func(*rados.WriteOp)),
) (err error) {
	end := u.trace(spanOmapCAS, attrOID(oid), attrCount(len(keys)))
	defer func() { end(err) }()

	for attempt := 0; attempt < casRetries; attempt++ {
		cur, ver, err := u.getKeysVer(ioctx, oid, keys)
		if err != nil {
			return err
		}

		set, rm, extra := fn(cur)
		if len(set) == 0 && len(rm) == 0 {
			return nil
		}

		op := rados.CreateWriteOp()
		if ver == 0 {
			op.Create(rados.CreateExclusive)
		} else {
			op.AssertVersion(ver)
		}

		if len(set) > 0 {
			op.SetOmap(set)
		}

		if len(rm) > 0 {
			op.RmOmapKeys(rm)
		}

		if extra != nil {
			extra(op)
		}

		err = op.Operate(ioctx, oid, rados.OperationNoFlag)
		op.Release()

		if err == nil {
			return nil
		}

		switch errCode(err) {
		case syscall.ERANGE, syscall.EOVERFLOW, syscall.EEXIST:
			mh.casRetry("omap")
			continue
		}

		return err
	}

	return fmt.Errorf("%s: too many concurrent writers", oid)
}
