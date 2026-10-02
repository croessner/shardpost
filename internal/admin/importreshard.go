package admin

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net/http"
	"sync"
	"time"

	"shardpost/internal/adminapi"
	"shardpost/internal/jobs"
	"shardpost/internal/model"
)

// The import reshard (stage B14b, §4.6): between import batches the
// importer asks this endpoint whether the account's layout 3 index has
// outgrown its objects; if so the endpoint reshards it under the import
// session — a fts_layout job would wait for the import lock until the
// import ends. The run's token is "<session>/<nonce>"; it renews the
// import lock and its heartbeat in the layout record every 30 s, waits
// jobs.settle_seconds before its first change, and works synchronously
// up to fts.import_reshard_sync_seconds before the request answers 202.
//
// The runs of this server are kept per account (the process map): the
// check and the insert of a run happen under one mutex, so concurrent
// POSTs start one run. A finished run's entry goes when the session
// closes, at the latest after twice the lock TTL. migrate talks to the
// same server through the import socket (pod affinity); on another
// server GET falls back to the layout record and POST clears a foreign
// run only after its heartbeat aged.

// Reshard states of the API.
const (
	reshardIdle    = "idle"
	reshardRunning = "running"
	reshardDone    = "done"
	reshardFailed  = "failed"
)

// reshardBeat is the heartbeat of a run (the freshness is three beats,
// jobs.ImportReshardFresh); a variable for the tests.
var reshardBeat = 30 * time.Second

// reshardRun is one run of the process map.
type reshardRun struct {
	session, tok string
	done         chan struct{}

	mu       sync.Mutex
	state    string
	err      string
	finished time.Time
}

func (r *reshardRun) result() (string, string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	return r.state, r.err
}

func (r *reshardRun) finish(err error) {
	r.mu.Lock()
	r.state, r.finished = reshardDone, time.Now()

	if err != nil {
		r.state, r.err = reshardFailed, err.Error()
	}
	r.mu.Unlock()

	close(r.done)
}

// reshards is the process map and the background runs of a Server.
type reshards struct {
	mu    sync.Mutex
	runs  map[string]*reshardRun // by account
	hints map[string]int         // import hints by account and session

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// expire drops the finished runs older than ttl (caller holds mu).
func (rs *reshards) expire(ttl time.Duration, now time.Time) {
	for account, run := range rs.runs {
		if state, _ := run.result(); state != reshardRunning && now.Sub(run.finished) > ttl {
			delete(rs.runs, account)
		}
	}
}

// forgetSession drops the finished runs and the hint of a session that
// closed.
func (rs *reshards) forgetSession(account, session string) {
	rs.mu.Lock()
	defer rs.mu.Unlock()

	if run := rs.runs[account]; run != nil && run.session == session {
		if state, _ := run.result(); state != reshardRunning {
			delete(rs.runs, account)
		}
	}

	delete(rs.hints, account+"\x00"+session)
}

// running returns the active run of the session on the account, if any.
func (rs *reshards) running(account, session string) *reshardRun {
	rs.mu.Lock()
	defer rs.mu.Unlock()

	run := rs.runs[account]
	if run == nil || run.session != session {
		return nil
	}

	if state, _ := run.result(); state != reshardRunning {
		return nil
	}

	return run
}

// Close stops the background runs and waits for them (before the store
// closes): they end on their error path, which clears their conversion.
func (s *Server) Close() {
	s.rs.cancel()
	s.rs.wg.Wait()
}

// StartImportReshard checks the account's index and reshards it.
func (s *Server) StartImportReshard(w http.ResponseWriter, r *http.Request, account adminapi.Account, p adminapi.StartImportReshardParams) {
	c, ok := s.beginImport(w, r, account, p.XImportSession)
	if !ok {
		return
	}
	defer c.u.Close()

	lay, ok := c.u.(model.FTSLayouter)
	if !ok {
		writeError(w, http.StatusInternalServerError, adminapi.Internal, "the store has no full-text layouts")
		return
	}

	run, answer, err := s.startReshard(account, p.XImportSession, lay)
	if err != nil {
		s.reshardError(w, r, err)
		return
	}

	if run == nil {
		writeJSON(w, http.StatusOK, answer)
		return
	}

	auditf(r, "reshard", run.tok)

	t := time.NewTimer(s.o.ImportReshardSync)
	defer t.Stop()

	select {
	case <-run.done:
	case <-t.C:
	case <-r.Context().Done():
	}

	state, msg := run.result()
	g := currentGen(lay)

	if state == reshardRunning {
		writeJSON(w, http.StatusAccepted, adminapi.ImportReshard{State: state, G: g})
		return
	}

	writeJSON(w, http.StatusOK, adminapi.ImportReshard{State: state, G: g, Error: optional(msg)})
}

// errReshardConflict: another run of another session works on the
// account.
var errReshardConflict = errors.New("another reshard of the account's full-text index is running")

func (s *Server) reshardError(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, errReshardConflict) {
		writeError(w, http.StatusConflict, adminapi.Conflict, err.Error())
		return
	}

	s.importError(w, r, err)
}

