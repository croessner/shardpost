// Package extprograms starts external programs for Sieve (execute, pipe,
// filter), the IMAPSieve learning rules and the quota warnings under an
// allowlist: an entry in the configuration names exactly one command
// line, scripts can only choose the entry. No shell, a private environment, stdin from the
// server, bounded output, a timeout that kills the process group, and a
// cap on parallel runs per entry.
package extprograms

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/foxcpp/go-sieve/interp"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"

	"shardpost/internal/config"
)

var runs = promauto.NewCounterVec(prometheus.CounterOpts{Name: "imap_extprograms_runs_total", Help: "External program runs by outcome."}, []string{"program", "result"})

// ExitTempFail is the sysexits EX_TEMPFAIL code: the caller should try
// again later.
const ExitTempFail = 75

// DefaultParallel is the parallelism of a program without max_parallel
// (the learning concurrency is bounded by it as well).
const (
	defaultTimeout   = 10 * time.Second
	DefaultParallel  = 4
	defaultMaxOutput = 1 << 20
	defaultMaxInput  = 64 << 20
)

// ErrNotAllowed is returned for a program that is not in the allowlist
// (or not for this kind of use).
var ErrNotAllowed = errors.New("program not allowed")

// KindQuota is the kind of the quota warning and over-quota flag runs.
const KindQuota = "quota"

// Context is what a run may pass to the program: {account}, {mailbox}
// and the Vars (as {name}) in the fixed arguments; the SIEVE_*
// environment for the Sieve and learning kinds, Env for quota runs.
type Context struct {
	Account, Mailbox, Cause, ChangedFlags string
	Vars                                  map[string]string
	Env                                   map[string]string
}

// Result is a finished run.
type Result struct {
	Output   []byte
	ExitCode int
}

// Runner starts allowlisted programs; it holds the configuration and a
// semaphore per entry that caps parallel runs.
type Runner struct {
	cfg   config.ExtPrograms
	slots map[string]chan struct{}
}

// New builds a Runner for cfg with one parallelism slot set per program
// (max_parallel, default 4).
func New(cfg config.ExtPrograms) *Runner {
	r := &Runner{cfg: cfg, slots: map[string]chan struct{}{}}
	for name, p := range cfg.Programs {
		n := int(p.MaxParallel)
		if n <= 0 {
			n = DefaultParallel
		}

		r.slots[name] = make(chan struct{}, n)
	}

	return r
}

// Enabled tells whether any program is configured.
func (r *Runner) Enabled() bool { return r != nil && len(r.cfg.Programs) > 0 }

// Allowed reports whether name may be used for kind ("" = any).
func (r *Runner) Allowed(name, kind string) bool {
	if r == nil {
		return false
	}

	p, ok := r.cfg.Programs[name]
	if !ok {
		return false
	}

	if kind == "" || len(p.Kinds) == 0 {
		return true
	}

	for _, k := range p.Kinds {
		if k == kind {
			return true
		}
	}

	return false
}

// Run starts program name for kind with the message (or data) on stdin.
func (r *Runner) Run(ctx context.Context, kind, name string, scriptArgs []string, input []byte, c Context) (Result, error) {
	if !r.Allowed(name, kind) {
		return Result{}, ErrNotAllowed
	}

	p := r.cfg.Programs[name]

	if err := checkRequest(name, p, scriptArgs, input); err != nil {
		return Result{}, err
	}

	path, err := r.resolve(p)
	if err != nil {
		runs.WithLabelValues(name, "unresolved").Inc()
		return Result{}, err
	}

	timeout := time.Duration(p.TimeoutMS) * time.Millisecond
	if timeout <= 0 {
		timeout = defaultTimeout
	}

	rctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	select {
	case r.slots[name] <- struct{}{}:
		defer func() { <-r.slots[name] }()
	case <-rctx.Done():
		return Result{ExitCode: ExitTempFail}, rctx.Err()
	}

	maxOut := p.MaxOutputBytes
	if maxOut <= 0 {
		maxOut = defaultMaxOutput
	}

	out := &limitedBuffer{limit: maxOut}
	cmd := r.command(rctx, kind, path, p, scriptArgs, input, c, out)
	err = cmd.Run()

	return runResult(rctx, name, out, err)
}

// checkRequest validates the script arguments and the input size
// against the program entry before anything is started.
func checkRequest(name string, p config.Program, scriptArgs []string, input []byte) error {
	if err := checkScriptArgs(scriptArgs); err != nil {
		return err
	}

	if len(scriptArgs) > 0 && !p.AllowScriptArgs {
		runs.WithLabelValues(name, "args_refused").Inc()
		return fmt.Errorf("program %q takes no script arguments", name)
	}

	maxIn := p.MaxInputBytes
	if maxIn <= 0 {
		maxIn = defaultMaxInput
	}

	if int64(len(input)) > maxIn {
		runs.WithLabelValues(name, "input_too_large").Inc()
		return fmt.Errorf("program %q: input too large", name)
	}

	return nil
}

// checkScriptArgs bounds the number and length of script arguments and
// refuses control characters.
func checkScriptArgs(scriptArgs []string) error {
	if len(scriptArgs) > 64 {
		return fmt.Errorf("too many program arguments")
	}

	for _, a := range scriptArgs {
		if len(a) > 1024 {
			return fmt.Errorf("program argument too long")
		}

		for _, c := range a {
			if c < 0x20 || c == 0x7f {
				return fmt.Errorf("invalid program argument")
			}
		}
	}

	return nil
}

