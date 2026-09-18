// Package quota implements the quota rules beyond the plain per-user
// limit: grace, per-mailbox rules (ignore, extra allowance), warning
// programs run when the usage crosses a threshold and the over-quota
// flag check. The store asks the Policy while it counts and reserves;
// the Service runs the allowlisted programs.
package quota

import (
	"math"
	"strings"

	"shardpost/internal/config"
	"shardpost/internal/model"
)

// Policy is the compiled set of mailbox rules and the grace. A nil
// Policy has no rules: every mailbox counts, no grace, no extras.
type Policy struct {
	rules []rule
	grace config.Amount
	// renameRecount: a name rule exists and some rule ignores, so a
	// rename can move a mailbox in or out of the count.
	renameRecount bool
}

type rule struct {
	pattern    []rune
	specialUse string
	ignore     bool
	extraBytes config.Amount
	extraMsgs  config.Amount
}

// NewPolicy compiles the rules of cfg (validated by the config package);
// without rules and grace it returns nil.
func NewPolicy(cfg config.Quota) *Policy {
	grace, _ := config.ParseAmount(cfg.Grace, true)
	if len(cfg.Rules) == 0 && grace.Value == 0 {
		return nil
	}

	p := &Policy{grace: grace}
	byName, ignores := false, false

	for _, r := range cfg.Rules {
		c := rule{specialUse: r.SpecialUse, ignore: r.Ignore}
		c.extraBytes, _ = config.ParseAmount(r.ExtraStorage, true)
		c.extraMsgs, _ = config.ParseAmount(r.ExtraMessages, false)

		if r.Mailbox != "" {
			c.pattern = []rune(canonicalPattern(r.Mailbox))
			byName = true
		}

		ignores = ignores || r.Ignore

		p.rules = append(p.rules, c)
	}

	p.renameRecount = byName && ignores

	return p
}

// canonicalPattern spells a pattern the way the store spells names: no
// leading or trailing delimiter, a leading INBOX in capitals (INBOX is
// case-insensitive, RFC 9051 5.1).
func canonicalPattern(pattern string) string {
	const inbox = "INBOX"

	pattern = strings.Trim(pattern, model.Delim)
	if len(pattern) >= len(inbox) && strings.EqualFold(pattern[:len(inbox)], inbox) &&
		(len(pattern) == len(inbox) || pattern[len(inbox)] == model.Delim[0]) {
		return inbox + pattern[len(inbox):]
	}

	return pattern
}

// match returns the first rule matching the mailbox (nil: none).
func (p *Policy) match(mb *model.Mailbox) *rule {
	if p == nil || mb == nil {
		return nil
	}

	for i := range p.rules {
		r := &p.rules[i]
		if r.pattern != nil && globMatch(r.pattern, []rune(mb.Name)) {
			return r
		}

		if r.specialUse != "" && model.HasFlag(mb.SpecialUse, r.specialUse) {
			return r
		}
	}

	return nil
}

// Counts reports whether the messages of mb count against the quota.
func (p *Policy) Counts(mb *model.Mailbox) bool {
	r := p.match(mb)
	return r == nil || !r.ignore
}

// RenameMayRecount reports whether a rename can move a mailbox in or
// out of the count: some rule matches by name and some rule ignores
// (a name rule can also shadow a special-use ignore rule).
func (p *Policy) RenameMayRecount() bool { return p != nil && p.renameRecount }

// Limit returns the limit for saving into mb: the base limit plus the
// extra allowance of the matching rule (unlimited stays unlimited).
func (p *Policy) Limit(base model.Quota, mb *model.Mailbox) model.Quota {
	r := p.match(mb)
	if r == nil {
		return base
	}

	if base.Bytes > 0 {
		base.Bytes = addCapped(base.Bytes, r.extraBytes.Of(base.Bytes))
	}

	if base.Msgs > 0 {
		base.Msgs = addCapped(base.Msgs, r.extraMsgs.Of(base.Msgs))
	}

	return base
}

// Allows reports whether size bytes and n messages fit on top of usage
// under limit (0 = unlimited). A save that starts below the storage
// limit may exceed it by the grace.
func (p *Policy) Allows(usage, limit model.Quota, size, n int64) bool {
	if limit.Msgs > 0 && usage.Msgs+n > limit.Msgs {
		return false
	}

	if limit.Bytes <= 0 || usage.Bytes+size <= limit.Bytes {
		return true
	}

	if p == nil || usage.Bytes >= limit.Bytes {
		return false
	}

	return usage.Bytes+size <= addCapped(limit.Bytes, p.grace.Of(limit.Bytes))
}

// addCapped adds two non-negative values without overflowing.
func addCapped(a, b int64) int64 {
	if s := a + b; s >= a {
		return s
	}

	return math.MaxInt64
}

// globMatch matches name against pattern: "*" any run of characters,
// "?" exactly one.
func globMatch(pattern, name []rune) bool {
	pi, ni := 0, 0
	star, mark := -1, 0

	for ni < len(name) {
		switch {
		case pi < len(pattern) && pattern[pi] == '*':
			star, mark = pi, ni
			pi++
		case pi < len(pattern) && (pattern[pi] == '?' || pattern[pi] == name[ni]):
			pi++
			ni++
		case star >= 0:
			pi = star + 1
			mark++
			ni = mark
		default:
			return false
		}
	}

	for pi < len(pattern) && pattern[pi] == '*' {
		pi++
	}

	return pi == len(pattern)
}
