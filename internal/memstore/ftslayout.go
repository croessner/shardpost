package memstore

import (
	"time"

	"shardpost/internal/model"
	"shardpost/internal/sortedindex"
)

// The layouts of the full-text index (stage B14b) are a matter of RADOS
// objects; this store keeps its index in a map and has one layout. Its
// view implements the interfaces as no-ops, so that the job, worker and
// admin code runs over it unchanged.

var (
	_ model.FTSLayouter  = (*User)(nil)
	_ model.ImportHinter = (*User)(nil)
)

// FTSLayout implements model.FTSLayouter: always layout 2.
func (u *User) FTSLayout() (model.FTSLayoutState, error) {
	return model.FTSLayoutState{Current: model.FTSLayoutShape{Layout: model.FTSLayout2}}, nil
}

// FTSLayoutDue implements model.FTSLayouter: never due.
func (u *User) FTSLayoutDue() (model.FTSLayoutShape, bool, error) {
	return model.FTSLayoutShape{Layout: model.FTSLayout2}, false, nil
}

// ConvertFTSLayout implements model.FTSLayouter: nothing to convert.
func (u *User) ConvertFTSLayout(req model.FTSLayoutRequest) (model.FTSLayoutReport, error) {
	if req.Fence != nil {
		if err := req.Fence(); err != nil {
			return model.FTSLayoutReport{}, err
		}
	}

	shape := model.FTSLayoutShape{Layout: model.FTSLayout2}

	// An online rebuild (stage B14i) indexes every message again with the
	// store's options at once: the switch to them, as a cephstore
	// generation built with them answers.
	if req.Online && req.Rebuild {
		n, err := u.rebuild(func(int, int) {})
		if err != nil {
			return model.FTSLayoutReport{}, err
		}

		return model.FTSLayoutReport{From: shape, To: shape, Online: true, Mode: model.FTSModeReindex, Docs: int64(n), Indexed: int64(n)}, nil
	}

	return model.FTSLayoutReport{From: shape, To: shape, Noop: true, Reason: "the store has one layout", Online: req.Online}, nil
}

// ClearFTSLayoutNext implements model.FTSLayouter: there is never an
// unfinished conversion.
func (u *User) ClearFTSLayoutNext(string) (bool, error) { return false, nil }

// ClearForeignFTSLayoutNext implements model.FTSLayouter.
func (u *User) ClearForeignFTSLayoutNext(string) (string, error) { return "", nil }

// ClearFTSLayoutOld implements model.FTSLayouter.
func (u *User) ClearFTSLayoutOld(string) (bool, error) { return false, nil }

// HeartbeatFTSLayoutNext implements model.FTSLayouter.
func (u *User) HeartbeatFTSLayoutNext(string, time.Time) (bool, error) { return false, nil }

// SetImportHint implements model.ImportHinter (the store sizes nothing).
func (u *User) SetImportHint(model.ImportHint) {}

// FTSDueQueue is the counterpart of the RADOS store's reshard queue: a
// due queue in the index objects of "~accounts" and an in-process worker
// lease. Nothing of this store queues an account; tests do.
func (s *Store) FTSDueQueue() (model.DueQueue, func(ttl time.Duration) (model.Lease, error), error) {
	s.mu.Lock()
	if s.dueLocks == nil {
		s.dueLocks = newMemLocks()
	}

	locks := s.dueLocks
	s.mu.Unlock()

	q := sortedindex.NewQueue(s.IndexBackend(model.AccountsNamespace), model.FTSDueQueue, model.DefaultQueueShards)

	return q, func(ttl time.Duration) (model.Lease, error) { return locks.lock(model.FTSDueQueue, ttl) }, nil
}

// RevokeFTSDueLease makes the holder of the reshard worker lease lose it
// (tests of a lease loss).
func (s *Store) RevokeFTSDueLease() {
	s.mu.Lock()
	locks := s.dueLocks
	s.mu.Unlock()

	if locks != nil {
		locks.revoke()
	}
}
