package cephbench

import (
	"context"
	"fmt"
	"slices"

	"shardpost/internal/bench"
	"shardpost/internal/cephstore"
)

// The search and expunge probes of measure. Like the other rounds they
// run once without and once with the objecter log; every call opens a
// view of its own, as a new IMAP session would, so the view's first
// check of the full-text index is part of a search.

// search runs the queries of bench.SearchQueries(k) in every probe
// account and compares the results of the two rounds.
func (m *measurer) search(ctx context.Context, k int) (*bench.SearchReport, error) {
	queries := bench.SearchQueries(k)
	plain, traced := bench.NewSearchResults(queries), bench.NewSearchResults(queries)

	query := func(s *cephstore.Store, account string, i int) error {
		u, err := s.User(account)
		if err != nil {
			return err
		}
		defer u.Close()

		h, err := bench.Search(u, queries[i])
		if err != nil {
			return fmt.Errorf("query %d %+v: %w", i, queries[i], err)
		}

		if s == m.o.Store {
			plain.Add(i, account, h)
		} else {
			traced.Add(i, account, h)
		}

		return nil
	}

	lat, ops, n, _, err := m.round(ctx, k, 1, query)
	if err != nil {
		return nil, fmt.Errorf("search probe: %w", err)
	}

	r := &bench.SearchReport{RoundReport: bench.RoundReport{What: "search query (IMAP substring and word search)",
		Accounts: len(m.accounts), Count: n, Latency: lat.Summary()}}
	r.Ops, r.Total = bench.Rates(ops, n)
	r.Queries = plain.Results()
	r.Fingerprint = bench.Fingerprint(r.Queries)

	if !slices.Equal(fingerprints(r.Queries), fingerprints(traced.Results())) {
		r.Mismatch = true
	}

	return r, nil
}

func fingerprints(rs []bench.SearchQueryResult) []string {
	out := make([]string, 0, len(rs))
	for _, r := range rs {
		out = append(out, r.Fingerprint)
	}

	return out
}

// expunge expunges the oldest INBOX message of every probe account e
// times per round.
func (m *measurer) expunge(ctx context.Context, e int) (*bench.RoundReport, error) {
	expunge := func(s *cephstore.Store, account string, _ int) error {
		u, err := s.User(account)
		if err != nil {
			return err
		}
		defer u.Close()

		return bench.ExpungeOldest(u, "INBOX")
	}

	cas0 := bench.CASRetries()

	lat, ops, n, _, err := m.round(ctx, e, 1, expunge)
	if err != nil {
		return nil, fmt.Errorf("expunge probe: %w", err)
	}

	r := &bench.RoundReport{What: "EXPUNGE of the oldest INBOX message", Accounts: len(m.accounts), Count: n,
		Latency: lat.Summary(), CASRetries: bench.CASDelta(cas0, bench.CASRetries(), 2*n)}
	r.Ops, r.Total = bench.Rates(ops, n)

	return r, nil
}
