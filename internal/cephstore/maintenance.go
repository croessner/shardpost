package cephstore

import (
	"fmt"
	"strings"
	"syscall"

	"github.com/ceph/go-ceph/rados"

	"shardpost/internal/model"
)

// Maintenance operations for the admin jobs (roadmap 8c).

var (
	_ model.Scrubber         = (*User)(nil)
	_ model.QuotaRecounter   = (*User)(nil)
	_ model.FTSRebuilder     = (*User)(nil)
	_ model.FTSDictConverter = (*User)(nil)
	_ model.NamespacePurger  = (*User)(nil)
)

// RunScrub implements model.Scrubber.
func (u *User) RunScrub(req model.ScrubRequest) (any, error) {
	return u.Scrub(ScrubOptions{DryRun: req.DryRun, Repairs: req.Repairs, Grace: req.Grace, Fence: req.Fence})
}

// RecountQuota implements model.QuotaRecounter: the usage is counted
// over all index entries with the scrubber's rule (mailboxes excluded by
// the quota rules do not count). The caller holds the maintenance lock,
// so no delivery holds a reservation while the counters are replaced.
func (u *User) RecountQuota(apply bool) (Quota, Quota, error) {
	stored, err := u.QuotaUsage()
	if err != nil {
		return Quota{}, Quota{}, err
	}

	boxes, err := u.ListMailboxes()
	if err != nil {
		return Quota{}, Quota{}, err
	}

	var counted Quota

	for i := range boxes {
		if !u.rules.Counts(&boxes[i]) {
			continue
		}

		msgs, err := u.ListMessages(boxes[i].ID)
		if err != nil {
			return Quota{}, Quota{}, fmt.Errorf("mailbox %q: %w", boxes[i].Name, err)
		}

		for j := range msgs {
			counted.Bytes += msgs[j].Size
			counted.Msgs++
		}
	}

	if apply && counted != stored {
		if err := u.SetQuotaUsage(counted); err != nil {
			return counted, stored, err
		}
	}

	return counted, stored, nil
}

// storedMessages returns every stored message of the account, each blob
// once.
func (u *User) storedMessages() ([]Meta, error) {
	boxes, err := u.ListMailboxes()
	if err != nil {
		return nil, err
	}

	seen := map[string]bool{}

	var metas []Meta

	for i := range boxes {
		msgs, err := u.ListMessages(boxes[i].ID)
		if err != nil {
			return nil, fmt.Errorf("mailbox %q: %w", boxes[i].Name, err)
		}

		for j := range msgs {
			if !seen[msgs[j].GUID] {
				seen[msgs[j].GUID] = true
				metas = append(metas, msgs[j])
			}
		}
	}

	return metas, nil
}

// RebuildFTS implements model.FTSRebuilder: the index is deleted and
// every stored message (each blob once) indexed again. A rebuild that
// stops midway is started over.
func (u *User) RebuildFTS(fence func() error, progress func(done, total int)) error {
	metas, err := u.storedMessages()
	if err != nil {
		return err
	}

	if err := fence(); err != nil {
		return err
	}

	if err := u.DeleteFTS(); err != nil {
		return err
	}

	// The version marker goes on only when every message is indexed;
	// until then the rebuild key keeps substring search from trusting
	// the index, also when the rebuild stops midway (see ftsRebuildKey).
	token, err := u.startFTSRebuild()
	if err != nil {
		return fmt.Errorf("index version: %w", err)
	}

	if err := u.reindex(metas, fence, progress); err != nil {
		return err
	}

	if err := u.finishFTSRebuild(token); err != nil {
		return fmt.Errorf("index version: %w", err)
	}

	return nil
}

// reindex indexes the messages in batches, as an import does: see
// ftsIndexDocs.
func (u *User) reindex(metas []Meta, fence func() error, progress func(done, total int)) error {
	var (
		docs []ftsDoc
		size int
	)

	flush := func(done int) error {
		if err := u.ftsIndexDocs(docs); err != nil {
			return fmt.Errorf("index: %w", err)
		}

		docs, size = docs[:0], 0

		progress(done, len(metas))

		return nil
	}

	for i := range metas {
		if err := fence(); err != nil {
			return err
		}

		raw, err := u.readBlob(&metas[i])
		if err != nil {
			return fmt.Errorf("read %s: %w", metas[i].GUID, err)
		}

		docs = append(docs, ftsDoc{guid: metas[i].GUID, raw: raw})
		size += len(raw)

		if len(docs) >= rebuildBatchDocs || size >= rebuildBatchBytes {
			if err := flush(i + 1); err != nil {
				return err
			}
		}
	}

	return flush(len(metas))
}

