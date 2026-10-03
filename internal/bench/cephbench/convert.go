package cephbench

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"sync"
	"sync/atomic"
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
func MarkLayout3(s *cephstore.Store) error { return setMark(s, model.FTSLayoutMarkKey) }

// setMark sets a cluster mark in the admin namespace of the store's
// pools (the bench pools).
func setMark(s *cephstore.Store, key string) error {
	kv, err := s.AdminKV()
	if err != nil {
		return err
	}
	defer kv.Close()

	mark, err := json.Marshal(model.FTSLayoutMark{EnabledAt: time.Now().UTC(), Principal: benchPrincipal, Pods: 1, Forced: true})
	if err != nil {
		return err
	}

	return kv.Update(key, func([]byte) ([]byte, error) { return mark, nil })
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

// MarkOnline sets the mark of online runs (stage B14i) in the admin
// namespace of the store's pools (the bench pools).
func MarkOnline(s *cephstore.Store) error { return setMark(s, model.FTSOnlineMarkKey) }

// probeOnline reshards every probe account online, one after the
// other, while the deliverer puts m.o.Load messages a second into the
// account (measure --convert --online --load r): the run's latency, its
// catch-up rounds and hold, and the latency and failures of the
// deliveries (there should be none).
func (m *measurer) probeOnline(ctx context.Context, r *bench.MeasureReport) error {
	if err := MarkOnline(m.o.Store); err != nil {
		return fmt.Errorf("online mark: %w", err)
	}

	rep := &bench.OnlineReport{Accounts: len(m.accounts), Load: m.o.Load}
	runs := &bench.Latency{}
	l := &onlineLoad{m: m, lat: &bench.Latency{}}

	for unit, account := range m.accounts {
		next, err := messageCount(m.o.Store, account)
		if err != nil {
			return fmt.Errorf("online probe: account %s: %w", account, err)
		}

		stop := l.start(ctx, account, next)
		start := time.Now()
		run, err := onlineReshard(m.o.Store, account, unit)

		stop()

		if err != nil {
			return fmt.Errorf("online probe: account %s: %w", account, err)
		}

		runs.Add(time.Since(start))

		rep.Rounds += run.Rounds
		rep.CaughtUp += run.CaughtUp
		rep.HoldMS += run.HoldMS
	}

	rep.Run, rep.Delivery = runs.Summary(), l.lat.Summary()
	rep.Deliveries, rep.Failures = l.delivered.Load(), l.failures.Load()
	r.ConvertOnline = rep

	if rep.Failures > 0 {
		r.Notes = append(r.Notes, fmt.Sprintf("%d deliveries failed during the online reshards", rep.Failures))
	}

	return nil
}

// onlineLoad delivers into the account of an online run while it runs.
type onlineLoad struct {
	m                   *measurer
	lat                 *bench.Latency
	delivered, failures atomic.Int64
}

// start delivers m.o.Load messages a second into the account, numbered
// from next on, until the returned function is called (it waits for the
// last delivery).
func (l *onlineLoad) start(ctx context.Context, account string, next int) func() {
	o := l.m.o
	if o.Load <= 0 {
		return func() {}
	}

	stop := make(chan struct{})

	var wg sync.WaitGroup

	wg.Go(func() {
		tick := time.NewTicker(time.Second / time.Duration(o.Load))
		defer tick.Stop()

		for n := next; ; n++ {
			select {
			case <-stop:
				return
			case <-ctx.Done():
				return
			case <-tick.C:
			}

			raw, _ := o.Gen.Message(account, n)
			start := time.Now()

			if _, err := o.NewDeliverer(o.Store).Deliver(ctx, bench.Request(account, raw, "")); err != nil {
				l.failures.Add(1)
				o.Log.Warn("bench: delivery during an online run failed", "account", account, "err", err)

				continue
			}

			l.lat.Add(time.Since(start))
			l.delivered.Add(1)
		}
	})

	return func() {
		close(stop)
		wg.Wait()
	}
}

// onlineReshard reshards one account online, as an fts_layout job
// without the lock does.
func onlineReshard(s *cephstore.Store, account string, unit int) (model.FTSLayoutReport, error) {
	u, err := s.User(account)
	if err != nil {
		return model.FTSLayoutReport{}, err
	}
	defer u.Close()

	tok := fmt.Sprintf("%s/online-%d-%d", convertSession, time.Now().UnixNano(), unit)

	rep, err := u.ConvertFTSLayout(model.FTSLayoutRequest{Layout: model.FTSLayout3, Tok: tok, Online: true})
	if err != nil {
		_, _ = u.ClearFTSLayoutNext(tok)
		return rep, err
	}

	if rep.Deferred != "" || rep.Unsupported {
		return rep, fmt.Errorf("online reshard of %s did not run: %s %s", account, rep.Deferred, rep.Reason)
	}

	return rep, nil
}

// probeConvert converts the probe accounts (first round: 2→3, its
// latency the lock time; second, counted round: a reshard) and repeats
// the search probe after it, comparing the fingerprints. With --online
// the online reshards follow (probeOnline).
func (m *measurer) probeConvert(ctx context.Context, r *bench.MeasureReport) error {
	if !m.o.Convert {
		return nil
	}

	if err := m.probeOffline(ctx, r); err != nil || !m.o.Online {
		return err
	}

	return m.probeOnline(ctx, r)
}

// probeOffline is the offline part of probeConvert.
func (m *measurer) probeOffline(ctx context.Context, r *bench.MeasureReport) error {
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
