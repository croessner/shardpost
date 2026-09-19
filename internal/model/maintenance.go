package model

import "time"

// Repair classes of the scrubber.
const (
	RepairDeliveries = "deliveries" // abandoned multi-target deliveries
	RepairMoves      = "moves"      // unfinished MOVE intents
	RepairRetired    = "retired"    // mailboxes left retired by an import
	RepairUncovered  = "uncovered"  // entries at or above UIDNEXT
	RepairEntries    = "entries"    // index entries without blob
	RepairRefs       = "refs"       // blob reference counts
	RepairOrphans    = "orphans"    // blobs without index entry
	RepairUnknown    = "unknown"    // index objects without registry entry
	RepairQuota      = "quota"      // quota counters
)

// RepairClasses lists every repair class of the scrubber.
var RepairClasses = []string{RepairDeliveries, RepairMoves, RepairRetired, RepairUncovered, RepairEntries, RepairRefs,
	RepairOrphans, RepairUnknown, RepairQuota}

// ScrubRequest is one scrub of an account from a maintenance job.
type ScrubRequest struct {
	// DryRun only reports; Repairs selects repair classes (nil = all).
	DryRun  bool
	Repairs map[string]bool
	Grace   time.Duration
	// Fence (if set) is checked before each repair; failing, the repair
	// is skipped (the job lost its lease).
	Fence func() error
}

// Scrubber is implemented by user views that can check and repair their
// store structures; the report is JSON-encodable.
type Scrubber interface {
	RunScrub(req ScrubRequest) (report any, err error)
}

// QuotaRecounter is implemented by user views that can recount their
// quota usage from the index (with apply, the counters are replaced).
type QuotaRecounter interface {
	RecountQuota(apply bool) (counted, stored Quota, err error)
}

// FTSRebuilder is implemented by user views that can rebuild their
// full-text index; fence is called before every write, progress after
// each message.
type FTSRebuilder interface {
	RebuildFTS(fence func() error, progress func(done, total int)) error
}

// NamespacePurger is implemented by user views that can remove every
// object of the account (index and data); fence is called before every
// batch of deletes. It returns the number of objects removed.
type NamespacePurger interface {
	PurgeNamespace(fence func() error) (int, error)
}
