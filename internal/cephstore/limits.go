package cephstore

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"syscall"

	"github.com/ceph/go-ceph/rados"

	"shardpost/internal/model"
)

// Per-account limits (model.Limits, configuration section "limits").
//
// The keyword limit is checked inside the header CAS of the mailbox, which
// reads the keyword list anyway, so it holds exactly. The other limits
// count omap keys, which would make a CAS that holds them read a page of
// keys per attempt; they are checked before the write instead and are
// advisory: writers that race past the check together can exceed a limit
// by one entry each. That is good enough for what the limits are for —
// keeping a runaway account's objects far below the size Ceph warns
// about — and none of them is a promise to a client.

// countOmap counts the keys of oid under prefix whose value keep accepts
// (nil: every key), reading pages until it has counted limit of them
// or reached the end. A missing object has no keys.
func countOmap(ioctx *rados.IOContext, oid, prefix string, limit int, keep func(value []byte) bool) (int, error) {
	n := 0
	after := ""

	for n < limit {
		want := omapPageSize
		if keep == nil {
			want = min(want, limit-n)
		}

		got := 0

		err := ioctx.ListOmapValues(oid, after, prefix, int64(want), func(key string, value []byte) {
			got++
			after = key

			if keep == nil || keep(value) {
				n++
			}
		})
		if err != nil {
			if errCode(err) == syscall.ENOENT {
				return 0, nil
			}

			return 0, err
		}

		if got < want {
			break
		}
	}

	return n, nil
}

// checkMailboxLimits refuses the creation of a mailbox named name when
// the account is at its mailbox limit, or at its name limit and name was
// never used (known: the registry has a UIDVALIDITY record for it).
// INBOX is never refused: an account must always be able to receive.
func (u *User) checkMailboxLimits(name string, known bool) error {
	if name == inboxName {
		return nil
	}

	if limit := u.limits.Mailboxes; limit > 0 {
		n, err := countOmap(u.idx, oidRegistry, regMboxPrefix, limit, nil)
		if err != nil {
			return fmt.Errorf("count mailboxes: %w", err)
		}

		if n >= limit {
			return model.LimitReached(model.LimitMailboxes, limit)
		}
	}

	if limit := u.limits.MailboxNames; limit > 0 && !known {
		n, err := countOmap(u.idx, oidRegistry, regLastValidity, limit, nil)
		if err != nil {
			return fmt.Errorf("count mailbox names: %w", err)
		}

		if n >= limit {
			return model.LimitReached(model.LimitMailboxNames, limit)
		}
	}

	return nil
}

// checkKeywordRoom refuses a COPY or MOVE of metas into mb that would add
// more keywords than the mailbox may know. It runs before anything is
// written: a MOVE that started is completed, a COPY that fails in the
// middle keeps what it copied. The link of every message checks the limit
// again in its header CAS (COPY), so a race costs at most one MOVE's
// keywords.
func (u *User) checkKeywordRoom(mb *Mailbox, metas []Meta) error {
	limit := u.limits.KeywordsPerMailbox
	if limit <= 0 {
		return nil
	}

	var flags []string
	for i := range metas {
		flags = append(flags, metas[i].Flags...)
	}

	info, _, err := u.readHeader(mb)
	if err != nil {
		return err
	}

	return model.CheckKeywords(info.Keywords, flags, limit)
}

// checkMetadataRoom refuses a METADATA change that adds entries (set
// keys that do not exist yet; rm keys are removed by the same change)
// beyond the account's limit. Entries the server keeps for itself are
// never refused: entries names the entry of each key in set.
func (u *User) checkMetadataRoom(set map[string][]byte, rm []string, entries map[string]string) error {
	limit := u.limits.MetadataEntries
	if limit <= 0 || !addsUserEntry(set, nil, entries) {
		return nil
	}

	keys := slices.AppendSeq(slices.Clone(rm), maps.Keys(set))

	have, err := getOmapKeys(u.idx, oidMetadata, keys)
	if err != nil {
		return fmt.Errorf("metadata: %w", err)
	}

	if !addsUserEntry(set, have, entries) {
		return nil
	}

	added := missingKeys(slices.Collect(maps.Keys(set)), have)
	removed := len(rm) - missingKeys(rm, have)

	n, err := countOmap(u.idx, oidMetadata, "", limit+removed, nil)
	if err != nil {
		return fmt.Errorf("count metadata: %w", err)
	}

	if n-removed+added > limit {
		return model.LimitReached(model.LimitMetadata, limit)
	}

	return nil
}

// addsUserEntry reports whether set holds a key that is not in have
// (nil: none is known yet) for an entry the server does not keep for
// itself.
func addsUserEntry(set, have map[string][]byte, entries map[string]string) bool {
	for k := range set {
		if _, ok := have[k]; !ok && !model.ServerManagedEntry(entries[k]) {
			return true
		}
	}

	return false
}

// missingKeys counts the keys that are not in have.
func missingKeys(keys []string, have map[string][]byte) int {
	n := 0

	for _, k := range keys {
		if _, ok := have[k]; !ok {
			n++
		}
	}

	return n
}

// checkSharedRoom refuses a grant on the mailbox name that would give
// identifier a pointer more than it may hold. It runs before the ACL is
// written, so that a refused grant leaves no right without its pointer.
// grant computes the new rights from the old ones; a change that keeps
// or removes rights, or one to a grantee that already has a pointer to
// this mailbox, needs no room. Failures other than the limit are left
// to the change itself.
func (u *User) checkSharedRoom(name, identifier string, grant func(old string) string) error {
	limit := u.limits.SharedPointers
	if limit <= 0 || identifier == identAnyone || model.IsPublic(u.name) || u.store == nil {
		return nil
	}

	mb, err := u.Lookup(name)
	if err != nil {
		return nil
	}

	if old := mb.ACL[identifier]; old != "" || grant(old) == "" {
		return nil
	}

	err = u.withGrantee(identifier, func(g *User) error { return g.sharedRoom(sharedKey(u.name, name), limit) })
	if errors.Is(err, model.ErrLimit) {
		return err
	}

	return nil
}

// sharedRoom refuses one pointer more than limit unless key is one of
// them already. Tombstones of revoked pointers do not count.
func (u *User) sharedRoom(key string, limit int) error {
	live := func(v []byte) bool {
		var rec sharedRec
		return json.Unmarshal(v, &rec) == nil && !rec.Deleted
	}

	vals, err := getOmapKeys(u.idx, oidRegistry, []string{key})
	if err != nil {
		return err
	}

	if v, ok := vals[key]; ok && live(v) {
		return nil
	}

	n, err := countOmap(u.idx, oidRegistry, regSharedPrefix, limit, live)
	if err != nil {
		return err
	}

	if n >= limit {
		return model.LimitReached(model.LimitSharedPointers, limit)
	}

	return nil
}
