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
	// Holder names the run's repair mark (stage B16): the job's token,
	// the admin request; "" for a name of the store's choosing.
	Holder string
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

// FTSDictReport is what a conversion of the substring dictionary did.
type FTSDictReport struct {
	// Terms counts the terms of the posting lists by field.
	Terms map[string]int64 `json:"terms"`
	// Entries is the number of dictionary entries written for them.
	Entries int64 `json:"entries"`
	// LegacyObjects is the number of objects of the old layout removed.
	LegacyObjects int `json:"legacy_objects"`
	// Skipped says why nothing was converted (the index has layout 3 or
	// a layout conversion is under way).
	Skipped string `json:"skipped,omitempty"`
}

// FTSDictSkippedLayout3 is FTSDictReport.Skipped of an account whose
// index has layout 3 (or is being converted): it has no layout 2
// dictionary to convert.
const FTSDictSkippedLayout3 = "the index has layout 3 or a layout conversion is under way: no layout 2 dictionary to convert"

// FTSDictConverter is implemented by user views whose substring
// dictionary has an older layout to convert from. LegacyFTSDict reports
// how many objects of that layout the account still has. ConvertFTSDict
// writes the dictionary again from the posting lists and removes them; it
// may run beside deliveries and searches, and running it again is
// harmless. fence is called before every batch of writes, progress after
// each posting shard.
type FTSDictConverter interface {
	LegacyFTSDict() (int, error)
	ConvertFTSDict(fence func() error, progress func(done, total int)) (FTSDictReport, error)
}

// NamespacePurger is implemented by user views that can remove every
// object of the account (index and data); fence is called before every
// batch of deletes. It returns the number of objects removed.
type NamespacePurger interface {
	PurgeNamespace(fence func() error) (int, error)
}

// OrphanSource is where a scrub finds what a crash left behind: objects
// that nothing points at (scaling plan B6, B7).
type OrphanSource string

// Orphan sources of the scrub.
const (
	// OrphanSourceListing lists the account's namespace in both pools
	// (the behaviour before 0.42.0).
	OrphanSourceListing OrphanSource = "listing"
	// OrphanSourceShadow lists as before and acts on what the listing
	// finds, and also works out what the intents name; differences are
	// reported (a migration aid: the listing stays authoritative).
	OrphanSourceShadow OrphanSource = "shadow"
	// OrphanSourceIntents reads the intents only and lists nothing.
	OrphanSourceIntents OrphanSource = "intents"
)

// Valid reports whether s is a known orphan source ("" is not).
func (s OrphanSource) Valid() bool {
	switch s {
	case OrphanSourceListing, OrphanSourceShadow, OrphanSourceIntents:
		return true
	}

	return false
}
