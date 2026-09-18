package quota

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"shardpost/internal/config"
	"shardpost/internal/extprograms"
	"shardpost/internal/model"
)

// testService builds a service whose program appends its arguments and
// part of its environment to a file; runs returns the lines once all
// runs have finished.
func testService(t *testing.T, cfg config.Quota) (*Service, func() []string) {
	t.Helper()

	dir := t.TempDir()
	out := filepath.Join(t.TempDir(), "runs")

	script := "#!/bin/sh\nprintf '%s|%s|%s|%s\\n' \"$*\" \"$QUOTA_EVENT\" \"$QUOTA_STORAGE_USAGE\" \"$SIEVE_ACCOUNT\" >> \"$OUT\"\n"
	if err := os.WriteFile(filepath.Join(dir, "quota.sh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(dir, "hang.sh"), []byte("#!/bin/sh\nsleep 30\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	runner := extprograms.New(config.ExtPrograms{BinDir: dir, Programs: map[string]config.Program{
		"warn": {Command: "quota.sh", Args: []string{"{warning}", "{direction}", "{percent}", "{account}"}, Kinds: []string{"quota"}, Env: map[string]string{"OUT": out}},
		"flag": {Command: "quota.sh", Args: []string{"{state}", "{flag}", "{account}"}, Kinds: []string{"quota"}, Env: map[string]string{"OUT": out}},
		"hang": {Command: "hang.sh", Kinds: []string{"quota"}, TimeoutMS: 60000},
	}})

	s := NewService(cfg, runner, slog.New(slog.NewTextHandler(io.Discard, nil)))

	return s, func() []string {
		s.Close(context.Background())

		b, err := os.ReadFile(out)
		if err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}

		var lines []string
		if len(b) > 0 {
			lines = strings.Split(strings.TrimSuffix(string(b), "\n"), "\n")
		}

		slices.Sort(lines)

		return lines
	}
}

func TestWarnings(t *testing.T) {
	s, runs := testService(t, config.Quota{Warnings: []config.QuotaWarning{
		{Name: "full", Resource: "storage", Percent: 100, Program: "warn"},
		{Name: "almost", Resource: "storage", Percent: 90, Program: "warn"},
		{Name: "msgs", Resource: "messages", Percent: 50, Program: "warn"},
		{Name: "under", Resource: "storage", Percent: 100, Direction: "under", Program: "warn"},
	}})

	limit := model.Quota{Bytes: 1000, Msgs: 100}

	s.Changed("a", model.Quota{Bytes: 100}, model.Quota{Bytes: 200}, limit)          // nothing crossed
	s.Changed("b", model.Quota{Bytes: 800}, model.Quota{Bytes: 1000}, limit)         // 100 and 90: the first wins
	s.Changed("c", model.Quota{Bytes: 850}, model.Quota{Bytes: 950}, limit)          // 90
	s.Changed("d", model.Quota{Bytes: 1000}, model.Quota{Bytes: 999}, limit)         // back under
	s.Changed("e", model.Quota{Msgs: 49}, model.Quota{Msgs: 50}, limit)              // messages
	s.Changed("f", model.Quota{Bytes: 800}, model.Quota{Bytes: 1000}, model.Quota{}) // unlimited

	want := []string{
		"almost over 90 c|warning|950|",
		"full over 100 b|warning|1000|",
		"msgs over 50 e|warning|0|",
		"under under 100 d|warning|999|",
	}
	if got := runs(); !slices.Equal(got, sorted(want)) {
		t.Fatalf("runs:\n%q\nwant\n%q", got, sorted(want))
	}
}

func TestOverFlag(t *testing.T) {
	s, runs := testService(t, config.Quota{OverFlag: config.QuotaOverFlag{
		Enabled: true, Attribute: "quotaOverFlag", OverValue: "TRUE", Program: "flag", RecheckSeconds: 60,
	}})

	now := time.Unix(1_000_000, 0)
	s.now = func() time.Time { return now }

	limit := model.Quota{Bytes: 1000}
	full, empty := model.Quota{Bytes: 1000}, model.Quota{Bytes: 10}
	flagged := map[string][]string{"quotaOverFlag": {"true"}}

	s.CheckOverFlag("ok1", flagged, full, limit) // consistent
	s.CheckOverFlag("ok2", nil, empty, limit)    // consistent

	s.CheckOverFlag("over", nil, full, limit)
	s.CheckOverFlag("over", nil, full, limit) // debounced

	s.CheckOverFlag("under", flagged, empty, limit)

	now = now.Add(time.Minute)

	s.CheckOverFlag("under", flagged, empty, limit) // interval passed

	s.CheckOverFlag("flip", nil, full, limit)
	s.CheckOverFlag("flip", flagged, empty, limit) // other state: runs at once

	s.CheckOverFlag("unlimited", flagged, full, model.Quota{})                                // never over
	s.CheckOverFlag("dirty", map[string][]string{"quotaOverFlag": {"x\x00\ny"}}, full, limit) // cleaned

	want := []string{
		"over  over|over_flag|1000|",
		"under true under|over_flag|10|",
		"under true under|over_flag|10|",
		"over  flip|over_flag|1000|",
		"under true flip|over_flag|10|",
		"under true unlimited|over_flag|1000|",
		"over xy dirty|over_flag|1000|",
	}
	if got := runs(); !slices.Equal(got, sorted(want)) {
		t.Fatalf("runs:\n%q\nwant\n%q", got, sorted(want))
	}
}

