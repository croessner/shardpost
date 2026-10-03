package config

import (
	"fmt"
	"math"
	"strconv"
	"strings"
)

// Quota holds the quota rules beyond the plain per-user limit
// (server.quota_bytes, server.quota_msgs and the userdb attribute): grace,
// per-mailbox rules, warning programs and the over-quota flag. It follows
// Dovecot's quota_grace, quota_rule, quota_warning and quota_over_flag.
// Programs are entries of extprograms.programs with the kind "quota".
type Quota struct {
	// Grace lets a message that starts below the storage limit exceed it
	// by this much ("10%" of the limit or a size such as "10M"; "" = 0).
	Grace    string         `mapstructure:"grace" env:"QUOTA_GRACE"`
	Rules    []QuotaRule    `mapstructure:"rules" validate:"dive"`
	Warnings []QuotaWarning `mapstructure:"warnings" validate:"dive"`
	OverFlag QuotaOverFlag  `mapstructure:"over_flag"`

	// The online recount (quota_recalc without the account lock, stage
	// B16): it needs the cluster mark ("shardpostctl quota online
	// enable").
	//
	// OnlineRecount is what a plan of quota_recalc without "online" gets:
	// "auto" online while the mark is set, "off" offline under the lock.
	OnlineRecount string `mapstructure:"online_recount" validate:"oneof=auto off"`
	// RecountBarrierSeconds bounds how long a run waits for the
	// operations that started before it; after it the run is busy and
	// planned again.
	RecountBarrierSeconds int `mapstructure:"recount_barrier_seconds" validate:"gte=10,lte=900"`
	// RecountDeadIntentSeconds is how old an intent may be before a run
	// takes it for abandoned; at most scrub.intent_grace_seconds.
	RecountDeadIntentSeconds int `mapstructure:"recount_dead_intent_seconds" validate:"gte=60,lte=3600"`
	// RecountSwitchAttempts are the switch calls before a run holds the
	// usage writers; RecountHoldMS how long it holds them (0: never).
	RecountSwitchAttempts int `mapstructure:"recount_switch_attempts" validate:"gte=1,lte=50"`
	RecountHoldMS         int `mapstructure:"recount_hold_ms" validate:"gte=0,lte=5000"`
	// RecountMaxCaptureKeys and RecountMaxCaptureEntries end a run whose
	// captures grow beyond them; RecountReleaseChunk is how many entries
	// one capture key holds.
	RecountMaxCaptureKeys    int `mapstructure:"recount_max_capture_keys" validate:"gte=10,lte=100000"`
	RecountMaxCaptureEntries int `mapstructure:"recount_max_capture_entries" validate:"gte=1000,lte=10000000"`
	RecountReleaseChunk      int `mapstructure:"recount_release_chunk" validate:"gte=100,lte=50000"`
}

// Values of quota.online_recount and jobs.mailbox_delete_online.
const (
	OnlineAuto = "auto"
	OnlineOff  = "off"
)

// QuotaRule applies to the mailboxes it matches; the first matching rule
// wins. Ignore takes the mailbox out of the usage count; the extras raise
// the limit for messages saved into the mailbox (e.g. Trash, so a full
// account can still move mail there).
type QuotaRule struct {
	// Mailbox is a name pattern: "*" matches any run of characters
	// (including the hierarchy delimiter), "?" a single character.
	Mailbox string `mapstructure:"mailbox"`
	// SpecialUse matches the mailbox carrying this RFC 6154 attribute.
	SpecialUse    string `mapstructure:"special_use"`
	Ignore        bool   `mapstructure:"ignore"`
	ExtraStorage  string `mapstructure:"extra_storage"`
	ExtraMessages string `mapstructure:"extra_messages"`
}

// QuotaWarning runs a program when a change of the usage crosses a
// threshold. Only the first matching warning (in configuration order)
// runs per change, so list the highest threshold first.
type QuotaWarning struct {
	Name     string `mapstructure:"name" validate:"required"`
	Resource string `mapstructure:"resource" validate:"oneof=storage messages"`
	Percent  int64  `mapstructure:"percent" validate:"gt=0,lte=1000"`
	// Direction "over" fires when the usage rises to or above the
	// threshold, "under" when it falls below it again.
	Direction string `mapstructure:"direction" validate:"omitempty,oneof=over under"`
	Program   string `mapstructure:"program" validate:"required"`
}

// QuotaOverFlag compares the over-quota flag the userdb returns (an
// attribute an MTA may use to reject mail early) with the real state and
// runs a program to correct the flag when they differ. The check runs at
// login and after each delivery.
type QuotaOverFlag struct {
	Enabled bool `mapstructure:"enabled" env:"QUOTA_OVER_FLAG_ENABLED"`
	// Attribute is the userdb attribute carrying the flag.
	Attribute string `mapstructure:"attribute" env:"QUOTA_OVER_FLAG_ATTRIBUTE"`
	// OverValue is the attribute value that means "over quota"
	// (compared case-insensitively); anything else, or no attribute,
	// means "not over".
	OverValue string `mapstructure:"over_value" env:"QUOTA_OVER_FLAG_OVER_VALUE"`
	Program   string `mapstructure:"program" env:"QUOTA_OVER_FLAG_PROGRAM"`
	// RecheckSeconds suppresses a second run for the same account and
	// state within this time (the userdb may cache the old flag).
	RecheckSeconds int64 `mapstructure:"recheck_seconds" env:"QUOTA_OVER_FLAG_RECHECK_SECONDS" validate:"gte=0,lte=86400"`
}