// command builds the process: fixed arguments with the placeholders
// substituted, then the script arguments; private environment, own
// process group killed on cancel, stdout bounded by out.
func (r *Runner) command(rctx context.Context, kind, path string, p config.Program, scriptArgs []string, input []byte, c Context, out *limitedBuffer) *exec.Cmd {
	pairs := []string{"{account}", c.Account, "{mailbox}", c.Mailbox}
	for k, v := range c.Vars {
		pairs = append(pairs, "{"+k+"}", v)
	}

	repl := strings.NewReplacer(pairs...)

	args := make([]string, 0, len(p.Args)+len(scriptArgs))
	for _, a := range p.Args {
		args = append(args, repl.Replace(a))
	}

	args = append(args, scriptArgs...)
	cmd := exec.CommandContext(rctx, path, args...)
	cmd.Dir = r.cfg.BinDir
	cmd.Env = r.environment(kind, p, c)
	cmd.Stdin = bytes.NewReader(input)
	cmd.Stdout = out
	cmd.Stderr = io.Discard
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = 2 * time.Second

	return cmd
}

// runResult maps the outcome of cmd.Run to a Result, an error and the
// run metric.
func runResult(rctx context.Context, name string, out *limitedBuffer, err error) (Result, error) {
	res := Result{Output: out.Bytes()}
	if out.truncated {
		runs.WithLabelValues(name, "output_truncated").Inc()
		return res, fmt.Errorf("program %q: output too large", name)
	}

	var ee *exec.ExitError
	switch {
	case err == nil:
		runs.WithLabelValues(name, "ok").Inc()
	case rctx.Err() != nil:
		runs.WithLabelValues(name, "timeout").Inc()
		return Result{ExitCode: ExitTempFail}, fmt.Errorf("program %q: timeout", name)
	case errors.As(err, &ee):
		res.ExitCode = ee.ExitCode()
		if res.ExitCode == ExitTempFail {
			runs.WithLabelValues(name, "tempfail").Inc()
		} else {
			runs.WithLabelValues(name, "failed").Inc()
		}
	default:
		runs.WithLabelValues(name, "error").Inc()
		return res, fmt.Errorf("program %q: %v", name, err)
	}

	return res, nil
}

// resolve checks the executable: below bin_dir, a regular file, not a
// symlink, not writable by group or others.
func (r *Runner) resolve(p config.Program) (string, error) {
	if p.Command != filepath.Base(p.Command) || strings.HasPrefix(p.Command, ".") {
		return "", ErrNotAllowed
	}

	dir, err := os.Lstat(r.cfg.BinDir)
	if err != nil || !dir.IsDir() || dir.Mode().Perm()&0o022 != 0 {
		return "", fmt.Errorf("unsafe program directory")
	}

	path := filepath.Join(r.cfg.BinDir, p.Command)

	st, err := os.Lstat(path)
	if err != nil {
		return "", fmt.Errorf("program %q not found", p.Command)
	}

	if !st.Mode().IsRegular() {
		return "", fmt.Errorf("program %q is not a regular file", p.Command)
	}

	if st.Mode().Perm()&0o022 != 0 {
		return "", fmt.Errorf("program %q is writable by group or others", p.Command)
	}

	if st.Mode().Perm()&0o111 == 0 {
		return "", fmt.Errorf("program %q is not executable", p.Command)
	}

	return path, nil
}

// environment is the private environment of a run: a fixed base, the
// entry's env, then the context (SIEVE_* or, for quota runs, c.Env).
func (r *Runner) environment(kind string, p config.Program, c Context) []string {
	env := []string{"PATH=/usr/local/bin:/usr/bin:/bin", "LANG=C.UTF-8", "HOME=/tmp"}
	for k, v := range p.Env {
		env = append(env, k+"="+v)
	}

	if kind == KindQuota {
		for k, v := range c.Env {
			env = append(env, k+"="+v)
		}

		return env
	}

	env = append(env, "SIEVE_ACCOUNT="+c.Account, "SIEVE_MAILBOX="+c.Mailbox, "SIEVE_CAUSE="+c.Cause, "SIEVE_CHANGED_FLAGS="+c.ChangedFlags)

	return env
}

// limitedBuffer collects stdout up to a limit. It deliberately has no
// ReadFrom, so exec's io.Copy goes through Write and the limit holds.
type limitedBuffer struct {
	buf       bytes.Buffer
	limit     int64
	truncated bool
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	if int64(b.buf.Len())+int64(len(p)) > b.limit {
		b.truncated = true
		return 0, fmt.Errorf("output limit exceeded")
	}

	return b.buf.Write(p)
}

func (b *limitedBuffer) Bytes() []byte { return b.buf.Bytes() }

// SieveHook adapts the runner to the interpreter's hook for one delivery
// or IMAP event context.
func (r *Runner) SieveHook(c Context) interp.ProgramRunner {
	return func(ctx context.Context, kind interp.ProgramKind, name string, args []string, input []byte) (interp.ProgramResult, bool, error) {
		if !r.Allowed(name, string(kind)) {
			return interp.ProgramResult{}, false, nil
		}

		res, err := r.Run(ctx, string(kind), name, args, input, c)
		if errors.Is(err, ErrNotAllowed) {
			return interp.ProgramResult{}, false, nil
		}

		if err != nil {
			return interp.ProgramResult{ExitCode: res.ExitCode}, true, err
		}

		return interp.ProgramResult{Output: res.Output, ExitCode: res.ExitCode}, true, nil
	}
}
