//go:build tentacle

package cephstore

import (
	"sort"
	"testing"

	"shardpost/internal/model"
)

// TestTrimPlanOnStoreKeys: the trims plan on the keys the store writes.
// A fresh mailbox (h:modseq 1, h:logstart 1, no c:1) is trimmed exactly
// where the count-based rule trimmed it, the account log from its first
// sequence on; the vanished keys follow the change keys they belong to.
func TestTrimPlanOnStoreKeys(t *testing.T) {
	for _, c := range []struct {
		name            string
		key             func(uint64) string
		from, head      uint64
		logStart        uint64
		retention       int
		start           uint64
		drop            int
		first, lastDrop uint64
	}{
		// LogRetention records (2..LogRetention+1): nothing to do yet.
		{"mailbox at the retention", chgKey, 2, LogRetention + 1, 1, LogRetention, 0, 0, 0, 0},
		{"mailbox one over", chgKey, 2, LogRetention + 2, 1, LogRetention, 3, 1, 2, 2},
		{"account log", acctEntryKey, 1, DefaultAccountLogRetention + 5, 0, DefaultAccountLogRetention, 6, 5, 1, 5},
	} {
		var keys []string
		for s := c.from; s <= c.head; s++ {
			keys = append(keys, c.key(s))
		}

		start, drop, err := model.PlanLogTrim(c.head, c.logStart, c.retention, omapPageSize, c.key, keyReader(keys))
		if err != nil || start != c.start || len(drop) != c.drop {
			t.Fatalf("%s: start %d, %d keys, %v", c.name, start, len(drop), err)
		}

		if c.drop > 0 && (drop[0] != c.key(c.first) || drop[len(drop)-1] != c.key(c.lastDrop)) {
			t.Fatalf("%s: drop %v", c.name, drop)
		}
	}
}

// keyReader serves keys page by page, as listOmapKeys does.
func keyReader(keys []string) model.LogPageReader {
	sort.Strings(keys)

	return func(after string, limit int) ([]string, error) {
		i := sort.SearchStrings(keys, after)
		if i < len(keys) && keys[i] == after {
			i++
		}

		return keys[i:min(i+limit, len(keys))], nil
	}
}

// TestLimitChecksWithoutReads: the checks that need no RADOS read do
// none (the user view here has no IOContext).
func TestLimitChecksWithoutReads(t *testing.T) {
	u := &User{limits: model.Limits{Mailboxes: 1, MailboxNames: 1, KeywordsPerMailbox: 3, MetadataEntries: 1, SharedPointers: 1}}

	// INBOX is never refused, whatever the limits say; the check does not
	// even read the registry for it.
	if err := u.checkMailboxLimits(inboxName, false); err != nil {
		t.Fatal(err)
	}

	// Server-managed METADATA entries are never refused.
	key := "p:alice:-:" + model.ServerManagedPrefix + "x"
	if err := u.checkMetadataRoom(map[string][]byte{key: nil}, nil, map[string]string{key: model.ServerManagedPrefix + "x"}); err != nil {
		t.Fatal(err)
	}

	// No grant without new rights, to "anyone", or from a store-less view.
	if err := u.checkSharedRoom("Box", "bob", func(string) string { return "" }); err != nil {
		t.Fatal(err)
	}

	if err := u.checkSharedRoom("Box", identAnyone, func(string) string { return "lr" }); err != nil {
		t.Fatal(err)
	}

	// Without a keyword limit no header is read.
	u.limits.KeywordsPerMailbox = 0
	if err := u.checkKeywordRoom(nil, []Meta{{Flags: []string{"$x"}}}); err != nil {
		t.Fatal(err)
	}
}
