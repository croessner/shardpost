package memstore

import (
	"encoding/json"
	"hash/fnv"
	"strings"

	"shardpost/internal/model"
)

// Maintenance operations mirroring cephstore for the job tests.

var (
	_ model.Scrubber         = (*User)(nil)
	_ model.QuotaRecounter   = (*User)(nil)
	_ model.FTSRebuilder     = (*User)(nil)
	_ model.FTSDictConverter = (*User)(nil)
	_ model.NamespacePurger  = (*User)(nil)
	_ model.ScrubProber      = (*User)(nil)
	_ model.ScrubOutcome     = (*ScrubReport)(nil)
)

// ScrubReport is the memstore scrub report (nothing to repair).
type ScrubReport struct {
	User    string
	DryRun  bool
	Repairs []string
}

// Findings implements model.ScrubOutcome (this store has nothing to
// find).
func (r *ScrubReport) Findings() int { return 0 }

// Complete implements model.ScrubOutcome.
func (r *ScrubReport) Complete() bool { return true }

// ScrubState implements model.ScrubProber. This store has no registry
// object and so no version; a hash of what the RADOS registry holds
// (mailbox records, quota counters and limits, the import lock) stands
// in for it, so a change of any of them changes the state as there.
func (u *User) ScrubState() (model.ScrubState, error) {
	boxes, err := u.ListMailboxes()
	if err != nil {
		return model.ScrubState{}, err
	}

	u.mu.Lock()
	defer u.mu.Unlock()

	h := fnv.New64a()
	b, _ := json.Marshal([]any{boxes, u.usage, u.limit, u.imp != nil, u.registered.Load()})
	_, _ = h.Write(b)

	return model.ScrubState{AccountSeq: u.acct.seq, RegistryVersion: h.Sum64()}, nil
}

// ScrubPending implements model.ScrubProber: this store keeps no scrub
// state and no intents.
func (u *User) ScrubPending() (string, error) { return "", nil }

// RunScrub implements model.Scrubber.
func (u *User) RunScrub(req model.ScrubRequest) (any, error) {
	rep := &ScrubReport{User: u.name, DryRun: req.DryRun}
	for c := range req.Repairs {
		rep.Repairs = append(rep.Repairs, c)
	}

	return rep, nil
}

// RebuildFTS implements model.FTSRebuilder.
func (u *User) RebuildFTS(fence func() error, progress func(done, total int)) error {
	if err := fence(); err != nil {
		return err
	}

	_, err := u.rebuild(progress)

	return err
}

// rebuild indexes every message again; it returns how many.
func (u *User) rebuild(progress func(done, total int)) (int, error) {
	u.mu.Lock()
	defer u.mu.Unlock()

	u.terms = map[string]map[string]bool{}
	done := 0

	for guid, raw := range u.blobs {
		u.index(guid, raw)

		done++
		progress(done, len(u.blobs))
	}

	return done, nil
}

// LegacyFTSDict implements model.FTSDictConverter; this store has no
// dictionary and so never one to convert.
func (u *User) LegacyFTSDict() (int, error) { return 0, nil }

// ConvertFTSDict implements model.FTSDictConverter: it counts the terms.
func (u *User) ConvertFTSDict(fence func() error, progress func(done, total int)) (model.FTSDictReport, error) {
	rep := model.FTSDictReport{Terms: map[string]int64{}}

	if err := fence(); err != nil {
		return rep, err
	}

	u.mu.Lock()
	defer u.mu.Unlock()

	for term := range u.terms {
		field, _, _ := strings.Cut(term, ":")
		rep.Terms[field]++
	}

	progress(1, 1)

	return rep, nil
}

// PurgeNamespace implements model.NamespacePurger.
func (u *User) PurgeNamespace(fence func() error) (int, error) {
	if err := fence(); err != nil {
		return 0, err
	}

	u.mu.Lock()
	defer u.mu.Unlock()

	// Everything the RADOS namespaces would hold goes, as there: the
	// registry (mailboxes, quota counters and limits, intents, the used
	// names, the import lock, the trace switch, share pointers, public
	// subscriptions), the account log, the indexes, METADATA, the Sieve
	// object with its vacation records, the \Seen marks of public
	// folders, blobs and the full-text index (TestPurgeLeavesNothing).
	n := len(u.boxes) + len(u.blobs) + len(u.scripts) + len(u.shared)
	u.boxes, u.blobs, u.terms = map[string]*box{}, map[string][]byte{}, map[string]map[string]bool{}
	u.scripts, u.active, u.shared = nil, "", map[string]*model.SharedRef{}
	u.usage, u.limit, u.imp, u.trace = model.Quota{}, model.Quota{}, nil, model.TraceState{}
	u.annot, u.vacSeen, u.subMiss, u.pubSubs, u.pubSeen = nil, nil, nil, nil, nil
	u.indexes, u.usedNames, u.intents, u.subCopies = nil, nil, 0, nil
	u.acct.entries, u.acct.seq, u.acct.logStart = nil, 0, 0
	u.registered.Store(false) // the registry goes last

	return n, nil
}
