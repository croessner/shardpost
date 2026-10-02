package cephstore

import (
	"fmt"
	"strings"
	"syscall"
	"time"

	"shardpost/internal/model"
)

// Maintenance operations for the admin jobs (roadmap 8c).

var (
	_ model.Scrubber         = (*User)(nil)
	_ model.QuotaRecounter   = (*User)(nil)
	_ model.FTSRebuilder     = (*User)(nil)
	_ model.FTSDictConverter = (*User)(nil)
	_ model.NamespacePurger  = (*User)(nil)
	_ model.ScrubProber      = (*User)(nil)
)

// RunScrub implements model.Scrubber.
func (u *User) RunScrub(req model.ScrubRequest) (any, error) {
	return u.Scrub(ScrubOptions{DryRun: req.DryRun, Repairs: req.Repairs, Grace: req.Grace, Fence: req.Fence})
}

// ScrubState implements model.ScrubProber: the account counter and the
// version of the registry object (two reads; 0 for an absent object).
func (u *User) ScrubState() (model.ScrubState, error) {
	seq, _, err := u.acctBounds()
	if err != nil {
		return model.ScrubState{}, err
	}

	// One key is read along: go-ceph wants a non-empty key list.
	_, ver, err := u.getKeysVer(u.idx, oidRegistry, []string{regImportLock})
	if err != nil {
		return model.ScrubState{}, fmt.Errorf("read registry: %w", err)
	}

	return model.ScrubState{AccountSeq: seq, RegistryVersion: ver}, nil
}

// ScrubPending implements model.ScrubProber: any key of the scrub object
// (orphan candidates "o:", the quota count), an intent in the registry
// (d:, bi:, bimp:, up:, sub:, ci:, dm:; one read op), or a MOVE or removal
// intent on a mailbox (i:, r:; one read op per mailbox, no listing).
func (u *User) ScrubPending() (string, error) {
	switch n, err := u.firstKeys(oidScrub, ""); {
	case err != nil:
		return "", fmt.Errorf("read scrub state: %w", err)
	case n > 0:
		return "scrub candidates", nil
	}

	switch p, err := u.anyKey(oidRegistry, intentPrefixes...); {
	case err != nil:
		return "", fmt.Errorf("read intents: %w", err)
	case p != "":
		return "intents " + p, nil
	}

	for _, prefix := range []string{regUploadPrefix, regSubmissionPrefix} {
		switch expired, err := u.expiredUpload(prefix, time.Now()); {
		case err != nil:
			return "", fmt.Errorf("read %s intents: %w", prefix, err)
		case expired:
			return "intents " + prefix, nil
		}
	}

	boxes, err := u.ListMailboxes()
	if err != nil {
		return "", err
	}

	for i := range boxes {
		switch p, err := u.anyKey(mboxOID(boxes[i].ID), keyIntentPrefix, keyRemovePrefix); {
		case err != nil:
			return "", fmt.Errorf("read intents of %q: %w", boxes[i].Name, err)
		case p != "":
			return "mailbox intents " + p, nil
		}
	}

	return "", nil
}

// firstKeys reports whether an object of the index pool has a key with
// the prefix (it reads at most one; an absent object has none).
func (u *User) firstKeys(oid, prefix string) (int, error) {
	vals, err := u.idx.GetOmapValues(oid, "", prefix, 1)
	if err != nil {
		if errCode(err) == syscall.ENOENT {
			return 0, nil
		}

		return 0, err
	}

	return len(vals), nil
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
	token, err := u.startFTSRebuild(len(metas))
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
// is why this needs no lock against deliveries and may be repeated. A
// layout 3 index has no layout 2 dictionary to write: nothing is done
// (a conversion to layout 3 derives the dictionary itself).
func (u *User) ConvertFTSDict(fence func() error, progress func(done, total int)) (model.FTSDictReport, error) {
	rep := model.FTSDictReport{Terms: map[string]int64{}}

	switch lay, err := u.readLayout(); {
	case err != nil:
		return rep, err
	case lay != nil && lay.F == model.FTSLayout3:
		progress(1, 1)
		return rep, nil
	}

	flush := func(terms []string) error {
		if err := fence(); err != nil {
			return err
		}

		n, err := u.dictApplyAt(layout2Loc(u.ftsShards), terms, true)
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
			after = key
			if isMarker(key) {
				return
			}

			field, _, _ := strings.Cut(key, ":")
			rep.Terms[field]++

			terms = append(terms, key)
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
