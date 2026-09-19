package cephstore

import (
	"encoding/json"
	"fmt"
	"time"

	"shardpost/internal/model"
)

// regTrace is the registry key of the account's protocol trace switch.
// It lives next to the mailbox registry so that every server sees it.
const regTrace = "trace"

// traceRec is the value of regTrace.
type traceRec struct {
	Until int64  `json:"until"` // unix seconds
	By    string `json:"by,omitempty"`
	SetAt int64  `json:"at,omitempty"`
}

var _ model.Tracer = (*User)(nil)

// SetTrace implements model.Tracer.
func (u *User) SetTrace(t model.TraceState) error {
	err := casOmap(u, u.idx, oidRegistry, []string{regTrace}, func(cur map[string][]byte) (map[string][]byte, []string) {
		if t.Until.IsZero() {
			// Nothing to remove: no write (it would create the registry).
			if _, ok := cur[regTrace]; !ok {
				return nil, nil
			}

			return nil, []string{regTrace}
		}

		b, _ := json.Marshal(traceRec{Until: t.Until.Unix(), By: t.By, SetAt: t.SetAt.Unix()})

		return map[string][]byte{regTrace: b}, nil
	})
	if err != nil {
		return fmt.Errorf("trace switch: %w", err)
	}

	return nil
}

// Trace implements model.Tracer.
func (u *User) Trace() (model.TraceState, error) {
	vals, err := getOmapKeys(u.idx, oidRegistry, []string{regTrace})
	if err != nil {
		return model.TraceState{}, fmt.Errorf("trace switch: %w", err)
	}

	v, ok := vals[regTrace]
	if !ok {
		return model.TraceState{}, nil
	}

	var r traceRec
	if err := json.Unmarshal(v, &r); err != nil {
		return model.TraceState{}, fmt.Errorf("trace switch unreadable: %w", err)
	}

	t := model.TraceState{Until: time.Unix(r.Until, 0), By: r.By}
	if r.SetAt != 0 {
		t.SetAt = time.Unix(r.SetAt, 0)
	}

	return t, nil
}
