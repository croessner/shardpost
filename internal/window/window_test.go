package window

import (
	"errors"
	"testing"
	"time"
)

func mustParse(t *testing.T, spec, tz string) Window {
	t.Helper()

	w, err := Parse(spec, tz)
	if err != nil {
		t.Fatal(err)
	}

	return w
}

// TestWindowEmpty: no window is always open and fits every run.
func TestWindowEmpty(t *testing.T) {
	w := mustParse(t, "", "")
	now := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)

	if w.Set() || !w.In(now) || w.Fit(now, 100*time.Hour) != nil || !w.NextStart(now).Equal(now) || w.String() != "" {
		t.Errorf("empty window %+v", w)
	}
}

// TestWindowOverMidnight: 23:00-04:00 contains the night on both sides of
// midnight and not the day; the next start of a time inside it is the
// next evening.
func TestWindowOverMidnight(t *testing.T) {
	w := mustParse(t, "23:00-04:00", "UTC")
	day := func(d, h, m int) time.Time { return time.Date(2026, 6, d, h, m, 0, 0, time.UTC) }

	for _, c := range []struct {
		t  time.Time
		in bool
	}{{day(3, 23, 0), true}, {day(4, 1, 30), true}, {day(4, 3, 59), true}, {day(4, 4, 0), false}, {day(4, 12, 0), false}, {day(3, 22, 59), false}} {
		if got := w.In(c.t); got != c.in {
			t.Errorf("In(%s) = %v, want %v", c.t, got, c.in)
		}
	}

	if got := w.NextStart(day(4, 1, 0)); !got.Equal(day(4, 23, 0)) {
		t.Errorf("next start inside the window: %s", got)
	}

	if got := w.NextStart(day(4, 12, 0)); !got.Equal(day(4, 23, 0)) {
		t.Errorf("next start in the day: %s", got)
	}

	if from, to := w.Bounds(day(4, 1, 0)); !from.Equal(day(3, 23, 0)) || !to.Equal(day(4, 4, 0)) {
		t.Errorf("bounds %s %s", from, to)
	}
}

// TestWindowDST: in Europe/Berlin the window of the night the clocks go
// forward (2026-03-29) is an hour shorter, the one they go back
// (2026-10-25) an hour longer.
func TestWindowDST(t *testing.T) {
	w := mustParse(t, "01:00-05:00", "Europe/Berlin")
	berlin, _ := time.LoadLocation("Europe/Berlin")

	for _, c := range []struct {
		day  int
		mon  time.Month
		want time.Duration
	}{{28, time.March, 4 * time.Hour}, {29, time.March, 3 * time.Hour}, {25, time.October, 5 * time.Hour}} {
		at := time.Date(2026, c.mon, c.day, 1, 30, 0, 0, berlin)

		from, to := w.Bounds(at)
		if got := to.Sub(from); got != c.want {
			t.Errorf("%d %s: window of %s, want %s", c.day, c.mon, got, c.want)
		}
	}
}

// TestWindowFit: a run that ends before the window does fits; one that
// does not fit into what is left, or that starts outside, is put off;
// one longer than the whole window is too small for it.
func TestWindowFit(t *testing.T) {
	w := mustParse(t, "02:00-05:00", "UTC")
	at := func(h, m int) time.Time { return time.Date(2026, 10, 3, h, m, 0, 0, time.UTC) }

	for _, c := range []struct {
		now  time.Time
		est  time.Duration
		want error
	}{
		{at(2, 0), 2 * time.Hour, nil},
		{at(4, 0), 2 * time.Hour, ErrOutside},
		{at(1, 0), time.Minute, ErrOutside},
		{at(2, 0), 4 * time.Hour, ErrTooSmall},
		{at(12, 0), 4 * time.Hour, ErrTooSmall},
	} {
		if err := w.Fit(c.now, c.est); !errors.Is(err, c.want) {
			t.Errorf("Fit(%s, %s) = %v, want %v", c.now, c.est, err, c.want)
		}
	}
}

// TestWindowSpread: the spread of an account is stable and within half
// the window.
func TestWindowSpread(t *testing.T) {
	w := mustParse(t, "02:00-05:00", "UTC")

	a, b := w.Spread("alice", 3*time.Hour), w.Spread("alice", 3*time.Hour)
	if a != b || a < 0 || a >= 90*time.Minute {
		t.Errorf("spread %s %s", a, b)
	}

	if w.Spread("alice", 0) != 0 {
		t.Error("spread of an empty window")
	}
}

// TestWindowParse: malformed windows and unknown zones are refused.
func TestWindowParse(t *testing.T) {
	for _, c := range [][2]string{{"2:00-05:00", ""}, {"02:00-02:00", ""}, {"02:00", ""}, {"02:60-03:00", ""}, {"02:00-03:00", "Nowhere/City"}} {
		if _, err := Parse(c[0], c[1]); err == nil {
			t.Errorf("accepted %q in %q", c[0], c[1])
		}
	}

	if w := mustParse(t, " 02:00 - 05:30 ", "Europe/Berlin"); w.String() != "02:00-05:30 Europe/Berlin" {
		t.Errorf("window %q", w.String())
	}
}