func TestDispatchBounds(t *testing.T) {
	s, runs := testService(t, config.Quota{Warnings: []config.QuotaWarning{{Name: "w", Resource: "storage", Percent: 50, Program: "warn"}}})

	for range maxPending {
		s.slots <- struct{}{}
	}

	if s.dispatch(eventWarning, "warn", runContext("a", model.Quota{}, model.Quota{})) {
		t.Fatal("dispatched beyond maxPending")
	}

	for range maxPending {
		<-s.slots
	}

	if got := runs(); len(got) != 0 {
		t.Fatalf("dropped run ran: %q", got)
	}

	if s.dispatch(eventWarning, "warn", runContext("a", model.Quota{}, model.Quota{})) {
		t.Fatal("dispatched after Close")
	}
}

func TestCloseKillsHangingPrograms(t *testing.T) {
	s, _ := testService(t, config.Quota{Warnings: []config.QuotaWarning{{Name: "w", Resource: "storage", Percent: 50, Program: "hang"}}})

	s.Changed("a", model.Quota{}, model.Quota{Bytes: 60}, model.Quota{Bytes: 100})

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	start := time.Now()

	s.Close(ctx)

	if d := time.Since(start); d > 10*time.Second {
		t.Fatalf("Close waited %v for a hanging program", d)
	}
}

func TestPrintable(t *testing.T) {
	if got := printable("a\tb\x7fc\r\n"); got != "abc" {
		t.Fatalf("control characters: %q", got)
	}

	long := strings.Repeat("a", maxValue-1) + "ä" // "ä" straddles the cap
	if got := printable(long); got != strings.Repeat("a", maxValue-1) {
		t.Fatalf("cut inside a rune: %q", got[maxValue-4:])
	}
}

func TestDisabledService(t *testing.T) {
	var s *Service

	s.Changed("a", model.Quota{}, model.Quota{Bytes: 1}, model.Quota{Bytes: 1})
	s.CheckOverFlag("a", nil, model.Quota{}, model.Quota{})
	s.Close(context.Background())

	if NewService(config.Quota{}, nil, nil) != nil {
		t.Fatal("service without warnings or flag")
	}
}

func sorted(lines []string) []string {
	out := slices.Clone(lines)
	slices.Sort(out)

	return out
}

func TestStatusLine(t *testing.T) {
	if got := statusLine([]byte("quota-flag: done\x1b[0m\nsecond line\n")); got != "quota-flag: done[0m" {
		t.Fatalf("status line: %q", got)
	}

	if got := statusLine(nil); got != "" {
		t.Fatalf("empty output: %q", got)
	}
}

func TestLimitCrossingCorrectsFlag(t *testing.T) {
	s, runs := testService(t, config.Quota{OverFlag: config.QuotaOverFlag{
		Enabled: true, Attribute: "quotaOverFlag", OverValue: "TRUE", Program: "flag", RecheckSeconds: 60,
	}})

	limit := model.Quota{Bytes: 1000, Msgs: 10}

	s.Changed("full", model.Quota{Bytes: 900}, model.Quota{Bytes: 1000}, limit)                               // reached the limit
	s.Changed("freed", model.Quota{Bytes: 1000}, model.Quota{Bytes: 999}, limit)                              // expunge made room
	s.Changed("msgs", model.Quota{Msgs: 9}, model.Quota{Msgs: 10}, limit)                                     // message limit
	s.Changed("inside", model.Quota{Bytes: 100}, model.Quota{Bytes: 200}, limit)                              // nothing crossed
	s.CheckOverFlag("full", map[string][]string{"quotaOverFlag": {"FALSE"}}, model.Quota{Bytes: 1000}, limit) // stale userdb value: debounced

	want := []string{
		"over  full|over_flag|1000|",
		"under  freed|over_flag|999|",
		"over  msgs|over_flag|0|",
	}
	if got := runs(); !slices.Equal(got, sorted(want)) {
		t.Fatalf("runs:\n%q\nwant\n%q", got, sorted(want))
	}
}

func TestFlagPathsShareDebounce(t *testing.T) {
	s, runs := testService(t, config.Quota{OverFlag: config.QuotaOverFlag{
		Enabled: true, Attribute: "quotaOverFlag", OverValue: "TRUE", Program: "flag", RecheckSeconds: 60,
	}})

	limit := model.Quota{Bytes: 1000}
	flagged := map[string][]string{"quotaOverFlag": {"TRUE"}}

	s.Changed("a", model.Quota{Bytes: 900}, model.Quota{Bytes: 1000}, limit) // crossing up: run
	s.CheckOverFlag("a", flagged, model.Quota{Bytes: 1000}, limit)           // fresh TRUE: consistent, forgotten
	s.Changed("a", model.Quota{Bytes: 1000}, model.Quota{Bytes: 900}, limit) // crossing down: runs at once
	s.Changed("a", model.Quota{Bytes: 900}, model.Quota{Bytes: 1000}, limit) // up again: new state, runs

	want := []string{"over  a|over_flag|1000|", "under  a|over_flag|900|", "over  a|over_flag|1000|"}
	if got := runs(); !slices.Equal(got, sorted(want)) {
		t.Fatalf("runs:\n%q\nwant\n%q", got, sorted(want))
	}
}
