package interp

import (
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

// Vacation periods (Pigeonhole settings). A script's :days or :seconds
// is clamped into [sieve_vacation_min_period, sieve_vacation_max_period];
// without either, sieve_vacation_default_period applies. A maximum of 0
// means no maximum; a minimum of 0 lets a script ask for a period of 0,
// which means a reply to every message.
const (
	DefaultVacationMinPeriod     = 24 * time.Hour
	DefaultVacationMaxPeriod     = 60 * 24 * time.Hour
	DefaultVacationDefaultPeriod = 7 * 24 * time.Hour
)

// VacationPeriods returns the minimum, maximum and default period under
// the settings. A value that does not parse, or a combination that
// contradicts itself (min > max, default outside [min, max]), makes all
// three the built-in defaults, as Pigeonhole does; CheckVacationPeriods
// reports such settings.
func VacationPeriods(conf map[string]string) (minPeriod, maxPeriod, defPeriod time.Duration) {
	minPeriod, maxPeriod, defPeriod, err := vacationPeriods(conf)
	if err != nil {
		return DefaultVacationMinPeriod, DefaultVacationMaxPeriod, DefaultVacationDefaultPeriod
	}
	return minPeriod, maxPeriod, defPeriod
}

// CheckVacationPeriods reports vacation period settings that do not
// parse or contradict each other.
func CheckVacationPeriods(conf map[string]string) error {
	_, _, _, err := vacationPeriods(conf)
	return err
}

func vacationPeriods(conf map[string]string) (minPeriod, maxPeriod, defPeriod time.Duration, err error) {
	get := func(key string, def time.Duration) (time.Duration, error) {
		v := strings.TrimSpace(conf[key])
		if v == "" {
			return def, nil
		}
		d, err := ParseSettingDuration(v)
		if err != nil {
			return 0, fmt.Errorf("%s: %w", key, err)
		}
		return d, nil
	}
	if minPeriod, err = get("sieve_vacation_min_period", DefaultVacationMinPeriod); err != nil {
		return
	}
	if maxPeriod, err = get("sieve_vacation_max_period", DefaultVacationMaxPeriod); err != nil {
		return
	}
	if defPeriod, err = get("sieve_vacation_default_period", DefaultVacationDefaultPeriod); err != nil {
		return
	}
	switch {
	case maxPeriod > 0 && minPeriod > maxPeriod:
		err = errors.New("sieve_vacation_min_period is above sieve_vacation_max_period")
	case defPeriod < minPeriod:
		err = errors.New("sieve_vacation_default_period is below sieve_vacation_min_period")
	case maxPeriod > 0 && defPeriod > maxPeriod:
		err = errors.New("sieve_vacation_default_period is above sieve_vacation_max_period")
	}
	return
}

// VacationPeriod is the effective suppression period of a vacation
// action: seconds as the script gave it (set false: it gave none),
// clamped into the configured range. 0 means a reply to every message.
func VacationPeriod(seconds int, set bool, conf map[string]string) time.Duration {
	minPeriod, maxPeriod, defPeriod := VacationPeriods(conf)
	if !set {
		return defPeriod
	}
	p := time.Duration(seconds) * time.Second
	if seconds < 0 || p < minPeriod {
		p = minPeriod
	}
	if maxPeriod > 0 && p > maxPeriod {
		p = maxPeriod
	}
	return p
}

// ParseSettingDuration reads a duration setting the way Dovecot writes
// them: a number with an optional unit (s, m, h, d, w and their long
// forms such as "secs", "minutes", "days"); a bare number is seconds.
// Go durations ("90m", "1h30m") are accepted as well.
func ParseSettingDuration(v string) (time.Duration, error) {
	v = strings.TrimSpace(v)
	i := 0
	for i < len(v) && v[i] >= '0' && v[i] <= '9' {
		i++
	}
	if i > 0 {
		n, err := strconv.ParseInt(v[:i], 10, 64)
		unit, ok := settingUnits[strings.ToLower(strings.TrimSpace(v[i:]))]
		if err == nil && ok && n <= int64(math.MaxInt64/unit) {
			return time.Duration(n) * unit, nil
		}
	}
	d, err := time.ParseDuration(v)
	if err != nil || d < 0 {
		return 0, fmt.Errorf("invalid duration %q", v)
	}
	return d, nil
}

// settingUnits are the units of ParseSettingDuration.
var settingUnits = map[string]time.Duration{
	"": time.Second, "s": time.Second, "sec": time.Second, "secs": time.Second, "second": time.Second, "seconds": time.Second,
	"m": time.Minute, "min": time.Minute, "mins": time.Minute, "minute": time.Minute, "minutes": time.Minute,
	"h": time.Hour, "hour": time.Hour, "hours": time.Hour,
	"d": 24 * time.Hour, "day": 24 * time.Hour, "days": 24 * time.Hour,
	"w": 7 * 24 * time.Hour, "week": 7 * 24 * time.Hour, "weeks": 7 * 24 * time.Hour,
}