// Amount is a quota amount: an absolute value or a percentage of a limit.
type Amount struct {
	Value   int64
	Percent bool
}

// Of returns the amount for the given limit (a percentage of 0 is 0).
func (a Amount) Of(limit int64) int64 {
	if !a.Percent {
		return a.Value
	}

	if limit <= 0 || a.Value == 0 {
		return 0
	}

	if limit <= math.MaxInt64/a.Value {
		return limit * a.Value / 100
	}

	if q := limit / 100; q <= math.MaxInt64/a.Value {
		return q * a.Value
	}

	return math.MaxInt64
}

// ParseSize parses a size: "123", "500M", "2G" (binary K/M/G/T units, an
// optional trailing B). Negative values and overflows are refused.
func ParseSize(s string) (int64, bool) {
	s = strings.TrimSpace(strings.ToUpper(s))
	s = strings.TrimSuffix(s, "B")

	if s == "" {
		return 0, false
	}

	mult := int64(1)

	switch s[len(s)-1] {
	case 'K':
		mult = 1 << 10
	case 'M':
		mult = 1 << 20
	case 'G':
		mult = 1 << 30
	case 'T':
		mult = 1 << 40
	}

	if mult > 1 {
		s = s[:len(s)-1]
	}

	n, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	if err != nil || n < 0 || n > math.MaxInt64/mult {
		return 0, false
	}

	return n * mult, true
}

// ParseAmount parses "10%" (1..1000 percent) or an absolute value; with
// size set the value may carry a unit (ParseSize), otherwise it is a
// plain count. "" is zero.
func ParseAmount(s string, size bool) (Amount, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return Amount{}, true
	}

	if p, ok := strings.CutSuffix(s, "%"); ok {
		n, err := strconv.ParseInt(strings.TrimSpace(p), 10, 64)
		if err != nil || n < 0 || n > 1000 {
			return Amount{}, false
		}

		return Amount{Value: n, Percent: true}, true
	}

	if size {
		n, ok := ParseSize(s)
		return Amount{Value: n}, ok
	}

	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n < 0 {
		return Amount{}, false
	}

	return Amount{Value: n}, true
}

// Validate checks grace, rules, warnings and the over-quota flag; the
// program references are checked by checkQuotaPrograms.
func (c Quota) Validate() error {
	if _, ok := ParseAmount(c.Grace, true); !ok {
		return fmt.Errorf("quota.grace must be a size or a percentage")
	}

	for i, r := range c.Rules {
		if err := r.validate(); err != nil {
			return fmt.Errorf("quota.rules[%d]: %w", i, err)
		}
	}

	names := map[string]bool{}
	for _, w := range c.Warnings {
		if names[w.Name] {
			return fmt.Errorf("duplicate quota warning name")
		}

		names[w.Name] = true
	}

	o := c.OverFlag
	if o.Enabled && (o.Attribute == "" || o.OverValue == "" || o.Program == "") {
		return fmt.Errorf("quota.over_flag requires attribute, over_value and program")
	}

	return nil
}

// validate checks the matcher and the effect of one mailbox rule.
func (r QuotaRule) validate() error {
	if (r.Mailbox == "") == (r.SpecialUse == "") {
		return fmt.Errorf("exactly one of mailbox and special_use is required")
	}

	if strings.ContainsAny(r.Mailbox, "\x00\r\n") {
		return fmt.Errorf("mailbox contains control characters")
	}

	// "/" is the hierarchy delimiter (model.Delim), trimmed like the
	// store trims mailbox names.
	if r.SpecialUse == "" && strings.Trim(r.Mailbox, "/") == "" {
		return fmt.Errorf("mailbox pattern is empty")
	}

	if r.SpecialUse != "" && !strings.HasPrefix(r.SpecialUse, `\`) {
		return fmt.Errorf("special_use must begin with a backslash")
	}

	return r.validateEffect()
}

// validateEffect checks that a rule either ignores or grants a valid
// extra allowance.
func (r QuotaRule) validateEffect() error {
	if r.Ignore && (r.ExtraStorage != "" || r.ExtraMessages != "") {
		return fmt.Errorf("ignore excludes extra_storage and extra_messages")
	}

	if !r.Ignore && r.ExtraStorage == "" && r.ExtraMessages == "" {
		return fmt.Errorf("rule has no effect")
	}

	if _, ok := ParseAmount(r.ExtraStorage, true); !ok {
		return fmt.Errorf("extra_storage must be a size or a percentage")
	}

	if _, ok := ParseAmount(r.ExtraMessages, false); !ok {
		return fmt.Errorf("extra_messages must be a count or a percentage")
	}

	return nil
}

// checkQuotaPrograms verifies that every quota warning and the
// over-quota flag name a program that may be used for quota.
func (c *Config) checkQuotaPrograms() error {
	for _, w := range c.Quota.Warnings {
		if !c.programAllows(w.Program, "quota") {
			return fmt.Errorf("quota warning %s names a program that is not allowed for quota", w.Name)
		}
	}

	if c.Quota.OverFlag.Enabled && !c.programAllows(c.Quota.OverFlag.Program, "quota") {
		return fmt.Errorf("quota.over_flag names a program that is not allowed for quota")
	}

	return nil
}

// programAllows reports whether name is an extprograms entry that may be
// used for kind.
func (c *Config) programAllows(name, kind string) bool {
	p, ok := c.ExtPrograms.Programs[name]
	if !ok {
		return false
	}

	if len(p.Kinds) == 0 {
		return true
	}

	for _, k := range p.Kinds {
		if k == kind {
			return true
		}
	}

	return false
}
