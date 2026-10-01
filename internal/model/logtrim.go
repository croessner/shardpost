package model

// Trimming of the sequence-keyed logs (the mailbox change log
// "c:<modseq>" and the account change log "a:<seq>").
//
// Both logs carry their head (h:modseq, h:seq) and the oldest sequence
// still covered (h:logstart) in their header, so the size of the window
// the log covers is known without reading it:
//
//	U = head - logstart + 1
//
// Only when U exceeds the retention does the trim read anything, and then
// only the keys below the new start, page by page from the beginning of
// the log.
//
// The rule differs in one respect from the count-based rule it replaces
// (which read the whole log and kept the newest `retention` existing
// records): it keeps the records whose sequence is at least
// head-retention+1. The two are identical when the records above
// h:logstart are contiguous:
//
//   - The mailbox log is: every modseq allocation writes c:<modseq> in the
//     same CAS as h:modseq (casHeader/headerSet). A new mailbox starts at
//     h:modseq=1 and h:logstart=1 without a c:1 record, so before its
//     first trim U is one more than the number of records; the new rule
//     then finds no key below the start and does nothing, exactly where
//     the old rule did nothing. An import moves h:logstart past h:modseq
//     and leaves older records below h:logstart; the old rule never moved
//     the start into them (its CAS refuses to move h:logstart backwards),
//     and both rules remove them with the first trim that runs.
//   - The account log is not: an announcement (p:<seq>) that is not yet
//     recorded is a gap near the head, and a withdrawn announcement is a
//     permanent gap. With gaps the new start can be later than the old
//     one by the number of missing sequences in the retained window, so
//     a client may have to resynchronise slightly earlier. Nothing a
//     reader may rely on changes: AccountChanges answers "not covered"
//     for any token below h:logstart either way.

// LogPageReader returns at most limit keys of a log that sort after the
// key after ("" for the beginning), in ascending order. Fewer than limit
// keys mean the log ends there.
type LogPageReader func(after string, limit int) ([]string, error)

// PlanLogTrim decides what a trim of a sequence-keyed log removes. head
// is the newest sequence allocated, logStart the oldest still covered (0
// is read as 1: an account log that was never trimmed has no h:logstart),
// key formats the log key of a sequence (it must sort like the sequence)
// and read lists the log. pageSize bounds a page.
//
// It returns the new log start and the keys below it, or start 0 when
// there is nothing to do; the caller removes the keys and moves
// h:logstart to start in one CAS that refuses to move it backwards.
func PlanLogTrim(head, logStart uint64, retention, pageSize int, key func(uint64) string, read LogPageReader) (start uint64, drop []string, err error) {
	if retention <= 0 || pageSize <= 0 {
		return 0, nil, nil
	}

	if logStart == 0 {
		logStart = 1
	}

	if head < logStart || head-logStart+1 <= uint64(retention) {
		return 0, nil, nil
	}

	start = head - uint64(retention) + 1
	bound := key(start)

	// The first page holds the sequences between the current start and
	// the new one plus the record at the new start: for a contiguous log
	// that is exactly what is dropped and the key that ends the read, so
	// the common case reads one page.
	limit := int(min(start-logStart+1, uint64(pageSize)))
	after := ""

	for {
		keys, err := read(after, limit)
		if err != nil {
			return 0, nil, err
		}

		for _, k := range keys {
			if k >= bound {
				return finish(start, drop)
			}

			drop = append(drop, k)
		}

		if len(keys) < limit {
			return finish(start, drop)
		}

		after = keys[len(keys)-1]
		limit = pageSize
	}
}

// finish returns the plan, or nothing when no key lies below start (the
// window is over the retention only by sequences that hold no record).
func finish(start uint64, drop []string) (uint64, []string, error) {
	if len(drop) == 0 {
		return 0, nil, nil
	}

	return start, drop, nil
}
