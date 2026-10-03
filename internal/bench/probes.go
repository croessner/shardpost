package bench

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math/rand/v2"
	"slices"
	"strings"
	"sync"

	"shardpost/internal/fts"
	"shardpost/internal/model"
)

// The expunge and search probes of measure (--expunge, --search). They
// work on any model.User, so the memory store tests them too.

// ErrNothingToExpunge is what ExpungeOldest returns for an empty
// mailbox.
var ErrNothingToExpunge = errors.New("no message left to expunge")

// ExpungeOldest expunges the message with the lowest UID of the mailbox:
// the store's expunge of one message (what IMAP EXPUNGE does after the
// client marked it \Deleted; the flag change is not part of it).
func ExpungeOldest(u model.User, mailbox string) error {
	mb, err := u.Lookup(mailbox)
	if err != nil {
		return err
	}

	metas, err := u.ListMessages(mb.ID)
	if err != nil {
		return err
	}

	if len(metas) == 0 {
		return fmt.Errorf("%s: %w", mailbox, ErrNothingToExpunge)
	}

	oldest := slices.MinFunc(metas, func(a, b model.Meta) int { return int(a.UID) - int(b.UID) })

	return u.Expunge(mailbox, []model.Meta{oldest})
}

// Kinds of search queries: a whole word, the start of a word and a part
// from inside a word (3 to 6 characters); with the identifier corpus
// also an identifier (the reference of a message, 32 hex digits) as a
// whole, its first 8 characters and 8 characters from inside it.
const (
	QueryWord    = "word"
	QueryPrefix  = "prefix"
	QueryInfix   = "infix"
	QueryIDWhole = "id-whole"
	QueryIDStart = "id-start"
	QueryIDInner = "id-inner"
)

// idQueryMessages are the messages whose references the identifier
// queries take (every account has at least these).
const idQueryMessages = 5

// IMAP search keys of the queries.
const (
	KeyText    = "TEXT"
	KeyBody    = "BODY"
	KeySubject = "SUBJECT"
	KeyFrom    = "FROM"
)

// SearchQuery is one query of the search probe: an IMAP search key
// (TEXT, BODY, SUBJECT, FROM) with its value.
type SearchQuery struct {
	Kind string `json:"kind"`
	Key  string `json:"key"`
	Text string `json:"text"`
	// Rare is set for a word drawn from the tail of the vocabulary.
	Rare bool `json:"rare"`
	// Msg is the message whose reference an identifier query takes; its
	// text is resolved per account (ResolveQuery).
	Msg int `json:"msg,omitempty"`
}

var (
	queryKinds = []string{QueryWord, QueryPrefix, QueryInfix}
	queryKeys  = []string{KeyText, KeyBody, KeySubject, KeyFrom}
)

// SearchQueries returns k queries from the generator's vocabulary: the
// kinds, keys and frequent and rare words in turn. They depend on k
// and i only, not on the account or the seed, so every account gets the
// same queries and the result fingerprints of two runs can be compared.
func SearchQueries(k int) []SearchQuery {
	out := make([]SearchQuery, 0, k)

	for i := range k {
		r := rand.New(rand.NewPCG(uint64(i), 0x5ea4c4)) //nolint:gosec // a fixed query set, not a secret
		q := SearchQuery{Kind: queryKinds[i%len(queryKinds)], Key: queryKeys[(i/len(queryKinds))%len(queryKeys)], Rare: i%2 == 1}

		var w string

		if q.Rare {
			w = term(uint64(len(common)) + 1000 + r.Uint64N(vocabSize-uint64(len(common))-1000))
		} else {
			w = term(r.Uint64N(uint64(len(common))))
		}

		q.Kind, q.Text = queryText(q.Kind, w, r)
		out = append(out, q)
	}

	return out
}

// SearchQueriesFor returns k queries for a corpus: the queries of
// SearchQueries for the plain corpus; for the identifier corpus every
// second query is an identifier query (whole, start, inner in turn, of
// BODY and TEXT), the others are SearchQueries's.
func SearchQueriesFor(k int, c Corpus) []SearchQuery {
	if !c.IDs {
		return SearchQueries(k)
	}

	words := SearchQueries((k + 1) / 2)
	idKinds := []string{QueryIDWhole, QueryIDStart, QueryIDInner}
	out := make([]SearchQuery, 0, k)

	for i := range k {
		if i%2 == 0 {
			out = append(out, words[i/2])
			continue
		}

		j := i / 2
		key := KeyBody
		if (j/len(idKinds))%2 == 1 {
			key = KeyText
		}

		out = append(out, SearchQuery{Kind: idKinds[j%len(idKinds)], Key: key, Msg: j % idQueryMessages})
	}

	return out
}

// ResolveQuery gives an identifier query its text in account: the
// reference of message q.Msg, whole or cut. Other queries are returned
// as they are.
func ResolveQuery(g Generator, account string, q SearchQuery) SearchQuery {
	id := ""

	switch q.Kind {
	case QueryIDWhole, QueryIDStart, QueryIDInner:
		id = g.RefID(account, q.Msg)
	default:
		return q
	}

	switch q.Kind {
	case QueryIDStart:
		q.Text = id[:8]
	case QueryIDInner:
		q.Text = id[12:20]
	default:
		q.Text = id
	}

	return q
}

