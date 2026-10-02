package bench

import (
	"context"
	"errors"
	"testing"
	"time"

	"shardpost/internal/delivery"
	"shardpost/internal/memstore"
	"shardpost/internal/model"
)

// TestSearchQueries: the query set is fixed, covers every kind and key,
// frequent and rare words, and cuts parts of 3 to 6 characters.
func TestSearchQueries(t *testing.T) {
	qs := SearchQueries(24)
	if len(qs) != 24 {
		t.Fatalf("%d queries", len(qs))
	}

	again := SearchQueries(24)
	kinds, keys, rare := map[string]bool{}, map[string]bool{}, 0

	for i, q := range qs {
		if q != again[i] {
			t.Errorf("query %d differs between calls: %+v %+v", i, q, again[i])
		}

		kinds[q.Kind], keys[q.Key] = true, true

		if q.Rare {
			rare++
		}

		if q.Kind != QueryWord && (len(q.Text) < 3 || len(q.Text) > 6) {
			t.Errorf("query %+v: a part of %d characters", q, len(q.Text))
		}
	}

	if len(kinds) != 3 || len(keys) != 4 || rare == 0 || rare == len(qs) {
		t.Errorf("kinds %v, keys %v, rare %d of %d", kinds, keys, rare, len(qs))
	}
}

// searchAll runs the queries in the accounts and returns the results.
func searchAll(t *testing.T, store *memstore.Store, accounts []string, qs []SearchQuery) []SearchQueryResult {
	t.Helper()

	res := NewSearchResults(qs)

	for _, a := range accounts {
		u, err := store.User(a)
		if err != nil {
			t.Fatal(err)
		}

		for i, q := range qs {
			h, err := Search(u, q)
			if err != nil {
				t.Fatal(err)
			}

			res.Add(i, a, h)
		}

		u.Close()
	}

	return res.Results()
}

// TestSearchAndExpungeProbes: the search finds the generator's frequent
// words, gives the same fingerprint on unchanged accounts and another
// after a delivery; ExpungeOldest removes the lowest UID.
func TestSearchAndExpungeProbes(t *testing.T) {
	store := memstore.New()
	sizes, _ := ParseSizes("1:1KiB-4KiB")
	o := PopulateOptions{Prefix: "bench-s-", Accounts: 2, Messages: 10, Mailboxes: []string{"INBOX"},
		InboxShare: 1, Gen: Generator{Seed: 5, Sizes: sizes}, Parallel: 1}

	if _, err := Populate(context.Background(), o, store, &delivery.Service{Store: store, Log: discard()}); err != nil {
		t.Fatal(err)
	}

	accounts := []string{AccountName(o.Prefix, 0), AccountName(o.Prefix, 1)}
	qs := SearchQueries(12)

	first := searchAll(t, store, accounts, qs)
	if Fingerprint(first) != Fingerprint(searchAll(t, store, accounts, qs)) {
		t.Fatal("two searches of unchanged accounts differ")
	}

	found := 0

	for _, r := range first {
		if r.Words > 0 {
			found++
		}
	}

	if found == 0 {
		t.Fatalf("no query found anything: %+v", first)
	}

	u, err := store.User(accounts[0])
	if err != nil {
		t.Fatal(err)
	}

	before := inboxUIDs(t, u)
	if err := ExpungeOldest(u, "INBOX"); err != nil {
		t.Fatal(err)
	}

	after := inboxUIDs(t, u)
	if len(after) != len(before)-1 || after[0] != before[1] {
		t.Errorf("UIDs %v after the expunge, before %v", after, before)
	}

	// The memory store keeps the index of expunged messages (the
	// protocol layer checks candidates); a new message changes it.
	raw, _ := o.Gen.Message(accounts[0], 99)
	if _, err := u.Deliver("INBOX", raw, nil, time.Time{}); err != nil {
		t.Fatal(err)
	}

	u.Close()

	if Fingerprint(first) == Fingerprint(searchAll(t, store, accounts, qs)) {
		t.Error("the search found the same after a delivery")
	}
}

func inboxUIDs(t *testing.T, u model.User) []uint32 {
	t.Helper()

	mb, err := u.Lookup("INBOX")
	if err != nil {
		t.Fatal(err)
	}

	metas, err := u.ListMessages(mb.ID)
	if err != nil {
		t.Fatal(err)
	}

	out := make([]uint32, 0, len(metas))
	for _, m := range metas {
		out = append(out, m.UID)
	}

	return out
}

// TestExpungeEmptyMailbox: an empty mailbox is an error the probe
// reports.
func TestExpungeEmptyMailbox(t *testing.T) {
	store := memstore.New()

	u, err := store.User("bench-e-000000")
	if err != nil {
		t.Fatal(err)
	}
	defer u.Close()

	if err := u.CreateMailbox("INBOX", nil); err != nil {
		t.Fatal(err)
	}

	if err := ExpungeOldest(u, "INBOX"); !errors.Is(err, ErrNothingToExpunge) {
		t.Fatalf("got %v, want ErrNothingToExpunge", err)
	}
}
