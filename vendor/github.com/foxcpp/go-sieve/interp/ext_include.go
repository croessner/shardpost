package interp

import (
	"bytes"
	"context"
	"encoding/gob"
	"fmt"
	"strings"
	"unicode"

	"github.com/foxcpp/go-sieve/lexer"
	"github.com/foxcpp/go-sieve/parser"
)

// ihave (RFC 5463) and include (RFC 6609), shardpost fork.

// --- ihave -----------------------------------------------------------------

// IhaveTest is true when every named extension is available; the load
// step then enables them for the script. RFC 5463 4: encoded-character
// and variables cannot be tested with ihave.
type IhaveTest struct{ Result bool }

func (t IhaveTest) Check(context.Context, *RuntimeData) (bool, error) { return t.Result, nil }

func loadIhaveTest(s *Script, test parser.Test) (Test, error) {
	if !s.RequiresExtension("ihave") {
		return nil, parser.ErrorAt(test.Position, "missing require 'ihave'")
	}
	var names []string
	if err := LoadSpec(s, &Spec{
		Pos: []SpecPosArg{{MinStrCount: 1, MatchStr: func(v []string) { names = v }, NoVariables: true}},
	}, test.Position, test.Args, test.Tests, nil); err != nil {
		return nil, err
	}
	ok := true
	for _, n := range names {
		if n == "imapsieve" && s.opts.Conf("imapsieve_enabled") == "yes" {
			continue
		}
		if s.opts.Conf("imapsieve_context") == "yes" && (n == "reject" || n == "ereject" || n == "vacation" || n == "vacation-seconds") {
			ok = false
			break
		}
		if n == "encoded-character" || n == "variables" || n == DovecotTestExtension {
			ok = false
			break
		}
		if _, supported := supportedRequires[n]; !supported {
			ok = false
			break
		}
	}
	if !ok {
		s.skipBlock = true
		return IhaveTest{}, nil
	}
	for _, n := range names {
		s.extensions[n] = struct{}{}
	}
	return IhaveTest{Result: true}, nil
}

// CmdError is the "error" command of ihave: a runtime error with the
// script's own message.
type CmdError struct{ Message string }

func (c CmdError) Execute(_ context.Context, d *RuntimeData) error {
	return fmt.Errorf("script error: %s", expandVars(d, c.Message))
}

func loadErrorCmd(s *Script, pcmd parser.Cmd) (Cmd, error) {
	if !s.RequiresExtension("ihave") {
		return nil, parser.ErrorAt(pcmd.Position, "missing require 'ihave'")
	}
	cmd := CmdError{}
	err := LoadSpec(s, &Spec{
		Pos: []SpecPosArg{{MinStrCount: 1, MaxStrCount: 1, MatchStr: func(v []string) { cmd.Message = v[0] }}},
	}, pcmd.Position, pcmd.Args, pcmd.Tests, pcmd.Block)
	return cmd, err
}

// --- include ---------------------------------------------------------------

const (
	defaultMaxIncludes     = 255
	defaultMaxIncludeDepth = 10
)

// includeState is the include chain of one top-level load.
type includeState struct {
	stack  []string // "p:name" / "g:name" of the scripts being loaded
	count  int
	loaded map[string]*Script
}

// CmdInclude runs another script in the same delivery: actions and the
// global variables are shared, local variables and match variables are
// the included script's own.
type CmdInclude struct {
	Name     string
	Global   bool
	Once     bool
	Optional bool
	Missing  bool // :optional and the script does not exist
	Script   *Script
	// Lazy: an :once include that closes a cycle; the script is looked
	// up when it runs (it is loaded by then) and skipped as already
	// included.
	Lazy bool
}

func (c CmdInclude) key() string {
	if c.Global {
		return "g:" + c.Name
	}
	return "p:" + c.Name
}

