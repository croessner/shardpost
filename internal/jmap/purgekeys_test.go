package jmap

import (
	"slices"
	"testing"

	"shardpost/internal/model"
)

// TestAccountRecordKeys: the admin store records JMAP keeps per account
// are the ones a purge deletes (model.AccountRecordKeys).
func TestAccountRecordKeys(t *testing.T) {
	for _, account := range []string{"alice@example.org", "odd/name+tag@example.org"} {
		want := model.AccountRecordKeys(account)

		for _, key := range []string{pushAcctKey(account), pushSeenKey(account), submissionKey(account)} {
			if !slices.Contains(want, key) {
				t.Errorf("%s is not among the keys a purge deletes: %v", key, want)
			}
		}

		// jmap's three and the record of the fts_due worker
		if len(want) != 4 || !slices.Contains(want, model.FTSDueRecordKey(account)) {
			t.Errorf("the purge deletes %v, want jmap's 3 and the fts_due record", want)
		}
	}
}
