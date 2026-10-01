package model

import (
	"encoding/json"
	"errors"
	"time"
)

// Skip-if-idle of the batch scrub (scaling plan B7). A scrub over every
// account of the directory leaves an account alone when nothing can have
// changed since its last complete scrub that found nothing. What the run
// compares is kept in the account's directory entry (AccountEntry.Scrub):
// the account counter (h:seq of the account change log) and the object
// version of the registry, both read after the scrub's own writes.

// ScrubState is the change state of an account a batch scrub compares.
type ScrubState struct {
	// AccountSeq is the account counter (h:seq of the change log): every
	// change of a mailbox or its messages raises it.
	AccountSeq uint64 `json:"seq"`
	// RegistryVersion is the object version of the account's registry
	// (mailboxes, quota counters, delivery intents, the import lock).
	RegistryVersion uint64 `json:"reg"`
}

// ScrubProber is implemented by user views a batch scrub may skip.
type ScrubProber interface {
	// ScrubState reads the account counter and the registry version.
	ScrubState() (ScrubState, error)
	// ScrubPending names what keeps the account from being skipped
	// whatever its state ("" for nothing): orphan candidates or a quota
	// count in the scrub object (the two-run rule waits for a second
	// look), open delivery intents (d:) or MOVE intents (i:).
	ScrubPending() (string, error)
}

// ScrubOutcome is implemented by scrub reports.
type ScrubOutcome interface {
	// Findings is the number of findings of the run (repaired or not).
	Findings() int
	// Complete reports whether the run looked at everything: not skipped
	// for an import, no error, the index scan complete.
	Complete() bool
}

// AccountScrub is the scrub record of a directory entry: when the last
// batch scrub of the account ended, the state it left the account in and
// what it found.
type AccountScrub struct {
	At       time.Time  `json:"at"`
	State    ScrubState `json:"state"`
	Complete bool       `json:"complete"`
	Findings int        `json:"findings"`
}

// ScrubOf decodes the scrub record of an entry; false without one (or an
// unreadable one, which counts as none).
func (e AccountEntry) ScrubOf() (AccountScrub, bool) {
	var s AccountScrub
	if len(e.Scrub) == 0 || json.Unmarshal(e.Scrub, &s) != nil || s.At.IsZero() {
		return AccountScrub{}, false
	}

	return s, true
}

// errNoEntry aborts SetAccountScrub without a write.
var errNoEntry = errors.New("no directory entry")

// SetAccountScrub stores the scrub record in the account's entry. An
// absent entry is not created (the directory only holds accounts with a
// registry), and the rest of the entry stays as it is.
func SetAccountScrub(ix SortedIndex, account string, rec AccountScrub) error {
	if !InDirectory(account) {
		return nil
	}

	b, err := json.Marshal(rec)
	if err != nil {
		return err
	}

	err = ix.Update(account, func(cur []byte) ([]byte, error) {
		if cur == nil {
			return nil, errNoEntry
		}

		e, err := DecodeAccountEntry(cur)
		if err != nil {
			return cur, nil // leave an unreadable entry to the repair
		}

		e.Scrub = b

		return json.Marshal(e)
	})
	if errors.Is(err, errNoEntry) {
		return nil
	}

	return err
}

// ScrubIdle decides whether a batch scrub may skip the account: its last
// scrub (last, false when there is none) was complete and found nothing,
// it is younger than interval, the account state is still the one that
// scrub left, and nothing is pending.
func ScrubIdle(last AccountScrub, ok bool, now ScrubState, pending string, at time.Time, interval time.Duration) bool {
	switch {
	case !ok, !last.Complete, last.Findings > 0, pending != "":
		return false
	case interval > 0 && at.Sub(last.At) >= interval:
		return false
	default:
		return last.State == now
	}
}