func (c CmdInclude) Execute(ctx context.Context, d *RuntimeData) error {
	if c.Missing {
		return nil
	}
	if c.Lazy && c.Script == nil {
		if st := d.Script.opts.inc; st != nil {
			c.Script = st.loaded[c.key()]
		}
	}
	if c.Script == nil {
		return nil
	}
	if d.IncludedOnce == nil {
		d.IncludedOnce = map[string]bool{}
	}
	if c.Once && d.IncludedOnce[c.key()] {
		return nil
	}
	d.IncludedOnce[c.key()] = true
	// the included script sees the shared state (actions, flags, globals)
	// through the same RuntimeData; only the script-local parts are swapped
	script, vars, mvars, ifRes := d.Script, d.Variables, d.MatchVariables, d.ifResult
	d.Script, d.Variables, d.MatchVariables, d.ifResult = c.Script, map[string]string{}, nil, false
	defer func() { d.Script, d.Variables, d.MatchVariables, d.ifResult = script, vars, mvars, ifRes }()
	for _, cmd := range c.Script.cmd {
		if err := d.checkExecution(); err != nil {
			return err
		}
		if err := cmd.Execute(ctx, d); err != nil {
			if err == ErrReturn {
				return nil
			}
			return err // ErrStop ends the whole run (RFC 6609 3.2)
		}
	}
	return nil
}

func validScriptName(name string) bool {
	if name == "" || name == "." || name == ".." || strings.ContainsAny(name, "/\x00") {
		return false
	}
	for _, r := range name {
		if r < 0x20 || r == 0x7f || (r >= 0x80 && r <= 0x9f) || r == 0x2028 || r == 0x2029 {
			return false
		}
		if !unicode.IsPrint(r) && r != ' ' {
			return false
		}
	}
	return true
}

func loadInclude(s *Script, pcmd parser.Cmd) (Cmd, error) {
	if !s.RequiresExtension("include") {
		return nil, parser.ErrorAt(pcmd.Position, "missing require 'include'")
	}
	cmd := CmdInclude{}
	err := LoadSpec(s, &Spec{
		Tags: map[string]SpecTag{
			"once":     {MatchBool: func() { cmd.Once = true }},
			"optional": {MatchBool: func() { cmd.Optional = true }},
			"personal": {MatchBool: func() { cmd.Global = false }},
			"global":   {MatchBool: func() { cmd.Global = true }},
		},
		Pos: []SpecPosArg{{MinStrCount: 1, MaxStrCount: 1, NoVariables: true, MatchStr: func(v []string) { cmd.Name = v[0] }}},
	}, pcmd.Position, pcmd.Args, pcmd.Tests, pcmd.Block)
	if err != nil {
		return nil, err
	}
	if !validScriptName(cmd.Name) {
		return nil, parser.ErrorAt(pcmd.Position, "include: invalid script name %q", cmd.Name)
	}
	if s.opts.IncludeLoader == nil {
		return nil, parser.ErrorAt(pcmd.Position, "include: no script store available")
	}
	if s.opts.inc == nil {
		s.opts.inc = &includeState{}
	}
	st := s.opts.inc
	maxInc, maxDepth := s.opts.MaxIncludes, s.opts.MaxIncludeDepth
	if v := s.opts.Conf("sieve_include_max_includes"); v != "" {
		fmt.Sscanf(v, "%d", &maxInc)
	}
	if v := s.opts.Conf("sieve_include_max_nesting_depth"); v != "" {
		fmt.Sscanf(v, "%d", &maxDepth)
	}
	if maxInc <= 0 {
		maxInc = defaultMaxIncludes
	}
	if maxDepth <= 0 {
		maxDepth = defaultMaxIncludeDepth
	}
	for _, k := range st.stack {
		if k == cmd.key() {
			if cmd.Once {
				cmd.Lazy = true // runs at most once anyway: no cycle at runtime
				return cmd, nil
			}
			return nil, parser.ErrorAt(pcmd.Position, "include: circular include of %q", cmd.Name)
		}
	}
	if len(st.stack) >= maxDepth {
		return nil, parser.ErrorAt(pcmd.Position, "include: nesting deeper than %d", maxDepth)
	}
	st.count++
	if st.count > maxInc {
		return nil, parser.ErrorAt(pcmd.Position, "include: more than %d includes", maxInc)
	}
	text, ok, err := s.opts.IncludeLoader(cmd.Name, cmd.Global)
	if err != nil {
		return nil, parser.ErrorAt(pcmd.Position, "include %q: %v", cmd.Name, err)
	}
	if !ok {
		if cmd.Optional {
			cmd.Missing = true
			return cmd, nil
		}
		return nil, parser.ErrorAt(pcmd.Position, "include: script %q does not exist", cmd.Name)
	}
	toks, err := lexer.Lex(bytes.NewReader(text), &lexer.Options{Filename: cmd.Name, MaxTokens: 5000})
	if err != nil {
		return nil, parser.ErrorAt(pcmd.Position, "include %q: %v", cmd.Name, err)
	}
	cmds, err := parser.Parse(lexer.NewStream(toks), &parser.Options{MaxBlockNesting: 15, MaxTestNesting: 15})
	if err != nil {
		return nil, parser.ErrorAt(pcmd.Position, "include %q: %v", cmd.Name, err)
	}
	st.stack = append(st.stack, cmd.key())
	sub, err := LoadScript(cmds, s.opts)
	st.stack = st.stack[:len(st.stack)-1]
	if err != nil {
		return nil, parser.ErrorAt(pcmd.Position, "include %q: %v", cmd.Name, err)
	}
	cmd.Script = sub
	if st.loaded == nil {
		st.loaded = map[string]*Script{}
	}
	st.loaded[cmd.key()] = sub
	return cmd, nil
}

