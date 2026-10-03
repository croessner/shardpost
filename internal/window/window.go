// Package window is the reshard window of the full-text index (stage
// B14i, temp/14 §4.12): fts.reshard_window "HH:MM-HH:MM" in the time
// zone fts.reshard_window_tz bounds when the fts_due worker and a batch
// of fts_layout or fts_rebuild start the run of an account. A window may
// go over midnight ("23:00-04:00"); on the days the clocks change it is
// an hour shorter or longer. Runs an operator plans for one account run
// at once, whatever the window says.
package window

import (
	"errors"
	"fmt"
	"hash/fnv"
	"strconv"
	"strings"
	"time"
)

// Window is a daily time window; the zero value is no window (always).
type Window struct {
	set        bool
	start, end int // minutes after midnight
	loc        *time.Location
}

// ErrWindowFormat refuses a window that is not "HH:MM-HH:MM".
var ErrWindowFormat = errors.New(`the window must be "HH:MM-HH:MM" with two different times`)

// Parse parses fts.reshard_window ("" is no window) in the time
// zone tz (an IANA name; "" is UTC).
func Parse(spec, tz string) (Window, error) {
	loc := time.UTC

	if tz != "" {
		l, err := time.LoadLocation(tz)
		if err != nil {
			return Window{}, fmt.Errorf("time zone %q: %w", tz, err)
		}

		loc = l
	}

	spec = strings.TrimSpace(spec)
	if spec == "" {
		return Window{loc: loc}, nil
	}

	from, to, ok := strings.Cut(spec, "-")
	if !ok {
		return Window{}, ErrWindowFormat
	}

	s, err1 := clockMinutes(from)
	e, err2 := clockMinutes(to)

	if err1 != nil || err2 != nil || s == e {
		return Window{}, ErrWindowFormat
	}

	return Window{set: true, start: s, end: e, loc: loc}, nil
}

// clockMinutes parses "HH:MM".
func clockMinutes(s string) (int, error) {
	hh, mm, ok := strings.Cut(strings.TrimSpace(s), ":")
	if !ok || len(hh) != 2 || len(mm) != 2 {
		return 0, ErrWindowFormat
	}

	h, err1 := strconv.Atoi(hh)
	m, err2 := strconv.Atoi(mm)

	if err1 != nil || err2 != nil || h < 0 || h > 23 || m < 0 || m > 59 {
		return 0, ErrWindowFormat
	}

	return h*60 + m, nil
}

// Set reports whether there is a window.
func (w Window) Set() bool { return w.set }

// String is the window as configured.
func (w Window) String() string {
	if !w.set {
		return ""
	}

	return fmt.Sprintf("%02d:%02d-%02d:%02d %s", w.start/60, w.start%60, w.end/60, w.end%60, w.loc)
}

// at returns the window that begins on the calendar day of t (in the
// window's zone) shifted by days.
func (w Window) at(t time.Time, days int) (time.Time, time.Time) {
	t = t.In(w.loc)
	y, m, d := t.Date()
	d += days

	from := time.Date(y, m, d, w.start/60, w.start%60, 0, 0, w.loc)

	ed := d
	if w.end <= w.start {
		ed++ // over midnight
	}

	to := time.Date(y, m, ed, w.end/60, w.end%60, 0, 0, w.loc)

	return from, to
}

// Bounds returns the window that contains t, or else the next one: its
// start and end. Without a window both are t.
func (w Window) Bounds(t time.Time) (time.Time, time.Time) {
	if !w.set {
		return t, t
	}

	for days := -1; days <= 2; days++ {
		from, to := w.at(t, days)
		if t.Before(to) {
			return from, to
		}
	}

	return w.at(t, 3)
}

// In reports whether t is inside the window (always without one).
func (w Window) In(t time.Time) bool {
	if !w.set {
		return true
	}

	from, to := w.Bounds(t)

	return !t.Before(from) && t.Before(to)
}

// NextStart is the start of the next window after t (t itself without
// a window or when t is inside one: then the window's own start is in
// the past, and the next one begins a day later).
func (w Window) NextStart(t time.Time) time.Time {
	if !w.set {
		return t
	}

	from, _ := w.Bounds(t)
	if from.After(t) {
		return from
	}

	_, to := w.Bounds(t)
	next, _ := w.Bounds(to)

	return next
}

// Spread is the stable offset of an account within the first half of
// the window (fnv64 of the account), so that the accounts the worker
// puts off do not all start at the same minute.
func (w Window) Spread(account string, length time.Duration) time.Duration {
	half := length / 2
	if half <= 0 {
		return 0
	}

	h := fnv.New64a()
	_, _ = h.Write([]byte(account))

	return time.Duration(h.Sum64() % uint64(half))
}

// Fit decides whether a run of estimate may start at now: nil when it
// ends before the window does (or there is no window), ErrOutside when
// now is outside the window or the run does not fit into what is left
// of it, ErrTooSmall when it is longer than the whole window.
func (w Window) Fit(now time.Time, estimate time.Duration) error {
	if !w.set {
		return nil
	}

	from, to := w.Bounds(now)
	if estimate > to.Sub(from) {
		return ErrTooSmall
	}

	if now.Before(from) || now.Add(estimate).After(to) {
		return ErrOutside
	}

	return nil
}

// Results of Window.Fit.
var (
	ErrOutside  = errors.New("outside the reshard window")
	ErrTooSmall = errors.New("longer than the reshard window")
)
