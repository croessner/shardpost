package main

import (
	"bytes"
	"strings"
	"testing"

	"shardpost/internal/adminapi"
)

// TestPrintLayoutStateOld (review round 3): "fts layout show" names the
// run of every old entry and, for an import reshard, its heartbeat.
func TestPrintLayoutStateOld(t *testing.T) {
	gen := "9c1e00aa00bb00cc"
	old := []adminapi.FTSLayoutShape{{Layout: 2}, {Layout: 3, Gen: &gen}}
	toks := []string{"job-20261001T000000Z-000000000000/1", "importsession0123456/n1"}
	at := []string{"", "2026-10-02T12:00:00Z"}

	var out bytes.Buffer

	printLayoutState(&out, &adminapi.FTSLayoutState{Current: adminapi.FTSLayoutShape{Layout: 3}, Old: &old, OldToks: &toks, OldAt: &at})

	text := out.String()

	for _, want := range []string{
		"layout 2 tok job-20261001T000000Z-000000000000/1",
		"layout 3 gen 9c1e00aa00bb00cc tok importsession0123456/n1 at 2026-10-02T12:00:00Z",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("output lacks %q:\n%s", want, text)
		}
	}

	if strings.Count(text, " at ") != 1 {
		t.Errorf("a heartbeat for the job entry:\n%s", text)
	}
}
