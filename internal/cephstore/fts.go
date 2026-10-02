package cephstore

import (
	"encoding/hex"
	"fmt"
	"hash/fnv"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/ceph/go-ceph/rados"

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
// of the messages that contain it, the terms, and the header text of
// every message (fts.IndexText decodes the header once for both).
func termDocs(docs []ftsDoc, ids map[string]uint64) (map[string][]uint64, []string, map[string]string) {
	docsOf := map[string][]uint64{}
	headers := make(map[string]string, len(docs))

	for _, d := range docs {
		terms, header := fts.IndexText(d.raw)
		headers[d.guid] = header

		for _, t := range terms {
			docsOf[t] = append(docsOf[t], ids[d.guid])
		}
	}

	terms := make([]string, 0, len(docsOf))
	for t := range docsOf {
		terms = append(terms, t)
	}

	return docsOf, terms, headers
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
// turns a legacy GUID list into the id encoding using known. casPostings
// calls fn only after legacyGUIDsNeeded found every GUID of the lists in
// known, so a GUID without an id does not occur there.
func legacyConverter(known map[string]uint64) func([]byte) []byte {
	return func(p []byte) []byte {
		if len(p) == 0 || !postings.IsLegacy(p) {
			return p
		}

		var ids []uint64

		for _, g := range postings.LegacyGUIDs(p) {
			if id, ok := known[hex.EncodeToString(g)]; ok {
				ids = append(ids, id)
			}
		}

		return postings.Encode(ids)
	}
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

// take looks at the next key of the range.
func (s *dictScan) take(key, tok string) {
	s.after = key

	if strings.Contains(key[len(s.prefix):], tok) {
		s.found = append(s.found, key[s.skip:])
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

// Whether the account still has objects of the old dictionary layout.
const (
	legacyDictUnknown = iota
	legacyDictPresent
	legacyDictAbsent
)

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

func keys(set map[string]struct{}) []string {
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}

	sort.Strings(out)

	return out
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
	return casOmapOpAs(u, ioctx, oid, keys, "", fn)
}

// casOmapOpAs is casOmapOp on behalf of an import lock session (the
// ImportLock CAS; "" for every other). Creating the account's registry
// registers the account in the directory first (registerAccount): once
// per account lifetime, and refused with ErrImportLocked while a purge
// of the account is under way, unless session is the purge's own. A
// round lost to another writer is counted (casRetry, by object class)
// and followed by a growing random wait (casBackoff).
func casOmapOpAs(u *User, ioctx *rados.IOContext, oid string, keys []string, session string,
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

		if ver == 0 && oid == oidRegistry && ioctx == u.idx {
			if err := u.registerAccount(session); err != nil {
				return err
			}
		}

		op := casWriteOp(ver, set, rm, extra)
		err = op.Operate(ioctx, oid, rados.OperationNoFlag)
		op.Release()

		if err == nil {
			return nil
		}

		switch errCode(err) {
		case syscall.ERANGE, syscall.EOVERFLOW, syscall.EEXIST:
			mh.casRetry("omap", casClass(oid))

			if attempt+1 < casRetries {
				time.Sleep(casBackoff(attempt))
			}

			continue
		}

		return err
	}

	return fmt.Errorf("%s: too many concurrent writers", oid)
}

// casWriteOp builds the write of one CAS round: an exclusive create at
// version 0, otherwise guarded by the version read.
func casWriteOp(ver uint64, set map[string][]byte, rm []string, extra func(*rados.WriteOp)) *rados.WriteOp {
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

	return op
}