// startReshard decides under the process map's mutex (R5.2): the
// session's run is already active (202, nothing new), another session's
// run has a fresh heartbeat (409), the counts ask for nothing (idle), or
// a new run starts. A stale foreign run is cleared first.
func (s *Server) startReshard(account, session string, lay model.FTSLayouter) (*reshardRun, adminapi.ImportReshard, error) {
	s.rs.mu.Lock()
	defer s.rs.mu.Unlock()

	now := time.Now()
	s.rs.expire(2*s.o.ImportLockTTL, now)

	if run := s.rs.runs[account]; run != nil && run.session == session {
		if state, _ := run.result(); state == reshardRunning {
			return run, adminapi.ImportReshard{}, nil
		}
	}

	st, err := lay.FTSLayout()
	if err != nil {
		return nil, adminapi.ImportReshard{}, err
	}

	if st.NextTok != "" {
		if !st.NextAt.IsZero() && now.Sub(st.NextAt) < jobs.ImportReshardFresh {
			return nil, adminapi.ImportReshard{}, errReshardConflict
		}

		if _, err := lay.ClearFTSLayoutNext(st.NextTok); err != nil {
			return nil, adminapi.ImportReshard{}, err
		}
	}

	_, due, err := lay.FTSLayoutDue()
	if err != nil || !due {
		return nil, adminapi.ImportReshard{State: reshardIdle, G: st.Current.Gen}, err
	}

	run := &reshardRun{session: session, tok: session + "/" + nonce(), done: make(chan struct{}), state: reshardRunning}
	s.rs.runs[account] = run
	s.rs.wg.Add(1)

	go s.runReshard(account, run)

	return run, adminapi.ImportReshard{}, nil
}

// nonce is a fresh nonce of a run's token.
func nonce() string {
	var b [6]byte

	_, _ = rand.Read(b[:])

	return hex.EncodeToString(b[:])
}

// currentGen is the current generation of an index ("" for layout 2 or
// when it cannot be read).
func currentGen(lay model.FTSLayouter) string {
	st, err := lay.FTSLayout()
	if err != nil {
		return ""
	}

	return st.Current.Gen
}

// runReshard is the background run: its own view of the account (the
// request's is closed when the request ends), the server's context (it
// ends at shutdown), the heartbeat, the settle time, the conversion, and
// on any error the clean-up of its conversion.
func (s *Server) runReshard(account string, run *reshardRun) {
	defer s.rs.wg.Done()

	var err error

	defer func() { run.finish(err) }()

	u, err := s.o.Import.User(account)
	if err != nil {
		return
	}
	defer u.Close()

	imp, ok1 := u.(model.Importer)
	lay, ok2 := u.(model.FTSLayouter)

	if !ok1 || !ok2 {
		err = errors.New("the store cannot reshard during an import")
		return
	}

	ctx, cancel := context.WithCancel(s.rs.ctx)
	defer cancel()

	hb := &reshardHeartbeat{imp: imp, lay: lay, session: run.session, tok: run.tok, ttl: s.o.ImportLockTTL, cancel: cancel}
	go hb.run(ctx)

	// Fence of the run (R5.6): its context is alive and the last renewal
	// of the import lock succeeded.
	fence := func() error {
		if err := hb.failed(); err != nil {
			return err
		}

		return ctx.Err()
	}

	err = s.reshardRun(ctx, lay, run.tok, fence)
	if err != nil {
		if _, cerr := lay.ClearFTSLayoutNext(run.tok); cerr != nil {
			s.o.Log.Warn("import reshard: conversion not cleared", "account", account, "err", cerr)
		}
	}
}

// reshardRun waits the settle time and converts.
func (s *Server) reshardRun(ctx context.Context, lay model.FTSLayouter, tok string, fence func() error) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(s.o.ImportReshardSettle):
	}

	_, err := lay.ConvertFTSLayout(model.FTSLayoutRequest{DueOnly: true, Tok: tok, Fence: fence})

	return err
}

// reshardHeartbeat renews the import lock and the run's heartbeat in the
// layout record every reshardBeat.
type reshardHeartbeat struct {
	imp          model.Importer
	lay          model.FTSLayouter
	session, tok string
	ttl          time.Duration
	cancel       context.CancelFunc

	mu  sync.Mutex
	err error
}

func (h *reshardHeartbeat) failed() error {
	h.mu.Lock()
	defer h.mu.Unlock()

	return h.err
}

