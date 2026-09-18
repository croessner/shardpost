package interp

import (
	"context"
	"encoding/gob"
	"fmt"
	"strings"

	"github.com/foxcpp/go-sieve/lexer"
	"github.com/foxcpp/go-sieve/parser"
)

// External programs (Pigeonhole's vnd.dovecot.execute, vnd.dovecot.pipe and
// vnd.dovecot.filter), shardpost fork. The interpreter never starts a
// process itself: RuntimeData.Program is the hook the server implements
// with its allowlist (name -> exact command line, limits, no shell).
//
//	execute [":input" <string> / ":pipe"] [":output" <varname>] <program> [<args: string-list>]
//	pipe [":copy"] [":try"] <program> [<args>]
//	filter [":try"] <program> [<args>]
//
// All three work as commands and as tests (true when the program exited 0).
// pipe as a command is an action the server runs once the delivery is
// durable (Delivery.Pipes); as a test it runs immediately. filter replaces
// the message with the program's output.

// ProgramKind names the extension a program is run for.
type ProgramKind string

const (
	ProgramExecute ProgramKind = "execute"
	ProgramPipe    ProgramKind = "pipe"
	ProgramFilter  ProgramKind = "filter"
)

// ProgramResult is what a program run produced.
type ProgramResult struct {
	Output   []byte
	ExitCode int
}

// ProgramRunner runs an allowed program with stdin input; ok is false for a
// program that is not allowed (a compile-time error where the name is a
// constant, otherwise a runtime failure).
type ProgramRunner func(ctx context.Context, kind ProgramKind, name string, args []string, input []byte) (ProgramResult, bool, error)

// ActionPipe is the pipe command: the message goes to the program once
// the delivery is durable.
type ActionPipe struct {
	Program string
	Args    []string
	Copy    bool
	Try     bool
}

func (ActionPipe) testActionName() string      { return "pipe" }
func (a ActionPipe) cancelsImplicitKeep() bool { return !a.Copy }

func validProgramName(name string) bool {
	if name == "" || len(name) > 128 || strings.HasPrefix(name, ".") {
		return false
	}
	for _, r := range name {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-' || r == '.') {
			return false
		}
	}
	return true
}

