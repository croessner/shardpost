package model

import (
	"context"
	"errors"
)

// Statistics of an account's full-text index ("shardpostctl fts stats",
// temp/13 §7.1). On a real account they are key counts only: the
// counters of the posting objects by default (S + 1 reads, no lock), or
// every object's keys counted page by page on request. The terms of an
// account are classified only for the test accounts the configuration
// names (fts.stats_test_accounts), and never printed.

// FTSStatsRequest asks for the statistics of an index.
type FTSStatsRequest struct {
	// Exact counts the keys of every object page by page instead of
	// reading the counters, with Parallel objects at a time.
	Exact    bool
	Parallel int
	// Classes classifies the terms of the posting objects (test accounts
	// only; the caller checks).
	Classes bool
	// Context ends a long count early (nil: none).
	Context context.Context
}

// Kinds of index objects in FTSObjectStats.
const (
	FTSObjectPostings   = "postings"
	FTSObjectDictionary = "dictionary"
	FTSObjectHeaders    = "headers"
	// FTSObjectLegacy is the dictionary of layout 2 before 0.35.0.
	FTSObjectLegacy = "legacy"
)

// FTSStats are the statistics of an index.
type FTSStats struct {
	// Shape is the current generation (layout 2: its fixed counts).
	Shape FTSLayoutShape `json:"shape"`
	// Docs are the documents of the index: the counter "c" of layout 3;
	// for layout 2 only with Exact (the id map).
	Docs int64 `json:"docs"`
	// Exact is set when the keys were counted, not the counters read.
	Exact bool `json:"exact"`
	// Terms and DictKeys are the posting and dictionary keys: the sums
	// of the counters "!n" and "!dn", or the counted keys. HeaderKeys
	// are the header texts (counted only).
	Terms      int64 `json:"terms"`
	DictKeys   int64 `json:"dict_keys"`
	HeaderKeys int64 `json:"header_keys"`
	// Objects are the objects read, in order.
	Objects []FTSObjectStats `json:"objects"`
	// Classes are the classes of the terms (test accounts only).
	Classes *FTSTermClasses `json:"classes,omitempty"`
}

// FTSObjectStats are the keys of one index object.
type FTSObjectStats struct {
	Object string `json:"object"`
	Kind   string `json:"kind"`
	// Keys are the keys without the markers: counted, or for a posting
	// object without Exact its counter "!n".
	Keys int64 `json:"keys"`
	// DictKeys is the counter "!dn" of a posting object (without Exact).
	DictKeys int64 `json:"dict_keys,omitempty"`
}

// FTSTermClasses are the terms of a test account by field, class and
// length, and what the dictionary would hold with the identifier rule
// (temp/13 §4.1) for every bound n.
type FTSTermClasses struct {
	Terms    int64           `json:"terms"`
	DictKeys int64           `json:"dict_keys"`
	Rows     []FTSTermClass  `json:"rows"`
	ByIdent  []FTSIdentBound `json:"by_ident_min_runes"`
}

// FTSTermClass counts the terms of a field, class (letters, digits,
// mixed, other) and length range in runes, and their dictionary keys.
type FTSTermClass struct {
	Field    string `json:"field"`
	Class    string `json:"class"`
	Length   string `json:"length"`
	Terms    int64  `json:"terms"`
	DictKeys int64  `json:"dict_keys"`
}

// FTSIdentBound is the projection for one identifier bound: the
// dictionary keys with the rule and the terms it counts as identifiers.
type FTSIdentBound struct {
	MinRunes    int   `json:"min_runes"`
	DictKeys    int64 `json:"dict_keys"`
	Identifiers int64 `json:"identifiers"`
}

// FTSStatser is implemented by user views that report index statistics.
type FTSStatser interface {
	FTSStats(req FTSStatsRequest) (FTSStats, error)
}

// ErrFTSStatsClasses refuses a classification of an account that is not
// a test account.
var ErrFTSStatsClasses = errors.New("the terms of an account are classified only for the test accounts of fts.stats_test_accounts")
