package interp

import (
	"testing"
	"time"
)

func TestParseSettingDuration(t *testing.T) {
	cases := map[string]time.Duration{
		"0": 0, "30": 30 * time.Second, "1s": time.Second, "5 mins": 5 * time.Minute, "2h": 2 * time.Hour,
		"1d": 24 * time.Hour, "60 days": 60 * 24 * time.Hour, "1w": 7 * 24 * time.Hour, "1h30m": 90 * time.Minute,
	}
	for in, want := range cases {
		if got, err := ParseSettingDuration(in); err != nil || got != want {
			t.Errorf("%q: %v %v, want %v", in, got, err, want)
		}
	}
	for _, in := range []string{"", "soon", "-1h", "1y", "99999999999999999999d"} {
		if _, err := ParseSettingDuration(in); err == nil {
			t.Errorf("%q accepted", in)
		}
	}
}

func TestVacationPeriod(t *testing.T) {
	day := 24 * time.Hour
	cases := []struct {
		seconds int
		set     bool
		conf    map[string]string
		want    time.Duration
	}{
		{0, false, nil, 7 * day},
		{0, true, nil, day},
		{30 * 86400, true, nil, 30 * day},
		{365 * 86400, true, nil, 60 * day},
		{0, true, map[string]string{"sieve_vacation_min_period": "0"}, 0},
		{1, true, map[string]string{"sieve_vacation_min_period": "1s"}, time.Second},
		{365 * 86400, true, map[string]string{"sieve_vacation_max_period": "0"}, 365 * day},
		// Contradicting settings: the built-in defaults.
		{0, false, map[string]string{"sieve_vacation_default_period": "100d"}, 7 * day},
	}
	for _, c := range cases {
		if got := VacationPeriod(c.seconds, c.set, c.conf); got != c.want {
			t.Errorf("%d %v %v: %v, want %v", c.seconds, c.set, c.conf, got, c.want)
		}
	}
	if CheckVacationPeriods(map[string]string{"sieve_vacation_default_period": "100d"}) == nil {
		t.Error("a default above the maximum was accepted")
	}
}
