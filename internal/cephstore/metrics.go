package cephstore

import (
	"math/rand/v2"
	"strings"
	"time"

	"github.com/ceph/go-ceph/rados"

	"shardpost/internal/metrics"
)

// metricsHooks keeps the store's dependency on the metrics package in one
// place.
type metricsHooks struct{}

var mh metricsHooks

func (metricsHooks) casRetry(kind, class string) {
	metrics.CASRetries.WithLabelValues(kind, class).Inc()
}
func (metricsHooks) timer(op string) func() { return metrics.Timer(op) }
func (metricsHooks) listing(site string)    { metrics.RadosListings.WithLabelValues(site).Inc() }

// Object classes of imapceph_rados_cas_retries_total (label "class"): a
// small fixed set, so that the label never grows with accounts or
// mailboxes.
const (
	casClassFTS      = "fts"      // fts/, ftst/, ftsh/, ftsd/: the full-text index
	casClassFTSMap   = "ftsmap"   // ftsmap: document ids and index markers
	casClassRegistry = "registry" // mailboxes: the mailbox registry
	casClassMbox     = "mbox"     // mbox/<id>[/<n>]: mailbox header and UID shards
	casClassAccount  = "account"  // account
	casClassIndex    = "index"    // ix/ and ix.<name>/: sorted indexes
	casClassMsg      = "msg"      // msg/: message blobs (reference counts)
	casClassOther    = "other"
)

// casClassPrefixes map an object name prefix to its class; "ftsmap" is
// matched before "fts".
var casClassPrefixes = []struct{ prefix, class string }{
	{oidFTSMap, casClassFTSMap},
	{"fts", casClassFTS},
	{mboxPrefix, casClassMbox},
	{msgPrefix, casClassMsg},
	{"ix/", casClassIndex},
	{"ix.", casClassIndex},
}

// casClass returns the class of an object for the retry counter.
func casClass(oid string) string {
	switch oid {
	case oidRegistry:
		return casClassRegistry
	case oidAccount:
		return casClassAccount
	}

	for _, p := range casClassPrefixes {
		if strings.HasPrefix(oid, p.prefix) {
			return p.class
		}
	}

	return casClassOther
}

// Backoff before the next round of a compare-and-swap that lost against
// another writer: a random wait of casBackoffMin to casBackoffMax, times
// the number of rounds lost so far, so that writers of one object that
// collided do not meet again in the next round.
const (
	casBackoffMin = time.Millisecond
	casBackoffMax = 10 * time.Millisecond
)

// casBackoff returns the wait after the lost round attempt (0-based).
func casBackoff(attempt int) time.Duration {
	d := casBackoffMin + rand.N(casBackoffMax-casBackoffMin+1) //nolint:gosec // jitter, not a secret

	return d * time.Duration(attempt+1)
}

// Call sites of RADOS object listings (label "site" of
// imapceph_rados_listings_total).
const (
	listUsers         = "list_users"
	listScrubOrphans  = "scrub_orphans"
	listScrubUnknown  = "scrub_unknown_index"
	listPurgeObjects  = "purge_objects"
	listAdminKV       = "admin_kv"
	listAdminBackfill = "admin_kv_backfill"
	listSweep         = "sweep"
)

// The learning queue's listing sites come from learnindex (SiteAll,
// SitePurge, SiteBackfill); learningRecords.List counts under the site it
// is given.

// listObjects starts a listing of the context's namespace (or of all
// namespaces) and counts it under site. It is the only place the store
// calls Iter (TestListingsAreCounted).
func listObjects(ioctx *rados.IOContext, site string) (*rados.Iter, error) {
	mh.listing(site)
	return ioctx.Iter()
}
