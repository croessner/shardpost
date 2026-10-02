package main

import (
	"slices"
	"testing"

	"shardpost/internal/bench/cephbench"
)

// TestMeasureConvertLayout: --convert needs layout 3, from --fts-layout
// or, without the flag, from fts.layout of the configuration.
func TestMeasureConvertLayout(t *testing.T) {
	cases := []struct {
		configured, flag int
		want             bool
	}{
		{3, 0, true},
		{3, 2, false},
		{2, 0, false},
		{2, 3, true},
	}

	for _, c := range cases {
		o := cephbench.MeasureOptions{Every: 1, Convert: true}
		f := benchFlags{parallel: 1, ftsLayout: c.flag}

		if got := measureValid(&o, &f, c.configured); got != c.want {
			t.Errorf("fts.layout %d, --fts-layout %d: valid %v, want %v", c.configured, c.flag, got, c.want)
		}
	}
}

func TestSplitBench(t *testing.T) {
	pre, args, ok := splitBench([]string{"--config", "x.yaml", "bench", "measure", "--json", "-"})
	if !ok || !slices.Equal(pre, []string{"--config", "x.yaml"}) || !slices.Equal(args, []string{"measure", "--json", "-"}) {
		t.Fatalf("got %v %v %v", pre, args, ok)
	}

	for _, line := range [][]string{{"scrub"}, {}, {"--config", "x.yaml"}, {"index", "bench"}} {
		if _, _, ok := splitBench(line); ok {
			t.Errorf("%v taken for bench", line)
		}
	}
}

// TestBenchRefusals: the guard refuses before anything connects (the
// configuration names the default production pools).
func TestBenchRefusals(t *testing.T) {
	t.Setenv("SHARDPOST_CONFIG", "")
	t.Setenv("IMAP_CONFIG", "")

	for name, args := range map[string][]string{
		"no subcommand":       {},
		"unknown subcommand":  {"bench-it"},
		"no pools":            {"populate"},
		"production index":    {"populate", "--index-pool", "shardpost-index", "--data-pool", "load-data"},
		"production data":     {"measure", "--index-pool", "load-index", "--data-pool", "shardpost-data"},
		"prefix outside":      {"populate", "--index-pool", "load-index", "--data-pool", "load-data", "--prefix", "load"},
		"bad sizes":           {"measure", "--index-pool", "load-index", "--data-pool", "load-data", "--sizes", "x"},
		"stray argument":      {"populate", "extra"},
		"unknown flag":        {"measure", "--no-such-flag"},
		"one pool for both":   {"populate", "--index-pool", "load", "--data-pool", "load"},
		"bad inbox share":     {"populate", "--index-pool", "load-index", "--data-pool", "load-data", "--inbox-share", "2"},
		"bad sample interval": {"measure", "--index-pool", "load-index", "--data-pool", "load-data", "--sample-every", "0"},
		"negative contention": {"measure", "--index-pool", "load-index", "--data-pool", "load-data", "--contention", "-1"},
		"negative search":     {"measure", "--index-pool", "load-index", "--data-pool", "load-data", "--search", "-1"},
		"negative expunge":    {"measure", "--index-pool", "load-index", "--data-pool", "load-data", "--expunge", "-1"},
	} {
		if code := runBench(nil, args); code != benchRefused {
			t.Errorf("%s: exit %d, want %d", name, code, benchRefused)
		}
	}
}

// TestBenchRefusesDefaultPools: the built-in pool names are refused even
// when the configuration names other production pools.
func TestBenchRefusesDefaultPools(t *testing.T) {
	t.Setenv("SHARDPOST_CONFIG", "")
	t.Setenv("IMAP_CONFIG", "")
	t.Setenv("CEPH_INDEX_POOL", "prod-index")
	t.Setenv("CEPH_DATA_POOL", "prod-data")

	for _, args := range [][]string{
		{"populate", "--index-pool", "shardpost-index", "--data-pool", "load-data"},
		{"measure", "--index-pool", "load-index", "--data-pool", "shardpost-data"},
		{"populate", "--index-pool", "prod-index", "--data-pool", "load-data"},
	} {
		if code := runBench(nil, args); code != benchRefused {
			t.Errorf("%v: exit %d, want %d", args, code, benchRefused)
		}
	}
}

// TestBenchRefusesProductionSystemPool: --system-pool must not name the
// production system pool (ceph.system_pool), nor a bench pool of another
// kind; the production system pool is never taken over by default.
func TestBenchRefusesProductionSystemPool(t *testing.T) {
	t.Setenv("SHARDPOST_CONFIG", "")
	t.Setenv("IMAP_CONFIG", "")
	t.Setenv("CEPH_SYSTEM_POOL", "prod-system")

	for _, args := range [][]string{
		{"populate", "--index-pool", "load-index", "--data-pool", "load-data", "--system-pool", "prod-system"},
		{"measure", "--index-pool", "load-index", "--data-pool", "load-data", "--system-pool", "prod-system"},
		{"populate", "--index-pool", "prod-system", "--data-pool", "load-data"},
		{"populate", "--index-pool", "load-index", "--data-pool", "load-data", "--system-pool", "load-index"},
		{"populate", "--index-pool", "load-index", "--data-pool", "load-data", "--system-pool", "shardpost-index"},
	} {
		if code := runBench(nil, args); code != benchRefused {
			t.Errorf("%v: exit %d, want %d", args, code, benchRefused)
		}
	}
}
