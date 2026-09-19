package model

import "time"

// TraceState is the protocol trace switch of an account (admin API).
// While it is active, the protocol servers log the exchange of new
// sessions of the account.
type TraceState struct {
	// Until ends the trace; zero means off.
	Until time.Time
	// By is the admin principal that switched it on, SetAt when.
	By    string
	SetAt time.Time
}

// Active reports whether the trace is on at now.
func (t TraceState) Active(now time.Time) bool { return !t.Until.IsZero() && now.Before(t.Until) }

// Tracer is implemented by user views that keep the trace switch of the
// account where every server of the cluster sees it.
type Tracer interface {
	// SetTrace stores the switch; a zero Until removes it.
	SetTrace(t TraceState) error
	// Trace returns the stored switch (zero if none).
	Trace() (TraceState, error)
}
