package interp

import (
	"fmt"
	"unicode/utf8"

	"github.com/foxcpp/go-sieve/lexer"
	"github.com/foxcpp/go-sieve/parser"
)

// fcc extension (RFC 8580), shardpost fork: the message an action
// generates (a vacation reply, a mailto notification) is also stored in
// one of the user's mailboxes. The interpreter only records where; the
// caller stores the message once it was actually sent.

// Fcc is a loaded :fcc argument with its optional companions :create
// (mailbox), :flags (imap4flags) and :specialuse (special-use).
type Fcc struct {
	Mailbox    string
	Create     bool
	Flags      Flags
	SpecialUse string
}

// FccTarget is the :fcc of an applied action, with its variables
// expanded.
type FccTarget struct {
	Mailbox string
	// Create is :create: the mailbox is created when it is missing;
	// without it a missing mailbox means the copy is not stored.
	Create bool
	// Flags are the flags of the stored copy (nil: none).
	Flags Flags
	// SpecialUse is :specialuse: the mailbox with that use wins over
	// Mailbox when there is one.
	SpecialUse string
}

// fccSpec collects the :fcc tags of one command while it is loaded.
type fccSpec struct {
	fcc                       Fcc
	given                     bool
	create, flags, specialuse bool
}

// addTags adds :fcc, :create, :flags and :specialuse to the tags of a
// command.
func (f *fccSpec) addTags(tags map[string]SpecTag) {
	tags["fcc"] = SpecTag{NeedsValue: true, MinStrCount: 1, MaxStrCount: 1, MatchStr: func(v []string) {
		f.given = true
		f.fcc.Mailbox = v[0]
	}}
	tags["create"] = SpecTag{MatchBool: func() {
		f.create = true
		f.fcc.Create = true
	}}
	tags["flags"] = SpecTag{NeedsValue: true, MinStrCount: 1, MatchStr: func(v []string) {
		f.flags = true
		f.fcc.Flags = canonicalFlags(v, nil, nil)
	}}
	tags["specialuse"] = SpecTag{NeedsValue: true, MinStrCount: 1, MaxStrCount: 1, MatchStr: func(v []string) {
		f.specialuse = true
		f.fcc.SpecialUse = v[0]
	}}
}

// check validates the collected tags (RFC 8580 3) and returns the :fcc
// argument, nil when none was given.
func (f *fccSpec) check(s *Script, pos lexer.Position, command string) (*Fcc, error) {
	if !f.given {
		for _, t := range []struct {
			name string
			used bool
		}{{"create", f.create}, {"flags", f.flags}, {"specialuse", f.specialuse}} {
			if t.used {
				return nil, parser.ErrorAt(pos, "%s: :%s is only allowed with :fcc", command, t.name)
			}
		}
		return nil, nil
	}
	if !s.RequiresExtension("fcc") {
		return nil, parser.ErrorAt(pos, "missing require 'fcc'")
	}
	if len(usedVars(s, f.fcc.Mailbox)) == 0 && (f.fcc.Mailbox == "" || !utf8.ValidString(f.fcc.Mailbox)) {
		return nil, parser.ErrorAt(pos, "%s: invalid :fcc mailbox name", command)
	}
	if f.create && !s.RequiresExtension("mailbox") {
		return nil, parser.ErrorAt(pos, ":create requires 'mailbox'")
	}
	if f.flags && !s.RequiresExtension("imap4flags") {
		return nil, parser.ErrorAt(pos, "missing require 'imap4flags'")
	}
	if f.specialuse {
		if !s.RequiresExtension("special-use") {
			return nil, parser.ErrorAt(pos, ":specialuse requires 'special-use'")
		}
		if len(usedVars(s, f.fcc.SpecialUse)) == 0 && !validSpecialUse(f.fcc.SpecialUse) {
			return nil, parser.ErrorAt(pos, "invalid special-use flag %q", f.fcc.SpecialUse)
		}
	}
	fcc := f.fcc
	return &fcc, nil
}

// expand returns the target of an :fcc at runtime (nil without one).
func (f *Fcc) expand(d *RuntimeData) (*FccTarget, error) {
	if f == nil {
		return nil, nil
	}
	t := &FccTarget{Mailbox: expandVars(d, f.Mailbox), Create: f.Create, SpecialUse: expandVars(d, f.SpecialUse)}
	if t.Mailbox == "" || !utf8.ValidString(t.Mailbox) {
		return nil, fmt.Errorf("fcc: mailbox name is not valid UTF-8")
	}
	if f.Flags != nil {
		t.Flags = canonicalFlags(expandVarsList(d, f.Flags), nil, d.FlagAliases)
	}
	return t, nil
}