// queryText cuts the query of a kind out of word w: 3 to 6 characters,
// fewer where the word is short (a prefix leaves at least the last
// character out, an infix the first and the last). A word too short for
// a cut of 3 characters becomes a word query, and the kind says so.
func queryText(kind, w string, r *rand.Rand) (string, string) {
	n := 3 + r.IntN(4)

	switch kind {
	case QueryPrefix:
		if n = min(n, len(w)-1); n >= 3 {
			return kind, w[:n]
		}
	case QueryInfix:
		if n = min(n, len(w)-2); n >= 3 {
			start := 1 + r.IntN(len(w)-n-1)
			return kind, w[start : start+n]
		}
	}

	return QueryWord, w
}

// searchFields are the index fields of an IMAP search key, as the
// protocol layer asks for them: TEXT as its header and its body.
func searchFields(key string) [][]string {
	switch key {
	case KeyText:
		return [][]string{{fts.FieldHeader}, {fts.FieldBody}}
	case KeyBody:
		return [][]string{{fts.FieldBody}}
	case KeySubject:
		return [][]string{{fts.FieldSubject}}
	default:
		return [][]string{{fts.FieldFrom}}
	}
}

// SearchHits is what one query found in one account: the candidates
// of the substring search (SearchCandidates, per field set; nil: the
// index cannot narrow) and the word search (SearchFields over all the
// key's fields).
type SearchHits struct {
	Candidates []map[string]bool
	Words      map[string]bool
}

// Search runs q on u the way IMAP SEARCH asks the index: SearchCandidates
// for each field set of the key (fts.imap_search: substring) and
// SearchFields over them together (the word search of JMAP and
// fts.imap_search: word).
func Search(u model.User, q SearchQuery) (SearchHits, error) {
	var (
		h   SearchHits
		all []string
	)

	for _, fields := range searchFields(q.Key) {
		c, err := u.SearchCandidates(fields, q.Text)
		if err != nil {
			return h, err
		}

		h.Candidates = append(h.Candidates, c)
		all = append(all, fields...)
	}

	w, err := u.SearchFields(all, q.Text)
	h.Words = w

	return h, err
}

// SearchQueryResult is the result of one query over all probe accounts.
type SearchQueryResult struct {
	SearchQuery

	// Candidates and Words count the messages found (an account where
	// the index cannot narrow counts none and is in NotNarrowed).
	Candidates  int `json:"candidates"`
	Words       int `json:"words"`
	NotNarrowed int `json:"not_narrowed,omitempty"`
	// Fingerprint is a SHA-256 over the sorted results of every account
	// (account, field set, GUID), equal for equal results.
	Fingerprint string `json:"fingerprint"`
}

// SearchResults collects the hits of the queries, by account; safe for
// concurrent use.
type SearchResults struct {
	mu      sync.Mutex
	queries []SearchQuery
	lines   [][]string
	res     []SearchQueryResult
}

// NewSearchResults collects the results of queries.
func NewSearchResults(queries []SearchQuery) *SearchResults {
	r := &SearchResults{queries: queries, lines: make([][]string, len(queries)), res: make([]SearchQueryResult, len(queries))}
	for i, q := range queries {
		r.res[i].SearchQuery = q
	}

	return r
}

// Add records the hits of query i in account.
func (r *SearchResults) Add(i int, account string, h SearchHits) {
	var lines []string

	res := SearchQueryResult{}

	for j, c := range h.Candidates {
		if c == nil {
			res.NotNarrowed++

			lines = append(lines, fmt.Sprintf("%s\tc%d\t*", account, j))

			continue
		}

		res.Candidates += len(c)
		for g := range c {
			lines = append(lines, fmt.Sprintf("%s\tc%d\t%s", account, j, g))
		}
	}

	res.Words = len(h.Words)
	for g := range h.Words {
		lines = append(lines, fmt.Sprintf("%s\tw\t%s", account, g))
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	r.lines[i] = append(r.lines[i], lines...)
	r.res[i].Candidates += res.Candidates
	r.res[i].NotNarrowed += res.NotNarrowed
	r.res[i].Words += res.Words
}

// Results returns the result of every query, with its fingerprint.
func (r *SearchResults) Results() []SearchQueryResult {
	r.mu.Lock()
	defer r.mu.Unlock()

	out := slices.Clone(r.res)

	for i := range out {
		lines := slices.Sorted(slices.Values(r.lines[i]))
		sum := sha256.Sum256([]byte(strings.Join(lines, "\n")))
		out[i].Fingerprint = hex.EncodeToString(sum[:])
	}

	return out
}

// Fingerprint is a SHA-256 over the fingerprints of all queries.
func Fingerprint(results []SearchQueryResult) string {
	h := sha256.New()
	for _, r := range results {
		h.Write([]byte(r.Fingerprint))
	}

	return hex.EncodeToString(h.Sum(nil))
}
