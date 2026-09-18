package config

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
)

// ExtPrograms is the allowlist of external programs the Sieve extensions
// vnd.dovecot.execute, vnd.dovecot.pipe and vnd.dovecot.filter, the
// IMAPSieve learning rules and the quota warnings may start. Like a sudoers rule, an entry
// names exactly one command line; a script can only pick the entry by
// name. Programs live below bin_dir and are started directly, never
// through a shell.
type ExtPrograms struct {
	BinDir   string             `mapstructure:"bin_dir" env:"EXTPROGRAMS_BIN_DIR"`
	Programs map[string]Program `mapstructure:"programs" validate:"dive"`
}

// Program is one allowlisted command line of ExtPrograms.
type Program struct {
	// Command is the executable, relative to bin_dir (no directories).
	Command string `mapstructure:"command" validate:"required"`
	// Args are the fixed arguments; {account}, {mailbox} and, for quota
	// runs, the quota placeholders are replaced by the calling context.
	Args []string `mapstructure:"args"`
	// AllowScriptArgs lets a script append its own arguments after Args.
	AllowScriptArgs bool `mapstructure:"allow_script_args"`
	// Kinds restricts the uses of the entry (the Sieve extensions
	// execute, pipe and filter, the learning rules and quota warnings and
	// the over-quota flag); empty = all.
	Kinds          []string          `mapstructure:"kinds" validate:"dive,oneof=execute pipe filter learning quota"`
	TimeoutMS      int64             `mapstructure:"timeout_ms" validate:"gte=0,lte=600000"`
	MaxParallel    int64             `mapstructure:"max_parallel" validate:"gte=0,lte=256"`
	MaxOutputBytes int64             `mapstructure:"max_output_bytes" validate:"gte=0,lte=67108864"`
	MaxInputBytes  int64             `mapstructure:"max_input_bytes" validate:"gte=0,lte=67108864"`
	Env            map[string]string `mapstructure:"env"`
}

var programName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,127}$`)
var envName = regexp.MustCompile(`^[A-Z_][A-Z0-9_]*$`)

// Validate checks bin_dir and every program entry.
func (c ExtPrograms) Validate() error {
	if len(c.Programs) == 0 {
		return nil
	}

	if c.BinDir == "" || !filepath.IsAbs(c.BinDir) {
		return fmt.Errorf("extprograms.bin_dir must be an absolute directory")
	}

	for name, p := range c.Programs {
		if err := validateProgram(name, p); err != nil {
			return err
		}
	}

	return nil
}

// validateProgram checks the name, command, arguments and environment of
// one program entry.
func validateProgram(name string, p Program) error {
	if !programName.MatchString(name) {
		return fmt.Errorf("invalid extprograms program name")
	}

	if p.Command == "" || p.Command != filepath.Base(p.Command) || strings.HasPrefix(p.Command, ".") {
		return fmt.Errorf("extprograms.programs.%s.command must be a file name below bin_dir", name)
	}

	for _, a := range p.Args {
		if strings.ContainsAny(a, "\x00\r\n") {
			return fmt.Errorf("extprograms.programs.%s.args contains control characters", name)
		}
	}

	for k, v := range p.Env {
		if !envName.MatchString(k) || strings.ContainsAny(v, "\x00\n") {
			return fmt.Errorf("extprograms.programs.%s.env has an invalid entry", name)
		}
	}

	return nil
}

// Program returns the entry for name (ok false if there is none).
func (c ExtPrograms) Program(name string) (Program, bool) {
	p, ok := c.Programs[name]
	return p, ok
}
