package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestConfigPathCompatibility(t *testing.T) {
	dir := t.TempDir()
	legacy := filepath.Join(dir, "legacy.yaml")
	current := filepath.Join(dir, "current.yaml")

	flag := filepath.Join(dir, "flag.yaml")
	for p, level := range map[string]string{legacy: "warn", current: "debug", flag: "error"} {
		if err := os.WriteFile(p, []byte("logging:\n  level: "+level+"\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}

	t.Setenv("LOG_LEVEL", "info")

	if err := os.Unsetenv("LOG_LEVEL"); err != nil {
		t.Fatal(err)
	}

	t.Setenv("IMAP_CONFIG", legacy)
	t.Setenv("SHARDPOST_CONFIG", "")

	if err := os.Unsetenv("SHARDPOST_CONFIG"); err != nil {
		t.Fatal(err)
	}

	check := func(args []string, want string) {
		t.Helper()

		c, command, err := loadConfig(args)
		if err != nil || command != "check" || c.Logging.Level != want {
			t.Fatalf("level %s: command=%s err=%v", want, command, err)
		}
	}
	check([]string{"config", "check"}, "warn")
	t.Setenv("SHARDPOST_CONFIG", current)
	check([]string{"config", "check"}, "debug")
	check([]string{"--config", flag, "config", "check"}, "error")
	t.Setenv("SHARDPOST_CONFIG", "")
	check([]string{"config", "check"}, "info")
}

func TestBackfillCommandLine(t *testing.T) {
	t.Setenv("SHARDPOST_CONFIG", "")
	t.Setenv("IMAP_CONFIG", "")

	for args, want := range map[string]string{
		"index backfill accounts":           commandBackfillAccounts,
		"index backfill accounts --restart": commandBackfillRestart,
		"index backfill kv":                 commandBackfillKV,
		"index backfill kv --restart":       commandBackfillKVRestart,
		"index show kv":                     commandShowKV,
		"index show kv job/1 plan/2":        commandShowKV + " job/1 plan/2",
		"index backfill learning":           commandBackfillLearning,
		"index backfill learning --restart": commandBackfillLearningRestart,
		"index show learning":               commandShowLearning,
		"index show learning a1 b2":         commandShowLearning + " a1 b2",
		"scrub":                             commandScrub,
		"scrub --batch":                     commandScrubBatch,
		"scrub --sweep":                     commandScrubSweep,
		"scrub --sweep --repair":            commandScrubSweep + " --repair",
		"scrub --sweep --restart --repair":  commandScrubSweep + " --repair --restart",
		"scrub --sweep --restart":           commandScrubSweep + " --restart",
		"push":                              commandPush,
		"runner":                            commandRunner,
	} {
		if _, command, err := loadConfig(strings.Fields(args)); err != nil || command != want {
			t.Errorf("%q: %q %v", args, command, err)
		}
	}

	for _, bad := range []string{"index", "index backfill", "index backfill jobs", "--restart", "scrub --restart", "index show kv --restart",
		"index show accounts", "index show learning --restart", "index backfill accounts learning", "--batch", "push --batch",
		"--sweep", "scrub --repair", "scrub --batch --repair", "scrub --sweep --repair --repair", "push --sweep", "scrub --batch --sweep"} {
		if _, _, err := loadConfig(strings.Fields(bad)); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}
