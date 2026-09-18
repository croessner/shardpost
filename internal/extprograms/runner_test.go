package extprograms

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/foxcpp/go-sieve/interp"

	"shardpost/internal/config"
)

func testRunner(t *testing.T) *Runner {
	t.Helper()
	dir := t.TempDir()
	write := func(name, body string, mode os.FileMode) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), mode); err != nil {
			t.Fatal(err)
		}
	}
	write("echo.sh", "#!/bin/sh\nprintf 'ARGS=%s ACCOUNT=%s IN=' \"$*\" \"$SIEVE_ACCOUNT\"; cat\n", 0o755)
	write("fail.sh", "#!/bin/sh\nexit 75\n", 0o755)
	write("slow.sh", "#!/bin/sh\nsleep 5\n", 0o755)
	write("loose.sh", "#!/bin/sh\necho hi\n", 0o755)

	if err := os.Chmod(filepath.Join(dir, "loose.sh"), 0o777); err != nil { // umask-proof
		t.Fatal(err)
	}

	write("big.sh", "#!/bin/sh\nhead -c 5000 /dev/zero\n", 0o755)

	cfg := config.ExtPrograms{BinDir: dir, Programs: map[string]config.Program{
		"echo":  {Command: "echo.sh", Args: []string{"--account", "{account}"}, AllowScriptArgs: true},
		"fixed": {Command: "echo.sh", Args: []string{"fixed"}},
		"fail":  {Command: "fail.sh"},
		"slow":  {Command: "slow.sh", TimeoutMS: 300},
		"loose": {Command: "loose.sh"},
		"big":   {Command: "big.sh", MaxOutputBytes: 1000},
		"learn": {Command: "echo.sh", Kinds: []string{"learning"}},
	}}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}

	return New(cfg)
}

func TestRunnerAllowlist(t *testing.T) {
	r := testRunner(t)
	ctx := context.Background()

	res, err := r.Run(ctx, "execute", "echo", []string{"x"}, []byte("body"), Context{Account: "u@example.org"})
	if err != nil || res.ExitCode != 0 || string(res.Output) != "ARGS=--account u@example.org x ACCOUNT=u@example.org IN=body" {
		t.Fatalf("%v %q", err, res.Output)
	}

	if _, err := r.Run(ctx, "execute", "fixed", []string{"x"}, nil, Context{}); err == nil || !strings.Contains(err.Error(), "no script arguments") {
		t.Fatalf("script args accepted: %v", err)
	}

	if _, err := r.Run(ctx, "execute", "nope", nil, nil, Context{}); err != ErrNotAllowed {
		t.Fatalf("unknown program: %v", err)
	}

	if _, err := r.Run(ctx, "execute", "learn", nil, nil, Context{}); err != ErrNotAllowed {
		t.Fatalf("kind restriction: %v", err)
	}
}

func TestRunnerLimits(t *testing.T) {
	r := testRunner(t)
	ctx := context.Background()

	if res, err := r.Run(ctx, "learning", "fail", nil, nil, Context{}); err != nil || res.ExitCode != ExitTempFail {
		t.Fatalf("tempfail: %v %d", err, res.ExitCode)
	}

	if _, err := r.Run(ctx, "execute", "loose", nil, nil, Context{}); err == nil || !strings.Contains(err.Error(), "writable") {
		t.Fatalf("world-writable program accepted: %v", err)
	}

	if _, err := r.Run(ctx, "execute", "big", nil, nil, Context{}); err == nil || !strings.Contains(err.Error(), "output too large") {
		t.Fatalf("output limit: %v", err)
	}

	start := time.Now()
	if res, err := r.Run(ctx, "execute", "slow", nil, nil, Context{}); err == nil || res.ExitCode != ExitTempFail || time.Since(start) > 3*time.Second {
		t.Fatalf("timeout: %v %d after %v", err, res.ExitCode, time.Since(start))
	}
}

func TestSieveHook(t *testing.T) {
	r := testRunner(t)
	hook := r.SieveHook(Context{Account: "a@b"})

	res, ok, err := hook(context.Background(), interp.ProgramExecute, "echo", []string{"y"}, []byte("in"))
	if err != nil || !ok || res.ExitCode != 0 || !strings.HasSuffix(string(res.Output), "IN=in") {
		t.Fatalf("%v %v %+v", err, ok, res)
	}

	if _, ok, _ := hook(context.Background(), interp.ProgramExecute, "learn", nil, nil); ok {
		t.Fatal("kind restriction ignored by hook")
	}

	if _, ok, err := hook(context.Background(), interp.ProgramPipe, "fixed", nil, nil); !ok || err != nil {
		t.Fatal("pipe execution with empty input")
	}
}

func TestWaitingForSlotIsBounded(t *testing.T) {
	r := testRunner(t)
	for i := 0; i < cap(r.slots["slow"]); i++ {
		r.slots["slow"] <- struct{}{}
	}

	start := time.Now()

	_, err := r.Run(context.Background(), "execute", "slow", nil, nil, Context{})
	if err == nil || time.Since(start) > 2*time.Second {
		t.Fatal("slot wait escaped timeout")
	}
}

func TestProgramSymlinks(t *testing.T) {
	r := testRunner(t)
	if err := os.Symlink(filepath.Join(r.cfg.BinDir, "echo.sh"), filepath.Join(r.cfg.BinDir, "link")); err != nil {
		t.Fatal(err)
	}

	r.cfg.Programs["echo"] = config.Program{Command: "link"}
	if _, err := r.Run(context.Background(), "execute", "echo", nil, nil, Context{}); err == nil {
		t.Fatal("symlink accepted")
	}
}

func TestQuotaContext(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "env.sh"), []byte("#!/bin/sh\nprintf '%s|%s|%s' \"$*\" \"$QUOTA_EVENT\" \"$SIEVE_ACCOUNT\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	r := New(config.ExtPrograms{BinDir: dir, Programs: map[string]config.Program{
		"q": {Command: "env.sh", Args: []string{"{percent}", "{account}", "{unknown}"}, Kinds: []string{KindQuota}},
	}})

	c := Context{Account: "u", Vars: map[string]string{"percent": "95"}, Env: map[string]string{"QUOTA_EVENT": "warning"}}

	res, err := r.Run(context.Background(), KindQuota, "q", nil, nil, c)
	if err != nil || string(res.Output) != "95 u {unknown}|warning|" {
		t.Fatalf("%v %q", err, res.Output)
	}

	if _, err := r.Run(context.Background(), "execute", "q", nil, nil, c); err == nil {
		t.Fatal("quota program ran for Sieve")
	}
}
