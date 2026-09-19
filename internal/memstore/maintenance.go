package memstore

import "shardpost/internal/model"

// Maintenance operations mirroring cephstore for the job tests.

var (
	_ model.Scrubber        = (*User)(nil)
	_ model.QuotaRecounter  = (*User)(nil)
	_ model.FTSRebuilder    = (*User)(nil)
	_ model.NamespacePurger = (*User)(nil)
)

// ScrubReport is the memstore scrub report (nothing to repair).
type ScrubReport struct {
	User    string
	DryRun  bool
	Repairs []string
}

// RunScrub implements model.Scrubber.
func (u *User) RunScrub(req model.ScrubRequest) (any, error) {
	rep := &ScrubReport{User: u.name, DryRun: req.DryRun}
	for c := range req.Repairs {
		rep.Repairs = append(rep.Repairs, c)
	}

	return rep, nil
}

// RecountQuota implements model.QuotaRecounter.
func (u *User) RecountQuota(apply bool) (model.Quota, model.Quota, error) {
	u.mu.Lock()
	defer u.mu.Unlock()

	var counted model.Quota

	for _, b := range u.boxes {
		for _, m := range b.msgs {
			counted.Bytes += m.Size
			counted.Msgs++
		}
	}

	stored := u.usage
	if apply {
		u.usage = counted
	}

	return counted, stored, nil
}

// RebuildFTS implements model.FTSRebuilder.
func (u *User) RebuildFTS(fence func() error, progress func(done, total int)) error {
	if err := fence(); err != nil {
		return err
	}

	u.mu.Lock()
	defer u.mu.Unlock()

	u.terms = map[string]map[string]bool{}
	done := 0

	for guid, raw := range u.blobs {
		u.index(guid, raw)

		done++
		progress(done, len(u.blobs))
	}

	return nil
}

// PurgeNamespace implements model.NamespacePurger.
func (u *User) PurgeNamespace(fence func() error) (int, error) {
	if err := fence(); err != nil {
		return 0, err
	}

	u.mu.Lock()
	defer u.mu.Unlock()

	n := len(u.boxes) + len(u.blobs) + len(u.scripts) + len(u.shared)
	u.boxes, u.blobs, u.terms = map[string]*box{}, map[string][]byte{}, map[string]map[string]bool{}
	u.scripts, u.active, u.shared = nil, "", map[string]*model.SharedRef{}
	u.usage, u.imp = model.Quota{}, nil

	return n, nil
}