func validProgramArg(a string) bool {
	if len(a) > 1024 {
		return false
	}
	for _, r := range a {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}

type CmdProgram struct {
	Kind     ProgramKind
	Program  string
	Args     []string
	HasInput bool
	Input    string // execute :input
	Pipe     bool   // execute :pipe (the message is the input)
	Output   string // execute :output variable
	Copy     bool   // pipe :copy
	Try      bool   // pipe/filter :try
	IsTest   bool
	NameVar  bool // the name comes from a variable (checked at runtime)
}

// run executes the program now and returns whether it succeeded.
func (c CmdProgram) run(ctx context.Context, d *RuntimeData) (bool, error) {
	if d.Program == nil {
		return false, fmt.Errorf("%s: external programs are not available", c.Kind)
	}
	name := expandVars(d, c.Program)
	if !validProgramName(name) {
		return false, fmt.Errorf("%s: invalid program name", c.Kind)
	}
	args := expandVarsList(d, c.Args)
	for _, a := range args {
		if !validProgramArg(a) {
			return false, fmt.Errorf("%s: invalid program argument", c.Kind)
		}
	}
	var input []byte
	switch {
	case c.Kind == ProgramExecute && c.Pipe, c.Kind != ProgramExecute:
		if rm, ok := d.Msg.(RawMessage); ok {
			input = rm.Raw()
		}
	case c.Kind == ProgramExecute && c.Input != "":
		input = []byte(expandVars(d, c.Input))
	}
	// Pigeonhole's default program input uses LF; callers may request CRLF.
	if input != nil {
		input = []byte(strings.ReplaceAll(string(input), "\r\n", "\n"))
		if d.Script.opts.Conf("sieve_"+string(c.Kind)+"_input_eol") == "crlf" {
			input = []byte(strings.ReplaceAll(string(input), "\n", "\r\n"))
		}
	}
	res, ok, err := d.Program(ctx, c.Kind, name, args, input)
	if err != nil {
		if c.Try || c.IsTest {
			return false, nil
		}
		return false, err
	}
	if !ok {
		if c.NameVar || c.Try || c.IsTest {
			return false, nil
		}
		return false, fmt.Errorf("%s: program %q is not allowed", c.Kind, name)
	}
	if res.ExitCode != 0 {
		if c.Try || c.IsTest {
			return false, nil
		}
		return false, fmt.Errorf("%s: program %q failed", c.Kind, name)
	}
	if c.Kind == ProgramExecute && c.Output != "" {
		if err := d.SetVar(c.Output, string(res.Output)); err != nil {
			return false, err
		}
	}
	if c.Kind == ProgramFilter && len(res.Output) == 0 {
		return false, nil
	}
	if c.Kind == ProgramFilter {
		if he, ok := d.Msg.(interface{ Replace([]byte) }); ok {
			he.Replace(res.Output)
		}
		d.HeaderEdits = nil
		d.mimeCache = nil
		d.Replaced = res.Output
	}
	return true, nil
}

func (c CmdProgram) Execute(ctx context.Context, d *RuntimeData) error {
	if c.Kind == ProgramPipe && !c.IsTest {
		name := expandVars(d, c.Program)
		if !validProgramName(name) {
			return fmt.Errorf("pipe: invalid program name")
		}
		args := expandVarsList(d, c.Args)
		for _, a := range args {
			if !validProgramArg(a) {
				return fmt.Errorf("pipe: invalid program argument")
			}
		}
		if err := d.OnAction(ctx, ActionPipe{Program: name, Args: args, Copy: c.Copy, Try: c.Try}, d); err != nil {
			return err
		}
		return nil
	}
	_, err := c.run(ctx, d)
	return err
}

func (c CmdProgram) Check(ctx context.Context, d *RuntimeData) (bool, error) {
	return c.run(ctx, d)
}

func programExt(k ProgramKind) string { return "vnd.dovecot." + string(k) }

func loadProgram(kind ProgramKind, isTest bool) func(s *Script, pos lexer.Position, args []parser.Arg, tests []parser.Test, block []parser.Cmd) (CmdProgram, error) {
	return func(s *Script, pos lexer.Position, args []parser.Arg, tests []parser.Test, block []parser.Cmd) (CmdProgram, error) {
		cmd := CmdProgram{Kind: kind, IsTest: isTest}
		spec := &Spec{Tags: map[string]SpecTag{}, Pos: []SpecPosArg{
			{MinStrCount: 1, MaxStrCount: 1, MatchStr: func(v []string) { cmd.Program = v[0] }},
			{MinStrCount: 1, Optional: true, MatchStr: func(v []string) { cmd.Args = v }},
		}}
		switch kind {
		case ProgramExecute:
			spec.Tags["input"] = SpecTag{NeedsValue: true, MinStrCount: 1, MaxStrCount: 1, MatchStr: func(v []string) { cmd.Input = v[0]; cmd.HasInput = true }}
			spec.Tags["pipe"] = SpecTag{MatchBool: func() { cmd.Pipe = true }}
			spec.Tags["output"] = SpecTag{NeedsValue: true, MinStrCount: 1, MaxStrCount: 1, NoVariables: true, MatchStr: func(v []string) { cmd.Output = strings.ToLower(v[0]) }}
		case ProgramPipe:
			spec.Tags["copy"] = SpecTag{MatchBool: func() { cmd.Copy = true }}
			spec.Tags["try"] = SpecTag{MatchBool: func() { cmd.Try = true }}
		case ProgramFilter:
			spec.Tags["try"] = SpecTag{MatchBool: func() { cmd.Try = true }}
		}
		if err := LoadSpec(s, spec, pos, args, tests, block); err != nil {
			return cmd, err
		}
		if cmd.Pipe && cmd.HasInput {
			return cmd, parser.ErrorAt(pos, "execute: :input and :pipe exclude each other")
		}
		if cmd.Output != "" {
			if !s.RequiresExtension("variables") {
				return cmd, parser.ErrorAt(pos, "execute :output requires 'variables'")
			}
			if settable, _ := s.IsVarUsable(cmd.Output); !settable {
				return cmd, parser.ErrorAt(pos, "execute: cannot set variable %q", cmd.Output)
			}
		}
		if cmd.Copy && !s.RequiresExtension("copy") {
			return cmd, parser.ErrorAt(pos, "pipe :copy requires 'copy'")
		}
		if len(usedVars(s, cmd.Program)) > 0 {
			return cmd, parser.ErrorAt(pos, "%s: program name cannot contain variables", kind)
		}
		if !validProgramName(cmd.Program) {
			return cmd, parser.ErrorAt(pos, "%s: invalid program name %q", kind, cmd.Program)
		}
		for _, a := range cmd.Args {
			if len(usedVars(s, a)) == 0 && !validProgramArg(a) {
				return cmd, parser.ErrorAt(pos, "%s: invalid program argument", kind)
			}
		}
		return cmd, nil
	}
}

func programCmdLoader(kind ProgramKind) func(*Script, parser.Cmd) (Cmd, error) {
	return func(s *Script, pcmd parser.Cmd) (Cmd, error) {
		if !s.RequiresExtension(programExt(kind)) {
			return nil, parser.ErrorAt(pcmd.Position, "missing require '%s'", programExt(kind))
		}
		return loadProgram(kind, false)(s, pcmd.Position, pcmd.Args, pcmd.Tests, pcmd.Block)
	}
}

func programTestLoader(kind ProgramKind) func(*Script, parser.Test) (Test, error) {
	return func(s *Script, test parser.Test) (Test, error) {
		if !s.RequiresExtension(programExt(kind)) {
			return nil, parser.ErrorAt(test.Position, "missing require '%s'", programExt(kind))
		}
		return loadProgram(kind, true)(s, test.Position, test.Args, test.Tests, nil)
	}
}

func init() {
	gob.Register(CmdProgram{})
	gob.Register(ActionPipe{})
}
