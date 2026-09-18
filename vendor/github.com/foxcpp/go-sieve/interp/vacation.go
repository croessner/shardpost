package interp

import (
	"context"
	"encoding/gob"

	"github.com/foxcpp/go-sieve/parser"
)

// Vacation extension (RFC 5230, with :seconds from RFC 6131). The
// interpreter only records the action; sending the reply and the
// duplicate suppression are the caller's job (it has the message store
// and the outbound path).

// ActionVacation is an applied "vacation" command.
type ActionVacation struct {
	Reason    string
	Subject   string
	From      string
	Addresses []string
	Handle    string
	Mime      bool
	// Seconds is the suppression period (:days * 86400 or :seconds); 0
	// means the implementation default.
	Seconds int
}

func (ActionVacation) testActionName() string    { return "vacation" }
func (ActionVacation) cancelsImplicitKeep() bool { return false }

// CmdVacation is the loaded command.
type CmdVacation struct {
	Reason    string
	Subject   string
	From      string
	Addresses []string
	Handle    string
	Mime      bool
	Seconds   int
}

func loadVacation(s *Script, pcmd parser.Cmd) (Cmd, error) {
	if !s.RequiresExtension("vacation") && !s.RequiresExtension("vacation-seconds") {
		return nil, parser.ErrorAt(pcmd.Position, "missing require 'vacation'")
	}
	cmd := CmdVacation{}
	err := LoadSpec(s, &Spec{
		Tags: map[string]SpecTag{
			"days": {
				NeedsValue: true,
				MatchNum:   func(v int) { cmd.Seconds = v * 86400 },
			},
			"seconds": {
				NeedsValue: true,
				MatchNum: func(v int) {
					cmd.Seconds = v
				},
			},
			"subject": {
				NeedsValue: true, MinStrCount: 1, MaxStrCount: 1,
				MatchStr: func(v []string) { cmd.Subject = v[0] },
			},
			"from": {
				NeedsValue: true, MinStrCount: 1, MaxStrCount: 1,
				MatchStr: func(v []string) { cmd.From = v[0] },
			},
			"addresses": {
				NeedsValue: true, MinStrCount: 1,
				MatchStr: func(v []string) { cmd.Addresses = v },
			},
			"handle": {
				NeedsValue: true, MinStrCount: 1, MaxStrCount: 1,
				MatchStr: func(v []string) { cmd.Handle = v[0] },
			},
			"mime": {
				NeedsValue: false,
				MatchBool:  func() { cmd.Mime = true },
			},
		},
		Pos: []SpecPosArg{
			{
				MinStrCount: 1, MaxStrCount: 1,
				MatchStr: func(v []string) { cmd.Reason = v[0] },
			},
		},
	}, pcmd.Position, pcmd.Args, pcmd.Tests, pcmd.Block)
	if err != nil {
		return nil, err
	}
	if !s.RequiresExtension("vacation-seconds") && cmd.Seconds != 0 && cmd.Seconds%86400 != 0 {
		return nil, parser.ErrorAt(pcmd.Position, ":seconds requires 'vacation-seconds'")
	}
	return cmd, nil
}

func (c CmdVacation) Execute(ctx context.Context, d *RuntimeData) error {
	act := ActionVacation{
		Reason:  expandVars(d, c.Reason),
		Subject: expandVars(d, c.Subject),
		From:    expandVars(d, c.From),
		Handle:  expandVars(d, c.Handle),
		Mime:    c.Mime,
		Seconds: c.Seconds,
	}
	for _, a := range c.Addresses {
		act.Addresses = append(act.Addresses, expandVars(d, a))
	}
	return d.OnAction(ctx, act, d)
}

func init() { gob.Register(CmdVacation{}) }
