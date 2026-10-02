package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"strings"
	"testing"

	"shardpost/internal/cephstore"
	"shardpost/internal/config"
)

func TestSplitSystem(t *testing.T) {
	pre, args, ok := splitSystem([]string{"--config", "/etc/x.yaml", "system", "migrate", "--to", "p"})
	if !ok || !slices.Equal(pre, []string{"--config", "/etc/x.yaml"}) || !slices.Equal(args, []string{"migrate", "--to", "p"}) {
		t.Fatalf("split: %v %v %v", pre, args, ok)
	}

	if _, _, ok := splitSystem([]string{"scrub", "system"}); ok {
		t.Fatal("system after another command")
	}
}

// parsed parses args of sub; it returns the flags and the exit code
// (-1: go on).
func parsed(sub string, args ...string) (systemFlags, int) {
	var f systemFlags

	return f, parseSystemFlags(sub, args, &f, io.Discard)
}

func TestSystemFlags(t *testing.T) {
	for _, c := range []struct {
		sub  string
		args []string
		ok   bool
	}{
		{systemStatus, []string{"--deep"}, true},
		{systemStatus, []string{"--confirm", "x"}, false},
		{systemInit, nil, false},
		{systemInit, []string{"--pool", "p", "--wait"}, true},
		{systemInit, []string{"--pool", "p", "--parallel", "4"}, false},
		{systemMigrate, nil, false},
		{systemMigrate, []string{"--to", "p", "--check"}, true},
		{systemMigrate, []string{"--to", "p", "--check", "--wait"}, false},
		{systemMigrate, []string{"--to", "p", "--check", "--confirm", "r1"}, true},
		{systemMigrate, []string{"--to", "p", "--parallel", "65"}, false},
		{systemMigrate, []string{"--to", "p", "--force-registrations", "--confirm", "r1", "--wait"}, true},
		{systemVerify, nil, true},
		{systemVerify, []string{"--delete-surplus", "--confirm", "r1"}, false},
		{systemVerify, []string{"--source-only", "--delete-surplus"}, false},
		{systemVerify, []string{"--source-only", "--delete-surplus", "--confirm", "r1"}, true},
		{systemAbort, nil, false},
		{systemAbort, []string{"--run", "r1"}, true},
		{systemAbort, []string{"--run", "r1", "--delete-copies"}, false},
		{systemAbort, []string{"--run", "r1", "--delete-copies", "--delete-surplus", "--confirm", "r1"}, true},
		{systemRevert, []string{"--run", "r1"}, false},
		{systemRevert, []string{"--run", "r1", "--confirm", "r1", "--delete-surplus"}, true},
		{systemCleanup, []string{"--run", "r1", "--confirm", "r1", "--min-age", "1h"}, true},
		{systemCleanup, []string{"--run", "r1", "--confirm", "r1", "--min-age", "-1h"}, false},
		{systemCleanup, []string{"--run", "r1", "--confirm", "r1", "extra"}, false},
		{"unknown", nil, false},
	} {
		_, code := parsed(c.sub, c.args...)
		if (code == -1) != c.ok {
			t.Errorf("%s %v: code %d, want ok=%v", c.sub, c.args, code, c.ok)
		}
	}

	f, _ := parsed(systemCleanup, "--run", "r1", "--confirm", "r1")
	if f.minAge != cephstore.DefaultCleanupAge {
		t.Errorf("default --min-age %s", f.minAge)
	}

	f, _ = parsed(systemMigrate, "--to", "p")
	if f.parallel != cephstore.DefaultSystemParallel {
		t.Errorf("default --parallel %d", f.parallel)
	}
}

func TestSystemExit(t *testing.T) {
	for err, want := range map[error]int{
		nil: systemOK,
		fmt.Errorf("x: %w", cephstore.ErrSystemMismatch):                     systemMismatch,
		fmt.Errorf("x: %w", cephstore.ErrSystemRefused):                      systemRefused,
		fmt.Errorf("x: %w", cephstore.ErrInitRefused):                        systemRefused,
		fmt.Errorf("x: %w: %w", cephstore.ErrSystemRefused, cephstore.ErrFenceHeld): systemRefused,
		fmt.Errorf("x: %w", cephstore.ErrSystemPlacement):                    systemRefused,
		fmt.Errorf("x: %w", cephstore.ErrLeaseLost):                          systemFailed,
		errors.New("timeout"):                                                systemFailed,
	} {
		if got := systemExit(err); got != want {
			t.Errorf("%v: exit %d, want %d", err, got, want)
		}
	}
}

func TestReportSystem(t *testing.T) {
	var b bytes.Buffer

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	rep := &cephstore.SystemReport{Command: "verify", Mismatches: []string{"~admin/kv.x: differs"}}

	if code := reportSystem(&b, log, rep, fmt.Errorf("x: %w", cephstore.ErrSystemMismatch)); code != systemMismatch {
		t.Fatalf("exit %d", code)
	}

	var back cephstore.SystemReport
	if err := json.Unmarshal(b.Bytes(), &back); err != nil || back.Command != "verify" || len(back.Mismatches) != 1 {
		t.Fatalf("report %s: %v", b.String(), err)
	}
}

func TestToolClientOptions(t *testing.T) {
	opts := toolClientOptions(nil, 16)
	if opts["objecter_inflight_ops"] != "1024" || opts["objecter_inflight_op_bytes"] != "104857600" {
		t.Fatalf("defaults for 16 workers: %v", opts)
	}

	opts = toolClientOptions(map[string]string{"objecter_inflight_ops": "5000"}, 64)
	if opts["objecter_inflight_ops"] != "5000" || opts["objecter_inflight_op_bytes"] != "268435456" {
		t.Fatalf("64 workers: %v", opts)
	}
}

func TestSystemConfig(t *testing.T) {
	settings := &config.Config{}

	f := systemFlags{pool: "sys", parallel: 1}

	cfg, err := systemConfig(settings, systemInit, &f)
	if err != nil || cfg.SystemPool != "sys" || !cfg.PlacementTool {
		t.Fatalf("init: %+v %v", cfg, err)
	}

	f = systemFlags{to: "sys", parallel: 4}

	if cfg, err = systemConfig(settings, systemMigrate, &f); err != nil || cfg.SystemPool != "" {
		t.Fatalf("migrate: %+v %v", cfg, err)
	}

	settings.Ceph.SystemPool = "other"

	if _, err := systemConfig(settings, systemMigrate, &f); err == nil || !strings.Contains(err.Error(), "other") {
		t.Fatalf("migrate to another pool than ceph.system_pool: %v", err)
	}
}
