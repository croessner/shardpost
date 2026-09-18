package interp

import (
	"fmt"
	"strings"

	"github.com/foxcpp/go-sieve/parser"
)

func loadSet(script *Script, pcmd parser.Cmd) (Cmd, error) {
	if !script.RequiresExtension("variables") {
		return nil, parser.ErrorAt(pcmd.Position, "missing require 'variables'")
	}
	cmd := CmdSet{}

	var mods []string
	seen := map[int]bool{}
	var conflictingMods bool
	mod := func(name string, prec int) SpecTag {
		return SpecTag{MatchBool: func() {
			if seen[prec] {
				conflictingMods = true
			}
			seen[prec] = true
			mods = append(mods, name)
		}}
	}
	err := LoadSpec(script, &Spec{
		Tags: map[string]SpecTag{
			"length":        mod("length", 10),
			"quotewildcard": mod("quotewildcard", 20),
			"encodeurl":     mod("encodeurl", 15),
			"upperfirst":    mod("upperfirst", 30),
			"lowerfirst":    mod("lowerfirst", 30),
			"upper":         mod("upper", 40),
			"lower":         mod("lower", 40),
		},
		Pos: []SpecPosArg{
			{
				MinStrCount: 1,
				MaxStrCount: 1,
				MatchStr: func(val []string) {
					cmd.Name = strings.ToLower(val[0])
				},
			},
			{
				MinStrCount: 1,
				MaxStrCount: 1,
				MatchStr: func(val []string) {
					cmd.Value = val[0]
				},
			},
		},
	}, pcmd.Position, pcmd.Args, pcmd.Tests, pcmd.Block)

	if conflictingMods {
		return nil, parser.ErrorAt(pcmd.Position, "conflicting value modifiers")
	}
	for _, m := range mods {
		if m == "encodeurl" && !script.RequiresExtension("enotify") {
			return nil, parser.ErrorAt(pcmd.Position, ":encodeurl requires 'enotify'")
		}
	}

	settable, _ := script.IsVarUsable(cmd.Name)
	if !settable {
		return nil, parser.ErrorAt(pcmd.Position, "cannot set this variable")
	}
	cmd.Modifiers = mods
	if !strings.Contains(cmd.Name, ".") && !script.globals[cmd.Name] {
		if script.setLocals == nil {
			script.setLocals = map[string]bool{}
		}
		script.setLocals[cmd.Name] = true
	}

	return cmd, err
}

func loadStringTest(s *Script, test parser.Test) (Test, error) {
	if !s.RequiresExtension("variables") {
		return nil, fmt.Errorf("missing require 'variables'")
	}

	loaded := TestString{matcherTest: newMatcherTest()}
	var key []string
	err := LoadSpec(s, loaded.addSpecTags(&Spec{
		Pos: []SpecPosArg{
			{
				MatchStr: func(val []string) {
					loaded.Source = val
				},
				MinStrCount: 1,
			},
			{
				MatchStr: func(val []string) {
					key = val
				},
				MinStrCount: 1,
			},
		},
	}), test.Position, test.Args, test.Tests, nil)
	if err != nil {
		return nil, err
	}

	if err := loaded.setKey(s, key); err != nil {
		return nil, err
	}

	return loaded, nil
}
