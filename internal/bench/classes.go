package bench

import "strings"

// Object classes of the measurement: the kinds of RADOS objects an
// account has (docs/architecture/storage.md, "Physical model"). The
// set is fixed and small, so a class can label anything without a
// cardinality concern.
const (
	ClassRegistry  = "registry"   // mailboxes
	ClassAccount   = "account"    // account
	ClassMbox      = "mbox"       // mbox/<id>: header and first UID shard
	ClassMboxShard = "mbox-shard" // mbox/<id>/<n>
	ClassMsg       = "msg"        // msg/<guid>/<n> (data pool)
	ClassFTS       = "fts"        // fts/<nn>: posting lists
	ClassFTST      = "ftst"       // ftst/<nnn>: trigram dictionary
	ClassFTSH      = "ftsh"       // ftsh/<nn>: header store
	ClassFTSD      = "ftsd"       // ftsd/<nn>: dictionary before 0.35.0
	ClassFTSMap    = "ftsmap"     // ftsmap
	ClassFTXP      = "ftx-p"      // ftx/<gen>/p<i>: layout 3 posting lists
	ClassFTXD      = "ftx-d"      // ftx/<gen>/d<j>: layout 3 dictionary
	ClassFTXH      = "ftx-h"      // ftx/<gen>/h<k>: layout 3 header texts
	ClassMetadata  = "metadata"   // metadata
	ClassSieve     = "sieve"      // sieve
	ClassScrub     = "scrub"      // scrub
	ClassIx        = "ix"         // ix/<space>/<nn>: front-end indexes
	ClassPubSeen   = "pubseen"    // pubseen/<id>
	ClassStoreIx   = "store-ix"   // ix.<name>/... of the store's own namespaces
	ClassOther     = "other"
)

// prefixClasses map an object name prefix to its class, longest first
// where one prefix starts another.
var prefixClasses = []struct{ prefix, class string }{
	{"msg/", ClassMsg},
	{"ftsmap", ClassFTSMap},
	{"ftst/", ClassFTST},
	{"ftsh/", ClassFTSH},
	{"ftsd/", ClassFTSD},
	{"fts/", ClassFTS},
	{"ix/", ClassIx},
	{"ix.", ClassStoreIx},
	{"pubseen/", ClassPubSeen},
}

var exactClasses = map[string]string{
	"mailboxes": ClassRegistry,
	"account":   ClassAccount,
	"metadata":  ClassMetadata,
	"sieve":     ClassSieve,
	"scrub":     ClassScrub,
}

// Classify returns the class of an object name.
func Classify(oid string) string {
	if c, ok := exactClasses[oid]; ok {
		return c
	}

	if rest, ok := strings.CutPrefix(oid, "mbox/"); ok {
		if strings.Contains(rest, "/") {
			return ClassMboxShard
		}

		return ClassMbox
	}

	if rest, ok := strings.CutPrefix(oid, "ftx/"); ok {
		return ftxClass(rest)
	}

	for _, p := range prefixClasses {
		if strings.HasPrefix(oid, p.prefix) {
			return p.class
		}
	}

	return ClassOther
}

// ftxClass is the class of a layout 3 object "<gen>/<kind><n>".
func ftxClass(rest string) string {
	_, obj, ok := strings.Cut(rest, "/")
	if !ok || obj == "" {
		return ClassOther
	}

	switch obj[0] {
	case 'p':
		return ClassFTXP
	case 'd':
		return ClassFTXD
	case 'h':
		return ClassFTXH
	default:
		return ClassOther
	}
}
