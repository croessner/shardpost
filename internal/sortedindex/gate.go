package sortedindex

import (
	"encoding/json"
	"sync"
	"time"

	"shardpost/internal/model"
)

// Header is what a Gate reads its marker from (an Index or a Queue).
type Header interface {
	Head() (model.IndexHead, error)
}

// Gate holds a reader back from its index until the index's backfill
// has completed (scaling plan §5: read mode only after a completed
// backfill that started after the last server without the index
// stopped). The marker is a JSON report in the head whose "finished" is
// set. While it is missing, Open is false and the marker is looked for
// again every Recheck; once found, the gate stays open for its lifetime
// (a marker is never removed).
type Gate struct {
	// Head carries the marker.
	Head Header
	// Marker is the head marker key.
	Marker string
	// Recheck is how often a closed gate looks for the marker
	// (BackfillRecheck when 0).
	Recheck time.Duration
	// Now is the clock (time.Now when nil).
	Now func() time.Time
	// Closed is called once, the first time the marker is missing (err:
	// the head could not be read); Opened when it is found after that.
	Closed func(err error)
	Opened func()

	mu     sync.Mutex
	open   bool
	next   time.Time
	warned bool
}

// BackfillRecheck is how often a closed Gate looks for its marker.
const BackfillRecheck = 30 * time.Second

// Open reports whether the marker exists.
func (g *Gate) Open() bool {
	g.mu.Lock()
	defer g.mu.Unlock()

	if g.open {
		return true
	}

	now := time.Now()
	if g.Now != nil {
		now = g.Now()
	}

	if now.Before(g.next) {
		return false
	}

	every := g.Recheck
	if every <= 0 {
		every = BackfillRecheck
	}

	g.next = now.Add(every)

	done, err := Backfilled(g.Head, g.Marker)

	switch {
	case done:
		g.open = true

		if g.warned && g.Opened != nil {
			g.Opened()
		}
	case !g.warned:
		g.warned = true

		if g.Closed != nil {
			g.Closed(err)
		}
	}

	return g.open
}

// Backfilled reports whether the head carries the completion marker
// key: a JSON report whose "finished" is set.
func Backfilled(h Header, key string) (bool, error) {
	head, err := h.Head()
	if err != nil {
		return false, err
	}

	var rep struct {
		Finished time.Time `json:"finished"`
	}

	v := head.Markers[key]

	return v != "" && json.Unmarshal([]byte(v), &rep) == nil && !rep.Finished.IsZero(), nil
}
