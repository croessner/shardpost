package cephbench

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"time"

	"shardpost/internal/bench"
	"shardpost/internal/cephstore"
	"shardpost/internal/model"
)

// The full-text layout in the bench (stage B14b): populate and measure
// with --fts-layout 3 set the layout 3 mark in the bench pools (their
// own admin namespace), and measure --convert converts the probe
// accounts as a fts_layout job would.

// MarkLayout3 sets the layout 3 mark in the admin namespace of the
// store's pools (the bench pools; the guard has checked them).
func MarkLayout3(s *cephstore.Store) error {
	kv, err := s.AdminKV()
	if err != nil {
		return err
	}
	defer kv.Close()

	mark, err := json.Marshal(model.FTSLayoutMark{EnabledAt: time.Now().UTC(), Principal: benchPrincipal, Pods: 1, Forced: true})
	if err != nil {
		return err
	}

	return kv.Update(model.FTSLayoutMarkKey, func([]byte) ([]byte, error) { return mark, nil })
}

// benchPrincipal is who the bench records in what it marks: the layout 3
// mark and its system pool init.
const benchPrincipal = "bench"

// convertSession is the lock session of the bench's conversions.
const convertSession = "benchconvert0000"

// convertAccount converts (or, already in layout 3, reshards) one
// account under its maintenance lock, as the job does: lock, convert,
// unlock. The latency of the call is the time the account is locked.
func convertAccount(s *cephstore.Store, account string, unit int) error {
	u, err := s.User(account)
	if err != nil {
		return err
	}
	defer u.Close()

	if _, err := u.ImportLock(convertSession, 10*time.Minute); err != nil {
		return err
	}

	defer func() { _ = u.ImportUnlock(convertSession) }()

	tok := fmt.Sprintf("%s/%d-%d", convertSession, time.Now().UnixNano(), unit)

	rep, err := u.ConvertFTSLayout(model.FTSLayoutRequest{Layout: model.FTSLayout3, Tok: tok})
	if err != nil {
		_, _ = u.ClearFTSLayoutNext(tok)
		return err
	}

	if rep.To.Layout != model.FTSLayout3 {
		return fmt.Errorf("conversion of %s ended in layout %d", account, rep.To.Layout)
	}

	return nil
}

// probeConvert converts the probe accounts (first round: 2→3, its
// latency the lock time; second, counted round: a reshard) and repeats
// the search probe after it, comparing the fingerprints.
func (m *measurer) probeConvert(ctx context.Context, r *bench.MeasureReport) error {
	if !m.o.Convert {
		return nil
	}

	cas0 := bench.CASRetries()

	lat, ops, n, _, err := m.round(ctx, 1, 1, convertAccount)
	if err != nil {
		return fmt.Errorf("convert probe: %w", err)
	}

	c := &bench.RoundReport{What: "fts_layout conversion to layout 3 (latency: first round 2→3, the lock time; ops: second round, a reshard)",
		Accounts: len(m.accounts), Count: n, Latency: lat.Summary(), CASRetries: bench.CASDelta(cas0, bench.CASRetries(), 2*n)}
	c.Ops, c.Total = bench.Rates(ops, n)
	r.Convert = c

	if m.o.Search <= 0 {
		return nil
	}

	after, err := m.search(ctx, m.o.Search)
	if err != nil {
		return err
	}

	r.SearchAfter = after

	if r.Search != nil && !slices.Equal(fingerprints(r.Search.Queries), fingerprints(after.Queries)) {
		r.Notes = append(r.Notes, "the search after the conversion found other messages than before it")
	}

	return nil
}
