package memstore

import "shardpost/internal/model"

var _ model.Tracer = (*User)(nil)

// SetTrace implements model.Tracer.
func (u *User) SetTrace(t model.TraceState) error {
	u.mu.Lock()
	defer u.mu.Unlock()

	u.trace = t
	if t.Until.IsZero() {
		u.trace = model.TraceState{}
	}

	return nil
}

// Trace implements model.Tracer.
func (u *User) Trace() (model.TraceState, error) {
	u.mu.Lock()
	defer u.mu.Unlock()

	return u.trace, nil
}
