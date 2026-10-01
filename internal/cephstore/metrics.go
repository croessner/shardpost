package cephstore

import (
	"github.com/ceph/go-ceph/rados"

	"shardpost/internal/metrics"
)

// metricsHooks keeps the store's dependency on the metrics package in one
// place.
type metricsHooks struct{}

var mh metricsHooks

func (metricsHooks) casRetry(kind string)   { metrics.CASRetries.WithLabelValues(kind).Inc() }
func (metricsHooks) timer(op string) func() { return metrics.Timer(op) }
func (metricsHooks) listing(site string)    { metrics.RadosListings.WithLabelValues(site).Inc() }

// Call sites of RADOS object listings (label "site" of
// imapceph_rados_listings_total).
const (
	listUsers         = "list_users"
	listScrubOrphans  = "scrub_orphans"
	listScrubUnknown  = "scrub_unknown_index"
	listPurgeObjects  = "purge_objects"
	listAdminKV       = "admin_kv"
	listAdminBackfill = "admin_kv_backfill"
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
