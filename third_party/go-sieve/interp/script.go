package interp

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/foxcpp/go-sieve/lexer"
)

type Cmd interface {
	Execute(ctx context.Context, d *RuntimeData) error
}

type Options struct {
	MaxRedirects int

	MaxVariableCount   int
	MaxVariableNameLen int
	MaxVariableLen     int

	// SubAddressSep is the set of separator characters for subaddress parsing
	// (RFC 5233). Each character in the string is treated as a separator.
	// Defaults to "+" if empty.
	SubAddressSep string

	// If specified - enables vnd.dovecot.testsuite extension
	// and will execute tests.
	T             *testing.T
	DisabledTests []string
	// TestPath is the directory of the test file (${tst.path}).
	TestPath string

	// Config holds implementation settings (Pigeonhole key names, e.g.
	// sieve_spamtest_status_header); the testsuite writes it with
	// test_config_set, a server fills it from its configuration.
	Config map[string]string

	// IncludeLoader returns the text of a script for the include
	// extension (RFC 6609); nil means include is unavailable. ok is
	// false for a missing script (":optional" then does nothing).
	IncludeLoader func(name string, global bool) (text []byte, ok bool, err error)
	// MaxIncludes and MaxIncludeDepth bound the include extension
	// (0: 255 includes, 10 levels).
	MaxIncludes     int
	MaxIncludeDepth int
	// MaxActions bounds the actions of one run (0: DefaultMaxActions,
	// configuration key sieve_max_actions).
	MaxActions int

	// inc tracks the include chain while loading (shared by the nested
	// loads of one top-level script).
	inc *includeState
}

// Conf returns a configuration value ("" if unset).
func (o *Options) Conf(key string) string {
	if o == nil || o.Config == nil {
		return ""
	}
	return o.Config[key]
}

// SetConf sets a configuration value (empty value removes it).
func (o *Options) SetConf(key, value string) {
	if o.Config == nil {
		o.Config = map[string]string{}
	}
	if value == "" {
		delete(o.Config, key)
		return
	}
	o.Config[key] = value
}

type Script struct {
	extensions map[string]struct{}
	cmd        []Cmd

	opts *Options

	// globals are the variables this script declared with "global"
	// (include extension); they live in RuntimeData.Globals.
	globals map[string]bool
	// setLocals are the local variables a "set" assigned (a later
	// "global" of the same name is an error, RFC 6609 3.4).
	setLocals map[string]bool
	// loopDepth counts the foreverypart blocks being loaded (break and
	// extracttext are only valid inside one).
	loopDepth int
	// skipBlock is set by an ihave test whose extensions are missing:
	// the following block is not loaded (RFC 5463 3.1).
	skipBlock bool
}

// Options returns the script's options (configuration lookups).
func (s *Script) Options() *Options { return s.opts }

// IsGlobal reports whether name (lower case, no namespace) is a global
// variable of this script.
func (s *Script) IsGlobal(name string) bool { return s.globals[name] }

var ErrStop = errors.New("interpreter: stop called")

// ErrReturn ends an included script (include extension "return"); in the
// top-level script it acts like stop.
var ErrReturn = errors.New("interpreter: return called")

func (s Script) Extensions() []string {
	exts := make([]string, 0, len(s.extensions))
	for ext := range s.extensions {
		exts = append(exts, ext)
	}
	return exts
}

func (s Script) RequiresExtension(name string) bool {
	_, ok := s.extensions[name]
	return ok
}

func (s Script) IsVarUsable(variableName string) (settable, gettable bool) {
	if len(variableName) > s.opts.MaxVariableNameLen {
		return false, false
	}

	namespace, name, ok := strings.Cut(strings.ToLower(variableName), ".")
	if !ok {
		name = namespace
		namespace = ""
	}

	if !lexer.IsValidIdentifier(name) {
		return false, false
	}

	switch namespace {
	case "envelope":
		if !s.RequiresExtension("envelope") {
			return false, false
		}
		return false, true
	case "global":
		if !s.RequiresExtension("include") {
			return false, false
		}
		return true, true
	case "tst":
		return false, s.RequiresExtension(DovecotTestExtension)
	case "":
		return true, true
	default:
		return false, false
	}
}

func (s Script) Execute(ctx context.Context, d *RuntimeData) error {
	// A wall-clock budget is stricter than a CPU-time budget and also bounds
	// blocked operations when their implementations honor context cancellation.
	if !s.RequiresExtension(DovecotTestExtension) {
		limit := 5 * time.Second
		if v := s.opts.Conf("sieve_max_cpu_time"); v != "" {
			parsed, err := time.ParseDuration(v)
			if err != nil || parsed <= 0 {
				return fmt.Errorf("invalid sieve_max_cpu_time %q", v)
			}
			limit = parsed
		}
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, limit)
		defer cancel()
	}
	previous := d.runContext
	d.runContext = ctx
	defer func() { d.runContext = previous }()
	for _, c := range s.cmd {
		if err := d.checkExecution(); err != nil {
			return err
		}
		if err := c.Execute(ctx, d); err != nil {
			if errors.Is(err, ErrStop) || errors.Is(err, ErrReturn) {
				break // RFC 5228 3.3: stop ends the script, the implicit keep still applies
			}
			return err
		}
	}

	if err := d.checkExecution(); err != nil {
		return err
	}
	implicitKeep := d.ImplicitKeep
	for _, act := range d.AppliedActions {
		if act.cancelsImplicitKeep() {
			implicitKeep = false
		}
	}

	if implicitKeep {
		if err := d.OnAction(ctx, ActionKeep{
			Implicit: true,
			Flags:    d.Flags,
		}, d); err != nil {
			return err
		}
	}

	return nil
}
