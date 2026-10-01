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
	} {
		if _, command, err := loadConfig(strings.Fields(args)); err != nil || command != want {
			t.Errorf("%q: %q %v", args, command, err)
		}
	}

	for _, bad := range []string{"index", "index backfill", "index backfill kv", "--restart", "scrub --restart"} {
		if _, _, err := loadConfig(strings.Fields(bad)); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}