func (h *reshardHeartbeat) run(ctx context.Context) {
	tick := time.NewTicker(reshardBeat)
	defer tick.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}

		if err := h.beat(); err != nil {
			h.mu.Lock()
			h.err = err
			h.mu.Unlock()
			h.cancel()

			return
		}
	}
}

// beat renews the lock and, when that worked, the heartbeat of a
// recorded run.
func (h *reshardHeartbeat) beat() error {
	if _, err := h.imp.ImportRenew(h.session, h.ttl); err != nil {
		return err
	}

	_, err := h.lay.HeartbeatFTSLayoutNext(h.tok, time.Now())

	return err
}

// GetImportReshard reports the state of the session's reshard.
func (s *Server) GetImportReshard(w http.ResponseWriter, r *http.Request, account adminapi.Account, p adminapi.GetImportReshardParams) {
	c, ok := s.beginImport(w, r, account, p.XImportSession)
	if !ok {
		return
	}
	defer c.u.Close()

	lay, ok := c.u.(model.FTSLayouter)
	if !ok {
		writeError(w, http.StatusInternalServerError, adminapi.Internal, "the store has no full-text layouts")
		return
	}

	st, err := lay.FTSLayout()
	if err != nil {
		s.importError(w, r, err)
		return
	}

	writeJSON(w, http.StatusOK, s.reshardState(account, p.XImportSession, st, time.Now()))
}

// reshardState is the state of the session's reshard: from the process
// map, else from the layout record.
func (s *Server) reshardState(account, session string, st model.FTSLayoutState, now time.Time) adminapi.ImportReshard {
	out := adminapi.ImportReshard{State: reshardIdle, G: st.Current.Gen}

	s.rs.mu.Lock()
	run := s.rs.runs[account]
	s.rs.mu.Unlock()

	if run != nil && run.session == session {
		state, msg := run.result()
		out.State, out.Error = state, optional(msg)

		return out
	}

	if st.NextTok == "" || model.TokenSession(st.NextTok) != session {
		return out
	}

	if !st.NextAt.IsZero() && now.Sub(st.NextAt) < jobs.ImportReshardFresh {
		out.State = reshardRunning
	} else {
		out.State, out.Error = reshardFailed, optional("the reshard stopped (its server ended or restarted)")
	}

	return out
}

// refuseDuringReshard answers 409 for an import batch of a session whose
// reshard runs: the batch would meet the frozen index. The process map
// answers without I/O; the layout record (one read) covers a run of the
// session on another server.
func (s *Server) refuseDuringReshard(w http.ResponseWriter, account, session string, u model.User) bool {
	busy := s.rs.running(account, session) != nil

	if lay, ok := u.(model.FTSLayouter); ok && !busy {
		if st, err := lay.FTSLayout(); err == nil && st.NextTok != "" && model.TokenSession(st.NextTok) == session {
			busy = true
		}
	}

	if busy {
		writeError(w, http.StatusConflict, adminapi.Conflict, "a reshard of the account's full-text index runs for this session; "+
			"wait for GET .../import/fts-reshard")
	}

	return busy
}

// afterImportLock is what OpenImport does once the session holds the
// lock: the lock holder clears a conversion an aborted import or a job
// left (F2), and the session's import hint is kept.
func (s *Server) afterImportLock(r *http.Request, account, session string, u model.User, messages *int) error {
	if err := clearForeignOnLock(session, u); err != nil {
		return err
	}

	if messages != nil {
		s.setHint(account, session, *messages)
		auditf(r, "messages_hint", *messages)
	}

	return nil
}

// clearForeignOnLock removes, after OpenImport took the lock, a
// conversion that is not this session's live run (F2, R4.3): an aborted
// import's or a job's that lost the account.
func clearForeignOnLock(session string, u model.User) error {
	lay, ok := u.(model.FTSLayouter)
	if !ok {
		return nil
	}

	st, err := lay.FTSLayout()
	if err != nil || st.NextTok == "" {
		return err
	}

	if model.TokenSession(st.NextTok) == session && !st.NextAt.IsZero() && time.Since(st.NextAt) < jobs.ImportReshardFresh {
		return nil // this session's run (on this or another server)
	}

	_, err = lay.ClearFTSLayoutNext(st.NextTok)

	return err
}

// setHint remembers the import hint of a session.
func (s *Server) setHint(account, session string, messages int) {
	s.rs.mu.Lock()
	defer s.rs.mu.Unlock()

	s.rs.hints[account+"\x00"+session] = messages
}

// applyHint hands the session's import hint to the account's view.
func (s *Server) applyHint(account, session string, u model.User) {
	s.rs.mu.Lock()
	n, ok := s.rs.hints[account+"\x00"+session]
	s.rs.mu.Unlock()

	if h, hok := u.(model.ImportHinter); ok && hok {
		h.SetImportHint(model.ImportHint{Messages: n})
	}
}