// A rebuild indexes this many messages, or this many bytes of them, at
// once.
const (
	rebuildBatchDocs  = 200
	rebuildBatchBytes = 8 << 20
)

// convertBatch is how many posting terms ConvertFTSDict collects before
// it writes their dictionary entries.
const convertBatch = 8 * omapPageSize

// LegacyFTSDict implements model.FTSDictConverter.
func (u *User) LegacyFTSDict() (int, error) {
	n := 0

	for _, oid := range u.legacyDictOIDs() {
		if _, err := u.idx.Stat(oid); err == nil {
			n++
		} else if errCode(err) != syscall.ENOENT {
			return n, err
		}
	}

	return n, nil
}

// ConvertFTSDict implements model.FTSDictConverter: the terms are the
// keys of the posting shards, so the dictionary is written from them
// without reading a message, and the objects of the old layout go last.
// Setting a dictionary key that is already there changes nothing, which
// is why this needs no lock against deliveries and may be repeated.
func (u *User) ConvertFTSDict(fence func() error, progress func(done, total int)) (model.FTSDictReport, error) {
	rep := model.FTSDictReport{Terms: map[string]int64{}}

	flush := func(terms []string) error {
		if err := fence(); err != nil {
			return err
		}

		n, err := u.dictApply(terms, true)
		rep.Entries += int64(n)

		return err
	}

	for i := 0; i < u.ftsShards; i++ {
		if err := u.convertShard(fmt.Sprintf("%s%02d", ftsPrefix, i), &rep, flush); err != nil {
			return rep, err
		}

		progress(i+1, u.ftsShards)
	}

	if err := fence(); err != nil {
		return rep, err
	}

	for _, oid := range u.legacyDictOIDs() {
		switch err := u.idx.Delete(oid); {
		case err == nil:
			rep.LegacyObjects++
		case errCode(err) != syscall.ENOENT:
			return rep, err
		}
	}

	return rep, nil
}

// convertShard hands the terms of one posting shard to flush, in batches.
func (u *User) convertShard(oid string, rep *model.FTSDictReport, flush func([]string) error) error {
	var terms []string

	after := ""

	for {
		more, err := listOmapKeys(u.idx, oid, after, "", omapPageSize, func(key string) {
			field, _, _ := strings.Cut(key, ":")
			rep.Terms[field]++

			terms = append(terms, key)
			after = key
		})
		if err != nil {
			return fmt.Errorf("fts shard %s: %w", oid, err)
		}

		if len(terms) >= convertBatch || !more {
			if err := flush(terms); err != nil {
				return err
			}

			terms = terms[:0]
		}

		if !more {
			return nil
		}
	}
}

// PurgeNamespace implements model.NamespacePurger: every object of the
// account's namespaces in the data and index pools is deleted, the
// registry (which holds the maintenance lock) last, so protocols keep
// refusing the account until the very end.
func (u *User) PurgeNamespace(fence func() error) (int, error) {
	removed := 0

	for _, ioctx := range []*rados.IOContext{u.data, u.idx} {
		n, err := purgeObjects(ioctx, fence, oidRegistry)
		removed += n

		if err != nil {
			return removed, err
		}
	}

	if err := fence(); err != nil {
		return removed, err
	}

	if err := u.idx.Delete(oidRegistry); err != nil && errCode(err) != syscall.ENOENT {
		return removed, fmt.Errorf("delete registry: %w", err)
	}

	u.invalidate()

	return removed + 1, nil
}

// purgeObjects deletes every object of the IO context's namespace except
// keep, in one listing, fencing before each batch.
func purgeObjects(ioctx *rados.IOContext, fence func() error, keep string) (int, error) {
	const batch = 100

	it, err := listObjects(ioctx, listPurgeObjects)
	if err != nil {
		return 0, err
	}
	defer it.Close()

	removed := 0

	for it.Next() {
		oid := it.Value()
		if oid == keep {
			continue
		}

		if removed%batch == 0 {
			if err := fence(); err != nil {
				return removed, err
			}
		}

		if err := ioctx.Delete(oid); err != nil && errCode(err) != syscall.ENOENT {
			return removed, fmt.Errorf("delete %s: %w", oid, err)
		}

		removed++
	}

	return removed, it.Err()
}
