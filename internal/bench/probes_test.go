package bench

import (
	"context"
	"errors"
	"math/rand/v2"
	"slices"
	"strings"
	"testing"
	"time"

	"shardpost/internal/delivery"
	"shardpost/internal/memstore"
	"shardpost/internal/model"
)

// TestSearchQueries: the query set is fixed, covers every kind and key,
// frequent and rare words, and cuts parts of 3 to 6 characters; a
// longer set starts with the shorter one.
func TestSearchQueries(t *testing.T) {
	short := SearchQueries(24)

	for _, k := range []int{24, 30, 60, 240} {
		qs := SearchQueries(k)
		if !slices.Equal(qs, SearchQueries(k)) || !slices.Equal(qs[:min(k, len(short))], short[:min(k, len(short))]) {
			t.Errorf("k %d: the queries differ between calls", k)
		}

		checkQuerySet(t, k, qs)
	}
}

func checkQuerySet(t *testing.T, k int, qs []SearchQuery) {
	t.Helper()

	if len(qs) != k {
		t.Fatalf("%d queries, want %d", len(qs), k)
	}

	kinds, keys, rare := map[string]bool{}, map[string]bool{}, 0

	for _, q := range qs {
		kinds[q.Kind], keys[q.Key] = true, true

		if q.Rare {
			rare++
		}

		if q.Kind != QueryWord && (len(q.Text) < 3 || len(q.Text) > 6) {
			t.Errorf("k %d: query %+v: a part of %d characters", k, q, len(q.Text))
		}
	}

	if len(kinds) != 3 || len(keys) != 4 || rare == 0 || rare == len(qs) {
		t.Errorf("k %d: kinds %v, keys %v, rare %d of %d", k, kinds, keys, rare, len(qs))
	}
}

// TestQueryTextShortWords: a word too short for a part becomes a word
// query; a prefix leaves the last character out, an infix the first
// and the last.
func TestQueryTextShortWords(t *testing.T) {
	for _, w := range []string{"ab", "abc", "abcd", "abcde", "abcdefghijk"} {
		for seed := range uint64(20) {
			for _, kind := range []string{QueryPrefix, QueryInfix} {
				got, text := queryText(kind, w, rand.New(rand.NewPCG(seed, 1)))
				if !cutOK(kind, w, got, text) {
					t.Errorf("%s of %q: %s %q", kind, w, got, text)
				}
			}
		}
	}
}

// wordOK: a word query comes only of a word too short for a cut of 3.
func wordOK(kind, w, text string) bool {
	short := 3
	if kind == QueryInfix {
		short = 4
	}

	return text == w && len(w) <= short
}

// cutOK checks what queryText made of a word (whose letters differ).
func cutOK(kind, w, got, text string) bool {
	if got == QueryWord {
		return wordOK(kind, w, text)
	}

	if got != kind || len(text) < 3 || len(text) > 6 || !strings.Contains(w, text) {
		return false
	}

	if kind == QueryPrefix {
		return strings.HasPrefix(w, text) && text != w
	}

	return !strings.HasPrefix(w, text) && !strings.HasSuffix(w, text)
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

// TestIdentifierSearch (temp/13 §7.7): in an account of the identifier
// corpus every identifier query — whole, start, inner — finds the
// message whose reference it took (unfiltered, as today).
func TestIdentifierSearch(t *testing.T) {
	store := memstore.New()
	sizes, _ := ParseSizes("1:1KiB-4KiB")
	o := PopulateOptions{Prefix: "bench-i-", Accounts: 1, Messages: 8, Mailboxes: []string{"INBOX"},
		InboxShare: 1, Gen: Generator{Seed: 5, Sizes: sizes, Corpus: Corpus{IDs: true}}, Parallel: 1}

	if _, err := Populate(context.Background(), o, store, &delivery.Service{Store: store, Log: discard()}); err != nil {
		t.Fatal(err)
	}

	account := AccountName(o.Prefix, 0)

	u, err := store.User(account)
	if err != nil {
		t.Fatal(err)
	}
	defer u.Close()

	for _, q := range SearchQueriesFor(12, o.Gen.Corpus) {
		if q.Msg == 0 && q.Text != "" {
			continue // a word query
		}

		r := ResolveQuery(o.Gen, account, q)

		h, err := Search(u, r)
		if err != nil {
			t.Fatal(err)
		}

		if len(h.Words) == 0 {
			t.Errorf("%s %q found nothing", r.Kind, r.Text)
		}

		for _, c := range h.Candidates {
			if c != nil && len(c) == 0 {
				t.Errorf("%s %q: no candidate", r.Kind, r.Text)
			}
		}
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