// CmdGlobal declares variables as shared (RFC 6609 3.4); it does
// nothing at runtime, the declaration lives in the script.
type CmdGlobal struct{ Names []string }

func (CmdGlobal) Execute(context.Context, *RuntimeData) error { return nil }

func loadGlobal(s *Script, pcmd parser.Cmd) (Cmd, error) {
	if !s.RequiresExtension("include") {
		return nil, parser.ErrorAt(pcmd.Position, "missing require 'include'")
	}
	if !s.RequiresExtension("variables") {
		return nil, parser.ErrorAt(pcmd.Position, "global requires 'variables'")
	}
	cmd := CmdGlobal{}
	if err := LoadSpec(s, &Spec{
		Pos: []SpecPosArg{{MinStrCount: 1, NoVariables: true, MatchStr: func(v []string) { cmd.Names = v }}},
	}, pcmd.Position, pcmd.Args, pcmd.Tests, pcmd.Block); err != nil {
		return nil, err
	}
	if s.globals == nil {
		s.globals = map[string]bool{}
	}
	for i, n := range cmd.Names {
		n = strings.ToLower(n)
		if strings.Contains(n, ".") || !lexer.IsValidIdentifier(n) {
			return nil, parser.ErrorAt(pcmd.Position, "global: invalid variable name %q", n)
		}
		if s.setLocals[n] {
			return nil, parser.ErrorAt(pcmd.Position, "global: variable %q was already used as a local variable", n)
		}
		cmd.Names[i] = n
		s.globals[n] = true
	}
	return cmd, nil
}

type CmdReturn struct{}

func (CmdReturn) Execute(context.Context, *RuntimeData) error { return ErrReturn }

func loadReturn(s *Script, pcmd parser.Cmd) (Cmd, error) {
	if !s.RequiresExtension("include") {
		return nil, parser.ErrorAt(pcmd.Position, "missing require 'include'")
	}
	err := LoadSpec(s, &Spec{}, pcmd.Position, pcmd.Args, pcmd.Tests, pcmd.Block)
	return CmdReturn{}, err
}

func init() {
	gob.Register(IhaveTest{})
	gob.Register(CmdError{})
	gob.Register(CmdInclude{})
	gob.Register(CmdGlobal{})
	gob.Register(CmdReturn{})
	gob.Register(CmdDebugLog{})
}

// CmdDebugLog is Pigeonhole's debug_log (vnd.dovecot.debug): a log line.
type CmdDebugLog struct{ Message string }

func (c CmdDebugLog) Execute(_ context.Context, d *RuntimeData) error {
	if d.Script.opts.T != nil {
		d.Script.opts.T.Log("debug_log:", expandVars(d, c.Message))
	}
	return nil
}

func loadDebugLog(s *Script, pcmd parser.Cmd) (Cmd, error) {
	if !s.RequiresExtension("vnd.dovecot.debug") {
		return nil, parser.ErrorAt(pcmd.Position, "missing require 'vnd.dovecot.debug'")
	}
	cmd := CmdDebugLog{}
	err := LoadSpec(s, &Spec{
		Pos: []SpecPosArg{{MinStrCount: 1, MaxStrCount: 1, MatchStr: func(v []string) { cmd.Message = v[0] }}},
	}, pcmd.Position, pcmd.Args, pcmd.Tests, pcmd.Block)
	return cmd, err
}
