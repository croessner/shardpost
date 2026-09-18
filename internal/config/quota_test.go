package config

import (
	"math"
	"strings"
	"testing"
)

func TestParseSize(t *testing.T) {
	cases := map[string]int64{"0": 0, "1024": 1024, "1K": 1024, "500M": 500 << 20, "2g": 2 << 30, "1T": 1 << 40, " 10 ": 10, "1KB": 1024, "7B": 7}
	for in, want := range cases {
		if got, ok := ParseSize(in); !ok || got != want {
			t.Errorf("%q: %d %v", in, got, ok)
		}
	}

	for _, bad := range []string{"", "x", "-1", "1X", "B", "9999999999T"} {
		if _, ok := ParseSize(bad); ok {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestParseAmount(t *testing.T) {
	for in, want := range map[string]Amount{"": {}, "10%": {Value: 10, Percent: true}, "100M": {Value: 100 << 20}} {
		if got, ok := ParseAmount(in, true); !ok || got != want {
			t.Errorf("%q: %+v %v", in, got, ok)
		}
	}

	if got, ok := ParseAmount("500", false); !ok || got.Value != 500 {
		t.Fatalf("count: %+v %v", got, ok)
	}

	for _, bad := range []string{"1001%", "-5%", "x%", "10M"} {
		if _, ok := ParseAmount(bad, false); ok {
			t.Errorf("%q accepted", bad)
		}
	}

	if got := (Amount{Value: 10, Percent: true}).Of(1000); got != 100 {
		t.Fatalf("10%% of 1000 = %d", got)
	}

	if got := (Amount{Value: 1000, Percent: true}).Of(math.MaxInt64); got != math.MaxInt64 {
		t.Fatalf("overflow not capped: %d", got)
	}

	if got := (Amount{Value: 0, Percent: true}).Of(1000); got != 0 {
		t.Fatalf("0%% = %d", got)
	}
}

const quotaYAML = `quota:
  grace: 10%
  rules:
    - special_use: '\Trash'
      extra_storage: 100M
    - mailbox: 'Spam*'
      ignore: true
  warnings:
    - name: warn-95
      resource: storage
      percent: 95
      program: warn
    - name: back-under
      resource: storage
      percent: 100
      direction: under
      program: warn
  over_flag:
    enabled: true
    attribute: quotaOverFlag
    program: warn
extprograms:
  bin_dir: /usr/local/lib/programs
  programs:
    warn:
      command: quota-warning.sh
      args: ['{percent}', '{account}']
      kinds: [quota]
`

func TestQuotaYAML(t *testing.T) {
	c, err := Load(configFile(t, quotaYAML))
	if err != nil {
		t.Fatal(err)
	}

	if len(c.Quota.Rules) != 2 || len(c.Quota.Warnings) != 2 || c.Quota.OverFlag.OverValue != "TRUE" || c.Quota.OverFlag.RecheckSeconds != 300 {
		t.Fatalf("quota section: %+v", c.Quota)
	}

	t.Setenv("QUOTA_OVER_FLAG_ENABLED", "false")
	t.Setenv("QUOTA_GRACE", "5M")

	c, err = Load(configFile(t, quotaYAML))
	if err != nil || c.Quota.OverFlag.Enabled || c.Quota.Grace != "5M" {
		t.Fatalf("environment: %v", err)
	}
}

func TestQuotaYAMLRejects(t *testing.T) {
	bad := map[string][2]string{
		"program kind":         {"kinds: [quota]", "kinds: [learning]"},
		"missing program":      {"program: warn\n  over_flag", "program: nope\n  over_flag"},
		"ignore with extra":    {"ignore: true", "ignore: true\n      extra_storage: 1M"},
		"both matchers":        {"- mailbox: 'Spam*'", "- mailbox: 'Spam*'\n      special_use: '\\Junk'"},
		"special-use":          {`'\Trash'`, "Trash"},
		"grace":                {"grace: 10%", "grace: lots"},
		"extra":                {"extra_storage: 100M", "extra_storage: 100X"},
		"resource":             {"resource: storage\n      percent: 95", "resource: disk\n      percent: 95"},
		"direction":            {"direction: under", "direction: sideways"},
		"duplicate name":       {"name: back-under", "name: warn-95"},
		"percent":              {"percent: 95", "percent: 0"},
		"over flag incomplete": {"attribute: quotaOverFlag", "attribute: ''"},
	}

	for name, r := range bad {
		if !strings.Contains(quotaYAML, r[0]) {
			t.Fatalf("%s: pattern not in body", name)
		}

		if _, err := Load(configFile(t, strings.Replace(quotaYAML, r[0], r[1], 1))); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}
