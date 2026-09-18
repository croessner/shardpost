package tests

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/foxcpp/go-sieve/interp"
)

// testProgramRunner is the extprograms hook of the testsuite: programs
// come from the Pigeonhole bin directories (sieve_execute_bin_dir,
// sieve_pipe_bin_dir, sieve_filter_bin_dir), arguments are passed as
// given, the timeout follows sieve_<kind>_exec_timeout ("0" = none).
// The server's own runner is stricter (an allowlist of exact command
// lines); this one only serves the conformance files.
func testProgramRunner(d *interp.RuntimeData) interp.ProgramRunner {
	return func(ctx context.Context, kind interp.ProgramKind, name string, args []string, input []byte) (interp.ProgramResult, bool, error) {
		conf := d.Script.Options().Conf
		dir := conf("sieve_" + string(kind) + "_bin_dir")
		if dir == "" {
			return interp.ProgramResult{}, false, nil
		}
		dir = strings.ReplaceAll(dir, "${tst.path}", d.Script.Options().TestPath)
		path := filepath.Join(dir, name)
		if st, err := os.Stat(path); err != nil || st.IsDir() {
			return interp.ProgramResult{}, false, nil
		}
		timeout := 10 * time.Second
		if t := conf("sieve_" + string(kind) + "_exec_timeout"); t != "" {
			if t == "0" {
				timeout = time.Minute
			} else if dur, err := time.ParseDuration(t); err == nil && dur > 0 {
				timeout = dur
			}
		}
		rctx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		cmd := exec.CommandContext(rctx, path, args...)
		cmd.Stdin = bytes.NewReader(input)
		var out, stderr bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &stderr
		host, _ := d.Env.GetEnvironment("host")
		if host == "" {
			host, _ = os.Hostname()
		}
		orig := d.Envelope.EnvelopeTo()
		if e, ok := d.Envelope.(interp.OriginalRecipient); ok {
			orig = e.OriginalRecipient()
		}
		cmd.Env = []string{"PATH=/usr/bin:/bin", "SENDER=" + d.Envelope.EnvelopeFrom(), "RECIPIENT=" + d.Envelope.EnvelopeTo(), "ORIG_RECIPIENT=" + orig, "HOST=" + host, "HOME=/tmp", "USER=test"}
		err := cmd.Run()
		res := interp.ProgramResult{Output: out.Bytes()}
		var ee *exec.ExitError
		switch {
		case err == nil:
		case errors.As(err, &ee):
			res.ExitCode = ee.ExitCode()
			if res.ExitCode < 0 {
				res.ExitCode = 124 // killed (timeout)
			}
		default:
			return res, true, err
		}
		return res, true, nil
	}
}
